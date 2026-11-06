//! Cloud credentials: tokens the gateway mints for itself from Entra ID and
//! Google, the refresh logic every short-lived token shares, and callers'
//! identity tokens.

pub mod entra;
pub mod gcp;
pub mod jwt;
pub mod token;

use std::time::Duration;

use bytes::Bytes;
use http::{HeaderValue, Request, StatusCode};
use http_body_util::{BodyExt, Full, Limited};

use token::FetchError;

/// How long a token request may take.
const TOKEN_TIMEOUT: Duration = Duration::from_secs(15);

/// Sends a token request and returns the status, the body as JSON (null when it
/// isn't) and the start of the raw body for errors that aren't JSON.
pub(crate) async fn call(req: Request<Full<Bytes>>) -> Result<(StatusCode, serde_json::Value, String), String> {
    let resp = tokio::time::timeout(TOKEN_TIMEOUT, crate::server::upstream_client().request(req))
        .await
        .map_err(|_| "timed out".to_string())?
        .map_err(|e| chain(&e))?;
    let status = resp.status();
    let raw = Limited::new(resp.into_body(), 1 << 16).collect().await.map_err(|e| e.to_string())?.to_bytes();
    let v = serde_json::from_slice(&raw).unwrap_or_default();
    let text = String::from_utf8_lossy(&raw[..raw.len().min(300)]).trim().to_string();
    Ok((status, v, text))
}

/// An error with its causes: hyper's own says only "client error (Connect)".
fn chain(e: &dyn std::error::Error) -> String {
    let mut s = e.to_string();
    let mut cur = e.source();
    while let Some(c) = cur {
        s.push_str(": ");
        s.push_str(&c.to_string());
        cur = c.source();
    }
    s
}

/// The Authorization header and lifetime from an OAuth token response, or why
/// there is none. Entra's managed identity endpoints send numbers as strings, and
/// some only an absolute expiry. A refusal shows the status and OAuth error code;
/// the description, or a body that isn't OAuth's, is only logged.
pub(crate) fn bearer_from(
    status: StatusCode,
    v: &serde_json::Value,
    text: &str,
) -> Result<(HeaderValue, Duration), FetchError> {
    if status != StatusCode::OK {
        let code = v["error"].as_str().filter(|e| e.len() <= 64 && e.bytes().all(|c| c.is_ascii_graphic()));
        let public = match code {
            Some(e) => format!("{}: {e}", status.as_u16()),
            None => status.as_u16().to_string(),
        };
        let detail = match (v["error"].as_str(), v["error_description"].as_str()) {
            (Some(e), Some(d)) => format!("{e}: {}", d.lines().next().unwrap_or("")),
            (Some(e), None) => e.to_string(),
            _ => match v["error"]["message"].as_str() {
                Some(m) => m.to_string(),
                None => text.to_string(),
            },
        };
        return Err(FetchError::new(public, format!("{}: {detail}", status.as_u16())));
    }
    let num = |v: &serde_json::Value| v.as_u64().or_else(|| v.as_str().and_then(|s| s.trim().parse().ok()));
    let lifetime = num(&v["expires_in"])
        .or_else(|| num(&v["expires_on"]).map(|at| at.saturating_sub(crate::now_ms() as u64 / 1000)))
        .filter(|s| *s > 0);
    let token = v["access_token"].as_str().filter(|t| !t.is_empty());
    match (token, lifetime) {
        (Some(t), Some(secs)) => HeaderValue::try_from(format!("Bearer {t}"))
            .map(|h| (h, Duration::from_secs(secs)))
            .map_err(|_| "the token response holds an unusable token".to_string().into()),
        _ => Err("the token response holds no usable token".to_string().into()),
    }
}

/// Plain HTTP only to this machine, for a URL a credential is sent to.
pub(crate) fn https_or_loopback(url: &str) -> bool {
    if url.starts_with("https://") {
        return true;
    }
    ["http://localhost", "http://127.0.0.1", "http://[::1]"].iter().any(|p| {
        url.strip_prefix(p).is_some_and(|rest| rest.is_empty() || rest.starts_with(':') || rest.starts_with('/'))
    })
}

