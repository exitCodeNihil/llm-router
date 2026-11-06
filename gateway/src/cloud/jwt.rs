//! Callers' identity tokens (internal/auth/cloudtoken.go): a JWT from an
//! issuer configured on the control plane, verified here against the issuer's
//! published keys and mapped to a user by its email claim.
//!
//! The issuer's keys come from OIDC discovery and are kept for an hour, then
//! renewed in the background while they go on being used; a key id they don't
//! have refetches them, at most once a minute. Only a verification that has no
//! key to use waits for a fetch, and one fetch per issuer runs at a time. A
//! verified token is kept until it expires, five minutes at most, so the hot
//! path for a known caller is one hash lookup: no control-plane call and no
//! database.
//!
//! A caller with no user, from an issuer that creates users, is provisioned
//! through the control plane, which verifies the token again itself: a gateway
//! can only create users it holds a valid token for. Concurrent first requests
//! from one person make one call, and the answer is kept: a created user until
//! they arrive with the config, a refusal for a minute.

use std::collections::{HashMap, HashSet};
use std::future::Future;
use std::pin::Pin;
use std::sync::{Arc, Mutex, OnceLock};
use std::time::{Duration, Instant};

use arc_swap::ArcSwap;
use bytes::Bytes;
use http::{Request, StatusCode, header};
use http_body_util::{BodyExt, Full, Limited};
use ring::signature;
use serde::Deserialize;
use serde_json::Value;

use super::https_or_loopback;
use crate::config::{Config, Issuer, Key, User};
use crate::now_ms;
use crate::server::{HttpsClient, https_client};

/// How long an issuer's keys are used before they are fetched again.
const KEYS_TTL: Duration = Duration::from_secs(3600);
/// The least time between two fetches of one issuer's keys once it has some.
const REFETCH_EVERY: Duration = Duration::from_secs(60);
/// The least time between two attempts for an issuer that has never answered.
const RETRY_EMPTY: Duration = Duration::from_secs(5);
const FETCH_TIMEOUT: Duration = Duration::from_secs(10);
/// A verified token is trusted this long at most, then verified again.
const CACHE_MS: i64 = 5 * 60_000;
const MAX_CACHED: usize = 10_000;
/// Clock skew allowed on nbf, as the control plane's verifier allows.
const NBF_LEEWAY_S: f64 = 300.0;
const PROVISION_TIMEOUT: Duration = Duration::from_secs(15);
/// How long a provisioned user is served before the config has them, at most.
const PROVISIONAL_FOR: Duration = Duration::from_secs(600);
/// How long the control plane's refusal to create a user stands.
const REFUSED_FOR: Duration = Duration::from_secs(60);
/// How long a provisioning failure (control plane down) stands.
const UNAVAILABLE_FOR: Duration = Duration::from_secs(5);

/// The user the control plane provisioned for a token (gwsync.ProvisionResponse).
#[derive(Debug, Clone, Deserialize)]
pub struct Provisioned {
    pub user_id: String,
    #[serde(default)]
    pub team_ids: Vec<String>,
}

#[derive(Debug, Clone)]
pub enum ProvisionError {
    /// The control plane said no: kept for a minute.
    Refused(String),
    /// It couldn't be asked, or failed: tried again shortly.
    Unavailable(String),
}

pub type BoxFuture<'a, T> = Pin<Box<dyn Future<Output = T> + Send + 'a>>;

/// Creates the user behind a verified identity token: the control plane.
pub trait Provisioner: Send + Sync {
    fn provision<'a>(&'a self, token: &'a str) -> BoxFuture<'a, Result<Provisioned, ProvisionError>>;
}

type Pending = Arc<tokio::sync::OnceCell<Result<Provisioned, ProvisionError>>>;

/// Verified tokens, issuers' keys and provisioning state.
#[derive(Default)]
pub struct Identities {
    client: OnceLock<HttpsClient>,
    provisioner: OnceLock<Arc<dyn Provisioner>>,
    keys: Mutex<HashMap<String, Arc<IssuerKeys>>>,
    tokens: Mutex<HashMap<[u8; 32], Cached>>,
    /// By SHA-256 of the email: what provisioning answered.
    outcomes: Mutex<HashMap<[u8; 32], Outcome>>,
    pending: Mutex<HashMap<[u8; 32], Pending>>,
}

struct Cached {
    version: i64,
    until_ms: i64,
    key: Arc<Key>,
}

enum Outcome {
    /// With the config version it was given under: the bump that brings the
    /// user comes after it, so a newer config without them means they're gone.
    Created(Provisioned, Instant, i64),
    Refused(String, Instant),
}

/// The gateway's identity-token state.
pub fn identities() -> &'static Identities {
    static IDS: OnceLock<Identities> = OnceLock::new();
    IDS.get_or_init(Identities::default)
}

/// What a verified token says about its caller.
#[derive(Debug)]
pub struct Verified {
    /// Index into the config's issuers.
    pub issuer: usize,
    /// The email claim, lowercased.
    pub email: String,
    /// exp, in unix seconds.
    pub exp: f64,
}

impl Identities {
    /// Set where users are provisioned; the first call wins.
    pub fn set_provisioner(&self, p: Arc<dyn Provisioner>) {
        let _ = self.provisioner.set(p);
    }

    fn client(&self) -> &HttpsClient {
        self.client.get_or_init(|| https_client(2))
    }

    /// Resolve an identity token to the caller (CloudValidator.Validate): a
    /// key-less identity carrying the user, and the team the issuer maps
    /// callers to.
    pub async fn authenticate(&self, cfg: &Config, token: &str) -> Result<Arc<Key>, String> {
        let hash = sha256(token.as_bytes());
        let now = now_ms();
        if let Some(c) = lock(&self.tokens).get(&hash)
            && c.version == cfg.version
            && now < c.until_ms
        {
            return Ok(c.key.clone());
        }
        let v = self.verify(cfg, token).await?;
        let issuer = &cfg.issuers[v.issuer];
        let email_hash = sha256(v.email.as_bytes());
        let user = match cfg.emails.get(&email_hash) {
            Some(u) => {
                lock(&self.outcomes).remove(&email_hash);
                u.clone()
            }
            None => self.provisioned(cfg, issuer, &v.email, email_hash, token).await?,
        };
        let team = Some(issuer.map_to_team.as_str()).filter(|t| !t.is_empty()).and_then(|t| cfg.teams.get(t)).cloned();
        let key = Arc::new(Key {
            id: String::new(),
            user_id: user.id.clone(),
            team_id: team.as_ref().map(|t| t.id.clone()).unwrap_or_default(),
            user: Some(user),
            team,
            allowed_models: None,
            expires_at: 0,
            scope: None,
            tags: Vec::new(),
        });
        let until_ms = ((v.exp * 1000.0) as i64).min(now + CACHE_MS);
        let mut tokens = lock(&self.tokens);
        if tokens.len() >= MAX_CACHED {
            tokens.retain(|_, c| c.version == cfg.version && now < c.until_ms);
            if tokens.len() >= MAX_CACHED {
                tokens.clear();
            }
        }
        tokens.insert(hash, Cached { version: cfg.version, until_ms, key: key.clone() });
        Ok(key)
    }

