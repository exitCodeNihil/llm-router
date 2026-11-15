//! Config deltas (wire schema 3, internal/gwsync): the digest both sides keep
//! of a config, and patching the full config a gateway holds with what
//! changed. The gateway keeps the full config as it would have been sent, so
//! a patched one is loaded, saved and patched again exactly like a received
//! one.

use std::borrow::Cow;
use std::collections::{BTreeMap, HashSet};

use serde::{Deserialize, Serialize};
use serde_json::value::RawValue;

use crate::config::Wire;

/// A config's digest: every entry hashes to four little-endian u64 lanes,
/// summed lane by lane, so it does not depend on order.
#[derive(Default)]
pub struct Digest([u64; 4]);

impl Digest {
    /// Add one entry: SHA-256(section, 0, key, 0, h).
    pub fn add(&mut self, section: &str, key: &str, h: &str) {
        let mut ctx = ring::digest::Context::new(&ring::digest::SHA256);
        for part in [section.as_bytes(), &[0], key.as_bytes(), &[0], h.as_bytes()] {
            ctx.update(part);
        }
        let d = ctx.finish();
        for (lane, bytes) in self.0.iter_mut().zip(d.as_ref().chunks_exact(8)) {
            let mut le = [0u8; 8];
            le.copy_from_slice(bytes);
            *lane = lane.wrapping_add(u64::from_le_bytes(le));
        }
    }

    /// The 32 bytes, lanes little-endian, in lowercase hex.
    pub fn hex(&self) -> String {
        use std::fmt::Write;
        let mut out = String::with_capacity(64);
        for b in self.0.iter().flat_map(|lane| lane.to_le_bytes()) {
            let _ = write!(out, "{b:02x}");
        }
        out
    }
}

/// The digest of a full config, from the entry hashes the control plane sent
/// with it; the gateway never hashes an entry's content itself.
pub fn digest(w: &Wire) -> String {
    let mut d = Digest::default();
    for k in &w.keys {
        d.add("keys", &k.hash, &k.h);
    }
    for u in &w.users {
        d.add("users", &u.id, &u.h);
    }
    for t in &w.teams {
        d.add("teams", &t.id, &t.h);
    }
    for (email, user) in &w.emails {
        d.add("emails", email, user);
    }
    d.add("rest", "", &w.rest_hash);
    d.hex()
}

/// Whether a config response is a delta.
pub fn is_delta(plain: &[u8]) -> bool {
    #[derive(Deserialize)]
    struct Head {
        #[serde(default)]
        delta: bool,
    }
    serde_json::from_slice::<Head>(plain).is_ok_and(|h| h.delta)
}

/// The config a delta applies to, as the gateway holds it.
pub struct Base<'a> {
    pub epoch: &'a str,
    pub version: i64,
    pub digest: &'a str,
}

/// A full config with its entries left as they were sent.
#[derive(Serialize, Deserialize)]
struct Doc<'a> {
    schema: i64,
    version: i64,
    #[serde(default)]
    epoch: String,
    #[serde(default)]
    digest: String,
    #[serde(default)]
    rest_hash: String,
    #[serde(borrow)]
    group: &'a RawValue,
    #[serde(borrow, default)]
    keys: Vec<&'a RawValue>,
    #[serde(borrow, default)]
    users: Vec<&'a RawValue>,
    #[serde(borrow, default)]
    teams: Vec<&'a RawValue>,
    #[serde(borrow, default, skip_serializing_if = "Option::is_none")]
    providers: Option<&'a RawValue>,
    #[serde(borrow, default, skip_serializing_if = "Option::is_none")]
    deployments: Option<&'a RawValue>,
    #[serde(borrow, default, skip_serializing_if = "Option::is_none")]
    prices: Option<&'a RawValue>,
    #[serde(borrow, default, skip_serializing_if = "Option::is_none")]
    issuers: Option<&'a RawValue>,
    #[serde(default)]
    emails: BTreeMap<String, String>,
    #[serde(borrow, default, skip_serializing_if = "Option::is_none")]
    telemetry: Option<&'a RawValue>,
}

#[derive(Deserialize)]
struct Delta<'a> {
    #[serde(default)]
    epoch: String,
    base_version: i64,
    #[serde(default)]
    base_digest: String,
    version: i64,
    #[serde(default)]
    digest: String,
    #[serde(borrow, default)]
    keys: Option<Patch<'a>>,
    #[serde(borrow, default)]
    users: Option<Patch<'a>>,
    #[serde(borrow, default)]
    teams: Option<Patch<'a>>,
    #[serde(default)]
    emails: Option<EmailPatch>,
    #[serde(borrow, default)]
    rest: Option<Rest<'a>>,
}

#[derive(Deserialize)]
struct Patch<'a> {
    #[serde(borrow, default)]
    upsert: Vec<&'a RawValue>,
    #[serde(default)]
    remove: Vec<String>,
}

