//! Telemetry end to end over real sockets, against a stub upstream: what a
//! request's usage event carries for the control plane to export (trace,
//! verdict, captured content) and the X-Llmr-Trace-Id a caller gets back.
//! internal/gateway holds the same cases for the Go gateway.

use std::sync::Arc;

use bytes::Bytes;
use http::{HeaderMap, Request, Response, StatusCode};
use http_body_util::{BodyExt, Full};
use hyper::body::Incoming;
use hyper::service::service_fn;
use hyper_util::rt::{TokioExecutor, TokioIo};
use hyper_util::server::conn::auto;
use llmr_gateway::config::{Config, Wire};
use llmr_gateway::server::{self, State};
use llmr_gateway::usage::{self, Event};
use serde_json::{Value, json};
use tokio::net::TcpListener;

const KEY: &str = "llmr_telemetrykey";
const TRACEPARENT: &str = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01";
/// What every stub answer says, 100 bytes. No hex digits, so it can't turn
/// up in a request or trace id by chance.
const TEXT: &str =
    "pqrstuvwxyPQRSTUVWXYpqrstuvwxyPQRSTUVWXYpqrstuvwxyPQRSTUVWXYpqrstuvwxyPQRSTUVWXYpqrstuvwxyPQRSTUVWXY";

/// The usage queue is the process's: one test at a time takes from it.
static QUEUE: tokio::sync::Mutex<()> = tokio::sync::Mutex::const_new(());

fn payload(flavor: &str, stream: bool) -> String {
    let parts: Vec<&str> = TEXT.as_bytes().chunks(10).map(|c| std::str::from_utf8(c).unwrap()).collect();
    match (flavor, stream) {
        ("anthropic", true) => {
            let mut s = String::from(
                "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":3,\"inference_geo\":\"us\",\"speed\":\"standard\"}}}\n\n\
                 event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
            );
            for p in parts {
                s += &format!(
                    "event: content_block_delta\ndata: {{\"type\":\"content_block_delta\",\"index\":0,\"delta\":{{\"type\":\"text_delta\",\"text\":\"{p}\"}}}}\n\n"
                );
            }
            s + "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n\
                 event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\n\
                 event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
        }
        ("anthropic", false) => json!({
            "content": [{"type": "text", "text": &TEXT[..50]}, {"type": "tool_use", "id": "t", "name": "f", "input": {}},
                        {"type": "text", "text": &TEXT[50..]}],
            "stop_reason": "end_turn",
            "usage": {"input_tokens": 3, "output_tokens": 7, "inference_geo": "us"}})
        .to_string(),
        (_, true) => {
            let mut s = String::new();
            for p in parts {
                s += &format!("data: {{\"choices\":[{{\"index\":0,\"delta\":{{\"content\":\"{p}\"}}}}]}}\n\n");
            }
            s + "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":7}}\n\ndata: [DONE]\n\n"
        }
        (_, false) => json!({"choices": [{"message": {"role": "assistant", "content": TEXT}, "finish_reason": "stop"}],
                             "usage": {"prompt_tokens": 3, "completion_tokens": 7}})
        .to_string(),
    }
}

/// A stub upstream answering in `flavor`; its base URL.
async fn upstream(flavor: &'static str) -> String {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("http://{}", listener.local_addr().unwrap());
    tokio::spawn(async move {
        while let Ok((conn, _)) = listener.accept().await {
            tokio::spawn(async move {
                let svc = service_fn(move |req: Request<Incoming>| async move {
                    let raw = req.into_body().collect().await.unwrap().to_bytes();
                    let v: Value = serde_json::from_slice(&raw).unwrap_or_default();
                    let body = payload(flavor, v["stream"] == true);
                    Ok::<_, std::convert::Infallible>(Response::new(Full::new(Bytes::from(body))))
                });
                let _ = auto::Builder::new(TokioExecutor::new()).serve_connection(TokioIo::new(conn), svc).await;
            });
        }
    });
    url
}

