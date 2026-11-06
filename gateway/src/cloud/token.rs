//! A short-lived bearer token kept fresh: renewed in the background about halfway
//! through its life, waited for only when the current one is about to expire, and
//! fetched again after a failure only once a backoff has passed. Anthropic workload
//! identity, Entra ID and Google all hand out tokens like this; what differs is how
//! a new one is got, which is the `TokenSource`.

use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;
use std::sync::OnceLock;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{Duration, Instant};

use arc_swap::ArcSwapOption;
use http::HeaderValue;

/// Renewal starts about halfway through a token's life, in the background, so an
/// outage of the token service shorter than that costs nothing; and always this
/// long before expiry at the latest.
pub(crate) const ADVISORY_REFRESH: Duration = Duration::from_secs(120);
/// Too close to expiry to send: requests wait for a new token instead.
pub(crate) const MANDATORY_REFRESH: Duration = Duration::from_secs(30);
/// A failed fetch isn't retried on every request.
pub(crate) const RETRY_AFTER: Duration = Duration::from_secs(5);
pub(crate) const MAX_RETRY_AFTER: Duration = Duration::from_secs(30);

/// Where new tokens come from.
pub trait TokenSource: Send + Sync + 'static {
    /// What a failure is logged as, e.g. "entra token request".
    fn what(&self) -> &str;
    /// Gets a new token. `usable` says the current one still works, so the source
    /// may answer `NotYet` when it has nothing new to offer.
    fn fetch(&self, usable: bool) -> impl Future<Output = Result<Fetched, FetchError>> + Send;
}

/// Why a token couldn't be got. The request that needed it is told `public`;
/// the log gets `detail`, which may name credential files or carry what the
/// token endpoint answered.
#[derive(Debug)]
pub struct FetchError {
    pub public: String,
    pub detail: String,
}

impl FetchError {
    pub fn new(public: impl Into<String>, detail: impl Into<String>) -> FetchError {
        FetchError { public: public.into(), detail: detail.into() }
    }

    /// Both, after `prefix: `.
    pub fn context(self, prefix: &str) -> FetchError {
        FetchError { public: format!("{prefix}: {}", self.public), detail: format!("{prefix}: {}", self.detail) }
    }
}

/// An error that is safe to show as it is.
impl From<String> for FetchError {
    fn from(s: String) -> FetchError {
        FetchError { public: s.clone(), detail: s }
    }
}

pub enum Fetched {
    /// An Authorization header value and how long it lasts.
    Token(HeaderValue, Duration),
    /// Nothing new yet: keep the current token and ask again in a while.
    NotYet,
}

/// The Authorization header for an upstream request, for whatever kind of
/// credential a provider uses.
pub trait Bearer: Send + Sync {
    fn bearer(&self) -> Pin<Box<dyn Future<Output = Result<HeaderValue, String>> + Send + '_>>;
}

/// A credential that can't work, with the reason every request gets.
pub struct Refused(pub String);

impl Bearer for Refused {
    fn bearer(&self) -> Pin<Box<dyn Future<Output = Result<HeaderValue, String>> + Send + '_>> {
        Box::pin(std::future::ready(Err(self.0.clone())))
    }
}

pub(crate) struct Token {
    pub(crate) header: HeaderValue,
    pub(crate) expires: Instant,
    pub(crate) refresh_at: Instant,
}

impl Token {
    fn new(mut header: HeaderValue, lifetime: Duration) -> Token {
        header.set_sensitive(true);
        let now = Instant::now();
        Token { header, expires: now + lifetime, refresh_at: now + refresh_after(lifetime) }
    }
}

/// Half the lifetime, give or take a tenth, so gateways that started together
/// don't all renew in the same second.
pub(crate) fn refresh_after(lifetime: Duration) -> Duration {
    let spread = lifetime / 5;
    lifetime / 2 - lifetime / 10 + spread.mul_f64(fastrand::f64())
}

/// Doubles from RETRY_AFTER up to MAX_RETRY_AFTER, with jitter.
pub(crate) fn backoff(failures: u32) -> Duration {
    let d = (RETRY_AFTER * 2u32.pow(failures.saturating_sub(1).min(4))).min(MAX_RETRY_AFTER);
    d / 2 + (d / 2).mul_f64(fastrand::f64())
}

/// Instants as milliseconds since the process started, for an atomic.
pub(crate) fn millis(t: Instant) -> u64 {
    static START: OnceLock<Instant> = OnceLock::new();
    t.saturating_duration_since(*START.get_or_init(Instant::now)).as_millis() as u64
}

#[derive(Default)]
struct Refresh {
    failures: u32,
    last_err: Option<String>,
}

