//! The /v1 data plane (internal/gateway): auth, limits, routing with failover,
//! and the OpenAI and Anthropic surfaces, plus /healthz and /metrics.

// Handlers return an error response as Err, which is a large value; it only
// moves on the refusal paths.
#![allow(clippy::result_large_err)]

use std::borrow::Cow;
use std::cell::RefCell;
use std::convert::Infallible;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::{Duration, Instant};

use arc_swap::ArcSwapOption;
use bytes::Bytes;
use http::{HeaderMap, HeaderValue, Method, Request, Response, StatusCode, header};
use http_body_util::{BodyExt, Full, Limited};
use hyper::body::Incoming;
use hyper_rustls::HttpsConnector;
use hyper_util::client::legacy::Client;
use hyper_util::client::legacy::connect::HttpConnector;
use hyper_util::rt::TokioExecutor;
use serde::Deserialize;
use serde_json::value::RawValue;

use crate::body::{Body, InFlight, Sse};
use crate::config::{AuthMode, Config, Deployment, Flavor, Key};
use crate::metrics::{self, Attempt, Route};
use crate::translate::{self, BadRequest, SseTransform};
use crate::upstream::{self, BuildError, Inbound};
use crate::usage::Emit;
use crate::{limits, now_ms};

const MAX_BODY: usize = 10 << 20;

pub struct State {
    pub config: ArcSwapOption<Config>,
    /// Config loaded and the first sync done, so budgets are known.
    pub ready: AtomicBool,
    pub draining: AtomicBool,
    pub max_in_flight: i64,
}

impl State {
    pub fn is_ready(&self) -> bool {
        self.ready.load(Ordering::Relaxed) && !self.draining.load(Ordering::Relaxed)
    }
}

pub type HttpsClient = Client<HttpsConnector<HttpConnector>, Full<Bytes>>;

/// An HTTP client with its own connection pool, for the control plane or for
/// one worker thread's upstream calls.
pub fn https_client(pool: usize) -> HttpsClient {
    let mut http = HttpConnector::new();
    http.set_nodelay(true);
    http.set_connect_timeout(Some(Duration::from_secs(5)));
    http.enforce_http(false);
    let https = hyper_rustls::HttpsConnectorBuilder::new()
        .with_native_roots()
        .unwrap_or_else(|_| {
            hyper_rustls::HttpsConnectorBuilder::new().with_tls_config(
                rustls::ClientConfig::builder()
                    .with_root_certificates(rustls::RootCertStore::empty())
                    .with_no_client_auth(),
            )
        })
        .https_or_http()
        .enable_http1()
        .enable_http2()
        .wrap_connector(http);
    Client::builder(TokioExecutor::new())
        .pool_idle_timeout(Duration::from_secs(90))
        .pool_max_idle_per_host(pool)
        .build(https)
}

thread_local! {
    // One pool per worker: a shared pool is one lock every request takes.
    static UPSTREAM: RefCell<Option<HttpsClient>> = const { RefCell::new(None) };
}

fn upstream_client() -> HttpsClient {
    UPSTREAM.with(|c| c.borrow_mut().get_or_insert_with(|| https_client(4096)).clone())
}

pub async fn handle(st: &'static State, req: Request<Incoming>) -> Result<Response<Body>, Infallible> {
    let (slot, n) = InFlight::take();
    let route = route_of(req.method(), req.uri().path());
    let resp = if n > st.max_in_flight {
        metrics::SHED.fetch_add(1, Ordering::Relaxed);
        let mut r = oai_error(
            StatusCode::SERVICE_UNAVAILABLE,
            "server_error",
            "overloaded",
            "too many requests in flight; retry shortly",
        );
        r.headers_mut().insert(header::RETRY_AFTER, HeaderValue::from_static("1"));
        r
    } else {
        dispatch(st, req).await
    };
    metrics::request(route, resp.status().as_u16());
    let (parts, body) = resp.into_parts();
    Ok(Response::from_parts(parts, body.holding(slot)))
}

