//! Budget reservations end to end, over real sockets against a stub
//! upstream: requests in progress hold what they are expected to cost, a
//! request the held budget refuses gets a 429 it can retry and uses no
//! rate-limit slot, and the hold is given back however the request ends.

use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::{Duration, Instant};

use llmr_gateway::config::{Config, Wire};
use llmr_gateway::limits;
use llmr_gateway::server::{self, State};
use serde_json::{Value, json};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};

/// The reservation gauge is one per process: one test at a time.
static SERIAL: tokio::sync::Mutex<()> = tokio::sync::Mutex::const_new(());

/// Read one HTTP request: its head and its body.
async fn read_request(conn: &mut TcpStream) -> Option<(String, Vec<u8>)> {
    let mut buf = Vec::new();
    let head_end = loop {
        if let Some(at) = buf.windows(4).position(|w| w == b"\r\n\r\n") {
            break at + 4;
        }
        let mut chunk = [0u8; 4096];
        match conn.read(&mut chunk).await {
            Ok(n) if n > 0 => buf.extend_from_slice(&chunk[..n]),
            _ => return None,
        }
    };
    let head = String::from_utf8_lossy(&buf[..head_end]).to_ascii_lowercase();
    let length: usize =
        head.lines().find_map(|l| l.strip_prefix("content-length:")).and_then(|v| v.trim().parse().ok()).unwrap_or(0);
    while buf.len() < head_end + length {
        let mut chunk = [0u8; 4096];
        match conn.read(&mut chunk).await {
            Ok(n) if n > 0 => buf.extend_from_slice(&chunk[..n]),
            _ => return None,
        }
    }
    Some((head, buf[head_end..].to_vec()))
}

/// A stub upstream that takes `delay` to answer, with 100 completion tokens:
/// a JSON body, or for a streamed request its first event at once and the
/// rest after the delay. Returns its base URL and how many requests reached it.
async fn upstream(delay: Duration) -> (String, Arc<AtomicUsize>) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("http://{}", listener.local_addr().unwrap());
    let hits = Arc::new(AtomicUsize::new(0));
    let counted = hits.clone();
    tokio::spawn(async move {
        while let Ok((mut conn, _)) = listener.accept().await {
            let hits = counted.clone();
            tokio::spawn(async move {
                let Some((_, body)) = read_request(&mut conn).await else { return };
                hits.fetch_add(1, Ordering::Relaxed);
                let v: Value = serde_json::from_slice(&body).unwrap_or_default();
                if v["stream"] == true {
                    let chunk = |data: &str| format!("{:x}\r\n{data}\r\n", data.len());
                    let head =
                        "HTTP/1.1 200 OK\r\ncontent-type: text/event-stream\r\ntransfer-encoding: chunked\r\n\r\n";
                    let first = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n";
                    let _ = conn.write_all(format!("{head}{}", chunk(first)).as_bytes()).await;
                    tokio::time::sleep(delay).await;
                    let last = "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":100}}\n\ndata: [DONE]\n\n";
                    let _ = conn.write_all(format!("{}0\r\n\r\n", chunk(last)).as_bytes()).await;
                } else {
                    tokio::time::sleep(delay).await;
                    let body = r#"{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":100}}"#;
                    let resp = format!(
                        "HTTP/1.1 200 OK\r\ncontent-type: application/json\r\ncontent-length: {}\r\n\r\n{body}",
                        body.len()
                    );
                    let _ = conn.write_all(resp.as_bytes()).await;
                }
            });
        }
    });
    (url, hits)
}

