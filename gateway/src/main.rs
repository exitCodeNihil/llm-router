//! llmr-gateway: run with LLMR_CONTROL_PLANE_URL and LLMR_JOIN_TOKEN.

use std::sync::Arc;
use std::sync::atomic::Ordering;
use std::time::{Duration, Instant};

use hyper::service::service_fn;
use hyper_util::rt::{TokioExecutor, TokioIo, TokioTimer};
use hyper_util::server::conn::auto;
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
        let mut builder = auto::Builder::new(TokioExecutor::new());
        builder.http1().timer(TokioTimer::new()).header_read_timeout(Duration::from_secs(10));
        builder.http2().timer(TokioTimer::new());
        let builder = Arc::new(builder);

        let mut term =
            tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate()).expect("signal handler");
        loop {
            tokio::select! {
                accepted = listener.accept() => {
                    let Ok((stream, _)) = accepted else { continue };
                    let _ = stream.set_nodelay(true);
                    let builder = builder.clone();
                    tokio::spawn(async move {
                        let svc = service_fn(move |req| server::handle(st, req));
                        let _ = builder.serve_connection(TokioIo::new(stream), svc).await;
                    });
                }
                _ = term.recv() => break,
                _ = tokio::signal::ctrl_c() => break,
            }
        }
        // Drain: report not ready so load balancers move on, let requests in
        // flight finish, then upload the last usage.
        st.draining.store(true, Ordering::Relaxed);
        drop(listener);
        let deadline = Instant::now() + drain;
        while metrics::IN_FLIGHT.load(Ordering::Relaxed) > 0 && Instant::now() < deadline {
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
        cp.flush(Instant::now() + Duration::from_secs(10)).await;
        eprintln!("llmr-gateway stopped");
    });
}