fn route_of(m: &Method, path: &str) -> Route {
    match (m, path) {
        (&Method::POST, "/v1/chat/completions") => Route::Chat,
        (&Method::POST, "/v1/completions") => Route::Completions,
        (&Method::POST, "/v1/embeddings") => Route::Embeddings,
        (&Method::POST, "/v1/messages") => Route::Messages,
        (&Method::GET, "/v1/models") => Route::Models,
        (&Method::POST, "/v1/messages/count_tokens") => Route::CountTokens,
        _ => Route::Other,
    }
}

async fn dispatch(st: &'static State, req: Request<Incoming>) -> Response<Body> {
    let path = req.uri().path();
    match (req.method(), path) {
        (&Method::GET, "/healthz") => {
            if st.is_ready() {
                text(StatusCode::OK, "ok")
            } else {
                text(StatusCode::SERVICE_UNAVAILABLE, "starting")
            }
        }
        (&Method::GET, "/metrics") => {
            let body =
                metrics::render(st.is_ready(), crate::usage::queued(), crate::usage::DROPPED.load(Ordering::Relaxed));
            let mut r = Response::new(Body::full(body));
            r.headers_mut().insert(header::CONTENT_TYPE, HeaderValue::from_static("text/plain; version=0.0.4"));
            r
        }
        (
            _,
            "/v1/chat/completions"
            | "/v1/completions"
            | "/v1/embeddings"
            | "/v1/models"
            | "/v1/messages"
            | "/v1/messages/count_tokens",
        ) => {
            let Some(cfg) = st.config.load_full() else {
                return oai_error(
                    StatusCode::SERVICE_UNAVAILABLE,
                    "server_error",
                    "starting_up",
                    "gateway has no config snapshot yet",
                );
            };
            let (key, inbound) = match authenticate(&cfg, req.headers()) {
                Ok(v) => v,
                Err(r) => return r,
            };
            let route = route_of(req.method(), path);
            if let Err(d) = limits::check(&key) {
                metrics::denial(d.code);
                let mut r = oai_error(StatusCode::TOO_MANY_REQUESTS, "insufficient_quota", d.code, &d.message);
                if let Some(s) = d.retry_after_s {
                    r.headers_mut().insert(header::RETRY_AFTER, HeaderValue::from(s));
                }
                // Recorded, so "how often is this key throttled" has an answer;
                // the body isn't read yet, so the model is blank.
                emit(&cfg, &key, String::new(), false).send(429, d.code, Default::default(), 0);
                return r;
            }
            match route {
                Route::Chat => proxy_openai(cfg, key, inbound, req, "/chat/completions").await,
                Route::Completions => proxy_openai(cfg, key, inbound, req, "/completions").await,
                Route::Embeddings => proxy_openai(cfg, key, inbound, req, "/embeddings").await,
                Route::Messages => proxy_anthropic(cfg, key, inbound, req).await,
                Route::Models => models(&cfg, &key, &inbound),
                Route::CountTokens => count_tokens(req).await,
                Route::Other => not_found(),
            }
        }
        _ => not_found(),
    }
}

/// Resolve the caller (gateway.authenticate): X-Llmr-Key first, so a
/// pass-through client can keep Authorization for its own upstream token, then
/// Authorization, then x-api-key.
fn authenticate(cfg: &Config, h: &HeaderMap) -> Result<(Arc<Key>, Inbound), Response<Body>> {
    let header_str = |name: &str| h.get(name).and_then(|v| v.to_str().ok()).filter(|s| !s.is_empty());
    let mut via_authz = false;
    let token = match header_str("x-llmr-key") {
        Some(t) => t,
        None => match header_str("authorization").map(|a| a.strip_prefix("Bearer ").unwrap_or(a)) {
            Some(t) if !t.is_empty() => {
                via_authz = true;
                t
            }
            _ => match header_str("x-api-key") {
                Some(t) => t,
                None => return Err(auth_error("missing API key: send Authorization, x-api-key, or X-Llmr-Key")),
            },
        },
    };
    let key = token
        .starts_with("llmr_")
        .then(|| {
            let digest = ring::digest::digest(&ring::digest::SHA256, token.as_bytes());
            let mut hash = [0u8; 32];
            hash.copy_from_slice(digest.as_ref());
            cfg.keys.get(&hash).cloned()
        })
        .flatten()
        .filter(|k| k.expires_at == 0 || now_ms() / 1000 <= k.expires_at);
    let Some(key) = key else { return Err(auth_error("invalid API key")) };
    let inbound = Inbound {
        // Only when it wasn't the gateway key: that must never travel upstream.
        authorization: if via_authz { None } else { h.get(header::AUTHORIZATION).cloned() },
        anthropic_beta: h.get("anthropic-beta").cloned(),
    };
    Ok((key, inbound))
}

