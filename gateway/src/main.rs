//! llmr-gateway: run with LLMR_CONTROL_PLANE_URL and LLMR_JOIN_TOKEN.

use std::sync::Arc;
use std::sync::atomic::Ordering;
use std::time::{Duration, Instant};

use llmr_gateway::cp::{ControlPlane, VERSION};
use llmr_gateway::metrics;
use llmr_gateway::server::{self, State};

#[global_allocator]
static GLOBAL: mimalloc::MiMalloc = mimalloc::MiMalloc;

fn env(name: &str) -> Option<String> {
    std::env::var(name).ok().filter(|v| !v.is_empty())
}

fn main() {
    let (Some(cp_url), Some(join_token)) = (env("LLMR_CONTROL_PLANE_URL"), env("LLMR_JOIN_TOKEN")) else {
        eprintln!(
            "llmr-gateway {VERSION}: set LLMR_CONTROL_PLANE_URL and LLMR_JOIN_TOKEN (a gateway group's join token)"
        );
        std::process::exit(2);
    };
    let listen = env("LLMR_LISTEN").unwrap_or_else(|| "0.0.0.0:8080".into());
    // available_parallelism honours the container's CPU quota.
    let workers = env("LLMR_WORKERS")
        .and_then(|v| v.parse().ok())
        .unwrap_or_else(|| std::thread::available_parallelism().map(|n| n.get()).unwrap_or(1));
    let max_in_flight = env("LLMR_MAX_IN_FLIGHT").and_then(|v| v.parse().ok()).unwrap_or(10_000);
    let drain = Duration::from_secs(env("LLMR_SHUTDOWN_TIMEOUT").and_then(|v| v.parse().ok()).unwrap_or(30));
    let name = env("LLMR_GATEWAY_NAME")
        .or_else(|| env("HOSTNAME"))
        .or_else(|| std::fs::read_to_string("/etc/hostname").ok().map(|s| s.trim().to_string()))
        .unwrap_or_else(|| "gateway".into());
    let _ = rustls::crypto::ring::default_provider().install_default();
    metrics::WORKERS.store(workers as i64, Ordering::Relaxed);

    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(workers)
        // Blocking work is a sync body's encoding and the odd file or DNS
        // lookup. Every blocking thread keeps its own allocator heap, so a
        // burst must not start hundreds of them.
        .max_blocking_threads(16)
        .enable_all()
        .build()
        .expect("tokio runtime");
    rt.block_on(async move {
        let st: &'static State = Box::leak(Box::new(State {
            config: Default::default(),
            ready: Default::default(),
            draining: Default::default(),
            max_in_flight,
        }));
        let cp = Arc::new(ControlPlane::new(&cp_url, join_token, name));
        tokio::spawn(cp.clone().run(st));

        let listener = tokio::net::TcpListener::bind(&listen).await.unwrap_or_else(|e| {
            eprintln!("listen on {listen}: {e}");
            std::process::exit(1);
        });
        eprintln!("llmr-gateway {VERSION} listening on {listen} with {workers} workers");
        let shutdown = async {
            let mut term =
                tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate()).expect("signal handler");
            tokio::select! {
                _ = term.recv() => {}
                _ = tokio::signal::ctrl_c() => {}
            }
        };
        // Drain: report not ready so load balancers move on, close connections
        // as their requests finish, then upload the last usage.
        server::run(listener, st, shutdown, drain).await;
        cp.flush(Instant::now() + Duration::from_secs(10)).await;
        eprintln!("llmr-gateway stopped");
    });
}