    /// The user for a verified email the config doesn't have: one provisioned
    /// earlier, or one the control plane creates now.
    async fn provisioned(
        &self,
        cfg: &Config,
        issuer: &Issuer,
        email: &str,
        hash: [u8; 32],
        token: &str,
    ) -> Result<Arc<User>, String> {
        if let Some(r) = self.outcome(&hash, cfg.version) {
            return r.map(|p| provisional_user(cfg, &p));
        }
        let no_user = || format!("no gateway user for {email}");
        if !issuer.auto_create {
            return Err(no_user());
        }
        let Some(prov) = self.provisioner.get().cloned() else { return Err(no_user()) };
        let cell = lock(&self.pending).entry(hash).or_default().clone();
        let result = cell
            .get_or_init(|| async {
                // Another request may have finished provisioning between our
                // first look and taking this slot.
                if let Some(r) = self.outcome(&hash, cfg.version) {
                    lock(&self.pending).remove(&hash);
                    return r.map_err(ProvisionError::Refused);
                }
                let r = tokio::time::timeout(PROVISION_TIMEOUT, prov.provision(token))
                    .await
                    .unwrap_or_else(|_| Err(ProvisionError::Unavailable("timed out".into())));
                let now = Instant::now();
                let outcome = match &r {
                    Ok(p) => Outcome::Created(p.clone(), now + PROVISIONAL_FOR, cfg.version),
                    Err(ProvisionError::Refused(why)) => Outcome::Refused(why.clone(), now + REFUSED_FOR),
                    Err(ProvisionError::Unavailable(why)) => {
                        eprintln!("identity token: provisioning {email} failed: {why}");
                        Outcome::Refused(format!("could not create a user for {email}"), now + UNAVAILABLE_FOR)
                    }
                };
                {
                    let mut outcomes = lock(&self.outcomes);
                    if outcomes.len() >= MAX_CACHED {
                        outcomes.retain(|_, o| match o {
                            Outcome::Created(_, until, _) | Outcome::Refused(_, until) => now < *until,
                        });
                    }
                    outcomes.insert(hash, outcome);
                }
                lock(&self.pending).remove(&hash);
                r
            })
            .await
            .clone();
        match result {
            Ok(p) => Ok(provisional_user(cfg, &p)),
            Err(ProvisionError::Refused(why)) => Err(why),
            Err(ProvisionError::Unavailable(_)) => Err(format!("could not create a user for {email}")),
        }
    }

    /// A provisioning answer still standing for this email, under config
    /// `version`.
    fn outcome(&self, hash: &[u8; 32], version: i64) -> Option<Result<Provisioned, String>> {
        let mut outcomes = lock(&self.outcomes);
        let now = Instant::now();
        let r = match outcomes.get(hash)? {
            Outcome::Created(p, until, seen) if now < *until && version <= *seen => Ok(p.clone()),
            Outcome::Refused(why, until) if now < *until => Err(why.clone()),
            _ => {
                outcomes.remove(hash);
                return None;
            }
        };
        Some(r)
    }

    /// Verify a token against the configured issuer it names: signature,
    /// exp, nbf, aud, the issuer's match claims and its email claim.
    pub async fn verify(&self, cfg: &Config, token: &str) -> Result<Verified, String> {
        if cfg.issuers.is_empty() {
            return Err("no token issuers configured".into());
        }
        let mut parts = token.split('.');
        let (Some(h), Some(p), Some(s), None) = (parts.next(), parts.next(), parts.next(), parts.next()) else {
            return Err("not a JWT".into());
        };
        #[derive(Deserialize)]
        struct Header {
            #[serde(default)]
            alg: String,
            kid: Option<String>,
        }
        let header: Header = b64url(h).and_then(|b| serde_json::from_slice(&b).ok()).ok_or("undecodable JWT header")?;
        let claims: serde_json::Map<String, Value> =
            b64url(p).and_then(|b| serde_json::from_slice(&b).ok()).ok_or("undecodable JWT payload")?;
        let sig = b64url(s).ok_or("undecodable JWT signature")?;
        let iss = claims.get("iss").and_then(Value::as_str).unwrap_or_default();
        let Some(idx) = cfg.issuers.iter().position(|i| i.url == iss) else {
            return Err(format!("issuer {iss} is not configured"));
        };
        let issuer = &cfg.issuers[idx];

        let signed = &token.as_bytes()[..h.len() + 1 + p.len()];
        self.check_signature(&issuer.url, &header.alg, header.kid.as_deref(), signed, &sig).await?;

        let now = now_ms() as f64 / 1000.0;
        let exp = claims.get("exp").and_then(Value::as_f64).ok_or("token has no exp claim")?;
        if exp < now {
            return Err("token is expired".into());
        }
        if let Some(nbf) = claims.get("nbf").and_then(Value::as_f64)
            && now + NBF_LEEWAY_S < nbf
        {
            return Err("token is not valid yet".into());
        }
        let aud_ok = match claims.get("aud") {
            Some(Value::String(a)) => *a == issuer.audience,
            Some(Value::Array(list)) => list.iter().any(|a| a.as_str() == Some(issuer.audience.as_str())),
            _ => false,
        };
        if !aud_ok {
            return Err(format!("expected audience {:?}", issuer.audience));
        }
        for (claim, want) in &issuer.match_claims {
            if claims.get(claim).and_then(Value::as_str) != Some(want.as_str()) {
                return Err(format!("claim {claim} mismatch"));
            }
        }
        let email_claim = if issuer.email_claim.is_empty() { "email" } else { issuer.email_claim.as_str() };
        let email = claims.get(email_claim).and_then(Value::as_str).filter(|e| !e.is_empty());
        let Some(email) = email else { return Err(format!("token has no {email_claim} claim")) };
        Ok(Verified { issuer: idx, email: email.to_lowercase(), exp })
    }