fn emit(cfg: &Arc<Config>, key: &Arc<Key>, model: String, stream: bool) -> Emit {
    Emit {
        cfg: cfg.clone(),
        key: key.clone(),
        model,
        request_id: format!("req_{:016x}", fastrand::u64(..)),
        stream,
        start: Instant::now(),
        ts_ms: now_ms(),
        deployment: None,
        attempts: 0,
    }
}

/// The few fields the router reads from a request body. Everything else is
/// passed through untouched, and a duplicate "model" is refused rather than
/// letting the allowlist check one name and the upstream read another.
#[derive(Deserialize)]
struct Peek<'a> {
    #[serde(borrow)]
    model: Option<&'a RawValue>,
    #[serde(borrow)]
    stream: Option<&'a RawValue>,
    #[serde(borrow)]
    stream_options: Option<&'a RawValue>,
}

struct Parsed<'a> {
    model: String,
    model_raw: Option<&'a RawValue>,
    stream: bool,
    include_usage: bool,
    stream_options: Option<&'a RawValue>,
}

fn peek(body: &[u8]) -> Result<Parsed<'_>, String> {
    let p: Peek = serde_json::from_slice(body).map_err(|e| format!("invalid JSON body: {e}"))?;
    let model = match p.model {
        Some(raw) => {
            serde_json::from_str::<Cow<str>>(raw.get()).map_err(|e| format!("invalid model field: {e}"))?.into_owned()
        }
        None => String::new(),
    };
    let stream = p.stream.is_some_and(|s| s.get() == "true");
    #[derive(Deserialize)]
    struct Opts {
        include_usage: Option<bool>,
    }
    let include_usage = p
        .stream_options
        .and_then(|o| serde_json::from_str::<Opts>(o.get()).ok())
        .and_then(|o| o.include_usage)
        .unwrap_or(false);
    Ok(Parsed { model, model_raw: p.model, stream, include_usage, stream_options: p.stream_options })
}

/// The request body for an upstream: the original bytes with the model name
/// replaced and, when asked, stream usage turned on. Spliced, not re-encoded:
/// a large prompt is copied once at most, and not at all when nothing changes.
fn splice(body: &Bytes, p: &Parsed, upstream: &str, inject_usage: bool) -> Bytes {
    let base = body.as_ptr() as usize;
    let mut edits: Vec<(usize, usize, Vec<u8>)> = Vec::new();
    if let Some(raw) = p.model_raw {
        let start = raw.get().as_ptr() as usize - base;
        if p.model != upstream {
            edits.push((start, start + raw.get().len(), serde_json::to_vec(upstream).unwrap_or_default()));
        }
    }
    if inject_usage {
        match p.stream_options {
            Some(raw) => {
                let start = raw.get().as_ptr() as usize - base;
                let mut opts: serde_json::Map<String, serde_json::Value> =
                    serde_json::from_str(raw.get()).unwrap_or_default();
                opts.insert("include_usage".into(), serde_json::Value::Bool(true));
                edits.push((start, start + raw.get().len(), serde_json::to_vec(&opts).unwrap_or_default()));
            }
            None => {
                // before the closing brace of the top-level object
                if let Some(end) = body.iter().rposition(|&c| c == b'}') {
                    let empty = body[..end].iter().rev().find(|c| !c.is_ascii_whitespace()) == Some(&b'{');
                    let ins: &[u8] = if empty {
                        br#""stream_options":{"include_usage":true}"#
                    } else {
                        br#","stream_options":{"include_usage":true}"#
                    };
                    edits.push((end, end, ins.to_vec()));
                }
            }
        }
    }
    if edits.is_empty() {
        return body.clone();
    }
    edits.sort_by_key(|e| e.0);
    let mut out = Vec::with_capacity(body.len() + 64);
    let mut at = 0;
    for (start, end, rep) in edits {
        out.extend_from_slice(&body[at..start]);
        out.extend_from_slice(&rep);
        at = end;
    }
    out.extend_from_slice(&body[at..]);
    out.into()
}

