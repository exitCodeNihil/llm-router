//! Rate limits and budgets, enforced here and kept in step with every other
//! gateway through the control plane (internal/limits/limits.go).
//!
//! Each limited scope ("key:<id>", "user:<id>", "team:<id>", "member:<u>:<t>")
//! holds the combined counts the control plane sent at the last sync, plus what
//! this gateway admitted since: "pending" until a sync freezes it into a batch,
//! "inflight" until the control plane acknowledges that batch. A sliding
//! one-minute window over those decides; budgets add local spend on top of the
//! control plane's. Two gateways can each admit what one sync interval allows
//! before they hear about each other.

use std::collections::HashMap;
use std::sync::atomic::{AtomicI64, AtomicU64, Ordering};
use std::sync::{Arc, Mutex, OnceLock};

use serde::{Deserialize, Serialize};

use crate::config::Key;
use crate::now_ms;

#[derive(Debug, Clone, Copy, Default)]
struct Delta {
    minute: i64,
    requests: i64,
    tokens: i64,
}

#[derive(Default)]
struct Inner {
    budget: Option<f64>,
    rpm: Option<i64>,
    tpm: Option<i64>,
    // the control plane's combined view at the last sync
    g_minute: i64,
    g_rpm: [i64; 2], // this minute, last minute
    g_tpm: [i64; 2],
    g_spend: f64,
    // what this gateway admitted that the control plane hasn't counted yet
    pending: Vec<Delta>,
    inflight: Vec<Delta>,
    spend_pending: f64,
    spend_inflight: f64,
    dirty: bool,
}

impl Inner {
    /// The count over the last 60 seconds at `minute`, `frac` of the way through it.
    fn estimate(&self, minute: i64, frac: f64, g: [i64; 2], pick: fn(&Delta) -> i64) -> f64 {
        let (mut cur, mut prev) = match minute - self.g_minute {
            0 => (g[0], g[1]),
            1 => (0, g[0]),
            _ => (0, 0),
        };
        for d in self.pending.iter().chain(&self.inflight) {
            if d.minute == minute {
                cur += pick(d);
            } else if d.minute == minute - 1 {
                prev += pick(d);
            }
        }
        prev as f64 * (1.0 - frac) + cur as f64
    }

    fn add(&mut self, minute: i64, requests: i64, tokens: i64) {
        match self.pending.iter_mut().find(|d| d.minute == minute) {
            Some(d) => {
                d.requests += requests;
                d.tokens += tokens;
            }
            None => self.pending.push(Delta { minute, requests, tokens }),
        }
        // forget minutes that can no longer count
        self.pending.retain(|d| d.minute >= minute - 1);
    }
}

pub struct Scope {
    pub name: String,
    generation: AtomicU64,
    inner: Mutex<Inner>,
}

impl Scope {
    fn mark_dirty(self: &Arc<Self>, inner: &mut Inner) {
        if !inner.dirty {
            inner.dirty = true;
            dirty().lock().unwrap().push(self.clone());
        }
    }
}

fn registry() -> &'static Mutex<HashMap<String, Arc<Scope>>> {
    static R: OnceLock<Mutex<HashMap<String, Arc<Scope>>>> = OnceLock::new();
    R.get_or_init(Default::default)
}

fn dirty() -> &'static Mutex<Vec<Arc<Scope>>> {
    static D: OnceLock<Mutex<Vec<Arc<Scope>>>> = OnceLock::new();
    D.get_or_init(Default::default)
}

static GENERATION: AtomicU64 = AtomicU64::new(0);
static CLOCK_OFFSET_MS: AtomicI64 = AtomicI64::new(0);

/// The scope for `name` if it has any limit, shared across config versions so
/// a reload keeps its counts.
pub fn scope(name: String, budget: Option<f64>, rpm: Option<i64>, tpm: Option<i64>) -> Option<Arc<Scope>> {
    if budget.is_none() && rpm.is_none() && tpm.is_none() {
        return None;
    }
    let s = registry()
        .lock()
        .unwrap()
        .entry(name.clone())
        .or_insert_with(|| Arc::new(Scope { name, generation: AtomicU64::new(0), inner: Default::default() }))
        .clone();
    s.generation.store(GENERATION.load(Ordering::Relaxed), Ordering::Relaxed);
    let mut i = s.inner.lock().unwrap();
    (i.budget, i.rpm, i.tpm) = (budget, rpm, tpm);
    drop(i);
    Some(s)
}

