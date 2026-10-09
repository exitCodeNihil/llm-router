//! Usage events and their cost (internal/usage, internal/pricing). Events queue
//! here and the sync loop uploads them; the control plane writes them to
//! Postgres and decides what telemetry to export.

use std::collections::VecDeque;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::Instant;

use serde::Serialize;

use crate::config::{AuthMode, Config, Deployment, Key};
use crate::translate::Usage;
use crate::{limits, metrics};

/// One request, as gwsync.SyncEvent expects it.
#[derive(Debug, Serialize)]
pub struct Event {
    pub ts_ms: i64,
    pub request_id: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub api_key_id: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub user_id: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub team_id: String,
    pub model_name: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub deployment_id: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub provider_id: String,
    pub prompt_tokens: i64,
    pub completion_tokens: i64,
    pub cached_tokens: i64,
    pub cost_usd: f64,
    pub unpriced: bool,
    pub latency_ms: i64,
    pub status_code: u16,
    pub stream: bool,
    #[serde(skip_serializing_if = "is_zero_f")]
    pub notional_cost_usd: f64,
    #[serde(skip_serializing_if = "is_zero")]
    pub attempts: i64,
    #[serde(skip_serializing_if = "str::is_empty")]
    pub error_code: &'static str,
    #[serde(skip_serializing_if = "is_zero")]
    pub ttft_ms: i64,
}

fn is_zero(n: &i64) -> bool {
    *n == 0
}
fn is_zero_f(n: &f64) -> bool {
    *n == 0.0
}

/// Events waiting for the next sync. When the control plane is unreachable for
/// long enough to fill it, new events are dropped and counted.
const MAX_QUEUED: usize = 200_000;

fn queue() -> &'static Mutex<VecDeque<Event>> {
    static Q: OnceLock<Mutex<VecDeque<Event>>> = OnceLock::new();
    Q.get_or_init(Default::default)
}

pub static DROPPED: AtomicU64 = AtomicU64::new(0);

fn push(e: Event) {
    let mut q = queue().lock().unwrap();
    if q.len() >= MAX_QUEUED {
        drop(q);
        DROPPED.fetch_add(1, Ordering::Relaxed);
        return;
    }
    q.push_back(e);
}

/// Up to `max` queued events, oldest first.
pub fn take(max: usize) -> Vec<Event> {
    let mut q = queue().lock().unwrap();
    let n = q.len().min(max);
    q.drain(..n).collect()
}

pub fn queued() -> usize {
    queue().lock().unwrap().len()
}

/// Price a request (pricing.price): the deployment's own price, else the
/// catalog's, else unpriced. Cached tokens are part of the prompt and billed at
/// the cached rate when there is one.
fn price(cfg: &Config, d: &Deployment, u: Usage) -> Option<f64> {
    let (input, output, cached) = match (d.input_per_1m, d.output_per_1m) {
        (Some(i), Some(o)) => (i, o, d.cached_input_per_1m),
        _ if !d.catalog_model_id.is_empty() => {
            let p = cfg.prices.get(&d.catalog_model_id)?;
            (p.input_per_1m, p.output_per_1m, p.cached_input_per_1m)
        }
        _ => return None,
    };
    let mut billed = u.prompt;
    let mut cached_cost = 0.0;
    if let Some(rate) = cached
        && u.cached > 0
    {
        let c = u.cached.min(u.prompt);
        billed = u.prompt - c;
        cached_cost = c as f64 * rate / 1e6;
    }
    Some(billed as f64 * input / 1e6 + u.completion as f64 * output / 1e6 + cached_cost)
}

/// Everything known about a request before its outcome, to emit its event once.
pub struct Emit {
    pub cfg: Arc<Config>,
    pub key: Arc<Key>,
    pub model: String,
    pub request_id: String,
    pub stream: bool,
    pub start: Instant,
    pub ts_ms: i64,
    pub deployment: Option<Arc<Deployment>>,
    pub attempts: i64,
}

impl Emit {
    /// Queue the request's event and count its tokens and spend against its limits.
    pub fn send(self, status: u16, error_code: &'static str, u: Usage, ttft_ms: i64) {
        let mut e = Event {
            ts_ms: self.ts_ms,
            request_id: self.request_id,
            api_key_id: self.key.id.clone(),
            user_id: self.key.user.as_ref().map(|u| u.id.clone()).unwrap_or_default(),
            team_id: self.key.team.as_ref().map(|t| t.id.clone()).unwrap_or_default(),
            model_name: self.model,
            deployment_id: String::new(),
            provider_id: String::new(),
            prompt_tokens: u.prompt,
            completion_tokens: u.completion,
            cached_tokens: u.cached,
            cost_usd: 0.0,
            unpriced: false,
            latency_ms: self.start.elapsed().as_millis() as i64,
            status_code: status,
            stream: self.stream,
            notional_cost_usd: 0.0,
            attempts: self.attempts,
            error_code,
            ttft_ms,
        };
        if let Some(d) = &self.deployment {
            e.deployment_id = d.id.clone();
            e.provider_id = d.provider.id.clone();
            let listed = price(&self.cfg, d, u);
            if d.provider.auth_mode == AuthMode::OAuthPassthrough {
                // Billed to the caller's own plan: no real money, but a list value.
                e.unpriced = true;
                e.notional_cost_usd = listed.unwrap_or(0.0);
            } else {
                e.unpriced = listed.is_none();
                e.cost_usd = listed.unwrap_or(0.0);
            }
        }
        let (cost, tokens) = (e.cost_usd, u.prompt + u.completion);
        metrics::latency(e.latency_ms);
        push(e);
        limits::record(&self.key, tokens, cost);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::Wire;

    #[test]
    fn prices_like_go() {
        let w: Wire = serde_json::from_str(
            r#"{"schema":1,"version":1,"group":{"id":"g","name":"g"},
            "providers":[{"id":"p","name":"p","type":"openai_compatible","auth_mode":"none"}],
            "deployments":[
              {"id":"custom","provider_id":"p","model_name":"a","upstream_name":"a","input_per_1m":2,"output_per_1m":8,"cached_input_per_1m":0.5},
              {"id":"cat","provider_id":"p","model_name":"b","upstream_name":"b","catalog_model_id":"cat-1"},
              {"id":"none","provider_id":"p","model_name":"c","upstream_name":"c","catalog_model_id":"missing"}
            ],
            "prices":{"cat-1":{"input_per_1m":1,"output_per_1m":2}}}"#,
        )
        .unwrap();
        let c = Config::build(w).unwrap();
        let d = |m: &str| c.resolve(m).unwrap().deployments[0].clone();
        let u = Usage { prompt: 1_000_000, completion: 500_000, cached: 400_000 };
        // 600k at $2 + 400k at $0.5 + 500k at $8
        assert!((price(&c, &d("a"), u).unwrap() - (1.2 + 0.2 + 4.0)).abs() < 1e-9);
        // no cached rate: all prompt tokens at the input rate
        assert!((price(&c, &d("b"), u).unwrap() - (1.0 + 1.0)).abs() < 1e-9);
        assert!(price(&c, &d("c"), u).is_none());
    }
}
