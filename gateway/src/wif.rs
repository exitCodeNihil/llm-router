//! Anthropic workload identity federation: present an identity token from this
//! machine's platform and get a short-lived Anthropic token back, so no static key
//! exists to leak (Go: internal/provider/anthropic_wif.go). Each gateway exchanges
//! its own token; the control plane sends only the rule's IDs.

use std::collections::HashMap;
use std::sync::{Arc, Mutex, OnceLock};
use std::time::{Duration, Instant};

use arc_swap::ArcSwapOption;
use bytes::Bytes;
use http::{HeaderValue, Request, StatusCode, header};
use http_body_util::{BodyExt, Full, Limited};
use serde::Deserialize;

/// The schedule Anthropic's SDKs use: try a new token this long before expiry but
/// keep serving the old one if that fails, then fail once it's too close to send.
const ADVISORY_REFRESH: Duration = Duration::from_secs(120);
const MANDATORY_REFRESH: Duration = Duration::from_secs(30);
/// A refused exchange isn't retried on every request: a token with a jti claim is
/// single use, and the file has to rotate before another try can work.
const RETRY_AFTER: Duration = Duration::from_secs(5);
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
}

struct Failure {
    retry_at: Instant,
    message: String,
}

#[derive(Default)]
struct Refresh {
    failure: Option<Failure>,
    /// A hash of the identity token behind `current`.
    used: Option<u64>,
    /// When to look at the file again.
    recheck_at: Option<Instant>,
}

pub struct Wif {
    cfg: WifConfig,
    base_url: String,
    current: ArcSwapOption<Token>,
    /// One exchange at a time.
    refresh: tokio::sync::Mutex<Refresh>,
}

/// Outlives config reloads: a reload builds new providers, and exchanging again
/// for each one would burn single-use identity tokens. Providers with the same
/// settings share a token for the same reason.
pub fn state(base_url: &str, cfg: WifConfig) -> Arc<Wif> {
    type Key = (String, WifConfig);
    static STATES: OnceLock<Mutex<HashMap<Key, Arc<Wif>>>> = OnceLock::new();
    let key = (base_url.to_string(), cfg.clone());
    let mut states = STATES.get_or_init(Default::default).lock().unwrap();
    states
        .entry(key)
        .or_insert_with(|| {
            Arc::new(Wif {
                cfg,
                base_url: base_url.to_string(),
                current: ArcSwapOption::empty(),
                refresh: tokio::sync::Mutex::default(),
            })
        })
        .clone()
}

impl Wif {
    /// The Authorization header for the next upstream request.
    pub async fn bearer(&self) -> Result<HeaderValue, String> {
        let left = |t: &Option<Arc<Token>>| {
            t.as_ref().map_or(Duration::ZERO, |t| t.expires.saturating_duration_since(Instant::now()))
        };
        let cur = self.current.load_full();
        if left(&cur) > ADVISORY_REFRESH {
            return Ok(cur.unwrap().header.clone());
        }
        let mut guard = if left(&cur) > MANDATORY_REFRESH {
            match self.refresh.try_lock() {
                Ok(g) => g,
                Err(_) => return Ok(cur.unwrap().header.clone()), // another request is refreshing it
            }
        } else {
            self.refresh.lock().await
        };

        let cur = self.current.load_full(); // it may have been refreshed while this one waited
        if left(&cur) > ADVISORY_REFRESH {
            return Ok(cur.unwrap().header.clone());
        }
        let usable = left(&cur) > MANDATORY_REFRESH;
        if let Some(f) = guard.failure.as_ref()
            && Instant::now() < f.retry_at
        {
            return if usable { Ok(cur.unwrap().header.clone()) } else { Err(f.message.clone()) };
        }
        if usable && guard.recheck_at.is_some_and(|at| Instant::now() < at) {
            return Ok(cur.unwrap().header.clone());
        }
        let assertion = identity_token(&self.cfg.identity_token_file);
        let hash = assertion.as_deref().ok().map(hash_token);
        // The file hasn't rotated yet. Sending it again would only be refused as a
        // replay, so wait while the current token lasts. Near expiry it is sent
        // anyway, since an issuer with check_jti off accepts it.
        if usable && hash.is_some() && hash == guard.used {
            guard.recheck_at = Some(Instant::now() + RETRY_AFTER);
            return Ok(cur.unwrap().header.clone());
        }
        let exchanged = match assertion {
            Ok(a) => self.exchange(a).await,
            Err(e) => Err(e),
        };
        match exchanged {
            Ok(t) => {
                let header = t.header.clone();
                self.current.store(Some(Arc::new(t)));
                guard.failure = None;
                guard.used = hash;
                Ok(header)
            }
            Err(message) => {
                eprintln!("anthropic workload identity exchange failed: {message}");
                guard.failure = Some(Failure { retry_at: Instant::now() + RETRY_AFTER, message: message.clone() });
                if usable { Ok(cur.unwrap().header.clone()) } else { Err(message) }
            }
        }
    }