struct Inner<S> {
    source: S,
    current: ArcSwapOption<Token>,
    /// No fetch before this (see `millis`).
    next_try: AtomicU64,
    /// One fetch at a time.
    refresh: Arc<tokio::sync::Mutex<Refresh>>,
}

pub struct RefreshingToken<S>(Arc<Inner<S>>);

impl<S> Clone for RefreshingToken<S> {
    fn clone(&self) -> Self {
        RefreshingToken(self.0.clone())
    }
}

impl<S: TokenSource> RefreshingToken<S> {
    pub fn new(source: S) -> Self {
        RefreshingToken(Arc::new(Inner {
            source,
            current: ArcSwapOption::empty(),
            next_try: AtomicU64::new(0),
            refresh: Default::default(),
        }))
    }

    pub fn source(&self) -> &S {
        &self.0.source
    }

    /// The Authorization header for the next upstream request.
    pub async fn bearer(&self) -> Result<HeaderValue, String> {
        let now = Instant::now();
        if let Some(t) = self.0.current.load_full()
            && t.expires.saturating_duration_since(now) > MANDATORY_REFRESH
        {
            let due = now >= t.refresh_at || t.expires.saturating_duration_since(now) <= ADVISORY_REFRESH;
            if due
                && millis(now) >= self.0.next_try.load(Ordering::Relaxed)
                && let Ok(mut guard) = self.0.refresh.clone().try_lock_owned()
            {
                let inner = self.0.clone();
                tokio::spawn(async move {
                    let _ = refresh(&inner, &mut guard).await;
                });
            }
            return Ok(t.header.clone());
        }
        // Nothing usable: this request waits for a fetch.
        let mut guard = self.0.refresh.lock().await;
        refresh(&self.0, &mut guard).await
    }

    /// Waits for a background renewal to finish.
    #[cfg(test)]
    pub(crate) async fn idle(&self) {
        let _ = self.0.refresh.lock().await;
    }

    /// Replaces the current token: `refresh_at` defaults to its expiry.
    #[cfg(test)]
    pub(crate) fn set_current(&self, header: &str, expires: Duration, refresh_at: Option<Instant>) {
        let now = Instant::now();
        self.0.current.store(Some(Arc::new(Token {
            header: HeaderValue::from_str(header).unwrap(),
            expires: now + expires,
            refresh_at: refresh_at.unwrap_or(now + expires),
        })));
    }
}

impl<S: TokenSource> Bearer for RefreshingToken<S> {
    fn bearer(&self) -> Pin<Box<dyn Future<Output = Result<HeaderValue, String>> + Send + '_>> {
        Box::pin(RefreshingToken::bearer(self))
    }
}

