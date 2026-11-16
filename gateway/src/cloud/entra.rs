//! Entra ID tokens for Azure OpenAI and Microsoft Foundry providers with auth
//! mode "entra". Like the control plane (azidentity's DefaultAzureCredential in
//! internal/provider/provider.go), nothing is stored: each gateway uses its own
//! identity, from the first of these its environment configures:
//!
//! 1. a client secret: AZURE_TENANT_ID, AZURE_CLIENT_ID, AZURE_CLIENT_SECRET
//! 2. workload identity: AZURE_FEDERATED_TOKEN_FILE, AZURE_TENANT_ID, AZURE_CLIENT_ID
//! 3. App Service and Container Apps managed identity: IDENTITY_ENDPOINT, IDENTITY_HEADER
//! 4. the instance metadata service (IMDS), on Azure VMs and AKS
//!
//! AZURE_AUTHORITY_HOST picks the sign-in cloud for 1 and 2. AZURE_CLIENT_ID picks
//! a user-assigned managed identity for 3 and 4. Certificates, user passwords and
//! the Azure CLI aren't supported.

use std::sync::{Arc, OnceLock};

use bytes::Bytes;
use http::{Request, header};
use http_body_util::Full;

use super::token::{Bearer, FetchError, Fetched, RefreshingToken, Refused, TokenSource};
use super::{bearer_from, call, form, https_or_loopback, read_small};

/// The scope the control plane asks for too.
const SCOPE: &str = "https://cognitiveservices.azure.com/.default";
/// The same, as managed identity endpoints name it.
const RESOURCE: &str = "https://cognitiveservices.azure.com";
const AUTHORITY: &str = "https://login.microsoftonline.com";
const IMDS: &str = "http://169.254.169.254/metadata/identity/oauth2/token";
const MAX_ASSERTION: u64 = 16 << 10;

enum Method {
    ClientSecret { tenant: String, client_id: String, secret: String },
    Federated { tenant: String, client_id: String, file: String },
    AppService { endpoint: String, secret: String, client_id: Option<String> },
    Imds { endpoint: String, client_id: Option<String> },
}

/// Without the client secret or the identity header.
impl std::fmt::Debug for Method {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Method::ClientSecret { tenant, client_id, .. } => f
                .debug_struct("ClientSecret")
                .field("tenant", tenant)
                .field("client_id", client_id)
                .finish_non_exhaustive(),
            Method::Federated { tenant, client_id, file } => f
                .debug_struct("Federated")
                .field("tenant", tenant)
                .field("client_id", client_id)
                .field("file", file)
                .finish(),
            Method::AppService { endpoint, client_id, .. } => f
                .debug_struct("AppService")
                .field("endpoint", endpoint)
                .field("client_id", client_id)
                .finish_non_exhaustive(),
            Method::Imds { endpoint, client_id } => {
                f.debug_struct("Imds").field("endpoint", endpoint).field("client_id", client_id).finish()
            }
        }
    }
}

#[derive(Debug)]
pub struct Entra {
    authority: String,
    method: Method,
}

/// The token every Entra provider on this gateway shares: there is one identity
/// and one scope, as on the control plane.
pub fn shared() -> Arc<dyn Bearer> {
    static SHARED: OnceLock<Arc<dyn Bearer>> = OnceLock::new();
    SHARED
        .get_or_init(|| match Entra::from_vars(|n| std::env::var(n).ok().filter(|v| !v.is_empty())) {
            Ok(e) => Arc::new(RefreshingToken::new(e)),
            Err(why) => {
                crate::log_error!("entra: {why}");
                Arc::new(Refused(format!("entra: {why}")))
            }
        })
        .clone()
}

impl Entra {
    fn from_vars(var: impl Fn(&str) -> Option<String>) -> Result<Entra, String> {
        let authority =
            var("AZURE_AUTHORITY_HOST").unwrap_or_else(|| AUTHORITY.into()).trim_end_matches('/').to_string();
        if !https_or_loopback(&authority) {
            return Err(format!("AZURE_AUTHORITY_HOST {authority:?} must use https"));
        }
        let tenant = var("AZURE_TENANT_ID");
        let client_id = var("AZURE_CLIENT_ID");
        if let Some(t) = &tenant
            && !t.bytes().all(|c| c.is_ascii_alphanumeric() || c == b'-' || c == b'.')
        {
            return Err(format!("AZURE_TENANT_ID {t:?} is not a tenant ID or domain"));
        }
        let method = match (tenant, client_id.clone(), var("AZURE_CLIENT_SECRET"), var("AZURE_FEDERATED_TOKEN_FILE")) {
            (Some(tenant), Some(client_id), Some(secret), _) => Method::ClientSecret { tenant, client_id, secret },
            (Some(tenant), Some(client_id), None, Some(file)) => Method::Federated { tenant, client_id, file },
            _ if var("AZURE_CLIENT_CERTIFICATE_PATH").is_some() => {
                return Err("certificate credentials (AZURE_CLIENT_CERTIFICATE_PATH) aren't supported: \
                            use a client secret, workload identity or a managed identity"
                    .into());
            }
            _ => match (var("IDENTITY_ENDPOINT"), var("IDENTITY_HEADER")) {
                (Some(endpoint), Some(secret)) => Method::AppService { endpoint, secret, client_id },
                (Some(_), None) => {
                    return Err("this managed identity (IDENTITY_ENDPOINT without IDENTITY_HEADER: Azure Arc or \
                                Service Fabric) isn't supported"
                        .into());
                }
                _ => Method::Imds { endpoint: IMDS.into(), client_id },
            },
        };
        Ok(Entra { authority, method })
    }

