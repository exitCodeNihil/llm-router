//! Anthropic workload identity federation: present an identity token from this
//! machine's platform and get a short-lived Anthropic token back, so no static key
//! exists to leak (Go: internal/provider/anthropic_wif.go). Each gateway exchanges
//! its own token; the control plane sends only the rule's IDs.

use std::collections::HashMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::{Duration, Instant};

use arc_swap::ArcSwapOption;
use bytes::Bytes;
use http::{HeaderValue, Request, StatusCode, header};
use http_body_util::{BodyExt, Full, Limited};
use serde::Deserialize;

/// Renewal starts about halfway through a token's life, in the background, so an
/// outage of the identity provider or Anthropic shorter than that costs nothing;
/// and always this long before expiry at the latest.
const ADVISORY_REFRESH: Duration = Duration::from_secs(120);
/// Too close to expiry to send: requests wait for a new token instead.
const MANDATORY_REFRESH: Duration = Duration::from_secs(30);
/// A refused exchange isn't retried on every request: a token with a jti claim is
/// single use, and the file has to rotate before another try can work.
const RETRY_AFTER: Duration = Duration::from_secs(5);
const MAX_RETRY_AFTER: Duration = Duration::from_secs(30);
const EXCHANGE_TIMEOUT: Duration = Duration::from_secs(15);
const MAX_ASSERTION: usize = 16 << 10; // Anthropic's limit

#[derive(Debug, Clone, Default, Deserialize, PartialEq, Eq, Hash)]
pub struct WifConfig {
    pub federation_rule_id: String,
    pub organization_id: String,
    pub service_account_id: String,
    #[serde(default)]
    pub workspace_id: String,
    #[serde(default)]
    pub identity_token_file: String,
}

struct Token {
    header: HeaderValue,
    expires: Instant,
    refresh_at: Instant,
}

impl Token {
    fn new(header: HeaderValue, lifetime: Duration) -> Token {
        let now = Instant::now();
        Token { header, expires: now + lifetime, refresh_at: now + refresh_after(lifetime) }
    }
}

/// Half the lifetime, give or take a tenth, so gateways that started together
/// don't all renew in the same second.
fn refresh_after(lifetime: Duration) -> Duration {
    let spread = lifetime / 5;
    lifetime / 2 - lifetime / 10 + spread.mul_f64(fastrand::f64())
}

/// Doubles from RETRY_AFTER up to MAX_RETRY_AFTER, with jitter.
fn backoff(failures: u32) -> Duration {
    let d = (RETRY_AFTER * 2u32.pow(failures.saturating_sub(1).min(4))).min(MAX_RETRY_AFTER);
    d / 2 + (d / 2).mul_f64(fastrand::f64())
}

/// Instants as milliseconds since the process started, for an atomic.
fn millis(t: Instant) -> u64 {
    static START: OnceLock<Instant> = OnceLock::new();
    t.saturating_duration_since(*START.get_or_init(Instant::now)).as_millis() as u64
}

#[derive(Default)]
struct Refresh {
    failures: u32,
    last_err: Option<String>,
    /// A hash of the identity token behind `current`.
    used: Option<u64>,
}

pub struct Wif {
    cfg: WifConfig,
    base_url: String,
    source: Source,
    current: ArcSwapOption<Token>,
    /// No exchange before this (see `millis`).
    next_try: AtomicU64,
    /// One exchange at a time.
    refresh: Arc<tokio::sync::Mutex<Refresh>>,
}

/// Outlives config reloads: a reload builds new providers, and exchanging again
/// for each one would burn single-use identity tokens. Providers with the same
/// settings share a token for the same reason.
pub fn state(base_url: &str, cfg: WifConfig) -> Arc<Wif> {
    type Key = (String, WifConfig);
    static STATES: OnceLock<Mutex<HashMap<Key, Arc<Wif>>>> = OnceLock::new();
    let key = (base_url.to_string(), cfg.clone());
    let mut states = STATES.get_or_init(Default::default).lock().unwrap();
    states.entry(key).or_insert_with(|| Arc::new(Wif::new(base_url, cfg, Source::from_env()))).clone()
}

impl Wif {
    fn new(base_url: &str, cfg: WifConfig, source: Source) -> Wif {
        Wif {
            cfg,
            base_url: base_url.to_string(),
            source,
            current: ArcSwapOption::empty(),
            next_try: AtomicU64::new(0),
            refresh: Default::default(),
        }
    }

