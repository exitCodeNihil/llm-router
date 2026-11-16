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
use crate::delta;
use crate::limits::{self, Counters, RateDelta};
use crate::server::{HttpsClient, State, https_client};
use crate::spool::{self, Spool};
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
/// The most a replay request carries: the control plane reads up to 32 MiB.
const REPLAY_EVENTS: usize = 5000;
const REPLAY_BYTES: usize = 8 << 20;
/// How long the first sync waits for the replay of what an earlier process
/// left; past that the replay goes on beside the syncs.
const REPLAY_FIRST: Duration = Duration::from_secs(30);
/// This many 404s in a row, and no replay ever answered, mean the control
/// plane has none.
const REPLAY_MISSING: u32 = 3;
/// The wait after a failed replay of leftovers; it doubles, up to 30 s.
const REPLAY_RETRY: Duration = Duration::from_secs(1);
/// The oldest confirmation a config from the state directory is served on.
const MAX_SAVED_AGE: Duration = Duration::from_secs(24 * 3600);
/// How far ahead of this clock a saved confirmation may be: past that the
/// clock, or the file, is wrong.
const MAX_CLOCK_SKEW_MS: i64 = 5 * 60_000;
/// The control-plane replicas whose limits cursor a sync names: the ones
/// that answered most recently.
const MAX_CURSORS: usize = 8;
/// How often a gateway that joined with the schema before its own asks the
/// control plane for its own again.
const UPGRADE_EVERY: Duration = Duration::from_secs(600);
/// The least time between two joins made because the control plane answered
/// a schema 3 session with an older config: replicas of two versions behind
/// one address would otherwise have the gateway join on every poll.
const RENEGOTIATE_EVERY: Duration = Duration::from_secs(60);
/// Sent with a config poll in a schema 3 session: the digest of the config
/// held, so the control plane only sends a delta to a gateway that holds
/// exactly what the delta is built on.
const DIGEST_HEADER: &str = "x-llmr-config-digest";

/// What the gateway keeps of the config in use besides the hot path's own
/// form: enough to poll for the next one, to patch it with a delta, and to
/// save it.
#[derive(Clone)]
struct Held {
    epoch: String,
    version: i64,
    group: String,
    /// Its digest, once the control plane's matched its entries: without
    /// one, no delta is asked for.
    digest: Option<String>,
    /// The full config as a config response's body, received or patched.
    gzip: bool,
    body: Bytes,
}

pub struct ControlPlane {
    base: String,
    join_token: String,
    name: String,
    client: HttpsClient,
    session: ArcSwap<String>,
    generation: AtomicU64,
    /// Successful joins: each makes the gateway a new instance to the control
    /// plane. A renewed session does not.
    joins: AtomicU64,
    /// The control plane has answered a replay with 200: a 404 from then on
    /// is a proxy's, not a control plane without replay.
    replay_seen: AtomicBool,
    expires_at_ms: AtomicI64,
    sync_interval_ms: AtomicU64,
    join_lock: tokio::sync::Mutex<()>,
    started: Instant,
    leaving: AtomicBool,
    /// The config in use; None asks for it whatever the control plane has.
    held: Mutex<Option<Held>>,
    /// Ask for the full config with the next poll, naming none: after a
    /// delta that did not apply, and for a saved config that cannot say
    /// which one it is.
    refetch: AtomicBool,
    /// The wire schema this session was joined with: SCHEMA, or the one
    /// before it against a control plane that has not been upgraded yet.
    schema: AtomicI64,
    older_warned: AtomicBool,
    /// The config held is the saved one, not yet replaced or confirmed.
    restored: AtomicBool,
    /// How often a session on an older schema asks for this gateway's own.
    upgrade_every: Duration,
    /// The control plane has refused the join since the last one that
    /// succeeded: when that was noticed in this process, and for how long it
    /// had been going on by then, as the state directory had it.
    refused_since: Mutex<Option<(Instant, Duration)>>,
    refuse_after: Duration,
    st: OnceLock<&'static State>,
    state: Option<Arc<Store>>,
    replay_first: Duration,
    replay_retry: Duration,
    /// Usage on disk until the control plane has it.
    spool: Option<Arc<Spool>>,
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
    /// The schema the control plane will speak with this gateway.
    #[serde(default)]
    schema: i64,
}

