//! Google access tokens for Vertex AI providers (Go: internal/provider/gcp.go).
//!
//! - gcp_sa: the provider's service account key, sent by the control plane, signs
//!   a JWT (RS256) that Google exchanges for an access token. Only a real
//!   service_account key is accepted: any other credential JSON names its own
//!   token endpoint, so a pasted blob could redirect token minting.
//! - gcp_adc: application default credentials on the gateway itself.
//!   GOOGLE_APPLICATION_CREDENTIALS (a service account key or an authorized_user
//!   file), else gcloud's application_default_credentials.json, else the metadata
//!   server on Google Cloud. external_account (workload identity federation)
//!   files aren't supported.

use std::collections::HashMap;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::Duration;

use bytes::Bytes;
use http::{HeaderValue, Request, header};
use http_body_util::Full;
use ring::rand::SystemRandom;
use ring::signature::{RSA_PKCS1_SHA256, RsaKeyPair};
use rustls::pki_types::PrivateKeyDer;
use rustls::pki_types::pem::PemObject;
use serde::Deserialize;

use super::token::{Bearer, FetchError, Fetched, RefreshingToken, Refused, TokenSource};
use super::{b64url, bearer_from, call, form, https_or_loopback, read_small};

const SCOPE: &str = "https://www.googleapis.com/auth/cloud-platform";
const TOKEN_URI: &str = "https://oauth2.googleapis.com/token";
/// The metadata server, by address as Google's own libraries use it.
const METADATA_HOST: &str = "169.254.169.254";
const MAX_FILE: u64 = 64 << 10;

/// A service account key.
pub struct ServiceAccount {
    email: String,
    key_id: String,
    token_uri: String,
    key: RsaKeyPair,
}

/// A credential JSON file.
enum Creds {
    ServiceAccount(Box<ServiceAccount>),
    AuthorizedUser { client_id: String, client_secret: String, refresh_token: String, token_uri: String },
}

#[derive(Deserialize)]
struct File {
    #[serde(default, rename = "type")]
    kind: String,
    #[serde(default)]
    client_email: String,
    #[serde(default)]
    private_key: String,
    #[serde(default)]
    private_key_id: String,
    #[serde(default)]
    token_uri: String,
    #[serde(default)]
    client_id: String,
    #[serde(default)]
    client_secret: String,
    #[serde(default)]
    refresh_token: String,
}

/// A credential JSON: a stored key (`allow_user` false) or a file on this
/// machine. A stored key's token_uri must be https; a file's may be plain
/// http to this machine, for a local test setup.
fn parse(json: &str, allow_user: bool) -> Result<Creds, String> {
    parse_with(json, allow_user, allow_user)
}

/// `parse`, with the loopback allowance for token_uri given separately, so
/// tests can point a stored key at a local token server.
fn parse_with(json: &str, allow_user: bool, loopback: bool) -> Result<Creds, String> {
    let f: File = serde_json::from_str(json).map_err(|e| format!("the credential JSON doesn't parse: {e}"))?;
    let token_uri = if f.token_uri.is_empty() { TOKEN_URI.to_string() } else { f.token_uri };
    let ok = if loopback { https_or_loopback(&token_uri) } else { token_uri.starts_with("https://") };
    if !ok {
        return Err(format!("token_uri {token_uri:?} must use https"));
    }
    match f.kind.as_str() {
        "service_account" => {
            if f.client_email.is_empty() {
                return Err("the service account key has no client_email".into());
            }
            let der = PrivateKeyDer::from_pem_slice(f.private_key.as_bytes())
                .map_err(|_| "the service account key's private_key isn't a PEM private key".to_string())?;
            let key = match &der {
                PrivateKeyDer::Pkcs8(k) => RsaKeyPair::from_pkcs8(k.secret_pkcs8_der()),
                PrivateKeyDer::Pkcs1(k) => RsaKeyPair::from_der(k.secret_pkcs1_der()),
                _ => return Err("the service account key's private_key isn't an RSA key".into()),
            }
            .map_err(|e| format!("the service account key's private_key isn't usable: {e}"))?;
            Ok(Creds::ServiceAccount(Box::new(ServiceAccount {
                email: f.client_email,
                key_id: f.private_key_id,
                token_uri,
                key,
            })))
        }
        "authorized_user" if allow_user => Ok(Creds::AuthorizedUser {
            client_id: f.client_id,
            client_secret: f.client_secret,
            refresh_token: f.refresh_token,
            token_uri,
        }),
        "external_account" if allow_user => {
            Err("external_account credentials (workload identity federation) aren't supported \
                                   by the gateway: use a service account key, or the metadata server on Google Cloud"
                .into())
        }
        other if allow_user => {
            Err(format!("{other:?} credentials aren't supported: use service_account or authorized_user"))
        }
        other => Err(format!("the stored JSON is a {other:?} credential, not a service_account key")),
    }
}