    async fn exchange(&self, assertion: String) -> Result<Token, String> {
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
        Ok(Token { header, expires: Instant::now() + Duration::from_secs(out.expires_in) })
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

/// Read on every exchange so rotated tokens get picked up. Where it comes from is
/// per machine: the provider's path, or the variables Anthropic's SDKs read.
fn identity_token(path: &str) -> Result<String, String> {
    let var = |name| std::env::var(name).ok().filter(|v: &String| !v.is_empty());
    let path = if path.is_empty() { var("ANTHROPIC_IDENTITY_TOKEN_FILE") } else { Some(path.to_string()) };
    let token = match path {
        Some(p) => {
            use std::io::Read;
            let mut raw = String::new();
            std::fs::File::open(&p)
                .and_then(|f| f.take(MAX_ASSERTION as u64 + 1).read_to_string(&mut raw))
                .map_err(|e| format!("anthropic workload identity: {p}: {e}"))?;
            raw
        }
        None => var("ANTHROPIC_IDENTITY_TOKEN").unwrap_or_default(),
    };
    let token = token.trim();
    if token.is_empty() {
        return Err("anthropic workload identity: no identity token: set the provider's token file, \
                    ANTHROPIC_IDENTITY_TOKEN_FILE or ANTHROPIC_IDENTITY_TOKEN"
            .into());
    }
    // The path is admin-set and the contents go to the base URL, so never send
    // anything but a JWT: this must not read arbitrary files out to a server.
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
    use std::sync::atomic::{AtomicUsize, Ordering};

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

    fn wif(url: &str, path: &str) -> Wif {
        Wif {
            cfg: WifConfig {
                federation_rule_id: "fdrl_1".into(),
                organization_id: "org".into(),
                service_account_id: "svac_1".into(),
                workspace_id: "wrkspc_1".into(),
                identity_token_file: path.into(),
            },
            base_url: url.into(),
            current: ArcSwapOption::empty(),
            refresh: tokio::sync::Mutex::default(),
        }
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
        assert_eq!(w.bearer().await.unwrap(), "Bearer sk-ant-oat01-0"); // 100 s left: tried, refused
        assert_eq!(hits.load(Ordering::SeqCst), 2);
        assert_eq!(w.bearer().await.unwrap(), "Bearer sk-ant-oat01-0"); // and not tried again at once
        assert_eq!(hits.load(Ordering::SeqCst), 2);
    }

    #[tokio::test]
    async fn does_not_resend_an_unrotated_token() {
        let (url, hits, _) = stub(vec![(200, 100), (401, 0)]).await;
        let w = wif(&url, &file("unrotated", "aaa.bbb.ccc"));
        for _ in 0..3 {
            assert_eq!(w.bearer().await.unwrap(), "Bearer sk-ant-oat01-0");
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
    async fn same_settings_share_one_state() {
        let cfg = WifConfig { federation_rule_id: "fdrl_shared".into(), ..Default::default() };
        let a = state("https://a.example", cfg.clone());
        assert!(Arc::ptr_eq(&a, &state("https://a.example", cfg.clone())));
        assert!(!Arc::ptr_eq(&a, &state("https://b.example", cfg)));
    }

    #[tokio::test]
    async fn advisory_refresh_rereads_a_rotated_token() {
        let (url, _, seen) = stub(vec![(200, 100), (200, 3600)]).await;
        let path = file("rotate", "aaa.bbb.ccc");
        let w = wif(&url, &path);
        w.bearer().await.unwrap();
        std::fs::write(&path, "ddd.eee.fff").unwrap();
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

    #[test]
    fn endpoints() {
        assert_eq!(endpoint("https://api.anthropic.com/v1/").unwrap(), "https://api.anthropic.com/v1/oauth/token");
        assert_eq!(endpoint("http://127.0.0.1:9000").unwrap(), "http://127.0.0.1:9000/v1/oauth/token");
        assert!(endpoint("http://evil.example.com").is_err());
        assert!(endpoint("http://127.0.0.1.evil.example.com").is_err());
    }
}