    /// Check the signature with the issuer's keys. Only a token the current
    /// keys can't check (none yet, or not its key id) waits for a fetch; keys
    /// past their time are used while new ones are fetched in the background.
    async fn check_signature(
        &self,
        issuer: &str,
        alg: &str,
        kid: Option<&str>,
        signed: &[u8],
        sig: &[u8],
    ) -> Result<(), String> {
        let kty = match alg {
            "RS256" => "RSA",
            "ES256" => "EC",
            _ => return Err(format!("unsupported signing algorithm {alg:?}")),
        };
        let slot = lock(&self.keys).entry(issuer.to_string()).or_default().clone();
        let mut ks = slot.current.load_full();
        if ks.fetched.is_none() || (kid.is_some() && !ks.has(kid, kty)) {
            // Nothing to check it with; for a known key id, a key the issuer
            // rotated in since we last looked.
            ks = refresh(self.client(), issuer, &slot, &ks).await?;
        } else if ks.fetched.is_some_and(|t| t.elapsed() > KEYS_TTL) {
            refresh_in_background(self.client().clone(), issuer.to_string(), slot.clone());
        }
        if !ks.algs.iter().any(|a| a == alg) {
            return Err(format!("issuer does not sign with {alg}"));
        }
        let mut found = false;
        for k in ks.keys.iter().filter(|k| k.matches(kid, kty, alg)) {
            found = true;
            if k.key.verify(signed, sig) {
                return Ok(());
            }
        }
        Err(if found { "bad signature".into() } else { "no issuer key for this token".into() })
    }
}

/// One issuer's keys. Verifications load `current` and never wait on a fetch
/// they don't need; `fetching` lets one fetch run at a time and holds when the
/// last one was tried.
#[derive(Default)]
struct IssuerKeys {
    current: ArcSwap<KeySet>,
    fetching: tokio::sync::Mutex<Option<Instant>>,
}

/// Fetch an issuer's keys, unless that was tried too recently, and swap them
/// in. `seen` is the set the caller found wanting: if another fetch replaced
/// it while this one waited its turn, that one's keys are the answer. A
/// failure keeps the keys there are.
async fn refresh(
    client: &HttpsClient,
    issuer: &str,
    slot: &IssuerKeys,
    seen: &Arc<KeySet>,
) -> Result<Arc<KeySet>, String> {
    let mut last = slot.fetching.lock().await;
    let cur = slot.current.load_full();
    if !Arc::ptr_eq(&cur, seen) {
        return Ok(cur);
    }
    let wait = if cur.fetched.is_some() { REFETCH_EVERY } else { RETRY_EMPTY };
    if last.is_some_and(|t| t.elapsed() < wait) {
        return if cur.fetched.is_some() { Ok(cur) } else { Err("issuer keys unavailable".into()) };
    }
    *last = Some(Instant::now());
    match fetch_keys(client, issuer).await {
        Ok(fresh) => {
            let fresh = Arc::new(fresh);
            slot.current.store(fresh.clone());
            Ok(fresh)
        }
        Err(e) if cur.fetched.is_some() => {
            eprintln!("identity token: refreshing keys of {issuer}: {e}");
            Ok(cur)
        }
        Err(e) => Err(format!("issuer discovery failed: {e}")),
    }
}

/// Renew keys past their time without making anyone wait for them.
fn refresh_in_background(client: HttpsClient, issuer: String, slot: Arc<IssuerKeys>) {
    // Not a task per request: only when no fetch is running or due to wait.
    match slot.fetching.try_lock() {
        Ok(last) if last.is_none_or(|t| t.elapsed() >= REFETCH_EVERY) => {}
        _ => return,
    }
    tokio::spawn(async move {
        let seen = slot.current.load_full();
        let _ = refresh(&client, &issuer, &slot, &seen).await;
    });
}

async fn fetch_keys(client: &HttpsClient, issuer: &str) -> Result<KeySet, String> {
    #[derive(Deserialize)]
    struct Discovery {
        issuer: String,
        jwks_uri: String,
        #[serde(default)]
        id_token_signing_alg_values_supported: Vec<String>,
    }
    // Tokens are trusted on the strength of these keys: fetch them only over
    // https (plain http to this machine, for a local test issuer).
    if !https_or_loopback(issuer) {
        return Err(format!("issuer {issuer:?} must use https"));
    }
    let url = format!("{}/.well-known/openid-configuration", issuer.trim_end_matches('/'));
    let d: Discovery = serde_json::from_slice(&get(client, &url).await?).map_err(|e| format!("discovery: {e}"))?;
    if d.issuer != issuer {
        return Err(format!(
            "issuer did not match the issuer returned by provider, expected {issuer:?} got {:?}",
            d.issuer
        ));
    }
    if !https_or_loopback(&d.jwks_uri) {
        return Err(format!("jwks_uri {:?} must use https", d.jwks_uri));
    }
    let jwks: Jwks = serde_json::from_slice(&get(client, &d.jwks_uri).await?).map_err(|e| format!("jwks: {e}"))?;
    // What the issuer says it signs with, of what we verify; RS256 when
    // it doesn't say (go-oidc's rule).
    let mut algs: Vec<String> =
        d.id_token_signing_alg_values_supported.into_iter().filter(|a| a == "RS256" || a == "ES256").collect();
    if algs.is_empty() {
        algs.push("RS256".into());
    }
    Ok(KeySet { keys: jwks.keys.into_iter().filter_map(Jwk::parse).collect(), algs, fetched: Some(Instant::now()) })
}

async fn get(client: &HttpsClient, url: &str) -> Result<Bytes, String> {
    let req = Request::get(url)
        .header(header::USER_AGENT, format!("llmr-gateway/{}", crate::cp::VERSION))
        .body(Full::new(Bytes::new()))
        .map_err(|e| e.to_string())?;
    let fetch = async {
        let resp = client.request(req).await.map_err(|e| format!("{url}: {e}"))?;
        if resp.status() != StatusCode::OK {
            return Err(format!("{url}: {}", resp.status()));
        }
        Limited::new(resp.into_body(), 1 << 20).collect().await.map(|b| b.to_bytes()).map_err(|e| format!("{url}: {e}"))
    };
    tokio::time::timeout(FETCH_TIMEOUT, fetch).await.map_err(|_| format!("{url}: timed out"))?
}

/// A user the control plane created that the config doesn't have yet: no
/// policy or limits of their own, and their teams' models (snapshot builder).
fn provisional_user(cfg: &Config, p: &Provisioned) -> Arc<User> {
    let mut team_models: Option<HashSet<String>> = None;
    let mut unrestricted = false;
    for t in p.team_ids.iter().filter_map(|id| cfg.teams.get(id)) {
        match &t.allowed_models {
            None => unrestricted = true,
            Some(set) => team_models.get_or_insert_default().extend(set.iter().cloned()),
        }
    }
    Arc::new(User {
        id: p.user_id.clone(),
        allowed_models: None,
        team_models: if unrestricted { None } else { team_models },
        memberships: HashMap::new(),
        scope: None,
        team_ids: p.team_ids.clone(),
        tags: Vec::new(),
    })
}

#[derive(Default, Clone)]
struct KeySet {
    keys: Vec<Jwk>,
    /// The algorithms tokens from this issuer may use.
    algs: Vec<String>,
    fetched: Option<Instant>,
}

