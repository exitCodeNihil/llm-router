//! The open-connections gauge over real sockets: up while a client holds a
//! connection, back down once it closes. Its own test binary, so no other
//! test's connections share the counters.

use std::sync::atomic::Ordering::Relaxed;
use std::time::Duration;

use llmr_gateway::metrics::{CONNECTIONS, OPEN_CONNECTIONS};
use llmr_gateway::server::{self, State};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};

async fn until(what: &str, f: impl Fn() -> bool) {
    for _ in 0..200 {
        if f() {
            return;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    panic!("timed out waiting for {what}");
}

#[tokio::test]
async fn open_connections_go_up_and_back_down() {
    let st: &'static State = Box::leak(Box::new(State {
        config: Default::default(),
        ready: Default::default(),
        draining: Default::default(),
        max_in_flight: 10,
    }));
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(server::run(listener, st, std::future::pending(), Duration::from_secs(1)));

    let mut clients = Vec::new();
    for _ in 0..3 {
        let mut c = TcpStream::connect(addr).await.unwrap();
        c.write_all(b"GET /metrics HTTP/1.1\r\nhost: x\r\n\r\n").await.unwrap();
        let mut buf = vec![0; 64 << 10];
        let n = c.read(&mut buf).await.unwrap();
        assert!(String::from_utf8_lossy(&buf[..n]).contains("llmr_open_connections"));
        clients.push(c);
    }
    until("3 open", || OPEN_CONNECTIONS.load(Relaxed) == 3).await;
    assert_eq!(CONNECTIONS.load(Relaxed), 3);

    clients.truncate(1);
    until("1 open", || OPEN_CONNECTIONS.load(Relaxed) == 1).await;
    drop(clients);
    until("none open", || OPEN_CONNECTIONS.load(Relaxed) == 0).await;
    assert_eq!(CONNECTIONS.load(Relaxed), 3, "accepted only ever counts up");
}
