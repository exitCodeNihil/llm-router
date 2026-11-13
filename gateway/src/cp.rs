//! The control-plane side of the gateway (internal/gwsync): join with the
//! group's token, keep the session fresh, long-poll the group's config, and
//! sync usage and limit counters every couple of seconds.

use std::io::{Read, Write};
use std::sync::atomic::{AtomicBool, AtomicI64, AtomicU64, Ordering};
use std::sync::{Arc, Mutex, OnceLock};
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
use crate::state::{SAVE_INTERVAL, Store};
use crate::usage::{self, Event};
use crate::{health, log_debug, log_error, log_info, log_warn, metrics, now_ms};

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

/// The control plane marks a 401 that is its own verdict (no such group, join
/// token or instance) with this header; a 401 without it may come from a proxy
/// in between.
const REFUSED_HEADER: &str = "x-llmr-refused";
/// How long the control plane must have been refusing the join before the
/// gateway stops serving the config it holds.
const REFUSE_AFTER: Duration = Duration::from_secs(15 * 60);
/// The oldest confirmation a config from the state directory is served on.
const MAX_SAVED_AGE: Duration = Duration::from_secs(24 * 3600);
/// How far ahead of this clock a saved confirmation may be: past that the
/// clock, or the file, is wrong.
const MAX_CLOCK_SKEW_MS: i64 = 5 * 60_000;

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
    /// The version to poll the config with: 0 asks for it whatever the
    /// control plane has.
    config_version: AtomicI64,
    /// The control plane has refused the join since the last one that
    /// succeeded: when that was noticed in this process, and for how long it
    /// had been going on by then, as the state directory had it.
    refused_since: Mutex<Option<(Instant, Duration)>>,
    refuse_after: Duration,
    st: OnceLock<&'static State>,
    state: Option<Arc<Store>>,
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
    /// A 401, and whether the control plane itself said so (REFUSED_HEADER).
    Unauthorized(bool),
}

impl ControlPlane {
    pub fn new(base: &str, join_token: String, name: String) -> ControlPlane {
        if !base.starts_with("https://") && !is_local(base) {
            log_warn!("LLMR_CONTROL_PLANE_URL is not https: provider credentials would cross the network in the clear");
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
            config_version: AtomicI64::new(0),
            refused_since: Mutex::new(None),
            refuse_after: REFUSE_AFTER,
            st: OnceLock::new(),
            state: None,
            syncer: tokio::sync::Mutex::new(Syncer::default()),
        }
    }

    /// Keep the config and spend in `state` across restarts.
    pub fn with_state(mut self, state: Option<Arc<Store>>) -> ControlPlane {
        self.state = state;
        self
    }

    #[cfg(test)]
    fn refusing_after(mut self, d: Duration) -> ControlPlane {
        self.refuse_after = d;
        self
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
            return Ok(Call::Unauthorized(resp.headers().contains_key(REFUSED_HEADER)));
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
                    log_warn!("join failed: {e}; retrying in {}s", wait.as_secs());
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
        let (status, bytes) = match call {
            Call::Ok(status, _, bytes) => (status, bytes),
            Call::Unauthorized(refused) => {
                if refused {
                    self.refused();
                }
                return Err("the control plane refused the join token".into());
            }
        };
        if status != StatusCode::OK {
            return Err(format!("join: {status}: {}", String::from_utf8_lossy(&bytes)));
        }
        let j: Joined = serde_json::from_slice(&bytes).map_err(|e| format!("join response: {e}"))?;
        self.adopt(&j);
        if self.refused_since.lock().unwrap().take().is_some()
            && let Some(state) = self.state.clone()
        {
            tokio::task::spawn_blocking(move || state.clear_refused());
        }
        if let Some(st) = self.st.get() {
            st.ready.refused.store(false, Ordering::Relaxed);
        }
        if let Some(g) = &j.group {
            log_info!("joined gateway group {} as instance {}", g.name, j.instance_id);
        }
        Ok(())
    }

