//! Usage events and their cost (internal/usage, internal/pricing). Events queue
//! here and the sync loop uploads them; the control plane writes them to
//! Postgres and decides what telemetry to export.

use std::collections::VecDeque;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::Instant;

use serde::Serialize;

use crate::config::{AuthMode, Config, Deployment, Key, Price};
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
    #[serde(skip_serializing_if = "is_zero", rename = "cache_write_5m_tokens")]
    pub cache_write_5m: i64,
    #[serde(skip_serializing_if = "is_zero", rename = "cache_write_1h_tokens")]
    pub cache_write_1h: i64,
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

/// Queued events, oldest first: at most `max_events`, and no more than about
/// `max_bytes` once encoded (always at least one).
pub fn take(max_events: usize, max_bytes: usize) -> Vec<Event> {
    let mut q = queue().lock().unwrap();
    let n = batch_len(q.iter().map(Event::encoded_size), max_events, max_bytes);
    q.drain(..n).collect()
}

/// How many leading items of the given sizes fit in one batch.
fn batch_len(sizes: impl Iterator<Item = usize>, max_events: usize, max_bytes: usize) -> usize {
    let mut n = 0;
    let mut bytes = 0;
    for size in sizes.take(max_events) {
        bytes += size;
        if n > 0 && bytes > max_bytes {
            break;
        }
        n += 1;
    }
    n
}

impl Event {
    /// About how many bytes the event takes as JSON: the fixed fields plus
    /// its strings. An estimate for sizing batches, not an exact count.
    fn encoded_size(&self) -> usize {
        400 + self.request_id.len()
            + self.api_key_id.len()
            + self.user_id.len()
            + self.team_id.len()
            + self.model_name.len()
            + self.deployment_id.len()
            + self.provider_id.len()
            + self.error_code.len()
    }
}

pub fn queued() -> usize {
    queue().lock().unwrap().len()
}

/// How many events the queue has room for without growing: memory it holds
/// even when empty.
pub fn capacity() -> usize {
    queue().lock().unwrap().capacity()
}

// Anthropic's pricing rules on top of a model's rates (internal/pricing).
const CACHE_WRITE_5M: f64 = 1.25;
const CACHE_WRITE_1H: f64 = 2.0;
const US_INFERENCE: f64 = 1.1;
const FAST_MODE: f64 = 2.0;

/// Price a request (pricing.price): the deployment's own price, else the
/// catalog's, else unpriced.
fn price(cfg: &Config, d: &Deployment, u: Usage) -> Option<f64> {
    let p = match (d.input_per_1m, d.output_per_1m) {
        (Some(input_per_1m), Some(output_per_1m)) => Price {
            input_per_1m,
            output_per_1m,
            cached_input_per_1m: d.cached_input_per_1m,
            long_prompt_tokens: 0,
            long_prompt_multiplier: 1.0,
        },
        _ if !d.catalog_model_id.is_empty() => *cfg.prices.get(&d.catalog_model_id)?,
        _ => return None,
    };
    Some(cost(p, u))
}

/// Go: pricing.priceCost. Shared fixtures in internal/pricing/testdata/cost.json.
fn cost(p: Price, u: Usage) -> f64 {
    // The parts can't exceed the whole, whatever an upstream reports.
    let part = |n: i64, left: i64| n.min(left).max(0);
    let cached = part(u.cached, u.prompt);
    let write_5m = part(u.write_5m, u.prompt - cached);
    let write_1h = part(u.write_1h, u.prompt - cached - write_5m);
    let fresh = u.prompt - cached - write_5m - write_1h;

    let cached_rate = p.cached_input_per_1m.unwrap_or(p.input_per_1m); // no cached rate: reads bill as input
    let mut usd = fresh as f64 * p.input_per_1m
        + cached as f64 * cached_rate
        + write_5m as f64 * p.input_per_1m * CACHE_WRITE_5M
        + write_1h as f64 * p.input_per_1m * CACHE_WRITE_1H
        + u.completion as f64 * p.output_per_1m;
    if p.long_prompt_tokens > 0 && u.prompt > p.long_prompt_tokens {
        usd *= p.long_prompt_multiplier;
    }
    if u.us_inference {
        usd *= US_INFERENCE;
    }
    if u.fast {
        usd *= FAST_MODE;
    }
    usd / 1e6
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
            cache_write_5m: u.write_5m,
            cache_write_1h: u.write_1h,
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
                // A wildcard has no price of its own: use the model asked for.
                e.unpriced = true;
                let by_model = || self.cfg.prices.get(&format!("anthropic/{}", e.model_name)).map(|p| cost(*p, u));
                e.notional_cost_usd = listed.or_else(by_model).unwrap_or(0.0);
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
    fn batches_are_capped_by_count_and_bytes() {
        assert_eq!(batch_len([100; 10].into_iter(), 4, 1 << 20), 4, "event cap");
        assert_eq!(batch_len([100; 10].into_iter(), 100, 350), 3, "byte cap");
        assert_eq!(batch_len([1000, 10].into_iter(), 100, 500), 1, "one oversized event still goes out alone");
        assert_eq!(batch_len(std::iter::empty(), 100, 500), 0);
    }

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
        let u = Usage { prompt: 1_000_000, completion: 500_000, cached: 400_000, ..Usage::default() };
        // 600k at $2 + 400k at $0.5 + 500k at $8
        assert!((price(&c, &d("a"), u).unwrap() - (1.2 + 0.2 + 4.0)).abs() < 1e-9);
        // no cached rate: all prompt tokens at the input rate
        assert!((price(&c, &d("b"), u).unwrap() - (1.0 + 1.0)).abs() < 1e-9);
        assert!(price(&c, &d("c"), u).is_none());
    }

    /// The same cases internal/pricing/pricing_test.go checks.
    #[test]
    fn cost_fixtures() {
        #[derive(serde::Deserialize, Default)]
        #[serde(default)]
        struct Tokens {
            prompt: i64,
            completion: i64,
            cached: i64,
            write_5m: i64,
            write_1h: i64,
            us: bool,
            fast: bool,
        }
        #[derive(serde::Deserialize)]
        struct Case {
            name: String,
            rates: Vec<f64>,
            #[serde(default)]
            long: Vec<f64>,
            tokens: Tokens,
            want: f64,
        }
        let raw = std::fs::read("../internal/pricing/testdata/cost.json").unwrap();
        let cases: Vec<Case> = serde_json::from_slice(&raw).unwrap();
        assert!(!cases.is_empty());
        for c in cases {
            let p = Price {
                input_per_1m: c.rates[0],
                output_per_1m: c.rates[1],
                cached_input_per_1m: c.rates.get(2).copied(),
                long_prompt_tokens: c.long.first().map_or(0, |t| *t as i64),
                long_prompt_multiplier: c.long.get(1).copied().unwrap_or(1.0),
            };
            let t = c.tokens;
            let got = cost(
                p,
                Usage {
                    prompt: t.prompt,
                    completion: t.completion,
                    cached: t.cached,
                    write_5m: t.write_5m,
                    write_1h: t.write_1h,
                    us_inference: t.us,
                    fast: t.fast,
                },
            );
            assert!((got - c.want).abs() < 1e-12, "{}: cost = {got}, want {}", c.name, c.want);
        }
    }
}
