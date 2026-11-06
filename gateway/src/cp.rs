//! The control-plane side of the gateway (internal/gwsync): join with the
//! group's token, keep the session fresh, long-poll the group's config, and
//! sync usage and limit counters every couple of seconds.

use std::io::{Read, Write};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicI64, AtomicU64, Ordering};
use std::time::{Duration, Instant};

use arc_swap::ArcSwap;
use bytes::Bytes;
use http::{HeaderValue, Method, Request, StatusCode, header};
use http_body_util::{BodyExt, Full, Limited};
use serde::{Deserialize, Serialize};

use crate::cloud::jwt::{BoxFuture, ProvisionError, Provisioned, Provisioner};
use crate::config::{Config, SCHEMA, Wire};
use crate::limits::{self, Counters, RateDelta};
use crate::server::{HttpsClient, State, https_client};
use crate::usage::{self, Event};
use crate::{health, metrics, now_ms};

pub const VERSION: &str = match option_env!("LLMR_VERSION") {
    Some(v) => v,
    None => "dev",
};

/// The most events one sync uploads, and about the most bytes before gzip; a
/// backlog goes out in back-to-back syncs. The control plane reads at most
/// 32 MiB of a sync, so the byte cap keeps a batch at half that. Event sizes
/// count captured content, escapes included, and one event holds under 10 MiB
/// of it (three parts of at most 1 MiB, the output up to 6x once escaped), so
/// even a batch of one fits. A proxy that refuses a batch as too large (413) halves the cap, down
/// to MIN_BATCH_BYTES; refused there, the batch goes without its content, then halved, and an
/// event refused even alone, without content, is dropped.
const MAX_BATCH: usize = 50_000;
const MAX_BATCH_BYTES: usize = 16 << 20;
const MIN_BATCH_BYTES: usize = 64 << 10;

pub struct ControlPlane {
    base: String,
    join_token: String,
    name: String,
    client: HttpsClient,
    session: ArcSwap<String>,
    generation: AtomicU64,
    expires_at_ms: AtomicI64,
    sync_interval_ms: AtomicU64,
    join_lock: tokio::sync::Mutex<()>,
    started: Instant,
    leaving: AtomicBool,
    /// The sync loop's and the shutdown flush's one batch sequence: the
    /// control plane skips a batch number it has seen, so a flush counting
    /// from 1 again had its events acknowledged and dropped.
    syncer: tokio::sync::Mutex<Syncer>,
}

#[derive(Deserialize)]
struct Joined {
    #[serde(default)]
    instance_id: String,
    session: String,
    expires_at_ms: i64,
    #[serde(default)]
    sync_interval_ms: u64,
    group: Option<crate::config::WireGroup>,
}

enum Call {
    Ok(StatusCode, http::HeaderMap, Bytes),
    Unauthorized,
}

impl ControlPlane {
    pub fn new(base: &str, join_token: String, name: String) -> ControlPlane {
        if !base.starts_with("https://") && !is_local(base) {
            eprintln!(
                "warning: LLMR_CONTROL_PLANE_URL is not https: provider credentials would cross the network in the clear"
            );
        }
        ControlPlane {
            base: base.trim_end_matches('/').to_string(),
            join_token,
            name,
            client: https_client(8),
            session: ArcSwap::from_pointee(String::new()),
            generation: AtomicU64::new(0),
            expires_at_ms: AtomicI64::new(0),
            sync_interval_ms: AtomicU64::new(2000),
            join_lock: tokio::sync::Mutex::new(()),
            started: Instant::now(),
            leaving: AtomicBool::new(false),
            syncer: tokio::sync::Mutex::new(Syncer::default()),
        }
    }