async fn read_body(req: Request<Incoming>) -> Result<Bytes, Response<Body>> {
    match Limited::new(req.into_body(), MAX_BODY).collect().await {
        Ok(b) => Ok(b.to_bytes()),
        Err(e) if e.downcast_ref::<http_body_util::LengthLimitError>().is_some() => Err(oai_error(
            StatusCode::PAYLOAD_TOO_LARGE,
            "invalid_request_error",
            "body_too_large",
            "request body exceeds 10MB",
        )),
        Err(_) => Err(oai_error(
            StatusCode::BAD_REQUEST,
            "invalid_request_error",
            "invalid_body",
            "could not read request body",
        )),
    }
}

async fn proxy_openai(
    cfg: Arc<Config>,
    key: Arc<Key>,
    inbound: Inbound,
    req: Request<Incoming>,
    path: &str,
) -> Response<Body> {
    let body = match read_body(req).await {
        Ok(b) => b,
        Err(r) => return r,
    };
    let p = match peek(&body) {
        Ok(p) => p,
        Err(m) => return oai_error(StatusCode::BAD_REQUEST, "invalid_request_error", "invalid_body", &m),
    };
    if p.model.is_empty() {
        return oai_error(StatusCode::BAD_REQUEST, "invalid_request_error", "missing_model", "model field is required");
    }
    let mut em = emit(&cfg, &key, p.model.clone(), p.stream);
    let Some(resolved) = cfg.resolve(&p.model) else {
        if !key.model_allowed(&[&p.model]) {
            return refuse_oai(em, "model_not_allowed");
        }
        return refuse_oai(em, "model_not_found");
    };
    if !key.model_allowed(&[&p.model, &resolved.alias]) {
        return refuse_oai(em, "model_not_allowed");
    }
    let inject = p.stream && !p.include_usage;
    let outcome = try_deployments(&resolved.deployments, path, &inbound, |d| match d.flavor {
        Flavor::Anthropic => translate::oai_req_to_anth(&body, &d.upstream_name).map(Bytes::from),
        Flavor::OpenAI => Ok(splice(&body, &p, &d.upstream_name, inject)),
    })
    .await;
    let (resp, dep, attempts) = match outcome {
        Outcome::Served { resp, dep, attempts } => (resp, dep, attempts),
        Outcome::BadBody(m) => return oai_error(StatusCode::BAD_REQUEST, "invalid_request_error", "invalid_body", &m),
        Outcome::Failed { attempts, build } => {
            em.attempts = attempts;
            return match build {
                Some(BuildError::NoCallerCredential(m)) => {
                    em.send(400, "missing_credential", Default::default(), 0);
                    oai_error(StatusCode::BAD_REQUEST, "invalid_request_error", "missing_credential", &m)
                }
                Some(BuildError::Unsupported(m)) => {
                    oai_error(StatusCode::INTERNAL_SERVER_ERROR, "server_error", "internal_error", &m)
                }
                None => {
                    em.send(502, "upstream_unavailable", Default::default(), 0);
                    oai_error(
                        StatusCode::BAD_GATEWAY,
                        "server_error",
                        "upstream_unavailable",
                        &format!("all deployments for model {} failed", p.model),
                    )
                }
            };
        }
    };
    em.deployment = Some(dep.clone());
    em.attempts = attempts;
    let status = resp.status();
    let mut out = routed(status, &em.request_id, &dep, attempts, resp.headers());
    let sse = p.stream && status == StatusCode::OK;
    match (dep.flavor, sse) {
        (Flavor::Anthropic, true) => {
            stream(out, resp, Box::new(translate::AnthToOaiStream::new(&p.model, &em.request_id)), em)
        }
        (Flavor::OpenAI, true) => stream(out, resp, Box::new(translate::OaiPassthrough::new(inject)), em),
        (flavor, false) => {
            let ct = resp.headers().get(header::CONTENT_TYPE).cloned();
            let bytes = match collect(resp).await {
                Ok(b) => b,
                Err(r) => return r,
            };
            let (bytes, usage) = if status != StatusCode::OK {
                (bytes, Default::default())
            } else if flavor == Flavor::Anthropic {
                let (b, u) = translate::anth_resp_to_oai(&bytes, &p.model, &em.request_id);
                (Bytes::from(b), u)
            } else {
                let u = translate::oai_json_usage(&bytes);
                (bytes, u)
            };
            let ct = if flavor == Flavor::Anthropic && status == StatusCode::OK {
                Some(HeaderValue::from_static("application/json"))
            } else {
                ct
            };
            if let Some(ct) = ct {
                out.headers_mut().insert(header::CONTENT_TYPE, ct);
            }
            em.send(status.as_u16(), upstream_error_code(status), usage, 0);
            body_response(out, Body::full(bytes))
        }
    }
}