    fn token_url(&self, tenant: &str) -> String {
        format!("{}/{tenant}/oauth2/v2.0/token", self.authority)
    }

    fn request(&self) -> Result<Request<Full<Bytes>>, FetchError> {
        let post = |url: String, body: String| {
            Request::post(url)
                .header(header::CONTENT_TYPE, "application/x-www-form-urlencoded")
                .body(Full::new(Bytes::from(body)))
        };
        let req = match &self.method {
            Method::ClientSecret { tenant, client_id, secret } => post(
                self.token_url(tenant),
                form(&[
                    ("grant_type", "client_credentials"),
                    ("client_id", client_id),
                    ("client_secret", secret),
                    ("scope", SCOPE),
                ]),
            ),
            Method::Federated { tenant, client_id, file } => {
                // Read on every request: the kubelet rotates it.
                let assertion = read_small(file, MAX_ASSERTION)
                    .map_err(|e| FetchError::new("AZURE_FEDERATED_TOKEN_FILE can't be read", e))?;
                post(
                    self.token_url(tenant),
                    form(&[
                        ("grant_type", "client_credentials"),
                        ("client_id", client_id),
                        ("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"),
                        ("client_assertion", assertion.trim()),
                        ("scope", SCOPE),
                    ]),
                )
            }
            Method::AppService { endpoint, secret, client_id } => {
                let mut q = vec![("api-version", "2019-08-01"), ("resource", RESOURCE)];
                if let Some(c) = client_id {
                    q.push(("client_id", c));
                }
                Request::get(format!("{endpoint}?{}", form(&q)))
                    .header("x-identity-header", secret.as_str())
                    .body(Full::new(Bytes::new()))
            }
            Method::Imds { endpoint, client_id } => {
                let mut q = vec![("api-version", "2018-02-01"), ("resource", RESOURCE)];
                if let Some(c) = client_id {
                    q.push(("client_id", c));
                }
                Request::get(format!("{endpoint}?{}", form(&q)))
                    .header("metadata", "true")
                    .body(Full::new(Bytes::new()))
            }
        };
        req.map_err(|e| FetchError::from(e.to_string()))
    }

    fn kind(&self) -> &'static str {
        match self.method {
            Method::ClientSecret { .. } => "client secret",
            Method::Federated { .. } => "workload identity",
            Method::AppService { .. } => "app service managed identity",
            Method::Imds { .. } => "managed identity (IMDS)",
        }
    }
}

impl TokenSource for Entra {
    fn what(&self) -> &str {
        "entra token request"
    }

    async fn fetch(&self, _usable: bool) -> Result<Fetched, FetchError> {
        let what = format!("entra token ({})", self.kind());
        let wrap = |e: FetchError| e.context(&what);
        let req = self.request().map_err(wrap)?;
        let (status, v, text) =
            call(req).await.map_err(|e| wrap(FetchError::new("the token endpoint did not answer", e)))?;
        let (header, lifetime) = bearer_from(status, &v, &text).map_err(wrap)?;
        Ok(Fetched::Token(header, lifetime))
    }
}

#[cfg(test)]
mod tests {
    use std::collections::HashMap;

    use super::super::mock;
    use super::*;

    fn entra(vars: &[(&str, &str)]) -> Result<Entra, String> {
        let vars: HashMap<String, String> = vars.iter().map(|(k, v)| (k.to_string(), v.to_string())).collect();
        Entra::from_vars(|n| vars.get(n).cloned())
    }

    const OK: &str = r#"{"token_type":"Bearer","expires_in":3599,"access_token":"entra-1"}"#;

