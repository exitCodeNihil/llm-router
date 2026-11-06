//! Upstream URLs and credentials (internal/provider/provider.go NewRequest, and
//! gcp.go for Vertex AI).

use std::collections::BTreeMap;

use bytes::Bytes;
use http::{HeaderMap, HeaderValue, header};
use serde_json::value::RawValue;

use crate::config::{AuthMode, Deployment, Flavor, Provider, ProviderKind};
use crate::translate::BadRequest;

/// What Vertex calls the Anthropic API version. It travels in the body, not the
/// anthropic-version header.
const VERTEX_ANTHROPIC_VERSION: &str = "vertex-2023-10-16";
const RAW_PREDICT: &str = ":rawPredict";
const STREAM_RAW_PREDICT: &str = ":streamRawPredict";

/// The part of the upstream URL before and after the request path, built once
/// per deployment. Anthropic-flavor deployments have one fixed URL.
pub fn url_parts(p: &Provider, upstream: &str, flavor: Flavor) -> (String, String) {
    let base = p.base_url.as_str();
    match (flavor, p.kind) {
        (_, ProviderKind::Vertex) => match vertex_parent(p) {
            // Gemini and other Model Garden models: the OpenAI-compatible surface.
            Ok(parent) if flavor == Flavor::OpenAI => (format!("{parent}/endpoints/openapi"), String::new()),
            // Claude: the verb turns into :streamRawPredict for a streaming body (finish).
            Ok(parent) => (
                format!("{parent}/publishers/anthropic/models/{}{RAW_PREDICT}", vertex_model_segment(upstream)),
                String::new(),
            ),
            Err(_) => (String::new(), String::new()), // request_parts says why
        },
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
    /// The caller asked for an endpoint the deployment's flavor doesn't have,
    /// such as /embeddings on Claude. Also the caller's mistake.
    UnsupportedRoute(String),
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
    let vertex = p.kind == ProviderKind::Vertex;
    if vertex {
        // Vertex's OpenAI-compatible endpoint answers /embeddings with
        // FAILED_PRECONDITION, and rawPredict is Messages.
        if path != "/chat/completions" && path != "/messages" {
            return Err(BuildError::UnsupportedRoute(format!(
                "unsupported route: {path} is not supported on vertex deployments"
            )));
        }
        vertex_parent(p).map_err(BuildError::Unsupported)?;
    }
    let url = match d.flavor {
        Flavor::Anthropic if path != "/chat/completions" && path != "/messages" => {
            return Err(BuildError::UnsupportedRoute(format!(
                "unsupported route: {path} is not supported by anthropic-flavor deployments"
            )));
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
        // Vertex carries the version in the body instead, and rejects requests
        // that also pin it in the header.
        if !vertex {
            h.insert("anthropic-version", HeaderValue::from_static("2023-06-01"));
        }
        if let Some(beta) = &inbound.anthropic_beta {
            h.insert("anthropic-beta", beta.clone());
        }
    }
    match p.auth_mode {
        // The caller adds it: getting one can mean waiting on an exchange.
        AuthMode::None | AuthMode::AnthropicWif | AuthMode::Entra | AuthMode::Gcp => {}
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
                "provider {} uses an auth mode this gateway doesn't know",
                p.name
            )));
        }
    }
    Ok((url, h))
}

/// The last step before sending: Claude on Vertex takes the model in the URL and
/// rejects it in the body, wants the API version in the body, and streams from
/// another verb.
pub fn finish(d: &Deployment, url: String, body: Bytes) -> Result<(String, Bytes), BadRequest> {
    if d.provider.kind != ProviderKind::Vertex || d.flavor != Flavor::Anthropic {
        return Ok((url, body));
    }
    let (body, stream) = vertex_anthropic_body(&body)?;
    let url = match url.strip_suffix(RAW_PREDICT) {
        Some(head) if stream => format!("{head}{STREAM_RAW_PREDICT}"),
        _ => url,
    };
    Ok((url, body))
}

