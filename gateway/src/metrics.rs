//! Prometheus metrics, kept in atomics: no locks and no per-key or per-model
//! labels on the hot path.

use std::fmt::Write;
use std::sync::atomic::{AtomicI64, AtomicU64, Ordering::Relaxed};

pub static IN_FLIGHT: AtomicI64 = AtomicI64::new(0);
pub static SHED: AtomicU64 = AtomicU64::new(0);
pub static SYNC_FAILURES: AtomicU64 = AtomicU64::new(0);
pub static CONFIG_VERSION: AtomicI64 = AtomicI64::new(0);
pub static WORKERS: AtomicI64 = AtomicI64::new(0);

/// Routes the request counter is split by.
#[derive(Clone, Copy)]
pub enum Route {
    Chat,
    Completions,
    Embeddings,
    Messages,
    Models,
    CountTokens,
    Other,
}

const ROUTES: [&str; 7] =
    ["chat_completions", "completions", "embeddings", "messages", "models", "count_tokens", "other"];
const CLASSES: [&str; 5] = ["1xx", "2xx", "3xx", "4xx", "5xx"];
static REQUESTS: [[AtomicU64; 5]; 7] = [const { [const { AtomicU64::new(0) }; 5] }; 7];

pub fn request(route: Route, status: u16) {
    let class = ((status / 100) as usize).clamp(1, 5) - 1;
    REQUESTS[route as usize][class].fetch_add(1, Relaxed);
}

/// Outcomes of single upstream attempts.
#[derive(Clone, Copy)]
pub enum Attempt {
    Ok,
    Error,
    Timeout,
    Retryable,
}
const ATTEMPTS: [&str; 4] = ["ok", "error", "timeout", "retryable_status"];
static ATTEMPT: [AtomicU64; 4] = [const { AtomicU64::new(0) }; 4];

pub fn attempt(a: Attempt) {
    ATTEMPT[a as usize].fetch_add(1, Relaxed);
}

const DENIALS: [&str; 2] = ["budget_exceeded", "rate_limit_exceeded"];
static DENIAL: [AtomicU64; 2] = [const { AtomicU64::new(0) }; 2];

pub fn denial(code: &str) {
    if let Some(i) = DENIALS.iter().position(|d| *d == code) {
        DENIAL[i].fetch_add(1, Relaxed);
    }
}

/// Request latency buckets, in milliseconds.
const BUCKETS: [i64; 15] = [1, 2, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000];
static LATENCY: [AtomicU64; 16] = [const { AtomicU64::new(0) }; 16];
static LATENCY_SUM_MS: AtomicU64 = AtomicU64::new(0);

pub fn latency(ms: i64) {
    let i = BUCKETS.iter().position(|b| ms <= *b).unwrap_or(BUCKETS.len());
    LATENCY[i].fetch_add(1, Relaxed);
    LATENCY_SUM_MS.fetch_add(ms.max(0) as u64, Relaxed);
}

pub fn render(ready: bool, usage_queued: usize, usage_dropped: u64) -> String {
    let mut s = String::with_capacity(4096);
    let mut gauge = |name: &str, help: &str, v: i64| {
        let _ = writeln!(s, "# HELP {name} {help}\n# TYPE {name} gauge\n{name} {v}");
    };
    gauge("llmr_ready", "1 once the gateway has its config and has synced with the control plane", ready as i64);
    gauge("llmr_in_flight", "Requests being served", IN_FLIGHT.load(Relaxed));
    gauge("llmr_config_version", "Config version in use", CONFIG_VERSION.load(Relaxed));
    gauge("llmr_workers", "Worker threads", WORKERS.load(Relaxed));
    gauge("llmr_usage_queued", "Usage events waiting for the next sync", usage_queued as i64);

    let _ = writeln!(s, "# HELP llmr_requests_total Requests served\n# TYPE llmr_requests_total counter");
    for (r, route) in ROUTES.iter().enumerate() {
        for (c, class) in CLASSES.iter().enumerate() {
            let v = REQUESTS[r][c].load(Relaxed);
            if v > 0 {
                let _ = writeln!(s, "llmr_requests_total{{route=\"{route}\",code=\"{class}\"}} {v}");
            }
        }
    }
    let _ = writeln!(
        s,
        "# HELP llmr_upstream_attempts_total Upstream attempts by outcome\n# TYPE llmr_upstream_attempts_total counter"
    );
    for (i, a) in ATTEMPTS.iter().enumerate() {
        let _ = writeln!(s, "llmr_upstream_attempts_total{{result=\"{a}\"}} {}", ATTEMPT[i].load(Relaxed));
    }
    let _ = writeln!(
        s,
        "# HELP llmr_limit_denials_total Requests refused by a budget or rate limit\n# TYPE llmr_limit_denials_total counter"
    );
    for (i, d) in DENIALS.iter().enumerate() {
        let _ = writeln!(s, "llmr_limit_denials_total{{reason=\"{d}\"}} {}", DENIAL[i].load(Relaxed));
    }
    let mut counter = |name: &str, help: &str, v: u64| {
        let _ = writeln!(s, "# HELP {name} {help}\n# TYPE {name} counter\n{name} {v}");
    };
    counter("llmr_shed_total", "Requests refused because too many were in flight", SHED.load(Relaxed));
    counter("llmr_sync_failures_total", "Failed syncs with the control plane", SYNC_FAILURES.load(Relaxed));
    counter("llmr_usage_dropped_total", "Usage events dropped because the queue was full", usage_dropped);

    let _ = writeln!(
        s,
        "# HELP llmr_request_duration_seconds Request latency\n# TYPE llmr_request_duration_seconds histogram"
    );
    let mut cum = 0;
    for (i, b) in BUCKETS.iter().enumerate() {
        cum += LATENCY[i].load(Relaxed);
        let _ = writeln!(s, "llmr_request_duration_seconds_bucket{{le=\"{}\"}} {cum}", *b as f64 / 1000.0);
    }
    cum += LATENCY[BUCKETS.len()].load(Relaxed);
    let _ = writeln!(s, "llmr_request_duration_seconds_bucket{{le=\"+Inf\"}} {cum}");
    let _ = writeln!(s, "llmr_request_duration_seconds_sum {}", LATENCY_SUM_MS.load(Relaxed) as f64 / 1000.0);
    let _ = writeln!(s, "llmr_request_duration_seconds_count {cum}");
    s
}
