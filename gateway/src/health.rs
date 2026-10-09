//! Upstream cooldown (internal/gateway/health.go): a deployment that fails is
//! tried last for 30 s, doubling per consecutive failure up to 5 minutes, or for
//! as long as the upstream's Retry-After asks if that is longer. State lives
//! in atomics on an Arc each config version shares, so the hot path takes no
//! lock and a config reload doesn't forget who is down.

use std::collections::HashMap;
use std::sync::atomic::{AtomicI64, AtomicU32, Ordering};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::Duration;

use crate::now_ms;

const BASE: Duration = Duration::from_secs(30);
const MAX: Duration = Duration::from_secs(300);

#[derive(Default)]
pub struct Health {
    failures: AtomicU32,
    until_ms: AtomicI64,
}

impl Health {
    pub fn cooling(&self, now_ms: i64) -> bool {
        self.until_ms.load(Ordering::Relaxed) > now_ms
    }

    pub fn ok(&self) {
        if self.failures.load(Ordering::Relaxed) != 0 {
            self.failures.store(0, Ordering::Relaxed);
            self.until_ms.store(0, Ordering::Relaxed);
        }
    }

    pub fn fail(&self, retry_after: Option<Duration>) {
        let n = self.failures.fetch_add(1, Ordering::Relaxed) + 1;
        let mut d = if n > 4 { MAX } else { (BASE * (1 << (n - 1))).min(MAX) };
        if let Some(ra) = retry_after
            && ra > d
        {
            d = ra;
        }
        self.until_ms.store(now_ms() + d.as_millis() as i64, Ordering::Relaxed);
    }
}

fn registry() -> &'static Mutex<HashMap<String, Arc<Health>>> {
    static R: OnceLock<Mutex<HashMap<String, Arc<Health>>>> = OnceLock::new();
    R.get_or_init(Default::default)
}

/// The health record for a deployment id, shared across config versions.
pub fn get(id: &str) -> Arc<Health> {
    registry().lock().unwrap().entry(id.to_string()).or_default().clone()
}

/// Forget deployments no longer in the config.
pub fn retain(ids: &std::collections::HashSet<&str>) {
    registry().lock().unwrap().retain(|id, _| ids.contains(id.as_str()));
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn backoff_and_retry_after() {
        let h = Health::default();
        let now = now_ms();
        assert!(!h.cooling(now));
        h.fail(None);
        assert!(h.cooling(now + 29_000) && !h.cooling(now + 31_000));
        h.fail(None);
        assert!(h.cooling(now + 59_000));
        h.fail(Some(Duration::from_secs(3600)));
        assert!(h.cooling(now + 3_500_000));
        h.ok();
        assert!(!h.cooling(now));
    }
}
