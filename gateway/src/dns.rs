//! A caching resolver for upstream connections. hyper's default resolver runs
//! every lookup on a blocking thread: a burst of new connections started a
//! thread each, up to tokio's 512, and each thread kept its own allocator heap.
//! Here one lookup per host runs at a time, and its answer serves the rest.

use std::collections::HashMap;
use std::future::Future;
use std::net::SocketAddr;
use std::pin::Pin;
use std::sync::{Arc, Mutex};
use std::task::{Context, Poll};
use std::time::{Duration, Instant};

use hyper_util::client::legacy::connect::dns::Name;

/// How long an answer is used before it is looked up again.
const TTL: Duration = Duration::from_secs(30);
/// How long the last answer may stand in when a fresh lookup fails.
const STALE: Duration = Duration::from_secs(300);

type Answer = Option<(Arc<[SocketAddr]>, Instant)>;

#[derive(Clone, Default)]
pub struct Resolver {
    // Hosts come from the config, so the map stays small.
    hosts: Arc<Mutex<HashMap<String, Arc<tokio::sync::Mutex<Answer>>>>>,
}

impl tower_service::Service<Name> for Resolver {
    type Response = Addrs;
    type Error = std::io::Error;
    type Future = Pin<Box<dyn Future<Output = Result<Self::Response, Self::Error>> + Send>>;

    fn poll_ready(&mut self, _: &mut Context<'_>) -> Poll<Result<(), Self::Error>> {
        Poll::Ready(Ok(()))
    }

    fn call(&mut self, name: Name) -> Self::Future {
        let slot = self.hosts.lock().unwrap().entry(name.as_str().to_owned()).or_default().clone();
        Box::pin(async move {
            // Callers for the same host queue here behind one lookup.
            let mut answer = slot.lock().await;
            if let Some((addrs, at)) = &*answer
                && at.elapsed() < TTL
            {
                return Ok(Addrs::new(addrs));
            }
            let looked_up = tokio::net::lookup_host((name.as_str(), 0)).await.and_then(|it| {
                let addrs: Arc<[SocketAddr]> = it.collect();
                if addrs.is_empty() { Err(std::io::Error::other(format!("{name}: no addresses"))) } else { Ok(addrs) }
            });
            match looked_up {
                Ok(addrs) => {
                    *answer = Some((addrs.clone(), Instant::now()));
                    Ok(Addrs::new(&addrs))
                }
                Err(e) => match &*answer {
                    Some((addrs, at)) if at.elapsed() < STALE => Ok(Addrs::new(addrs)),
                    _ => Err(e),
                },
            }
        })
    }
}

/// The addresses of one answer, shared with the cache rather than copied.
pub struct Addrs {
    list: Arc<[SocketAddr]>,
    next: usize,
}

impl Addrs {
    fn new(list: &Arc<[SocketAddr]>) -> Self {
        Addrs { list: list.clone(), next: 0 }
    }
}

impl Iterator for Addrs {
    type Item = SocketAddr;

    fn next(&mut self) -> Option<SocketAddr> {
        let a = self.list.get(self.next).copied();
        self.next += 1;
        a
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::str::FromStr;
    use tower_service::Service;

    #[tokio::test]
    async fn resolves_and_reuses_the_answer() {
        let mut r = Resolver::default();
        let first: Vec<_> = r.call(Name::from_str("localhost").unwrap()).await.unwrap().collect();
        assert!(!first.is_empty());
        let slot = r.hosts.lock().unwrap()["localhost"].clone();
        let at = slot.lock().await.as_ref().unwrap().1;
        let again: Vec<_> = r.call(Name::from_str("localhost").unwrap()).await.unwrap().collect();
        assert_eq!(first, again);
        // served from the cache: the answer's time did not move
        assert_eq!(slot.lock().await.as_ref().unwrap().1, at);
    }

    #[tokio::test]
    async fn a_failed_lookup_serves_the_last_answer_while_it_is_recent() {
        let mut r = Resolver::default();
        let name = "no-such-host.invalid";
        let addr: SocketAddr = "192.0.2.1:0".parse().unwrap();
        let slot = r.hosts.lock().unwrap().entry(name.into()).or_default().clone();
        *slot.lock().await = Some((Arc::from([addr]), Instant::now() - TTL));
        let got: Vec<_> = r.call(Name::from_str(name).unwrap()).await.unwrap().collect();
        assert_eq!(got, vec![addr]);
        *slot.lock().await = Some((Arc::from([addr]), Instant::now() - STALE));
        assert!(r.call(Name::from_str(name).unwrap()).await.is_err());
    }
}