/// Call before building a config; pass the result to `end_config` after, so
/// scopes the new config no longer has are forgotten.
pub fn begin_config() -> u64 {
    GENERATION.fetch_add(1, Ordering::Relaxed) + 1
}

pub fn end_config(generation: u64) {
    registry().lock().unwrap().retain(|_, s| s.generation.load(Ordering::Relaxed) == generation);
}

/// The control plane's clock, as of the last sync: minute buckets must agree
/// across gateways.
fn clock() -> (i64, f64) {
    let ms = now_ms() + CLOCK_OFFSET_MS.load(Ordering::Relaxed);
    (ms.div_euclid(60_000), ms.rem_euclid(60_000) as f64 / 60_000.0)
}

/// Why a request was refused: the status, error code and message the client
/// gets, and when to retry.
#[derive(Debug, PartialEq)]
pub struct Denial {
    pub code: &'static str,
    pub message: String,
    pub retry_after_s: Option<u64>,
}

/// Check every scope of `key`, then take one request from each rate limit.
/// Nothing is taken when any scope refuses.
pub fn check(key: &Key) -> Result<(), Denial> {
    if key.scopes().next().is_none() {
        return Ok(());
    }
    let (minute, frac) = clock();
    let retry = Some((60.0 * (1.0 - frac)).ceil() as u64);
    for s in key.scopes() {
        let i = s.inner.lock().unwrap();
        if let Some(budget) = i.budget {
            let spent = i.g_spend + i.spend_pending + i.spend_inflight;
            if spent >= budget {
                return Err(Denial {
                    code: "budget_exceeded",
                    message: format!("budget of ${budget} exhausted for {} (spent ${spent})", s.name),
                    retry_after_s: None,
                });
            }
        }
        if let Some(tpm) = i.tpm
            && i.estimate(minute, frac, i.g_tpm, |d| d.tokens) >= tpm as f64
        {
            return Err(Denial {
                code: "rate_limit_exceeded",
                message: format!("token rate limit of {tpm} TPM exceeded for {}", s.name),
                retry_after_s: retry,
            });
        }
        if let Some(rpm) = i.rpm
            && i.estimate(minute, frac, i.g_rpm, |d| d.requests) + 1.0 > rpm as f64
        {
            return Err(Denial {
                code: "rate_limit_exceeded",
                message: format!("rate limit of {rpm} RPM exceeded for {}", s.name),
                retry_after_s: retry,
            });
        }
    }
    for s in key.scopes() {
        let mut i = s.inner.lock().unwrap();
        if i.rpm.is_some() {
            i.add(minute, 1, 0);
            s.mark_dirty(&mut i);
        }
    }
    Ok(())
}

/// Count a finished request's tokens and spend against `key`'s scopes. Call it
/// after the request's usage event is queued, so the control plane never
/// acknowledges spend before it has the event.
pub fn record(key: &Key, tokens: i64, cost_usd: f64) {
    if tokens <= 0 && cost_usd <= 0.0 {
        return;
    }
    let (minute, _) = clock();
    for s in key.scopes() {
        let mut i = s.inner.lock().unwrap();
        let mut changed = false;
        if tokens > 0 && i.tpm.is_some() {
            i.add(minute, 0, tokens);
            changed = true;
        }
        if cost_usd > 0.0 && i.budget.is_some() {
            i.spend_pending += cost_usd;
            changed = true;
        }
        if changed {
            s.mark_dirty(&mut i);
        }
    }
}

/// One scope's counts for one minute, as the sync sends them (gwsync.RateDelta).
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct RateDelta {
    pub scope: String,
    pub minute: i64,
    pub requests: i64,
    pub tokens: i64,
}

/// Freeze everything pending into a batch: the rate deltas to send, and the
/// scopes to settle when the control plane acknowledges it.
pub fn freeze() -> (Vec<RateDelta>, Vec<Arc<Scope>>) {
    let scopes = std::mem::take(&mut *dirty().lock().unwrap());
    let mut deltas = Vec::new();
    for s in &scopes {
        let mut i = s.inner.lock().unwrap();
        i.dirty = false;
        let pending = std::mem::take(&mut i.pending);
        for d in &pending {
            if d.requests != 0 || d.tokens != 0 {
                deltas.push(RateDelta {
                    scope: s.name.clone(),
                    minute: d.minute,
                    requests: d.requests,
                    tokens: d.tokens,
                });
            }
        }
        i.inflight.extend(pending);
        i.spend_inflight += std::mem::take(&mut i.spend_pending);
    }
    (deltas, scopes)
}

