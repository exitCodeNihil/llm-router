//! The control-plane side of the gateway (internal/gwsync): join with the
//! group's token, keep the session fresh, long-poll the group's config, and
//! sync usage and limit counters every couple of seconds.

use std::io::Read;
use std::sync::Arc;
use std::sync::atomic::{AtomicI64, AtomicU64, Ordering};
use std::time::{Duration, Instant};

use arc_swap::ArcSwap;
use bytes::Bytes;
use http::{HeaderValue, Method, Request, StatusCode, header};
use http_body_util::{BodyExt, Full, Limited};
use serde::{Deserialize, Serialize};

use crate::config::{Config, SCHEMA, Wire};
use crate::limits::{self, Counters, RateDelta};
use crate::server::{HttpsClient, State, https_client};
use crate::usage::{self, Event};
use crate::{health, metrics, now_ms};

pub const VERSION: &str = match option_env!("LLMR_VERSION") {
    Some(v) => v,
    None => "dev",
};

/// The most events one sync uploads; a backlog goes out in back-to-back syncs.
const MAX_BATCH: usize = 50_000;

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
        let mut syncer = Syncer::default();
        loop {
            let synced = self.sync_once(&mut syncer).await;
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
    pub async fn flush(&self, deadline: Instant) {
        let mut syncer = Syncer::default();
        while Instant::now() < deadline {
            if self.sync_once(&mut syncer).await && usage::queued() == 0 && syncer.pending.is_none() {
                return;
            }
        }
    }

    /// One sync: freeze a batch unless the last one is unacknowledged, send it,
    /// and settle it on success.
    async fn sync_once(&self, s: &mut Syncer) -> bool {
        if s.pending.is_none() {
            let events = usage::take(MAX_BATCH);
            let (rates, scopes) = limits::freeze();
            s.batch += 1;
            let req = SyncRequest {
                batch: s.batch,
                cursor: &s.cursor,
                events: &events,
                rates: &rates,
                stats: Stats {
                    version: VERSION,
                    uptime_s: self.started.elapsed().as_secs(),
                    in_flight: metrics::IN_FLIGHT.load(Ordering::Relaxed),
                    workers: metrics::WORKERS.load(Ordering::Relaxed),
                },
            };
            let mut gz = flate2::write::GzEncoder::new(Vec::with_capacity(4096), flate2::Compression::fast());
            if serde_json::to_writer(&mut gz, &req).is_err() {
                return false;
            }
            let body = gz.finish().unwrap_or_default();
            s.pending = Some(Batch { body: body.into(), scopes });
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
                eprintln!("sync: {status}: {}", String::from_utf8_lossy(&bytes[..bytes.len().min(200)]).trim());
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

#[derive(Default)]
struct Syncer {
    batch: i64,
    cursor: String,
    pending: Option<Batch>,
}

/// A frozen upload, resent as is until acknowledged.
struct Batch {
    body: Bytes,
    scopes: Vec<Arc<limits::Scope>>,
}

#[derive(Serialize)]
struct SyncRequest<'a> {
    batch: i64,
    cursor: &'a str,
    events: &'a [Event],
    rates: &'a [RateDelta],
    stats: Stats,
}

#[derive(Serialize)]
struct Stats {
    version: &'static str,
    uptime_s: u64,
    in_flight: i64,
    workers: i64,
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