    /// The Authorization header for the next upstream request.
    pub async fn bearer(self: &Arc<Self>) -> Result<HeaderValue, String> {
        let now = Instant::now();
        if let Some(t) = self.current.load_full()
            && t.expires.saturating_duration_since(now) > MANDATORY_REFRESH
        {
            let due = now >= t.refresh_at || t.expires.saturating_duration_since(now) <= ADVISORY_REFRESH;
            if due
                && millis(now) >= self.next_try.load(Ordering::Relaxed)
                && let Ok(mut guard) = self.refresh.clone().try_lock_owned()
            {
                let me = self.clone();
                tokio::spawn(async move {
                    let _ = me.exchange(&mut guard).await;
                });
            }
            return Ok(t.header.clone());
        }
        // Nothing usable: this request waits for an exchange.
        let mut guard = self.refresh.lock().await;
        self.exchange(&mut guard).await
    }

    /// Gets a new token if one is due and allowed, and returns the header to send.
    async fn exchange(&self, r: &mut Refresh) -> Result<HeaderValue, String> {
        let now = Instant::now();
        let cur = self.current.load_full();
        let left = cur.as_ref().map_or(Duration::ZERO, |t| t.expires.saturating_duration_since(now));
        let usable = left > MANDATORY_REFRESH;
        if let Some(t) = &cur
            && usable
            && now < t.refresh_at
            && left > ADVISORY_REFRESH
        {
            return Ok(t.header.clone()); // another exchange just finished
        }
        // Backing off: after a failure, or while waiting for the file to rotate.
        // Once the current token is too close to expiry, only a failure stops a try.
        if millis(now) < self.next_try.load(Ordering::Relaxed) && (usable || r.last_err.is_some()) {
            return match (&cur, usable) {
                (Some(t), true) => Ok(t.header.clone()),
                _ => Err(r.last_err.clone().unwrap_or_default()),
            };
        }
        let assertion = self.source.identity_token(&self.cfg.identity_token_file).await;
        let hash = assertion.as_deref().ok().map(hash_token);
        // The file hasn't rotated yet. Sending it again would only be refused as a
        // replay, so wait while the current token lasts. Near expiry it is sent
        // anyway, since an issuer with check_jti off accepts it.
        if usable && hash.is_some() && hash == r.used {
            self.next_try.store(millis(now + RETRY_AFTER), Ordering::Relaxed);
            return Ok(cur.unwrap().header.clone());
        }
        let exchanged = match assertion {
            Ok(a) => self.exchange_once(a).await,
            Err(e) => Err(e),
        };
        match exchanged {
            Ok(t) => {
                let header = t.header.clone();
                self.current.store(Some(Arc::new(t)));
                *r = Refresh { failures: 0, last_err: None, used: hash };
                self.next_try.store(0, Ordering::Relaxed);
                Ok(header)
            }
            Err(message) => {
                eprintln!("anthropic workload identity exchange failed: {message}");
                r.failures += 1;
                r.last_err = Some(message.clone());
                self.next_try.store(millis(now + backoff(r.failures)), Ordering::Relaxed);
                match (cur, usable) {
                    (Some(t), true) => Ok(t.header.clone()),
                    _ => Err(message),
                }
            }
        }
    }

    async fn exchange_once(&self, assertion: String) -> Result<Token, String> {
        let endpoint = endpoint(&self.base_url)?;
        let mut grant = serde_json::json!({
            "grant_type": "urn:ietf:params:oauth:grant-type:jwt-bearer",
            "assertion": assertion,
            "federation_rule_id": self.cfg.federation_rule_id,
            "organization_id": self.cfg.organization_id,
            "service_account_id": self.cfg.service_account_id,
        });
        if !self.cfg.workspace_id.is_empty() {
            grant["workspace_id"] = self.cfg.workspace_id.clone().into();
        }
        let body = Bytes::from(serde_json::to_vec(&grant).map_err(|e| e.to_string())?);
        let req = Request::post(endpoint)
            .header(header::CONTENT_TYPE, "application/json")
            .body(Full::new(body))
            .map_err(|e| format!("anthropic workload identity: {e}"))?;
        let wrap = |e: String| format!("anthropic workload identity: {e}");
        let resp = tokio::time::timeout(EXCHANGE_TIMEOUT, crate::server::upstream_client().request(req))
            .await
            .map_err(|_| wrap("token exchange timed out".into()))?
            .map_err(|e| wrap(e.to_string()))?;
        let status = resp.status();
        let raw = Limited::new(resp.into_body(), 1 << 16).collect().await.map_err(|e| wrap(e.to_string()))?.to_bytes();
        if status != StatusCode::OK {
            let v: serde_json::Value = serde_json::from_slice(&raw).unwrap_or_default();
            let hint = if status == StatusCode::UNAUTHORIZED {
                " (the reason is in the Claude Console: Settings, Workload identity, authentication history)"
            } else {
                ""
            };
            let message = v["error"]["message"].as_str().unwrap_or("");
            return Err(wrap(format!("token exchange returned {}: {message}{hint}", status.as_u16())));
        }
        #[derive(Deserialize)]
        struct Out {
            access_token: String,
            expires_in: u64,
        }
        let bad = || wrap("token exchange returned no usable token".into());
        let out: Out = serde_json::from_slice(&raw).map_err(|_| bad())?;
        if out.access_token.is_empty() || out.expires_in == 0 {
            return Err(bad());
        }
        let mut header = HeaderValue::try_from(format!("Bearer {}", out.access_token)).map_err(|_| bad())?;
        header.set_sensitive(true);
        Ok(Token::new(header, Duration::from_secs(out.expires_in)))
    }
}