enum JoinError {
    /// The control plane only knows an older schema than the one asked for.
    Older(String),
    Other(String),
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
            joins: AtomicU64::new(0),
            replay_seen: AtomicBool::new(false),
            expires_at_ms: AtomicI64::new(0),
            sync_interval_ms: AtomicU64::new(2000),
            join_lock: tokio::sync::Mutex::new(()),
            started: Instant::now(),
            leaving: AtomicBool::new(false),
            held: Mutex::new(None),
            refetch: AtomicBool::new(false),
            schema: AtomicI64::new(SCHEMA),
            older_warned: AtomicBool::new(false),
            restored: AtomicBool::new(false),
            upgrade_every: UPGRADE_EVERY,
            refused_since: Mutex::new(None),
            refuse_after: REFUSE_AFTER,
            st: OnceLock::new(),
            state: None,
            replay_first: REPLAY_FIRST,
            replay_retry: REPLAY_RETRY,
            spool: None,
            syncer: tokio::sync::Mutex::new(Syncer::default()),
        }
    }

    /// Keep the config and spend in `state` across restarts.
    pub fn with_state(mut self, state: Option<Arc<Store>>) -> ControlPlane {
        self.state = state;
        self
    }

    /// Keep unsent usage in `spool` across restarts.
    pub fn with_spool(mut self, spool: Option<Arc<Spool>>) -> ControlPlane {
        self.spool = spool;
        self
    }

    #[cfg(test)]
    fn replaying_with(mut self, first: Duration, retry: Duration) -> ControlPlane {
        (self.replay_first, self.replay_retry) = (first, retry);
        self
    }

    #[cfg(test)]
    fn upgrading_every(mut self, d: Duration) -> ControlPlane {
        self.upgrade_every = d;
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

    /// Join with this gateway's schema, or with the one before it when the
    /// control plane says it is older: such a session has no config deltas
    /// and none of the newer fields, as before them.
    async fn join(&self) -> Result<(), String> {
        let (j, schema) = match self.join_as(SCHEMA, false).await {
            Ok(j) => (j, SCHEMA),
            Err(JoinError::Older(why)) => {
                if !self.older_warned.swap(true, Ordering::Relaxed) {
                    log_warn!(
                        "the control plane does not know config schema {SCHEMA} ({why}); joining with schema {}: no config deltas until it is upgraded, which is looked for every {}s",
                        SCHEMA - 1,
                        self.upgrade_every.as_secs()
                    );
                }
                match self.join_as(SCHEMA - 1, false).await {
                    Ok(j) => (j, SCHEMA - 1),
                    Err(JoinError::Older(e) | JoinError::Other(e)) => return Err(e),
                }
            }
            Err(JoinError::Other(e)) => return Err(e),
        };
        self.joined(&j, schema);
        Ok(())
    }

    /// A join succeeded, asked for with `schema`: this gateway is a new
    /// instance to the control plane from here on.
    fn joined(&self, j: &Joined, schema: i64) {
        self.schema.store(if (1..schema).contains(&j.schema) { j.schema } else { schema }, Ordering::Relaxed);
        self.adopt(j);
        self.joins.fetch_add(1, Ordering::Relaxed);
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
    }

    /// On a session joined with an older schema: ask the control plane for
    /// this gateway's own again, in case it was upgraded since. A join that
    /// succeeds replaces the session; any other answer leaves the one in use
    /// as it is. True when the session was replaced.
    async fn try_upgrade(&self) -> bool {
        let _guard = self.join_lock.lock().await;
        if self.schema.load(Ordering::Relaxed) >= SCHEMA {
            return false;
        }
        match self.join_as(SCHEMA, true).await {
            Ok(j) => {
                log_info!("the control plane now knows config schema {SCHEMA}");
                self.joined(&j, SCHEMA);
                // The config held came without an epoch or a digest: fetch
                // it in full once, so later changes can come as deltas.
                self.refetch.store(true, Ordering::Relaxed);
                true
            }
            Err(JoinError::Older(_)) => false,
            Err(JoinError::Other(e)) => {
                log_debug!("looking for an upgraded control plane: {e}");
                false
            }
        }
    }

    /// Ask to join with `schema`. A `probe` only looks whether the control
    /// plane would take it: its refusal says nothing about the session in use.
    async fn join_as(&self, schema: i64, probe: bool) -> Result<Joined, JoinError> {
        let body = serde_json::to_vec(&serde_json::json!({"name": self.name, "version": VERSION, "schema": schema}))
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
            .await
            .map_err(JoinError::Other)?;
        let (status, bytes) = match call {
            Call::Ok(status, _, bytes) => (status, bytes),
            Call::Unauthorized(refused) => {
                if refused && !probe {
                    self.refused();
                }
                return Err(JoinError::Other("the control plane refused the join token".into()));
            }
        };
        if status != StatusCode::OK {
            let why = String::from_utf8_lossy(&bytes[..bytes.len().min(300)]).trim().to_string();
            // An older control plane refuses a schema it does not know and
            // says to upgrade it; any other 400 is about something else.
            return Err(if status == StatusCode::BAD_REQUEST && why.to_ascii_lowercase().contains("upgrad") {
                JoinError::Older(why)
            } else {
                JoinError::Other(format!("join: {status}: {why}"))
            });
        }
        serde_json::from_slice(&bytes).map_err(|e| JoinError::Other(format!("join response: {e}")))
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
        *self.held.lock().unwrap() = None;
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
        // Usage an earlier process left goes first, so the first sync's spend
        // figures count it.
        if self.spool.as_ref().is_some_and(|spool| !spool.leftovers().is_empty()) {
            let (done, finished) = tokio::sync::oneshot::channel();
            let cp = self.clone();
            loops.spawn(async move {
                cp.replay_leftovers().await;
                let _ = done.send(());
            });
            let _ = tokio::time::timeout(self.replay_first, finished).await;
        }
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
        let (gzip, body) = (saved.gzip, Bytes::from(saved.config));
        let loaded = tokio::task::spawn_blocking(move || load(gzip, body)).await;
        let Loaded { cfg, held, .. } = match loaded.unwrap_or_else(|e| Err(e.to_string())) {
            Ok(loaded) => loaded,
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
        // A saved config that knows its digest is polled for by name, and
        // the control plane answers with what changed since; one that does
        // not is replaced by the control plane's own, whatever it has.
        self.refetch.store(held.digest.is_none(), Ordering::Relaxed);
        self.restored.store(true, Ordering::Relaxed);
        // A control plane with nothing newer answers 304 from here on: the
        // saved confirmation is the one those answers, and syncs, renew.
        state.resume(saved.confirmed.clone());
        *self.held.lock().unwrap() = Some(held);
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
            let _ = tokio::task::spawn_blocking(move || {
                state.touch();
                state.flush(limits::spend)
            })
            .await;
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

    /// Renew the session five minutes before it expires. On a session
    /// joined with an older schema, also look every `upgrade_every` whether
    /// the control plane has been upgraded.
    async fn renew_loop(self: Arc<Self>) {
        loop {
            let left = self.expires_at_ms.load(Ordering::Relaxed) - now_ms() - 5 * 60_000;
            let left = Duration::from_millis(left.max(30_000) as u64);
            let fallback = self.schema.load(Ordering::Relaxed) < SCHEMA;
            let wait = if fallback { left.min(self.upgrade_every) } else { left };
            tokio::time::sleep(wait).await;
            // A new join is a new session: nothing to renew. Woken early
            // only to look, the session is not due yet either.
            if (fallback && self.try_upgrade().await) || wait < left {
                continue;
            }
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

    /// Long-poll the config, naming the one held so the control plane
    /// answers only once it has another: with that config, or in a schema 3
    /// session with what changed since (a delta). A delta that does not
    /// apply is discarded and the full config fetched instead.
    async fn config_loop(self: Arc<Self>, st: &'static State) {
        let mut discarded = 0u32;
        let mut renegotiated: Option<Instant> = None;
        loop {
            let (s, generation) = self.session();
            let schema = self.schema.load(Ordering::Relaxed);
            let held = self.held.lock().unwrap().clone();
            // Before schema 3 a version says nothing about which control
            // plane numbered it: a saved config is never named there.
            let unnamed = self.refetch.load(Ordering::Relaxed) || (schema < 3 && self.restored.load(Ordering::Relaxed));
            let named = held.as_ref().filter(|_| !unnamed);
            let headers = named.map(|h| poll_headers(h, schema)).unwrap_or_default();
            let unconditional = headers.is_empty();
            match self.call(Method::GET, "/gw/v1/config?wait=25", &s, None, &headers, Duration::from_secs(40)).await {
                Ok(Call::Ok(StatusCode::OK, headers, bytes)) => {
                    let gzip = headers.get(header::CONTENT_ENCODING) == Some(&HeaderValue::from_static("gzip"));
                    // Reading a large config takes a while: off the request threads.
                    let received =
                        tokio::task::spawn_blocking(move || receive(held, gzip, bytes, schema, unconditional)).await;
                    match received.unwrap_or_else(|e| Received::Rejected(e.to_string())) {
                        Received::Config(loaded) => {
                            let Loaded { cfg, held, older } = *loaded;
                            discarded = 0;
                            self.refetch.store(false, Ordering::Relaxed);
                            self.restored.store(false, Ordering::Relaxed);
                            if let Some(state) = &self.state {
                                state.put_config(held.gzip, held.body.clone(), now_ms(), held.version, &held.group);
                            }
                            *self.held.lock().unwrap() = Some(held);
                            self.install(st, cfg);
                            st.ready.config_ok();
                            if older {
                                self.renegotiate(&mut renegotiated, generation).await;
                            }
                        }
                        Received::Kept { why, confirmed, older } => {
                            if confirmed {
                                st.ready.config_ok();
                                if let Some(state) = &self.state {
                                    state.confirm(now_ms());
                                }
                            } else {
                                log_warn!("config ignored: {why}");
                            }
                            if !older || !self.renegotiate(&mut renegotiated, generation).await {
                                tokio::time::sleep(Duration::from_secs(2)).await;
                            }
                        }
                        Received::Discarded(why) => {
                            log_warn!("config delta discarded: {why}; fetching the full config");
                            self.refetch.store(true, Ordering::Relaxed);
                            // at once the first time; a control plane that keeps
                            // sending deltas that do not apply is not hammered
                            tokio::time::sleep(Duration::from_secs(discarded.min(5) as u64)).await;
                            discarded += 1;
                        }
                        Received::Rejected(e) => {
                            log_error!("config rejected: {e}");
                            tokio::time::sleep(Duration::from_secs(5)).await;
                        }
                    }
                }
                Ok(Call::Ok(StatusCode::NOT_MODIFIED, _, _)) => {
                    self.restored.store(false, Ordering::Relaxed);
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

    /// The control plane answered a schema 3 session as one from before
    /// schema 3 does (it was rolled back, or replicas of two versions share
    /// an address): join again, which settles on the schema it speaks. At
    /// most every RENEGOTIATE_EVERY; true when it joined.
    async fn renegotiate(&self, last: &mut Option<Instant>, generation: u64) -> bool {
        if last.is_some_and(|at| at.elapsed() < RENEGOTIATE_EVERY) {
            return false;
        }
        *last = Some(Instant::now());
        log_warn!("the control plane answered a schema {SCHEMA} session with an older config: joining again");
        self.rejoin(generation).await;
        true
    }

    /// The config in use, as a schema 3 sync or replay names it, so the
    /// control plane can hold off until it knows that config too; None in a
    /// session that does not say.
    fn config_named(&self) -> Option<(i64, String)> {
        if self.schema.load(Ordering::Relaxed) < 3 {
            return None;
        }
        let held = self.held.lock().unwrap();
        Some(held.as_ref().map_or((0, String::new()), |h| (h.version, h.epoch.clone())))
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

    /// Write the state directory now, for shutdown: the timer may be up to
    /// SAVE_INTERVAL behind, and a restart would then start from spend that
    /// old.
    pub async fn save(&self) {
        if let Some(state) = self.state.clone() {
            let _ = tokio::task::spawn_blocking(move || {
                state.touch();
                state.flush(limits::spend)
            })
            .await;
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

    /// Send events to the control plane's replay, which stores each request
    /// once however often it is sent (POST /gw/v1/replay).
    async fn replay(&self, body: Bytes, events: usize) -> Replay {
        let (session, generation) = self.session();
        match self.call(Method::POST, "/gw/v1/replay", &session, Some((body, true)), &[], Duration::from_secs(30)).await
        {
            Ok(Call::Ok(StatusCode::OK, _, bytes)) => {
                let r: Replayed = serde_json::from_slice(&bytes).unwrap_or_default();
                self.replay_seen.store(true, Ordering::Relaxed);
                metrics::REPLAYED_EVENTS.fetch_add(events as u64, Ordering::Relaxed);
                metrics::REPLAY_DUPLICATE_EVENTS.fetch_add(r.duplicates, Ordering::Relaxed);
                log_info!(
                    "replayed {events} usage events: {} stored, {} already there, {} dropped",
                    r.stored,
                    r.duplicates,
                    r.dropped
                );
                Replay::Done
            }
            Ok(Call::Unauthorized(_)) => {
                self.rejoin(generation).await;
                Replay::Retry("the session ended".into())
            }
            Ok(Call::Ok(status, _, _)) if refused(status) => {
                metrics::REPLAY_REFUSED_EVENTS.fetch_add(events as u64, Ordering::Relaxed);
                log_error!("replay: {status}; dropping {events} usage events");
                Replay::Done
            }
            // Proxies answer 404 during a rollout: it only says "no replay
            // here" of a control plane that has never answered one.
            Ok(Call::Ok(StatusCode::NOT_FOUND, _, _)) if !self.replay_seen.load(Ordering::Relaxed) => Replay::Missing,
            Ok(Call::Ok(StatusCode::PAYLOAD_TOO_LARGE, _, _)) => Replay::TooLarge,
            Ok(Call::Ok(status, _, _)) => Replay::Retry(status.to_string()),
            Err(e) => Replay::Retry(e),
        }
    }

    /// Hand over the usage an earlier process left in the spool, oldest
    /// first, deleting each file once the control plane has answered for all
    /// of it. A control plane without replay leaves the files for a later
    /// start.
    async fn replay_leftovers(&self) {
        let Some(spool) = self.spool.clone() else { return };
        let mut chunk = Chunk::default();
        for path in spool.leftovers() {
            let file = path.clone();
            let Ok((events, torn)) = tokio::task::spawn_blocking(move || spool::read(&file)).await else { return };
            if torn > 0 {
                log_warn!(
                    "spool: {} ends in {torn} bytes that are cut short or damaged; replaying the {} events before them",
                    path.display(),
                    events.len()
                );
            }
            for json in events {
                chunk.bytes += json.len() + 1;
                chunk.events.push(json);
                if (chunk.events.len() >= REPLAY_EVENTS || chunk.bytes >= REPLAY_BYTES)
                    && !self.replay_chunk(&spool, &mut chunk).await
                {
                    return;
                }
            }
            // every event of the file is in a chunk now
            chunk.files.push(path);
        }
        self.replay_chunk(&spool, &mut chunk).await;
    }

    /// Send one chunk until the control plane answers for it; false to stop
    /// replaying.
    async fn replay_chunk(&self, spool: &Spool, chunk: &mut Chunk) -> bool {
        let n = chunk.events.len();
        if n > 0 {
            let events = std::mem::take(&mut chunk.events);
            let config = self.config_named();
            let Ok(Ok(body)) = tokio::task::spawn_blocking(move || {
                let mut json = Vec::with_capacity(events.iter().map(|e| e.len() + 1).sum::<usize>() + 16);
                json.extend_from_slice(b"{\"events\":[");
                for (i, e) in events.iter().enumerate() {
                    if i > 0 {
                        json.push(b',');
                    }
                    json.extend_from_slice(e);
                }
                json.push(b']');
                if let Some((version, epoch)) = config {
                    let epoch = serde_json::to_string(&epoch).unwrap_or_else(|_| "\"\"".into());
                    json.extend_from_slice(
                        format!(",\"config_version\":{version},\"config_epoch\":{epoch}").as_bytes(),
                    );
                }
                json.push(b'}');
                gzip(&json)
            })
            .await
            else {
                return false;
            };
            let body = Bytes::from(body);
            let mut wait = self.replay_retry;
            let mut missing = 0;
            loop {
                let outcome = self.replay(body.clone(), n).await;
                missing = if matches!(outcome, Replay::Missing) { missing + 1 } else { 0 };
                match outcome {
                    Replay::Done => break,
                    Replay::Missing if missing >= REPLAY_MISSING => {
                        log_warn!(
                            "replay: this control plane has no /gw/v1/replay; usage left by an earlier run stays in the spool until it is upgraded"
                        );
                        return false;
                    }
                    Replay::Missing => log_warn!("replay: 404; retrying in {}s", wait.as_secs_f32()),
                    Replay::TooLarge => {
                        log_warn!("replay: 413 for {n} usage events; retrying in {}s", wait.as_secs_f32());
                    }
                    Replay::Retry(why) => log_warn!("replay: {why}; retrying in {}s", wait.as_secs_f32()),
                }
                if self.leaving.load(Ordering::Relaxed) {
                    return false;
                }
                tokio::time::sleep(wait).await;
                wait = (wait * 2).min(Duration::from_secs(30));
            }
        }
        chunk.bytes = 0;
        spool.replayed(std::mem::take(&mut chunk.files)).await;
        true
    }

    /// The pending batch was sent under an earlier join: the control plane
    /// may have stored it for the instance this gateway was then, and no
    /// longer knows its number. Its events go through the replay instead,
    /// which stores each once; its rate counts are given up. Some(result)
    /// when that settled it or must be tried again, None to send it as a sync
    /// after all: to a control plane that has no replay (404 on
    /// REPLAY_MISSING syncs running and never a 200), or past a proxy that
    /// finds it too large.
    async fn replay_pending(&self, s: &mut Syncer, joins: u64) -> Option<bool> {
        let mut batch = s.pending.take()?;
        let n = batch.frozen.events.len();
        let config = self.config_named();
        let (body, frozen) = tokio::task::spawn_blocking(move || {
            let frozen = batch.frozen;
            let (config_version, config_epoch) = (config.as_ref().map(|c| c.0), config.as_ref().map(|c| c.1.as_str()));
            let body = serde_json::to_vec(&ReplayRequest { events: &frozen.events, config_version, config_epoch })
                .map_err(std::io::Error::other)
                .and_then(|json| gzip(&json));
            (body, frozen)
        })
        .await
        .ok()?;
        batch.frozen = frozen;
        let outcome = match body {
            Ok(body) => self.replay(body.into(), n).await,
            Err(_) => Replay::TooLarge,
        };
        s.replay_missing = if matches!(outcome, Replay::Missing) { s.replay_missing + 1 } else { 0 };
        match outcome {
            Replay::Done => {
                limits::discard(&batch.scopes);
                self.settle(batch.frozen.high()).await;
                Some(false)
            }
            Replay::Missing if s.replay_missing < REPLAY_MISSING => {
                log_warn!("replay: 404");
                s.pending = Some(batch);
                Some(false)
            }
            Replay::Missing | Replay::TooLarge => {
                // An older control plane, or a proxy the sync knows how to
                // get a large batch past.
                s.replay_missing = 0;
                batch.sent = Some(joins);
                s.pending = Some(batch);
                None
            }
            Replay::Retry(why) => {
                metrics::SYNC_FAILURES.fetch_add(1, Ordering::Relaxed);
                log_warn!("replay: {why}");
                s.pending = Some(batch);
                Some(false)
            }
        }
    }

    /// Every event numbered up to `seq` is acknowledged or dropped for good.
    async fn settle(&self, seq: u64) {
        if let Some(spool) = &self.spool
            && seq > 0
        {
            spool.settle(seq).await;
        }
    }

    /// One sync: freeze a batch unless the last one is unacknowledged, send it,
    /// and settle it on success.
    async fn sync_once(&self, s: &mut Syncer) -> bool {
        // What was queued since the last sync goes to disk before it is sent.
        if let Some(spool) = &self.spool {
            spool.seal().await;
        }
        if s.pending.is_none() {
            // With a spool, only what it has been through: an event queued
            // while the seal was written waits for the next sync's.
            let sealed = self.spool.as_ref().map_or(u64::MAX, |spool| spool.sealed());
            let (events, sending) = usage::take_batch_upto(MAX_BATCH, s.max_bytes(), sealed);
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
            // Schema 3: the config this gateway runs on, and its cursor at
            // each control-plane replica that answered lately.
            let config = self.config_named();
            let frozen = Frozen {
                batch: s.batch,
                cursor: s.cursor().to_string(),
                cursors: config.as_ref().map(|_| s.cursors.clone()),
                config,
                events,
                sending,
                rates,
                stats,
                dropped_high: 0,
            };
            s.pending = Batch::build(frozen, scopes).await;
        }
        // A renewed session is the same instance: only a join in between
        // sends a batch through the replay.
        let joins = self.joins.load(Ordering::Relaxed);
        let rejoined = |b: &Batch| b.sent.is_some_and(|sent| sent != joins) && !b.frozen.events.is_empty();
        if s.pending.as_ref().is_some_and(rejoined)
            && let Some(done) = self.replay_pending(s, joins).await
        {
            return done;
        }
        let (session, generation) = self.session();
        if let Some(batch) = &mut s.pending {
            batch.sent = Some(joins);
        }
        let Some(batch) = &s.pending else { return false };
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
                self.settle(batch.frozen.high()).await;
                s.answered(counters.replica, counters.cursor);
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
                    let sent = batch.sent;
                    s.pending = batch.smaller().await;
                    if let Some(batch) = &mut s.pending {
                        batch.sent = sent;
                    }
                } else if refused(status) {
                    // Resending the same bytes would be refused again and hold
                    // back every later event: drop this batch and move on.
                    let Some(batch) = s.pending.take() else { return false };
                    let n = batch.frozen.events.len();
                    metrics::SYNC_REFUSED_EVENTS.fetch_add(n as u64, Ordering::Relaxed);
                    log_error!("sync: {status}: {why}; dropping a batch of {n} events");
                    limits::discard(&batch.scopes);
                    self.settle(batch.frozen.high()).await;
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
    /// The limits cursor each control-plane replica last answered with, by
    /// replica id, the latest first: replicas number their cursors apart.
    /// A control plane that names no replica is the one named "".
    cursors: Vec<(String, String)>,
    pending: Option<Batch>,
    /// A smaller byte cap than MAX_BATCH_BYTES, after a 413.
    limit_bytes: Option<usize>,
    /// 404s in a row for the pending batch's replay.
    replay_missing: u32,
}

impl Syncer {
    fn max_bytes(&self) -> usize {
        self.limit_bytes.unwrap_or(MAX_BATCH_BYTES)
    }

    /// The cursor of the replica that answered last.
    fn cursor(&self) -> &str {
        self.cursors.first().map_or("", |(_, cursor)| cursor)
    }

    /// `replica` answered a sync with `cursor`.
    fn answered(&mut self, replica: String, cursor: String) {
        self.cursors.retain(|(r, _)| *r != replica);
        self.cursors.insert(0, (replica, cursor));
        self.cursors.truncate(MAX_CURSORS);
    }
}

/// A frozen upload, resent as is until acknowledged. What it was built from is
/// kept, to queue its events again or build it smaller if it turns out to be
/// too large to send.
struct Batch {
    body: Bytes,
    scopes: Vec<Arc<limits::Scope>>,
    frozen: Frozen,
    /// The join (`ControlPlane::joins`) it was last sent under; None before
    /// the first send.
    sent: Option<u64>,
}

/// A batch's contents: the same number, cursor and rates however it is built.
struct Frozen {
    batch: i64,
    cursor: String,
    /// Schema 3 only: every replica's cursor, and the config in use.
    cursors: Option<Vec<(String, String)>>,
    config: Option<(i64, String)>,
    events: Vec<Event>,
    /// The events' content, counted against the capture budget while the
    /// batch lives.
    sending: usage::Sending,
    rates: Vec<RateDelta>,
    stats: Stats,
    /// The highest sequence number among events dropped from it for good.
    dropped_high: u64,
}

impl Frozen {
    /// The highest sequence number it accounts for: events are in order.
    fn high(&self) -> u64 {
        self.events.last().map_or(0, |e| e.seq).max(self.dropped_high)
    }
}

/// What the control plane answers a replay with.
#[derive(Default, Deserialize)]
#[serde(default)]
struct Replayed {
    stored: u64,
    duplicates: u64,
    dropped: u64,
}

enum Replay {
    /// Answered for: stored, already there, or refused for good.
    Done,
    /// 404 from a control plane that has never answered a replay: it may
    /// have none (before 0.4), or a proxy answered for it.
    Missing,
    TooLarge,
    Retry(String),
}

#[derive(Serialize)]
struct ReplayRequest<'a> {
    events: &'a [Event],
    #[serde(skip_serializing_if = "Option::is_none")]
    config_version: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    config_epoch: Option<&'a str>,
}

/// Leftover events on their way to the replay, and the spool files that end
/// within them.
#[derive(Default)]
struct Chunk {
    events: Vec<Vec<u8>>,
    bytes: usize,
    files: Vec<std::path::PathBuf>,
}

fn gzip(json: &[u8]) -> std::io::Result<Vec<u8>> {
    let mut gz = flate2::write::GzEncoder::new(Vec::with_capacity(json.len() / 8), flate2::Compression::fast());
    gz.write_all(json)?;
    gz.finish()
}

impl Batch {
    /// Encode a batch. Encoding a large one takes milliseconds: keep it off
    /// the request threads. JSON first, then one gzip pass; serde's many small
    /// writes straight into the encoder cost ~50x more.
    async fn build(frozen: Frozen, scopes: Vec<Arc<limits::Scope>>) -> Option<Batch> {
        let body = tokio::task::spawn_blocking(move || {
            let f = &frozen;
            let req = SyncRequest {
                batch: f.batch,
                cursor: &f.cursor,
                cursors: f.cursors.as_ref().map(|c| c.iter().map(|(r, c)| (r.as_str(), c.as_str())).collect()),
                config_version: f.config.as_ref().map(|c| c.0),
                config_epoch: f.config.as_ref().map(|c| c.1.as_str()),
                events: &f.events,
                rates: &f.rates,
                stats: &f.stats,
            };
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
            Ok((Ok(body), frozen)) => Some(Batch { body: body.into(), scopes, frozen, sent: None }),
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
            f.dropped_high = f.high();
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
    #[serde(skip_serializing_if = "Option::is_none")]
    cursors: Option<std::collections::BTreeMap<&'a str, &'a str>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    config_version: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    config_epoch: Option<&'a str>,
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

/// What a config poll names the config held with. Schema 3 names its epoch
/// too, and its digest once that is verified; before, the version alone.
fn poll_headers(held: &Held, schema: i64) -> Vec<(&'static str, String)> {
    if schema < 3 {
        return vec![("if-none-match", format!("\"v{}\"", held.version))];
    }
    let tag = format!("\"{}-v{}\"", held.epoch, held.version);
    // An epoch that cannot travel in a header names nothing: the full
    // config comes back.
    if held.epoch.contains('"') || HeaderValue::try_from(tag.as_str()).is_err() {
        return Vec::new();
    }
    let mut headers = vec![("if-none-match", tag)];
    if let Some(digest) = &held.digest {
        headers.push((DIGEST_HEADER, digest.clone()));
    }
    headers
}

/// A config ready to serve, and what to hold of it.
struct Loaded {
    cfg: Config,
    held: Held,
    /// It came, in a schema 3 session, from a control plane that does not
    /// speak schema 3.
    older: bool,
}

/// What became of a config response.
enum Received {
    Config(Box<Loaded>),
    /// Not newer than the config held, which stays. `confirmed`: it is the
    /// one held, as far as can be told. `older`: as in `Loaded`.
    Kept {
        why: String,
        confirmed: bool,
        older: bool,
    },
    /// A delta that does not apply to the config held.
    Discarded(String),
    Rejected(String),
}

fn gunzip(gzip: bool, bytes: &[u8]) -> Result<Vec<u8>, String> {
    if !gzip {
        return Ok(bytes.to_vec());
    }
    let mut out = Vec::with_capacity(bytes.len() * 8);
    flate2::read::GzDecoder::new(bytes).read_to_end(&mut out).map_err(|e| format!("gunzip: {e}"))?;
    Ok(out)
}

/// Read a config response in a session of `schema`, against the config
/// `held`. `unconditional` says the poll named no config. Blocking.
fn receive(held: Option<Held>, gzip: bool, body: Bytes, schema: i64, unconditional: bool) -> Received {
    let raw = match gunzip(gzip, &body) {
        Ok(raw) => raw,
        Err(e) => return Received::Rejected(e),
    };
    if schema >= 3 && delta::is_delta(&raw) {
        return match patched(held, &raw) {
            Ok(loaded) => {
                metrics::CONFIG_DELTAS.fetch_add(1, Ordering::Relaxed);
                Received::Config(Box::new(loaded))
            }
            Err(e) => Received::Discarded(e),
        };
    }
    let wire: Wire = match serde_json::from_slice(&raw) {
        Ok(wire) => wire,
        Err(e) => return Received::Rejected(e.to_string()),
    };
    drop(raw);
    // Versions count within an epoch: a control plane that starts over
    // names a new one, and its config is taken whatever its number. Within
    // one, an older config is a replica lagging behind; the same version is
    // only taken when it was asked for by no name, to replace the one held.
    //
    // A control plane from before schema 3 names no epoch, and does not
    // understand the one it is polled with: it answers every poll at once
    // with its config. That is no new epoch. Its config is judged by version
    // alone: an older one is not taken (it would bring back what was since
    // removed), the same one confirms the config held.
    let older = schema >= 3 && (wire.schema < 3 || wire.epoch.is_empty());
    if schema >= 3
        && let Some(h) = &held
        && (older || wire.epoch == h.epoch)
        && (wire.version < h.version || (wire.version == h.version && (older || !unconditional)))
    {
        let why = format!(
            "the control plane sent version {} of epoch {:?}; version {} of epoch {:?} is in use",
            wire.version, wire.epoch, h.version, h.epoch
        );
        return Received::Kept { why, confirmed: older && wire.version == h.version, older };
    }
    let digest = verified(&wire);
    if schema >= 3 && digest.is_none() && !older {
        // The control plane is the authority on the config, so it is used;
        // but no delta is asked for on top of what cannot be told apart.
        log_warn!(
            "config version {}: its digest {:?} does not match its entries; using it, without deltas until one matches",
            wire.version,
            wire.digest
        );
    }
    match build(wire, digest, gzip, body) {
        Ok(loaded) => {
            metrics::CONFIG_FULL.fetch_add(1, Ordering::Relaxed);
            Received::Config(Box::new(Loaded { older, ..loaded }))
        }
        Err(e) => Received::Rejected(e),
    }
}

/// The digest of a full config, when it is the one the config names.
fn verified(wire: &Wire) -> Option<String> {
    if wire.digest.is_empty() {
        return None;
    }
    Some(delta::digest(wire)).filter(|computed| *computed == wire.digest)
}

/// The config `held` with a delta applied: only onto exactly the config the
/// delta was built on, and only if the result has the digest the delta
/// names. The patched config is kept as the control plane would have sent
/// it, to patch again and to save.
fn patched(held: Option<Held>, delta: &[u8]) -> Result<Loaded, String> {
    let held = held.ok_or("no config is held")?;
    let digest = held.digest.as_deref().ok_or("the config held has no verified digest")?;
    let full = gunzip(held.gzip, &held.body)?;
    let base = delta::Base { epoch: &held.epoch, version: held.version, digest };
    let next = delta::apply(&full, delta, &base)?;
    drop(full);
    let wire: Wire = serde_json::from_slice(&next).map_err(|e| format!("the patched config is unreadable: {e}"))?;
    let computed = delta::digest(&wire);
    if computed != wire.digest {
        return Err(format!("the patched config's digest is {computed}, not the {} it names", wire.digest));
    }
    let body = gzip(&next).map_err(|e| e.to_string())?;
    drop(next);
    build(wire, Some(computed), true, body.into())
}

/// Build the hot-path config from a full config, `body` being its bytes as
/// a config response carries them.
fn build(wire: Wire, digest: Option<String>, gzip: bool, body: Bytes) -> Result<Loaded, String> {
    let epoch = wire.epoch.clone();
    let generation = limits::begin_config();
    let cfg = Config::build(wire)?;
    limits::end_config(generation);
    let held = Held { epoch, version: cfg.version, group: cfg.group.id.clone(), digest, gzip, body };
    Ok(Loaded { cfg, held, older: false })
}

/// Read a full config as it was saved. Blocking.
fn load(gzip: bool, body: Bytes) -> Result<Loaded, String> {
    let wire: Wire = serde_json::from_slice(&gunzip(gzip, &body)?).map_err(|e| e.to_string())?;
    let digest = verified(&wire);
    build(wire, digest, gzip, body)
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
    /// A replay status: the connection is closed without an answer.
    const CUT_OFF: usize = 1;

    /// A full schema 3 config as the control plane sends it: epoch "e1", the
    /// keys of `wire_config`, each entry with its hash, and the digest.
    fn config3(version: i64) -> serde_json::Value {
        delta::tests::sealed(serde_json::json!({
            "schema": 3, "version": version, "epoch": "e1", "rest_hash": "r1",
            "group": {"id": "g1", "name": "group"},
            "keys": [
                {"hash": "11".repeat(32), "id": "cp-budgeted", "budget_usd": 1.0, "h": "k1"},
                {"hash": "22".repeat(32), "id": "cp-free", "h": "k2"},
            ],
        }))
    }

    /// A delta from `base` to `target`, changing only what `changes` says.
    fn delta3(base: &serde_json::Value, target: &serde_json::Value, changes: serde_json::Value) -> serde_json::Value {
        let mut d = serde_json::json!({"schema": 3, "delta": true, "epoch": target["epoch"],
            "base_version": base["version"], "base_digest": base["digest"],
            "version": target["version"], "digest": target["digest"]});
        d.as_object_mut().unwrap().extend(changes.as_object().unwrap().clone());
        d
    }

    /// What a mock control plane answers config polls with.
    #[derive(Default)]
    struct Configs {
        /// The config it has; None is `config3(3)`.
        current: Option<serde_json::Value>,
        /// Answers to the next polls, before `current` is looked at.
        next: std::collections::VecDeque<serde_json::Value>,
    }

    #[derive(Default)]
    struct Gwcp {
        mode: AtomicUsize,
        joins: AtomicUsize,
        /// A control plane from before schema 3: it refuses a join that
        /// names it, and speaks schema 2.
        older: AtomicBool,
        /// The schema of each join.
        join_schemas: Mutex<Vec<i64>>,
        configs: Mutex<Configs>,
        /// The If-None-Match of each config poll.
        polls: Mutex<Vec<Option<String>>>,
        /// The digest header of each config poll.
        digests: Mutex<Vec<Option<String>>>,
        /// The replica ids syncs are answered under, in turn; none names no replica.
        replicas: Mutex<Vec<&'static str>>,
        /// Every sync and replay in order: its path, the status answered, its JSON.
        calls: Mutex<Vec<(String, u16, serde_json::Value)>>,
        /// How many of the next syncs are answered 401, as an ended session is.
        expire: AtomicUsize,
        /// How many of the next syncs are answered 503.
        fail: AtomicUsize,
        /// What a replay is answered with; 0 is 200, and CUT_OFF no answer at all.
        replay_status: AtomicUsize,
    }

    impl Gwcp {
        /// Answer the next polls with `next`, in order, and have `current` from then on.
        fn serve(&self, next: Vec<serde_json::Value>, current: serde_json::Value) {
            let mut configs = self.configs.lock().unwrap();
            configs.next.extend(next);
            configs.current = Some(current);
        }

        /// Every sync's JSON, in order, with the status it was answered.
        fn syncs(&self) -> Vec<(u16, serde_json::Value)> {
            let calls = self.calls.lock().unwrap();
            calls.iter().filter(|(path, _, _)| path.ends_with("sync")).map(|(_, code, v)| (*code, v.clone())).collect()
        }

        /// The model names of the events in each call to `path` answered `status`.
        fn models(&self, path: &str, status: u16) -> Vec<Vec<String>> {
            let calls = self.calls.lock().unwrap();
            let of = |v: &serde_json::Value| {
                v["events"].as_array().unwrap().iter().map(|e| e["model_name"].as_str().unwrap().to_string()).collect()
            };
            calls.iter().filter(|(p, code, _)| p.ends_with(path) && *code == status).map(|(_, _, v)| of(v)).collect()
        }
    }

    /// A control plane with the whole gateway protocol: joins answered as
    /// `mode` says, sessions that end when it stops accepting, the schema 3
    /// config `config3(3)` unless told otherwise (`serve`), syncs that
    /// report no spend, and a replay that calls the first event of each
    /// request a duplicate. As an `older` one it speaks schema 2.
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
                                Ok::<_, std::io::Error>(resp.status(status).body(Full::new(Bytes::from(body))).unwrap())
                            };
                            let path = req.uri().path().to_string();
                            let header = |name: &str| req.headers().get(name).map(|v| v.to_str().unwrap().to_string());
                            let (tag, digest) = (header("if-none-match"), header(DIGEST_HEADER));
                            let older = cp.older.load(Ordering::Relaxed);
                            if path == "/gw/v1/join" {
                                cp.joins.fetch_add(1, Ordering::Relaxed);
                                let body = req.into_body().collect().await.unwrap().to_bytes();
                                let schema = serde_json::from_slice::<serde_json::Value>(&body).unwrap()["schema"]
                                    .as_i64()
                                    .unwrap();
                                cp.join_schemas.lock().unwrap().push(schema);
                                if mode == ACCEPT && older && schema > 2 {
                                    let why =
                                        "this gateway is newer than the control plane: upgrade llm-router first\n";
                                    return answer(resp, 400, why.as_bytes().to_vec());
                                }
                                return match mode {
                                    ACCEPT => answer(
                                        resp,
                                        200,
                                        serde_json::to_vec(&serde_json::json!({
                                            "instance_id": "i1", "session": "s", "sync_interval_ms": 20,
                                            "expires_at_ms": now_ms() + 900_000,
                                            "group": {"id": "g1", "name": "group"}, "schema": schema.min(SCHEMA),
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
                                    cp.polls.lock().unwrap().push(tag.clone());
                                    cp.digests.lock().unwrap().push(digest);
                                    if older {
                                        if tag.as_deref() == Some("\"v3\"") {
                                            tokio::time::sleep(Duration::from_millis(20)).await;
                                            return answer(resp, 304, Vec::new());
                                        }
                                        return answer(resp, 200, wire_config(3));
                                    }
                                    let body = {
                                        let mut configs = cp.configs.lock().unwrap();
                                        let current = configs.current.get_or_insert_with(|| config3(3)).clone();
                                        let name = format!(
                                            "\"{}-v{}\"",
                                            current["epoch"].as_str().unwrap(),
                                            current["version"]
                                        );
                                        match configs.next.pop_front() {
                                            Some(next) => Some(next),
                                            None if tag.as_deref() == Some(&name) => None,
                                            None => Some(current),
                                        }
                                    };
                                    let Some(body) = body else {
                                        tokio::time::sleep(Duration::from_millis(20)).await;
                                        return answer(resp, 304, Vec::new());
                                    };
                                    let json = serde_json::to_vec(&body).unwrap();
                                    answer(resp.header("content-encoding", "gzip"), 200, gzip(&json).unwrap())
                                }
                                "/gw/v1/sync" | "/gw/v1/replay" => {
                                    use std::io::Read;
                                    let gz = req.into_body().collect().await.unwrap().to_bytes();
                                    let mut json = Vec::new();
                                    flate2::read::GzDecoder::new(&gz[..]).read_to_end(&mut json).unwrap();
                                    let v: serde_json::Value = serde_json::from_slice(&json).unwrap();
                                    let n = v["events"].as_array().unwrap().len();
                                    let (status, body) = if path.ends_with("sync") {
                                        let expired = cp.expire.load(Ordering::Relaxed) > 0;
                                        if expired {
                                            cp.expire.fetch_sub(1, Ordering::Relaxed);
                                        }
                                        let failed = !expired && cp.fail.load(Ordering::Relaxed) > 0;
                                        if failed {
                                            cp.fail.fetch_sub(1, Ordering::Relaxed);
                                        }
                                        let status = if expired {
                                            401
                                        } else if failed {
                                            503
                                        } else {
                                            200
                                        };
                                        // Each replica numbers its own cursors.
                                        let replicas = cp.replicas.lock().unwrap();
                                        let body = if status != 200 || replicas.is_empty() {
                                            "{}".to_string()
                                        } else {
                                            let done = cp.calls.lock().unwrap();
                                            let done =
                                                done.iter().filter(|(p, code, _)| p.ends_with("sync") && *code == 200);
                                            let n = done.count();
                                            let replica = replicas[n % replicas.len()];
                                            format!(
                                                "{{\"replica\":\"{replica}\",\"cursor\":\"{replica}-{}\"}}",
                                                n / replicas.len() + 1
                                            )
                                        };
                                        (status, body)
                                    } else {
                                        match cp.replay_status.load(Ordering::Relaxed) {
                                            0 => (
                                                200,
                                                format!("{{\"stored\":{},\"duplicates\":1,\"dropped\":0}}", n - 1),
                                            ),
                                            status => (status as u16, "no".to_string()),
                                        }
                                    };
                                    cp.calls.lock().unwrap().push((path, status, v));
                                    if status as usize == CUT_OFF {
                                        return Err(std::io::Error::other("cut off"));
                                    }
                                    answer(resp, status, body.into_bytes())
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
            [None, Some("\"e1-v3\"".to_string())],
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

    /// Queue one request's event under `model`, with captured content.
    fn request(model: &str) {
        let mut em = emit_capturing(model, 1 << 20);
        em.capture_request(br#"{"messages":[{"role":"user","content":"a secret question"}]}"#, false);
        em.set_output(crate::translate::oai_json_text(
            br#"{"choices":[{"message":{"content":"a secret answer"}}]}"#,
            1 << 20,
        ));
        em.send(200, "", Default::default(), 0);
    }

    fn spool_files(dir: &TempDir) -> Vec<String> {
        let mut names: Vec<String> = std::fs::read_dir(dir.0.join("spool"))
            .unwrap()
            .map(|e| e.unwrap().file_name().into_string().unwrap())
            .collect();
        names.sort();
        names
    }

    /// A spool in `dir`, as a gateway started on it has.
    fn spool_in(dir: &TempDir, max_bytes: u64) -> Arc<Spool> {
        Arc::new(Spool::open(&dir.0.join("spool"), max_bytes).unwrap())
    }

    /// The spool of a gateway that died with `models` queued and sealed.
    async fn left_behind(dir: &TempDir, models: &[&[&str]]) {
        let spool = spool_in(dir, u64::MAX);
        for sealed_together in models {
            sealed_together.iter().for_each(|m| request(m));
            spool.seal().await;
        }
        drop(usage::take(usize::MAX, usize::MAX));
    }

    // Each sync puts what was queued since the last on disk, once, without
    // content; a settled batch takes with it exactly the files that end at
    // or before its last event.
    #[tokio::test]
    #[allow(clippy::await_holding_lock)] // one test at a time: see hold_globals
    async fn the_spool_holds_events_until_their_batch_is_settled() {
        let _globals = hold_globals();
        drop(usage::take(usize::MAX, usize::MAX));
        let dir = TempDir::new("spool");
        let spool = spool_in(&dir, u64::MAX);
        spool.seal().await;
        assert!(spool_files(&dir).is_empty(), "nothing queued, nothing written");

        ["a1", "a2", "a3"].iter().for_each(|m| request(m));
        spool.seal().await;
        spool.seal().await;
        ["b1", "b2"].iter().for_each(|m| request(m));
        spool.seal().await;
        let files = spool_files(&dir);
        assert_eq!(files.len(), 2, "one file per seal, no event twice: {files:?}");
        let on_disk: Vec<String> = files
            .iter()
            .flat_map(|f| spool::read(&dir.0.join("spool").join(f)).0)
            .map(|json| String::from_utf8(json).unwrap())
            .collect();
        assert_eq!(on_disk.len(), 5);
        assert!(
            on_disk.iter().all(|e| e.contains("\"content_truncated\":true") && !e.contains("secret")),
            "{on_disk:?}"
        );
        let size: u64 = files.iter().map(|f| std::fs::metadata(dir.0.join("spool").join(f)).unwrap().len()).sum();
        assert_eq!(metrics::SPOOL_BYTES.load(Ordering::Relaxed) as u64, size);

        let events = usage::take(usize::MAX, usize::MAX);
        let seqs: Vec<u64> = events.iter().map(|e| e.seq).collect();
        spool.settle(seqs[1]).await;
        assert_eq!(spool_files(&dir), files, "a file with an unsettled event stays");
        spool.settle(seqs[3]).await;
        assert_eq!(spool_files(&dir), files[1..], "the first file is settled, the second is not");
        spool.settle(seqs[4]).await;
        assert!(spool_files(&dir).is_empty());
        assert_eq!(metrics::SPOOL_BYTES.load(Ordering::Relaxed), 0);
    }

    #[tokio::test]
    #[allow(clippy::await_holding_lock)] // one test at a time: see hold_globals
    async fn a_full_spool_skips_events_until_settled_files_make_room() {
        let _globals = hold_globals();
        drop(usage::take(usize::MAX, usize::MAX));
        let dir = TempDir::new("spool-cap");
        request("sizing");
        let probe = spool_in(&dir, u64::MAX);
        probe.seal().await;
        let one = metrics::SPOOL_BYTES.load(Ordering::Relaxed) as u64;
        let first = usage::take(usize::MAX, usize::MAX)[0].seq;
        probe.settle(first).await;
        drop(probe);

        // room for one event's file, not two
        let spool = spool_in(&dir, one + one / 2);
        let skipped = metrics::SPOOL_SKIPPED_EVENTS.load(Ordering::Relaxed);
        request("fits-1");
        spool.seal().await;
        ["over-1", "over-2"].iter().for_each(|m| request(m));
        spool.seal().await;
        assert_eq!(spool_files(&dir).len(), 1, "the unsettled file is kept, the new one is not written");
        assert_eq!(metrics::SPOOL_SKIPPED_EVENTS.load(Ordering::Relaxed) - skipped, 2);
        spool.seal().await;
        assert_eq!(metrics::SPOOL_SKIPPED_EVENTS.load(Ordering::Relaxed) - skipped, 2, "counted once");

        let seqs: Vec<u64> = usage::take(usize::MAX, usize::MAX).iter().map(|e| e.seq).collect();
        spool.settle(seqs[0]).await;
        request("fits-2");
        spool.seal().await;
        let files = spool_files(&dir);
        assert_eq!(files.len(), 1);
        let (events, _) = spool::read(&dir.0.join("spool").join(&files[0]));
        assert!(String::from_utf8(events[0].clone()).unwrap().contains("fits-2"));
        drop(usage::take(usize::MAX, usize::MAX));
    }

    // A gateway that died with usage queued hands it over when it starts
    // again, before its first sync, and numbers its own events after it.
    #[tokio::test]
    #[allow(clippy::await_holding_lock)] // one test at a time: see hold_globals
    async fn a_restart_replays_what_the_last_run_left_before_it_syncs() {
        let _globals = hold_globals();
        drop(usage::take(usize::MAX, usize::MAX));
        let dir = TempDir::new("spool-replay");
        left_behind(&dir, &[&["left-1", "left-2"], &["left-3"]]).await;
        // the newest file lost its tail when the process died
        let files = spool_files(&dir);
        let newest = dir.0.join("spool").join(&files[1]);
        let mut bytes = std::fs::read(&newest).unwrap();
        bytes.extend_from_slice(&[200, 0, 0, 0, 1, 2, 3, 4, b'{']);
        std::fs::write(&newest, bytes).unwrap();
        std::fs::write(dir.0.join("spool").join("7-9.tmp"), b"unfinished").unwrap();

        let (url, mock) = gateway_cp().await;
        let spool = spool_in(&dir, u64::MAX);
        assert_eq!(spool_files(&dir), files, "an unfinished write is cleared away");
        let (replayed, duplicates) = (
            metrics::REPLAYED_EVENTS.load(Ordering::Relaxed),
            metrics::REPLAY_DUPLICATE_EVENTS.load(Ordering::Relaxed),
        );
        request("new-1");
        let cp = ControlPlane::new(&url, "llmrg_test".into(), "test".into()).with_spool(Some(spool));
        let st = new_state();
        let running = tokio::spawn(Arc::new(cp).start(st));
        until("the new event is synced", || mock.models("sync", 200).iter().any(|m| m == &["new-1"])).await;

        let calls: Vec<String> = mock.calls.lock().unwrap().iter().map(|(path, _, _)| path.clone()).collect();
        assert_eq!(calls[0], "/gw/v1/replay", "before the first sync: {calls:?}");
        assert_eq!(mock.models("replay", 200), [["left-1", "left-2", "left-3"]], "once, oldest first");
        let sent = mock.calls.lock().unwrap()[0].2.to_string();
        assert!(sent.contains("\"content_truncated\":true") && !sent.contains("secret"), "{sent}");
        assert_eq!(metrics::REPLAYED_EVENTS.load(Ordering::Relaxed) - replayed, 3);
        assert_eq!(metrics::REPLAY_DUPLICATE_EVENTS.load(Ordering::Relaxed) - duplicates, 1, "as the answer said");
        until("the spool is empty", || spool_files(&dir).is_empty()).await;
        assert_eq!(metrics::SPOOL_BYTES.load(Ordering::Relaxed), 0);

        running.abort();
        let _ = running.await;
    }

    // A control plane from before the replay answers 404: the files wait
    // for a later start, and syncing goes on.
    #[tokio::test]
    #[allow(clippy::await_holding_lock)] // one test at a time: see hold_globals
    async fn a_control_plane_without_replay_leaves_the_spool_alone() {
        let _globals = hold_globals();
        drop(usage::take(usize::MAX, usize::MAX));
        let dir = TempDir::new("spool-404");
        left_behind(&dir, &[&["left-1"]]).await;
        let files = spool_files(&dir);
        let (url, mock) = gateway_cp().await;
        mock.replay_status.store(404, Ordering::Relaxed);
        let cp = ControlPlane::new(&url, "llmrg_test".into(), "test".into())
            .with_spool(Some(spool_in(&dir, u64::MAX)))
            .replaying_with(Duration::from_millis(200), Duration::from_millis(10));
        let st = new_state();
        let running = tokio::spawn(Arc::new(cp).start(st));
        request("new-1");
        until("the new event is synced", || mock.models("sync", 200).iter().any(|m| m == &["new-1"])).await;
        until("its own file is settled", || spool_files(&dir).len() == 1).await;
        assert_eq!(spool_files(&dir), files, "the leftover file is kept");
        tokio::time::sleep(Duration::from_millis(100)).await;
        assert_eq!(mock.models("replay", 404).len(), 3, "one 404 may be a proxy's; three in a row are not");
        assert!(st.is_ready());
        running.abort();
        let _ = running.await;

        // the next start, against a control plane that has it, hands it over
        mock.replay_status.store(0, Ordering::Relaxed);
        let cp = ControlPlane::new(&url, "llmrg_test".into(), "test".into()).with_spool(Some(spool_in(&dir, u64::MAX)));
        let running = tokio::spawn(Arc::new(cp).start(new_state()));
        until("the leftover is replayed", || mock.models("replay", 200) == [["left-1"]]).await;
        until("the spool is empty", || spool_files(&dir).is_empty()).await;
        running.abort();
        let _ = running.await;
    }

    // A batch that was sent, then the session ended: the control plane may
    // have stored it for the instance this gateway was. After the new join
    // its events go through the replay, with their content, not as a sync.
    #[tokio::test]
    #[allow(clippy::await_holding_lock)] // one test at a time: see hold_globals
    async fn a_batch_pending_across_a_rejoin_is_replayed() {
        let _globals = hold_globals();
        drop(usage::take(usize::MAX, usize::MAX));
        let dir = TempDir::new("spool-rejoin");
        let (url, mock) = gateway_cp().await;
        let cp = ControlPlane::new(&url, "llmrg_test".into(), "test".into()).with_spool(Some(spool_in(&dir, u64::MAX)));
        cp.join().await.unwrap();
        let mut s = cp.syncer.lock().await;

        request("pending-1");
        mock.expire.store(1, Ordering::Relaxed);
        assert!(!cp.sync_once(&mut s).await, "401, and it joins again");
        assert_eq!(mock.joins.load(Ordering::Relaxed), 2);
        assert_eq!(spool_files(&dir).len(), 1);
        request("later-1");
        assert!(!cp.sync_once(&mut s).await, "the pending batch is replayed");
        assert!(s.pending.is_none());
        assert_eq!(mock.models("replay", 200), [["pending-1"]]);
        assert!(mock.calls.lock().unwrap().last().unwrap().2.to_string().contains("a secret answer"));
        assert_eq!(spool_files(&dir).len(), 1, "its file is settled; the later event's stays");
        assert!(cp.sync_once(&mut s).await);
        assert_eq!(mock.models("sync", 200), [["later-1"]], "the batch itself is never sent as a sync again");
        assert!(spool_files(&dir).is_empty());

        // This control plane has answered a replay, so a 404 is a proxy's
        // (a rollout, say): the batch waits for the replay, however long,
        // and is never resent as a sync, which could count it twice.
        mock.replay_status.store(404, Ordering::Relaxed);
        request("pending-2");
        mock.expire.store(1, Ordering::Relaxed);
        for _ in 0..6 {
            assert!(!cp.sync_once(&mut s).await);
        }
        assert!(s.pending.is_some());
        assert_eq!(mock.models("replay", 404).len(), 5);
        assert_eq!(mock.models("sync", 200), [["later-1"]]);
        mock.replay_status.store(0, Ordering::Relaxed);
        assert!(!cp.sync_once(&mut s).await);
        assert_eq!(mock.models("replay", 200).last().unwrap(), &["pending-2"]);
        assert!(s.pending.is_none() && spool_files(&dir).is_empty());

        // a replay the control plane cannot read drops the batch, so it cannot wedge the gateway
        mock.replay_status.store(400, Ordering::Relaxed);
        let refused = metrics::REPLAY_REFUSED_EVENTS.load(Ordering::Relaxed);
        request("pending-3");
        mock.expire.store(1, Ordering::Relaxed);
        assert!(!cp.sync_once(&mut s).await);
        assert!(!cp.sync_once(&mut s).await);
        assert!(s.pending.is_none() && spool_files(&dir).is_empty());
        assert_eq!(metrics::REPLAY_REFUSED_EVENTS.load(Ordering::Relaxed) - refused, 1);
    }

    // A proxy's 413s make a batch smaller: stripped, halved, and an event too
    // large alone dropped. Whatever is still to send stays in sequence, and
    // each file goes once everything up to its last event is sent or dropped.
    #[tokio::test]
    #[allow(clippy::await_holding_lock)] // one test at a time: see hold_globals
    async fn batches_made_smaller_stay_in_sequence_and_settle_the_spool() {
        let _globals = hold_globals();
        drop(usage::take(usize::MAX, usize::MAX));
        let dir = TempDir::new("spool-413");
        let (url, seen) = mock_cp(|_, size| if size > 3000 { 413 } else { 200 }).await;
        let cp = ControlPlane::new(&url, "llmrg_test".into(), "test".into()).with_spool(Some(spool_in(&dir, u64::MAX)));
        let mut s = cp.syncer.lock().await;
        s.limit_bytes = Some(MIN_BATCH_BYTES);
        for model in ["one", "two", &"too-large-".repeat(500), "four", "five"] {
            request(model);
        }
        let mut files = usize::MAX;
        for _ in 0..30 {
            let synced = cp.sync_once(&mut s).await;
            let pending = s.pending.iter().flat_map(|b| &b.frozen.events).map(|e| e.seq);
            let seqs: Vec<u64> = pending.chain(crate::usage::tests::queued_seqs()).collect();
            assert!(seqs.windows(2).all(|w| w[0] < w[1]), "{seqs:?}");
            assert!(spool_files(&dir).len() <= files, "files only go");
            files = spool_files(&dir).len();
            if synced && usage::queued() == 0 {
                break;
            }
        }
        assert!(s.pending.is_none() && usage::queued() == 0);
        assert!(spool_files(&dir).is_empty(), "{:?}", spool_files(&dir));
        let accepted: Vec<String> = seen
            .lock()
            .unwrap()
            .iter()
            .filter(|(_, _, code)| *code == 200)
            .flat_map(|(_, v, _)| v["events"].as_array().unwrap().clone())
            .map(|e| e["model_name"].as_str().unwrap().to_string())
            .collect();
        assert_eq!(accepted, ["one", "two", "four", "five"]);
    }

    // While the control plane cannot take the replay, what an earlier run
    // left stays on disk and normal syncs go on; once it can, the events are
    // handed over once and the files go.
    #[tokio::test]
    #[allow(clippy::await_holding_lock)] // one test at a time: see hold_globals
    async fn a_failing_replay_keeps_the_files_and_does_not_hold_up_syncs() {
        let _globals = hold_globals();
        drop(usage::take(usize::MAX, usize::MAX));
        let dir = TempDir::new("spool-retry");
        left_behind(&dir, &[&["left-1", "left-2"], &["left-3"]]).await;
        let files = spool_files(&dir);
        let (url, mock) = gateway_cp().await;
        mock.replay_status.store(503, Ordering::Relaxed);
        let cp = ControlPlane::new(&url, "llmrg_test".into(), "test".into())
            .with_spool(Some(spool_in(&dir, u64::MAX)))
            .replaying_with(Duration::from_millis(100), Duration::from_millis(10));
        let st = new_state();
        let running = tokio::spawn(Arc::new(cp).start(st));
        request("new-1");
        until("the new event is synced", || mock.models("sync", 200).iter().any(|m| m == &["new-1"])).await;
        until("its own file is settled", || spool_files(&dir).len() == files.len()).await;
        assert!(st.is_ready());
        assert_eq!(mock.calls.lock().unwrap()[0].0, "/gw/v1/replay", "the replay had its head start");
        assert!(mock.models("replay", 503).len() >= 2, "and is retried");

        // no answer at all
        mock.replay_status.store(CUT_OFF, Ordering::Relaxed);
        until("a replay is cut off", || !mock.models("replay", CUT_OFF as u16).is_empty()).await;
        request("new-2");
        until("syncs go on", || mock.models("sync", 200).iter().any(|m| m == &["new-2"])).await;
        until("its own file is settled", || spool_files(&dir).len() == files.len()).await;
        assert_eq!(spool_files(&dir), files, "the leftover files wait");
        assert!(mock.models("replay", 200).is_empty());

        mock.replay_status.store(0, Ordering::Relaxed);
        until("the spool is empty", || spool_files(&dir).is_empty()).await;
        tokio::time::sleep(Duration::from_millis(150)).await;
        assert_eq!(mock.models("replay", 200), [["left-1", "left-2", "left-3"]], "handed over once");
        let synced: Vec<String> = mock.models("sync", 200).concat();
        assert_eq!(synced, ["new-1", "new-2"], "and never as a sync");

        running.abort();
        let _ = running.await;
    }

    // The replay of a batch pending across a rejoin needs no state
    // directory; without one nothing is written. A control plane that has
    // never answered a replay and says 404 three times running has none:
    // only then is the batch resent as a sync.
    #[tokio::test]
    #[allow(clippy::await_holding_lock)] // one test at a time: see hold_globals
    async fn without_a_spool_nothing_is_written_and_a_rejoin_still_replays() {
        let _globals = hold_globals();
        drop(usage::take(usize::MAX, usize::MAX));
        let (url, mock) = gateway_cp().await;
        let cp = ControlPlane::new(&url, "llmrg_test".into(), "test".into());
        cp.join().await.unwrap();
        let mut s = cp.syncer.lock().await;
        let bytes = metrics::SPOOL_BYTES.load(Ordering::Relaxed);

        mock.replay_status.store(404, Ordering::Relaxed);
        request("plain-1");
        mock.expire.store(1, Ordering::Relaxed);
        assert!(!cp.sync_once(&mut s).await, "401, and it joins again");
        assert!(!cp.sync_once(&mut s).await, "404: it may be a proxy's");
        assert!(!cp.sync_once(&mut s).await, "404 again");
        assert!(mock.models("sync", 200).is_empty() && s.pending.is_some());
        assert!(cp.sync_once(&mut s).await, "the third 404: resent as a sync");
        assert_eq!(mock.models("replay", 404).len(), 3);
        assert_eq!(mock.models("sync", 200), [["plain-1"]]);

        // one 404 between answers does not count towards the three
        request("plain-2");
        mock.expire.store(1, Ordering::Relaxed);
        assert!(!cp.sync_once(&mut s).await);
        assert!(!cp.sync_once(&mut s).await, "404");
        mock.replay_status.store(503, Ordering::Relaxed);
        assert!(!cp.sync_once(&mut s).await, "503");
        mock.replay_status.store(404, Ordering::Relaxed);
        assert!(!cp.sync_once(&mut s).await, "404");
        assert!(!cp.sync_once(&mut s).await, "404");
        assert!(s.pending.is_some());
        mock.replay_status.store(0, Ordering::Relaxed);
        assert!(!cp.sync_once(&mut s).await, "replayed");
        assert!(s.pending.is_none());
        assert_eq!(mock.models("replay", 200), [["plain-2"]]);
        assert_eq!(mock.models("sync", 200), [["plain-1"]]);
        assert_eq!(metrics::SPOOL_BYTES.load(Ordering::Relaxed), bytes);
    }

    // A renewed session is the same instance to the control plane, which
    // still knows the pending batch's number: the batch is resent as the
    // sync it is, rate counts and all. Only a new join sends it to the replay.
    #[tokio::test]
    #[allow(clippy::await_holding_lock)] // one test at a time: see hold_globals
    async fn a_renewed_session_resends_the_pending_batch_as_a_sync() {
        let _globals = hold_globals();
        drop(usage::take(usize::MAX, usize::MAX));
        drop(limits::freeze());
        let (url, mock) = gateway_cp().await;
        let cp = ControlPlane::new(&url, "llmrg_test".into(), "test".into());
        cp.join().await.unwrap();
        let mut s = cp.syncer.lock().await;
        let limited = crate::config::Key {
            id: "renew-test".into(),
            user_id: String::new(),
            team_id: String::new(),
            user: None,
            team: None,
            allowed_models: None,
            expires_at: 0,
            scope: limits::scope("key:renew-test".into(), None, Some(1000), None),
            tags: Vec::new(),
        };
        let renew = |cp: &ControlPlane| {
            let generation = cp.generation.load(Ordering::Relaxed);
            cp.adopt(&Joined {
                instance_id: String::new(),
                session: "s".into(),
                expires_at_ms: now_ms() + 900_000,
                sync_interval_ms: 0,
                group: None,
                schema: 0,
            });
            assert_eq!(cp.generation.load(Ordering::Relaxed), generation + 1);
        };

        request("renewed-1");
        assert!(limits::check(&limited).is_ok());
        mock.fail.store(1, Ordering::Relaxed);
        assert!(!cp.sync_once(&mut s).await, "503: the batch stays pending");
        renew(&cp);
        assert!(cp.sync_once(&mut s).await);
        assert!(mock.models("replay", 200).is_empty(), "a renewal is no reason to replay");
        {
            let calls = mock.calls.lock().unwrap();
            let syncs: Vec<_> = calls.iter().filter(|(path, _, _)| path.ends_with("sync")).collect();
            assert_eq!(syncs.iter().map(|(_, code, _)| *code).collect::<Vec<_>>(), [503, 200]);
            assert_eq!(syncs[0].2, syncs[1].2, "the same batch, resent as it was");
            let rates = syncs[1].2["rates"].as_array().unwrap();
            assert!(rates.iter().any(|r| r["scope"] == "key:renew-test" && r["requests"] == 1), "{rates:?}");
        }

        // the session ends and the gateway joins again: now it is replayed
        request("rejoined-1");
        mock.expire.store(1, Ordering::Relaxed);
        assert!(!cp.sync_once(&mut s).await, "401, and it joins again");
        renew(&cp);
        assert!(!cp.sync_once(&mut s).await);
        assert_eq!(mock.models("replay", 200), [["rejoined-1"]]);
        assert_eq!(mock.models("sync", 200), [["renewed-1"]]);
    }

    // An event queued while the seal is being written is not on disk: it
    // waits for the next sync, so nothing is ever sent that a crash in the
    // meantime would lose.
    #[tokio::test]
    #[allow(clippy::await_holding_lock)] // one test at a time: see hold_globals
    async fn only_events_on_disk_are_sent() {
        let _globals = hold_globals();
        drop(usage::take(usize::MAX, usize::MAX));
        let dir = TempDir::new("spool-gap");
        let (url, _) = mock_cp(|_, _| 503).await;
        let spool = spool_in(&dir, u64::MAX);
        let cp = ControlPlane::new(&url, "llmrg_test".into(), "test".into()).with_spool(Some(spool.clone()));
        let mut s = cp.syncer.lock().await;
        // requests finishing all the while, also during the seal's write
        let stop = Arc::new(AtomicBool::new(false));
        let stopped = stop.clone();
        let traffic = tokio::spawn(async move {
            let mut n = 0;
            while !stopped.load(Ordering::Relaxed) {
                request(&format!("during-{n}"));
                n += 1;
                tokio::task::yield_now().await;
            }
        });
        let on_disk = || -> Vec<String> {
            spool_files(&dir)
                .iter()
                .flat_map(|f| spool::read(&dir.0.join("spool").join(f)).0)
                .map(|json| {
                    serde_json::from_slice::<serde_json::Value>(&json).unwrap()["model_name"].as_str().unwrap().into()
                })
                .collect()
        };
        let mut left_for_later = 0;
        for _ in 0..5 {
            tokio::task::yield_now().await;
            assert!(!cp.sync_once(&mut s).await, "503: the batch is sent and stays pending");
            let batch = s.pending.take().unwrap();
            let saved = on_disk();
            assert!(!batch.frozen.events.is_empty());
            for e in &batch.frozen.events {
                assert!(
                    e.seq <= spool.sealed() && saved.contains(&e.model_name),
                    "{} was sent but is not on disk",
                    e.model_name
                );
            }
            left_for_later += usage::queued();
            limits::discard(&batch.scopes);
        }
        assert!(left_for_later > 0, "events did arrive during the seal");
        stop.store(true, Ordering::Relaxed);
        let _ = traffic.await;
        drop(usage::take(usize::MAX, usize::MAX));
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

    /// A config response as it arrives: gzipped JSON.
    fn body(v: &serde_json::Value) -> Bytes {
        gzip(&serde_json::to_vec(v).unwrap()).unwrap().into()
    }

    /// `receive` in a schema 3 session, a config expected.
    fn received(held: Option<&Held>, v: &serde_json::Value, unconditional: bool) -> Loaded {
        match receive(held.cloned(), true, body(v), 3, unconditional) {
            Received::Config(loaded) => *loaded,
            Received::Kept { why, .. } => panic!("ignored: {why}"),
            Received::Discarded(why) => panic!("discarded: {why}"),
            Received::Rejected(why) => panic!("rejected: {why}"),
        }
    }

    fn key_ids(cfg: &Config) -> Vec<String> {
        let mut ids: Vec<String> = cfg.keys.values().map(|k| k.id.clone()).collect();
        ids.sort();
        ids
    }

    fn header_names(held: &Held) -> Vec<(&'static str, String)> {
        poll_headers(held, 3)
    }

    // A full config is held with its epoch, version and verified digest; a
    // delta on exactly that config patches it, and the patched config is
    // held as a full one: patched again, and read back as it would be from
    // the state directory.
    #[test]
    fn a_delta_patches_the_config_held() {
        let _globals = hold_globals();
        let v3 = config3(3);
        let Loaded { cfg, held, .. } = received(None, &v3, true);
        assert_eq!((cfg.version, key_ids(&cfg)), (3, vec!["cp-budgeted".to_string(), "cp-free".to_string()]));
        assert_eq!((held.epoch.as_str(), held.version, held.group.as_str()), ("e1", 3, "g1"));
        assert_eq!(held.digest.as_deref(), v3["digest"].as_str());
        assert_eq!(
            header_names(&held),
            [("if-none-match", "\"e1-v3\"".to_string()), (DIGEST_HEADER, v3["digest"].as_str().unwrap().to_string())]
        );

        // version 5: one key gone, one changed, one new; a user, a team and an email appear
        let mut v5 = v3.clone();
        v5["version"] = 5.into();
        v5["keys"] = serde_json::json!([
            {"hash": "22".repeat(32), "id": "cp-free", "rpm_limit": 7, "h": "k2b"},
            {"hash": "33".repeat(32), "id": "cp-new", "user_id": "u1", "team_id": "t1", "h": "k3"},
        ]);
        v5["users"] = serde_json::json!([{"id": "u1", "h": "u1a"}]);
        v5["teams"] = serde_json::json!([{"id": "t1", "h": "t1a"}]);
        v5["emails"] = serde_json::json!({"ee".repeat(32): "u1"});
        let v5 = delta::tests::sealed(v5);
        let changes = serde_json::json!({
            "keys": {"upsert": v5["keys"], "remove": ["11".repeat(32)]},
            "users": {"upsert": v5["users"]}, "teams": {"upsert": v5["teams"]},
            "emails": {"upsert": v5["emails"]},
        });
        let Loaded { cfg, held, .. } = received(Some(&held), &delta3(&v3, &v5, changes), false);
        assert_eq!((cfg.version, key_ids(&cfg)), (5, vec!["cp-free".to_string(), "cp-new".to_string()]));
        let new = &cfg.keys[&[0x33; 32]];
        assert!(new.user.is_some() && new.team.is_some() && cfg.emails.contains_key(&[0xee; 32]));
        assert!(cfg.keys[&[0x22; 32]].scope.is_some(), "the changed key has its limit");
        assert_eq!((held.epoch.as_str(), held.version, held.digest.as_deref()), ("e1", 5, v5["digest"].as_str()));

        // version 6, on the patched config: everything else replaced, the user, team and email removed
        let mut v6 = v5.clone();
        v6["version"] = 6.into();
        v6["keys"] = serde_json::json!([{"hash": "22".repeat(32), "id": "cp-free", "rpm_limit": 7, "h": "k2b"}]);
        (v6["users"], v6["teams"], v6["emails"]) =
            (serde_json::json!([]), serde_json::json!([]), serde_json::json!({}));
        v6["rest_hash"] = "r2".into();
        v6["group"] = serde_json::json!({"id": "g1", "name": "renamed"});
        v6["providers"] =
            serde_json::json!([{"id": "p1", "name": "p", "type": "openai_compatible", "base_url": "http://p"}]);
        v6["deployments"] =
            serde_json::json!([{"id": "delta-d1", "provider_id": "p1", "model_name": "m1", "upstream_name": "up"}]);
        let v6 = delta::tests::sealed(v6);
        let changes = serde_json::json!({
            "keys": {"remove": ["33".repeat(32)]},
            "users": {"remove": ["u1"]}, "teams": {"remove": ["t1"]}, "emails": {"remove": ["ee".repeat(32)]},
            "rest": {"group": v6["group"], "providers": v6["providers"], "deployments": v6["deployments"], "rest_hash": "r2"},
        });
        let Loaded { cfg, held, .. } = received(Some(&held), &delta3(&v5, &v6, changes), false);
        assert_eq!((cfg.version, key_ids(&cfg)), (6, vec!["cp-free".to_string()]));
        assert!(cfg.emails.is_empty() && cfg.teams.is_empty());
        assert_eq!((cfg.group.name.as_str(), cfg.names.as_slice()), ("renamed", &["m1".to_string()][..]));
        assert_eq!(held.digest.as_deref(), v6["digest"].as_str());

        // what is held is a full config: saved and loaded, it is the same one
        let Loaded { cfg, held: reloaded, .. } = load(held.gzip, held.body.clone()).unwrap();
        assert_eq!((cfg.version, key_ids(&cfg), cfg.names.len()), (6, vec!["cp-free".to_string()], 1));
        assert_eq!(
            (reloaded.epoch, reloaded.version, reloaded.digest, reloaded.group),
            (held.epoch.clone(), 6, held.digest.clone(), "g1".to_string())
        );
        let kept: serde_json::Value = serde_json::from_slice(&gunzip(true, &held.body).unwrap()).unwrap();
        assert_eq!(kept["keys"][0]["h"], "k2b", "entry hashes stay with their entries");
        assert_eq!(
            (&kept["digest"], &kept["rest_hash"], &kept["epoch"]),
            (&v6["digest"], &v6["rest_hash"], &v6["epoch"])
        );
    }

    // A delta is applied only onto exactly the config it was built on, and
    // only when the result has the digest it names; anything else is
    // discarded for the full config, and the config in use stays.
    #[test]
    fn a_delta_that_does_not_fit_is_discarded() {
        let _globals = hold_globals();
        let v3 = config3(3);
        let Loaded { held, .. } = received(None, &v3, true);
        let mut v4 = v3.clone();
        v4["version"] = 4.into();
        v4["keys"].as_array_mut().unwrap().pop();
        let v4 = delta::tests::sealed(v4);
        let good = delta3(&v3, &v4, serde_json::json!({"keys": {"remove": ["22".repeat(32)]}}));
        let discarded =
            |held: Option<&Held>, d: &serde_json::Value| match receive(held.cloned(), true, body(d), 3, false) {
                Received::Discarded(why) => why,
                _ => panic!("not discarded: {d}"),
            };
        // the result is not what the control plane says it should be
        let mut wrong = good.clone();
        wrong["keys"] = serde_json::json!({"remove": []});
        assert!(discarded(Some(&held), &wrong).contains("digest"));
        let mut wrong = good.clone();
        wrong["digest"] = "00".repeat(32).into();
        assert!(discarded(Some(&held), &wrong).contains("digest"));
        // built on another version, another digest, another epoch
        for (field, value) in [("base_version", serde_json::json!(2)), ("base_digest", "ab".repeat(32).into())] {
            let mut wrong = good.clone();
            wrong[field] = value;
            assert!(discarded(Some(&held), &wrong).contains("built on"), "{field}");
        }
        let mut wrong = good.clone();
        wrong["epoch"] = "e2".into();
        assert!(discarded(Some(&held), &wrong).contains("epoch"));
        // unreadable, and with nothing, or nothing verified, to apply it to
        assert!(discarded(Some(&held), &serde_json::json!({"delta": true, "keys": 7})).contains("unreadable"));
        assert!(discarded(None, &good).contains("no config"));
        let unverified = Held { digest: None, ..held.clone() };
        assert!(discarded(Some(&unverified), &good).contains("no verified digest"));
        // and the same delta, untouched, applies
        assert_eq!(received(Some(&held), &good, false).cfg.version, 4);
        // before schema 3 there are no deltas: one is not a config
        assert!(matches!(receive(Some(held), true, body(&good), 2, false), Received::Rejected(_)));
    }

    // Versions count within an epoch. Another epoch's config is taken
    // whatever its number; the same epoch's only when newer, or when the
    // gateway asked for whatever the control plane has.
    #[test]
    fn a_config_is_taken_by_epoch_then_version() {
        let _globals = hold_globals();
        let Loaded { held, .. } = received(None, &config3(7), true);
        let ignored = |v: &serde_json::Value, unconditional| {
            matches!(
                receive(Some(held.clone()), true, body(v), 3, unconditional),
                Received::Kept { confirmed: false, older: false, .. }
            )
        };
        assert!(ignored(&config3(6), false), "older, the same epoch");
        assert!(ignored(&config3(6), true), "older, even when asked for by no name");
        assert!(ignored(&config3(7), false), "the same version");
        assert_eq!(received(Some(&held), &config3(7), true).cfg.version, 7, "the same version, asked for by no name");
        assert_eq!(received(Some(&held), &config3(8), false).cfg.version, 8);

        // a control plane that started over: a new epoch, a lower version
        let mut restored = config3(2);
        restored["epoch"] = "e2".into();
        let Loaded { cfg, held: now, .. } = received(Some(&held), &restored, false);
        assert_eq!((cfg.version, now.epoch.as_str(), now.version), (2, "e2", 2));
        assert_eq!(header_names(&now)[0].1, "\"e2-v2\"");

        // schema 2 sessions take what they are sent, as before
        assert!(matches!(receive(Some(held.clone()), false, wire_config(3).into(), 2, false), Received::Config(_)));
        assert_eq!(poll_headers(&held, 2), [("if-none-match", "\"v7\"".to_string())]);
    }

    // A full config whose digest does not match its entries is used: the
    // control plane decides what the config is. But it is named without a
    // digest, so no delta is built on it, until a config verifies.
    #[test]
    fn a_full_config_with_a_wrong_digest_is_used_without_deltas() {
        let _globals = hold_globals();
        let mut wrong = config3(3);
        wrong["digest"] = "00".repeat(32).into();
        let Loaded { cfg, held, .. } = received(None, &wrong, true);
        assert_eq!(cfg.version, 3);
        assert_eq!(held.digest, None);
        assert_eq!(header_names(&held), [("if-none-match", "\"e1-v3\"".to_string())]);
        // none at all: a schema 2 config
        let Loaded { held, .. } = load(false, wire_config(3).into()).unwrap();
        assert_eq!((held.digest.as_deref(), held.epoch.as_str()), (None, ""));
        // the next that verifies is named in full again
        let Loaded { held, .. } = received(Some(&held), &config3(4), false);
        assert_eq!(header_names(&held).len(), 2);
        // an epoch that cannot be a header value names nothing
        assert!(poll_headers(&Held { epoch: "a\"b".into(), ..held.clone() }, 3).is_empty());
        assert!(poll_headers(&Held { epoch: "a\nb".into(), ..held }, 3).is_empty());
    }

    // A rotated key is two entries with one id while the old secret still
    // works: both authenticate as the key and count against its one limit,
    // and a delta that removes the old hash leaves the new one.
    #[test]
    fn a_rotated_keys_old_and_new_secret_are_one_key() {
        let _globals = hold_globals();
        let mut v3 = config3(3);
        v3["keys"] = serde_json::json!([
            {"hash": "aa".repeat(32), "id": "cp-rotated", "rpm_limit": 2, "h": "old"},
            {"hash": "bb".repeat(32), "id": "cp-rotated", "rpm_limit": 2, "h": "new"},
            {"hash": "22".repeat(32), "id": "cp-free", "h": "k2"},
        ]);
        let v3 = delta::tests::sealed(v3);
        let Loaded { cfg, held, .. } = received(None, &v3, true);
        assert_eq!(cfg.keys.len(), 3);
        let (old, new) = (&cfg.keys[&[0xaa; 32]], &cfg.keys[&[0xbb; 32]]);
        assert_eq!((old.id.as_str(), new.id.as_str()), ("cp-rotated", "cp-rotated"));
        assert!(Arc::ptr_eq(old.scope.as_ref().unwrap(), new.scope.as_ref().unwrap()), "one limit for both");
        assert!(limits::check(old).is_ok() && limits::check(new).is_ok());
        assert_eq!(limits::check(old).unwrap_err().code, "rate_limit_exceeded");
        assert_eq!(limits::check(new).unwrap_err().code, "rate_limit_exceeded");

        let mut v4 = v3.clone();
        v4["version"] = 4.into();
        v4["keys"].as_array_mut().unwrap().remove(0);
        let v4 = delta::tests::sealed(v4);
        let gone = delta3(&v3, &v4, serde_json::json!({"keys": {"remove": ["aa".repeat(32)]}}));
        let Loaded { cfg, held, .. } = received(Some(&held), &gone, false);
        assert!(!cfg.keys.contains_key(&[0xaa; 32]), "the old secret no longer works");
        assert_eq!(cfg.keys[&[0xbb; 32]].id, "cp-rotated");
        assert_eq!(cfg.keys.len(), 2);
        assert_eq!(held.digest.as_deref(), v4["digest"].as_str());
        drop(limits::freeze());
    }

    /// A gateway running against `url`, keeping its state in `dir`.
    fn running(url: &str, dir: Option<&TempDir>) -> (&'static State, tokio::task::JoinHandle<()>, Option<Arc<Store>>) {
        let store = dir.map(|dir| Arc::new(Store::open(&dir.0, "llmrg_test").unwrap()));
        let cp = ControlPlane::new(url, "llmrg_test".into(), "test".into()).with_state(store.clone());
        let st = new_state();
        (st, tokio::spawn(Arc::new(cp).start(st)), store)
    }

    fn version_of(st: &State) -> i64 {
        st.config.load().as_ref().map_or(0, |c| c.version)
    }

    // Over the wire: the gateway names the config it holds and its digest;
    // a delta whose result has another digest is discarded and the full
    // config fetched by a poll that names nothing; the next delta applies.
    #[tokio::test]
    #[allow(clippy::await_holding_lock)] // one test at a time: see hold_globals
    async fn a_digest_mismatch_fetches_the_full_config() {
        let _globals = hold_globals();
        let (url, mock) = gateway_cp().await;
        let (st, running, _) = running(&url, None);
        until("it has version 3", || version_of(st) == 3).await;
        until("it polls by name", || mock.polls.lock().unwrap().len() >= 2).await;

        let v3 = config3(3);
        let without = |v: &serde_json::Value, version: i64, hash: &str| {
            let mut next = v.clone();
            next["version"] = version.into();
            next["keys"].as_array_mut().unwrap().retain(|k| k["hash"] != hash.repeat(32));
            delta::tests::sealed(next)
        };
        let v4 = without(&v3, 4, "22");
        // a delta that leaves the key in: the result is not version 4's
        let mut bad = delta3(&v3, &v4, serde_json::json!({"keys": {"remove": []}}));
        bad["digest"] = v4["digest"].clone();
        let polls = mock.polls.lock().unwrap().len();
        mock.serve(vec![bad], v4.clone());
        until("it has version 4", || version_of(st) == 4).await;
        assert_eq!(key_ids(&st.config.load_full().unwrap()), ["cp-budgeted"]);
        {
            let (tags, digests) = (mock.polls.lock().unwrap(), mock.digests.lock().unwrap());
            let v3_name = (Some("\"e1-v3\"".to_string()), v3["digest"].as_str().map(str::to_string));
            assert_eq!((tags[0].clone(), digests[0].clone()), (None, None), "nothing held, nothing named");
            for i in 1..=polls {
                assert_eq!((tags[i].clone(), digests[i].clone()), v3_name, "poll {i}");
            }
            // the poll after the one the bad delta answered
            assert_eq!(
                (tags[polls + 1].clone(), digests[polls + 1].clone()),
                (None, None),
                "the full config, by no name"
            );
        }

        // and a good delta from there
        let v5 = without(&v4, 5, "11");
        mock.serve(vec![delta3(&v4, &v5, serde_json::json!({"keys": {"remove": ["11".repeat(32)]}}))], v5.clone());
        until("it has version 5", || version_of(st) == 5).await;
        assert!(st.config.load_full().unwrap().keys.is_empty());
        let asked = mock.polls.lock().unwrap().len();
        until("it polls again", || mock.polls.lock().unwrap().len() > asked).await;
        assert_eq!(mock.polls.lock().unwrap().last().unwrap().as_deref(), Some("\"e1-v5\""));
        assert_eq!(mock.digests.lock().unwrap().last().unwrap().as_deref(), v5["digest"].as_str());
        assert_eq!(mock.polls.lock().unwrap().iter().filter(|tag| tag.is_none()).count(), 2);

        running.abort();
        let _ = running.await;
    }

    // What a delta made of the config is what the state directory holds: a
    // restart serves it, and names it to the control plane, digest and all,
    // which then has nothing to send.
    #[tokio::test]
    #[allow(clippy::await_holding_lock)] // one test at a time: see hold_globals
    async fn a_patched_config_survives_a_restart() {
        let _globals = hold_globals();
        let (url, mock) = gateway_cp().await;
        let dir = TempDir::new("delta-state");
        let (st, first, store) = running(&url, Some(&dir));
        until("it has version 3", || version_of(st) == 3).await;
        let v3 = config3(3);
        let mut v4 = v3.clone();
        v4["version"] = 4.into();
        v4["keys"][1] = serde_json::json!({"hash": "44".repeat(32), "id": "cp-added", "budget_usd": 2.0, "h": "k4"});
        let v4 = delta::tests::sealed(v4);
        let changes = serde_json::json!({"keys": {"upsert": [v4["keys"][1]], "remove": ["22".repeat(32)]}});
        mock.serve(vec![delta3(&v3, &v4, changes)], v4.clone());
        until("it has version 4", || version_of(st) == 4).await;
        // the save loop writes at most every SAVE_INTERVAL: write now
        let store = store.unwrap();
        store.flush(|| Some(Default::default()));
        first.abort();
        let _ = first.await;
        drop(store);

        // the state directory is free once the first gateway's tasks are gone
        let deadline = Instant::now() + Duration::from_secs(20);
        let store = loop {
            match Store::open(&dir.0, "llmrg_test") {
                Ok(store) => break Arc::new(store),
                Err(e) => assert!(Instant::now() < deadline, "{e}"),
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        };
        let polls = mock.polls.lock().unwrap().len();
        let cp = ControlPlane::new(&url, "llmrg_test".into(), "test".into()).with_state(Some(store));
        let st = new_state();
        cp.restore(st).await;
        assert!(st.is_ready(), "served from the state directory");
        assert_eq!(key_ids(&st.config.load_full().unwrap()), ["cp-added", "cp-budgeted"]);
        {
            let held = cp.held.lock().unwrap().clone().unwrap();
            assert_eq!((held.epoch.as_str(), held.version, held.digest.as_deref()), ("e1", 4, v4["digest"].as_str()));
        }
        let second = tokio::spawn(Arc::new(cp).start(st));
        until("it polls", || mock.polls.lock().unwrap().len() > polls + 1).await;
        assert_eq!(mock.polls.lock().unwrap()[polls].as_deref(), Some("\"e1-v4\""), "named, not fetched anew");
        assert_eq!(mock.digests.lock().unwrap()[polls].as_deref(), v4["digest"].as_str());
        assert_eq!(version_of(st), 4);
        second.abort();
        let _ = second.await;
    }

    // A control plane that only knows schema 2 answers the join with a 400
    // that says to upgrade it: the gateway joins with schema 2 and runs as
    // it did before schema 3, until a join finds the control plane upgraded
    // (it also looks by itself: see a_fallback_session_looks_for_an_upgraded_control_plane).
    #[tokio::test]
    #[allow(clippy::await_holding_lock)] // one test at a time: see hold_globals
    async fn an_older_control_plane_is_joined_with_its_schema() {
        let _globals = hold_globals();
        drop(usage::take(usize::MAX, usize::MAX));
        let (url, mock) = gateway_cp().await;
        mock.older.store(true, Ordering::Relaxed);
        let (st, running, _) = running(&url, None);
        until("it serves", || st.is_ready()).await;
        until("it polls by name", || mock.polls.lock().unwrap().len() >= 2).await;
        assert_eq!(*mock.join_schemas.lock().unwrap(), [3, 2]);
        assert_eq!(mock.polls.lock().unwrap()[..2], [None, Some("\"v3\"".to_string())]);
        assert!(mock.digests.lock().unwrap().iter().all(|d| d.is_none()), "no digest before schema 3");
        assert_eq!(key_ids(&st.config.load_full().unwrap()), ["cp-budgeted", "cp-free"]);
        request("older-1");
        until("it syncs", || mock.models("sync", 200).iter().any(|m| m == &["older-1"])).await;
        for (_, sync) in mock.syncs() {
            let sent = sync.as_object().unwrap();
            assert!(["config_version", "config_epoch", "cursors"].iter().all(|f| !sent.contains_key(*f)), "{sync}");
            assert_eq!(sync["cursor"], "");
        }

        // upgraded: the next join is on schema 3, and so is everything after it
        mock.older.store(false, Ordering::Relaxed);
        mock.expire.store(1, Ordering::Relaxed);
        until("it joins again", || mock.join_schemas.lock().unwrap().len() >= 3).await;
        assert_eq!(*mock.join_schemas.lock().unwrap(), [3, 2, 3]);
        // the schema 2 config names no epoch: it is not version 3 of "e1", which is fetched in full
        until("it has the schema 3 config", || {
            mock.polls.lock().unwrap().last().is_some_and(|tag| tag.as_deref() == Some("\"e1-v3\""))
        })
        .await;
        until("it names the digest", || mock.digests.lock().unwrap().last().unwrap().is_some()).await;
        request("newer-1");
        until("it syncs", || mock.models("sync", 200).iter().any(|m| m.contains(&"newer-1".to_string()))).await;
        let (_, last) = mock.syncs().pop().unwrap();
        assert_eq!((&last["config_version"], &last["config_epoch"]), (&serde_json::json!(3), &serde_json::json!("e1")));
        assert!(last["cursors"].is_object());
        running.abort();
        let _ = running.await;
    }

    // Each control-plane replica numbers its limits cursors itself: the
    // gateway keeps the last one of each and sends them all, with the one
    // used last in the field a single control plane reads.
    #[tokio::test]
    #[allow(clippy::await_holding_lock)] // one test at a time: see hold_globals
    async fn each_replica_gets_its_own_cursor_back() {
        let _globals = hold_globals();
        drop(usage::take(usize::MAX, usize::MAX));
        let (url, mock) = gateway_cp().await;
        *mock.replicas.lock().unwrap() = vec!["a", "b"];
        let cp = ControlPlane::new(&url, "llmrg_test".into(), "test".into());
        cp.join().await.unwrap();
        let mut s = cp.syncer.lock().await;
        for _ in 0..5 {
            assert!(cp.sync_once(&mut s).await);
        }
        let sent: Vec<(serde_json::Value, serde_json::Value)> =
            mock.syncs().into_iter().map(|(_, v)| (v["cursor"].clone(), v["cursors"].clone())).collect();
        let j = |v: serde_json::Value| v;
        assert_eq!(
            sent,
            [
                (j("".into()), serde_json::json!({})),
                (j("a-1".into()), serde_json::json!({"a": "a-1"})),
                (j("b-1".into()), serde_json::json!({"a": "a-1", "b": "b-1"})),
                (j("a-2".into()), serde_json::json!({"a": "a-2", "b": "b-1"})),
                (j("b-2".into()), serde_json::json!({"a": "a-2", "b": "b-2"})),
            ]
        );
        // no config yet: version 0 of no epoch
        let (_, first) = &mock.syncs()[0];
        assert_eq!((&first["config_version"], &first["config_epoch"]), (&serde_json::json!(0), &serde_json::json!("")));

        // a failed sync is resent with the cursors it was frozen with, and moves none
        mock.fail.store(1, Ordering::Relaxed);
        assert!(!cp.sync_once(&mut s).await);
        assert_eq!(s.cursor(), "a-3");
        assert!(cp.sync_once(&mut s).await);
        let syncs = mock.syncs();
        assert_eq!(syncs[5].1, syncs[6].1);
        assert_eq!(s.cursor(), "b-3");

        // the eight used last are kept
        let mut s = Syncer::default();
        for i in 0..12 {
            s.answered(format!("r{i}"), format!("c{i}"));
        }
        s.answered("r7".into(), "c7b".into());
        assert_eq!(s.cursors.len(), MAX_CURSORS);
        assert_eq!(s.cursor(), "c7b");
        let replicas: Vec<&str> = s.cursors.iter().map(|(r, _)| r.as_str()).collect();
        assert_eq!(replicas, ["r7", "r11", "r10", "r9", "r8", "r6", "r5", "r4"]);
        // a control plane that names no replica has one cursor, as before
        let mut s = Syncer::default();
        s.answered(String::new(), "c1".into());
        s.answered(String::new(), "c2".into());
        assert_eq!((s.cursor(), s.cursors.len()), ("c2", 1));
    }

    // A 503 from sync (a control plane whose snapshot is behind this
    // gateway's config says so, with Retry-After) is a failure to retry:
    // the batch keeps its number and its events, and goes out again as it was.
    #[tokio::test]
    #[allow(clippy::await_holding_lock)] // one test at a time: see hold_globals
    async fn a_503_from_sync_keeps_the_batch() {
        let _globals = hold_globals();
        drop(usage::take(usize::MAX, usize::MAX));
        let dir = TempDir::new("sync-503");
        let (url, mock) = gateway_cp().await;
        let cp = ControlPlane::new(&url, "llmrg_test".into(), "test".into()).with_spool(Some(spool_in(&dir, u64::MAX)));
        cp.join().await.unwrap();
        let mut s = cp.syncer.lock().await;
        request("kept-1");
        request("kept-2");
        mock.fail.store(3, Ordering::Relaxed);
        for _ in 0..3 {
            assert!(!cp.sync_once(&mut s).await, "503");
            request("later"); // what arrives meanwhile waits for the next batch
            assert_eq!(s.batch, 1, "the batch number does not move");
            assert_eq!(s.pending.as_ref().unwrap().frozen.events.len(), 2);
            assert!(!spool_files(&dir).is_empty(), "its events stay on disk");
        }
        assert!(mock.models("replay", 200).is_empty(), "a 503 is no reason to replay");
        assert!(cp.sync_once(&mut s).await);
        let syncs = mock.syncs();
        assert_eq!(syncs.iter().map(|(code, _)| *code).collect::<Vec<_>>(), [503, 503, 503, 200]);
        assert!(syncs.iter().all(|(_, v)| *v == syncs[0].1), "the same batch each time");
        assert_eq!(syncs[3].1["batch"], 1);
        assert_eq!(mock.models("sync", 200), [["kept-1", "kept-2"]]);
        assert!(cp.sync_once(&mut s).await);
        assert_eq!(mock.syncs()[4].1["batch"], 2);
        assert_eq!(mock.models("sync", 200)[1], ["later", "later", "later"]);
        assert!(spool_files(&dir).is_empty());
    }

    // A saved config the control plane has nothing newer than is confirmed
    // by 304s and never sent again: those confirmations, and the spend the
    // syncs bring, must still reach the state directory, or the next start
    // would find a confirmation as old as the last config change.
    #[tokio::test]
    #[allow(clippy::await_holding_lock)] // one test at a time: see hold_globals
    async fn a_restored_config_confirmed_by_304_is_saved_again() {
        let _globals = hold_globals();
        let (url, mock) = gateway_cp().await;
        let dir = TempDir::new("restored-304");
        let hour_ago = now_ms() - 3_600_000;
        {
            let store = Store::open(&dir.0, "llmrg_test").unwrap();
            store.put_config(true, body(&config3(3)), hour_ago, 3, "g1");
            store.flush(|| Some([("key:cp-budgeted".to_string(), 0.25)].into_iter().collect()));
        }
        let (st, running, store) = running(&url, Some(&dir));
        let store = store.unwrap();
        until("several polls", || mock.polls.lock().unwrap().len() >= 4).await;
        until("several syncs", || mock.syncs().len() >= 3).await;
        assert_eq!(version_of(st), 3);
        assert!(mock.polls.lock().unwrap().iter().all(|tag| tag.as_deref() == Some("\"e1-v3\"")), "never sent anew");
        let started = now_ms();
        until("a 304 after this", || st.ready.config_age().is_some_and(|age| age < Duration::from_millis(50))).await;
        store.flush(|| Some([("key:cp-budgeted".to_string(), 0.75)].into_iter().collect()));
        running.abort();
        let _ = running.await;
        let saved = store.load().unwrap();
        assert!(saved.confirmed.confirmed_at_ms >= started - 1000, "{:?}", saved.confirmed);
        assert_eq!((saved.confirmed.version, saved.confirmed.group.as_str()), (3, "g1"));
        assert_eq!(saved.confirmed.spend.unwrap()["key:cp-budgeted"], 0.75);
        // and the config beside it is still the one it confirms
        assert_eq!(load(saved.gzip, saved.config.into()).unwrap().cfg.version, 3);
    }

    // A control plane from before schema 3 answering a schema 3 session
    // (rolled back, or one replica of several): its config has no epoch,
    // which is not a new epoch. It is taken only when newer by version; the
    // same version confirms the config held; and the gateway joins again.
    #[test]
    fn an_older_control_planes_config_is_judged_by_version() {
        let _globals = hold_globals();
        let Loaded { held, .. } = received(None, &config3(7), true);
        let answer = |version: i64, unconditional| {
            receive(Some(held.clone()), false, wire_config(version).into(), 3, unconditional)
        };
        for unconditional in [false, true] {
            assert!(
                matches!(answer(6, unconditional), Received::Kept { confirmed: false, older: true, .. }),
                "an older version would bring back what was removed since"
            );
            assert!(matches!(answer(7, unconditional), Received::Kept { confirmed: true, older: true, .. }));
        }
        let Received::Config(newer) = answer(8, false) else { panic!("a newer version is taken") };
        assert!(newer.older && newer.cfg.version == 8 && newer.held.digest.is_none());
        // with nothing held there is nothing to compare: taken, and noted
        let Received::Config(first) = receive(None, false, wire_config(6).into(), 3, true) else { panic!("not taken") };
        assert!(first.older);
        // a schema 3 config is never mistaken for one
        assert!(!received(Some(&held), &config3(8), false).older);
    }

    // Over the wire: the control plane is rolled back under a schema 3
    // session. The config held stays and counts as confirmed, and the
    // gateway joins again, on schema 2, where polls are long polls again.
    #[tokio::test]
    #[allow(clippy::await_holding_lock)] // one test at a time: see hold_globals
    async fn a_rolled_back_control_plane_is_joined_again_on_its_schema() {
        let _globals = hold_globals();
        let (url, mock) = gateway_cp().await;
        let (st, running, _) = running(&url, None);
        until("it polls by name", || mock.polls.lock().unwrap().len() >= 2).await;
        assert_eq!(*mock.join_schemas.lock().unwrap(), [3]);

        mock.older.store(true, Ordering::Relaxed);
        until("it joins again", || mock.join_schemas.lock().unwrap().len() >= 3).await;
        assert_eq!(*mock.join_schemas.lock().unwrap(), [3, 3, 2]);
        until("it polls as schema 2 does", || {
            mock.polls.lock().unwrap().last().is_some_and(|tag| tag.as_deref() == Some("\"v3\""))
        })
        .await;
        assert_eq!(version_of(st), 3);
        assert!(st.is_ready() && st.ready.config_age().is_some_and(|age| age < Duration::from_secs(5)));
        // long polls, not a download every two seconds
        let polls = mock.polls.lock().unwrap().len();
        tokio::time::sleep(Duration::from_millis(100)).await;
        let later = mock.polls.lock().unwrap();
        assert!(later.len() <= polls + 6 && later[polls..].iter().all(|tag| tag.as_deref() == Some("\"v3\"")));
        drop(later);
        running.abort();
        let _ = running.await;
    }

    // A gateway on a schema 2 session keeps asking for schema 3: a control
    // plane upgraded later is picked up without a restart, and until then
    // each attempt leaves the session in use alone.
    #[tokio::test]
    #[allow(clippy::await_holding_lock)] // one test at a time: see hold_globals
    async fn a_fallback_session_looks_for_an_upgraded_control_plane() {
        let _globals = hold_globals();
        let (url, mock) = gateway_cp().await;
        mock.older.store(true, Ordering::Relaxed);
        let cp = ControlPlane::new(&url, "llmrg_test".into(), "test".into());
        cp.join().await.unwrap();
        let before = (cp.session(), cp.joins.load(Ordering::Relaxed));
        assert_eq!(cp.schema.load(Ordering::Relaxed), 2);
        assert!(!cp.try_upgrade().await);
        assert_eq!(*mock.join_schemas.lock().unwrap(), [3, 2, 3], "asked once, not joined on 2 again");
        assert_eq!((cp.session(), cp.joins.load(Ordering::Relaxed)), before, "the session in use is untouched");
        assert_eq!(cp.schema.load(Ordering::Relaxed), 2);
        // a refusal of the attempt is no verdict on the gateway
        mock.mode.store(REFUSE, Ordering::Relaxed);
        assert!(!cp.try_upgrade().await);
        assert!(cp.refused_since.lock().unwrap().is_none());
        mock.mode.store(ACCEPT, Ordering::Relaxed);

        mock.older.store(false, Ordering::Relaxed);
        assert!(cp.try_upgrade().await);
        assert_eq!(cp.schema.load(Ordering::Relaxed), 3);
        assert_eq!(cp.joins.load(Ordering::Relaxed), before.1 + 1);
        let asked = mock.join_schemas.lock().unwrap().len();
        assert!(!cp.try_upgrade().await, "nothing to look for on schema 3");
        assert_eq!(mock.join_schemas.lock().unwrap().len(), asked);

        // the running gateway does it by itself
        mock.older.store(true, Ordering::Relaxed);
        let cp = ControlPlane::new(&url, "llmrg_test".into(), "test".into()).upgrading_every(Duration::from_millis(30));
        let st = new_state();
        let running = tokio::spawn(Arc::new(cp).start(st));
        until("it serves on schema 2", || st.is_ready()).await;
        let asked = mock.join_schemas.lock().unwrap().len();
        until("it asks again, twice", || mock.join_schemas.lock().unwrap().len() >= asked + 2).await;
        assert!(mock.join_schemas.lock().unwrap()[asked..].iter().all(|schema| *schema == 3));
        assert!(mock.digests.lock().unwrap().iter().all(|d| d.is_none()), "still a schema 2 session");
        mock.older.store(false, Ordering::Relaxed);
        until("it polls as schema 3 does", || mock.digests.lock().unwrap().last().unwrap().is_some()).await;
        assert_eq!(version_of(st), 3);
        running.abort();
        let _ = running.await;
    }

    /// This process's resident memory in MiB, as `ps` reports it.
    fn rss_mib() -> f64 {
        let out = std::process::Command::new("ps")
            .args(["-o", "rss=", "-p", &std::process::id().to_string()])
            .output()
            .unwrap();
        String::from_utf8_lossy(&out.stdout).trim().parse::<f64>().unwrap() / 1024.0
    }

    /// A full schema 3 config of 100,000 keys, 20,000 users and 200 teams as
    /// the control plane sends it, gzipped, with its digest and its first
    /// key's entry; `first_h` is that key's entry hash.
    fn large_config(version: i64, first_h: &str) -> (Bytes, String, String) {
        let hex = |seed: u64, n: usize| -> String {
            let mut rng = fastrand::Rng::with_seed(seed);
            (0..n).map(|_| char::from_digit(rng.u32(..16), 16).unwrap()).collect()
        };
        let id = |kind: u64, i: u64| {
            let h = hex(kind << 32 | i, 32);
            format!("{}-{}-{}-{}-{}", &h[..8], &h[8..12], &h[12..16], &h[16..20], &h[20..])
        };
        let mut digest = delta::Digest::default();
        let mut first = String::new();
        let mut json = String::with_capacity(64 << 20);
        json.push_str(&format!(
            r#"{{"schema":3,"version":{version},"epoch":"e1","rest_hash":"r1","group":{{"id":"g1","name":"group"}},"keys":["#
        ));
        for i in 0..100_000u64 {
            let hash = hex(1 << 40 | i, 64);
            let h = if i == 0 { first_h.to_string() } else { hex(2 << 40 | i, 64) };
            digest.add("keys", &hash, &h);
            let entry = format!(
                r#"{{"hash":"{hash}","id":"{}","user_id":"{}","team_id":"{}","allowed_models":null,"budget_usd":50,"rpm_limit":600,"expires_at":0,"tags":["team-a"],"h":"{h}"}}"#,
                id(1, i),
                id(2, i % 20_000),
                id(3, i % 200),
            );
            if i == 0 {
                first = entry.clone();
            } else {
                json.push(',');
            }
            json.push_str(&entry);
        }
        json.push_str(r#"],"users":["#);
        for i in 0..20_000u64 {
            let h = hex(3 << 40 | i, 64);
            digest.add("users", &id(2, i), &h);
            json.push_str(&format!(
                r#"{}{{"id":"{}","allowed_models":null,"team_models":["gpt-4o","claude-*"],"memberships":{{"{}":100}},"budget_usd":200,"team_ids":["{}"],"tags":[],"h":"{h}"}}"#,
                if i > 0 { "," } else { "" },
                id(2, i),
                id(3, i % 200),
                id(3, i % 200),
            ));
        }
        json.push_str(r#"],"teams":["#);
        for i in 0..200u64 {
            let h = hex(4 << 40 | i, 64);
            digest.add("teams", &id(3, i), &h);
            json.push_str(&format!(
                r#"{}{{"id":"{}","allowed_models":["gpt-4o","claude-*"],"budget_usd":10000,"h":"{h}"}}"#,
                if i > 0 { "," } else { "" },
                id(3, i),
            ));
        }
        digest.add("rest", "", "r1");
        json.push_str(&format!(r#"],"digest":"{}"}}"#, digest.hex()));
        println!("the config is {} MiB of JSON", json.len() >> 20);
        (gzip(json.as_bytes()).unwrap().into(), digest.hex(), first)
    }

    // What holding the config for deltas costs in memory and time, with
    // 100,000 keys. Run it alone, the way the gateway runs (one config, one
    // process):
    //   cargo test --release --lib -- --ignored --nocapture --test-threads 1 memory_of_a_large_config
    #[test]
    #[ignore = "a measurement, not a check: prints resident memory"]
    fn memory_of_a_large_config() {
        let _globals = hold_globals();
        // the delta first: making it is not what is measured
        let (old_h, new_h) = ("cd".repeat(32), "ab".repeat(32));
        let (_, v4, changed) = large_config(4, &new_h);
        let (body, v3, _) = large_config(3, &old_h);
        let d = format!(
            r#"{{"schema":3,"delta":true,"epoch":"e1","base_version":3,"base_digest":"{v3}","version":4,"digest":"{v4}","keys":{{"upsert":[{changed}]}}}}"#
        );
        let d: Bytes = gzip(d.as_bytes()).unwrap().into();
        let start = rss_mib();
        println!("before: {start:.0} MiB, the response body ({:.1} MiB gzipped) in it", body.len() as f64 / 1048576.0);

        let took = Instant::now();
        let Loaded { cfg, held, .. } = match receive(None, true, body, 3, true) {
            Received::Config(loaded) => *loaded,
            _ => panic!("the config was not taken"),
        };
        let took = took.elapsed();
        assert_eq!((cfg.keys.len(), held.digest.as_deref()), (100_000, Some(v3.as_str())));
        let cfg = Arc::new(cfg);
        let serving = rss_mib();
        println!(
            "serving the full config, read in {took:?}: {serving:.0} MiB, {:.1} MiB of it the config held for deltas",
            held.body.len() as f64 / 1048576.0
        );

        let took = Instant::now();
        let Loaded { cfg: next, held, .. } = match receive(Some(held), true, d, 3, false) {
            Received::Config(loaded) => *loaded,
            Received::Discarded(why) => panic!("{why}"),
            _ => panic!("the delta was not taken"),
        };
        let took = took.elapsed();
        let both = rss_mib();
        drop(cfg); // the hot path swaps to the new one
        assert_eq!((next.version, next.keys.len(), held.digest.as_deref()), (4, 100_000, Some(v4.as_str())));
        println!(
            "after a delta, applied in {took:?}: {both:.0} MiB with both configs, {:.0} MiB with the new one alone ({:.1} MiB held)",
            rss_mib(),
            held.body.len() as f64 / 1048576.0
        );
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