impl KeySet {
    fn has(&self, kid: Option<&str>, kty: &str) -> bool {
        self.keys.iter().any(|k| k.kid.as_deref() == kid && k.key.kty() == kty)
    }
}

#[derive(Deserialize)]
struct Jwks {
    #[serde(default)]
    keys: Vec<RawJwk>,
}

#[derive(Deserialize)]
struct RawJwk {
    #[serde(default)]
    kty: String,
    kid: Option<String>,
    alg: Option<String>,
    #[serde(rename = "use")]
    use_: Option<String>,
    n: Option<String>,
    e: Option<String>,
    crv: Option<String>,
    x: Option<String>,
    y: Option<String>,
}

#[derive(Clone)]
struct Jwk {
    kid: Option<String>,
    alg: Option<String>,
    key: PublicKey,
}

#[derive(Clone)]
enum PublicKey {
    Rsa {
        n: Vec<u8>,
        e: Vec<u8>,
    },
    /// An uncompressed P-256 point.
    P256(Vec<u8>),
}

impl Jwk {
    fn parse(r: RawJwk) -> Option<Jwk> {
        if r.use_.as_deref().is_some_and(|u| u != "sig") {
            return None;
        }
        let key = match r.kty.as_str() {
            "RSA" => {
                let n = b64url(r.n.as_deref()?)?;
                let e = b64url(r.e.as_deref()?)?;
                let skip = n.iter().take_while(|&&b| b == 0).count();
                PublicKey::Rsa { n: n[skip..].to_vec(), e }
            }
            "EC" if r.crv.as_deref() == Some("P-256") => {
                let (x, y) = (b64url(r.x.as_deref()?)?, b64url(r.y.as_deref()?)?);
                if x.len() != 32 || y.len() != 32 {
                    return None;
                }
                let mut point = Vec::with_capacity(65);
                point.push(4);
                point.extend_from_slice(&x);
                point.extend_from_slice(&y);
                PublicKey::P256(point)
            }
            _ => return None,
        };
        Some(Jwk { kid: r.kid, alg: r.alg, key })
    }

    /// Whether this key can have signed a token with this header. A token
    /// without a key id is tried against every key of its type.
    fn matches(&self, kid: Option<&str>, kty: &str, alg: &str) -> bool {
        self.key.kty() == kty
            && (kid.is_none() || self.kid.as_deref() == kid)
            && self.alg.as_deref().is_none_or(|a| a == alg)
    }
}

impl PublicKey {
    fn kty(&self) -> &'static str {
        match self {
            PublicKey::Rsa { .. } => "RSA",
            PublicKey::P256(_) => "EC",
        }
    }

    fn verify(&self, msg: &[u8], sig: &[u8]) -> bool {
        match self {
            PublicKey::Rsa { n, e } => signature::RsaPublicKeyComponents { n, e }
                .verify(&signature::RSA_PKCS1_2048_8192_SHA256, msg, sig)
                .is_ok(),
            PublicKey::P256(point) => {
                signature::UnparsedPublicKey::new(&signature::ECDSA_P256_SHA256_FIXED, point).verify(msg, sig).is_ok()
            }
        }
    }
}

fn sha256(b: &[u8]) -> [u8; 32] {
    let d = ring::digest::digest(&ring::digest::SHA256, b);
    let mut out = [0u8; 32];
    out.copy_from_slice(d.as_ref());
    out
}

fn lock<T>(m: &Mutex<T>) -> std::sync::MutexGuard<'_, T> {
    m.lock().unwrap_or_else(|e| e.into_inner())
}

/// Decode unpadded (or padded) base64url.
fn b64url(s: &str) -> Option<Vec<u8>> {
    let s = s.trim_end_matches('=');
    if s.len() % 4 == 1 {
        return None;
    }
    let mut out = Vec::with_capacity(s.len() * 3 / 4);
    let (mut acc, mut bits) = (0u32, 0u32);
    for c in s.bytes() {
        let v = match c {
            b'A'..=b'Z' => c - b'A',
            b'a'..=b'z' => c - b'a' + 26,
            b'0'..=b'9' => c - b'0' + 52,
            b'-' => 62,
            b'_' => 63,
            _ => return None,
        };
        acc = (acc << 6) | u32::from(v);
        bits += 6;
        if bits >= 8 {
            bits -= 8;
            out.push((acc >> bits) as u8);
            acc &= (1 << bits) - 1;
        }
    }
    Some(out)
}

#[cfg(test)]
mod tests {
    use std::sync::atomic::{AtomicU64, AtomicUsize, Ordering};

    use hyper::body::Incoming;
    use hyper::service::service_fn;
    use hyper_util::rt::{TokioExecutor, TokioIo};
    use hyper_util::server::conn::auto;
    use ring::rand::SystemRandom;
    use ring::signature::{ECDSA_P256_SHA256_FIXED_SIGNING, EcdsaKeyPair, KeyPair, RsaKeyPair};
    use serde_json::json;
    use tokio::net::TcpListener;

    use super::*;
    use crate::config::Wire;
    use crate::cp::ControlPlane;