/// Plain HTTP only to this machine: the identity token is a credential until it
/// expires.
fn endpoint(base: &str) -> Result<String, String> {
    let base = base.trim_end_matches('/').trim_end_matches("/v1");
    let local = ["http://localhost", "http://127.0.0.1", "http://[::1]"].iter().any(|p| {
        base.strip_prefix(p).is_some_and(|rest| rest.is_empty() || rest.starts_with(':') || rest.starts_with('/'))
    });
    if !base.starts_with("https://") && !local {
        return Err("anthropic workload identity: the base URL must use https".into());
    }
    Ok(format!("{base}/v1/oauth/token"))
}

/// Where identity tokens come from on this machine, read from the environment
/// once: the variables Anthropic's SDKs read, and optionally Kubernetes.
#[derive(Default)]
struct Source {
    file: Option<String>,
    token: Option<String>,
    kubernetes: Option<Kubernetes>,
}

/// A TokenRequest for this pod's own service account. The chart grants the pod
/// create on its serviceaccounts/token and sets the LLMR_POD_* variables from the
/// downward API.
struct Kubernetes {
    api: String,
    dir: String,
    namespace: Option<String>,
    service_account: String,
    pod: Option<(String, String)>,
    audience: String,
    lifetime: u64,
}

impl Source {
    fn from_env() -> Source {
        let var = |name| std::env::var(name).ok().filter(|v: &String| !v.is_empty());
        let kubernetes = (var("ANTHROPIC_IDENTITY_TOKEN_SOURCE").as_deref() == Some("kubernetes")).then(|| {
            let host = var("KUBERNETES_SERVICE_HOST").unwrap_or_default();
            let port = var("KUBERNETES_SERVICE_PORT").unwrap_or_else(|| "443".into());
            let host = if host.contains(':') { format!("[{host}]") } else { host };
            Kubernetes {
                api: if host.is_empty() { String::new() } else { format!("https://{host}:{port}") },
                dir: "/var/run/secrets/kubernetes.io/serviceaccount".into(),
                namespace: var("LLMR_POD_NAMESPACE"),
                service_account: var("LLMR_POD_SERVICE_ACCOUNT").unwrap_or_default(),
                pod: var("LLMR_POD_NAME").zip(var("LLMR_POD_UID")),
                audience: var("ANTHROPIC_IDENTITY_TOKEN_AUDIENCE")
                    .unwrap_or_else(|| "https://api.anthropic.com".into()),
                lifetime: var("ANTHROPIC_IDENTITY_TOKEN_EXPIRATION")
                    .and_then(|v| v.parse().ok())
                    .filter(|v| *v >= 600)
                    .unwrap_or(3600), // the most an Anthropic issuer accepts by default
            }
        });
        Source { file: var("ANTHROPIC_IDENTITY_TOKEN_FILE"), token: var("ANTHROPIC_IDENTITY_TOKEN"), kubernetes }
    }

    /// Read or requested on every exchange, so rotated tokens get picked up: the
    /// provider's path, a token requested from Kubernetes, or the variables.
    async fn identity_token(&self, path: &str) -> Result<String, String> {
        if path.is_empty()
            && let Some(k) = &self.kubernetes
        {
            let err = match k.token().await {
                Ok(t) => return Ok(t),
                Err(e) => e,
            };
            // The file the kubelet keeps fresh still works while the API server is
            // unreachable, as long as it hasn't been exchanged yet.
            return match self.file_token("") {
                Ok(t) => {
                    // Once a minute at most: while the file waits to rotate this
                    // runs every few seconds.
                    static WARNED: AtomicU64 = AtomicU64::new(0);
                    let now = millis(Instant::now()) + 60_000;
                    if now.saturating_sub(WARNED.load(Ordering::Relaxed)) > 60_000 {
                        WARNED.store(now, Ordering::Relaxed);
                        eprintln!("{err}; using the token file");
                    }
                    Ok(t)
                }
                Err(_) => Err(err),
            };
        }
        self.file_token(path)
    }