    #[test]
    fn picks_the_first_configured_method() {
        let e = entra(&[
            ("AZURE_TENANT_ID", "t"),
            ("AZURE_CLIENT_ID", "c"),
            ("AZURE_CLIENT_SECRET", "s"),
            ("AZURE_FEDERATED_TOKEN_FILE", "/f"),
            ("IDENTITY_ENDPOINT", "http://localhost:1/msi"),
            ("IDENTITY_HEADER", "h"),
        ])
        .unwrap();
        assert_eq!(e.kind(), "client secret");
        let e = entra(&[("AZURE_TENANT_ID", "t"), ("AZURE_CLIENT_ID", "c"), ("AZURE_FEDERATED_TOKEN_FILE", "/f")]);
        assert_eq!(e.unwrap().kind(), "workload identity");
        let e = entra(&[("IDENTITY_ENDPOINT", "http://localhost:1/msi"), ("IDENTITY_HEADER", "h")]).unwrap();
        assert_eq!(e.kind(), "app service managed identity");
        // A client ID alone names a user-assigned managed identity.
        let e = entra(&[("AZURE_CLIENT_ID", "c")]).unwrap();
        assert!(matches!(e.method, Method::Imds { client_id: Some(ref c), .. } if c == "c"));
        assert!(matches!(entra(&[]).unwrap().method, Method::Imds { client_id: None, .. }));
    }

    #[test]
    fn refuses_what_it_cannot_do() {
        let err =
            entra(&[("AZURE_TENANT_ID", "t"), ("AZURE_CLIENT_ID", "c"), ("AZURE_CLIENT_CERTIFICATE_PATH", "/c.pem")])
                .unwrap_err();
        assert!(err.contains("certificate"), "{err}");
        let err = entra(&[("IDENTITY_ENDPOINT", "http://localhost:40342/metadata/identity/oauth2/token")]).unwrap_err();
        assert!(err.contains("Arc"), "{err}");
        assert!(entra(&[("AZURE_AUTHORITY_HOST", "http://login.evil.example")]).is_err());
        assert!(
            entra(&[("AZURE_TENANT_ID", "t/../x"), ("AZURE_CLIENT_ID", "c"), ("AZURE_CLIENT_SECRET", "s")]).is_err()
        );
    }

    #[tokio::test]
    async fn client_secret_against_the_authority_host() {
        let (url, log) = mock::server(HashMap::from([("/tenant-1/oauth2/v2.0/token", (200, OK.into()))])).await;
        let e = entra(&[
            ("AZURE_AUTHORITY_HOST", &format!("{url}/")),
            ("AZURE_TENANT_ID", "tenant-1"),
            ("AZURE_CLIENT_ID", "client-1"),
            ("AZURE_CLIENT_SECRET", "s3cret&="),
        ])
        .unwrap();
        let t = RefreshingToken::new(e);
        assert_eq!(t.bearer().await.unwrap(), "Bearer entra-1");
        assert_eq!(t.bearer().await.unwrap(), "Bearer entra-1");
        let log = log.lock().unwrap();
        assert_eq!(log.len(), 1, "the token is reused while fresh");
        assert_eq!(log[0].method, "POST");
        let f = mock::fields(&log[0].body);
        assert_eq!(f["grant_type"], "client_credentials");
        assert_eq!(f["client_id"], "client-1");
        assert_eq!(f["client_secret"], "s3cret&=");
        assert_eq!(f["scope"], SCOPE);
    }

    #[tokio::test]
    async fn federated_token_file_is_read_each_time() {
        let (url, log) = mock::server(HashMap::from([("/t/oauth2/v2.0/token", (200, OK.into()))])).await;
        let path = std::env::temp_dir().join(format!("llmr-entra-{}", std::process::id()));
        std::fs::write(&path, "aaa.bbb.ccc\n").unwrap();
        let e = entra(&[
            ("AZURE_AUTHORITY_HOST", &url),
            ("AZURE_TENANT_ID", "t"),
            ("AZURE_CLIENT_ID", "c"),
            ("AZURE_FEDERATED_TOKEN_FILE", path.to_str().unwrap()),
        ])
        .unwrap();
        e.fetch(false).await.unwrap();
        std::fs::write(&path, "ddd.eee.fff").unwrap();
        e.fetch(false).await.unwrap();
        let log = log.lock().unwrap();
        let (a, b) = (mock::fields(&log[0].body), mock::fields(&log[1].body));
        assert_eq!(a["client_assertion"], "aaa.bbb.ccc");
        assert_eq!(b["client_assertion"], "ddd.eee.fff");
        assert_eq!(a["client_assertion_type"], "urn:ietf:params:oauth:client-assertion-type:jwt-bearer");
        assert!(!a.contains_key("client_secret"));
    }