/// What the control plane answers a sync with (limits.Counters).
#[derive(Debug, Default, Deserialize)]
pub struct Counters {
    #[serde(default)]
    pub cursor: String,
    #[serde(default)]
    pub now_ms: i64,
    #[serde(default)]
    pub minute: i64,
    #[serde(default)]
    pub spend: HashMap<String, f64>,
    #[serde(default)]
    pub rates: HashMap<String, [i64; 4]>,
}

/// Settle an acknowledged batch: its counts are now in the control plane's
/// figures, which replace ours.
pub fn apply(batch: &[Arc<Scope>], c: &Counters) {
    if c.now_ms > 0 {
        CLOCK_OFFSET_MS.store(c.now_ms - now_ms(), Ordering::Relaxed);
    }
    for s in batch {
        let mut i = s.inner.lock().unwrap();
        i.inflight.clear();
        i.spend_inflight = 0.0;
    }
    let reg = registry().lock().unwrap();
    for s in reg.values() {
        let mut i = s.inner.lock().unwrap();
        if let Some(usd) = c.spend.get(&s.name) {
            i.g_spend = *usd;
        }
        if i.rpm.is_some() || i.tpm.is_some() {
            // Scopes without recent traffic anywhere aren't in the answer: zero.
            let r = c.rates.get(&s.name).copied().unwrap_or_default();
            i.g_minute = c.minute;
            i.g_rpm = [r[0], r[1]];
            i.g_tpm = [r[2], r[3]];
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    // The scope registry and dirty list are process-wide: one test at a time.
    static SERIAL: Mutex<()> = Mutex::new(());

    fn key(name: &str, budget: Option<f64>, rpm: Option<i64>) -> Key {
        Key {
            id: name.into(),
            user_id: String::new(),
            team_id: String::new(),
            user: None,
            team: None,
            allowed_models: None,
            expires_at: 0,
            scope: scope(format!("key:{name}"), budget, rpm, None),
        }
    }

    #[test]
    fn rpm_holds_until_synced_counts_say_otherwise() {
        let _serial = SERIAL.lock().unwrap();
        let k = key("t-rpm", None, Some(3));
        for _ in 0..3 {
            assert!(check(&k).is_ok());
        }
        let d = check(&k).unwrap_err();
        assert_eq!(d.code, "rate_limit_exceeded");
        assert!(d.retry_after_s.is_some());

        // Sync: the three requests go out, and the control plane says other
        // gateways sent nothing more.
        let (deltas, scopes) = freeze();
        let mine: i64 = deltas.iter().filter(|d| d.scope == "key:t-rpm").map(|d| d.requests).sum();
        assert_eq!(mine, 3);
        assert!(check(&k).is_err(), "inflight requests still count");
        let (minute, _) = clock();
        let mut c = Counters { minute, ..Default::default() };
        c.rates.insert("key:t-rpm".into(), [3, 0, 0, 0]);
        apply(&scopes, &c);
        assert!(check(&k).is_err(), "the control plane's count includes them now");

        // Another sync where the control plane reports the minute empty.
        let (_, scopes) = freeze();
        apply(&scopes, &Counters { minute: minute + 2, ..Default::default() });
        assert!(check(&k).is_ok());
    }

    #[test]
    fn budget_counts_local_and_global_spend() {
        let _serial = SERIAL.lock().unwrap();
        let k = key("t-budget", Some(1.0), None);
        assert!(check(&k).is_ok());
        record(&k, 10, 0.6);
        assert!(check(&k).is_ok());
        record(&k, 10, 0.5);
        assert_eq!(check(&k).unwrap_err().code, "budget_exceeded");
        let (_, scopes) = freeze();
        let mut c = Counters::default();
        c.spend.insert("key:t-budget".into(), 0.2); // e.g. a refund the control plane knows of
        apply(&scopes, &c);
        assert!(check(&k).is_ok(), "after the ack the control plane's figure stands");
    }

    #[test]
    fn unlimited_keys_skip_everything() {
        let _serial = SERIAL.lock().unwrap();
        let k = key("t-free", None, None);
        assert!(k.scope.is_none());
        assert!(check(&k).is_ok());
    }
}
