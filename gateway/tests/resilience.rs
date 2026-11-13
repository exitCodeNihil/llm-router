//! What a gateway answers when it may not serve the config it holds: one the
//! control plane has not confirmed within LLMR_MAX_CONFIG_AGE, or after the
//! control plane refused it. /livez answers throughout.

use std::sync::Arc;
use std::sync::atomic::Ordering;
use std::time::Duration;

use llmr_gateway::config::{Config, Wire};
use llmr_gateway::server::{self, State};
use serde_json::json;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};

const KEY: &str = "llmr_testkey";

/// The status, head and body of one GET.
async fn get(addr: std::net::SocketAddr, path: &str) -> (u16, String, String) {
    let mut c = TcpStream::connect(addr).await.unwrap();
    let req = format!("GET {path} HTTP/1.1\r\nhost: x\r\nauthorization: Bearer {KEY}\r\nconnection: close\r\n\r\n");
    c.write_all(req.as_bytes()).await.unwrap();
    let mut out = Vec::new();
    c.read_to_end(&mut out).await.unwrap();
    let out = String::from_utf8_lossy(&out).into_owned();
    let (head, body) = out.split_once("\r\n\r\n").unwrap();
    (head[9..12].parse().unwrap(), head.to_ascii_lowercase(), body.to_string())
}

#[tokio::test]
async fn a_stale_or_refused_gateway_answers_503_and_stays_alive() {
    let digest = ring::digest::digest(&ring::digest::SHA256, KEY.as_bytes());
    let hash: String = digest.as_ref().iter().map(|b| format!("{b:02x}")).collect();
    let wire: Wire = serde_json::from_value(json!({
        "schema": 1, "version": 1, "group": {"id": "g", "name": "g"},
        "keys": [{"hash": hash, "id": "k"}],
    }))
    .unwrap();
    let st: &'static State = Box::leak(Box::new(State {
        config: Default::default(),
        ready: Default::default(),
        draining: Default::default(),
        max_in_flight: 100,
    }));
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(server::run(listener, st, std::future::pending(), Duration::from_secs(1)));

    // Starting: alive, not ready.
    assert_eq!(get(addr, "/livez").await.0, 200);
    assert_eq!(get(addr, "/healthz").await.0, 503);
    let (status, _, body) = get(addr, "/v1/models").await;
    assert!(status == 503 && body.contains("starting_up"), "{body}");
    assert!(get(addr, "/metrics").await.2.contains("\nllmr_config_age_seconds 0\n"));

    st.config.store(Some(Arc::new(Config::build(wire).unwrap())));
    st.ready.set(true);
    st.ready.config_ok();
    assert_eq!(get(addr, "/healthz").await.0, 200);
    assert_eq!(get(addr, "/v1/models").await.0, 200);

    // Without LLMR_MAX_CONFIG_AGE a config of any age serves.
    st.ready.config_ok_ago(Duration::from_secs(7 * 24 * 3600));
    assert_eq!(get(addr, "/healthz").await.0, 200);
    assert_eq!(get(addr, "/v1/models").await.0, 200);
    let metrics = get(addr, "/metrics").await.2;
    assert!(metrics.contains("\nllmr_config_age_seconds 604800\n") && metrics.contains("\nllmr_config_stale 0\n"));
    assert!(!metrics.contains("llmr_state_dir_locked"), "no state directory, no gauge");
    assert!(metrics.contains("\nllmr_refused 0\n"));

    // With it, a config last confirmed longer ago is not served.
    st.ready.set_max_config_age(Some(Duration::from_secs(120)));
    st.ready.config_ok_ago(Duration::from_secs(119));
    assert_eq!(get(addr, "/v1/models").await.0, 200);
    st.ready.config_ok_ago(Duration::from_secs(121));
    let (status, head, body) = get(addr, "/v1/models").await;
    assert!(status == 503 && body.contains("\"code\":\"config_stale\""), "{body}");
    assert!(head.contains("retry-after: 5"), "{head}");
    assert_eq!(get(addr, "/healthz").await.0, 503);
    assert_eq!(get(addr, "/livez").await.0, 200);
    assert!(get(addr, "/metrics").await.2.contains("\nllmr_config_stale 1\n"));
    assert!(st.config.load().is_some(), "the config stays in memory");

    // The next answered poll resumes serving.
    st.ready.config_ok();
    assert_eq!(get(addr, "/healthz").await.0, 200);
    assert_eq!(get(addr, "/v1/models").await.0, 200);
    assert!(get(addr, "/metrics").await.2.contains("\nllmr_config_stale 0\n"));

    // Refused by the control plane: the config is gone, and the answer says why.
    st.ready.refused.store(true, Ordering::Relaxed);
    st.ready.set(false);
    st.config.store(None);
    let (status, head, body) = get(addr, "/v1/models").await;
    assert!(status == 503 && body.contains("\"code\":\"gateway_refused\"") && body.contains("refused"), "{body}");
    assert!(head.contains("retry-after: 30"), "{head}");
    assert_eq!(get(addr, "/healthz").await.0, 503);
    assert_eq!(get(addr, "/livez").await.0, 200);
    assert!(get(addr, "/metrics").await.2.contains("\nllmr_refused 1\n"));
}
