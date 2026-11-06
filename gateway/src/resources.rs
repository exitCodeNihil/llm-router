//! What the gateway uses of its container: memory against the cgroup v2 limit,
//! and CPU against its CPU allowance. Past saturation the gateway's memory
//! grows with open connections, so memory against the limit is the number
//! that says how close a replica is to being killed.

use std::sync::{Mutex, OnceLock};
use std::time::{Duration, Instant};

pub struct Memory {
    /// The cgroup's working set (memory.current less inactive page cache,
    /// what the kubelet and `docker stats` report), or the process's RSS
    /// outside a cgroup v2.
    pub used: u64,
    /// memory.max; None when there is no limit.
    pub limit: Option<u64>,
}

/// The cgroup v2 directory of this process, if there is one.
fn cgroup_dir() -> Option<&'static str> {
    static DIR: OnceLock<Option<String>> = OnceLock::new();
    DIR.get_or_init(|| {
        let own = std::fs::read_to_string("/proc/self/cgroup").ok().and_then(|s| {
            s.lines().find_map(|l| l.strip_prefix("0::").map(|p| format!("/sys/fs/cgroup{}", p.trim_end_matches('/'))))
        });
        // With a cgroup namespace the path is "/", and without one the
        // container may still see only its own cgroup at the root.
        [own, Some("/sys/fs/cgroup".to_string())]
            .into_iter()
            .flatten()
            .find(|d| std::path::Path::new(&format!("{d}/memory.current")).exists())
    })
    .as_deref()
}

fn read(path: &str) -> Option<String> {
    std::fs::read_to_string(path).ok()
}

/// A cgroup value file: a number, or "max" for none.
fn number(s: &str) -> Option<u64> {
    s.trim().parse().ok()
}

/// A field of a "key value" file such as memory.stat or cpu.stat.
fn field(s: &str, key: &str) -> Option<u64> {
    s.lines().find_map(|l| l.strip_prefix(key)?.strip_prefix(' ')?.trim().parse().ok())
}

pub fn memory() -> Memory {
    if let Some(dir) = cgroup_dir()
        && let Some(current) = read(&format!("{dir}/memory.current")).as_deref().and_then(number)
    {
        let inactive = read(&format!("{dir}/memory.stat")).and_then(|s| field(&s, "inactive_file")).unwrap_or(0);
        let limit = read(&format!("{dir}/memory.max")).as_deref().and_then(number);
        return Memory { used: current.saturating_sub(inactive), limit };
    }
    let rss = read("/proc/self/status").and_then(|s| vm_rss(&s)).unwrap_or(0);
    Memory { used: rss, limit: None }
}

/// VmRSS from /proc/self/status, in bytes (the file counts kB, so no page
/// size is needed).
fn vm_rss(status: &str) -> Option<u64> {
    let line = status.lines().find(|l| l.starts_with("VmRSS:"))?;
    let kb: u64 = line.split_whitespace().nth(1)?.parse().ok()?;
    Some(kb * 1024)
}

/// CPU time used so far, in seconds: the cgroup's, or the process's.
fn cpu_seconds() -> Option<f64> {
    if let Some(dir) = cgroup_dir()
        && let Some(us) = read(&format!("{dir}/cpu.stat")).and_then(|s| field(&s, "usage_usec"))
    {
        return Some(us as f64 / 1e6);
    }
    read("/proc/self/stat").and_then(|s| proc_stat_ticks(&s)).map(|t| t as f64 / 100.0)
}

/// utime + stime from /proc/self/stat, in clock ticks (100 a second on Linux).
fn proc_stat_ticks(stat: &str) -> Option<u64> {
    // The command name may hold spaces; fields count on from after its ')'.
    let rest = &stat[stat.rfind(')')? + 1..];
    let f: Vec<&str> = rest.split_whitespace().collect();
    Some(f.get(11)?.parse::<u64>().ok()? + f.get(12)?.parse::<u64>().ok()?)
}

/// The CPUs this gateway may use: cpu.max's quota, else the CPUs it sees.
fn cpu_allowance() -> f64 {
    if let Some(dir) = cgroup_dir()
        && let Some(cores) = read(&format!("{dir}/cpu.max")).and_then(|s| cpu_max(&s))
    {
        return cores;
    }
    std::thread::available_parallelism().map(|n| n.get() as f64).unwrap_or(1.0)
}

fn cpu_max(s: &str) -> Option<f64> {
    let mut it = s.split_whitespace();
    let quota: f64 = it.next()?.parse().ok()?;
    let period: f64 = it.next()?.parse().ok()?;
    (period > 0.0).then_some(quota / period)
}

/// CPU use as a percentage of the allowance, averaged over at least the last
/// five seconds. 0 where it can't be read (not Linux).
pub fn cpu_pct() -> f64 {
    static LAST: Mutex<Option<(Instant, f64, f64)>> = Mutex::new(None);
    let mut last = LAST.lock().unwrap();
    let now = Instant::now();
    let Some(used) = cpu_seconds() else { return 0.0 };
    match *last {
        Some((at, _, pct)) if now.duration_since(at) < Duration::from_secs(5) => pct,
        Some((at, before, _)) => {
            let wall = now.duration_since(at).as_secs_f64();
            let pct = ((used - before) / wall / cpu_allowance() * 100.0).clamp(0.0, 100.0);
            *last = Some((now, used, pct));
            pct
        }
        None => {
            *last = Some((now, used, 0.0));
            0.0
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_cgroup_and_proc_files() {
        assert_eq!(number("536870912\n"), Some(536870912));
        assert_eq!(number("max\n"), None);
        let stat = "anon 1000\nfile 900\nactive_file 100\ninactive_file 800\n";
        assert_eq!(field(stat, "inactive_file"), Some(800));
        assert_eq!(field(stat, "file"), Some(900));
        assert_eq!(field("usage_usec 1500000\nuser_usec 1\n", "usage_usec"), Some(1_500_000));
        assert_eq!(vm_rss("Name:\tx\nVmRSS:\t  2048 kB\n"), Some(2 * 1024 * 1024));
        let ps = "42 (llmr gw) S 1 42 42 0 -1 4194560 100 0 0 0 250 50 0 0 20 0 8 0 1 2 3";
        assert_eq!(proc_stat_ticks(ps), Some(300));
        assert_eq!(cpu_max("200000 100000\n"), Some(2.0));
        assert_eq!(cpu_max("max 100000\n"), None);
    }

    #[test]
    fn reads_something_or_nothing() {
        let m = memory();
        if cfg!(target_os = "linux") {
            assert!(m.used > 0);
        }
        assert!((0.0..=100.0).contains(&cpu_pct()));
    }
}
