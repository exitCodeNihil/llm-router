//! The gateway end to end over real sockets, against a stub upstream: what a
//! client sees of rate-limit headers and of routes a deployment can't serve.
//! internal/gateway/proxy_test.go holds the same cases for the Go gateway.

use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};

use bytes::Bytes;
use http::{HeaderMap, Request, Response, StatusCode};
use http_body_util::{BodyExt, Full};
use hyper::body::Incoming;
use hyper::service::service_fn;
use hyper_util::rt::{TokioExecutor, TokioIo};
use hyper_util::server::conn::auto;
use llmr_gateway::config::{Config, Wire};
use llmr_gateway::server::{self, State};
use llmr_gateway::usage;
use serde_json::{Value, json};
use tokio::net::TcpListener;

const KEY: &str = "llmr_testkey";

/// What a stub upstream answers with.
#[derive(Clone, Copy)]
struct Reply {
    status: u16,
    flavor: &'static str,
    headers: &'static [(&'static str, &'static str)],
}

const LIMITS: &[(&str, &str)] = &[
    ("anthropic-ratelimit-requests-remaining", "41"),
    ("anthropic-ratelimit-unified-status", "allowed"),
    ("x-ratelimit-remaining-tokens", "900"),
    ("x-ratelimit-limit", "a"),
    ("x-ratelimit-limit", "b"),
    ("anthropic-organization-id", "org-secret"),
    ("set-cookie", "session=upstream"),
];

const REJECTED: &[(&str, &str)] = &[
    ("anthropic-ratelimit-requests-remaining", "0"),
    ("anthropic-ratelimit-unified-status", "rejected"),
    ("x-ratelimit-reset-requests", "9s"),
    ("retry-after", "9"),
];

fn payload(flavor: &str, stream: bool) -> &'static str {
    match (flavor, stream) {
        ("anthropic", true) => {
            "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":1}}}\n\n\
             event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
        }
        ("anthropic", false) => {
            r#"{"content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":1,"output_tokens":1}}"#
        }
        (_, true) => "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n",
        (_, false) => r#"{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}"#,
    }
}

/// A stub upstream: its base URL and how many requests reached it.
async fn upstream(reply: Reply) -> (String, Arc<AtomicUsize>) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("http://{}", listener.local_addr().unwrap());
    let hits = Arc::new(AtomicUsize::new(0));
    let counted = hits.clone();
    tokio::spawn(async move {
        while let Ok((conn, _)) = listener.accept().await {
            let hits = counted.clone();
            tokio::spawn(async move {
                let svc = service_fn(move |req: Request<Incoming>| {
                    hits.fetch_add(1, Ordering::Relaxed);
                    async move {
                        let raw = req.into_body().collect().await.unwrap().to_bytes();
                        let v: Value = serde_json::from_slice(&raw).unwrap_or_default();
                        let mut r = Response::builder().status(reply.status);
                        for (k, val) in reply.headers {
                            r = r.header(*k, *val);
                        }
                        let body = payload(reply.flavor, v["stream"] == true);
                        Ok::<_, std::convert::Infallible>(
                            r.body(Full::new(Bytes::from_static(body.as_bytes()))).unwrap(),
                        )
                    }
                });
                let _ = auto::Builder::new(TokioExecutor::new()).serve_connection(TokioIo::new(conn), svc).await;
            });
        }
    });
    (url, hits)
}

/// A base URL nothing listens on.
async fn dead_upstream() -> String {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    format!("http://{}", listener.local_addr().unwrap())
}