    /// The control plane answered the join with its own 401: the group is
    /// deleted or the join token replaced. Refused for `refuse_after` with no
    /// join succeeding in between, the gateway stops serving and deletes its
    /// saved config; it keeps trying to join, and a join that succeeds brings
    /// the config back. When it began is kept in the state directory, so a
    /// restart neither serves the saved config nor starts the count again.
    fn refused(&self) {
        let (since, before) = {
            let mut refused = self.refused_since.lock().unwrap();
            if refused.is_none()
                && let Some(state) = self.state.clone()
            {
                let at = now_ms();
                tokio::task::spawn_blocking(move || state.put_refused(at));
            }
            *refused.get_or_insert((Instant::now(), Duration::ZERO))
        };
        let (Some(st), elapsed) = (self.st.get(), since.elapsed() + before) else { return };
        if elapsed >= self.refuse_after {
            self.stop_serving(st, elapsed);
        }
    }

    fn stop_serving(&self, st: &State, refused_for: Duration) {
        if st.ready.refused.swap(true, Ordering::Relaxed) {
            return;
        }
        log_error!(
            "the control plane has refused this gateway's join token for {}s: its group was deleted or its token replaced; no longer serving",
            refused_for.as_secs()
        );
        st.ready.set(false);
        st.config.store(None);
        self.config_version.store(0, Ordering::Relaxed);
        metrics::CONFIG_VERSION.store(0, Ordering::Relaxed);
        if let Some(state) = self.state.clone() {
            tokio::task::spawn_blocking(move || state.forget());
        }
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
    /// A config in the state directory is served first, without waiting for
    /// the control plane.
    pub async fn run(self: Arc<Self>, st: &'static State) {
        crate::cloud::jwt::identities().set_provisioner(self.clone());
        self.start(st).await;
    }

    async fn start(self: Arc<Self>, st: &'static State) {
        let _ = self.st.set(st);
        self.restore(st).await;
        // The loops end with this task.
        let mut loops = tokio::task::JoinSet::new();
        loops.spawn(self.clone().save_loop());
        self.rejoin(0).await;
        loops.spawn(self.clone().renew_loop());
        loops.spawn(self.clone().config_loop(st));
        self.sync_loop(st).await;
    }

    /// Serve the config in the state directory when the control plane
    /// confirmed it recently enough, with the spend saved beside it; budgeted
    /// scopes wait for the first sync when no spend was saved. Not while a
    /// refusal is on record there: the count goes on from when it began, and
    /// only a join that succeeds serves again.
    async fn restore(&self, st: &State) {
        let Some(state) = self.state.clone() else { return };
        let read = {
            let state = state.clone();
            tokio::task::spawn_blocking(move || (state.refused_at(), state.load())).await
        };
        let Ok((refused_at, saved)) = read else { return };
        if let Some(at) = refused_at {
            // a time further ahead than clocks differ counts from now
            let ago = now_ms() - at;
            let before = Duration::from_millis(if ago < -MAX_CLOCK_SKEW_MS { 0 } else { ago.max(0) as u64 });
            *self.refused_since.lock().unwrap() = Some((Instant::now(), before));
            if before >= self.refuse_after {
                self.stop_serving(st, before);
            } else {
                log_warn!(
                    "state: the control plane was refusing this gateway {}s ago; not serving the saved config until it can join",
                    before.as_secs()
                );
            }
            return;
        }
        let Some(saved) = saved else { return };
        let limit = st.ready.max_config_age().map_or(MAX_SAVED_AGE, |max| max.min(MAX_SAVED_AGE));
        let age_ms = now_ms() - saved.confirmed.confirmed_at_ms;
        if age_ms > limit.as_millis() as i64 || age_ms < -MAX_CLOCK_SKEW_MS {
            log_info!(
                "state: the saved config was last confirmed {}s ago, the limit is {}s; waiting for the control plane",
                age_ms / 1000,
                limit.as_secs()
            );
            return;
        }
        let cfg = match load(saved.gzip, &saved.config) {
            Ok(cfg) => cfg,
            Err(e) => {
                log_warn!("state: the saved config is unusable ({e}); deleting it");
                let _ = tokio::task::spawn_blocking(move || state.forget()).await;
                return;
            }
        };
        if cfg.version != saved.confirmed.version || cfg.group.id != saved.confirmed.group {
            log_info!("state: the saved config and its confirmation do not match; waiting for the control plane");
            return;
        }
        match &saved.confirmed.spend {
            Some(spend) => limits::restore(spend),
            None => limits::require_spend(),
        }
        log_info!(
            "state: serving the saved config, confirmed {}s ago{}",
            age_ms.max(0) / 1000,
            if saved.confirmed.spend.is_some() { "" } else { "; budgeted keys wait for the first sync" }
        );
        self.install(st, cfg);
        st.ready.config_ok_ago(Duration::from_millis(age_ms.max(0) as u64));
        st.ready.set(true);
    }

    /// Write the state directory when something changed, SAVE_INTERVAL
    /// apart at most.
    async fn save_loop(self: Arc<Self>) {
        let Some(state) = self.state.clone() else { return };
        loop {
            state.changed().await;
            let state = state.clone();
            let _ = tokio::task::spawn_blocking(move || state.flush(limits::spend)).await;
            tokio::time::sleep(SAVE_INTERVAL).await;
        }
    }

    fn install(&self, st: &State, cfg: Config) {
        let ids: std::collections::HashSet<&str> = cfg
            .models
            .values()
            .chain(cfg.patterns.iter().map(|(_, d)| d))
            .flat_map(|ds| ds.iter().map(|d| d.id.as_str()))
            .collect();
        health::retain(&ids);
        metrics::CONFIG_VERSION.store(cfg.version, Ordering::Relaxed);
        log_info!(
            "config version {}: {} keys, {} models",
            cfg.version,
            cfg.keys.len(),
            cfg.names.len() + cfg.patterns.len()
        );
        st.config.store(Some(Arc::new(cfg)));
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
                Ok(Call::Unauthorized(_)) => self.rejoin(generation).await,
                Ok(Call::Ok(status, _, _)) => log_warn!("session renew: {status}"),
                Err(e) => log_warn!("session renew: {e}"),
            }
        }
    }

    /// Long-poll the config. The first poll names no version, so a config
    /// from the state directory is replaced by the control plane's own.
    async fn config_loop(self: Arc<Self>, st: &'static State) {
        loop {
            let (s, generation) = self.session();
            let etag = match self.config_version.load(Ordering::Relaxed) {
                0 => Vec::new(),
                version => vec![("if-none-match", format!("\"v{version}\""))],
            };
            match self.call(Method::GET, "/gw/v1/config?wait=25", &s, None, &etag, Duration::from_secs(40)).await {
                Ok(Call::Ok(StatusCode::OK, headers, bytes)) => {
                    let gzip = headers.get(header::CONTENT_ENCODING) == Some(&HeaderValue::from_static("gzip"));
                    match load(gzip, &bytes) {
                        Ok(cfg) => {
                            self.config_version.store(cfg.version, Ordering::Relaxed);
                            if let Some(state) = &self.state {
                                state.put_config(gzip, bytes, now_ms(), cfg.version, &cfg.group.id);
                            }
                            self.install(st, cfg);
                            st.ready.config_ok();
                        }
                        Err(e) => {
                            log_error!("config rejected: {e}");
                            tokio::time::sleep(Duration::from_secs(5)).await;
                        }
                    }
                }
                Ok(Call::Ok(StatusCode::NOT_MODIFIED, _, _)) => {
                    st.ready.config_ok();
                    if let Some(state) = &self.state {
                        state.confirm(now_ms());
                    }
                }
                Ok(Call::Unauthorized(_)) => self.rejoin(generation).await,
                Ok(Call::Ok(status, _, _)) => {
                    log_warn!("config: {status}");
                    tokio::time::sleep(Duration::from_secs(2)).await;
                }
                Err(e) => {
                    log_warn!("config: {e}");
                    tokio::time::sleep(Duration::from_secs(2)).await;
                }
            }
        }
    }

    async fn sync_loop(self: Arc<Self>, st: &'static State) {
        loop {
            let synced = self.sync_once(&mut *self.syncer.lock().await).await;
            if synced && st.config.load().is_some() {
                st.ready.set(true);
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
            let (events, sending) = usage::take_batch(MAX_BATCH, s.max_bytes());
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
            let frozen = Frozen { batch: s.batch, cursor: s.cursor.clone(), events, sending, rates, stats };
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
                log_debug!("sync: batch {} acknowledged with {} events", batch.frozen.batch, batch.frozen.events.len());
                if let Some(state) = &self.state {
                    state.touch();
                }
                s.cursor = counters.cursor;
                s.pending = None;
                true
            }
            Ok(Call::Unauthorized(_)) => {
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
                    log_warn!("sync: {status}; sending batches of at most {} bytes", s.max_bytes());
                    limits::discard(&batch.scopes);
                    let Frozen { events, sending, .. } = batch.frozen;
                    usage::requeue_batch(events, sending);
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
                    log_error!("sync: {status}: {why}; dropping a batch of {n} events");
                    limits::discard(&batch.scopes);
                } else {
                    log_warn!("sync: {status}: {why}");
                }
                false
            }
            Err(e) => {
                metrics::SYNC_FAILURES.fetch_add(1, Ordering::Relaxed);
                log_warn!("sync: {e}");
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
                    Ok(Call::Unauthorized(_)) => {
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
    /// The events' content, counted against the capture budget while the
    /// batch lives.
    sending: usage::Sending,
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
                log_error!("sync: could not encode a batch");
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
            log_warn!("sync: 413 at the smallest batch size; sending {stripped} events without their content");
        } else if f.events.len() > 1 {
            log_warn!(
                "sync: 413 at the smallest batch size; sending {} of its {} events",
                f.events.len() / 2,
                f.events.len()
            );
            let rest = f.events.split_off(f.events.len() / 2);
            let moved = usage::Sending::of(&rest);
            f.sending = usage::Sending::of(&f.events);
            usage::requeue_batch(rest, moved);
        } else {
            metrics::SYNC_REFUSED_EVENTS.fetch_add(f.events.len() as u64, Ordering::Relaxed);
            log_error!("sync: 413 for a single event without content; dropping it");
            f.events.clear();
        }
        f.sending = usage::Sending::of(&f.events);
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

/// Build the config from a config response's body, as received or as saved.
fn load(gzip: bool, bytes: &[u8]) -> Result<Config, String> {
    let raw = if gzip {
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

    use crate::state::tests::TempDir;
    use std::sync::atomic::AtomicUsize;

    fn new_state() -> &'static State {
        Box::leak(Box::new(State {
            config: Default::default(),
            ready: Default::default(),
            draining: Default::default(),
            max_in_flight: 1,
        }))
    }

    /// A config with a budgeted key and a free one, as the control plane
    /// sends it.
    fn wire_config(version: i64) -> Vec<u8> {
        serde_json::to_vec(&serde_json::json!({
            "schema": 2, "version": version, "group": {"id": "g1", "name": "group"},
            "keys": [
                {"hash": "11".repeat(32), "id": "cp-budgeted", "budget_usd": 1.0},
                {"hash": "22".repeat(32), "id": "cp-free"},
            ],
        }))
        .unwrap()
    }

    fn key_of(st: &State, hash: u8) -> Arc<crate::config::Key> {
        st.config.load_full().unwrap().keys[&[hash; 32]].clone()
    }

    /// A state directory as a gateway that got config version 3 left it.
    fn saved(dir: &TempDir, token: &str, confirmed_at_ms: i64, spend: Option<&[(&str, f64)]>) {
        let store = Store::open(&dir.0, token).unwrap();
        store.put_config(false, wire_config(3).into(), confirmed_at_ms, 3, "g1");
        store.flush(|| spend.map(|s| s.iter().map(|(k, v)| (k.to_string(), *v)).collect()));
    }

    /// A gateway starting on `dir` with no control plane to reach.
    async fn started(dir: &TempDir, token: &str, max_age: Option<Duration>) -> &'static State {
        let st = new_state();
        st.ready.set_max_config_age(max_age);
        let store = Store::open(&dir.0, token).unwrap();
        let cp = ControlPlane::new("http://127.0.0.1:1", token.into(), "test".into()).with_state(Some(Arc::new(store)));
        cp.restore(st).await;
        st
    }

    #[tokio::test]
    #[allow(clippy::await_holding_lock)] // one test at a time: see hold_globals
    async fn a_saved_config_serves_at_once_with_its_spend() {
        let _globals = hold_globals();
        let dir = TempDir::new("restore");
        saved(&dir, "llmrg_a", now_ms() - 90_000, Some(&[("key:cp-budgeted", 1.5)]));
        let st = started(&dir, "llmrg_a", None).await;
        assert!(st.is_ready(), "without waiting for the control plane");
        assert_eq!(st.config.load_full().unwrap().version, 3);
        let age = st.ready.config_age().unwrap();
        assert!(age >= Duration::from_secs(90) && age < Duration::from_secs(100), "{age:?}");
        assert_eq!(limits::check(&key_of(st, 0x11)).unwrap_err().code, "budget_exceeded");
        assert!(limits::check(&key_of(st, 0x22)).is_ok());

        // Spend under the budget serves, and the next sync's figure replaces it.
        saved(&dir, "llmrg_a", now_ms(), Some(&[("key:cp-budgeted", 0.5)]));
        let st = started(&dir, "llmrg_a", None).await;
        assert!(limits::check(&key_of(st, 0x11)).is_ok());
    }

    #[tokio::test]
    #[allow(clippy::await_holding_lock)] // one test at a time: see hold_globals
    async fn budgeted_keys_wait_for_a_sync_when_no_spend_was_saved() {
        let _globals = hold_globals();
        let dir = TempDir::new("nospend");
        saved(&dir, "llmrg_a", now_ms(), None);
        let st = started(&dir, "llmrg_a", None).await;
        assert!(st.is_ready());
        assert_eq!(limits::check(&key_of(st, 0x11)).unwrap_err().code, "budget_unknown");
        assert!(limits::check(&key_of(st, 0x22)).is_ok(), "a key without a budget serves");
        limits::apply(&[], &Counters::default());
        assert!(limits::check(&key_of(st, 0x11)).is_ok(), "the first sync settles it");
    }

    #[tokio::test]
    #[allow(clippy::await_holding_lock)] // one test at a time: see hold_globals
    async fn a_saved_config_is_served_only_while_recently_confirmed() {
        let _globals = hold_globals();
        let (minute, hour) = (60_000, 3_600_000);
        for (ago_ms, max_age, served) in [
            (23 * hour, None, true),
            (25 * hour, None, false),
            (25 * hour, Some(Duration::from_secs(48 * 3600)), false),
            (2 * minute, Some(Duration::from_secs(300)), true),
            (10 * minute, Some(Duration::from_secs(300)), false),
            (-minute, None, true),
            (-10 * minute, None, false),
        ] {
            let dir = TempDir::new("age");
            saved(&dir, "llmrg_a", now_ms() - ago_ms, Some(&[]));
            let st = started(&dir, "llmrg_a", max_age).await;
            assert_eq!(st.config.load().is_some(), served, "confirmed {ago_ms} ms ago, limit {max_age:?}");
            assert_eq!(st.is_ready(), served);
            assert!(!st.ready.stale());
        }
    }

    #[tokio::test]
    #[allow(clippy::await_holding_lock)] // one test at a time: see hold_globals
    async fn unusable_state_is_deleted_and_the_gateway_starts_cold() {
        let _globals = hold_globals();
        // saved under a join token that has since been replaced
        let dir = TempDir::new("token");
        saved(&dir, "llmrg_old", now_ms(), Some(&[]));
        let st = started(&dir, "llmrg_new", None).await;
        assert!(st.config.load().is_none() && !st.is_ready());
        assert!(!dir.0.join("config.bin").exists() && !dir.0.join("confirmed.bin").exists());

        // a config this gateway cannot read
        let dir = TempDir::new("garbage");
        let store = Store::open(&dir.0, "llmrg_a").unwrap();
        store.put_config(false, Bytes::from_static(b"{\"schema\":99"), now_ms(), 3, "g1");
        store.flush(|| None);
        drop(store);
        let st = started(&dir, "llmrg_a", None).await;
        assert!(st.config.load().is_none() && !st.is_ready());
        assert!(!dir.0.join("config.bin").exists() && !dir.0.join("confirmed.bin").exists());

        // a confirmation for another group's config
        let dir = TempDir::new("mismatch");
        let store = Store::open(&dir.0, "llmrg_a").unwrap();
        store.put_config(false, wire_config(3).into(), now_ms(), 3, "g2");
        store.flush(|| None);
        drop(store);
        let st = started(&dir, "llmrg_a", None).await;
        assert!(st.config.load().is_none() && !st.is_ready());

        // no state directory at all
        let st = new_state();
        ControlPlane::new("http://127.0.0.1:1", "llmrg_a".into(), "test".into()).restore(st).await;
        assert!(st.config.load().is_none() && !st.is_ready());
    }

    /// A gateway starting on `dir`, which stops serving once refused for a minute.
    async fn restarted(dir: &TempDir) -> (ControlPlane, &'static State) {
        let st = new_state();
        let store = Store::open(&dir.0, "llmrg_a").unwrap();
        let cp = ControlPlane::new("http://127.0.0.1:1", "llmrg_a".into(), "test".into())
            .with_state(Some(Arc::new(store)))
            .refusing_after(Duration::from_secs(60));
        let _ = cp.st.set(st);
        cp.restore(st).await;
        (cp, st)
    }

    fn refused_for(cp: &ControlPlane) -> Duration {
        let (since, before) = cp.refused_since.lock().unwrap().unwrap();
        since.elapsed() + before
    }

    // A restart must not buy a refused gateway more time: the refusal is on
    // disk, the saved config is not served while it stands, and the count
    // goes on from when it began.
    #[tokio::test]
    #[allow(clippy::await_holding_lock)] // one test at a time: see hold_globals
    async fn a_restart_does_not_reset_a_refusal() {
        let _globals = hold_globals();
        let files = |dir: &TempDir| ["config.bin", "confirmed.bin", "refused.bin"].map(|f| dir.0.join(f).exists());
        let refused = |dir: &TempDir, at_ms: i64| {
            saved(dir, "llmrg_a", now_ms(), Some(&[]));
            Store::open(&dir.0, "llmrg_a").unwrap().put_refused(at_ms);
        };

        // within the window: nothing is served, and the time already spent counts
        let dir = TempDir::new("refused-early");
        refused(&dir, now_ms() - 59_700);
        let (cp, st) = restarted(&dir).await;
        assert!(st.config.load().is_none() && !st.is_ready() && !st.ready.refused.load(Ordering::Relaxed));
        assert!(refused_for(&cp) >= Duration::from_millis(59_700) && refused_for(&cp) < Duration::from_secs(60));
        assert_eq!(files(&dir), [true, true, true], "kept for the join that may still succeed");
        tokio::time::sleep(Duration::from_millis(400)).await;
        cp.refused(); // the next join is refused too, a minute after the first
        assert!(st.ready.refused.load(Ordering::Relaxed));
        until("the saved config is deleted", || files(&dir) == [false, false, true]).await;
        // and the next restart is past the window
        drop(cp);
        let (_, st) = restarted(&dir).await;
        assert!(st.ready.refused.load(Ordering::Relaxed) && st.config.load().is_none());

        // after the window: stopped at once, the saved config deleted
        let dir = TempDir::new("refused-late");
        refused(&dir, now_ms() - 61_000);
        let (_, st) = restarted(&dir).await;
        assert!(st.config.load().is_none() && !st.is_ready() && st.ready.refused.load(Ordering::Relaxed));
        until("the saved config is deleted", || files(&dir) == [false, false, true]).await;

        // a time far ahead of this clock counts from now
        let dir = TempDir::new("refused-future");
        refused(&dir, now_ms() + 600_000);
        let (cp, st) = restarted(&dir).await;
        assert!(st.config.load().is_none() && !st.ready.refused.load(Ordering::Relaxed));
        assert!(refused_for(&cp) < Duration::from_secs(5));

        // no refusal on record: served as before
        let dir = TempDir::new("refused-never");
        saved(&dir, "llmrg_a", now_ms(), Some(&[]));
        let (cp, st) = restarted(&dir).await;
        assert!(st.is_ready() && cp.refused_since.lock().unwrap().is_none());
    }

    /// How a mock control plane answers a join.
    const ACCEPT: usize = 0;
    const REFUSE: usize = 1; // 401 with the control plane's header
    const BARE_401: usize = 2; // 401 as a proxy would send it

    #[derive(Default)]
    struct Gwcp {
        mode: AtomicUsize,
        joins: AtomicUsize,
        /// The If-None-Match of each config poll.
        polls: Mutex<Vec<Option<String>>>,
    }

    /// A control plane with the whole gateway protocol: joins answered as
    /// `mode` says, sessions that end when it stops accepting, config
    /// version 3, and syncs that report no spend.
    async fn gateway_cp() -> (String, Arc<Gwcp>) {
        use hyper::service::service_fn;
        use hyper_util::rt::{TokioExecutor, TokioIo};
        let _ = rustls::crypto::ring::default_provider().install_default();
        let cp: Arc<Gwcp> = Default::default();
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let url = format!("http://{}", listener.local_addr().unwrap());
        let shared = cp.clone();
        tokio::spawn(async move {
            while let Ok((conn, _)) = listener.accept().await {
                let cp = shared.clone();
                tokio::spawn(async move {
                    let svc = service_fn(move |req: Request<hyper::body::Incoming>| {
                        let cp = cp.clone();
                        async move {
                            let mode = cp.mode.load(Ordering::Relaxed);
                            let resp = http::Response::builder();
                            let answer = |resp: http::response::Builder, status: u16, body: Vec<u8>| {
                                Ok::<_, std::convert::Infallible>(
                                    resp.status(status).body(Full::new(Bytes::from(body))).unwrap(),
                                )
                            };
                            let path = req.uri().path().to_string();
                            if path == "/gw/v1/join" {
                                cp.joins.fetch_add(1, Ordering::Relaxed);
                                return match mode {
                                    ACCEPT => answer(
                                        resp,
                                        200,
                                        serde_json::to_vec(&serde_json::json!({
                                            "instance_id": "i1", "session": "s", "sync_interval_ms": 20,
                                            "expires_at_ms": now_ms() + 900_000,
                                            "group": {"id": "g1", "name": "group"},
                                        }))
                                        .unwrap(),
                                    ),
                                    REFUSE => {
                                        answer(resp.header(REFUSED_HEADER, "1"), 401, b"unknown join token".to_vec())
                                    }
                                    _ => answer(resp, 401, b"<html>sign in</html>".to_vec()),
                                };
                            }
                            if mode != ACCEPT {
                                return answer(resp, 401, b"gateway group removed".to_vec());
                            }
                            match path.as_str() {
                                "/gw/v1/config" => {
                                    let tag =
                                        req.headers().get("if-none-match").map(|v| v.to_str().unwrap().to_string());
                                    cp.polls.lock().unwrap().push(tag.clone());
                                    if tag.as_deref() == Some("\"v3\"") {
                                        tokio::time::sleep(Duration::from_millis(20)).await;
                                        return answer(resp, 304, Vec::new());
                                    }
                                    answer(resp, 200, wire_config(3))
                                }
                                _ => answer(resp, 200, b"{}".to_vec()),
                            }
                        }
                    });
                    let _ = hyper_util::server::conn::auto::Builder::new(TokioExecutor::new())
                        .serve_connection(TokioIo::new(conn), svc)
                        .await;
                });
            }
        });
        (url, cp)
    }

    async fn until(what: &str, cond: impl Fn() -> bool) {
        let deadline = Instant::now() + Duration::from_secs(20);
        while !cond() {
            assert!(Instant::now() < deadline, "timed out waiting until {what}");
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    }

    // A deleted group or a replaced join token: the control plane says so on
    // the join, and after `refuse_after` of that the gateway stops serving
    // and deletes what it saved. A join that succeeds brings it back.
    #[tokio::test]
    #[allow(clippy::await_holding_lock)] // one test at a time: see hold_globals
    async fn a_gateway_the_control_plane_refuses_stops_serving_until_it_can_join_again() {
        let _globals = hold_globals();
        let (url, mock) = gateway_cp().await;
        let dir = TempDir::new("refused");
        // saved by an earlier run: served before the join, then polled for without a version
        saved(&dir, "llmrg_test", now_ms() - 60_000, Some(&[]));
        let store = Arc::new(Store::open(&dir.0, "llmrg_test").unwrap());
        let cp = ControlPlane::new(&url, "llmrg_test".into(), "test".into())
            .with_state(Some(store))
            .refusing_after(Duration::from_millis(500));
        let st = new_state();
        let running = tokio::spawn(Arc::new(cp).start(st));
        until("the config is polled twice", || mock.polls.lock().unwrap().len() >= 2).await;
        assert_eq!(
            mock.polls.lock().unwrap()[..2],
            [None, Some("\"v3\"".to_string())],
            "the first poll is unconditional"
        );
        until("a 304 confirms the config", || st.ready.config_age().is_some_and(|a| a < Duration::from_secs(30))).await;
        assert!(st.is_ready() && !st.ready.refused.load(Ordering::Relaxed));

        mock.mode.store(REFUSE, Ordering::Relaxed);
        let joins = mock.joins.load(Ordering::Relaxed);
        until("the first refusal", || mock.joins.load(Ordering::Relaxed) > joins).await;
        assert!(st.is_ready() && st.config.load().is_some(), "one refusal changes nothing");
        until("the refusal is on disk", || dir.0.join("refused.bin").exists()).await;
        until("it stops serving", || st.ready.refused.load(Ordering::Relaxed)).await;
        assert!(st.config.load().is_none() && !st.is_ready());
        assert!(mock.joins.load(Ordering::Relaxed) >= joins + 2);
        until("the saved state is deleted", || {
            !dir.0.join("config.bin").exists() && !dir.0.join("confirmed.bin").exists()
        })
        .await;

        assert!(dir.0.join("refused.bin").exists(), "the refusal stays on record while it stands");

        mock.mode.store(ACCEPT, Ordering::Relaxed);
        until("it serves again", || st.is_ready()).await;
        until("the refusal is off the record", || !dir.0.join("refused.bin").exists()).await;
        assert!(!st.ready.refused.load(Ordering::Relaxed));
        assert_eq!(st.config.load_full().unwrap().version, 3);
        let unconditional = mock.polls.lock().unwrap().iter().filter(|tag| tag.is_none()).count();
        assert_eq!(unconditional, 2, "the config is fetched anew");

        running.abort();
        let _ = running.await;
    }

    // A 401 without the control plane's header (a proxy's, or an older
    // control plane's) is no verdict: the gateway keeps serving what it has.
    #[tokio::test]
    #[allow(clippy::await_holding_lock)] // one test at a time: see hold_globals
    async fn a_401_that_is_not_the_control_planes_verdict_never_stops_a_gateway() {
        let _globals = hold_globals();
        let (url, mock) = gateway_cp().await;
        let cp = ControlPlane::new(&url, "llmrg_test".into(), "test".into()).refusing_after(Duration::ZERO);
        let st = new_state();
        let running = tokio::spawn(Arc::new(cp).start(st));
        until("it serves", || st.is_ready()).await;

        mock.mode.store(BARE_401, Ordering::Relaxed);
        let joins = mock.joins.load(Ordering::Relaxed);
        until("three joins are answered 401", || mock.joins.load(Ordering::Relaxed) >= joins + 3).await;
        assert!(st.is_ready() && st.config.load().is_some() && !st.ready.refused.load(Ordering::Relaxed));

        // the same gateway stops at the first refusal that is one
        mock.mode.store(REFUSE, Ordering::Relaxed);
        until("it stops serving", || st.ready.refused.load(Ordering::Relaxed)).await;
        assert!(st.config.load().is_none());

        running.abort();
        let _ = running.await;
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
        assert_eq!(usage::sending_content(), 0, "nothing is counted once every batch is settled");
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

    /// A batch's captured content counts against the budget while it waits to
    /// be acknowledged, and stops counting once it is, once it is refused for
    /// good, or once a 413 puts it back in the queue (where the queue counts
    /// it, content intact).
    #[tokio::test]
    #[allow(clippy::await_holding_lock)] // one test at a time: see hold_globals
    async fn the_batch_being_sent_holds_its_content_until_settled() {
        let _globals = hold_globals();
        // 503, then 200; 400; 413, then 200
        let (url, _) = mock_cp(|n, _| match n {
            1 => 503,
            3 => 400,
            4 => 413,
            _ => 200,
        })
        .await;
        drop(usage::take(usize::MAX, usize::MAX));
        let send = |model: &str| {
            let mut em = emit_capturing(model, 1 << 20);
            em.capture_request(br#"{"messages":[{"role":"user","content":"question"}]}"#, false);
            em.set_output(crate::translate::oai_json_text(
                br#"{"choices":[{"message":{"content":"answer"}}]}"#,
                1 << 20,
            ));
            em.send(200, "", Default::default(), 0);
        };
        let cp = ControlPlane::new(&url, "llmrg_test".into(), "test".into());
        let mut s = cp.syncer.lock().await;

        send("pending");
        let queued = usage::queued_content();
        assert!(queued > 0);
        assert!(!cp.sync_once(&mut s).await, "503");
        assert_eq!((usage::queued_content(), usage::sending_content()), (0, queued), "counted while unacknowledged");
        assert!(cp.sync_once(&mut s).await);
        assert_eq!(usage::sending_content(), 0, "acknowledged");

        send("refused");
        assert!(!cp.sync_once(&mut s).await, "400");
        assert_eq!(usage::sending_content(), 0, "dropped");

        send("too-large");
        let queued = usage::queued_content();
        assert!(!cp.sync_once(&mut s).await, "413");
        assert_eq!((usage::queued_content(), usage::sending_content()), (queued, 0), "back in the queue, content kept");
        assert!(cp.sync_once(&mut s).await);
        assert_eq!((usage::queued_content(), usage::sending_content()), (0, 0));
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