    #[tokio::test]
    async fn app_service_identity_endpoint() {
        let at = crate::now_ms() / 1000 + 3600;
        let reply = format!(r#"{{"access_token":"as-1","expires_on":"{at}","resource":"{RESOURCE}"}}"#);
        let (url, log) = mock::server(HashMap::from([("/msi/token", (200, reply))])).await;
        let e = entra(&[
            ("IDENTITY_ENDPOINT", &format!("{url}/msi/token")),
            ("IDENTITY_HEADER", "id-header"),
            ("AZURE_CLIENT_ID", "uami"),
        ])
        .unwrap();
        assert_eq!(RefreshingToken::new(e).bearer().await.unwrap(), "Bearer as-1");
        let log = log.lock().unwrap();
        assert_eq!(log[0].method, "GET");
        assert_eq!(log[0].headers["x-identity-header"], "id-header");
        assert!(log[0].uri.contains("api-version=2019-08-01"), "{}", log[0].uri);
        assert!(log[0].uri.contains("resource=https%3A%2F%2Fcognitiveservices.azure.com"), "{}", log[0].uri);
        assert!(log[0].uri.contains("client_id=uami"), "{}", log[0].uri);
    }

    #[tokio::test]
    async fn imds() {
        let reply = r#"{"access_token":"imds-1","expires_in":"86399","expires_on":"1","token_type":"Bearer"}"#;
        let (url, log) = mock::server(HashMap::from([("/metadata/identity/oauth2/token", (200, reply.into()))])).await;
        let mut e = entra(&[]).unwrap();
        e.method = Method::Imds { endpoint: format!("{url}/metadata/identity/oauth2/token"), client_id: None };
        let Fetched::Token(h, d) = e.fetch(false).await.unwrap() else { panic!() };
        assert_eq!((h.to_str().unwrap(), d.as_secs()), ("Bearer imds-1", 86399));
        let log = log.lock().unwrap();
        assert_eq!(log[0].headers["metadata"], "true");
        assert!(log[0].uri.contains("api-version=2018-02-01") && !log[0].uri.contains("client_id"), "{}", log[0].uri);
    }

    #[tokio::test]
    async fn errors_name_the_method_and_keep_the_secret() {
        let reply = r#"{"error":"invalid_client","error_description":"AADSTS7000215: Invalid client secret provided.\r\nTrace ID: 1"}"#;
        let (url, _) = mock::server(HashMap::from([("/t/oauth2/v2.0/token", (401, reply.into()))])).await;
        let e = entra(&[
            ("AZURE_AUTHORITY_HOST", &url),
            ("AZURE_TENANT_ID", "t"),
            ("AZURE_CLIENT_ID", "c"),
            ("AZURE_CLIENT_SECRET", "hunter2"),
        ])
        .unwrap();
        let err = RefreshingToken::new(e).bearer().await.unwrap_err();
        assert_eq!(err, "entra token (client secret): 401: invalid_client");
        // The description is for the log.
        let err = entra(&[
            ("AZURE_AUTHORITY_HOST", &url),
            ("AZURE_TENANT_ID", "t"),
            ("AZURE_CLIENT_ID", "c"),
            ("AZURE_CLIENT_SECRET", "hunter2"),
        ])
        .unwrap()
        .fetch(false)
        .await
        .err()
        .unwrap();
        assert!(err.detail.contains("AADSTS7000215") && !err.detail.contains("hunter2"), "{err:?}");
    }

    #[test]
    fn debug_shows_no_secrets() {
        let e = entra(&[("AZURE_TENANT_ID", "t"), ("AZURE_CLIENT_ID", "c"), ("AZURE_CLIENT_SECRET", "hunter2")]);
        let shown = format!("{:?}", e.unwrap());
        assert!(shown.contains("ClientSecret") && !shown.contains("hunter2"), "{shown}");
        let e = entra(&[("IDENTITY_ENDPOINT", "http://localhost:1/msi"), ("IDENTITY_HEADER", "s3cret-header")]);
        let shown = format!("{:?}", e.unwrap());
        assert!(shown.contains("AppService") && !shown.contains("s3cret-header"), "{shown}");
    }

    #[tokio::test]
    async fn a_token_file_path_is_only_logged() {
        let path = "/nonexistent/llmr-test/azure-identity-token";
        let e = entra(&[("AZURE_TENANT_ID", "t"), ("AZURE_CLIENT_ID", "c"), ("AZURE_FEDERATED_TOKEN_FILE", path)]);
        let err = e.unwrap().fetch(false).await.err().unwrap();
        assert!(!err.public.contains(path) && err.public.contains("AZURE_FEDERATED_TOKEN_FILE"), "{err:?}");
        assert!(err.detail.contains(path), "{err:?}");
    }
}