/// A gateway with one key, `llmr_<name>`, limited as `limit` says, and two
/// models served by `upstream`: "priced" at $1000 per million output tokens
/// (input is free), and "free", which has no price.
async fn gateway(name: &str, upstream: &str, limit: Value) -> std::net::SocketAddr {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let digest = ring::digest::digest(&ring::digest::SHA256, format!("llmr_{name}").as_bytes());
    let hash: String = digest.as_ref().iter().map(|b| format!("{b:02x}")).collect();
    let mut key = json!({"hash": hash, "id": name});
    key.as_object_mut().unwrap().extend(limit.as_object().unwrap().clone());
    let wire: Wire = serde_json::from_value(json!({
        "schema": 3, "version": 1, "group": {"id": "g", "name": "g"},
        "keys": [key],
        "providers": [{"id": "p", "name": "p", "type": "openai_compatible", "base_url": upstream, "auth_mode": "none"}],
        "deployments": [
            {"id": format!("{name}-priced"), "provider_id": "p", "model_name": "priced", "upstream_name": "up",
             "input_per_1m": 0.0, "output_per_1m": 1000.0},
            {"id": format!("{name}-free"), "provider_id": "p", "model_name": "free", "upstream_name": "up"},
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
    st.ready.set(true);
    st.ready.config_ok();
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(server::run(listener, st, std::future::pending(), Duration::from_secs(1)));
    addr
}

/// A chat request of 400 output tokens at most: expected to cost $0.40 on
/// the priced model, and costing $0.10 as the stub answers it.
fn chat(key: &str, model: &str, stream: bool) -> Vec<u8> {
    let body = json!({"model": model, "max_tokens": 400, "stream": stream,
        "messages": [{"role": "user", "content": "hello"}]})
    .to_string();
    format!(
        "POST /v1/chat/completions HTTP/1.1\r\nhost: x\r\nauthorization: Bearer llmr_{key}\r\n\
         content-type: application/json\r\ncontent-length: {}\r\nconnection: close\r\n\r\n{body}",
        body.len()
    )
    .into_bytes()
}

/// Send a request and read the whole answer: its status, head and body.
async fn send(addr: std::net::SocketAddr, request: Vec<u8>) -> (u16, String, String) {
    let mut c = TcpStream::connect(addr).await.unwrap();
    c.write_all(&request).await.unwrap();
    let mut out = Vec::new();
    c.read_to_end(&mut out).await.unwrap();
    let out = String::from_utf8_lossy(&out).into_owned();
    let (head, body) = out.split_once("\r\n\r\n").unwrap();
    (head[9..12].parse().unwrap(), head.to_ascii_lowercase(), body.to_string())
}

async fn until(what: &str, cond: impl Fn() -> bool) {
    let deadline = Instant::now() + Duration::from_secs(10);
    while !cond() {
        assert!(Instant::now() < deadline, "timed out waiting until {what}");
        tokio::time::sleep(Duration::from_millis(5)).await;
    }
}

fn close(a: f64, b: f64) -> bool {
    (a - b).abs() < 1e-9
}

async fn metric(addr: std::net::SocketAddr, name: &str) -> f64 {
    let (_, _, body) = send(addr, b"GET /metrics HTTP/1.1\r\nhost: x\r\nconnection: close\r\n\r\n".to_vec()).await;
    let line = body.lines().find(|l| l.starts_with(name) && l[name.len()..].starts_with(' ')).unwrap();
    line[name.len()..].trim().parse().unwrap()
}

// A $1 budget and requests expected to cost $0.40: three run at once (the
// third is admitted with $0.80 held by the others), the rest are told to
// retry, without using the key's rate limit. What the three really cost is
// what is spent.
#[tokio::test]
async fn requests_in_progress_hold_the_budget_and_the_rest_retry() {
    let _serial = SERIAL.lock().await;
    let (up, hits) = upstream(Duration::from_millis(600)).await;
    let addr = gateway("res-hold", &up, json!({"budget_usd": 1.0, "rpm_limit": 4})).await;
    let refusals = metric(addr, "llmr_budget_reserved_refusals_total").await;
    assert!(close(limits::reserved_usd(), 0.0));

    let running: Vec<_> = (0..3).map(|_| tokio::spawn(send(addr, chat("res-hold", "priced", false)))).collect();
    until("three are with the upstream", || hits.load(Ordering::Relaxed) == 3).await;
    assert!(close(limits::reserved_usd(), 1.2), "{}", limits::reserved_usd());
    assert!(close(metric(addr, "llmr_budget_reserved_usd").await, 1.2));

    let refused = futures(7, || send(addr, chat("res-hold", "priced", false))).await;
    for (status, head, body) in &refused {
        assert_eq!(*status, 429, "{body}");
        assert!(body.contains("\"code\":\"budget_reserved\"") && body.contains("requests in progress"), "{body}");
        assert!(head.contains("retry-after: 1\r\n") || head.ends_with("retry-after: 1"), "{head}");
    }
    assert_eq!(hits.load(Ordering::Relaxed), 3, "a refused request never reaches the upstream");
    assert!(close(metric(addr, "llmr_budget_reserved_refusals_total").await - refusals, 7.0));
    assert!(close(limits::reserved_usd(), 1.2), "a refusal holds nothing");

    for done in running {
        assert_eq!(done.await.unwrap().0, 200);
    }
    assert!(close(limits::reserved_usd(), 0.0), "{}", limits::reserved_usd());

    // $0.30 is spent, nothing held. The seven refusals used no rate-limit
    // slot: of the four a minute, the fourth is still there, the fifth is not.
    assert_eq!(send(addr, chat("res-hold", "priced", false)).await.0, 200);
    let (status, _, body) = send(addr, chat("res-hold", "priced", false)).await;
    assert!(status == 429 && body.contains("\"code\":\"rate_limit_exceeded\""), "{body}");
    assert!(close(limits::reserved_usd(), 0.0), "a request the rate limit refuses holds nothing");
}

async fn futures<F: Future<Output = (u16, String, String)> + Send + 'static>(
    n: usize,
    make: impl Fn() -> F,
) -> Vec<(u16, String, String)> {
    let tasks: Vec<_> = (0..n).map(|_| tokio::spawn(make())).collect();
    let mut out = Vec::new();
    for t in tasks {
        out.push(t.await.unwrap());
    }
    out
}

// However many arrive at once, the budget is overshot by at most the one
// request admitted last: with $0.10 answers on a $0.35 budget, spend ends
// between $0.35 and $0.45.
#[tokio::test]
async fn a_burst_cannot_overshoot_the_budget_by_more_than_one_request() {
    let _serial = SERIAL.lock().await;
    let (up, hits) = upstream(Duration::from_millis(20)).await;
    let addr = gateway("res-burst", &up, json!({"budget_usd": 0.35})).await;
    let mut served = 0;
    let mut exhausted = false;
    for _ in 0..20 {
        for (status, _, body) in futures(16, || send(addr, chat("res-burst", "priced", false))).await {
            match status {
                200 => served += 1,
                429 if body.contains("\"code\":\"budget_exceeded\"") => exhausted = true,
                429 => assert!(body.contains("\"code\":\"budget_reserved\""), "{body}"),
                other => panic!("{other}: {body}"),
            }
        }
        if exhausted {
            break;
        }
    }
    assert!(exhausted, "the budget runs out");
    // each costs $0.10: four reach $0.40, past the budget by less than one request
    assert_eq!(served, 4);
    assert_eq!(hits.load(Ordering::Relaxed), 4);
    assert!(close(limits::reserved_usd(), 0.0));
}

// A stream holds its reservation until it ends; a client that goes away, in
// the middle of a stream or before any answer, gives it back at once, and so
// does an upstream that cannot be reached.
#[tokio::test]
async fn a_reservation_ends_with_its_request() {
    let _serial = SERIAL.lock().await;
    let (up, hits) = upstream(Duration::from_secs(1)).await;
    let addr = gateway("res-end", &up, json!({"budget_usd": 0.4})).await;

    // streaming: held while it runs
    let stream = tokio::spawn(send(addr, chat("res-end", "priced", true)));
    until("the stream is with the upstream", || hits.load(Ordering::Relaxed) == 1).await;
    assert!(close(limits::reserved_usd(), 0.4));
    let (status, _, body) = send(addr, chat("res-end", "priced", false)).await;
    assert!(status == 429 && body.contains("\"code\":\"budget_reserved\""), "{body}");
    let (status, _, body) = stream.await.unwrap();
    assert!(status == 200 && body.contains("[DONE]"), "{body}");
    assert!(close(limits::reserved_usd(), 0.0), "given back when the stream ends");

    // a client that goes away mid-stream ($0.10 of $0.40 is spent by now)
    let mut c = TcpStream::connect(addr).await.unwrap();
    c.write_all(&chat("res-end", "priced", true)).await.unwrap();
    let mut first = [0u8; 16];
    c.read_exact(&mut first).await.unwrap();
    assert!(close(limits::reserved_usd(), 0.4));
    let gone = Instant::now();
    drop(c);
    until("the abandoned stream gives its reservation back", || close(limits::reserved_usd(), 0.0)).await;
    assert!(gone.elapsed() < Duration::from_millis(700), "without waiting for the upstream: {:?}", gone.elapsed());

    // a client that goes away before the upstream has answered
    let before = hits.load(Ordering::Relaxed);
    let mut c = TcpStream::connect(addr).await.unwrap();
    c.write_all(&chat("res-end", "priced", false)).await.unwrap();
    until("it is with the upstream", || hits.load(Ordering::Relaxed) == before + 1).await;
    assert!(close(limits::reserved_usd(), 0.4));
    let gone = Instant::now();
    drop(c);
    until("the abandoned request gives its reservation back", || close(limits::reserved_usd(), 0.0)).await;
    assert!(gone.elapsed() < Duration::from_millis(700), "without waiting for the upstream: {:?}", gone.elapsed());

    // an upstream that cannot be reached: the failure gives it back
    let dead = {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        format!("http://{}", listener.local_addr().unwrap())
    };
    let addr = gateway("res-dead", &dead, json!({"budget_usd": 0.4})).await;
    for _ in 0..3 {
        let (status, _, body) = send(addr, chat("res-dead", "priced", false)).await;
        assert!(status == 502 && body.contains("upstream_unavailable"), "{body}");
        assert!(close(limits::reserved_usd(), 0.0));
    }
    // and so does a request for a model that does not exist
    let (status, _, _) = send(addr, chat("res-dead", "nope", false)).await;
    assert_eq!(status, 404);
    assert!(close(limits::reserved_usd(), 0.0));
}

// A model without a price costs nothing: its requests hold nothing, and are
// not held up by the ones that do.
#[tokio::test]
async fn an_unpriced_model_reserves_nothing() {
    let _serial = SERIAL.lock().await;
    let (up, hits) = upstream(Duration::from_millis(300)).await;
    let addr = gateway("res-free", &up, json!({"budget_usd": 0.4})).await;
    let free = tokio::spawn(futures(8, move || send(addr, chat("res-free", "free", false))));
    until("all are with the upstream", || hits.load(Ordering::Relaxed) == 8).await;
    assert!(close(limits::reserved_usd(), 0.0), "{}", limits::reserved_usd());

    // one priced request holds the whole budget; the free model still serves
    let priced = tokio::spawn(send(addr, chat("res-free", "priced", false)));
    until("the priced one is with the upstream", || hits.load(Ordering::Relaxed) == 9).await;
    assert!(close(limits::reserved_usd(), 0.4));
    let (status, _, body) = send(addr, chat("res-free", "priced", false)).await;
    assert!(status == 429 && body.contains("\"code\":\"budget_reserved\""), "{body}");
    assert_eq!(send(addr, chat("res-free", "free", true)).await.0, 200);
    assert!(free.await.unwrap().iter().all(|(status, _, _)| *status == 200));
    assert_eq!(priced.await.unwrap().0, 200);
    assert!(close(limits::reserved_usd(), 0.0));
}