    /// A 2048-bit RSA key in PKCS#8, made for these tests only (ring can't
    /// generate RSA keys).
    const RSA_PKCS8: &str = "MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQDE-_4p2rChUAVwQ-8selszb-\
         W6GEYFh0_DYx0xlyYnIcLO3TXOUtETXAw3Y2cifkSK9rx8qCRu-bh9HGCJz5Kzj3ED0XdKsgPkYKIxDUjREpVVLOliYPw9GA1hec\
         lDKlQ3lGEA_fIKaUWfN5qxUp6eV5TqxBGDNPGcxSzkQfhn7wrmnkX3jhfm7YylnE8JsqoN9zCSlfRUvJ1Rp7Nv19gaaU3SM3V-\
         EzaF-ZXJzRIqSORluykHaYv9wQgDOhDgj6ngBf2aZfCA7ck2xxzSKgtiwrE-pmnWBaCCMkqI6PW8sEN9FMbDGfQJ3ClY9_mA_JCB\
         8S5eW2Q19Bye3hJG8HO_AgMBAAECggEAIJoO8su9uAplHAjiU4YyBKKxxB8IV8SRWvvTH8YDC9mhQwOG8uLvX444fLei9iktHE1s\
         h2UnbTE_a4Aq1M7aBNs_ZSu3MVGie0XTEjM_U1BeuX4kEk2NnGmYAq4ECsMStTAwQy3EcByu59nCwd5etO43OLPCrOiHpIlYaLQn\
         x2uU_5d3-\
         t1amyx_1VV51MiYKBJz2P6uTm3NMQgqW4RxSln4aj2VgQAnaW_7sR68uAjqClrfWJpewG4_WpsC0jDBMEP49n4QKegidx93wZ4yU\
         DuxoRejJ6YePeLhG_rhw2YO7SkguDnMEpjwfhYGz0AQCwbSqdA5kOBsp0C5sLv6gQKBgQDseSOs288fGcVRYayyLHWmO0DofFR9M\
         CinSOhEJoJy87zuT-0XqFjEASxQiw-HohTBOh68ju5VQv_AjCRf4s7wxj_kYOCjTNuRkmtjQAkXy7-\
         o1IHXYHBdH2eqOnyz42nYJ1c42gR4fjZ6YFblhQnMxVo_6WusXxoZeE7fKj7WrwKBgQDVQBbyLGSfChooJuQFOZLvOganu_Fsw25\
         FLLdckeXZnVw4IuBx0dGJK5oDKmrEDgi5Y5Kg0N5bMulQEUot7RWT-\
         YWvHbmifCjVslkLhZbmGGV0jgzu3nh0uqhpY3HBLmcfo5fnkKjgTL-\
         Si9gwx5Q_nriIKuqKVzKv7D8OsxN38QKBgD-8ZuEwXaMETyqgKOvdD9i-H8CJwjv7vvlXrUKxHnfdOg8Be-XkeghaSJds48Kq7O0\
         nvSEVZSeV51j-bM9rUluTE6V6dxGMpaGW1Vrq54iCYiYfIN_sHlz7GfXHzdSudtjLwbaRDKNtYUGatdEnCn0mtCyFvX-\
         4IlC7HIcJ2-YBAoGAMY9ehdPZYrjXUWOQdMJnqp9ispLUTgGxW_RDfQkPUR7oMsewP-\
         OSff2bIoc2HvBgCK3igutn8ewIGyqZgcjqzeQctJEE9oVCfD-hwDLA2iLrWUY2LUGY3xAOWcfZxRrCrrC3egK9rLtJfoZvrQ6FHr\
         AFlOfccGBJkGcoVIM70BECgYEAm1zOqJkbT1gybNLxZw3KQ9WBVds-fiJNfAw-RbQaM73RAfw6XwM8xw6SRVnujHt1r78qg9lOLj\
         NThC0oXAmc1tSfcFmDeRb5pruX9xZd997nfzRrGcppHhSNF3ljM4splbejR1uui0_j25wW-bDOyF4Dpl904UT5RC4cM9GIXtA";

    fn b64(b: &[u8]) -> String {
        const A: &[u8] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
        let mut out = String::new();
        for c in b.chunks(3) {
            let n = c.iter().enumerate().fold(0u32, |n, (i, &x)| n | u32::from(x) << (16 - 8 * i));
            for i in 0..=c.len() {
                out.push(A[(n >> (18 - 6 * i) & 63) as usize] as char);
            }
        }
        out
    }

    trait Refused {
        fn refused(self) -> String;
    }

    impl<T> Refused for Result<T, String> {
        fn refused(self) -> String {
            match self {
                Ok(_) => panic!("accepted"),
                Err(e) => e,
            }
        }
    }

    fn hex(b: &[u8]) -> String {
        b.iter().map(|x| format!("{x:02x}")).collect()
    }

    type Handler = Arc<dyn Fn(String, http::HeaderMap, Bytes) -> BoxFuture<'static, (u16, String)> + Send + Sync>;

    /// An HTTP server answering every request with `h`; returns its base URL.
    async fn serve(h: Handler) -> String {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let url = format!("http://{}", listener.local_addr().unwrap());
        tokio::spawn(async move {
            while let Ok((conn, _)) = listener.accept().await {
                let h = h.clone();
                tokio::spawn(async move {
                    let svc = service_fn(move |req: Request<Incoming>| {
                        let h = h.clone();
                        async move {
                            let path = req.uri().path().to_string();
                            let headers = req.headers().clone();
                            let body = req.into_body().collect().await.map(|b| b.to_bytes()).unwrap_or_default();
                            let (status, text) = h(path, headers, body).await;
                            Ok::<_, std::convert::Infallible>(
                                http::Response::builder().status(status).body(Full::new(Bytes::from(text))).unwrap(),
                            )
                        }
                    });
                    let _ = auto::Builder::new(TokioExecutor::new()).serve_connection(TokioIo::new(conn), svc).await;
                });
            }
        });
        url
    }

    /// An OIDC issuer: discovery, a JWKS that tests can change, and its keys.
    struct Idp {
        url: String,
        jwks: Arc<Mutex<Value>>,
        fetches: Arc<AtomicUsize>,
        /// How long the JWKS endpoint takes to answer, in milliseconds.
        delay_ms: Arc<AtomicU64>,
        rsa: RsaKeyPair,
        ec: EcdsaKeyPair,
    }

    impl Idp {
        async fn new() -> Idp {
            let _ = rustls::crypto::ring::default_provider().install_default();
            let rsa = RsaKeyPair::from_pkcs8(&b64url(RSA_PKCS8).unwrap()).unwrap();
            let rng = SystemRandom::new();
            let pkcs8 = EcdsaKeyPair::generate_pkcs8(&ECDSA_P256_SHA256_FIXED_SIGNING, &rng).unwrap();
            let ec = EcdsaKeyPair::from_pkcs8(&ECDSA_P256_SHA256_FIXED_SIGNING, pkcs8.as_ref(), &rng).unwrap();
            let jwks = Arc::new(Mutex::new(json!({"keys": []})));
            let fetches = Arc::new(AtomicUsize::new(0));
            let delay_ms = Arc::new(AtomicU64::new(0));
            let me: Arc<OnceLock<String>> = Arc::default();
            let (j, f, m, d) = (jwks.clone(), fetches.clone(), me.clone(), delay_ms.clone());
            let url = serve(Arc::new(move |path, _, _| {
                let delay = Duration::from_millis(d.load(Ordering::SeqCst));
                let base = m.get().cloned().unwrap_or_default();
                let reply = match path.as_str() {
                    "/.well-known/openid-configuration" => (
                        200,
                        json!({"issuer": base, "jwks_uri": format!("{base}/jwks"),
                            "id_token_signing_alg_values_supported": ["RS256", "ES256"]})
                        .to_string(),
                    ),
                    "/jwks" => {
                        f.fetch_add(1, Ordering::SeqCst);
                        (200, lock(&j).to_string())
                    }
                    _ => (404, String::new()),
                };
                let slow = path == "/jwks";
                Box::pin(async move {
                    if slow {
                        tokio::time::sleep(delay).await;
                    }
                    reply
                })
            }))
            .await;
            me.set(url.clone()).unwrap();
            let idp = Idp { url, jwks, fetches, delay_ms, rsa, ec };
            idp.publish(&["k1", "ec1"]);
            idp
        }

