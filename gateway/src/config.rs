//! The config a gateway runs on: the wire format the control plane sends
//! (internal/gwsync/config.go) and the in-memory model the hot path reads.
//! Everything a request needs is resolved here once per config version: keys
//! point at their user and team, deployments at their provider, upstream URLs
//! and auth headers are built, and limit state is attached.

use std::collections::{HashMap, HashSet};
use std::sync::Arc;
use std::time::Duration;

use http::HeaderValue;
use serde::Deserialize;

use crate::health::{self, Health};
use crate::limits::{self, Scope};

/// The wire schema this gateway speaks (gwsync.schemaVersion).
pub const SCHEMA: i64 = 1;

// ---- wire ---------------------------------------------------------------------------------

#[derive(Debug, Deserialize)]
pub struct Wire {
    pub schema: i64,
    pub version: i64,
    pub group: WireGroup,
    #[serde(default)]
    pub keys: Vec<WireKey>,
    #[serde(default)]
    pub users: Vec<WireUser>,
    #[serde(default)]
    pub teams: Vec<WireTeam>,
    #[serde(default)]
    pub providers: Vec<WireProvider>,
    #[serde(default)]
    pub deployments: Vec<WireDeployment>,
    #[serde(default)]
    pub prices: HashMap<String, Price>,
}

#[derive(Debug, Clone, Deserialize)]
pub struct WireGroup {
    pub id: String,
    pub name: String,
    #[serde(default)]
    pub region: String,
    #[serde(default)]
    pub data_rule: String,
}

#[derive(Debug, Deserialize)]
pub struct WireKey {
    pub hash: String,
    pub id: String,
    #[serde(default)]
    pub user_id: String,
    #[serde(default)]
    pub team_id: String,
    pub allowed_models: Option<Vec<String>>,
    pub budget_usd: Option<f64>,
    pub rpm_limit: Option<i64>,
    pub tpm_limit: Option<i64>,
    #[serde(default)]
    pub expires_at: i64,
}

#[derive(Debug, Deserialize)]
pub struct WireUser {
    pub id: String,
    pub allowed_models: Option<Vec<String>>,
    pub team_models: Option<Vec<String>>,
    #[serde(default)]
    pub memberships: HashMap<String, f64>,
    pub budget_usd: Option<f64>,
    pub rpm_limit: Option<i64>,
    pub tpm_limit: Option<i64>,
}

#[derive(Debug, Deserialize)]
pub struct WireTeam {
    pub id: String,
    pub allowed_models: Option<Vec<String>>,
    pub budget_usd: Option<f64>,
    pub rpm_limit: Option<i64>,
    pub tpm_limit: Option<i64>,
}

#[derive(Debug, Deserialize)]
pub struct WireProvider {
    pub id: String,
    pub name: String,
    #[serde(rename = "type")]
    pub kind: String,
    #[serde(default)]
    pub base_url: String,
    #[serde(default)]
    pub auth_mode: String,
    #[serde(default)]
    pub api_key: String,
    #[serde(default)]
    pub api_version: String,
    #[serde(default)]
    pub project: String,
    #[serde(default)]
    pub location: String,
}

#[derive(Debug, Deserialize)]
pub struct WireDeployment {
    pub id: String,
    pub provider_id: String,
    pub model_name: String,
    pub upstream_name: String,
    #[serde(default)]
    pub api_flavor: String,
    pub timeout_ms: Option<u64>,
    #[serde(default)]
    pub catalog_model_id: String,
    pub input_per_1m: Option<f64>,
    pub output_per_1m: Option<f64>,
    pub cached_input_per_1m: Option<f64>,
}

#[derive(Debug, Clone, Copy, Deserialize)]
pub struct Price {
    pub input_per_1m: f64,
    pub output_per_1m: f64,
    pub cached_input_per_1m: Option<f64>,
}

// ---- model --------------------------------------------------------------------------------

/// A model policy: None doesn't restrict, an empty set admits nothing.
pub type Policy = Option<HashSet<String>>;

fn policy(list: Option<Vec<String>>) -> Policy {
    list.map(|l| l.into_iter().collect())
}