    async fn call(
        &self,
        method: Method,
        path: &str,
        bearer: &str,
        body: Option<(Bytes, bool)>,
        extra: &[(&str, String)],
        timeout: Duration,
    ) -> Result<Call, String> {
        let mut req = Request::builder().method(method).uri(format!("{}{path}", self.base));
        req = req
            .header(header::AUTHORIZATION, format!("Bearer {bearer}"))
            .header(header::USER_AGENT, format!("llmr-gateway/{VERSION}"));
        for (k, v) in extra {
            req = req.header(*k, v);
        }
        let body = match body {
            Some((b, gz)) => {
                req = req.header(header::CONTENT_TYPE, "application/json");
                if gz {
                    req = req.header(header::CONTENT_ENCODING, "gzip");
                }
                b
            }
            None => Bytes::new(),
        };
        let req = req.body(Full::new(body)).map_err(|e| e.to_string())?;
        let resp = tokio::time::timeout(timeout, self.client.request(req))
            .await
            .map_err(|_| "timed out".to_string())?
            .map_err(|e| e.to_string())?;
        if resp.status() == StatusCode::UNAUTHORIZED {
            return Ok(Call::Unauthorized);
        }
        let (parts, body) = resp.into_parts();
        let bytes = tokio::time::timeout(timeout, Limited::new(body, 256 << 20).collect())
            .await
            .map_err(|_| "timed out".to_string())?
            .map_err(|e| e.to_string())?
            .to_bytes();
        Ok(Call::Ok(parts.status, parts.headers, bytes))
    }

    /// Join (again) unless another task already did since `seen` failed.
    async fn rejoin(&self, seen: u64) {
        let _guard = self.join_lock.lock().await;
        if self.generation.load(Ordering::Relaxed) != seen {
            return;
        }
        let mut wait = Duration::from_secs(1);
        loop {
            match self.join().await {
                Ok(()) => return,
                Err(e) => {
                    eprintln!("join failed: {e}; retrying in {}s", wait.as_secs());
                    tokio::time::sleep(wait).await;
                    wait = (wait * 2).min(Duration::from_secs(30));
                }
            }
        }
    }

    async fn join(&self) -> Result<(), String> {
        let body = serde_json::to_vec(&serde_json::json!({"name": self.name, "version": VERSION, "schema": SCHEMA}))
            .unwrap_or_default();
        let call = self
            .call(
                Method::POST,
                "/gw/v1/join",
                &self.join_token,
                Some((body.into(), false)),
                &[],
                Duration::from_secs(15),
            )
            .await?;
        let Call::Ok(status, _, bytes) = call else { return Err("the control plane refused the join token".into()) };
        if status != StatusCode::OK {
            return Err(format!("join: {status}: {}", String::from_utf8_lossy(&bytes)));
        }
        let j: Joined = serde_json::from_slice(&bytes).map_err(|e| format!("join response: {e}"))?;
        self.adopt(&j);
        if let Some(g) = &j.group {
            eprintln!("joined gateway group {} as instance {}", g.name, j.instance_id);
        }
        Ok(())
    }

    fn adopt(&self, j: &Joined) {
        self.session.store(Arc::new(j.session.clone()));
        self.expires_at_ms.store(j.expires_at_ms, Ordering::Relaxed);
        if j.sync_interval_ms > 0 {
            self.sync_interval_ms.store(j.sync_interval_ms, Ordering::Relaxed);
        }
        self.generation.fetch_add(1, Ordering::Relaxed);
    }

    fn session(&self) -> (Arc<String>, u64) {
        (self.session.load_full(), self.generation.load(Ordering::Relaxed))
    }

    /// Join, then run the control-plane loops for as long as the process lives.
    pub async fn run(self: Arc<Self>, st: &'static State) {
        crate::cloud::jwt::identities().set_provisioner(self.clone());
        self.rejoin(0).await;
        tokio::spawn(self.clone().renew_loop());
        tokio::spawn(self.clone().config_loop(st));
        self.sync_loop(st).await;
    }

    /// Renew the session five minutes before it expires.
    async fn renew_loop(self: Arc<Self>) {
        loop {
            let left = self.expires_at_ms.load(Ordering::Relaxed) - now_ms() - 5 * 60_000;
            tokio::time::sleep(Duration::from_millis(left.max(30_000) as u64)).await;
            let (s, generation) = self.session();
            match self.call(Method::POST, "/gw/v1/renew", &s, None, &[], Duration::from_secs(15)).await {
                Ok(Call::Ok(StatusCode::OK, _, bytes)) => {
                    if let Ok(j) = serde_json::from_slice::<Joined>(&bytes) {
                        self.adopt(&j);
                    }
                }
                Ok(Call::Unauthorized) => self.rejoin(generation).await,
                Ok(Call::Ok(status, _, _)) => eprintln!("session renew: {status}"),
                Err(e) => eprintln!("session renew: {e}"),
            }
        }
    }

