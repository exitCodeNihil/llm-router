//! llm-router's data-plane gateway. It joins a control plane with a gateway
//! group's token, serves /v1 from the config it gets back, and syncs usage and
//! limit counters with the control plane every couple of seconds.

pub mod body;
pub mod config;
pub mod cp;
pub mod health;
pub mod limits;
pub mod metrics;
pub mod server;
pub mod translate;
pub mod upstream;
pub mod usage;

/// Wall-clock milliseconds since the Unix epoch.
pub fn now_ms() -> i64 {
    std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).map(|d| d.as_millis() as i64).unwrap_or(0)
}