        /// Publish the RSA key and the EC key under these key ids.
        fn publish(&self, kids: &[&str]) {
            let rsa = signature::RsaPublicKeyComponents::<Vec<u8>>::from(self.rsa.public());
            let point = self.ec.public_key().as_ref();
            let keys: Vec<Value> = kids
                .iter()
                .map(|kid| {
                    if kid.starts_with("ec") {
                        json!({"kty": "EC", "crv": "P-256", "kid": kid, "use": "sig",
                            "x": b64(&point[1..33]), "y": b64(&point[33..65])})
                    } else {
                        json!({"kty": "RSA", "kid": kid, "alg": "RS256", "n": b64(&rsa.n), "e": b64(&rsa.e)})
                    }
                })
                .collect();
            *lock(&self.jwks) = json!({ "keys": keys });
        }

        fn fetches(&self) -> usize {
            self.fetches.load(Ordering::SeqCst)
        }

        /// A token signed with alg under kid, with these claims over the defaults.
        fn mint(&self, alg: &str, kid: &str, claims: Value) -> String {
            let mut payload = json!({"iss": self.url, "aud": "llmr", "exp": now_ms() / 1000 + 3600,
                "email": "Dev@Example.com", "tid": "tenant"});
            for (k, v) in claims.as_object().unwrap() {
                if v.is_null() {
                    payload.as_object_mut().unwrap().remove(k);
                } else {
                    payload[k] = v.clone();
                }
            }
            let head = b64(json!({"alg": alg, "kid": kid, "typ": "JWT"}).to_string().as_bytes());
            let signed = format!("{head}.{}", b64(payload.to_string().as_bytes()));
            let rng = SystemRandom::new();
            let sig = match alg {
                "ES256" => self.ec.sign(&rng, signed.as_bytes()).unwrap().as_ref().to_vec(),
                _ => {
                    let mut sig = vec![0; self.rsa.public().modulus_len()];
                    self.rsa.sign(&signature::RSA_PKCS1_SHA256, &rng, signed.as_bytes(), &mut sig).unwrap();
                    sig
                }
            };
            format!("{signed}.{}", b64(&sig))
        }

        fn config(&self, version: i64, auto_create: bool) -> Config {
            let dev = hex(&sha256(b"dev@example.com"));
            let wire: Wire = serde_json::from_value(json!({
                "schema": 2, "version": version, "group": {"id": "g", "name": "g"},
                "users": [{"id": "u1", "allowed_models": null, "team_models": null, "team_ids": ["t1"]}],
                "teams": [{"id": "t1", "allowed_models": ["m"]}],
                "issuers": [{"url": self.url, "audience": "llmr", "email_claim": "email",
                    "match": {"tid": "tenant"}, "map_to_team": "t1", "auto_create": auto_create}],
                "emails": {dev: "u1"},
            }))
            .unwrap();
            Config::build(wire).unwrap()
        }
    }

    #[test]
    fn decodes_base64url() {
        assert_eq!(b64url("aGk").unwrap(), b"hi");
        assert_eq!(b64url("aGk=").unwrap(), b"hi");
        assert_eq!(b64url("-_8").unwrap(), [0xfb, 0xff]);
        assert!(b64url("a").is_none() && b64url("a+b/").is_none());
        let all: Vec<u8> = (0..=255).collect();
        assert_eq!(b64url(&b64(&all)).unwrap(), all);
    }

    #[tokio::test]
    async fn verifies_rs256_and_es256() {
        let idp = Idp::new().await;
        let ids = Identities::default();
        let cfg = idp.config(1, false);
        for (alg, kid) in [("RS256", "k1"), ("ES256", "ec1")] {
            let key = ids.authenticate(&cfg, &idp.mint(alg, kid, json!({}))).await.unwrap();
            assert_eq!(key.user.as_ref().unwrap().id, "u1", "{alg}");
            assert_eq!(key.team.as_ref().unwrap().id, "t1", "the issuer maps callers to t1");
            assert!(key.id.is_empty() && key.scope.is_none(), "an identity has no key of its own");
            assert!(key.model_allowed(&["m"]) && !key.model_allowed(&["other"]), "the team's policy holds");
        }
        // aud may be a list, and nbf a little ahead of our clock.
        let now = now_ms() / 1000;
        let ok = idp.mint("RS256", "k1", json!({"aud": ["x", "llmr"], "nbf": now + 60}));
        ids.verify(&cfg, &ok).await.unwrap();
        // No kid: every key of the type is tried.
        let mut no_kid = idp.mint("RS256", "", json!({}));
        let head = b64(json!({"alg": "RS256"}).to_string().as_bytes());
        let rest = no_kid.split_once('.').unwrap().1.to_string();
        no_kid = format!("{head}.{rest}");
        let resigned = {
            let (signed, _) = no_kid.rsplit_once('.').unwrap();
            let mut sig = vec![0; idp.rsa.public().modulus_len()];
            idp.rsa.sign(&signature::RSA_PKCS1_SHA256, &SystemRandom::new(), signed.as_bytes(), &mut sig).unwrap();
            format!("{signed}.{}", b64(&sig))
        };
        ids.verify(&cfg, &resigned).await.unwrap();
    }