    async fn config_loop(self: Arc<Self>, st: &'static State) {
        let mut version: i64 = 0;
        loop {
            let (s, generation) = self.session();
            let etag = [("if-none-match", format!("\"v{version}\""))];
            match self.call(Method::GET, "/gw/v1/config?wait=25", &s, None, &etag, Duration::from_secs(40)).await {
                Ok(Call::Ok(StatusCode::OK, headers, bytes)) => match load(&headers, &bytes) {
                    Ok(cfg) => {
                        version = cfg.version;
                        let ids: std::collections::HashSet<&str> = cfg
                            .models
                            .values()
                            .chain(cfg.patterns.iter().map(|(_, d)| d))
                            .flat_map(|ds| ds.iter().map(|d| d.id.as_str()))
                            .collect();
                        health::retain(&ids);
                        metrics::CONFIG_VERSION.store(version, Ordering::Relaxed);
                        eprintln!(
                            "config version {version}: {} keys, {} models",
                            cfg.keys.len(),
                            cfg.names.len() + cfg.patterns.len()
                        );
                        st.config.store(Some(Arc::new(cfg)));
                    }
                    Err(e) => {
                        eprintln!("config rejected: {e}");
                        tokio::time::sleep(Duration::from_secs(5)).await;
                    }
                },
                Ok(Call::Ok(StatusCode::NOT_MODIFIED, _, _)) => {}
                Ok(Call::Unauthorized) => self.rejoin(generation).await,
                Ok(Call::Ok(status, _, _)) => {
                    eprintln!("config: {status}");
                    tokio::time::sleep(Duration::from_secs(2)).await;
                }
                Err(e) => {
                    eprintln!("config: {e}");
                    tokio::time::sleep(Duration::from_secs(2)).await;
                }
            }
        }
    }

    async fn sync_loop(self: Arc<Self>, st: &'static State) {
        loop {
            let synced = self.sync_once(&mut *self.syncer.lock().await).await;
            if synced && st.config.load().is_some() {
                st.ready.store(true, std::sync::atomic::Ordering::Relaxed);
            }
            // a backlog goes out back to back
            if synced && usage::queued() > 0 {
                continue;
            }
            tokio::time::sleep(Duration::from_millis(self.sync_interval_ms.load(Ordering::Relaxed))).await;
        }
    }

    /// Upload until the queue is empty or `deadline` passes, for shutdown.
    /// It continues the sync loop's batch sequence, including a batch that
    /// is still unacknowledged.
    pub async fn flush(&self, deadline: Instant) {
        self.leaving.store(true, Ordering::Relaxed);
        let deadline = tokio::time::Instant::from_std(deadline);
        // Waits out a sync in progress, then keeps the lock so the loop can't
        // interleave.
        let Ok(mut syncer) = tokio::time::timeout_at(deadline, self.syncer.lock()).await else { return };
        while tokio::time::Instant::now() < deadline {
            let done = tokio::time::timeout_at(deadline, self.sync_once(&mut syncer)).await;
            match done {
                Ok(true) if usage::queued() == 0 && syncer.pending.is_none() => return,
                Ok(true) => {}
                Ok(false) => tokio::time::sleep(Duration::from_millis(200)).await,
                Err(_) => return,
            }
        }
    }