impl ServiceAccount {
    /// The signed JWT that's exchanged for an access token.
    fn assertion(&self, now: u64) -> Result<String, String> {
        let mut head = serde_json::json!({"alg": "RS256", "typ": "JWT"});
        if !self.key_id.is_empty() {
            head["kid"] = self.key_id.clone().into();
        }
        let claims = serde_json::json!({
            "iss": self.email,
            "scope": SCOPE,
            "aud": self.token_uri,
            "iat": now,
            "exp": now + 3600,
        });
        let signed = format!("{}.{}", b64url(head.to_string().as_bytes()), b64url(claims.to_string().as_bytes()));
        let mut sig = vec![0; self.key.public().modulus_len()];
        self.key
            .sign(&RSA_PKCS1_SHA256, &SystemRandom::new(), signed.as_bytes(), &mut sig)
            .map_err(|_| "signing the token request failed".to_string())?;
        Ok(format!("{signed}.{}", b64url(&sig)))
    }

    async fn token(&self) -> Result<(HeaderValue, Duration), FetchError> {
        let assertion = self.assertion(crate::now_ms() as u64 / 1000)?;
        let body = form(&[("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer"), ("assertion", &assertion)]);
        post(&self.token_uri, body).await
    }
}

async fn post(url: &str, body: String) -> Result<(HeaderValue, Duration), FetchError> {
    let req = Request::post(url)
        .header(header::CONTENT_TYPE, "application/x-www-form-urlencoded")
        .body(Full::new(Bytes::from(body)))
        .map_err(|e| e.to_string())?;
    let (status, v, text) = call(req).await.map_err(|e| FetchError::new("the token endpoint did not answer", e))?;
    bearer_from(status, &v, &text)
}

impl Creds {
    async fn token(&self) -> Result<(HeaderValue, Duration), FetchError> {
        match self {
            Creds::ServiceAccount(sa) => sa.token().await,
            Creds::AuthorizedUser { client_id, client_secret, refresh_token, token_uri } => {
                let body = form(&[
                    ("grant_type", "refresh_token"),
                    ("client_id", client_id),
                    ("client_secret", client_secret),
                    ("refresh_token", refresh_token),
                ]);
                post(token_uri, body).await
            }
        }
    }
}

/// A gcp_sa provider's stored key.
pub struct Key(ServiceAccount);

impl TokenSource for Key {
    fn what(&self) -> &str {
        "google token request"
    }

    async fn fetch(&self, _usable: bool) -> Result<Fetched, FetchError> {
        let (h, d) = self.0.token().await.map_err(|e| e.context("google token (service account key)"))?;
        Ok(Fetched::Token(h, d))
    }
}

/// Application default credentials, looked up on every fetch so a rotated key
/// file is picked up.
pub struct Adc {
    file: Option<String>,
    well_known: Option<String>,
    metadata: String,
}

impl Adc {
    fn from_vars(var: impl Fn(&str) -> Option<String>) -> Adc {
        let gcloud = var("CLOUDSDK_CONFIG").or_else(|| var("HOME").map(|h| format!("{h}/.config/gcloud")));
        Adc {
            file: var("GOOGLE_APPLICATION_CREDENTIALS"),
            well_known: gcloud.map(|d| format!("{d}/application_default_credentials.json")),
            metadata: format!("http://{}", var("GCE_METADATA_HOST").unwrap_or_else(|| METADATA_HOST.into())),
        }
    }