fn allows(p: &Policy, names: &[&str]) -> bool {
    match p {
        None => true,
        Some(set) => names.iter().any(|n| set.contains(*n)),
    }
}

pub struct Team {
    pub id: String,
    pub allowed_models: Policy,
    pub scope: Option<Arc<Scope>>,
}

pub struct User {
    pub id: String,
    pub allowed_models: Policy,
    pub team_models: Policy,
    /// The member's share of a team's budget, by team id.
    pub memberships: HashMap<String, Arc<Scope>>,
    pub scope: Option<Arc<Scope>>,
}

pub struct Key {
    pub id: String,
    pub user_id: String,
    pub team_id: String,
    pub user: Option<Arc<User>>,
    pub team: Option<Arc<Team>>,
    pub allowed_models: Policy,
    pub expires_at: i64,
    pub scope: Option<Arc<Scope>>,
}

impl Key {
    /// The model policy: a name passes when the team, the user (their own
    /// policy, else their teams') and the key all allow it, or the pattern it
    /// resolved through (auth.Identity.ModelAllowed).
    pub fn model_allowed(&self, names: &[&str]) -> bool {
        if let Some(t) = &self.team
            && !allows(&t.allowed_models, names)
        {
            return false;
        }
        if let Some(u) = &self.user {
            let p = if u.allowed_models.is_some() { &u.allowed_models } else { &u.team_models };
            if !allows(p, names) {
                return false;
            }
        }
        allows(&self.allowed_models, names)
    }