    /// One sync: freeze a batch unless the last one is unacknowledged, send it,
    /// and settle it on success.
    async fn sync_once(&self, s: &mut Syncer) -> bool {
        if s.pending.is_none() {
            let events = usage::take(MAX_BATCH, s.max_bytes());
            let (rates, scopes) = limits::freeze();
            s.batch += 1;
            let mem = crate::resources::memory();
            let stats = Stats {
                version: VERSION,
                uptime_s: self.started.elapsed().as_secs(),
                in_flight: metrics::IN_FLIGHT.load(Ordering::Relaxed),
                workers: metrics::WORKERS.load(Ordering::Relaxed),
                open_connections: metrics::OPEN_CONNECTIONS.load(Ordering::Relaxed),
                mem_bytes: mem.used,
                mem_limit_bytes: mem.limit,
                cpu_pct: crate::resources::cpu_pct(),
                leaving: self.leaving.load(Ordering::Relaxed),
            };
            let frozen = Frozen { batch: s.batch, cursor: s.cursor.clone(), events, rates, stats };
            s.pending = Batch::build(frozen, scopes).await;
        }
        let Some(batch) = &s.pending else { return false };
        let (session, generation) = self.session();
        match self
            .call(Method::POST, "/gw/v1/sync", &session, Some((batch.body.clone(), true)), &[], Duration::from_secs(30))
            .await
        {
            Ok(Call::Ok(StatusCode::OK, _, bytes)) => {
                let counters: Counters = serde_json::from_slice(&bytes).unwrap_or_default();
                limits::apply(&batch.scopes, &counters);
                s.cursor = counters.cursor;
                s.pending = None;
                true
            }
            Ok(Call::Unauthorized) => {
                self.rejoin(generation).await;
                false
            }
            Ok(Call::Ok(status, _, bytes)) => {
                metrics::SYNC_FAILURES.fetch_add(1, Ordering::Relaxed);
                let why = String::from_utf8_lossy(&bytes[..bytes.len().min(200)]).trim().to_string();
                if status == StatusCode::PAYLOAD_TOO_LARGE && s.max_bytes() > MIN_BATCH_BYTES {
                    // Something in between limits request size: put the events
                    // back and send smaller batches from now on.
                    let Some(batch) = s.pending.take() else { return false };
                    s.limit_bytes = Some(s.max_bytes() / 2);
                    eprintln!("sync: {status}; sending batches of at most {} bytes", s.max_bytes());
                    limits::discard(&batch.scopes);
                    usage::requeue(batch.frozen.events);
                } else if status == StatusCode::PAYLOAD_TOO_LARGE {
                    // Refused at the smallest batch: resending it as is would
                    // hold back every later event, and the limit counters.
                    let Some(batch) = s.pending.take() else { return false };
                    s.pending = batch.smaller().await;
                } else if refused(status) {
                    // Resending the same bytes would be refused again and hold
                    // back every later event: drop this batch and move on.
                    let Some(batch) = s.pending.take() else { return false };
                    let n = batch.frozen.events.len();
                    metrics::SYNC_REFUSED_EVENTS.fetch_add(n as u64, Ordering::Relaxed);
                    eprintln!("sync: {status}: {why}; dropping a batch of {n} events");
                    limits::discard(&batch.scopes);
                } else {
                    eprintln!("sync: {status}: {why}");
                }
                false
            }
            Err(e) => {
                metrics::SYNC_FAILURES.fetch_add(1, Ordering::Relaxed);
                eprintln!("sync: {e}");
                false
            }
        }
    }
}

impl Provisioner for ControlPlane {
    /// Ask the control plane to create the user behind an identity token
    /// (POST /gw/v1/provision). It verifies the token itself.
    fn provision<'a>(&'a self, token: &'a str) -> BoxFuture<'a, Result<Provisioned, ProvisionError>> {
        Box::pin(async move {
            let body: Bytes = serde_json::to_vec(&serde_json::json!({"token": token})).unwrap_or_default().into();
            for _ in 0..2 {
                let (session, generation) = self.session();
                if session.is_empty() {
                    self.rejoin(generation).await;
                    continue;
                }
                let call = self
                    .call(
                        Method::POST,
                        "/gw/v1/provision",
                        &session,
                        Some((body.clone(), false)),
                        &[],
                        Duration::from_secs(15),
                    )
                    .await;
                return match call {
                    Ok(Call::Ok(StatusCode::OK, _, bytes)) => serde_json::from_slice(&bytes)
                        .map_err(|e| ProvisionError::Unavailable(format!("provision response: {e}"))),
                    Ok(Call::Unauthorized) => {
                        self.rejoin(generation).await;
                        continue;
                    }
                    Ok(Call::Ok(status, _, bytes)) => {
                        let why = String::from_utf8_lossy(&bytes[..bytes.len().min(300)]).trim().to_string();
                        if status == StatusCode::FORBIDDEN || status == StatusCode::BAD_REQUEST {
                            Err(ProvisionError::Refused(why))
                        } else {
                            Err(ProvisionError::Unavailable(format!("{status}: {why}")))
                        }
                    }
                    Err(e) => Err(ProvisionError::Unavailable(e)),
                };
            }
            Err(ProvisionError::Unavailable("no control-plane session".into()))
        })
    }
}