    fn file(&self) -> Option<&str> {
        self.file.as_deref().or(self.well_known.as_deref().filter(|p| std::path::Path::new(p).is_file()))
    }

    fn creds(&self, path: &str) -> Result<Creds, String> {
        parse(&read_small(path, MAX_FILE)?, true).map_err(|e| format!("{path}: {e}"))
    }

    async fn metadata(&self) -> Result<(HeaderValue, Duration), FetchError> {
        let url = format!(
            "{}/computeMetadata/v1/instance/service-accounts/default/token?{}",
            self.metadata,
            form(&[("scopes", SCOPE)])
        );
        let req = Request::get(url)
            .header("metadata-flavor", "Google")
            .body(Full::new(Bytes::new()))
            .map_err(|e| e.to_string())?;
        let (status, v, text) = call(req).await.map_err(|e| {
            let none = "no credentials: GOOGLE_APPLICATION_CREDENTIALS isn't set and the metadata server didn't answer";
            FetchError::new(none, format!("{none} ({e})"))
        })?;
        bearer_from(status, &v, &text).map_err(|e| e.context("metadata server"))
    }
}

impl TokenSource for Adc {
    fn what(&self) -> &str {
        "google token request"
    }

    async fn fetch(&self, _usable: bool) -> Result<Fetched, FetchError> {
        let wrap = |e: FetchError| e.context("google application default credentials");
        // The file's path, and what is wrong with it, are only logged.
        let (h, d) = match self.file() {
            Some(path) => self
                .creds(path)
                .map_err(|e| wrap(FetchError::new("the credentials file is unusable", e)))?
                .token()
                .await
                .map_err(|e| wrap(FetchError::new(e.public, format!("{path}: {}", e.detail))))?,
            None => self.metadata().await.map_err(wrap)?,
        };
        Ok(Fetched::Token(h, d))
    }
}

/// The token every gcp_adc provider on this gateway shares.
pub fn adc() -> Arc<dyn Bearer> {
    static SHARED: OnceLock<Arc<dyn Bearer>> = OnceLock::new();
    SHARED
        .get_or_init(|| {
            let adc = Adc::from_vars(|n| std::env::var(n).ok().filter(|v| !v.is_empty()));
            // Said once at startup; every failed fetch says it again.
            if let Some(path) = adc.file()
                && let Err(e) = adc.creds(path)
            {
                eprintln!("google application default credentials: {e}");
            }
            Arc::new(RefreshingToken::new(adc))
        })
        .clone()
}

/// Set only by tests (allow_loopback_token_uri_for_tests).
static LOOPBACK_TOKEN_URI: AtomicBool = AtomicBool::new(false);

/// Lets stored keys name a token endpoint on this machine, for tests that
/// serve their own. The gateway never calls it: a stored key's token_uri is
/// https only.
#[doc(hidden)]
pub fn allow_loopback_token_uri_for_tests() {
    LOOPBACK_TOKEN_URI.store(true, Ordering::Relaxed);
}

/// The token for a gcp_sa provider's key. Outlives config reloads, so a reload
/// doesn't mint again; a re-pasted key is a new entry.
pub fn service_account(provider: &str, json: &str) -> Arc<dyn Bearer> {
    type Keys = HashMap<(String, [u8; 32]), Arc<dyn Bearer>>;
    static KEYS: OnceLock<Mutex<Keys>> = OnceLock::new();
    let mut digest = [0u8; 32];
    digest.copy_from_slice(ring::digest::digest(&ring::digest::SHA256, json.as_bytes()).as_ref());
    let mut keys = KEYS.get_or_init(Default::default).lock().unwrap();
    keys.entry((provider.to_string(), digest))
        .or_insert_with(|| {
            let parsed = if json.trim().is_empty() {
                Err("no service account JSON stored".to_string())
            } else {
                parse_with(json, false, LOOPBACK_TOKEN_URI.load(Ordering::Relaxed))
            };
            match parsed {
                Ok(Creds::ServiceAccount(sa)) => Arc::new(RefreshingToken::new(Key(*sa))),
                Ok(Creds::AuthorizedUser { .. }) => unreachable!("not allowed for a stored key"),
                Err(e) => {
                    let e = format!("provider {provider:?}: google credentials: {e}");
                    eprintln!("{e}");
                    Arc::new(Refused(e))
                }
            }
        })
        .clone()
}

