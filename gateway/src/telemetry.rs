//! What a request exports (internal/snapshot/telemetry.go): the capture policy
//! with its deterministic sampling, the gateway group's data rule, and the
//! content scopes of the destinations that take the group's traffic with
//! content. Also the caller's W3C traceparent (internal/gateway/traceparent.go).
//!
//! The gateway's verdict decides only what content it captures: it captures
//! content only when the policy does and some destination would take it, so a
//! request nobody wants content of costs nothing extra. The verdict doesn't
//! travel with the event: the control plane decides export, content and rule
//! again for each event, then per destination what each one receives.

use std::sync::Arc;

use http::HeaderMap;
use serde::Deserialize;

use crate::config::Key;

// ---- wire (gwsync.Telemetry) ------------------------------------------------------------

#[derive(Debug, Default, Deserialize)]
pub struct WireTelemetry {
    #[serde(default)]
    pub mode: String,
    #[serde(default)]
    pub capture_content: bool,
    #[serde(default)]
    pub rules: Option<Vec<WireRule>>,
    #[serde(default)]
    pub content: Option<Vec<WireContent>>,
}

#[derive(Debug, Default, Deserialize)]
#[serde(default)]
pub struct WireRule {
    pub id: String,
    pub order: i64,
    pub scope_type: String,
    pub scope_value: String,
    pub capture: String,
    pub sample: i64,
    pub enabled: bool,
}

#[derive(Debug, Default, Deserialize)]
#[serde(default)]
pub struct WireContent {
    pub teams: WireScope,
    pub max_bytes: i64,
}

#[derive(Debug, Default, Deserialize)]
#[serde(default)]
pub struct WireScope {
    pub mode: String,
    pub values: Option<Vec<String>>,
}