#[derive(Default)]
struct Syncer {
    batch: i64,
    cursor: String,
    pending: Option<Batch>,
    /// A smaller byte cap than MAX_BATCH_BYTES, after a 413.
    limit_bytes: Option<usize>,
}

impl Syncer {
    fn max_bytes(&self) -> usize {
        self.limit_bytes.unwrap_or(MAX_BATCH_BYTES)
    }
}

/// A frozen upload, resent as is until acknowledged. What it was built from is
/// kept, to queue its events again or build it smaller if it turns out to be
/// too large to send.
struct Batch {
    body: Bytes,
    scopes: Vec<Arc<limits::Scope>>,
    frozen: Frozen,
}

/// A batch's contents: the same number, cursor and rates however it is built.
struct Frozen {
    batch: i64,
    cursor: String,
    events: Vec<Event>,
    rates: Vec<RateDelta>,
    stats: Stats,
}

impl Batch {
    /// Encode a batch. Encoding a large one takes milliseconds: keep it off
    /// the request threads. JSON first, then one gzip pass; serde's many small
    /// writes straight into the encoder cost ~50x more.
    async fn build(frozen: Frozen, scopes: Vec<Arc<limits::Scope>>) -> Option<Batch> {
        let body = tokio::task::spawn_blocking(move || {
            let f = &frozen;
            let req =
                SyncRequest { batch: f.batch, cursor: &f.cursor, events: &f.events, rates: &f.rates, stats: &f.stats };
            let gz = (|| {
                let json = serde_json::to_vec(&req).map_err(std::io::Error::other)?;
                let mut gz =
                    flate2::write::GzEncoder::new(Vec::with_capacity(json.len() / 8), flate2::Compression::fast());
                gz.write_all(&json)?;
                gz.finish()
            })();
            (gz, frozen)
        })
        .await;
        match body {
            Ok((Ok(body), frozen)) => Some(Batch { body: body.into(), scopes, frozen }),
            _ => {
                eprintln!("sync: could not encode a batch");
                limits::discard(&scopes);
                None
            }
        }
    }

    /// The batch smaller, after a 413 at the smallest batch size: without its
    /// events' content; already without, its first half, the rest put back
    /// in the queue; down to one event, without that event, which is dropped
    /// and counted. The batch number, cursor and rates stay.
    async fn smaller(mut self) -> Option<Batch> {
        let f = &mut self.frozen;
        let stripped = usage::strip_content(&mut f.events);
        if stripped > 0 {
            eprintln!("sync: 413 at the smallest batch size; sending {stripped} events without their content");
        } else if f.events.len() > 1 {
            eprintln!(
                "sync: 413 at the smallest batch size; sending {} of its {} events",
                f.events.len() / 2,
                f.events.len()
            );
            usage::requeue(f.events.split_off(f.events.len() / 2));
        } else {
            metrics::SYNC_REFUSED_EVENTS.fetch_add(f.events.len() as u64, Ordering::Relaxed);
            eprintln!("sync: 413 for a single event without content; dropping it");
            f.events.clear();
        }
        Batch::build(self.frozen, self.scopes).await
    }
}

/// Whether the control plane refused a batch for good: it answers 400 when it
/// cannot read one, and a sync it cannot read now it never will. Anything
/// else, including a 4xx from a proxy or load balancer in between (a 404 or
/// 503 during a rollout, say), is resent. 401 is handled before this.
fn refused(status: StatusCode) -> bool {
    status == StatusCode::BAD_REQUEST || status == StatusCode::UNPROCESSABLE_ENTITY
}

#[derive(Serialize)]
struct SyncRequest<'a> {
    batch: i64,
    cursor: &'a str,
    events: &'a [Event],
    rates: &'a [RateDelta],
    stats: &'a Stats,
}

#[derive(Serialize)]
struct Stats {
    version: &'static str,
    uptime_s: u64,
    in_flight: i64,
    workers: i64,
    open_connections: i64,
    mem_bytes: u64,
    #[serde(skip_serializing_if = "Option::is_none")]
    mem_limit_bytes: Option<u64>,
    cpu_pct: f64,
    /// Shutting down: these are its last syncs, so the control plane can
    /// forget it at once instead of reporting it as missing.
    leaving: bool,
}

