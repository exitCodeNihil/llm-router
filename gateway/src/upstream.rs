//! Upstream URLs and credentials (internal/provider/provider.go NewRequest).

use http::{HeaderMap, HeaderValue, header};

use crate::config::{AuthMode, Deployment, Flavor, Provider, ProviderKind};

/// The part of the upstream URL before and after the request path, built once
/// per deployment. Anthropic-flavor deployments have one fixed URL.
pub fn url_parts(p: &Provider, upstream: &str, flavor: Flavor) -> (String, String) {
    let base = p.base_url.as_str();
    match (flavor, p.kind) {
        (_, ProviderKind::Vertex) => (String::new(), String::new()), // not served yet
        (Flavor::Anthropic, ProviderKind::Azure) => (format!("{base}/anthropic/v1/messages"), String::new()),
        (Flavor::Anthropic, _) => (format!("{}/v1/messages", base.trim_end_matches("/v1")), String::new()),
        (Flavor::OpenAI, ProviderKind::Azure) => {
            let version = if p.api_version.is_empty() { "2024-10-21" } else { p.api_version.as_str() };
            (
                format!("{base}/openai/deployments/{}", path_escape(upstream)),
                format!("?api-version={}", query_escape(version)),
            )
        }
        (Flavor::OpenAI, _) => (base.to_string(), String::new()),
    }
}

/// Why a request couldn't be built for a deployment; the gateway moves on to
/// the next one.
#[derive(Debug, PartialEq)]
pub enum BuildError {
    /// The provider forwards the caller's own credential and the caller sent none.
    NoCallerCredential(String),
    Unsupported(String),
}

/// The caller headers allowed to reach an upstream (provider.Inbound).
#[derive(Default, Clone)]
pub struct Inbound {
    /// The caller's own upstream credential; empty when the gateway key came in Authorization.
    pub authorization: Option<HeaderValue>,
    pub anthropic_beta: Option<HeaderValue>,
}

/// The URL and headers for `path` (/chat/completions, /completions, /embeddings)
/// on deployment `d`.
pub fn request_parts(d: &Deployment, path: &str, inbound: &Inbound) -> Result<(String, HeaderMap), BuildError> {
    let p = &d.provider;
    if p.kind == ProviderKind::Vertex {
        return Err(BuildError::Unsupported(format!(
            "provider {} is Google Vertex AI, which this gateway doesn't serve yet",
            p.name
        )));
    }
    let url = match d.flavor {
        Flavor::Anthropic if path != "/chat/completions" && path != "/messages" => {
            return Err(BuildError::Unsupported(format!("{path} is not supported by anthropic-flavor deployments")));
        }
        Flavor::Anthropic => d.url_prefix.clone(),
        Flavor::OpenAI => {
            let mut u = String::with_capacity(d.url_prefix.len() + path.len() + d.url_suffix.len());
            u.push_str(&d.url_prefix);
            u.push_str(path);
            u.push_str(&d.url_suffix);
            u
        }
    };
    let mut h = HeaderMap::with_capacity(4);
    h.insert(header::CONTENT_TYPE, HeaderValue::from_static("application/json"));
    let anthropic = d.flavor == Flavor::Anthropic;
    if anthropic {
        h.insert("anthropic-version", HeaderValue::from_static("2023-06-01"));
        if let Some(beta) = &inbound.anthropic_beta {
            h.insert("anthropic-beta", beta.clone());
        }
    }
    match p.auth_mode {
        AuthMode::None => {}
        AuthMode::Bearer => {
            if let Some((name, value)) = &p.auth_header {
                h.insert(name.clone(), value.clone());
            }
        }
        // Azure's header; the Anthropic surface reads x-api-key.
        AuthMode::ApiKey => {
            if let Some((name, value)) = &p.auth_header {
                h.insert(
                    if anthropic { http::HeaderName::from_static("x-api-key") } else { name.clone() },
                    value.clone(),
                );
            }
        }
        AuthMode::OAuthPassthrough => match &inbound.authorization {
            Some(v) => {
                h.insert(header::AUTHORIZATION, v.clone());
            }
            None => {
                return Err(BuildError::NoCallerCredential(format!(
                    "no caller credential to forward: provider {:?} forwards the caller's credential — \
                     send the upstream token in Authorization and the gateway key in X-Llmr-Key",
                    p.name
                )));
            }
        },
        AuthMode::Unsupported => {
            return Err(BuildError::Unsupported(format!(
                "provider {} uses a cloud identity this gateway can't mint yet",
                p.name
            )));
        }
    }
    Ok((url, h))
}