// ---- policy -----------------------------------------------------------------------------

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Mode {
    Everything,
    ByRule,
    Off,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum RuleScope {
    Team,
    User,
    Key,
    Tag,
    Unknown,
}

#[derive(Debug)]
struct Rule {
    id: Arc<str>,
    scope: RuleScope,
    value: String,
    content: bool,
    sample: i64,
}

/// Teams or groups: all of them, only `values`, or all except `values`.
#[derive(Debug)]
pub struct Scope {
    mode: ScopeMode,
    values: Vec<String>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum ScopeMode {
    All,
    Selected,
    Except,
}

impl Scope {
    fn from_wire(w: WireScope) -> Scope {
        let mode = match w.mode.as_str() {
            "selected" => ScopeMode::Selected,
            "except" => ScopeMode::Except,
            _ => ScopeMode::All,
        };
        Scope { mode, values: w.values.unwrap_or_default() }
    }

    fn matches(&self, v: &str) -> bool {
        match self.mode {
            ScopeMode::All => true,
            ScopeMode::Selected => self.values.iter().any(|x| x == v),
            ScopeMode::Except => !self.values.iter().any(|x| x == v),
        }
    }
}

#[derive(Debug)]
struct ContentScope {
    teams: Scope,
    max_bytes: usize,
}

/// The capture policy, with the rules that can decide anything in the order
/// they are tried, and this group's content scopes.
#[derive(Debug)]
pub struct Policy {
    mode: Mode,
    capture_content: bool,
    rules: Vec<Rule>,
    content: Vec<ContentScope>,
}

impl Default for Policy {
    /// What a control plane that sends no policy runs: export everything,
    /// capture no content.
    fn default() -> Policy {
        Policy { mode: Mode::Everything, capture_content: false, rules: Vec::new(), content: Vec::new() }
    }
}

/// The policy's verdict for one request (snapshot.Decision).
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Decision {
    pub export: bool,
    pub capture_content: bool,
    pub rule_id: Option<Arc<str>>,
}

impl Policy {
    pub fn from_wire(w: Option<WireTelemetry>) -> Policy {
        let Some(w) = w else { return Policy::default() };
        let mode = match w.mode.as_str() {
            "off" => Mode::Off,
            "by_rule" => Mode::ByRule,
            _ => Mode::Everything,
        };
        let mut rules: Vec<WireRule> = w.rules.unwrap_or_default();
        rules.sort_by_key(|r| r.order); // stable, as Go's sort.SliceStable
        let rules = rules
            .into_iter()
            .filter(|r| r.enabled)
            .map(|r| Rule {
                id: r.id.into(),
                scope: match r.scope_type.as_str() {
                    "team" => RuleScope::Team,
                    "user" => RuleScope::User,
                    "key" => RuleScope::Key,
                    "tag" => RuleScope::Tag,
                    _ => RuleScope::Unknown,
                },
                value: r.scope_value,
                content: r.capture == "content",
                sample: r.sample,
            })
            .collect();
        let content = w
            .content
            .unwrap_or_default()
            .into_iter()
            .filter(|c| c.max_bytes > 0)
            .map(|c| ContentScope { teams: Scope::from_wire(c.teams), max_bytes: c.max_bytes as usize })
            .collect();
        Policy { mode, capture_content: w.capture_content, rules, content }
    }

    /// Whether this request is exported and its content captured
    /// (Telemetry.Evaluate): the first enabled rule, by order, whose scope
    /// matches decides.
    pub fn evaluate(&self, key: &Key, request_id: &str) -> Decision {
        match self.mode {
            Mode::Off => Decision::default(),
            Mode::Everything => Decision { export: true, capture_content: self.capture_content, rule_id: None },
            Mode::ByRule => {
                for r in &self.rules {
                    if !rule_matches(r, key) {
                        continue;
                    }
                    if !sampled(&r.id, request_id, r.sample) {
                        return Decision { rule_id: Some(r.id.clone()), ..Decision::default() };
                    }
                    return Decision { export: true, capture_content: r.content, rule_id: Some(r.id.clone()) };
                }
                Decision::default()
            }
        }
    }

    /// The most content bytes a destination taking this team's traffic with
    /// content wants (Snapshot.ContentWanted); 0 when none does.
    pub fn content_wanted(&self, team: &str) -> usize {
        self.content.iter().filter(|c| c.teams.matches(team)).map(|c| c.max_bytes).max().unwrap_or(0)
    }
}

fn rule_matches(r: &Rule, key: &Key) -> bool {
    let user = key.user.as_deref();
    let team = key.team.as_deref();
    match r.scope {
        RuleScope::Key => r.value == key.id,
        RuleScope::User => r.value == user.map_or("", |u| u.id.as_str()),
        RuleScope::Team => r.value == team.map_or("", |t| t.id.as_str()),
        RuleScope::Tag => {
            let has = |tags: &[String]| tags.contains(&r.value);
            has(&key.tags) || user.is_some_and(|u| has(&u.tags)) || team.is_some_and(|t| has(&t.tags))
        }
        RuleScope::Unknown => false,
    }
}

/// Passes `sample` percent of requests, the same ones on every control plane
/// and gateway (snapshot.Sampled): 0 never, 100 always.
pub fn sampled(rule_id: &str, request_id: &str, sample: i64) -> bool {
    if sample >= 100 {
        return true;
    }
    sample > 0 && bucket(rule_id, request_id) < sample as u64
}

/// The request's sampling bucket for a rule, 0..99: FNV-1a 64 over
/// "rule_id:request_id", modulo 100.
pub fn bucket(rule_id: &str, request_id: &str) -> u64 {
    let mut h: u64 = 0xcbf2_9ce4_8422_2325;
    for &b in rule_id.as_bytes().iter().chain(b":").chain(request_id.as_bytes()) {
        h ^= b as u64;
        h = h.wrapping_mul(0x0000_0100_0000_01b3);
    }
    h % 100
}

/// What a gateway group lets leave it.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum DataRule {
    #[default]
    Inherit,
    NoContent,
    NoExport,
}

impl DataRule {
    /// Unknown rules keep content in, as Snapshot.ContentWanted does.
    pub fn parse(s: &str) -> DataRule {
        match s {
            "" | "inherit" => DataRule::Inherit,
            "no_export" => DataRule::NoExport,
            _ => DataRule::NoContent,
        }
    }
}

/// Everything decided about a request's telemetry once it is authenticated.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Verdict {
    pub decision: Decision,
    /// Bytes of content to capture per part; 0 captures nothing.
    pub capture: usize,
}