#[derive(Deserialize)]
struct EmailPatch {
    #[serde(default)]
    upsert: BTreeMap<String, String>,
    #[serde(default)]
    remove: Vec<String>,
}

/// Everything that is not a key, user, team or email: replaced as a whole.
#[derive(Deserialize)]
struct Rest<'a> {
    #[serde(borrow)]
    group: &'a RawValue,
    #[serde(borrow, default)]
    providers: Option<&'a RawValue>,
    #[serde(borrow, default)]
    deployments: Option<&'a RawValue>,
    #[serde(borrow, default)]
    prices: Option<&'a RawValue>,
    #[serde(borrow, default)]
    issuers: Option<&'a RawValue>,
    #[serde(borrow, default)]
    telemetry: Option<&'a RawValue>,
    #[serde(default)]
    rest_hash: String,
}

/// What names a key entry: its hash. Two entries can share a key id while a
/// rotated key's old secret still works.
fn key_name(entry: &RawValue) -> Option<Cow<'_, str>> {
    #[derive(Deserialize)]
    struct Name<'a> {
        #[serde(borrow)]
        hash: Cow<'a, str>,
    }
    serde_json::from_str::<Name>(entry.get()).ok().map(|n| n.hash)
}

fn id_name(entry: &RawValue) -> Option<Cow<'_, str>> {
    #[derive(Deserialize)]
    struct Name<'a> {
        #[serde(borrow)]
        id: Cow<'a, str>,
    }
    serde_json::from_str::<Name>(entry.get()).ok().map(|n| n.id)
}

fn patch<'a>(
    section: &str,
    list: &mut Vec<&'a RawValue>,
    patch: Option<Patch<'a>>,
    name: for<'e> fn(&'e RawValue) -> Option<Cow<'e, str>>,
) -> Result<(), String> {
    let Some(patch) = patch else { return Ok(()) };
    let mut gone: HashSet<Cow<str>> = patch.remove.iter().map(|r| Cow::Borrowed(r.as_str())).collect();
    for entry in &patch.upsert {
        gone.insert(name(entry).ok_or_else(|| format!("a {section} entry without a name"))?);
    }
    list.retain(|entry| name(entry).is_none_or(|n| !gone.contains(&n)));
    list.extend(patch.upsert.iter().copied());
    Ok(())
}

