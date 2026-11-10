//! Shutdown over real sockets: a stream in flight finishes, while an idle
//! keep-alive connection is closed at once, so its client reconnects to
//! another gateway instead of sending this one requests until the drain runs
//! out. Its own test binary, as server::run counts global in-flight requests.

use std::sync::Arc;
use std::time::{Duration, Instant};

use llmr_gateway::config::{Config, Wire};
use llmr_gateway::server::{self, State};
use serde_json::json;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::oneshot;

const KEY: &str = "llmr_testkey";
const CHUNKS: usize = 5;

/// An upstream that streams CHUNKS OpenAI deltas 100 ms apart, then [DONE].
async fn slow_upstream() -> String {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("http://{}", listener.local_addr().unwrap());
    tokio::spawn(async move {
        while let Ok((mut conn, _)) = listener.accept().await {
            tokio::spawn(async move {
                let mut buf = vec![0; 64 << 10];
                let _ = conn.read(&mut buf).await;
                let head = "HTTP/1.1 200 OK\r\ncontent-type: text/event-stream\r\ntransfer-encoding: chunked\r\n\r\n";
                conn.write_all(head.as_bytes()).await.unwrap();
                let chunk = |data: &str| format!("{:x}\r\n{data}\r\n", data.len());
                for _ in 0..CHUNKS {
                    let d = chunk("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n");
                    conn.write_all(d.as_bytes()).await.unwrap();
                    tokio::time::sleep(Duration::from_millis(100)).await;
                }
                conn.write_all(chunk("data: [DONE]\n\n").as_bytes()).await.unwrap();
                conn.write_all(b"0\r\n\r\n").await.unwrap();
            });
        }
    });
    url
}

fn state(upstream: &str) -> &'static State {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let digest = ring::digest::digest(&ring::digest::SHA256, KEY.as_bytes());
    let hash: String = digest.as_ref().iter().map(|b| format!("{b:02x}")).collect();
    let wire: Wire = serde_json::from_value(json!({
        "schema": 1, "version": 1, "group": {"id": "g", "name": "g"},
        "keys": [{"hash": hash, "id": "k"}],
        "providers": [{"id": "p", "name": "p", "type": "openai_compatible", "base_url": upstream, "auth_mode": "none"}],
        "deployments": [{"id": "d", "provider_id": "p", "model_name": "m", "upstream_name": "up", "api_flavor": "openai"}],
    }))
    .unwrap();
    let st: &'static State = Box::leak(Box::new(State {
        config: Default::default(),
        ready: Default::default(),
        draining: Default::default(),
        max_in_flight: 100,
    }));
    st.config.store(Some(Arc::new(Config::build(wire).unwrap())));
    st
}

/// Reads until the peer closes, or panics after `within`.
async fn read_to_close(c: &mut TcpStream, within: Duration) -> String {
    let mut out = Vec::new();
    tokio::time::timeout(within, c.read_to_end(&mut out)).await.expect("connection still open").unwrap();
    String::from_utf8_lossy(&out).into_owned()
}

#[tokio::test]
async fn shutdown_finishes_streams_and_closes_idle_connections() {
    let st = state(&slow_upstream().await);
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let (stop, stopped) = oneshot::channel::<()>();
    let server = tokio::spawn(server::run(
        listener,
        st,
        async {
            let _ = stopped.await;
        },
        Duration::from_secs(10),
    ));

    // A client that has finished a request and keeps the connection for its next one.
    let mut idle = TcpStream::connect(addr).await.unwrap();
    idle.write_all(b"GET /metrics HTTP/1.1\r\nhost: x\r\n\r\n").await.unwrap();
    let mut buf = vec![0; 64 << 10];
    assert!(idle.read(&mut buf).await.unwrap() > 0);

    // A stream in flight.
    let mut streaming = TcpStream::connect(addr).await.unwrap();
    let body = r#"{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}"#;
    let req = format!(
        "POST /v1/chat/completions HTTP/1.1\r\nhost: x\r\nauthorization: Bearer {KEY}\r\n\
         content-type: application/json\r\ncontent-length: {}\r\n\r\n{body}",
        body.len()
    );
    streaming.write_all(req.as_bytes()).await.unwrap();
    let n = streaming.read(&mut buf).await.unwrap();
    let mut seen = String::from_utf8_lossy(&buf[..n]).into_owned();
    assert!(seen.starts_with("HTTP/1.1 200"), "{seen}");

    let began = Instant::now();
    stop.send(()).unwrap();
    assert_eq!(read_to_close(&mut idle, Duration::from_secs(1)).await, "", "the idle connection closes at once");
    seen += &read_to_close(&mut streaming, Duration::from_secs(5)).await;
    assert_eq!(seen.matches("\"content\":\"hi\"").count(), CHUNKS, "{seen}");
    assert!(seen.contains("[DONE]"), "the stream finishes: {seen}");
    tokio::time::timeout(Duration::from_secs(2), server).await.expect("run returns once drained").unwrap();
    assert!(began.elapsed() < Duration::from_secs(3), "drained in {:?}, not the full drain", began.elapsed());
    assert!(TcpStream::connect(addr).await.is_err(), "no longer accepting");
}