    fn file_token(&self, path: &str) -> Result<String, String> {
        let path = if path.is_empty() { self.file.clone() } else { Some(path.to_string()) };
        let token = match path {
            Some(p) => {
                use std::io::Read;
                let mut raw = String::new();
                std::fs::File::open(&p)
                    .and_then(|f| f.take(MAX_ASSERTION as u64 + 1).read_to_string(&mut raw))
                    .map_err(|e| format!("anthropic workload identity: {p}: {e}"))?;
                raw
            }
            None => self.token.clone().unwrap_or_default(),
        };
        let token = token.trim();
        if token.is_empty() {
            return Err("anthropic workload identity: no identity token: set the provider's token file, \
                        ANTHROPIC_IDENTITY_TOKEN_FILE or ANTHROPIC_IDENTITY_TOKEN"
                .into());
        }
        check_assertion(token)
    }
}

impl Kubernetes {
    async fn token(&self) -> Result<String, String> {
        use rustls::pki_types::CertificateDer;
        use rustls::pki_types::pem::PemObject;
        let fail = |e: String| format!("anthropic workload identity: kubernetes token request: {e}");
        if self.api.is_empty() || self.service_account.is_empty() {
            return Err(fail("needs KUBERNETES_SERVICE_HOST and LLMR_POD_SERVICE_ACCOUNT".into()));
        }
        let read = |name: &str| std::fs::read(format!("{}/{name}", self.dir)).map_err(|e| fail(format!("{name}: {e}")));
        let namespace = match &self.namespace {
            Some(n) => n.clone(),
            None => String::from_utf8_lossy(&read("namespace")?).trim().to_string(),
        };
        let bearer = String::from_utf8_lossy(&read("token")?).trim().to_string();
        let mut roots = rustls::RootCertStore::empty();
        for cert in CertificateDer::pem_slice_iter(&read("ca.crt")?).flatten() {
            let _ = roots.add(cert);
        }
        if roots.is_empty() {
            return Err(fail("no certificate in ca.crt".into()));
        }
        let tls = rustls::ClientConfig::builder().with_root_certificates(roots).with_no_client_auth();
        let https = hyper_rustls::HttpsConnectorBuilder::new().with_tls_config(tls).https_only().enable_http1().build();
        let client: hyper_util::client::legacy::Client<_, Full<Bytes>> =
            hyper_util::client::legacy::Client::builder(hyper_util::rt::TokioExecutor::new()).build(https);
        let mut spec = serde_json::json!({"audiences": [self.audience], "expirationSeconds": self.lifetime});
        if let Some((name, uid)) = &self.pod {
            spec["boundObjectRef"] = serde_json::json!({"apiVersion": "v1", "kind": "Pod", "name": name, "uid": uid});
        }
        let body = serde_json::json!({"apiVersion": "authentication.k8s.io/v1", "kind": "TokenRequest", "spec": spec});
        let req = Request::post(format!(
            "{}/api/v1/namespaces/{namespace}/serviceaccounts/{}/token",
            self.api, self.service_account
        ))
        .header(header::CONTENT_TYPE, "application/json")
        .header(header::AUTHORIZATION, format!("Bearer {bearer}"))
        .body(Full::new(Bytes::from(body.to_string())))
        .map_err(|e| fail(e.to_string()))?;
        let resp = tokio::time::timeout(Duration::from_secs(10), client.request(req))
            .await
            .map_err(|_| fail("timed out".into()))?
            .map_err(|e| fail(format!("{e:?}")))?;
        let status = resp.status();
        let raw = Limited::new(resp.into_body(), 1 << 16).collect().await.map_err(|e| fail(e.to_string()))?.to_bytes();
        let v: serde_json::Value = serde_json::from_slice(&raw).unwrap_or_default();
        if status != StatusCode::CREATED && status != StatusCode::OK {
            return Err(fail(format!("{}: {}", status.as_u16(), v["message"].as_str().unwrap_or(""))));
        }
        check_assertion(v["status"]["token"].as_str().unwrap_or("").trim())
    }
}

/// Never send anything but a JWT: the path is admin-set and the contents go to
/// the base URL, so this must not read arbitrary files out to a server.
fn check_assertion(token: &str) -> Result<String, String> {
    if token.len() > MAX_ASSERTION || !is_jwt_shape(token) {
        return Err("anthropic workload identity: the identity token is not a JWT".into());
    }
    Ok(token.to_string())
}