/// A gateway serving `model` from one deployment of `flavor`, in team "A" of
/// a group with `data_rule`, under `telemetry`. Returns its base URL.
async fn gateway(model: &str, flavor: &str, data_rule: &str, telemetry: Value) -> String {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let up = upstream(if flavor == "anthropic" { "anthropic" } else { "openai" }).await;
    let digest = ring::digest::digest(&ring::digest::SHA256, KEY.as_bytes());
    let hash: String = digest.as_ref().iter().map(|b| format!("{b:02x}")).collect();
    let wire: Wire = serde_json::from_value(json!({
        "schema": 2, "version": 1, "group": {"id": "g", "name": "g", "data_rule": data_rule},
        "keys": [{"hash": hash, "id": "k", "team_id": "A", "allowed_models": null, "tags": ["prod"]}],
        "teams": [{"id": "A", "allowed_models": null}],
        "providers": [{"id": "p", "name": "p", "type": "openai_compatible", "base_url": up, "auth_mode": "none"}],
        "deployments": [{"id": "d", "provider_id": "p", "model_name": model, "upstream_name": "up", "api_flavor": flavor}],
        "telemetry": telemetry,
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

fn capturing(max_bytes: usize) -> Value {
    json!({"mode": "everything", "capture_content": true, "rules": [],
           "content": [{"teams": {"mode": "all", "values": []}, "max_bytes": max_bytes}]})
}

async fn call(url: String, body: &str, traceparent: Option<&str>) -> (StatusCode, HeaderMap, Bytes) {
    let mut req = Request::post(url).header("authorization", format!("Bearer {KEY}"));
    if let Some(t) = traceparent {
        req = req.header("traceparent", t);
    }
    let resp =
        server::https_client(1).request(req.body(Full::new(Bytes::from(body.to_string()))).unwrap()).await.unwrap();
    let (parts, body) = resp.into_parts();
    (parts.status, parts.headers, body.collect().await.unwrap().to_bytes())
}

fn events(model: &str) -> Vec<Event> {
    let (mine, rest): (Vec<Event>, Vec<Event>) =
        usage::take(usize::MAX, usize::MAX).into_iter().partition(|e| e.model_name == model);
    usage::requeue(rest);
    mine
}

/// The prompt's text is a marker no id can contain either.
const PROMPT: &str = "zqxprompt";
const MESSAGES: &str = r#"[{"role":"user", "content":"zqxprompt"}]"#;
const SYSTEM: &str = r#""be brief""#;

/// Input, system and output, each within the cap, on both surfaces, from both
/// kinds of upstream, streamed or not; and the caller's trace throughout.
#[tokio::test]
async fn captures_content_within_the_cap() {
    let _q = QUEUE.lock().await;
    let cap = 64;
    for path in ["/v1/chat/completions", "/v1/messages"] {
        for flavor in ["openai", "anthropic"] {
            for stream in [false, true] {
                let model = format!("cap-{}-{flavor}-{stream}", path.len());
                let what = format!("{path} {flavor} stream={stream}");
                let gw = gateway(&model, flavor, "inherit", capturing(cap)).await;
                let system = if path == "/v1/messages" { format!(r#""system": {SYSTEM},"#) } else { String::new() };
                let body = format!(
                    r#"{{"model": "{model}", {system} "max_tokens": 5, "stream": {stream}, "messages": {MESSAGES}}}"#
                );
                let (status, h, raw) = call(format!("{gw}{path}"), &body, Some(TRACEPARENT)).await;
                assert_eq!(status, 200, "{what}: {}", String::from_utf8_lossy(&raw));
                assert_eq!(h["x-llmr-trace-id"], "4bf92f3577b34da6a3ce929d0e0e4736", "{what}");
                assert!(h["access-control-expose-headers"].to_str().unwrap().contains("X-Llmr-Trace-Id"), "{what}");

                let evs = events(&model);
                assert_eq!(evs.len(), 1, "{what}");
                let e = &evs[0];
                assert_eq!(u128::from_be_bytes(e.trace_id), 0x4bf92f3577b34da6a3ce929d0e0e4736, "{what}");
                assert_eq!(e.parent_span_id, 0x00f067aa0ba902b7, "{what}");
                assert_eq!((e.operation.as_str(), e.cost_basis), ("chat", 1), "{what}");
                if flavor == "anthropic" {
                    assert_eq!(e.reported.as_ref().unwrap().inference_geo.as_str(), "us", "{what}");
                }
                let c = e.content.as_ref().unwrap_or_else(|| panic!("{what}: no content"));
                // the request's own bytes, spaces and all
                assert_eq!(c.input.as_ref().unwrap().get(), MESSAGES, "{what}");
                assert_eq!(c.system.as_ref().map(|s| s.get()), (path == "/v1/messages").then_some(SYSTEM), "{what}");
                assert_eq!(c.output, &TEXT[..cap], "{what}");
                assert!(c.content_truncated, "{what}");

                let v: Value = serde_json::to_value(e).unwrap();
                assert_eq!(v["trace_id"], "4bf92f3577b34da6a3ce929d0e0e4736", "{what}");
                assert_eq!(v["input"][0]["content"], PROMPT, "{what}");
                assert_eq!(v["output"], &TEXT[..cap], "{what}");
                assert_eq!(v["content_truncated"], true, "{what}");
            }
        }
    }
}

/// Content leaves only when the policy captures it, the group's data rule
/// lets it, and a destination in scope wants it; mirrors TestContentLeakTable.
#[tokio::test]
async fn content_leak_table() {
    let _q = QUEUE.lock().await;
    for capture in [false, true] {
        for rule in ["inherit", "no_content", "no_export"] {
            for scope in [false, true] {
                let model = format!("leak-{capture}-{rule}-{scope}");
                let content = if scope {
                    json!([{"teams": {"mode": "all", "values": null}, "max_bytes": 4096}])
                } else {
                    json!([])
                };
                let tel = json!({"mode": "everything", "capture_content": capture, "content": content});
                let gw = gateway(&model, "openai", rule, tel).await;
                for stream in [false, true] {
                    let what = format!("{model} stream={stream}");
                    let body = format!(r#"{{"model":"{model}","stream":{stream},"messages":{MESSAGES}}}"#);
                    let (status, _, _) = call(format!("{gw}/v1/chat/completions"), &body, None).await;
                    assert_eq!(status, 200, "{what}");
                    let evs = events(&model);
                    let e = &evs[0];
                    let want = capture && rule == "inherit" && scope;
                    assert_eq!(e.content.is_some(), want, "{what}");
                    if want {
                        assert_eq!(e.content.as_ref().unwrap().output, TEXT, "{what}");
                    }
                    let raw = serde_json::to_string(e).unwrap();
                    assert_eq!(raw.contains(PROMPT) || raw.contains(&TEXT[..10]), want, "{what}: {raw}");
                }
            }
        }
    }
}

/// A caller's traceparent places the request in its trace but decides nothing:
/// an unsampled one can't opt out of export or capture. An invalid one starts
/// a new trace.
#[tokio::test]
async fn traceparent_places_but_never_decides() {
    let _q = QUEUE.lock().await;
    let tel = json!({"mode": "by_rule", "rules": [
        {"id": "r-prod", "order": 1, "scope_type": "tag", "scope_value": "prod", "capture": "content", "sample": 100, "enabled": true}],
        "content": [{"teams": {"mode": "selected", "values": ["A"]}, "max_bytes": 4096}]});
    let model = "traced";
    let gw = gateway(model, "anthropic", "inherit", tel).await;
    let body = format!(r#"{{"model":"{model}","max_tokens":5,"messages":{MESSAGES}}}"#);

    let unsampled = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00";
    let (_, h, _) = call(format!("{gw}/v1/messages"), &body, Some(unsampled)).await;
    assert_eq!(h["x-llmr-trace-id"], "4bf92f3577b34da6a3ce929d0e0e4736");
    let e = &events(model)[0];
    assert_eq!(
        (u128::from_be_bytes(e.trace_id), e.parent_span_id),
        (0x4bf92f3577b34da6a3ce929d0e0e4736, 0x00f067aa0ba902b7)
    );
    assert!(e.content.is_some(), "the policy decides, not the caller");

    let (_, h, _) = call(format!("{gw}/v1/messages"), &body, Some("00-nonsense")).await;
    let id = h["x-llmr-trace-id"].to_str().unwrap().to_string();
    assert!(id.len() == 32 && id.bytes().all(|b| b.is_ascii_hexdigit() && !b.is_ascii_uppercase()), "{id}");
    assert_ne!(id, "4bf92f3577b34da6a3ce929d0e0e4736");
    let e = &events(model)[0];
    assert_eq!(format!("{:032x}", u128::from_be_bytes(e.trace_id)), id);
    assert_eq!(e.parent_span_id, 0);
    assert!(e.content.is_some(), "a tag rule with content, sampled at 100");

    // Every authenticated response names its trace, not only the proxied ones.
    let req = Request::get(format!("{gw}/v1/models")).header("x-llmr-key", KEY).body(Full::new(Bytes::new())).unwrap();
    let resp = server::https_client(1).request(req).await.unwrap();
    assert_eq!(resp.status(), 200);
    assert_eq!(resp.headers()["x-llmr-trace-id"].len(), 32);
    let req = Request::get(format!("{gw}/v1/models"))
        .header("x-llmr-key", "llmr_wrong")
        .body(Full::new(Bytes::new()))
        .unwrap();
    let resp = server::https_client(1).request(req).await.unwrap();
    assert_eq!(resp.status(), 401);
    assert!(!resp.headers().contains_key("x-llmr-trace-id"));
}

/// With capture off nothing is held: no content on the event at all.
#[tokio::test]
async fn capture_off_captures_nothing() {
    let _q = QUEUE.lock().await;
    for (flavor, path) in [("openai", "/v1/chat/completions"), ("anthropic", "/v1/messages")] {
        let model = format!("off-{flavor}");
        let gw = gateway(&model, flavor, "inherit", json!({"mode": "everything"})).await;
        for stream in [false, true] {
            let body = format!(r#"{{"model":"{model}","max_tokens":5,"stream":{stream},"messages":{MESSAGES}}}"#);
            let (status, _, _) = call(format!("{gw}{path}"), &body, None).await;
            assert_eq!(status, 200);
            let e = &events(&model)[0];
            assert!(e.content.is_none(), "{model} stream={stream}");
        }
    }
}