/// Base64url without padding, as JWTs use it.
pub(crate) fn b64url(data: &[u8]) -> String {
    const ABC: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
    let mut out = String::with_capacity(data.len().div_ceil(3) * 4);
    for chunk in data.chunks(3) {
        let n = chunk.iter().enumerate().fold(0u32, |n, (i, &b)| n | (b as u32) << (16 - 8 * i));
        for i in 0..=chunk.len() {
            out.push(ABC[(n >> (18 - 6 * i) & 63) as usize] as char);
        }
    }
    out
}

/// application/x-www-form-urlencoded.
pub(crate) fn form(pairs: &[(&str, &str)]) -> String {
    pairs.iter().map(|(k, v)| format!("{}={}", form_escape(k), form_escape(v))).collect::<Vec<_>>().join("&")
}

fn form_escape(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for &c in s.as_bytes() {
        match c {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'_' | b'.' | b'~' => out.push(c as char),
            b' ' => out.push('+'),
            _ => out.push_str(&format!("%{c:02X}")),
        }
    }
    out
}

/// Reads a small credential file: a token or a key, never anything large.
pub(crate) fn read_small(path: &str, max: u64) -> Result<String, String> {
    use std::io::Read;
    let mut raw = String::new();
    std::fs::File::open(path)
        .and_then(|f| f.take(max + 1).read_to_string(&mut raw))
        .map_err(|e| format!("{path}: {e}"))?;
    if raw.len() as u64 > max {
        return Err(format!("{path}: larger than {max} bytes"));
    }
    Ok(raw)
}

/// A stand-in token server for tests: answers each request with the reply for
/// its path and records (method, path and query, headers, body).
#[cfg(test)]
pub(crate) mod mock {
    use std::collections::HashMap;
    use std::sync::{Arc, Mutex};

    use bytes::Bytes;
    use http::{HeaderMap, Request, Response};
    use http_body_util::{BodyExt, Full};
    use hyper::body::Incoming;
    use hyper::service::service_fn;
    use hyper_util::rt::{TokioExecutor, TokioIo};
    use hyper_util::server::conn::auto;
    use tokio::net::TcpListener;

    pub struct Seen {
        pub method: String,
        pub uri: String,
        pub headers: HeaderMap,
        pub body: String,
    }

    pub type Log = Arc<Mutex<Vec<Seen>>>;

    /// Starts a server; `replies` maps a path to (status, body).
    pub async fn server(replies: HashMap<&'static str, (u16, String)>) -> (String, Log) {
        let _ = rustls::crypto::ring::default_provider().install_default();
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let url = format!("http://{}", listener.local_addr().unwrap());
        let log: Log = Default::default();
        let replies = Arc::new(replies);
        let l = log.clone();
        tokio::spawn(async move {
            while let Ok((conn, _)) = listener.accept().await {
                let (l, replies) = (l.clone(), replies.clone());
                tokio::spawn(async move {
                    let svc = service_fn(move |req: Request<Incoming>| {
                        let (l, replies) = (l.clone(), replies.clone());
                        async move {
                            let (parts, body) = req.into_parts();
                            let body = body.collect().await.unwrap().to_bytes();
                            let (status, reply) =
                                replies.get(parts.uri.path()).cloned().unwrap_or((404, "not found".into()));
                            l.lock().unwrap().push(Seen {
                                method: parts.method.to_string(),
                                uri: parts.uri.to_string(),
                                headers: parts.headers,
                                body: String::from_utf8_lossy(&body).into_owned(),
                            });
                            Ok::<_, std::convert::Infallible>(
                                Response::builder().status(status).body(Full::new(Bytes::from(reply))).unwrap(),
                            )
                        }
                    });
                    let _ = auto::Builder::new(TokioExecutor::new()).serve_connection(TokioIo::new(conn), svc).await;
                });
            }
        });
        (url, log)
    }