fn hash_token(token: &str) -> u64 {
    use std::hash::{DefaultHasher, Hash, Hasher};
    let mut h = DefaultHasher::new();
    token.hash(&mut h);
    h.finish()
}

fn is_jwt_shape(s: &str) -> bool {
    let parts: Vec<&str> = s.split('.').collect();
    parts.len() == 3
        && parts.iter().all(|p| !p.is_empty() && p.bytes().all(|c| c.is_ascii_alphanumeric() || c == b'-' || c == b'_'))
}

#[cfg(test)]
mod tests {
    use std::sync::atomic::AtomicUsize;

    use http::Response;
    use hyper::body::Incoming;
    use hyper::service::service_fn;
    use hyper_util::rt::{TokioExecutor, TokioIo};
    use hyper_util::server::conn::auto;
    use tokio::net::TcpListener;

    use super::*;

    /// Answers each exchange with the next (status, expires_in); records assertions.
    async fn stub(replies: Vec<(u16, u64)>) -> (String, Arc<AtomicUsize>, Arc<Mutex<Vec<serde_json::Value>>>) {
        let _ = rustls::crypto::ring::default_provider().install_default();
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let url = format!("http://{}", listener.local_addr().unwrap());
        let hits = Arc::new(AtomicUsize::new(0));
        let seen = Arc::new(Mutex::new(Vec::new()));
        let replies = Arc::new(replies);
        let (h, s) = (hits.clone(), seen.clone());
        tokio::spawn(async move {
            while let Ok((conn, _)) = listener.accept().await {
                let (h, s, replies) = (h.clone(), s.clone(), replies.clone());
                tokio::spawn(async move {
                    let svc = service_fn(move |req: Request<Incoming>| {
                        let (h, s, replies) = (h.clone(), s.clone(), replies.clone());
                        async move {
                            assert_eq!(req.uri().path(), "/v1/oauth/token");
                            let raw = req.into_body().collect().await.unwrap().to_bytes();
                            s.lock().unwrap().push(serde_json::from_slice(&raw).unwrap());
                            let n = h.fetch_add(1, Ordering::SeqCst);
                            let (status, expires) = replies[n.min(replies.len() - 1)];
                            let body = if status == 200 {
                                format!(
                                    r#"{{"access_token":"sk-ant-oat01-{n}","token_type":"Bearer","expires_in":{expires}}}"#
                                )
                            } else {
                                r#"{"type":"error","error":{"type":"authentication_error","message":"Authentication failed"}}"#.to_string()
                            };
                            Ok::<_, std::convert::Infallible>(
                                Response::builder().status(status).body(Full::new(Bytes::from(body))).unwrap(),
                            )
                        }
                    });
                    let _ = auto::Builder::new(TokioExecutor::new()).serve_connection(TokioIo::new(conn), svc).await;
                });
            }
        });
        (url, hits, seen)
    }

    fn file(name: &str, contents: &str) -> String {
        let p = std::env::temp_dir().join(format!("llmr-wif-{}-{name}", std::process::id()));
        std::fs::write(&p, contents).unwrap();
        p.to_string_lossy().into_owned()
    }

    fn cfg(path: &str) -> WifConfig {
        WifConfig {
            federation_rule_id: "fdrl_1".into(),
            organization_id: "org".into(),
            service_account_id: "svac_1".into(),
            workspace_id: "wrkspc_1".into(),
            identity_token_file: path.into(),
        }
    }

    fn wif(url: &str, path: &str) -> Arc<Wif> {
        Arc::new(Wif::new(url, cfg(path), Source::default()))
    }

    /// Waits for a background renewal to finish.
    async fn idle(w: &Arc<Wif>) {
        let _ = w.refresh.lock().await;
    }

    fn header(token: &str, expires: Duration, refresh_at: Option<Instant>) -> Arc<Token> {
        let now = Instant::now();
        Arc::new(Token {
            header: HeaderValue::from_str(token).unwrap(),
            expires: now + expires,
            refresh_at: refresh_at.unwrap_or(now + expires),
        })
    }