/// The policy's decision, taken away from by the group's data rule, and how
/// much content to capture. A caller's traceparent doesn't take part: its
/// sampled flag would let any key holder opt out of export.
pub fn decide(policy: &Policy, rule: DataRule, key: &Key, request_id: &str) -> Verdict {
    let mut d = policy.evaluate(key, request_id);
    match rule {
        DataRule::Inherit => {}
        DataRule::NoContent => d.capture_content = false,
        DataRule::NoExport => (d.export, d.capture_content) = (false, false),
    }
    let capture = if d.capture_content && d.export {
        policy.content_wanted(key.team.as_deref().map_or("", |t| t.id.as_str()))
    } else {
        0
    };
    Verdict { decision: d, capture }
}

// ---- traces -----------------------------------------------------------------------------

/// A request's place in a trace: the caller's, from its traceparent, or new.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Trace {
    pub id: u128,
    /// The caller's span; 0 for a new trace.
    pub parent: u64,
    /// The caller's sampled flag. Read, never acted on: what is exported is
    /// the policy's decision alone.
    pub sampled: bool,
}

impl Trace {
    /// The caller's trace when it sent a valid traceparent, else a new one.
    pub fn read(h: &HeaderMap) -> Trace {
        if let Some((id, parent, flags)) = h.get("traceparent").and_then(|v| parse_traceparent(v.as_bytes())) {
            return Trace { id, parent, sampled: flags & 1 == 1 };
        }
        Trace::new()
    }

    pub fn new() -> Trace {
        Trace { id: fastrand::u128(1..), parent: 0, sampled: true }
    }

    /// The trace id as 32 lowercase hex digits.
    pub fn hex(&self) -> [u8; 32] {
        hex128(self.id)
    }
}

impl Default for Trace {
    fn default() -> Trace {
        Trace::new()
    }
}

pub fn hex128(n: u128) -> [u8; 32] {
    let mut out = [0u8; 32];
    for (i, b) in out.iter_mut().enumerate() {
        *b = HEX[(n >> (124 - 4 * i) & 0xf) as usize];
    }
    out
}

pub fn hex64(n: u64) -> [u8; 16] {
    let mut out = [0u8; 16];
    for (i, b) in out.iter_mut().enumerate() {
        *b = HEX[(n >> (60 - 4 * i) & 0xf) as usize];
    }
    out
}

const HEX: &[u8; 16] = b"0123456789abcdef";

/// A W3C traceparent (trace-context level 1): version-traceid-parentid-flags
/// in lowercase hex. Version ff is invalid; version 00 must be exactly 55
/// characters, and a later version may append fields after a "-". All-zero
/// ids are invalid. internal/gateway/testdata/traceparent.json pins it.
pub fn parse_traceparent(mut h: &[u8]) -> Option<(u128, u64, u8)> {
    while let [b' ' | b'\t', rest @ ..] = h {
        h = rest;
    }
    while let [rest @ .., b' ' | b'\t'] = h {
        h = rest;
    }
    if h.len() < 55 || h[2] != b'-' || h[35] != b'-' || h[52] != b'-' {
        return None;
    }
    let version = hex_n(&h[0..2])? as u8;
    if version == 0xff || (version == 0 && h.len() != 55) || (h.len() > 55 && h[55] != b'-') {
        return None;
    }
    let id = hex_n(&h[3..35])?;
    let parent = hex_n(&h[36..52])? as u64;
    let flags = hex_n(&h[53..55])? as u8;
    (id != 0 && parent != 0).then_some((id, parent, flags))
}

/// Lowercase hex, at most 32 digits.
fn hex_n(s: &[u8]) -> Option<u128> {
    let mut n: u128 = 0;
    for &c in s {
        let d = match c {
            b'0'..=b'9' => c - b'0',
            b'a'..=b'f' => c - b'a' + 10,
            _ => return None,
        };
        n = n << 4 | d as u128;
    }
    Some(n)
}

/// gen_ai.operation.name, one byte on a queued event.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub enum Operation {
    #[default]
    Chat,
    TextCompletion,
    Embeddings,
}

impl Operation {
    pub fn as_str(self) -> &'static str {
        match self {
            Operation::Chat => "chat",
            Operation::TextCompletion => "text_completion",
            Operation::Embeddings => "embeddings",
        }
    }
}

impl serde::Serialize for Operation {
    fn serialize<S: serde::Serializer>(&self, s: S) -> Result<S::Ok, S::Error> {
        s.serialize_str(self.as_str())
    }
}