fn vertex_anthropic_body(body: &[u8]) -> Result<(Bytes, bool), BadRequest> {
    let mut m: BTreeMap<String, Box<RawValue>> =
        serde_json::from_slice(body).map_err(|e| BadRequest(format!("invalid JSON body: {e}")))?;
    m.remove("model");
    if !m.contains_key("anthropic_version") {
        let v = RawValue::from_string(format!("\"{VERTEX_ANTHROPIC_VERSION}\"")).expect("a JSON string");
        m.insert("anthropic_version".into(), v);
    }
    let stream = m.get("stream").is_some_and(|s| s.get() == "true");
    let out = serde_json::to_vec(&m).map_err(|e| BadRequest(e.to_string()))?;
    Ok((Bytes::from(out), stream))
}

/// .../v1/projects/{project}/locations/{location}, or why there is none.
fn vertex_parent(p: &Provider) -> Result<String, String> {
    if p.project.is_empty() {
        return Err(format!("provider {:?}: gcp project is not set", p.name));
    }
    // It becomes part of a hostname: anything else would send the bearer token,
    // cloud-platform scope, to whatever host it spells.
    if !p.location.is_empty() && !p.location.bytes().all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == b'-')
    {
        return Err(format!("provider {:?}: config.location {:?} is not a valid region", p.name, p.location));
    }
    let location = if p.location.is_empty() { "global" } else { p.location.as_str() };
    // base_url is honoured only for a private endpoint. Google's own hosts follow
    // the location: a stored copy would go stale the moment it changed, and Vertex
    // rejects a path whose location doesn't match the host.
    let base = if !p.base_url.is_empty() && !public_vertex_host(&p.base_url) {
        p.base_url.trim_end_matches('/').to_string()
    } else if location != "global" {
        format!("https://{location}-aiplatform.googleapis.com")
    } else {
        "https://aiplatform.googleapis.com".to_string()
    };
    Ok(format!("{base}/v1/projects/{}/locations/{}", path_escape(&p.project), path_escape(location)))
}

/// ^https://([a-z0-9-]+-)?aiplatform\.googleapis\.com/?$
fn public_vertex_host(url: &str) -> bool {
    let Some(host) = url.strip_prefix("https://") else { return false };
    let host = host.strip_suffix('/').unwrap_or(host);
    let Some(region) = host.strip_suffix("aiplatform.googleapis.com") else { return false };
    region.is_empty()
        || (region.len() > 1
            && region.ends_with('-')
            && region.bytes().all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == b'-'))
}