    #[tokio::test]
    async fn exchanges_once_and_reuses_the_token() {
        let (url, hits, seen) = stub(vec![(200, 3600)]).await;
        let w = wif(&url, &file("once", "aaa.bbb.ccc\n"));
        assert_eq!(w.bearer().await.unwrap(), "Bearer sk-ant-oat01-0");
        assert_eq!(w.bearer().await.unwrap(), "Bearer sk-ant-oat01-0");
        assert_eq!(hits.load(Ordering::SeqCst), 1);
        let body = &seen.lock().unwrap()[0];
        assert_eq!(body["grant_type"], "urn:ietf:params:oauth:grant-type:jwt-bearer");
        assert_eq!(body["assertion"], "aaa.bbb.ccc");
        assert_eq!(body["federation_rule_id"], "fdrl_1");
        assert_eq!(body["organization_id"], "org");
        assert_eq!(body["service_account_id"], "svac_1");
        assert_eq!(body["workspace_id"], "wrkspc_1");
    }

    #[tokio::test]
    async fn advisory_refresh_failure_keeps_serving_the_token() {
        let (url, hits, _) = stub(vec![(200, 100), (401, 0)]).await;
        let path = file("advisory", "aaa.bbb.ccc");
        let w = wif(&url, &path);
        assert_eq!(w.bearer().await.unwrap(), "Bearer sk-ant-oat01-0");
        std::fs::write(&path, "ddd.eee.fff").unwrap();
        for _ in 0..2 {
            // 100 s left: tried once in the background, refused, then not again at once
            assert_eq!(w.bearer().await.unwrap(), "Bearer sk-ant-oat01-0");
            idle(&w).await;
            assert_eq!(hits.load(Ordering::SeqCst), 2);
        }
    }

    #[tokio::test]
    async fn does_not_resend_an_unrotated_token() {
        let (url, hits, _) = stub(vec![(200, 100), (401, 0)]).await;
        let w = wif(&url, &file("unrotated", "aaa.bbb.ccc"));
        for _ in 0..3 {
            assert_eq!(w.bearer().await.unwrap(), "Bearer sk-ant-oat01-0");
            idle(&w).await;
        }
        assert_eq!(hits.load(Ordering::SeqCst), 1);
    }

    #[tokio::test]
    async fn unrotated_token_is_sent_near_expiry() {
        // An issuer with check_jti off accepts the same token again.
        let (url, hits, _) = stub(vec![(200, 10), (200, 3600)]).await;
        let w = wif(&url, &file("nearexpiry", "aaa.bbb.ccc"));
        w.bearer().await.unwrap();
        assert_eq!(w.bearer().await.unwrap(), "Bearer sk-ant-oat01-1");
        assert_eq!(hits.load(Ordering::SeqCst), 2);
    }

    #[tokio::test]
    async fn unrotated_skip_then_near_expiry_still_tries() {
        // A skip for an unrotated file must not leave a request near expiry with
        // no token and no error.
        let (url, hits, _) = stub(vec![(200, 3600), (200, 3600)]).await;
        let w = wif(&url, &file("skipthenexpiry", "aaa.bbb.ccc"));
        w.bearer().await.unwrap();
        w.current.store(Some(header("Bearer sk-ant-oat01-0", Duration::from_secs(100), None)));
        w.bearer().await.unwrap(); // advisory: skipped, file unchanged
        idle(&w).await;
        w.current.store(Some(header("Bearer sk-ant-oat01-0", Duration::from_secs(10), None)));
        assert_eq!(w.bearer().await.unwrap(), "Bearer sk-ant-oat01-1");
        assert_eq!(hits.load(Ordering::SeqCst), 2);
    }

