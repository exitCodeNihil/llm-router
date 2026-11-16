//! Over the in-flight limit the gateway sheds API requests, but still answers
//! its probes and the metrics scrape. Its own test binary: the counters are
//! process-wide.

use std::sync::atomic::Ordering::Relaxed;
use std::time::Duration;

use llmr_gateway::metrics::SHED;
use llmr_gateway::server::{self, State};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};

async fn send(addr: std::net::SocketAddr, request: &str) -> String {
    let mut c = TcpStream::connect(addr).await.unwrap();
    c.write_all(request.as_bytes()).await.unwrap();
    let mut out = Vec::new();
    let _ = tokio::time::timeout(Duration::from_secs(5), c.read_to_end(&mut out)).await;
    String::from_utf8_lossy(&out).into_owned()
}

#[tokio::test]
async fn probes_and_metrics_are_not_shed() {
    // A limit of 0 puts every request over it.
    let st: &'static State = Box::leak(Box::new(State {
        config: Default::default(),
        ready: Default::default(),
        draining: Default::default(),
        max_in_flight: 0,
    }));
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(server::run(listener, st, std::future::pending(), Duration::from_secs(1)));

    let get = |path: &str| format!("GET {path} HTTP/1.1\r\nhost: x\r\nconnection: close\r\n\r\n");
    let live = send(addr, &get("/livez")).await;
    assert!(live.starts_with("HTTP/1.1 200"), "liveness probe was refused: {live}");
    let ready = send(addr, &get("/healthz")).await;
    assert!(ready.contains("starting") && !ready.contains("overloaded"), "readiness probe was shed: {ready}");
    let scrape = send(addr, &get("/metrics")).await;
    assert!(scrape.starts_with("HTTP/1.1 200") && scrape.contains("llmr_shed_total"), "scrape was refused: {scrape}");
    assert_eq!(SHED.load(Relaxed), 0, "probes and scrapes are not counted as shed");

    let body = r#"{"model":"m","messages":[]}"#;
    let chat = send(
        addr,
        &format!(
            "POST /v1/chat/completions HTTP/1.1\r\nhost: x\r\nconnection: close\r\ncontent-type: application/json\r\ncontent-length: {}\r\n\r\n{body}",
            body.len()
        ),
    )
    .await;
    assert!(chat.starts_with("HTTP/1.1 503") && chat.contains("overloaded"), "API request was not shed: {chat}");
    assert_eq!(SHED.load(Relaxed), 1);
}
