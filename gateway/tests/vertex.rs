//! Vertex AI end to end over real sockets: a gcp_sa provider mints a Google token
//! from its service account key at a stub token endpoint, then reaches a stub
//! Vertex endpoint (a private base URL) with it.

use std::sync::{Arc, Mutex};

use bytes::Bytes;
use http::{Request, Response, StatusCode};
use http_body_util::{BodyExt, Full};
use hyper::body::Incoming;
use hyper::service::service_fn;
use hyper_util::rt::{TokioExecutor, TokioIo};
use hyper_util::server::conn::auto;
use llmr_gateway::config::{Config, Wire};
use llmr_gateway::server::{self, State};
use serde_json::{Value, json};
use tokio::net::TcpListener;

const KEY: &str = "llmr_testkey";
const PEM: &str = include_str!("../src/cloud/testdata/rsa-pkcs8.pem");

/// (path, authorization, body) of each request to the stub.
type Seen = Arc<Mutex<Vec<(String, String, Value)>>>;

/// Google: the token endpoint at /token, Vertex everywhere else.
async fn google() -> (String, Seen) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("http://{}", listener.local_addr().unwrap());
    let seen: Seen = Default::default();
    let s = seen.clone();
    tokio::spawn(async move {
        while let Ok((conn, _)) = listener.accept().await {
            let s = s.clone();
            tokio::spawn(async move {
                let svc = service_fn(move |req: Request<Incoming>| {
                    let s = s.clone();
                    async move {
                        let path = req.uri().path().to_string();
                        let auth = req.headers().get("authorization").map(|v| v.to_str().unwrap().to_string());
                        let raw = req.into_body().collect().await.unwrap().to_bytes();
                        let body = serde_json::from_slice(&raw).unwrap_or(Value::Null);
                        s.lock().unwrap().push((path.clone(), auth.unwrap_or_default(), body.clone()));
                        let reply = if path == "/token" {
                            r#"{"access_token":"ya29.vertex","token_type":"Bearer","expires_in":3599}"#
                        } else if path.ends_with(":streamRawPredict") {
                            "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":1}}}\n\n\
                             event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
                        } else if path.ends_with(":rawPredict") {
                            r#"{"content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":1,"output_tokens":1}}"#
                        } else {
                            r#"{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}"#
                        };
                        Ok::<_, std::convert::Infallible>(Response::new(Full::new(Bytes::from_static(
                            reply.as_bytes(),
                        ))))
                    }
                });
                let _ = auto::Builder::new(TokioExecutor::new()).serve_connection(TokioIo::new(conn), svc).await;
            });
        }
    });
    (url, seen)
}

async fn gateway(google: &str) -> String {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let digest = ring::digest::digest(&ring::digest::SHA256, KEY.as_bytes());
    let hash: String = digest.as_ref().iter().map(|b| format!("{b:02x}")).collect();
    let sa = json!({"type": "service_account", "client_email": "gw@p.iam.gserviceaccount.com",
        "private_key": PEM, "token_uri": format!("{google}/token")});
    let wire: Wire = serde_json::from_value(json!({
        "schema": 2, "version": 1, "group": {"id": "g", "name": "g"},
        "keys": [{"hash": hash, "id": "k"}],
        "providers": [{"id": "v", "name": "vertex", "type": "gcp_vertex", "base_url": google,
            "auth_mode": "gcp_sa", "api_key": sa.to_string(), "project": "proj-1", "location": "us-east5"}],
        "deployments": [
            {"id": "d1", "provider_id": "v", "model_name": "claude", "upstream_name": "claude-sonnet-4-5@20250929",
             "api_flavor": "anthropic"},
            {"id": "d2", "provider_id": "v", "model_name": "gemini", "upstream_name": "google/gemini-2.5-flash",
             "api_flavor": "openai"},
        ],
    }))
    .unwrap();
    let st: &'static State = Box::leak(Box::new(State {
        config: Default::default(),
        ready: Default::default(),
        draining: Default::default(),
        max_in_flight: 1000,
    }));
    st.config.store(Some(Arc::new(Config::build(wire).unwrap())));
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("http://{}", listener.local_addr().unwrap());
    tokio::spawn(async move {
        while let Ok((conn, _)) = listener.accept().await {
            tokio::spawn(async move {
                let svc = service_fn(move |req| server::handle(st, req));
                let _ = auto::Builder::new(TokioExecutor::new()).serve_connection(TokioIo::new(conn), svc).await;
            });
        }
    });
    url
}

async fn post(url: String, body: Value) -> (StatusCode, Bytes) {
    let req = Request::post(url)
        .header("authorization", format!("Bearer {KEY}"))
        .body(Full::new(Bytes::from(body.to_string())))
        .unwrap();
    let resp = server::https_client(1).request(req).await.unwrap();
    let status = resp.status();
    (status, resp.into_body().collect().await.unwrap().to_bytes())
}

#[tokio::test]
async fn vertex_with_a_service_account_key() {
    let (google, seen) = google().await;
    let gw = gateway(&google).await;
    let msg = json!([{"role": "user", "content": "hi"}]);

    let (status, body) =
        post(format!("{gw}/v1/messages"), json!({"model": "claude", "max_tokens": 5, "messages": msg})).await;
    assert_eq!(status, StatusCode::OK, "{body:?}");
    let (status, body) =
        post(format!("{gw}/v1/chat/completions"), json!({"model": "claude", "stream": true, "messages": msg})).await;
    assert_eq!(status, StatusCode::OK, "{body:?}");
    let (status, body) = post(format!("{gw}/v1/chat/completions"), json!({"model": "gemini", "messages": msg})).await;
    assert_eq!(status, StatusCode::OK, "{body:?}");
    let (status, _) = post(format!("{gw}/v1/embeddings"), json!({"model": "gemini", "input": "x"})).await;
    assert_eq!(status, StatusCode::BAD_REQUEST, "vertex has no embeddings");

    let seen = seen.lock().unwrap();
    let paths: Vec<&str> = seen.iter().map(|(p, _, _)| p.as_str()).collect();
    let parent = "/v1/projects/proj-1/locations/us-east5";
    assert_eq!(
        paths,
        [
            "/token".to_string(),
            format!("{parent}/publishers/anthropic/models/claude-sonnet-4-5%4020250929:rawPredict"),
            format!("{parent}/publishers/anthropic/models/claude-sonnet-4-5%4020250929:streamRawPredict"),
            format!("{parent}/endpoints/openapi/chat/completions"),
        ],
        "one token for every request"
    );
    for (path, auth, body) in &seen[1..] {
        assert_eq!(auth, "Bearer ya29.vertex", "{path}");
        if path.contains("anthropic") {
            assert!(body.get("model").is_none(), "{path}: vertex rejects the model in the body");
            assert_eq!(body["anthropic_version"], "vertex-2023-10-16", "{path}");
        } else {
            assert_eq!(body["model"], "google/gemini-2.5-flash");
        }
    }
    assert_eq!(seen[2].2["stream"], true);
}