    #[tokio::test]
    async fn renews_at_half_life_in_the_background() {
        let (url, hits, _) = stub(vec![(200, 3600)]).await;
        let path = file("halflife", "aaa.bbb.ccc");
        let w = wif(&url, &path);
        w.bearer().await.unwrap();
        // Half its life gone, still well before expiry.
        let past = Instant::now() - Duration::from_secs(1);
        w.current.store(Some(header("old", Duration::from_secs(1800), Some(past))));
        std::fs::write(&path, "ddd.eee.fff").unwrap();
        assert_eq!(w.bearer().await.unwrap(), "old", "the request must not wait for the renewal");
        idle(&w).await;
        assert_eq!(w.bearer().await.unwrap(), "Bearer sk-ant-oat01-1");
        assert_eq!(hits.load(Ordering::SeqCst), 2);
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

    #[tokio::test]
    async fn advisory_refresh_rereads_a_rotated_token() {
        let (url, _, seen) = stub(vec![(200, 100), (200, 3600)]).await;
        let path = file("rotate", "aaa.bbb.ccc");
        let w = wif(&url, &path);
        w.bearer().await.unwrap();
        std::fs::write(&path, "ddd.eee.fff").unwrap();
        w.bearer().await.unwrap();
        idle(&w).await;
        assert_eq!(w.bearer().await.unwrap(), "Bearer sk-ant-oat01-1");
        assert_eq!(seen.lock().unwrap()[1]["assertion"], "ddd.eee.fff");
    }

    #[tokio::test]
    async fn mandatory_refresh_failure_fails_and_explains() {
        let (url, _, _) = stub(vec![(200, 10), (401, 0)]).await;
        let w = wif(&url, &file("mandatory", "aaa.bbb.ccc"));
        w.bearer().await.unwrap();
        let err = w.bearer().await.unwrap_err(); // 10 s left: too close to send
        assert!(err.contains("401") && err.contains("authentication history"), "{err}");
        assert!(!err.contains("aaa.bbb.ccc"), "{err}");
    }

    #[tokio::test]
    async fn only_a_jwt_leaves_the_machine() {
        let (url, hits, _) = stub(vec![(200, 3600)]).await;
        let err = wif(&url, &file("notjwt", "SECRET=value\nOTHER=value\n")).bearer().await.unwrap_err();
        assert!(err.contains("not a JWT") && !err.contains("SECRET"), "{err}");
        assert_eq!(hits.load(Ordering::SeqCst), 0);
    }

    #[tokio::test]
    async fn same_settings_share_one_state() {
        let cfg = WifConfig { federation_rule_id: "fdrl_shared".into(), ..Default::default() };
        let a = state("https://a.example", cfg.clone());
        assert!(Arc::ptr_eq(&a, &state("https://a.example", cfg.clone())));
        assert!(!Arc::ptr_eq(&a, &state("https://b.example", cfg)));
    }

    #[test]
    fn endpoints() {
        assert_eq!(endpoint("https://api.anthropic.com/v1/").unwrap(), "https://api.anthropic.com/v1/oauth/token");
        assert_eq!(endpoint("http://127.0.0.1:9000").unwrap(), "http://127.0.0.1:9000/v1/oauth/token");
        assert!(endpoint("http://evil.example.com").is_err());
        assert!(endpoint("http://127.0.0.1.evil.example.com").is_err());
    }

    /// A CA and a server certificate for 127.0.0.1 signed by it, as PEM and DER.
    fn ca_and_leaf() -> (String, rustls::pki_types::CertificateDer<'static>, rustls::pki_types::PrivateKeyDer<'static>)
    {
        use rcgen::{BasicConstraints, CertificateParams, IsCa, KeyPair};
        let ca_key = KeyPair::generate().unwrap();
        let mut ca_params = CertificateParams::new(Vec::<String>::new()).unwrap();
        ca_params.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
        let ca = ca_params.self_signed(&ca_key).unwrap();
        let leaf_key = KeyPair::generate().unwrap();
        let leaf =
            CertificateParams::new(vec!["127.0.0.1".to_string()]).unwrap().signed_by(&leaf_key, &ca, &ca_key).unwrap();
        (ca.pem(), leaf.der().clone(), rustls::pki_types::PrivateKeyDer::Pkcs8(leaf_key.serialize_der().into()))
    }

    /// A stand-in API server for TokenRequest over TLS, and the pod's service
    /// account directory pointing at it. Each request gets a new token; `status`
    /// overrides the answer.
    async fn k8s_stub(name: &str, status: u16) -> (Kubernetes, Arc<AtomicUsize>, Arc<Mutex<Vec<serde_json::Value>>>) {
        let _ = rustls::crypto::ring::default_provider().install_default();
        let (ca_pem, cert, key) = ca_and_leaf();
        let tls = rustls::ServerConfig::builder().with_no_client_auth().with_single_cert(vec![cert], key).unwrap();
        let acceptor = tokio_rustls::TlsAcceptor::from(Arc::new(tls));
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let port = listener.local_addr().unwrap().port();
        let hits = Arc::new(AtomicUsize::new(0));
        let seen = Arc::new(Mutex::new(Vec::new()));
        let (h, s) = (hits.clone(), seen.clone());
        tokio::spawn(async move {
            while let Ok((conn, _)) = listener.accept().await {
                let (h, s, acceptor) = (h.clone(), s.clone(), acceptor.clone());
                tokio::spawn(async move {
                    let Ok(conn) = acceptor.accept(conn).await else { return };
                    let svc = service_fn(move |req: Request<Incoming>| {
                        let (h, s) = (h.clone(), s.clone());
                        async move {
                            assert_eq!(req.uri().path(), "/api/v1/namespaces/llm-router/serviceaccounts/gw/token");
                            assert_eq!(req.headers()["authorization"], "Bearer pod-api-token");
                            let raw = req.into_body().collect().await.unwrap().to_bytes();
                            s.lock().unwrap().push(serde_json::from_slice(&raw).unwrap());
                            let n = h.fetch_add(1, Ordering::SeqCst) + 1;
                            let body = if status == 201 {
                                format!(r#"{{"kind":"TokenRequest","status":{{"token":"k8s.token.n{n}"}}}}"#)
                            } else {
                                r#"{"kind":"Status","message":"serviceaccounts \"gw\" is forbidden"}"#.to_string()
                            };
                            Ok::<_, std::convert::Infallible>(
                                Response::builder().status(status).body(Full::new(Bytes::from(body))).unwrap(),
                            )
                        }
                    });
                    let _ = auto::Builder::new(TokioExecutor::new()).serve_connection(TokioIo::new(conn), svc).await;
                });
            }
        });
        let dir = std::env::temp_dir().join(format!("llmr-k8s-{}-{name}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::write(dir.join("ca.crt"), ca_pem).unwrap();
        std::fs::write(dir.join("token"), "pod-api-token\n").unwrap();
        std::fs::write(dir.join("namespace"), "llm-router").unwrap();
        let k = Kubernetes {
            api: format!("https://127.0.0.1:{port}"),
            dir: dir.to_string_lossy().into_owned(),
            namespace: None,
            service_account: "gw".into(),
            pod: Some(("gw-abc".into(), "uid-1".into())),
            audience: "https://api.anthropic.com".into(),
            lifetime: 3600,
        };
        (k, hits, seen)
    }

    #[tokio::test]
    async fn kubernetes_token_per_exchange() {
        let (k, k8s_hits, requests) = k8s_stub("perexchange", 201).await;
        let (url, hits, seen) = stub(vec![(200, 10), (200, 3600)]).await;
        let w = Arc::new(Wif::new(&url, cfg(""), Source { kubernetes: Some(k), ..Default::default() }));
        w.bearer().await.unwrap();
        w.bearer().await.unwrap(); // 10 s left after the first: the second needs a new token
        assert_eq!((k8s_hits.load(Ordering::SeqCst), hits.load(Ordering::SeqCst)), (2, 2));
        let seen = seen.lock().unwrap();
        assert_eq!(seen[0]["assertion"], "k8s.token.n1");
        assert_eq!(seen[1]["assertion"], "k8s.token.n2", "each exchange needs a token no process has used");
        let spec = &requests.lock().unwrap()[0]["spec"];
        assert_eq!(spec["audiences"], serde_json::json!(["https://api.anthropic.com"]));
        assert_eq!(spec["expirationSeconds"], 3600);
        assert_eq!(spec["boundObjectRef"]["kind"], "Pod");
        assert_eq!(spec["boundObjectRef"]["name"], "gw-abc");
        assert_eq!(spec["boundObjectRef"]["uid"], "uid-1");
    }

    #[tokio::test]
    async fn kubernetes_falls_back_to_the_file() {
        let (k, _, _) = k8s_stub("fallback", 403).await;
        let (url, _, seen) = stub(vec![(200, 3600)]).await;
        let source = Source { file: Some(file("fallback", "fff.ggg.hhh")), kubernetes: Some(k), ..Default::default() };
        Arc::new(Wif::new(&url, cfg(""), source)).bearer().await.unwrap();
        assert_eq!(seen.lock().unwrap()[0]["assertion"], "fff.ggg.hhh");
    }

    #[tokio::test]
    async fn kubernetes_error_without_a_file() {
        let (k, _, _) = k8s_stub("nofile", 403).await;
        let (url, hits, _) = stub(vec![(200, 3600)]).await;
        let w = Arc::new(Wif::new(&url, cfg(""), Source { kubernetes: Some(k), ..Default::default() }));
        let err = w.bearer().await.unwrap_err();
        assert!(err.contains("403") && err.contains("forbidden") && !err.contains("pod-api-token"), "{err}");
        assert_eq!(hits.load(Ordering::SeqCst), 0);
    }

    #[tokio::test]
    async fn kubernetes_refuses_an_untrusted_server() {
        let (k, k8s_hits, _) = k8s_stub("untrusted", 201).await;
        std::fs::write(format!("{}/ca.crt", k.dir), "not a certificate").unwrap();
        let err = k.token().await.unwrap_err();
        assert!(err.contains("ca.crt"), "{err}");
        let (other_ca, _, _) = ca_and_leaf(); // a CA that didn't sign the server's certificate
        std::fs::write(format!("{}/ca.crt", k.dir), other_ca).unwrap();
        assert!(k.token().await.is_err());
        assert_eq!(k8s_hits.load(Ordering::SeqCst), 0);
    }
}