async fn proxy_anthropic(cfg: Arc<Config>, key: Arc<Key>, inbound: Inbound, req: Request<Incoming>) -> Response<Body> {
    let body = match read_body(req).await {
        Ok(b) => b,
        Err(_) => return anth_error(StatusCode::BAD_REQUEST, "invalid_request_error", "unreadable or oversized body"),
    };
    let p = match peek(&body) {
        Ok(p) => p,
        Err(m) => return anth_error(StatusCode::BAD_REQUEST, "invalid_request_error", &m),
    };
    if p.model.is_empty() {
        return anth_error(StatusCode::BAD_REQUEST, "invalid_request_error", "model is required");
    }
    let mut em = emit(&cfg, &key, p.model.clone(), p.stream);
    let refuse = |em: Emit, code: &'static str| {
        let why = if code == "model_not_allowed" { "is not available for this API key" } else { "does not exist" };
        em.send(404, code, Default::default(), 0);
        anth_error(StatusCode::NOT_FOUND, "not_found_error", &format!("model {} {why}", p.model))
    };
    let Some(resolved) = cfg.resolve(&p.model) else {
        let code = if key.model_allowed(&[&p.model]) { "model_not_found" } else { "model_not_allowed" };
        return refuse(em, code);
    };
    if !key.model_allowed(&[&p.model, &resolved.alias]) {
        return refuse(em, "model_not_allowed");
    }
    let outcome = try_deployments(&resolved.deployments, "/chat/completions", &inbound, |d| match d.flavor {
        // Anthropic to Anthropic: the body passes through with only the model
        // swapped, keeping thinking, cache_control and tool streaming intact.
        Flavor::Anthropic => Ok(splice(&body, &p, &d.upstream_name, false)),
        Flavor::OpenAI => translate::anth_req_to_oai(&body, &d.upstream_name).map(Bytes::from),
    })
    .await;
    let (resp, dep, attempts) = match outcome {
        Outcome::Served { resp, dep, attempts } => (resp, dep, attempts),
        Outcome::BadBody(m) => return anth_error(StatusCode::BAD_REQUEST, "invalid_request_error", &m),
        Outcome::Failed { attempts, build } => {
            em.attempts = attempts;
            return match build {
                Some(BuildError::NoCallerCredential(m)) => {
                    em.send(400, "missing_credential", Default::default(), 0);
                    anth_error(StatusCode::BAD_REQUEST, "invalid_request_error", &m)
                }
                Some(BuildError::Unsupported(m)) => anth_error(StatusCode::INTERNAL_SERVER_ERROR, "api_error", &m),
                None => {
                    em.send(502, "upstream_unavailable", Default::default(), 0);
                    anth_error(
                        StatusCode::BAD_GATEWAY,
                        "api_error",
                        &format!("all deployments for model {} failed", p.model),
                    )
                }
            };
        }
    };
    em.deployment = Some(dep.clone());
    em.attempts = attempts;
    let status = resp.status();
    let mut out = routed(status, &em.request_id, &dep, attempts, resp.headers());
    if status != StatusCode::OK {
        // the upstream's error, in Anthropic's shape
        let bytes = collect(resp).await.unwrap_or_default();
        let msg = String::from_utf8_lossy(&bytes[..bytes.len().min(4096)]).trim().to_string();
        em.send(status.as_u16(), upstream_error_code(status), Default::default(), 0);
        let mut r = anth_error(status, "api_error", &msg);
        for (k, v) in out.headers() {
            r.headers_mut().insert(k.clone(), v.clone());
        }
        return r;
    }
    match (dep.flavor, p.stream) {
        (Flavor::Anthropic, true) => stream(out, resp, Box::new(translate::AnthPassthrough::new()), em),
        (Flavor::OpenAI, true) => {
            stream(out, resp, Box::new(translate::OaiToAnthStream::new(&p.model, &em.request_id)), em)
        }
        (flavor, false) => {
            let ct = resp.headers().get(header::CONTENT_TYPE).cloned();
            let bytes = match collect(resp).await {
                Ok(b) => b,
                Err(r) => return r,
            };
            let (bytes, usage) = if flavor == Flavor::Anthropic {
                let u = translate::anth_json_usage(&bytes);
                (bytes, u)
            } else {
                let (b, u) = translate::oai_resp_to_anth(&bytes, &p.model, &em.request_id);
                (Bytes::from(b), u)
            };
            let ct = if flavor == Flavor::Anthropic { ct } else { Some(HeaderValue::from_static("application/json")) };
            if let Some(ct) = ct {
                out.headers_mut().insert(header::CONTENT_TYPE, ct);
            }
            em.send(200, "", usage, 0);
            body_response(out, Body::full(bytes))
        }
    }
}