    /// The form fields of a request body.
    pub fn fields(body: &str) -> HashMap<String, String> {
        body.split('&').filter_map(|kv| kv.split_once('=')).map(|(k, v)| (unescape(k), unescape(v))).collect()
    }

    fn unescape(s: &str) -> String {
        let b = s.as_bytes();
        let mut out = Vec::with_capacity(b.len());
        let mut i = 0;
        while i < b.len() {
            match b[i] {
                b'+' => out.push(b' '),
                b'%' if i + 2 < b.len() => {
                    out.push(u8::from_str_radix(&s[i + 1..i + 3], 16).unwrap());
                    i += 2;
                }
                c => out.push(c),
            }
            i += 1;
        }
        String::from_utf8(out).unwrap()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn base64url() {
        assert_eq!(b64url(b""), "");
        assert_eq!(b64url(b"f"), "Zg");
        assert_eq!(b64url(b"fo"), "Zm8");
        assert_eq!(b64url(b"foo"), "Zm9v");
        assert_eq!(b64url(b"foob"), "Zm9vYg");
        assert_eq!(b64url(&[0xfb, 0xff, 0xfe]), "-__-");
    }

    #[test]
    fn form_round_trip() {
        let f = form(&[("a", "b c"), ("x", "1/2&=")]);
        assert_eq!(f, "a=b+c&x=1%2F2%26%3D");
        assert_eq!(mock::fields(&f)["x"], "1/2&=");
    }

    #[test]
    fn token_responses() {
        let ok = |v: serde_json::Value| bearer_from(StatusCode::OK, &v, "");
        let (h, d) = ok(serde_json::json!({"access_token": "a", "expires_in": 3599})).unwrap();
        assert_eq!((h.to_str().unwrap(), d), ("Bearer a", Duration::from_secs(3599)));
        // IMDS: strings
        let (_, d) = ok(serde_json::json!({"access_token": "a", "expires_in": "86399"})).unwrap();
        assert_eq!(d, Duration::from_secs(86399));
        // App Service: only an absolute expiry
        let at = crate::now_ms() as u64 / 1000 + 600;
        let (_, d) = ok(serde_json::json!({"access_token": "a", "expires_on": at.to_string()})).unwrap();
        assert!(d > Duration::from_secs(590) && d <= Duration::from_secs(600), "{d:?}");
        assert!(ok(serde_json::json!({"access_token": "", "expires_in": 1})).is_err());
        assert!(ok(serde_json::json!({"access_token": "a"})).is_err());
        let err = bearer_from(
            StatusCode::BAD_REQUEST,
            &serde_json::json!({"error": "invalid_client", "error_description": "AADSTS7000215: bad secret.\r\nTrace ID: x"}),
            "",
        )
        .unwrap_err();
        assert_eq!(err.public, "400: invalid_client");
        assert_eq!(err.detail, "400: invalid_client: AADSTS7000215: bad secret.");
        // A body that isn't an OAuth error is never shown, only logged.
        let err = bearer_from(StatusCode::NOT_FOUND, &serde_json::Value::Null, "nope").unwrap_err();
        assert_eq!((err.public.as_str(), err.detail.as_str()), ("404", "404: nope"));
        let err =
            bearer_from(StatusCode::FORBIDDEN, &serde_json::json!({"error": {"message": "inner"}}), "").unwrap_err();
        assert_eq!((err.public.as_str(), err.detail.as_str()), ("403", "403: inner"));
    }

    #[test]
    fn loopback_only_for_http() {
        assert!(https_or_loopback("https://oauth2.googleapis.com/token"));
        assert!(https_or_loopback("http://127.0.0.1:9/token"));
        assert!(!https_or_loopback("http://127.0.0.1.evil.example/token"));
        assert!(!https_or_loopback("http://evil.example/token"));
    }
}