#[cfg(test)]
mod tests {
    use super::super::mock;
    use super::*;

    const PKCS8: &str = include_str!("testdata/rsa-pkcs8.pem");
    const PKCS1: &str = include_str!("testdata/rsa-pkcs1.pem");
    const OK: &str = r#"{"access_token":"sa-token-1","token_type":"Bearer","expires_in":3599}"#;

    fn key_json(pem: &str, token_uri: &str) -> String {
        serde_json::json!({
            "type": "service_account",
            "project_id": "proj-1",
            "private_key_id": "kid-1",
            "client_email": "llmr@proj-1.iam.gserviceaccount.com",
            "private_key": pem,
            "token_uri": token_uri,
        })
        .to_string()
    }

    fn unb64(s: &str) -> Vec<u8> {
        let val = |c: u8| match c {
            b'A'..=b'Z' => c - b'A',
            b'a'..=b'z' => c - b'a' + 26,
            b'0'..=b'9' => c - b'0' + 52,
            b'-' => 62,
            _ => 63,
        } as u32;
        let mut out = Vec::new();
        for chunk in s.as_bytes().chunks(4) {
            let n = chunk.iter().enumerate().fold(0u32, |n, (i, &c)| n | val(c) << (18 - 6 * i));
            for i in 0..chunk.len() - 1 {
                out.push((n >> (16 - 8 * i)) as u8);
            }
        }
        out
    }

    #[tokio::test]
    async fn service_account_mints_a_signed_jwt_grant() {
        let (url, log) = mock::server(HashMap::from([("/token", (200, OK.into()))])).await;
        let Creds::ServiceAccount(sa) = parse_with(&key_json(PKCS8, &format!("{url}/token")), false, true).unwrap()
        else {
            panic!()
        };
        let public = sa.key.public().as_ref().to_vec();
        let t = RefreshingToken::new(Key(*sa));
        assert_eq!(t.bearer().await.unwrap(), "Bearer sa-token-1");
        assert_eq!(t.bearer().await.unwrap(), "Bearer sa-token-1");
        let log = log.lock().unwrap();
        assert_eq!(log.len(), 1, "minted again while still valid");
        let f = mock::fields(&log[0].body);
        assert_eq!(f["grant_type"], "urn:ietf:params:oauth:grant-type:jwt-bearer");
        let parts: Vec<&str> = f["assertion"].split('.').collect();
        assert_eq!(parts.len(), 3, "a signed JWT, not the raw key");
        let head: serde_json::Value = serde_json::from_slice(&unb64(parts[0])).unwrap();
        assert_eq!(head, serde_json::json!({"alg": "RS256", "typ": "JWT", "kid": "kid-1"}));
        let claims: serde_json::Value = serde_json::from_slice(&unb64(parts[1])).unwrap();
        assert_eq!(claims["iss"], "llmr@proj-1.iam.gserviceaccount.com");
        assert_eq!(claims["scope"], SCOPE);
        assert_eq!(claims["aud"], format!("{url}/token"));
        assert_eq!(claims["exp"].as_u64().unwrap() - claims["iat"].as_u64().unwrap(), 3600);
        let signed = format!("{}.{}", parts[0], parts[1]);
        ring::signature::UnparsedPublicKey::new(&ring::signature::RSA_PKCS1_2048_8192_SHA256, public)
            .verify(signed.as_bytes(), &unb64(parts[2]))
            .expect("RS256 signature verifies with the key's public half");
    }