fn refuse_oai(em: Emit, code: &'static str) -> Response<Body> {
    let model = em.model.clone();
    em.send(404, code, Default::default(), 0);
    let msg = if code == "model_not_allowed" {
        format!("model {model} is not available for this API key")
    } else {
        format!("model {model} does not exist")
    };
    oai_error(StatusCode::NOT_FOUND, "invalid_request_error", code, &msg)
}

fn stream(out: Response<Body>, resp: Response<Incoming>, t: Box<dyn SseTransform>, em: Emit) -> Response<Body> {
    let (mut parts, _) = out.into_parts();
    parts.headers.insert(header::CONTENT_TYPE, HeaderValue::from_static("text/event-stream"));
    parts.headers.insert(header::CACHE_CONTROL, HeaderValue::from_static("no-cache"));
    Response::from_parts(parts, Body::sse(Sse::new(resp.into_body(), t, em)))
}

fn body_response(out: Response<Body>, body: Body) -> Response<Body> {
    let (parts, _) = out.into_parts();
    Response::from_parts(parts, body)
}

async fn collect(resp: Response<Incoming>) -> Result<Bytes, Response<Body>> {
    match Limited::new(resp.into_body(), MAX_BODY).collect().await {
        Ok(b) => Ok(b.to_bytes()),
        Err(_) => Err(oai_error(
            StatusCode::BAD_GATEWAY,
            "server_error",
            "upstream_error",
            "upstream response unreadable or over 10MB",
        )),
    }
}

/// A response carrying the routing attribution: the request id, and which
/// deployment served it after how many attempts.
fn routed(status: StatusCode, req_id: &str, d: &Deployment, attempts: i64, upstream: &HeaderMap) -> Response<Body> {
    let mut r = Response::new(Body::empty());
    *r.status_mut() = status;
    let h = r.headers_mut();
    if let Ok(v) = HeaderValue::try_from(req_id) {
        h.insert("x-request-id", v);
    }
    if let Ok(v) = HeaderValue::try_from(d.provider.name.as_str()) {
        h.insert("x-llmr-provider", v);
    }
    if let Ok(v) = HeaderValue::try_from(d.upstream_name.as_str()) {
        h.insert("x-llmr-upstream", v);
    }
    h.insert("x-llmr-attempts", HeaderValue::from(attempts));
    h.insert(
        "access-control-expose-headers",
        HeaderValue::from_static("X-Request-Id, X-Llmr-Provider, X-Llmr-Upstream, X-Llmr-Attempts"),
    );
    // The upstream's advice on a 429 or 503, so the client knows when to come back.
    if (status == StatusCode::TOO_MANY_REQUESTS || status == StatusCode::SERVICE_UNAVAILABLE)
        && let Some(ra) = upstream.get(header::RETRY_AFTER)
    {
        h.insert(header::RETRY_AFTER, ra.clone());
    }
    r
}