    #[tokio::test]
    async fn rejects_bad_tokens() {
        let idp = Idp::new().await;
        let ids = Identities::default();
        let cfg = idp.config(1, false);
        let now = now_ms() / 1000;
        let good = idp.mint("RS256", "k1", json!({}));
        let (signed, _) = good.rsplit_once('.').unwrap();
        let other_payload = b64(json!({"iss": idp.url, "aud": "llmr", "exp": now + 3600,
            "email": "admin@example.com", "tid": "tenant"})
        .to_string()
        .as_bytes());
        let tampered =
            format!("{}.{other_payload}.{}", signed.split('.').next().unwrap(), good.rsplit_once('.').unwrap().1);
        let es_as_rs = {
            let t = idp.mint("ES256", "ec1", json!({}));
            let (_, rest) = t.split_once('.').unwrap();
            format!("{}.{rest}", b64(json!({"alg": "RS256", "kid": "k1"}).to_string().as_bytes()))
        };
        let cases = [
            ("expired", idp.mint("RS256", "k1", json!({"exp": now - 10})), "expired"),
            ("no exp", idp.mint("RS256", "k1", json!({"exp": null})), "exp"),
            ("not yet valid", idp.mint("RS256", "k1", json!({"nbf": now + 600})), "not valid yet"),
            ("wrong audience", idp.mint("RS256", "k1", json!({"aud": "other"})), "audience"),
            ("wrong issuer", idp.mint("RS256", "k1", json!({"iss": "https://evil.example.com"})), "not configured"),
            ("wrong tenant", idp.mint("RS256", "k1", json!({"tid": "evil"})), "claim tid mismatch"),
            ("no email", idp.mint("RS256", "k1", json!({"email": null})), "no email claim"),
            ("tampered", tampered, "bad signature"),
            ("signature of another key", es_as_rs, "bad signature"),
            (
                "alg none",
                format!("{}.{}.", b64(br#"{"alg":"none"}"#), signed.split('.').nth(1).unwrap()),
                "unsupported",
            ),
            ("HS256", idp.mint("HS256", "k1", json!({})), "unsupported"),
            ("garbage", "not.a.jwt".to_string(), "undecodable"),
            ("two parts", "a.b".to_string(), "not a JWT"),
            ("unknown user", idp.mint("RS256", "k1", json!({"email": "stranger@example.com"})), "no gateway user"),
        ];
        for (name, token, want) in cases {
            let err = match ids.authenticate(&cfg, &token).await {
                Ok(_) => panic!("{name}: accepted"),
                Err(e) => e,
            };
            assert!(err.contains(want), "{name}: {err}");
        }
        let empty = Config::build(
            serde_json::from_value(json!({"schema": 2, "version": 1, "group": {"id": "g", "name": "g"}})).unwrap(),
        )
        .unwrap();
        assert!(ids.authenticate(&empty, &good).await.refused().contains("no token issuers"));
    }

    #[tokio::test]
    async fn refetches_keys_for_an_unknown_kid_once_a_minute() {
        let idp = Idp::new().await;
        idp.publish(&["k1"]);
        let ids = Identities::default();
        let cfg = idp.config(1, false);
        ids.verify(&cfg, &idp.mint("RS256", "k1", json!({}))).await.unwrap();
        assert_eq!(idp.fetches(), 1);

        // A minute on, the issuer rotates in a key: a token under it
        // refetches the keys.
        idp.publish(&["k1", "ec2"]);
        let a_minute_ago = Instant::now().checked_sub(REFETCH_EVERY).unwrap();
        *lock(&ids.keys)[&idp.url].fetching.try_lock().unwrap() = Some(a_minute_ago);
        ids.verify(&cfg, &idp.mint("ES256", "ec2", json!({}))).await.unwrap();
        assert_eq!(idp.fetches(), 2);

        // A kid nobody publishes doesn't make every request fetch again.
        for _ in 0..3 {
            let err = ids.verify(&cfg, &idp.mint("RS256", "k9", json!({}))).await.refused();
            assert!(err.contains("no issuer key"), "{err}");
        }
        assert_eq!(idp.fetches(), 2, "at most one refetch a minute");
        ids.verify(&cfg, &idp.mint("RS256", "k1", json!({}))).await.unwrap();
        assert_eq!(idp.fetches(), 2);
    }

    #[tokio::test]
    async fn fetches_keys_only_over_https() {
        let _ = rustls::crypto::ring::default_provider().install_default();
        let client = https_client(1);
        let err = fetch_keys(&client, "http://idp.example").await.err().unwrap();
        assert!(err.contains("must use https"), "{err}");
        // A discovery document that points at plain-http keys elsewhere.
        let me: Arc<OnceLock<String>> = Arc::default();
        let m = me.clone();
        let url = serve(Arc::new(move |_, _, _| {
            let base = m.get().cloned().unwrap_or_default();
            let doc = json!({"issuer": base, "jwks_uri": "http://keys.example/jwks"}).to_string();
            Box::pin(async move { (200, doc) })
        }))
        .await;
        me.set(url.clone()).unwrap();
        let err = fetch_keys(&client, &url).await.err().unwrap();
        assert!(err.contains("jwks_uri") && err.contains("must use https"), "{err}");
    }

    /// The current keys, as if fetched `age` ago.
    fn age_keys(ids: &Identities, issuer: &str, age: Duration) {
        let slot = lock(&ids.keys)[issuer].clone();
        let mut ks = KeySet::clone(&slot.current.load());
        ks.fetched = Some(Instant::now().checked_sub(age).unwrap());
        slot.current.store(Arc::new(ks));
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 4)]
    async fn a_slow_key_fetch_holds_up_only_those_who_need_it() {
        let idp = Idp::new().await;
        let ids = Arc::new(Identities::default());
        let cfg = Arc::new(idp.config(1, false));
        ids.verify(&cfg, &idp.mint("RS256", "k1", json!({}))).await.unwrap();
        idp.delay_ms.store(1500, Ordering::SeqCst);

        // A token with a key id nobody publishes (anyone can send one) makes
        // a slow refetch...
        *lock(&ids.keys)[&idp.url].fetching.try_lock().unwrap() = Some(Instant::now() - REFETCH_EVERY);
        let unknown = {
            let (ids, cfg, t) = (ids.clone(), cfg.clone(), idp.mint("RS256", "k9", json!({})));
            tokio::spawn(async move { ids.verify(&cfg, &t).await })
        };
        tokio::time::sleep(Duration::from_millis(200)).await;
        assert_eq!(idp.fetches(), 2, "the refetch is under way");
        // ...that a token under a key we have doesn't wait for.
        let t = Instant::now();
        ids.verify(&cfg, &idp.mint("ES256", "ec1", json!({}))).await.unwrap();
        assert!(t.elapsed() < Duration::from_millis(500), "waited {:?} for a fetch it didn't need", t.elapsed());
        assert!(unknown.await.unwrap().refused().contains("no issuer key"));

        // Keys past their hour go on being used while new ones are fetched.
        age_keys(&ids, &idp.url, KEYS_TTL + Duration::from_secs(1));
        *lock(&ids.keys)[&idp.url].fetching.try_lock().unwrap() = Some(Instant::now() - REFETCH_EVERY);
        let t = Instant::now();
        ids.verify(&cfg, &idp.mint("RS256", "k1", json!({}))).await.unwrap();
        assert!(t.elapsed() < Duration::from_millis(500), "waited {:?} for a background renewal", t.elapsed());
        tokio::time::sleep(Duration::from_millis(1800)).await;
        assert_eq!(idp.fetches(), 3, "renewed in the background");
        let renewed = lock(&ids.keys)[&idp.url].current.load().fetched.unwrap();
        assert!(renewed.elapsed() < Duration::from_secs(5));
    }

    #[tokio::test]
    async fn caches_verified_tokens_per_config_version() {
        let idp = Idp::new().await;
        let ids = Identities::default();
        let cfg = idp.config(1, false);
        let token = idp.mint("RS256", "k1", json!({}));
        let first = ids.authenticate(&cfg, &token).await.unwrap();
        // With the issuer's keys forgotten, only the cache can answer.
        lock(&ids.keys).clear();
        let again = ids.authenticate(&cfg, &token).await.unwrap();
        assert!(Arc::ptr_eq(&first, &again));
        assert_eq!(idp.fetches(), 1);
        // A new config version verifies again: the user may be gone.
        let mut next = idp.config(2, false);
        next.emails.clear();
        assert!(ids.authenticate(&next, &token).await.refused().contains("no gateway user"));
        assert_eq!(idp.fetches(), 2);
        // A token about to expire is kept no longer than it lives.
        let short = idp.mint("RS256", "k1", json!({"exp": now_ms() / 1000 + 1}));
        ids.authenticate(&cfg, &short).await.unwrap();
        let until = lock(&ids.tokens).get(&sha256(short.as_bytes())).unwrap().until_ms;
        assert!(until <= now_ms() + 1000);
    }

    /// A control plane that creates new@example.com and refuses anyone else,
    /// slowly, counting the calls.
    struct FakeCp {
        calls: AtomicUsize,
    }

    impl Provisioner for FakeCp {
        fn provision<'a>(&'a self, token: &'a str) -> BoxFuture<'a, Result<Provisioned, ProvisionError>> {
            Box::pin(async move {
                self.calls.fetch_add(1, Ordering::SeqCst);
                tokio::time::sleep(Duration::from_millis(100)).await;
                provision_answer(token)
            })
        }
    }

    fn provision_answer(token: &str) -> Result<Provisioned, ProvisionError> {
        let claims: Value = serde_json::from_slice(&b64url(token.split('.').nth(1).unwrap()).unwrap()).unwrap();
        if claims["email"].as_str().unwrap().eq_ignore_ascii_case("new@example.com") {
            Ok(Provisioned { user_id: "u-new".into(), team_ids: vec!["t1".into()] })
        } else {
            Err(ProvisionError::Refused("no gateway user".into()))
        }
    }

    async fn concurrently(
        ids: &Arc<Identities>,
        cfg: &Arc<Config>,
        tokens: Vec<String>,
    ) -> Vec<Result<Arc<Key>, String>> {
        let tasks: Vec<_> = tokens
            .into_iter()
            .map(|t| {
                let (ids, cfg) = (ids.clone(), cfg.clone());
                tokio::spawn(async move { ids.authenticate(&cfg, &t).await })
            })
            .collect();
        let mut out = Vec::new();
        for t in tasks {
            out.push(t.await.unwrap());
        }
        out
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 4)]
    async fn provisions_once_and_remembers_the_answer() {
        let idp = Idp::new().await;
        let ids = Arc::new(Identities::default());
        let cp = Arc::new(FakeCp { calls: AtomicUsize::new(0) });
        ids.set_provisioner(cp.clone());
        let cfg = Arc::new(idp.config(1, true));

        // Ten first requests at once, each with its own token: one call.
        let tokens = (0..10).map(|i| idp.mint("RS256", "k1", json!({"email": "New@Example.com", "jti": i}))).collect();
        for r in concurrently(&ids, &cfg, tokens).await {
            let key = r.unwrap();
            let user = key.user.as_ref().unwrap();
            assert_eq!(user.id, "u-new");
            assert_eq!(user.team_ids, ["t1"]);
            assert!(key.model_allowed(&["m"]) && !key.model_allowed(&["x"]), "a new user gets their team's models");
        }
        assert_eq!(cp.calls.load(Ordering::SeqCst), 1);
        // Until the config has them, later tokens reuse the answer.
        ids.authenticate(&cfg, &idp.mint("RS256", "k1", json!({"email": "new@example.com", "jti": "x"})))
            .await
            .unwrap();
        assert_eq!(cp.calls.load(Ordering::SeqCst), 1);

        // A newer config that still doesn't have them asks again: they may
        // have been disabled since.
        let newer = Arc::new(idp.config(2, true));
        ids.authenticate(&newer, &idp.mint("RS256", "k1", json!({"email": "new@example.com", "jti": "z"})))
            .await
            .unwrap();
        assert_eq!(cp.calls.load(Ordering::SeqCst), 2);

        // A refusal stands for a minute.
        let tokens =
            (0..5).map(|i| idp.mint("RS256", "k1", json!({"email": "nobody@example.com", "jti": i}))).collect();
        for r in concurrently(&ids, &cfg, tokens).await {
            assert!(r.refused().contains("no gateway user"));
        }
        ids.authenticate(&cfg, &idp.mint("RS256", "k1", json!({"email": "nobody@example.com", "jti": "y"})))
            .await
            .refused();
        assert_eq!(cp.calls.load(Ordering::SeqCst), 3);

        // An issuer that doesn't create users never asks.
        let manual = Arc::new(idp.config(1, false));
        let r = ids.authenticate(&manual, &idp.mint("RS256", "k1", json!({"email": "other@example.com"}))).await;
        assert!(r.refused().contains("no gateway user for other@example.com"));
        assert_eq!(cp.calls.load(Ordering::SeqCst), 3);
    }