    #[test]
    fn pkcs1_keys_parse_too() {
        assert!(matches!(parse(&key_json(PKCS1, TOKEN_URI), false), Ok(Creds::ServiceAccount(_))));
    }

    #[tokio::test]
    async fn stored_key_must_be_a_service_account() {
        // An external_account config names its own token endpoint; accepting one
        // from the console would let a pasted blob redirect credential minting.
        let ext = r#"{"type":"external_account","audience":"x","token_url":"https://evil.example/token"}"#;
        let err = service_account("gcp", ext).bearer().await.unwrap_err();
        assert!(err.contains("service_account") && err.contains("\"gcp\""), "{err}");
        let user = r#"{"type":"authorized_user","client_id":"a","client_secret":"b","refresh_token":"c"}"#;
        assert!(service_account("gcp", user).bearer().await.unwrap_err().contains("service_account"));
        // Blank must not fall back to ambient credentials.
        let err = service_account("gcp", " ").bearer().await.unwrap_err();
        assert!(err.contains("no service account JSON"), "{err}");
        let err = service_account("gcp", &key_json(PKCS8, "http://evil.example/token")).bearer().await.unwrap_err();
        assert!(err.contains("https"), "{err}");
        // Not even to this machine: a stored key is never a local test setup.
        let err = service_account("gcp", &key_json(PKCS8, "http://127.0.0.1:9/token")).bearer().await.unwrap_err();
        assert!(err.contains("https"), "{err}");
        assert!(service_account("gcp", "{").bearer().await.is_err());
    }

    #[test]
    fn same_key_shares_one_token() {
        let json = key_json(PKCS8, TOKEN_URI);
        assert!(Arc::ptr_eq(&service_account("a", &json), &service_account("a", &json)));
        assert!(!Arc::ptr_eq(&service_account("a", &json), &service_account("a", &key_json(PKCS1, TOKEN_URI))));
    }

    fn adc(vars: &[(&str, &str)]) -> Adc {
        let vars: HashMap<String, String> = vars.iter().map(|(k, v)| (k.to_string(), v.to_string())).collect();
        Adc::from_vars(|n| vars.get(n).cloned())
    }

    fn file(name: &str, contents: &str) -> String {
        let p = std::env::temp_dir().join(format!("llmr-gcp-{}-{name}", std::process::id()));
        std::fs::write(&p, contents).unwrap();
        p.to_string_lossy().into_owned()
    }

    #[tokio::test]
    async fn adc_service_account_file() {
        let (url, log) = mock::server(HashMap::from([("/token", (200, OK.into()))])).await;
        let path = file("adc-sa", &key_json(PKCS8, &format!("{url}/token")));
        let a = adc(&[("GOOGLE_APPLICATION_CREDENTIALS", &path), ("GCE_METADATA_HOST", "127.0.0.1:1")]);
        let Fetched::Token(h, _) = a.fetch(false).await.unwrap() else { panic!() };
        assert_eq!(h, "Bearer sa-token-1");
        assert!(mock::fields(&log.lock().unwrap()[0].body).contains_key("assertion"));
    }

    #[tokio::test]
    async fn adc_authorized_user_file() {
        let (url, log) = mock::server(HashMap::from([("/token", (200, OK.into()))])).await;
        let user = serde_json::json!({"type": "authorized_user", "client_id": "cid", "client_secret": "cs",
            "refresh_token": "rt", "token_uri": format!("{url}/token")});
        let a = adc(&[("GOOGLE_APPLICATION_CREDENTIALS", &file("adc-user", &user.to_string()))]);
        let Fetched::Token(h, _) = a.fetch(false).await.unwrap() else { panic!() };
        assert_eq!(h, "Bearer sa-token-1");
        let f = mock::fields(&log.lock().unwrap()[0].body);
        assert_eq!((f["grant_type"].as_str(), f["refresh_token"].as_str()), ("refresh_token", "rt"));
        assert_eq!((f["client_id"].as_str(), f["client_secret"].as_str()), ("cid", "cs"));
    }