fn upstream_error_code(s: StatusCode) -> &'static str {
    match s.as_u16() {
        0..400 => "",
        429 => "upstream_rate_limited",
        500.. => "upstream_error",
        _ => "upstream_rejected",
    }
}

enum Outcome {
    Served {
        resp: Response<Incoming>,
        dep: Arc<Deployment>,
        attempts: i64,
    },
    /// Every deployment failed; `build` is set when none could even be asked.
    Failed {
        attempts: i64,
        build: Option<BuildError>,
    },
    BadBody(String),
}

/// Try each deployment in routing order, those cooling down last, failing over
/// on connect errors, timeouts, 429 and 5xx, before any byte reaches the client.
async fn try_deployments(
    deployments: &[Arc<Deployment>],
    path: &str,
    inbound: &Inbound,
    build: impl Fn(&Deployment) -> Result<Bytes, BadRequest>,
) -> Outcome {
    let now = now_ms();
    let order: Vec<&Arc<Deployment>> = deployments
        .iter()
        .filter(|d| !d.health.cooling(now))
        .chain(deployments.iter().filter(|d| d.health.cooling(now)))
        .collect();
    let mut attempts = 0;
    let mut last_build = None;
    let mut reached = false;
    for (i, d) in order.iter().enumerate() {
        attempts += 1;
        let body = match build(d) {
            Ok(b) => b,
            Err(BadRequest(m)) => return Outcome::BadBody(m),
        };
        let (url, headers) = match upstream::request_parts(d, path, inbound) {
            Ok(v) => v,
            Err(e) => {
                last_build = Some(e);
                continue;
            }
        };
        let mut req = match Request::post(url).body(Full::new(body)) {
            Ok(r) => r,
            Err(e) => {
                last_build = Some(BuildError::Unsupported(format!("bad upstream URL for {}: {e}", d.provider.name)));
                continue;
            }
        };
        *req.headers_mut() = headers;
        reached = true;
        let resp = match tokio::time::timeout(d.timeout, upstream_client().request(req)).await {
            Err(_) => {
                d.health.fail(None);
                metrics::attempt(Attempt::Timeout);
                continue;
            }
            Ok(Err(_)) => {
                d.health.fail(None);
                metrics::attempt(Attempt::Error);
                continue;
            }
            Ok(Ok(r)) => r,
        };
        let s = resp.status().as_u16();
        if s == 429 || s >= 500 {
            // A pass-through 429 is the caller's own plan limit, not a sick
            // upstream: cooling the shared row would mark it down for everyone.
            if s >= 500 || d.provider.auth_mode != AuthMode::OAuthPassthrough {
                d.health.fail(retry_after(resp.headers()));
            }
            if i < order.len() - 1 {
                metrics::attempt(Attempt::Retryable);
                continue;
            }
        } else {
            d.health.ok();
        }
        metrics::attempt(Attempt::Ok);
        return Outcome::Served { resp, dep: (*d).clone(), attempts };
    }
    Outcome::Failed { attempts, build: if reached { None } else { last_build } }
}

fn retry_after(h: &HeaderMap) -> Option<Duration> {
    h.get(header::RETRY_AFTER)?.to_str().ok()?.trim().parse::<u64>().ok().map(Duration::from_secs)
}

/// One list for both dialects: OpenAI clients read object/owned_by, Anthropic
/// clients read type/display_name. Pass-through models are only listed for a
/// caller that brought its own upstream token.
fn models(cfg: &Config, key: &Key, inbound: &Inbound) -> Response<Body> {
    let has_credential = inbound.authorization.is_some();
    let data: Vec<serde_json::Value> = cfg
        .names
        .iter()
        .filter(|n| key.model_allowed(&[n.as_str()]))
        .filter(|n| has_credential || !cfg.caller_credential_only(n))
        .map(|n| serde_json::json!({"id": n, "object": "model", "type": "model", "display_name": n, "owned_by": "llm-router"}))
        .collect();
    let first = data.first().map(|m| m["id"].clone()).unwrap_or(serde_json::Value::Null);
    let last = data.last().map(|m| m["id"].clone()).unwrap_or(serde_json::Value::Null);
    json(
        StatusCode::OK,
        serde_json::json!({"object": "list", "data": data, "has_more": false, "first_id": first, "last_id": last}),
    )
}