fn load(headers: &http::HeaderMap, bytes: &[u8]) -> Result<Config, String> {
    let gz = headers.get(header::CONTENT_ENCODING) == Some(&HeaderValue::from_static("gzip"));
    let raw = if gz {
        let mut out = Vec::with_capacity(bytes.len() * 8);
        flate2::read::GzDecoder::new(bytes).read_to_end(&mut out).map_err(|e| format!("gunzip: {e}"))?;
        out
    } else {
        bytes.to_vec()
    };
    let wire: Wire = serde_json::from_slice(&raw).map_err(|e| e.to_string())?;
    let generation = limits::begin_config();
    let cfg = Config::build(wire)?;
    limits::end_config(generation);
    Ok(cfg)
}

fn is_local(url: &str) -> bool {
    let rest = url.trim_start_matches("http://");
    let host = rest.split(['/', ':']).next().unwrap_or("");
    host == "localhost" || host == "127.0.0.1" || host == "[::1]" || !host.contains('.')
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::usage::tests::emit_capturing;
    use std::sync::Mutex;

    /// Hold the process-wide usage queue and limit counters: a sync takes
    /// from both, and other tests use them.
    fn hold_globals() -> [std::sync::MutexGuard<'static, ()>; 2] {
        let q = crate::usage::tests::hold_queue();
        [q, crate::limits::tests::SERIAL.lock().unwrap_or_else(|e| e.into_inner())]
    }

    /// What a mock control plane saw of a sync: its batch number, its JSON,
    /// and the status it answered.
    type Seen = Arc<Mutex<Vec<(i64, serde_json::Value, u16)>>>;

    /// A control plane answering each sync with `status(n, json size)`, n
    /// counting syncs from 1.
    async fn mock_cp(status: impl Fn(usize, usize) -> u16 + Send + Sync + 'static) -> (String, Seen) {
        use hyper::service::service_fn;
        use hyper_util::rt::{TokioExecutor, TokioIo};
        use std::io::Read;
        let _ = rustls::crypto::ring::default_provider().install_default();
        let seen: Seen = Default::default();
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let url = format!("http://{}", listener.local_addr().unwrap());
        let (s, status) = (seen.clone(), Arc::new(status));
        tokio::spawn(async move {
            while let Ok((conn, _)) = listener.accept().await {
                let (s, status) = (s.clone(), status.clone());
                tokio::spawn(async move {
                    let svc = service_fn(move |req: Request<hyper::body::Incoming>| {
                        let (s, status) = (s.clone(), status.clone());
                        async move {
                            let gz = req.into_body().collect().await.unwrap().to_bytes();
                            let mut json = Vec::new();
                            flate2::read::GzDecoder::new(&gz[..]).read_to_end(&mut json).unwrap();
                            let v: serde_json::Value = serde_json::from_slice(&json).unwrap();
                            let code = {
                                let mut seen = s.lock().unwrap();
                                let code = status(seen.len() + 1, json.len());
                                seen.push((v["batch"].as_i64().unwrap(), v, code));
                                code
                            };
                            Ok::<_, std::convert::Infallible>(
                                http::Response::builder().status(code).body(Full::new(Bytes::from("{}"))).unwrap(),
                            )
                        }
                    });
                    let _ = hyper_util::server::conn::auto::Builder::new(TokioExecutor::new())
                        .serve_connection(TokioIo::new(conn), svc)
                        .await;
                });
            }
        });
        (url, seen)
    }

    // The control plane skips a batch number it has already stored and still
    // answers 200: a shutdown flush that counted from 1 again lost its events.
    // The real sync loop runs, then the flush takes over from it.
    #[tokio::test]
    #[allow(clippy::await_holding_lock)] // one test at a time: see hold_globals
    async fn the_shutdown_flush_continues_the_batch_sequence() {
        let _globals = hold_globals();
        let (url, seen) = mock_cp(|n, _| if n == 2 { 503 } else { 200 }).await;
        let cp = Arc::new(ControlPlane::new(&url, "llmrg_test".into(), "test".into()));
        cp.sync_interval_ms.store(50, Ordering::Relaxed);
        let st: &'static State = Box::leak(Box::new(State {
            config: Default::default(),
            ready: Default::default(),
            draining: Default::default(),
            max_in_flight: 1,
        }));
        let sync_loop = tokio::spawn(cp.clone().sync_loop(st));
        // until the loop has synced, failed the second, and is waiting to resend it
        while seen.lock().unwrap().len() < 2 {
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
        cp.flush(Instant::now() + Duration::from_secs(5)).await;
        sync_loop.abort();
        let seen: Vec<i64> = seen.lock().unwrap().iter().map(|(b, _, _)| *b).collect();
        assert_eq!(&seen[..3], &[1, 2, 2], "the unacknowledged batch is resent as is: {seen:?}");
        assert!(seen.windows(2).all(|w| w[0] <= w[1]), "never counts from 1 again: {seen:?}");
    }

    /// A proxy that refuses batches over a size even at the smallest batch
    /// size: the batch goes without its content, then halved, and an event
    /// too large alone is dropped, so the rest, and the limit counters, flow.
    #[tokio::test]
    #[allow(clippy::await_holding_lock)] // one test at a time: see hold_globals
    async fn a_413_at_the_smallest_batch_never_blocks_the_sync() {
        let _globals = hold_globals();
        let (url, seen) = mock_cp(|_, size| if size > 3000 { 413 } else { 200 }).await;
        drop(usage::take(usize::MAX, usize::MAX));
        let send = |model: &str, cap: usize, output: &str| {
            let mut em = emit_capturing(model, cap);
            em.capture_request(br#"{"messages":[{"role":"user","content":"q"}]}"#, false);
            let json = serde_json::json!({"choices": [{"message": {"content": output}}]}).to_string();
            em.set_output(crate::translate::oai_json_text(json.as_bytes(), cap));
            em.send(200, "", Default::default(), 0);
        };
        send("small-1", 1 << 20, "answer");
        send("big", 1 << 20, &"x".repeat(10_000));
        send(&"too-large-".repeat(500), 0, "");
        send("small-2", 1 << 20, "answer");
        let (stripped, refused) =
            (usage::CONTENT_STRIPPED.load(Ordering::Relaxed), metrics::SYNC_REFUSED_EVENTS.load(Ordering::Relaxed));

        let cp = ControlPlane::new(&url, "llmrg_test".into(), "test".into());
        let mut s = cp.syncer.lock().await;
        s.limit_bytes = Some(MIN_BATCH_BYTES);
        for _ in 0..20 {
            if cp.sync_once(&mut s).await && usage::queued() == 0 {
                break;
            }
        }
        assert!(s.pending.is_none() && usage::queued() == 0, "{:?}", seen.lock().unwrap());
        let seen = seen.lock().unwrap();
        let accepted: Vec<_> = seen.iter().filter(|(_, _, code)| *code == 200).collect();
        assert!(accepted.windows(2).all(|w| w[0].0 < w[1].0), "each batch once, in order");
        let events: Vec<&serde_json::Value> =
            accepted.iter().flat_map(|(_, v, _)| v["events"].as_array().unwrap()).collect();
        let models: Vec<&str> = events.iter().map(|e| e["model_name"].as_str().unwrap()).collect();
        assert_eq!(models, ["small-1", "big", "small-2"], "the one too large alone is dropped");
        for e in events {
            assert!(e["content_truncated"] == true && e.get("input").is_none() && e.get("output").is_none(), "{e}");
        }
        assert!(seen.iter().any(|(_, v, code)| *code == 413 && v["events"].as_array().unwrap().len() == 1));
        // other tests count too
        assert!(usage::CONTENT_STRIPPED.load(Ordering::Relaxed) - stripped >= 3);
        assert_eq!(metrics::SYNC_REFUSED_EVENTS.load(Ordering::Relaxed) - refused, 1);
    }

    #[test]
    fn only_lasting_refusals_drop_a_batch() {
        for (status, drop) in [
            (StatusCode::BAD_REQUEST, true),
            (StatusCode::UNPROCESSABLE_ENTITY, true),
            (StatusCode::FORBIDDEN, false),
            (StatusCode::NOT_FOUND, false),
            (StatusCode::PAYLOAD_TOO_LARGE, false),
            (StatusCode::REQUEST_TIMEOUT, false),
            (StatusCode::TOO_MANY_REQUESTS, false),
            (StatusCode::INTERNAL_SERVER_ERROR, false),
            (StatusCode::SERVICE_UNAVAILABLE, false),
        ] {
            assert_eq!(refused(status), drop, "{status}");
        }
    }
}