/// Claude ids carry a version suffix ("claude-sonnet-4-5@20250929"). A literal
/// "@" gets an HTML 404 from Google's frontend, so it goes percent-encoded.
fn vertex_model_segment(name: &str) -> String {
    path_escape(name).replace('@', "%40")
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
        assert!(matches!(request_parts(&d("claude"), "/embeddings", &none), Err(BuildError::UnsupportedRoute(_))));

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

    fn vertex(providers: &str, deployments: &str) -> Config {
        let w: Wire = serde_json::from_str(&format!(
            r#"{{"schema":2,"version":1,"group":{{"id":"g","name":"g"}},"providers":[{providers}],"deployments":[{deployments}]}}"#
        ))
        .unwrap();
        Config::build(w).unwrap()
    }

    /// The URL and body a request for `model` goes out with.
    fn send(c: &Config, model: &str, path: &str, body: &str) -> Result<(String, HeaderMap, String), BuildError> {
        let d = c.resolve(model).unwrap().deployments[0].clone();
        let (url, h) = request_parts(&d, path, &Inbound::default())?;
        let (url, body) = finish(&d, url, Bytes::from(body.to_string())).map_err(|e| BuildError::Unsupported(e.0))?;
        Ok((url, h, String::from_utf8(body.to_vec()).unwrap()))
    }

    // internal/provider/gcp_test.go TestVertexURLs
    #[test]
    fn vertex_urls() {
        let p = |id: &str, loc: &str, base: &str| {
            format!(
                r#"{{"id":"{id}","name":"{id}","type":"gcp_vertex","auth_mode":"none","project":"proj-1","location":"{loc}","base_url":"{base}"}}"#
            )
        };
        let c = vertex(
            &[
                p("global", "global", ""),
                p("east", "us-east5", ""),
                p("private", "us-east5", "https://vertex.internal/"),
                p("stale", "us-east5", "https://aiplatform.googleapis.com"),
                p("blank", "", ""),
            ]
            .join(","),
            r#"{"id":"1","provider_id":"global","model_name":"gemini","upstream_name":"google/gemini-2.5-flash"},
               {"id":"2","provider_id":"east","model_name":"gemini-east","upstream_name":"google/gemini-2.5-flash"},
               {"id":"3","provider_id":"east","model_name":"sonnet","upstream_name":"claude-sonnet-4-5@20250929","api_flavor":"anthropic"},
               {"id":"4","provider_id":"east","model_name":"haiku","upstream_name":"claude-haiku-4-5@20251001","api_flavor":"anthropic"},
               {"id":"5","provider_id":"private","model_name":"private","upstream_name":"google/gemini-2.5-flash"},
               {"id":"6","provider_id":"stale","model_name":"stale","upstream_name":"google/gemini-2.5-flash"},
               {"id":"7","provider_id":"blank","model_name":"blank","upstream_name":"google/gemini-2.5-flash"}"#,
        );
        let cases = [
            (
                "gemini",
                "/chat/completions",
                r#"{"model":"google/gemini-2.5-flash"}"#,
                "https://aiplatform.googleapis.com/v1/projects/proj-1/locations/global/endpoints/openapi/chat/completions",
            ),
            // regional host derived from the location
            (
                "gemini-east",
                "/chat/completions",
                "{}",
                "https://us-east5-aiplatform.googleapis.com/v1/projects/proj-1/locations/us-east5/endpoints/openapi/chat/completions",
            ),
            // rawPredict escapes the version suffix
            (
                "sonnet",
                "/messages",
                r#"{"model":"claude-sonnet-4-5@20250929"}"#,
                "https://us-east5-aiplatform.googleapis.com/v1/projects/proj-1/locations/us-east5/publishers/anthropic/models/claude-sonnet-4-5%4020250929:rawPredict",
            ),
            // streamRawPredict when the body streams
            (
                "haiku",
                "/chat/completions",
                r#"{"stream":true}"#,
                "https://us-east5-aiplatform.googleapis.com/v1/projects/proj-1/locations/us-east5/publishers/anthropic/models/claude-haiku-4-5%4020251001:streamRawPredict",
            ),
            // a private base_url wins over the derived host
            (
                "private",
                "/chat/completions",
                "{}",
                "https://vertex.internal/v1/projects/proj-1/locations/us-east5/endpoints/openapi/chat/completions",
            ),
            // a stored public host from an earlier location follows the location
            (
                "stale",
                "/chat/completions",
                "{}",
                "https://us-east5-aiplatform.googleapis.com/v1/projects/proj-1/locations/us-east5/endpoints/openapi/chat/completions",
            ),
            (
                "blank",
                "/chat/completions",
                "{}",
                "https://aiplatform.googleapis.com/v1/projects/proj-1/locations/global/endpoints/openapi/chat/completions",
            ),
        ];
        for (model, path, body, want) in cases {
            let (url, h, _) = send(&c, model, path, body).unwrap();
            assert_eq!(url, want, "{model}");
            // Vertex takes the API version in the body; pinning it in the header
            // too is rejected upstream.
            assert!(h.get("anthropic-version").is_none(), "{model}");
        }
        // Gemini's body passes through untouched.
        assert_eq!(send(&c, "gemini", "/chat/completions", r#"{"model":"m"}"#).unwrap().2, r#"{"model":"m"}"#);
    }

    // TestVertexAnthBody
    #[test]
    fn vertex_anthropic_body_reshape() {
        let (out, stream) = vertex_anthropic_body(br#"{"model":"claude-x","max_tokens":8,"messages":[]}"#).unwrap();
        assert!(!stream);
        let m: serde_json::Value = serde_json::from_slice(&out).unwrap();
        assert!(m.get("model").is_none(), "vertex carries the model in the URL and rejects it in the body");
        assert_eq!(m["anthropic_version"], VERTEX_ANTHROPIC_VERSION);
        assert_eq!(m["max_tokens"], 8);
        assert_eq!(m["messages"], serde_json::json!([]));

        // A client that already pinned a version keeps it.
        let (out, stream) =
            vertex_anthropic_body(br#"{"anthropic_version":"vertex-2024-01-01","stream":true}"#).unwrap();
        assert!(stream);
        let m: serde_json::Value = serde_json::from_slice(&out).unwrap();
        assert_eq!(m["anthropic_version"], "vertex-2024-01-01");
        assert!(vertex_anthropic_body(b"{").is_err());
    }

    // TestVertexRejectsEmbeddings, TestVertexRequiresProject, ValidateVertex's location check
    #[test]
    fn vertex_refusals() {
        let c = vertex(
            r#"{"id":"ok","name":"ok","type":"gcp_vertex","auth_mode":"none","project":"p"},
               {"id":"noproj","name":"gcp","type":"gcp_vertex","auth_mode":"none"},
               {"id":"evil","name":"evil","type":"gcp_vertex","auth_mode":"none","project":"p","location":"x.evil.com/"}"#,
            r#"{"id":"1","provider_id":"ok","model_name":"emb","upstream_name":"google/text-embedding-005"},
               {"id":"2","provider_id":"noproj","model_name":"noproj","upstream_name":"google/gemini-2.5-flash"},
               {"id":"3","provider_id":"evil","model_name":"evil","upstream_name":"claude-x","api_flavor":"anthropic"}"#,
        );
        assert!(
            matches!(send(&c, "emb", "/embeddings", "{}"), Err(BuildError::UnsupportedRoute(m)) if m.contains("not supported"))
        );
        assert!(
            matches!(send(&c, "noproj", "/chat/completions", "{}"), Err(BuildError::Unsupported(m)) if m.contains("project"))
        );
        assert!(
            matches!(send(&c, "evil", "/messages", "{}"), Err(BuildError::Unsupported(m)) if m.contains("not a valid region"))
        );
    }

    #[test]
    fn public_vertex_hosts() {
        for (url, public) in [
            ("https://aiplatform.googleapis.com", true),
            ("https://aiplatform.googleapis.com/", true),
            ("https://us-east5-aiplatform.googleapis.com", true),
            ("https://-aiplatform.googleapis.com", false),
            ("https://vertex.internal", false),
            ("http://aiplatform.googleapis.com", false),
            ("https://evil.com/aiplatform.googleapis.com", false),
        ] {
            assert_eq!(public_vertex_host(url), public, "{url}");
        }
    }

    #[test]
    fn cloud_auth_modes_leave_the_header_to_the_token() {
        let c = vertex(
            r#"{"id":"az","name":"az","type":"azure","base_url":"https://r.openai.azure.com","auth_mode":"entra"},
               {"id":"sa","name":"sa","type":"gcp_vertex","auth_mode":"gcp_sa","api_key":"","project":"p"}"#,
            r#"{"id":"1","provider_id":"az","model_name":"gpt","upstream_name":"dep"},
               {"id":"2","provider_id":"sa","model_name":"gemini","upstream_name":"google/gemini-2.5-flash"}"#,
        );
        for m in ["gpt", "gemini"] {
            let d = c.resolve(m).unwrap().deployments[0].clone();
            assert!(d.provider.token.is_some(), "{m}");
            let (_, h) = request_parts(&d, "/chat/completions", &Inbound::default()).unwrap();
            assert!(h.get("authorization").is_none() && h.get("api-key").is_none(), "{m}");
        }
    }
}
