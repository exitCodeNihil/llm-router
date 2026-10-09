//! Runs the Rust translation on the fixtures the Go reference wrote
//! (internal/gateway/testdata/translate; regenerate with
//! `go test ./internal/gateway -run Golden -update`).
//!
//! JSON bodies compare as values (key order and number spelling aside);
//! streams compare event by event, data payloads as JSON when they are JSON.

use std::path::Path;

use llmr_gateway::translate::{self, SseTransform, Usage};
use serde::Deserialize;
use serde_json::Value;
use serde_json::value::RawValue;

#[derive(Deserialize)]
struct Golden {
    #[serde(rename = "fn")]
    func: String,
    #[serde(default)]
    model: String,
    #[serde(default)]
    req_id: String,
    #[serde(default)]
    drop_usage_chunk: bool,
    #[serde(rename = "in")]
    input: Box<RawValue>,
    out: Option<Box<RawValue>>,
    usage: Option<GoldenUsage>,
    error: Option<String>,
}

#[derive(Deserialize, Debug, PartialEq)]
struct GoldenUsage {
    prompt: i64,
    completion: i64,
    cached: i64,
}

#[test]
fn golden_translate() {
    let dir = Path::new(env!("CARGO_MANIFEST_DIR")).join("../internal/gateway/testdata/translate");
    let mut paths: Vec<_> = std::fs::read_dir(&dir)
        .unwrap_or_else(|e| panic!("{}: {e}", dir.display()))
        .map(|e| e.unwrap().path())
        .filter(|p| p.extension().is_some_and(|x| x == "json"))
        .collect();
    paths.sort();
    assert!(!paths.is_empty(), "no fixtures in {}", dir.display());

    let failures: Vec<String> = paths
        .iter()
        .filter_map(|p| {
            let name = p.file_stem().unwrap().to_string_lossy().into_owned();
            let g: Golden = serde_json::from_slice(&std::fs::read(p).unwrap()).unwrap();
            check(&g).err().map(|e| format!("{name}: {e}"))
        })
        .collect();
    assert!(failures.is_empty(), "{} of {} fixtures failed:\n\n{}", failures.len(), paths.len(), failures.join("\n\n"));
}

fn check(g: &Golden) -> Result<(), String> {
    let input = text(&g.input);
    let input = input.as_bytes();
    let want_out = g.out.as_deref().map(text);
    let (out, usage) = match g.func.as_str() {
        "anth_req_to_oai" | "oai_req_to_anth" => {
            let res = if g.func == "anth_req_to_oai" {
                translate::anth_req_to_oai(input, &g.model)
            } else {
                translate::oai_req_to_anth(input, &g.model)
            };
            return match (res, &g.error) {
                (Err(_), Some(_)) => Ok(()),
                (Err(e), None) => Err(format!("unexpected error {e:?}")),
                (Ok(out), Some(want)) => Err(format!("want error {want:?}, got {}", String::from_utf8_lossy(&out))),
                (Ok(out), None) => json_matches(&out, want_out.as_deref().unwrap_or("")),
            };
        }
        "oai_resp_to_anth" => translate::oai_resp_to_anth(input, &g.model, &g.req_id),
        "anth_resp_to_oai" => translate::anth_resp_to_oai(input, &g.model, &g.req_id),
        "oai_json_usage" => (Vec::new(), translate::oai_json_usage(input)),
        "anth_json_usage" => (Vec::new(), translate::anth_json_usage(input)),
        "oai_passthrough" => stream(translate::OaiPassthrough::new(g.drop_usage_chunk), input),
        "anth_passthrough" => stream(translate::AnthPassthrough::new(), input),
        "oai_to_anth_stream" => stream(translate::OaiToAnthStream::new(&g.model, &g.req_id), input),
        "anth_to_oai_stream" => stream(translate::AnthToOaiStream::new(&g.model, &g.req_id), input),
        other => return Err(format!("unknown fn {other:?}")),
    };
    let have = GoldenUsage { prompt: usage.prompt, completion: usage.completion, cached: usage.cached };
    if g.usage.as_ref() != Some(&have) {
        return Err(format!("usage: want {:?}, have {have:?}", g.usage));
    }
    match (g.func.as_str(), want_out) {
        ("oai_resp_to_anth" | "anth_resp_to_oai", Some(want)) => json_matches(&out, &want),
        (_, Some(want)) => sse_matches(&String::from_utf8(out).map_err(|e| e.to_string())?, &want),
        (_, None) => Ok(()),
    }
}

/// A fixture field holds a string, or the JSON value itself.
fn text(raw: &RawValue) -> String {
    match serde_json::from_str::<String>(raw.get()) {
        Ok(s) => s,
        Err(_) => raw.get().to_owned(),
    }
}

/// Feeds `input` one event at a time, as the gateway's framing does: an
/// event's lines joined with "\n", no trailing blank line.
fn stream(mut t: impl SseTransform, input: &[u8]) -> (Vec<u8>, Usage) {
    let mut out = Vec::new();
    for ev in events(std::str::from_utf8(input).unwrap()) {
        t.event(ev.join("\n").as_bytes(), &mut out);
    }
    t.finish(&mut out);
    (out, t.usage())
}

/// Splits SSE text into events of non-blank lines. Carriage returns stay on
/// the lines (the transforms drop them); a lone "\r" still ends an event.
fn events(s: &str) -> Vec<Vec<&str>> {
    let mut evs = Vec::new();
    let mut cur = Vec::new();
    for line in s.split('\n') {
        if line.trim_end_matches('\r').is_empty() {
            if !cur.is_empty() {
                evs.push(std::mem::take(&mut cur));
            }
        } else {
            cur.push(line);
        }
    }
    if !cur.is_empty() {
        evs.push(cur);
    }
    evs
}

fn json_matches(have: &[u8], want: &str) -> Result<(), String> {
    let h: Value = serde_json::from_slice(have)
        .map_err(|e| format!("output is not JSON ({e}): {}", String::from_utf8_lossy(have)))?;
    let w: Value = serde_json::from_str(want).map_err(|e| format!("fixture out is not JSON: {e}"))?;
    if same(&h, &w) { Ok(()) } else { Err(format!("JSON differs\nhave {h}\nwant {w}")) }
}

fn sse_matches(have: &str, want: &str) -> Result<(), String> {
    let (h, w) = (events(have), events(want));
    let differs = h.len() != w.len()
        || h.iter().zip(&w).any(|(a, b)| a.len() != b.len() || a.iter().zip(b).any(|(x, y)| !same_line(x, y)));
    if differs { Err(format!("stream differs\nhave:\n{have}\nwant:\n{want}")) } else { Ok(()) }
}

fn same_line(a: &str, b: &str) -> bool {
    if let (Some(x), Some(y)) = (a.strip_prefix("data: "), b.strip_prefix("data: "))
        && let (Ok(x), Ok(y)) = (serde_json::from_str::<Value>(x), serde_json::from_str::<Value>(y))
    {
        return same(&x, &y);
    }
    a == b
}

/// Value equality, with numbers equal when they are the same number (Go
/// writes 1 where Rust writes 1.0).
fn same(a: &Value, b: &Value) -> bool {
    match (a, b) {
        (Value::Number(x), Value::Number(y)) => x == y || x.as_f64() == y.as_f64(),
        (Value::Array(x), Value::Array(y)) => x.len() == y.len() && x.iter().zip(y).all(|(a, b)| same(a, b)),
        (Value::Object(x), Value::Object(y)) => {
            x.len() == y.len() && x.iter().all(|(k, v)| y.get(k).is_some_and(|w| same(v, w)))
        }
        _ => a == b,
    }
}