    /// The real control-plane client against a stub control plane.
    #[tokio::test(flavor = "multi_thread", worker_threads = 4)]
    async fn provisions_through_the_control_plane() {
        let idp = Idp::new().await;
        let calls = Arc::new(AtomicUsize::new(0));
        let c = calls.clone();
        let cp_url = serve(Arc::new(move |path, headers, body| {
            let c = c.clone();
            Box::pin(async move {
                let bearer = headers.get("authorization").and_then(|v| v.to_str().ok()).unwrap_or("").to_string();
                match path.as_str() {
                    "/gw/v1/join" if bearer == "Bearer llmrg_test" => (
                        200,
                        json!({"instance_id": "i1", "session": "sess", "expires_at_ms": now_ms() + 900_000})
                            .to_string(),
                    ),
                    "/gw/v1/provision" if bearer == "Bearer sess" => {
                        c.fetch_add(1, Ordering::SeqCst);
                        tokio::time::sleep(Duration::from_millis(100)).await;
                        let req: Value = serde_json::from_slice(&body).unwrap();
                        match provision_answer(req["token"].as_str().unwrap()) {
                            Ok(p) => (200, json!({"user_id": p.user_id, "team_ids": p.team_ids}).to_string()),
                            Err(_) => (403, "no gateway user for nobody@example.com".to_string()),
                        }
                    }
                    _ => (401, String::new()),
                }
            })
        }))
        .await;
        let ids = Arc::new(Identities::default());
        ids.set_provisioner(Arc::new(ControlPlane::new(&cp_url, "llmrg_test".into(), "test".into())));
        let cfg = Arc::new(idp.config(1, true));

        let tokens = (0..5).map(|i| idp.mint("RS256", "k1", json!({"email": "new@example.com", "jti": i}))).collect();
        for r in concurrently(&ids, &cfg, tokens).await {
            assert_eq!(r.unwrap().user.as_ref().unwrap().id, "u-new");
        }
        assert_eq!(calls.load(Ordering::SeqCst), 1);

        for i in 0..2 {
            let t = idp.mint("RS256", "k1", json!({"email": "nobody@example.com", "jti": i}));
            let err = ids.authenticate(&cfg, &t).await.refused();
            assert!(err.contains("no gateway user for nobody@example.com"), "{err}");
        }
        assert_eq!(calls.load(Ordering::SeqCst), 2, "a refusal is kept");
    }
}
