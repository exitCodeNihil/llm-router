//! LLMR_USAGE_CLIENT_IP end to end: the address a usage event names is the
//! connection's, or the one a trusted proxy forwarded for. Its own process:
//! recording, once on, stays on.

use std::sync::Arc;
use std::time::Duration;

use llmr_gateway::config::{Config, Wire};
use llmr_gateway::server::{self, State};
use llmr_gateway::{clientip, usage};
use serde_json::json;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};

const KEY: &str = "llmr_testkey";

/// Ask for a model that does not exist, with these headers; the client
/// address on the usage event of the refusal.
async fn asked(addr: std::net::SocketAddr, headers: &str) -> Option<String> {
    let body = r#"{"model":"nope","messages":[]}"#;
    let req = format!(
        "POST /v1/chat/completions HTTP/1.1\r\nhost: x\r\nauthorization: Bearer {KEY}\r\n{headers}\
         content-length: {}\r\nconnection: close\r\n\r\n{body}",
        body.len()
    );
    let mut c = TcpStream::connect(addr).await.unwrap();
    c.write_all(req.as_bytes()).await.unwrap();
    let mut out = Vec::new();
    c.read_to_end(&mut out).await.unwrap();
    assert!(out.starts_with(b"HTTP/1.1 404"), "{}", String::from_utf8_lossy(&out));
    let events = usage::take(usize::MAX, usize::MAX);
    assert_eq!(events.len(), 1);
    events[0].client_ip.as_deref().map(str::to_string)
}

#[tokio::test]
async fn usage_events_name_the_client_once_recording_is_on() {
    let digest = ring::digest::digest(&ring::digest::SHA256, KEY.as_bytes());
    let hash: String = digest.as_ref().iter().map(|b| format!("{b:02x}")).collect();
    let wire: Wire = serde_json::from_value(json!({
        "schema": 3, "version": 1, "group": {"id": "g", "name": "g"}, "keys": [{"hash": hash, "id": "k"}],
    }))
    .unwrap();
    let st: &'static State = Box::leak(Box::new(State {
        config: Default::default(),
        ready: Default::default(),
        draining: Default::default(),
        max_in_flight: 100,
    }));
    st.config.store(Some(Arc::new(Config::build(wire).unwrap())));
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(server::run(listener, st, std::future::pending(), Duration::from_secs(1)));

    // off, as it is unless asked for: no address, whatever the request says
    assert_eq!(asked(addr, "x-forwarded-for: 203.0.113.7\r\n").await, None);

    // on, trusting the proxy these requests come through (this machine)
    clientip::enable(clientip::parse_trusted("127.0.0.1, 10.0.0.0/8").unwrap());
    assert_eq!(asked(addr, "").await.as_deref(), Some("127.0.0.1"), "no header: the peer");
    assert_eq!(asked(addr, "x-forwarded-for: 203.0.113.7\r\n").await.as_deref(), Some("203.0.113.7"));
    let chain = "x-forwarded-for: 6.6.6.6, 203.0.113.7:4711\r\nx-forwarded-for: 10.1.1.1\r\n";
    assert_eq!(asked(addr, chain).await.as_deref(), Some("203.0.113.7"), "past the trusted hops, not further");
    assert_eq!(asked(addr, "x-forwarded-for: [2001:db8::9]:443\r\n").await.as_deref(), Some("2001:db8::9"));
}