/// A gateway serving `model` from these deployments, in order: (base URL,
/// flavor, auth mode). Returns its base URL.
async fn gateway(model: &str, deployments: &[(&str, &str, &str)]) -> String {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let digest = ring::digest::digest(&ring::digest::SHA256, KEY.as_bytes());
    let hash: String = digest.as_ref().iter().map(|b| format!("{b:02x}")).collect();
    let mut providers = Vec::new();
    let mut deps = Vec::new();
    for (i, (url, flavor, auth)) in deployments.iter().enumerate() {
        let mut p = json!({"id": format!("p{i}"), "name": format!("p{i}"), "type": "openai_compatible",
            "base_url": url, "auth_mode": auth});
        if *auth == "anthropic_wif" {
            // No identity token on this machine, so every exchange fails.
            p["wif"] = json!({"federation_rule_id": format!("fdrl_{i}"), "organization_id": "o",
                "service_account_id": "svac_1", "identity_token_file": "/nonexistent/llmr-test-token"});
        }
        providers.push(p);
        deps.push(json!({"id": format!("d{i}"), "provider_id": format!("p{i}"), "model_name": model,
            "upstream_name": format!("up{i}"), "api_flavor": flavor}));
    }
    let wire: Wire = serde_json::from_value(json!({
        "schema": 1, "version": 1, "group": {"id": "g", "name": "g"},
        "keys": [{"hash": hash, "id": "k"}], "providers": providers, "deployments": deps,
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

/// POST with the gateway key in Authorization, so no caller credential.
async fn post(url: String, body: Value) -> (StatusCode, HeaderMap, Bytes) {
    let req = Request::post(url)
        .header("authorization", format!("Bearer {KEY}"))
        .body(Full::new(Bytes::from(body.to_string())))
        .unwrap();
    let resp = server::https_client(1).request(req).await.unwrap();
    let (parts, body) = resp.into_parts();
    (parts.status, parts.headers, body.collect().await.unwrap().to_bytes())
}

fn values<'a>(h: &'a HeaderMap, name: &str) -> Vec<&'a str> {
    h.get_all(name).iter().map(|v| v.to_str().unwrap()).collect()
}

/// Every way a successful response leaves the gateway.
#[tokio::test]
async fn forwards_rate_limit_headers_on_success() {
    for path in ["/v1/chat/completions", "/v1/messages"] {
        for flavor in ["openai", "anthropic"] {
            for stream in [false, true] {
                let what = format!("{path} {flavor} stream={stream}");
                let (up, _) = upstream(Reply { status: 200, flavor, headers: LIMITS }).await;
                let gw = gateway("m", &[(&up, flavor, "none")]).await;
                let (status, h, _) = post(
                    format!("{gw}{path}"),
                    json!({"model": "m", "stream": stream, "max_tokens": 5,
                           "messages": [{"role": "user", "content": "hi"}]}),
                )
                .await;
                assert_eq!(status, 200, "{what}");
                assert_eq!(values(&h, "anthropic-ratelimit-requests-remaining"), ["41"], "{what}");
                assert_eq!(values(&h, "anthropic-ratelimit-unified-status"), ["allowed"], "{what}");
                assert_eq!(values(&h, "x-ratelimit-remaining-tokens"), ["900"], "{what}");
                assert_eq!(values(&h, "x-ratelimit-limit"), ["a", "b"], "{what}");
                assert!(!h.contains_key("anthropic-organization-id"), "{what}");
                assert!(!h.contains_key("set-cookie"), "{what}");
            }
        }
    }
}

/// The attempt that failed over describes a deployment that didn't answer.
#[tokio::test]
async fn rate_limit_headers_not_forwarded_from_failed_over_deployment() {
    let (limited, _) = upstream(Reply { status: 429, flavor: "openai", headers: REJECTED }).await;
    let (good, _) =
        upstream(Reply { status: 200, flavor: "openai", headers: &[("anthropic-ratelimit-requests-remaining", "41")] })
            .await;
    let gw = gateway("m", &[(&limited, "openai", "none"), (&good, "openai", "none")]).await;
    let (status, h, _) = post(format!("{gw}/v1/chat/completions"), json!({"model": "m", "messages": []})).await;
    assert_eq!(status, 200);
    assert_eq!(values(&h, "x-llmr-attempts"), ["2"]);
    assert_eq!(values(&h, "anthropic-ratelimit-requests-remaining"), ["41"]);
    for name in ["anthropic-ratelimit-unified-status", "x-ratelimit-reset-requests", "retry-after"] {
        assert!(!h.contains_key(name), "{name} from the failed attempt reached the client");
    }
}

#[tokio::test]
async fn rate_limit_headers_only_on_success() {
    let (limited, _) = upstream(Reply { status: 429, flavor: "openai", headers: REJECTED }).await;
    let gw = gateway("m", &[(&limited, "openai", "none")]).await;
    let (status, h, _) = post(format!("{gw}/v1/chat/completions"), json!({"model": "m", "messages": []})).await;
    assert_eq!(status, 429);
    assert_eq!(values(&h, "retry-after"), ["9"]);
    assert!(!h.contains_key("anthropic-ratelimit-requests-remaining"));
}

#[tokio::test]
async fn unsupported_route_is_the_callers_mistake() {
    let (up, hits) = upstream(Reply { status: 200, flavor: "anthropic", headers: &[] }).await;
    let cases = [
        ("route-embeddings", "/v1/embeddings", json!({"input": "hi"}), "none"),
        ("route-completions", "/v1/completions", json!({"prompt": "hi"}), "none"),
        // A body that can't translate doesn't get to hide the actual reason.
        ("route-bad-body", "/v1/embeddings", json!({"messages": "nope"}), "none"),
        // Nor does a missing credential: bringing one wouldn't help.
        ("route-no-credential", "/v1/embeddings", json!({"input": "hi"}), "oauth_passthrough"),
    ];
    for (model, path, mut body, auth) in cases {
        body["model"] = json!(model);
        let gw = gateway(model, &[(&up, "anthropic", auth)]).await;
        let (status, _, raw) = post(format!("{gw}{path}"), body).await;
        assert_eq!(status, 400, "{model}");
        let v: Value = serde_json::from_slice(&raw).unwrap();
        assert_eq!(v["error"]["type"], "invalid_request_error", "{model}");
        assert_eq!(v["error"]["code"], "unsupported_route", "{model}");
        assert!(v["error"]["message"].as_str().unwrap().contains("is not supported"), "{model}");
        let events: Vec<_> =
            usage::take(usize::MAX, usize::MAX).into_iter().filter(|e| e.model_name == model).collect();
        assert_eq!(events.len(), 1, "{model}");
        assert_eq!((events[0].status_code, events[0].error_code), (400, "unsupported_route"), "{model}");
    }
    assert_eq!(hits.load(Ordering::Relaxed), 0);
}

/// The route is only the caller's mistake when no deployment could have served
/// it: a model that also has one that can isn't refused for its other one.
#[tokio::test]
async fn unsupported_route_with_another_deployment() {
    let (good, _) = upstream(Reply { status: 200, flavor: "openai", headers: &[] }).await;
    let down = dead_upstream().await;
    let body = json!({"model": "m", "input": "hi"});

    let gw = gateway("m", &[(&good, "anthropic", "none"), (&good, "openai", "none")]).await;
    let (status, h, _) = post(format!("{gw}/v1/embeddings"), body.clone()).await;
    assert_eq!(status, 200);
    assert_eq!(values(&h, "x-llmr-upstream"), ["up1"]);

    for deployments in [
        [(good.as_str(), "anthropic", "none"), (down.as_str(), "openai", "none")],
        [(down.as_str(), "openai", "none"), (good.as_str(), "anthropic", "none")],
    ] {
        let gw = gateway("m", &deployments).await;
        let (status, _, raw) = post(format!("{gw}/v1/embeddings"), body.clone()).await;
        assert_eq!(status, 502);
        let v: Value = serde_json::from_slice(&raw).unwrap();
        assert_eq!(v["error"]["code"], "upstream_unavailable");
    }
}

/// A provider that can't be reached at all, such as a refused workload identity
/// exchange, still leaves a usage row on both surfaces.
#[tokio::test]
async fn unreachable_provider_is_recorded() {
    let down = dead_upstream().await;
    for (path, model) in [("/v1/chat/completions", "wif-oai"), ("/v1/messages", "wif-anth")] {
        let gw = gateway(model, &[(&down, "anthropic", "anthropic_wif")]).await;
        let (status, _, raw) = post(
            format!("{gw}{path}"),
            json!({"model": model, "max_tokens": 5, "messages": [{"role": "user", "content": "hi"}]}),
        )
        .await;
        assert_eq!(status, 500, "{path}: {}", String::from_utf8_lossy(&raw));
        let events: Vec<_> =
            usage::take(usize::MAX, usize::MAX).into_iter().filter(|e| e.model_name == model).collect();
        assert_eq!(events.len(), 1, "{path}");
        assert_eq!((events[0].status_code, events[0].error_code), (500, "internal_error"), "{path}");
    }
}