/// The full config `full` with `delta` applied, as JSON, carrying the delta's
/// version and digest. The delta must be built on exactly the config held:
/// its epoch, version and digest. Whether the result has the digest the delta
/// names is for the caller to check, from the result itself.
pub fn apply(full: &[u8], delta: &[u8], base: &Base) -> Result<Vec<u8>, String> {
    let d: Delta = serde_json::from_slice(delta).map_err(|e| format!("unreadable: {e}"))?;
    if d.epoch != base.epoch {
        return Err(format!("it is for epoch {:?}, the config held is of {:?}", d.epoch, base.epoch));
    }
    if d.base_version != base.version || d.base_digest != base.digest {
        return Err(format!(
            "it is built on version {} ({}), the config held is version {} ({})",
            d.base_version, d.base_digest, base.version, base.digest
        ));
    }
    if d.version <= base.version {
        return Err(format!("it leads to version {}, not past version {}", d.version, base.version));
    }
    let mut doc: Doc = serde_json::from_slice(full).map_err(|e| format!("the config held is unreadable: {e}"))?;
    patch("keys", &mut doc.keys, d.keys, key_name)?;
    patch("users", &mut doc.users, d.users, id_name)?;
    patch("teams", &mut doc.teams, d.teams, id_name)?;
    if let Some(emails) = d.emails {
        for email in &emails.remove {
            doc.emails.remove(email);
        }
        doc.emails.extend(emails.upsert);
    }
    if let Some(rest) = d.rest {
        doc.group = rest.group;
        doc.providers = rest.providers;
        doc.deployments = rest.deployments;
        doc.prices = rest.prices;
        doc.issuers = rest.issuers;
        doc.telemetry = rest.telemetry;
        doc.rest_hash = rest.rest_hash;
    }
    doc.version = d.version;
    doc.digest = d.digest;
    serde_json::to_vec(&doc).map_err(|e| e.to_string())
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use serde_json::{Value, json};

    /// `config` with the digest of its entries, as the control plane sends it.
    pub(crate) fn sealed(mut config: Value) -> Value {
        let mut d = Digest::default();
        let of = |v: &Value, k: &str| v[k].as_str().unwrap_or_default().to_string();
        for (section, name) in [("keys", "hash"), ("users", "id"), ("teams", "id")] {
            for e in config[section].as_array().into_iter().flatten() {
                d.add(section, &of(e, name), &of(e, "h"));
            }
        }
        for (email, user) in config["emails"].as_object().into_iter().flatten() {
            d.add("emails", email, user.as_str().unwrap());
        }
        d.add("rest", "", &of(&config, "rest_hash"));
        config["digest"] = d.hex().into();
        config
    }

    fn key(hash: &str, id: &str, h: &str) -> Value {
        json!({"hash": hash.repeat(32), "id": id, "h": h})
    }

    fn base_config() -> Value {
        sealed(json!({
            "schema": 3, "version": 5, "epoch": "e1", "rest_hash": "r1",
            "group": {"id": "g1", "name": "group"},
            "keys": [key("11", "k1", "a1"), key("22", "k2", "a2")],
            "users": [{"id": "u1", "h": "b1"}, {"id": "u2", "h": "b2"}],
            "teams": [{"id": "t1", "h": "c1"}],
            "emails": {"e-one": "u1", "e-two": "u2"},
            "providers": [{"id": "p1", "name": "p", "type": "openai_compatible", "base_url": "http://p"}],
            "deployments": [{"id": "d1", "provider_id": "p1", "model_name": "m1", "upstream_name": "up"}],
            "prices": {"x/y": {"input_per_1m": 1.0, "output_per_1m": 2.0}},
            "issuers": [{"url": "https://issuer", "audience": "a"}],
        }))
    }

    fn applied(base: &Value, delta: &Value) -> Result<Value, String> {
        let held = Base {
            epoch: base["epoch"].as_str().unwrap(),
            version: base["version"].as_i64().unwrap(),
            digest: base["digest"].as_str().unwrap(),
        };
        let out = apply(&serde_json::to_vec(base).unwrap(), &serde_json::to_vec(delta).unwrap(), &held)?;
        Ok(serde_json::from_slice(&out).unwrap())
    }

    fn delta_on(base: &Value, version: i64, changes: Value) -> Value {
        let mut d = json!({"schema": 3, "delta": true, "epoch": base["epoch"], "base_version": base["version"],
            "base_digest": base["digest"], "version": version, "digest": "set-by-the-test"});
        d.as_object_mut().unwrap().extend(changes.as_object().unwrap().clone());
        d
    }

    // The byte layout, pinned against values worked out independently
    // (Python's hashlib): the Go side must produce the same.
    #[test]
    fn the_digest_is_the_lane_sum_of_the_entry_hashes() {
        let mut d = Digest::default();
        d.add("rest", "", "ff");
        assert_eq!(d.hex(), REST_FF);
        d.add("keys", "aa", "01");
        assert_eq!(d.hex(), ONE_KEY);
        let w: Wire = serde_json::from_value(json!({
            "schema": 3, "version": 1, "group": {"id": "g", "name": "g"}, "rest_hash": "ff",
            "keys": [{"hash": "aa", "id": "k", "h": "01"}],
        }))
        .unwrap();
        assert_eq!(digest(&w), ONE_KEY);

        // any order, the same digest
        let mut forward = Digest::default();
        let mut backward = Digest::default();
        let entries = [("keys", "a", "1"), ("users", "b", "2"), ("teams", "c", "3"), ("emails", "d", "u")];
        entries.iter().for_each(|(s, k, h)| forward.add(s, k, h));
        entries.iter().rev().for_each(|(s, k, h)| backward.add(s, k, h));
        assert_eq!(forward.hex(), backward.hex());
        // the separators keep (section, key, h) apart
        let (mut a, mut b) = (Digest::default(), Digest::default());
        a.add("keys", "ab", "c");
        b.add("keys", "a", "bc");
        assert_ne!(a.hex(), b.hex());
    }

    /// SHA-256("rest" 00 00 "ff"), whose lanes are its own bytes.
    const REST_FF: &str = "289f3d468706e449e8e9016ec133ac38c4a8052e57dea4d2662da4ecce49a17e";
    /// One key {hash "aa", h "01"}, no users, teams or emails, rest_hash "ff".
    const ONE_KEY: &str = "22e7c251c12858fd1086bc603b27ede89b191b03996c3bac3202c3cdf91c4430";

    #[test]
    fn a_delta_upserts_and_removes_in_each_section() {
        let base = base_config();
        let delta = delta_on(
            &base,
            6,
            json!({
                "keys": {"upsert": [key("22", "k2", "a2x"), key("33", "k3", "a3")], "remove": ["11".repeat(32)]},
                "users": {"upsert": [{"id": "u3", "h": "b3"}], "remove": ["u1"]},
                "teams": {"upsert": [{"id": "t1", "h": "c1x", "rpm_limit": 5}], "remove": ["never-there"]},
                "emails": {"upsert": {"e-three": "u3", "e-two": "u3"}, "remove": ["e-one"]},
            }),
        );
        let out = applied(&base, &delta).unwrap();
        assert_eq!(out["version"], 6);
        assert_eq!(out["digest"], "set-by-the-test");
        assert_eq!(out["keys"], json!([key("22", "k2", "a2x"), key("33", "k3", "a3")]));
        assert_eq!(out["users"], json!([{"id": "u2", "h": "b2"}, {"id": "u3", "h": "b3"}]));
        assert_eq!(out["teams"], json!([{"id": "t1", "h": "c1x", "rpm_limit": 5}]));
        assert_eq!(out["emails"], json!({"e-three": "u3", "e-two": "u3"}));
        for untouched in ["group", "providers", "deployments", "prices", "issuers", "rest_hash", "epoch"] {
            assert_eq!(out[untouched], base[untouched], "{untouched}");
        }
        // an absent section is unchanged
        let out = applied(&base, &delta_on(&base, 6, json!({"users": {"remove": ["u2"]}}))).unwrap();
        assert_eq!(out["users"], json!([{"id": "u1", "h": "b1"}]));
        assert_eq!((&out["keys"], &out["teams"], &out["emails"]), (&base["keys"], &base["teams"], &base["emails"]));
    }

    #[test]
    fn rest_replaces_everything_else_as_a_whole() {
        let base = base_config();
        let rest = json!({"group": {"id": "g1", "name": "renamed"}, "rest_hash": "r2", "telemetry": null,
            "deployments": [{"id": "d2", "provider_id": "p1", "model_name": "m2", "upstream_name": "up"}],
            "providers": [{"id": "p1", "name": "p", "type": "openai_compatible", "base_url": "http://q"}]});
        let out = applied(&base, &delta_on(&base, 9, json!({"rest": rest}))).unwrap();
        assert_eq!(out["group"]["name"], "renamed");
        assert_eq!(out["rest_hash"], "r2");
        assert_eq!(out["deployments"][0]["id"], "d2");
        assert_eq!(out["providers"][0]["base_url"], "http://q");
        // absent from rest: none, as in a full config
        assert!(out.get("issuers").is_none() && out.get("prices").is_none() && out.get("telemetry").is_none());
        assert_eq!((&out["keys"], &out["users"]), (&base["keys"], &base["users"]));
        // and the result is a config the gateway reads
        let w: Wire = serde_json::from_value(out).unwrap();
        assert!(w.issuers.is_empty() && w.prices.is_empty() && w.keys.len() == 2);
    }

    #[test]
    fn a_delta_built_on_another_config_is_refused() {
        let base = base_config();
        let good = delta_on(&base, 6, json!({}));
        assert!(applied(&base, &good).is_ok());
        for (field, value) in [
            ("base_version", json!(4)),
            ("base_digest", json!("00".repeat(32))),
            ("epoch", json!("e2")),
            ("version", json!(5)),
            ("version", json!(3)),
        ] {
            let mut d = good.clone();
            d[field] = value;
            assert!(applied(&base, &d).is_err(), "{field}");
        }
        let nameless = delta_on(&base, 6, json!({"keys": {"upsert": [{"id": "k9"}]}}));
        assert!(applied(&base, &nameless).unwrap_err().contains("without a name"));
        let held = Base { epoch: "e1", version: 5, digest: base["digest"].as_str().unwrap() };
        assert!(apply(&serde_json::to_vec(&base).unwrap(), b"{\"delta\":true,", &held).is_err());
    }

    // A rotated key is two entries with one id until the old secret's grace
    // period ends: a delta that removes the old hash must leave the new one.
    #[test]
    fn a_rotated_keys_two_entries_are_told_apart_by_hash() {
        let mut base = base_config();
        base["keys"] = json!([key("11", "k1", "old"), key("44", "k1", "new"), key("22", "k2", "a2")]);
        let base = sealed(base);
        let out = applied(&base, &delta_on(&base, 6, json!({"keys": {"remove": ["11".repeat(32)]}}))).unwrap();
        assert_eq!(out["keys"], json!([key("44", "k1", "new"), key("22", "k2", "a2")]));
        // and an upsert of one hash leaves the other entry of that id alone
        let delta = delta_on(&base, 6, json!({"keys": {"upsert": [key("44", "k1", "newer")]}}));
        let out = applied(&base, &delta).unwrap();
        assert_eq!(out["keys"], json!([key("11", "k1", "old"), key("22", "k2", "a2"), key("44", "k1", "newer")]));
    }

    #[test]
    fn tells_a_delta_from_a_full_config() {
        assert!(is_delta(br#"{"schema":3,"delta":true,"version":2}"#));
        assert!(!is_delta(br#"{"schema":3,"delta":false,"version":2}"#));
        assert!(!is_delta(&serde_json::to_vec(&base_config()).unwrap()));
        assert!(!is_delta(b"not json"));
    }
}