/// A cheap estimate so Anthropic clients that call it don't break.
async fn count_tokens(req: Request<Incoming>) -> Response<Body> {
    match read_body(req).await {
        Ok(b) => json(StatusCode::OK, serde_json::json!({"input_tokens": b.len() / 4})),
        Err(_) => anth_error(StatusCode::BAD_REQUEST, "invalid_request_error", "unreadable body"),
    }
}

fn json(status: StatusCode, v: serde_json::Value) -> Response<Body> {
    let mut r = Response::new(Body::full(serde_json::to_vec(&v).unwrap_or_default()));
    *r.status_mut() = status;
    r.headers_mut().insert(header::CONTENT_TYPE, HeaderValue::from_static("application/json"));
    r
}

fn text(status: StatusCode, s: &'static str) -> Response<Body> {
    let mut r = Response::new(Body::full(s));
    *r.status_mut() = status;
    r
}

pub fn oai_error(status: StatusCode, kind: &str, code: &str, message: &str) -> Response<Body> {
    json(status, serde_json::json!({"error": {"message": message, "type": kind, "code": code}}))
}

fn auth_error(message: &str) -> Response<Body> {
    oai_error(StatusCode::UNAUTHORIZED, "invalid_request_error", "invalid_api_key", message)
}

fn anth_error(status: StatusCode, kind: &str, message: &str) -> Response<Body> {
    json(status, serde_json::json!({"type": "error", "error": {"type": kind, "message": message}}))
}

fn not_found() -> Response<Body> {
    json(StatusCode::NOT_FOUND, serde_json::json!({"error": "not found"}))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn sp(body: &str, upstream: &str, inject: bool) -> String {
        let b = Bytes::from(body.to_string());
        let p = peek(&b).unwrap();
        String::from_utf8(splice(&b, &p, upstream, inject).to_vec()).unwrap()
    }

    #[test]
    fn splices_without_reencoding() {
        let body = r#"{"messages":[{"role":"user","content":"hi \"there\""}], "model" : "gpt-a","x":1}"#;
        assert_eq!(
            sp(body, "up", false),
            r#"{"messages":[{"role":"user","content":"hi \"there\""}], "model" : "up","x":1}"#
        );
        // nothing to change: the same bytes
        assert_eq!(sp(body, "gpt-a", false), body);
        // escaped model names are compared decoded and written encoded
        assert_eq!(sp(r#"{"model":"a\/b"}"#, "a/b", false), r#"{"model":"a\/b"}"#);
        assert_eq!(sp(r#"{"model":"a"}"#, "q\"x", false), r#"{"model":"q\"x"}"#);
    }

    #[test]
    fn injects_stream_usage() {
        let v: serde_json::Value = serde_json::from_str(&sp(r#"{"model":"m","stream":true }"#, "m", true)).unwrap();
        assert_eq!(v["stream_options"]["include_usage"], true);
        let v: serde_json::Value = serde_json::from_str(&sp(
            r#"{"model":"m","stream":true,"stream_options":{"include_usage":false,"x":2}}"#,
            "u",
            true,
        ))
        .unwrap();
        assert_eq!(v["stream_options"]["include_usage"], true);
        assert_eq!(v["stream_options"]["x"], 2);
        assert_eq!(v["model"], "u");
    }

    #[test]
    fn refuses_duplicate_model() {
        assert!(peek(br#"{"model":"allowed","model":"other"}"#).is_err());
        assert!(peek(br#"{"model":7}"#).is_err());
        assert!(peek(b"not json").is_err());
        let p = peek(br#"{"model":"m","stream":"true"}"#).unwrap();
        assert!(!p.stream, "a non-boolean stream is not a stream, as in Go");
    }
}