/// Gets a new token if one is due and allowed, and returns the header to send.
async fn refresh<S: TokenSource>(inner: &Inner<S>, r: &mut Refresh) -> Result<HeaderValue, String> {
    let now = Instant::now();
    let cur = inner.current.load_full();
    let left = cur.as_ref().map_or(Duration::ZERO, |t| t.expires.saturating_duration_since(now));
    let usable = left > MANDATORY_REFRESH;
    if let Some(t) = &cur
        && usable
        && now < t.refresh_at
        && left > ADVISORY_REFRESH
    {
        return Ok(t.header.clone()); // another fetch just finished
    }
    // Backing off: after a failure, or while the source has nothing new. Once the
    // current token is too close to expiry, only a failure stops a try.
    if millis(now) < inner.next_try.load(Ordering::Relaxed) && (usable || r.last_err.is_some()) {
        return match (&cur, usable) {
            (Some(t), true) => Ok(t.header.clone()),
            _ => Err(r.last_err.clone().unwrap_or_default()),
        };
    }
    let fetched = inner.source.fetch(usable).await;
    match fetched {
        Ok(Fetched::Token(header, lifetime)) => {
            let t = Token::new(header, lifetime);
            let header = t.header.clone();
            inner.current.store(Some(Arc::new(t)));
            *r = Refresh::default();
            inner.next_try.store(0, Ordering::Relaxed);
            Ok(header)
        }
        Ok(Fetched::NotYet) => {
            inner.next_try.store(millis(now + RETRY_AFTER), Ordering::Relaxed);
            match (cur, usable) {
                (Some(t), true) => Ok(t.header.clone()),
                _ => Err(format!("{}: no new token yet", inner.source.what())),
            }
        }
        Err(FetchError { public: message, detail }) => {
            eprintln!("{} failed: {detail}", inner.source.what());
            r.failures += 1;
            r.last_err = Some(message.clone());
            inner.next_try.store(millis(now + backoff(r.failures)), Ordering::Relaxed);
            match (cur, usable) {
                (Some(t), true) => Ok(t.header.clone()),
                _ => Err(message),
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use std::sync::Mutex;
    use std::sync::atomic::AtomicUsize;

    use super::*;

    /// Answers each fetch with the next reply: a lifetime in seconds, or an error.
    struct Fake {
        replies: Vec<Result<u64, &'static str>>,
        calls: AtomicUsize,
        usable: Mutex<Vec<bool>>,
    }

    impl Fake {
        fn token(replies: Vec<Result<u64, &'static str>>) -> RefreshingToken<Fake> {
            RefreshingToken::new(Fake { replies, calls: AtomicUsize::new(0), usable: Default::default() })
        }
    }

    impl TokenSource for Fake {
        fn what(&self) -> &str {
            "fake token request"
        }
        async fn fetch(&self, usable: bool) -> Result<Fetched, FetchError> {
            self.usable.lock().unwrap().push(usable);
            let n = self.calls.fetch_add(1, Ordering::SeqCst);
            match self.replies[n.min(self.replies.len() - 1)] {
                Ok(secs) => Ok(Fetched::Token(
                    HeaderValue::from_str(&format!("Bearer t{n}")).unwrap(),
                    Duration::from_secs(secs),
                )),
                Err(e) => Err(FetchError::new(e, format!("{e} (with detail)"))),
            }
        }
    }

    fn calls(t: &RefreshingToken<Fake>) -> usize {
        t.source().calls.load(Ordering::SeqCst)
    }

    #[tokio::test]
    async fn fetches_once_and_reuses() {
        let t = Fake::token(vec![Ok(3600)]);
        assert_eq!(t.bearer().await.unwrap(), "Bearer t0");
        assert_eq!(t.bearer().await.unwrap(), "Bearer t0");
        assert_eq!(calls(&t), 1);
    }

    #[tokio::test]
    async fn renews_at_half_life_without_making_the_request_wait() {
        let t = Fake::token(vec![Ok(3600)]);
        t.bearer().await.unwrap();
        t.set_current("old", Duration::from_secs(1800), Some(Instant::now() - Duration::from_secs(1)));
        assert_eq!(t.bearer().await.unwrap(), "old");
        t.idle().await;
        assert_eq!(t.bearer().await.unwrap(), "Bearer t1");
        assert_eq!(*t.source().usable.lock().unwrap(), [false, true]);
    }

    #[tokio::test]
    async fn waits_when_near_expiry() {
        let t = Fake::token(vec![Ok(10), Ok(3600)]);
        assert_eq!(t.bearer().await.unwrap(), "Bearer t0");
        // 10 s left: too close to send, so this request waits for a new one.
        assert_eq!(t.bearer().await.unwrap(), "Bearer t1");
        assert_eq!(calls(&t), 2);
    }

    #[tokio::test]
    async fn a_failure_shows_the_request_no_detail() {
        let t = Fake::token(vec![Err("refused")]);
        assert_eq!(t.bearer().await.unwrap_err(), "refused");
        assert_eq!(t.bearer().await.unwrap_err(), "refused", "nor while backing off");
    }

    #[tokio::test]
    async fn backs_off_after_a_failure() {
        let t = Fake::token(vec![Err("refused"), Ok(3600)]);
        assert_eq!(t.bearer().await.unwrap_err(), "refused");
        // Within the backoff: the last error, without asking again.
        assert_eq!(t.bearer().await.unwrap_err(), "refused");
        assert_eq!(calls(&t), 1);
        t.0.next_try.store(0, Ordering::Relaxed);
        assert_eq!(t.bearer().await.unwrap(), "Bearer t1");
    }

    #[tokio::test]
    async fn advisory_failure_keeps_serving_the_token() {
        let t = Fake::token(vec![Ok(100), Err("down")]);
        t.bearer().await.unwrap();
        for _ in 0..2 {
            // 100 s left: tried once in the background, failed, then not again at once
            assert_eq!(t.bearer().await.unwrap(), "Bearer t0");
            t.idle().await;
            assert_eq!(calls(&t), 2);
        }
    }

    #[tokio::test]
    async fn refused_always_explains() {
        let r: Arc<dyn Bearer> = Arc::new(Refused("no way".into()));
        assert_eq!(r.bearer().await.unwrap_err(), "no way");
    }

    #[test]
    fn refresh_timing_and_backoff() {
        for _ in 0..200 {
            let d = refresh_after(Duration::from_secs(3600));
            assert!(d >= Duration::from_secs(24 * 60) && d <= Duration::from_secs(36 * 60), "{d:?}");
        }
        for n in 1..=8 {
            let d = backoff(n);
            assert!(d >= RETRY_AFTER / 2 && d <= MAX_RETRY_AFTER, "backoff({n}) = {d:?}");
            if n >= 5 {
                assert!(d >= MAX_RETRY_AFTER / 2, "backoff({n}) = {d:?} should reach the cap");
            }
        }
    }
}