    #[tokio::test]
    async fn adc_refuses_external_account() {
        let ext = r#"{"type":"external_account","audience":"x","token_url":"https://sts.googleapis.com/v1/token"}"#;
        let a = adc(&[("GOOGLE_APPLICATION_CREDENTIALS", &file("adc-ext", ext))]);
        let Err(err) = a.fetch(false).await else { panic!() };
        assert!(err.detail.contains("external_account") && err.detail.contains("aren't supported"), "{err:?}");
        assert_eq!(err.public, "google application default credentials: the credentials file is unusable");
    }

    #[tokio::test]
    async fn adc_metadata_server() {
        let path = "/computeMetadata/v1/instance/service-accounts/default/token";
        let (url, log) = mock::server(HashMap::from([(path, (200, OK.into()))])).await;
        let host = url.trim_start_matches("http://");
        let a = adc(&[("GCE_METADATA_HOST", host), ("HOME", "/nonexistent")]);
        let t = RefreshingToken::new(a);
        assert_eq!(t.bearer().await.unwrap(), "Bearer sa-token-1");
        let log = log.lock().unwrap();
        assert_eq!(log[0].headers["metadata-flavor"], "Google");
        assert!(
            log[0].uri.contains("scopes=https%3A%2F%2Fwww.googleapis.com%2Fauth%2Fcloud-platform"),
            "{}",
            log[0].uri
        );
    }

    #[tokio::test]
    async fn adc_prefers_the_gcloud_file_to_the_metadata_server() {
        let (url, log) = mock::server(HashMap::from([("/token", (200, OK.into()))])).await;
        let dir = std::env::temp_dir().join(format!("llmr-gcloud-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let user = serde_json::json!({"type": "authorized_user", "client_id": "c", "client_secret": "s",
            "refresh_token": "r", "token_uri": format!("{url}/token")});
        std::fs::write(dir.join("application_default_credentials.json"), user.to_string()).unwrap();
        let a = adc(&[("CLOUDSDK_CONFIG", dir.to_str().unwrap()), ("GCE_METADATA_HOST", "127.0.0.1:1")]);
        a.fetch(false).await.unwrap();
        assert_eq!(log.lock().unwrap().len(), 1);
    }

    #[tokio::test]
    async fn token_errors_are_explained() {
        let reply = r#"{"error":"invalid_grant","error_description":"Invalid JWT Signature."}"#;
        let (url, _) = mock::server(HashMap::from([("/token", (400, reply.into()))])).await;
        let Creds::ServiceAccount(sa) = parse_with(&key_json(PKCS8, &format!("{url}/token")), false, true).unwrap()
        else {
            panic!()
        };
        let key = Key(*sa);
        let detail = key.fetch(false).await.err().unwrap().detail;
        assert!(detail.contains("400: invalid_grant: Invalid JWT Signature."), "{detail}");
        let err = RefreshingToken::new(key).bearer().await.unwrap_err();
        assert_eq!(err, "google token (service account key): 400: invalid_grant");
        assert!(!err.contains("PRIVATE KEY") && !detail.contains("PRIVATE KEY"), "{err}");
    }

    #[tokio::test]
    async fn adc_shows_no_paths_or_bodies() {
        let (url, _) = mock::server(HashMap::from([("/token", (502, "<html>upstream secret</html>".into()))])).await;
        let path = file("adc-bad", &key_json(PKCS8, &format!("{url}/token")));
        let a = adc(&[("GOOGLE_APPLICATION_CREDENTIALS", &path)]);
        let err = a.fetch(false).await.err().unwrap();
        assert_eq!(err.public, "google application default credentials: 502");
        assert!(err.detail.contains(&path) && err.detail.contains("upstream secret"), "{err:?}");
        let missing = "/nonexistent/llmr-test/adc.json";
        let err = adc(&[("GOOGLE_APPLICATION_CREDENTIALS", missing)]).fetch(false).await.err().unwrap();
        assert!(!err.public.contains(missing) && err.detail.contains(missing), "{err:?}");
    }
}