/// url.PathEscape: escape everything but unreserved characters and the
/// sub-delimiters a path segment may carry.
fn path_escape(s: &str) -> String {
    escape(s, |c| c.is_ascii_alphanumeric() || b"-_.~$&+,:;=@".contains(&c), false)
}

/// url.QueryEscape.
fn query_escape(s: &str) -> String {
    escape(s, |c| c.is_ascii_alphanumeric() || b"-_.~".contains(&c), true)
}

fn escape(s: &str, keep: impl Fn(u8) -> bool, space_plus: bool) -> String {
    let mut out = String::with_capacity(s.len());
    for &c in s.as_bytes() {
        if keep(c) {
            out.push(c as char);
        } else if space_plus && c == b' ' {
            out.push('+');
        } else {
            out.push_str(&format!("%{c:02X}"));
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::{Config, Wire};

    fn cfg() -> Config {
        let w: Wire = serde_json::from_str(
            r#"{"schema":1,"version":1,"group":{"id":"g","name":"g"},
            "providers":[
              {"id":"az","name":"az","type":"azure","base_url":"https://r.openai.azure.com/","auth_mode":"api_key","api_key":"k1"},
              {"id":"an","name":"an","type":"openai_compatible","base_url":"https://api.anthropic.com/v1","auth_mode":"oauth_passthrough"},
              {"id":"or","name":"or","type":"openrouter","auth_mode":"bearer","api_key":"sk-or"}
            ],
            "deployments":[
              {"id":"1","provider_id":"az","model_name":"gpt","upstream_name":"my dep/1"},
              {"id":"2","provider_id":"az","model_name":"claude","upstream_name":"claude-x","api_flavor":"anthropic"},
              {"id":"3","provider_id":"an","model_name":"sub","upstream_name":"claude-y","api_flavor":"anthropic"},
              {"id":"4","provider_id":"or","model_name":"free","upstream_name":"x/y:free"}
            ]}"#,
        )
        .unwrap();
        Config::build(w).unwrap()
    }

    #[test]
    fn urls_and_auth() {
        let c = cfg();
        let d = |m: &str| c.resolve(m).unwrap().deployments[0].clone();
        let none = Inbound::default();

        let (u, h) = request_parts(&d("gpt"), "/chat/completions", &none).unwrap();
        assert_eq!(
            u,
            "https://r.openai.azure.com/openai/deployments/my%20dep%2F1/chat/completions?api-version=2024-10-21"
        );
        assert_eq!(h["api-key"], "k1");

        let (u, h) = request_parts(&d("claude"), "/chat/completions", &none).unwrap();
        assert_eq!(u, "https://r.openai.azure.com/anthropic/v1/messages");
        assert_eq!(h["x-api-key"], "k1");
        assert_eq!(h["anthropic-version"], "2023-06-01");
        assert!(request_parts(&d("claude"), "/embeddings", &none).is_err());

        assert!(matches!(request_parts(&d("sub"), "/chat/completions", &none), Err(BuildError::NoCallerCredential(_))));
        let caller =
            Inbound { authorization: Some(HeaderValue::from_static("Bearer sub-token")), ..Default::default() };
        let (u, h) = request_parts(&d("sub"), "/chat/completions", &caller).unwrap();
        assert_eq!(u, "https://api.anthropic.com/v1/messages");
        assert_eq!(h["authorization"], "Bearer sub-token");

        let (u, h) = request_parts(&d("free"), "/chat/completions", &none).unwrap();
        assert_eq!(u, "https://openrouter.ai/api/v1/chat/completions");
        assert_eq!(h["authorization"], "Bearer sk-or");
    }
}