    /// The limit scopes this key's requests count against, in check order:
    /// key, user, team, then the member's share of the team.
    pub fn scopes(&self) -> impl Iterator<Item = &Arc<Scope>> {
        let member = match (&self.user, &self.team) {
            (Some(u), Some(t)) => u.memberships.get(&t.id),
            _ => None,
        };
        self.scope
            .iter()
            .chain(self.user.as_ref().and_then(|u| u.scope.as_ref()))
            .chain(self.team.as_ref().and_then(|t| t.scope.as_ref()))
            .chain(member)
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Flavor {
    OpenAI,
    Anthropic,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ProviderKind {
    Azure,
    OpenAICompatible,
    OpenRouter,
    Vertex,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AuthMode {
    None,
    Bearer,
    ApiKey,
    OAuthPassthrough,
    /// Needs a cloud token the gateway can't mint yet (Entra, GCP).
    Unsupported,
}

pub struct Provider {
    pub id: String,
    pub name: String,
    pub kind: ProviderKind,
    pub base_url: String,
    pub auth_mode: AuthMode,
    pub api_version: String,
    /// The credential header, built once: (name, value).
    pub auth_header: Option<(http::HeaderName, HeaderValue)>,
}

pub struct Deployment {
    pub id: String,
    pub provider: Arc<Provider>,
    pub model_name: String,
    pub upstream_name: String,
    pub flavor: Flavor,
    pub timeout: Duration,
    pub catalog_model_id: String,
    pub input_per_1m: Option<f64>,
    pub output_per_1m: Option<f64>,
    pub cached_input_per_1m: Option<f64>,
    pub health: Arc<Health>,
    /// The upstream URL for an OpenAI-flavor request, minus the path: the path
    /// (/chat/completions, ...) goes between `url_prefix` and `url_suffix`.
    pub url_prefix: String,
    pub url_suffix: String,
}

impl Deployment {
    /// The deployment's upstream name with a pattern's tail filled in.
    pub fn with_upstream(&self, upstream: String) -> Deployment {
        Deployment {
            id: self.id.clone(),
            provider: self.provider.clone(),
            model_name: self.model_name.clone(),
            upstream_name: upstream,
            flavor: self.flavor,
            timeout: self.timeout,
            catalog_model_id: self.catalog_model_id.clone(),
            input_per_1m: self.input_per_1m,
            output_per_1m: self.output_per_1m,
            cached_input_per_1m: self.cached_input_per_1m,
            health: self.health.clone(),
            url_prefix: self.url_prefix.clone(),
            url_suffix: self.url_suffix.clone(),
        }
    }
}

/// The default wait for an upstream's response headers (gateway.defaultUpstreamTimeout).
pub const DEFAULT_TIMEOUT: Duration = Duration::from_secs(600);

pub struct Config {
    pub version: i64,
    pub group: WireGroup,
    pub keys: HashMap<[u8; 32], Arc<Key>>,
    /// Exact model names → deployments in routing order.
    pub models: HashMap<String, Arc<[Arc<Deployment>]>>,
    /// Prefix patterns ("claude-*"), longest prefix first.
    pub patterns: Vec<(String, Arc<[Arc<Deployment>]>)>,
    /// Exact names in order, for /v1/models.
    pub names: Vec<String>,
    pub prices: HashMap<String, Price>,
}

/// What a requested model name resolves to.
pub struct Resolved {
    /// The name or pattern it matched, for the allowlist.
    pub alias: String,
    pub deployments: Arc<[Arc<Deployment>]>,
}

impl Config {
    /// Build the hot-path model from the wire config.
    pub fn build(w: Wire) -> Result<Config, String> {
        if w.schema > SCHEMA {
            return Err(format!("config schema {} is newer than this gateway ({SCHEMA})", w.schema));
        }
        let teams: HashMap<String, Arc<Team>> = w
            .teams
            .into_iter()
            .map(|t| {
                let scope = limits::scope(format!("team:{}", t.id), t.budget_usd, t.rpm_limit, t.tpm_limit);
                (t.id.clone(), Arc::new(Team { id: t.id, allowed_models: policy(t.allowed_models), scope }))
            })
            .collect();
        let users: HashMap<String, Arc<User>> = w
            .users
            .into_iter()
            .map(|u| {
                let memberships = u
                    .memberships
                    .iter()
                    .filter_map(|(team, budget)| {
                        limits::scope(format!("member:{}:{}", u.id, team), Some(*budget), None, None)
                            .map(|s| (team.clone(), s))
                    })
                    .collect();
                let scope = limits::scope(format!("user:{}", u.id), u.budget_usd, u.rpm_limit, u.tpm_limit);
                let user = User {
                    id: u.id.clone(),
                    allowed_models: policy(u.allowed_models),
                    team_models: policy(u.team_models),
                    memberships,
                    scope,
                };
                (u.id, Arc::new(user))
            })
            .collect();
        let mut keys = HashMap::with_capacity(w.keys.len());
        for k in w.keys {
            let Some(hash) = unhex32(&k.hash) else { continue };
            let user = users.get(&k.user_id).cloned();
            if !k.user_id.is_empty() && user.is_none() {
                continue; // a disabled user's keys don't work
            }
            let scope = limits::scope(format!("key:{}", k.id), k.budget_usd, k.rpm_limit, k.tpm_limit);
            let key = Key {
                team: teams.get(&k.team_id).cloned(),
                user,
                id: k.id,
                user_id: k.user_id,
                team_id: k.team_id,
                allowed_models: policy(k.allowed_models),
                expires_at: k.expires_at,
                scope,
            };
            keys.insert(hash, Arc::new(key));
        }

        let providers: HashMap<String, Arc<Provider>> =
            w.providers.into_iter().map(|p| (p.id.clone(), Arc::new(provider(p)))).collect();
        let mut by_model: Vec<(String, Vec<Arc<Deployment>>)> = Vec::new();
        for d in w.deployments {
            let Some(p) = providers.get(&d.provider_id) else { continue };
            let flavor = if d.api_flavor == "anthropic" { Flavor::Anthropic } else { Flavor::OpenAI };
            let (url_prefix, url_suffix) = crate::upstream::url_parts(p, &d.upstream_name, flavor);
            let dep = Arc::new(Deployment {
                health: health::get(&d.id),
                id: d.id,
                provider: p.clone(),
                upstream_name: d.upstream_name,
                flavor,
                timeout: d.timeout_ms.filter(|&ms| ms > 0).map(Duration::from_millis).unwrap_or(DEFAULT_TIMEOUT),
                catalog_model_id: d.catalog_model_id,
                input_per_1m: d.input_per_1m,
                output_per_1m: d.output_per_1m,
                cached_input_per_1m: d.cached_input_per_1m,
                url_prefix,
                url_suffix,
                model_name: d.model_name.clone(),
            });
            // The control plane sends each model's deployments together, in
            // routing order for this group.
            match by_model.last_mut() {
                Some((name, list)) if *name == d.model_name => list.push(dep),
                _ => by_model.push((d.model_name, vec![dep])),
            }
        }
        let mut models = HashMap::new();
        let mut patterns = Vec::new();
        let mut names = Vec::new();
        for (name, list) in by_model {
            let list: Arc<[Arc<Deployment>]> = list.into();
            if let Some(prefix) = name.strip_suffix('*') {
                patterns.push((prefix.to_string(), list));
            } else {
                names.push(name.clone());
                models.insert(name, list);
            }
        }
        patterns.sort_by(|a, b| b.0.len().cmp(&a.0.len()));
        names.sort();
        Ok(Config { version: w.version, group: w.group, keys, models, patterns, names, prices: w.prices })
    }

    /// Resolve a requested model (snapshot.Resolve): an exact name wins, else
    /// the longest matching pattern; a pattern deployment whose upstream name
    /// also ends in "*" gets the rest of the requested name.
    pub fn resolve(&self, name: &str) -> Option<Resolved> {
        if name.ends_with('*') {
            return None;
        }
        if let Some(ds) = self.models.get(name) {
            return Some(Resolved { alias: name.to_string(), deployments: ds.clone() });
        }
        let (prefix, ds) = self.patterns.iter().find(|(p, _)| name.starts_with(p.as_str()))?;
        let rest = &name[prefix.len()..];
        let alias = format!("{prefix}*");
        if !ds.iter().any(|d| d.upstream_name.ends_with('*')) {
            return Some(Resolved { alias, deployments: ds.clone() });
        }
        let mut out = Vec::with_capacity(ds.len());
        for d in ds.iter() {
            match d.upstream_name.strip_suffix('*') {
                Some(head) => {
                    let up = format!("{head}{rest}");
                    // Azure puts the upstream name in a URL path segment, sent
                    // with the gateway's own credential: never a dot segment.
                    if up == "." || up == ".." {
                        return None;
                    }
                    let mut dd = d.with_upstream(up);
                    let (p, s) = crate::upstream::url_parts(&d.provider, &dd.upstream_name, d.flavor);
                    dd.url_prefix = p;
                    dd.url_suffix = s;
                    out.push(Arc::new(dd));
                }
                None => out.push(d.clone()),
            }
        }
        Some(Resolved { alias, deployments: out.into() })
    }

    /// Whether every deployment of a model forwards the caller's own
    /// credential, so callers without one shouldn't see it.
    pub fn caller_credential_only(&self, name: &str) -> bool {
        self.models.get(name).is_some_and(|ds| ds.iter().all(|d| d.provider.auth_mode == AuthMode::OAuthPassthrough))
    }
}

fn provider(p: WireProvider) -> Provider {
    let kind = match p.kind.as_str() {
        "azure" => ProviderKind::Azure,
        "openrouter" => ProviderKind::OpenRouter,
        "gcp_vertex" => ProviderKind::Vertex,
        _ => ProviderKind::OpenAICompatible,
    };
    let auth_mode = match p.auth_mode.as_str() {
        "" | "none" => AuthMode::None,
        "bearer" => AuthMode::Bearer,
        "api_key" => AuthMode::ApiKey,
        "oauth_passthrough" => AuthMode::OAuthPassthrough,
        _ => AuthMode::Unsupported,
    };
    let auth_header = match auth_mode {
        AuthMode::Bearer => {
            HeaderValue::try_from(format!("Bearer {}", p.api_key)).ok().map(|v| (http::header::AUTHORIZATION, v))
        }
        AuthMode::ApiKey => {
            HeaderValue::try_from(p.api_key.as_str()).ok().map(|v| (http::HeaderName::from_static("api-key"), v))
        }
        _ => None,
    };
    let base_url = if p.base_url.is_empty() && kind == ProviderKind::OpenRouter {
        "https://openrouter.ai/api/v1".to_string()
    } else {
        p.base_url.trim_end_matches('/').to_string()
    };
    Provider { id: p.id, name: p.name, kind, base_url, auth_mode, api_version: p.api_version, auth_header }
}

fn unhex32(s: &str) -> Option<[u8; 32]> {
    let b = s.as_bytes();
    if b.len() != 64 {
        return None;
    }
    let mut out = [0u8; 32];
    for (i, pair) in b.chunks(2).enumerate() {
        let v = |c: u8| match c {
            b'0'..=b'9' => Some(c - b'0'),
            b'a'..=b'f' => Some(c - b'a' + 10),
            b'A'..=b'F' => Some(c - b'A' + 10),
            _ => None,
        };
        out[i] = v(pair[0])? << 4 | v(pair[1])?;
    }
    Some(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn wire(json: &str) -> Config {
        Config::build(serde_json::from_str(json).unwrap()).unwrap()
    }

    const CFG: &str = r#"{
        "schema": 1, "version": 3, "group": {"id": "g", "name": "eu", "region": "eu", "data_rule": "inherit"},
        "keys": [
            {"hash": "0101010101010101010101010101010101010101010101010101010101010101", "id": "k1", "user_id": "u1", "team_id": "t1", "allowed_models": null},
            {"hash": "0202020202020202020202020202020202020202020202020202020202020202", "id": "k2", "user_id": "gone", "allowed_models": null}
        ],
        "users": [{"id": "u1", "allowed_models": null, "team_models": ["m1", "claude-*"]}],
        "teams": [{"id": "t1", "allowed_models": null, "rpm_limit": 10}],
        "providers": [
            {"id": "p1", "name": "a", "type": "openai_compatible", "base_url": "http://a/v1/", "auth_mode": "bearer", "api_key": "sk"},
            {"id": "p2", "name": "z", "type": "azure", "base_url": "https://z", "auth_mode": "api_key", "api_key": "k"}
        ],
        "deployments": [
            {"id": "d1", "provider_id": "p1", "model_name": "claude-*", "upstream_name": "claude-*", "api_flavor": "anthropic"},
            {"id": "d2", "provider_id": "p1", "model_name": "m1", "upstream_name": "up1"},
            {"id": "d3", "provider_id": "p2", "model_name": "m1", "upstream_name": "dep", "timeout_ms": 5000}
        ],
        "prices": {}
    }"#;

    #[test]
    fn builds_and_resolves() {
        let c = wire(CFG);
        assert_eq!(c.keys.len(), 1, "a key whose user is gone must not load");
        let k = c.keys.get(&[1u8; 32]).unwrap();
        assert!(k.team.is_some() && k.user.is_some());
        assert_eq!(k.scopes().count(), 1, "only the team has a limit");

        let r = c.resolve("m1").unwrap();
        assert_eq!(r.deployments.iter().map(|d| d.id.as_str()).collect::<Vec<_>>(), ["d2", "d3"]);
        assert_eq!(r.deployments[1].timeout, Duration::from_secs(5));

        let r = c.resolve("claude-sonnet-4").unwrap();
        assert_eq!(r.alias, "claude-*");
        assert_eq!(r.deployments[0].upstream_name, "claude-sonnet-4");
        assert!(k.model_allowed(&["claude-sonnet-4", &r.alias]));
        assert!(!k.model_allowed(&["m2"]));
        assert!(c.resolve("claude-*").is_none());
        assert!(c.resolve("nope").is_none());
    }

    #[test]
    fn rejects_dot_segments() {
        let c = wire(CFG);
        assert!(c.resolve("claude-").is_some());
        let mut w: Wire = serde_json::from_str(CFG).unwrap();
        w.deployments[0].upstream_name = "*".into();
        w.deployments[0].model_name = "x*".into();
        let c = Config::build(w).unwrap();
        assert!(c.resolve("x..").is_none());
    }
}