/// The operation of a route.
pub fn operation(path: &str) -> Operation {
    if path.ends_with("/embeddings") {
        Operation::Embeddings
    } else if path.ends_with("/v1/completions") {
        Operation::TextCompletion
    } else {
        Operation::Chat
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::{Team, User};

    fn fixture(name: &str) -> Vec<u8> {
        std::fs::read(std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("..").join(name)).unwrap()
    }

    /// The cases internal/snapshot/telemetry_test.go checks.
    #[test]
    fn sampling_fixture() {
        #[derive(Deserialize)]
        struct Case {
            rule_id: String,
            request_id: String,
            sample: i64,
            bucket: u64,
            sampled: bool,
        }
        #[derive(Deserialize)]
        struct File {
            cases: Vec<Case>,
        }
        let f: File = serde_json::from_slice(&fixture("internal/snapshot/testdata/sample.json")).unwrap();
        assert!(f.cases.len() > 10);
        for c in f.cases {
            assert_eq!(bucket(&c.rule_id, &c.request_id), c.bucket, "{}:{}", c.rule_id, c.request_id);
            assert_eq!(
                sampled(&c.rule_id, &c.request_id, c.sample),
                c.sampled,
                "{}:{} at {}",
                c.rule_id,
                c.request_id,
                c.sample
            );
        }
    }

    /// The cases internal/gateway/traceparent_test.go checks.
    #[test]
    fn traceparent_fixture() {
        #[derive(Deserialize)]
        struct Case {
            header: String,
            valid: bool,
            #[serde(default)]
            trace_id: String,
            #[serde(default)]
            parent_id: String,
            #[serde(default)]
            sampled: bool,
        }
        #[derive(Deserialize)]
        struct File {
            cases: Vec<Case>,
        }
        let f: File = serde_json::from_slice(&fixture("internal/gateway/testdata/traceparent.json")).unwrap();
        assert!(f.cases.len() > 10);
        for c in f.cases {
            let got = parse_traceparent(c.header.as_bytes());
            assert_eq!(got.is_some(), c.valid, "{:?}", c.header);
            let mut h = HeaderMap::new();
            h.insert("traceparent", c.header.parse().unwrap());
            let t = Trace::read(&h);
            if c.valid {
                assert_eq!(std::str::from_utf8(&t.hex()).unwrap(), c.trace_id, "{:?}", c.header);
                assert_eq!(std::str::from_utf8(&hex64(t.parent)).unwrap(), c.parent_id, "{:?}", c.header);
                assert_eq!(t.sampled, c.sampled, "{:?}", c.header);
            } else {
                // a new trace, exported as usual
                assert!(t.id != 0 && t.parent == 0 && t.sampled, "{:?}", c.header);
            }
        }
        assert_ne!(Trace::new().id, Trace::new().id);
    }

    fn key(id: &str, user: &str, team: &str, tags: [&[&str]; 3]) -> Key {
        let v = |t: &[&str]| t.iter().map(|s| s.to_string()).collect::<Vec<_>>();
        Key {
            id: id.into(),
            user_id: user.into(),
            team_id: team.into(),
            user: (!user.is_empty()).then(|| {
                Arc::new(User {
                    id: user.into(),
                    allowed_models: None,
                    team_models: None,
                    memberships: Default::default(),
                    scope: None,
                    team_ids: Vec::new(),
                    tags: v(tags[1]),
                })
            }),
            team: (!team.is_empty())
                .then(|| Arc::new(Team { id: team.into(), allowed_models: None, scope: None, tags: v(tags[2]) })),
            allowed_models: None,
            expires_at: 0,
            scope: None,
            tags: v(tags[0]),
        }
    }

    fn policy(json: &str) -> Policy {
        Policy::from_wire(Some(serde_json::from_str(json).unwrap()))
    }

    /// The cases internal/snapshot/telemetry_test.go checks for Evaluate.
    #[test]
    fn evaluates_like_go() {
        let k = key("k1", "u1", "t1", [&["kt"], &["ut"], &["tt"]]);
        let d = |p: &Policy| p.evaluate(&k, "req_x");
        assert_eq!(d(&Policy::default()), Decision { export: true, ..Decision::default() });
        assert!(d(&policy(r#"{"mode":"everything","capture_content":true}"#)).capture_content);
        assert_eq!(d(&policy(r#"{"mode":"off","capture_content":true}"#)), Decision::default());
        let rules = |scope: &str, value: &str| {
            policy(&format!(
                r#"{{"mode":"by_rule","rules":[
                {{"id":"off","order":0,"scope_type":"{scope}","scope_value":"{value}","capture":"content","sample":100,"enabled":false}},
                {{"id":"later","order":5,"scope_type":"{scope}","scope_value":"{value}","capture":"meta","sample":100,"enabled":true}},
                {{"id":"first","order":1,"scope_type":"{scope}","scope_value":"{value}","capture":"content","sample":100,"enabled":true}}]}}"#
            ))
        };
        for (scope, value) in
            [("key", "k1"), ("user", "u1"), ("team", "t1"), ("tag", "kt"), ("tag", "ut"), ("tag", "tt")]
        {
            let got = d(&rules(scope, value));
            assert_eq!(got, Decision { export: true, capture_content: true, rule_id: Some("first".into()) }, "{scope}");
            assert_eq!(d(&rules(scope, "other")), Decision::default(), "{scope} other");
        }
        // matched, sampled out: not exported, the rule still named
        let p = policy(
            r#"{"mode":"by_rule","rules":[{"id":"r","order":0,"scope_type":"key","scope_value":"k1","capture":"content","sample":0,"enabled":true}]}"#,
        );
        assert_eq!(d(&p), Decision { rule_id: Some("r".into()), ..Decision::default() });
        // a keyless identity-token caller has no key tags or id
        let jwt = key("", "u1", "", [&[], &["ut"], &[]]);
        assert!(rules("tag", "ut").evaluate(&jwt, "r").export);
        assert!(!rules("team", "t1").evaluate(&jwt, "r").export);
    }

    /// Content leaves only when the policy captures it, the group's data rule
    /// lets it, and a destination in scope wants it (TestContentLeakTable).
    #[test]
    fn content_leak_table() {
        let k = key("k", "u", "A", [&[], &[], &[]]);
        for capture in [false, true] {
            for rule in ["inherit", "no_content", "no_export"] {
                for scope in [false, true] {
                    let content =
                        if scope { r#"[{"teams":{"mode":"all","values":null},"max_bytes":4096}]"# } else { "[]" };
                    let p =
                        policy(&format!(r#"{{"mode":"everything","capture_content":{capture},"content":{content}}}"#));
                    let v = decide(&p, DataRule::parse(rule), &k, "r");
                    let what = format!("capture={capture} {rule} scope={scope}");
                    let want = capture && rule == "inherit" && scope;
                    assert_eq!(v.capture, if want { 4096 } else { 0 }, "{what}");
                    assert_eq!(v.decision.export, rule != "no_export", "{what}");
                    assert_eq!(v.decision.capture_content, capture && rule == "inherit", "{what}");
                }
            }
        }
    }

    #[test]
    fn content_scopes_pick_the_largest_cap_for_the_team() {
        let p = policy(
            r#"{"content":[
            {"teams":{"mode":"all"},"max_bytes":100},
            {"teams":{"mode":"selected","values":["A"]},"max_bytes":500},
            {"teams":{"mode":"except","values":["A","B"]},"max_bytes":300}]}"#,
        );
        assert_eq!(p.content_wanted("A"), 500);
        assert_eq!(p.content_wanted("B"), 100);
        assert_eq!(p.content_wanted("C"), 300);
        assert_eq!(p.content_wanted(""), 300);
        assert_eq!(Policy::default().content_wanted("A"), 0);
        // only the scopes for the caller's team count
        let p = policy(
            r#"{"capture_content":true,"content":[{"teams":{"mode":"selected","values":["A"]},"max_bytes":9}]}"#,
        );
        assert_eq!(decide(&p, DataRule::Inherit, &key("k", "", "B", [&[], &[], &[]]), "r").capture, 0);
        assert_eq!(decide(&p, DataRule::Inherit, &key("k", "", "A", [&[], &[], &[]]), "r").capture, 9);
        assert_eq!(decide(&p, DataRule::Inherit, &key("k", "", "", [&[], &[], &[]]), "r").capture, 0);
    }

    #[test]
    fn operations() {
        assert_eq!(operation("/v1/embeddings").as_str(), "embeddings");
        assert_eq!(operation("/v1/completions").as_str(), "text_completion");
        assert_eq!(operation("/v1/chat/completions").as_str(), "chat");
        assert_eq!(operation("/v1/messages").as_str(), "chat");
    }
}
