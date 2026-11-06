//! Usage events and their cost (internal/usage, internal/pricing). Events queue
//! here and the sync loop uploads them; the control plane writes them to
//! Postgres and decides what telemetry to export.

use std::collections::VecDeque;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::Instant;

use serde::{Deserialize, Serialize, Serializer};
use serde_json::value::RawValue;

use crate::config::{AuthMode, Config, Deployment, Key, Price};
use crate::telemetry::{self, Trace, Verdict};
use crate::translate::{Tag, Usage};
use crate::{limits, metrics};

/// One request, as gwsync.SyncEvent expects it.
#[derive(Debug, Default, Serialize)]
pub struct Event {
    pub ts_ms: i64,
    pub request_id: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub api_key_id: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub user_id: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub team_id: String,
    pub model_name: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub deployment_id: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub provider_id: String,
    pub prompt_tokens: i64,
    pub completion_tokens: i64,
    pub cached_tokens: i64,
    #[serde(skip_serializing_if = "is_zero", rename = "cache_write_5m_tokens")]
    pub cache_write_5m: i64,
    #[serde(skip_serializing_if = "is_zero", rename = "cache_write_1h_tokens")]
    pub cache_write_1h: i64,
    pub cost_usd: f64,
    pub unpriced: bool,
    pub latency_ms: i64,
    pub status_code: u16,
    pub stream: bool,
    #[serde(skip_serializing_if = "is_zero_f")]
    pub notional_cost_usd: f64,
    #[serde(skip_serializing_if = "is_zero")]
    pub attempts: i64,
    #[serde(skip_serializing_if = "str::is_empty")]
    pub error_code: &'static str,
    #[serde(skip_serializing_if = "is_zero")]
    pub ttft_ms: i64,

    /// The caller's trace, or a new one (32 hex); `parent_span_id` is the
    /// caller's span (16 hex), 0 for none.
    #[serde(serialize_with = "hex_trace")]
    pub trace_id: u128,
    #[serde(skip_serializing_if = "is_zero_u64", serialize_with = "hex_span")]
    pub parent_span_id: u64,
    /// gen_ai.operation.name.
    pub operation: &'static str,
    #[serde(skip_serializing_if = "Tag::is_empty")]
    pub inference_geo: Tag,
    #[serde(skip_serializing_if = "Tag::is_empty")]
    pub speed: Tag,
    /// The pricing cost_usd was computed with (usage.CostBasisCurrent).
    #[serde(skip_serializing_if = "is_zero_u8")]
    pub cost_basis: u8,
    /// The gateway's telemetry verdict. The control plane decides again and
    /// can only take away from it.
    #[serde(skip_serializing_if = "is_false")]
    pub export: bool,
    #[serde(skip_serializing_if = "is_false")]
    pub capture_content: bool,
    #[serde(skip_serializing_if = "Option::is_none", serialize_with = "arc_str")]
    pub rule_id: Option<Arc<str>>,
    /// Captured content: only when the verdict captures it.
    #[serde(flatten, skip_serializing_if = "Option::is_none")]
    pub content: Option<Box<Content>>,
}

/// A request's captured content, each part within the capture cap: input
/// and system are the request's own JSON, not a re-encoding.
#[derive(Debug, Default, Serialize)]
pub struct Content {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub input: Option<Box<RawValue>>,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub output: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub system: Option<Box<RawValue>>,
    /// Something was cut to the cap, or left out over the queue's budget.
    #[serde(skip_serializing_if = "is_false")]
    pub content_truncated: bool,
}

impl Content {
    /// The bytes it holds, counted against the queue's content budget.
    pub fn bytes(&self) -> usize {
        self.input.as_ref().map_or(0, |r| r.get().len())
            + self.system.as_ref().map_or(0, |r| r.get().len())
            + self.output.len()
    }

    /// About how many bytes it takes as JSON: the output's escapes counted.
    fn encoded_size(&self) -> usize {
        let escaped = self
            .output
            .bytes()
            .map(|b| match b {
                b'"' | b'\\' | b'\n' | b'\r' | b'\t' | 0x08 | 0x0c => 2,
                0..0x20 => 6,
                _ => 1,
            })
            .sum::<usize>();
        64 + escaped
            + self.input.as_ref().map_or(0, |r| r.get().len())
            + self.system.as_ref().map_or(0, |r| r.get().len())
    }
}

fn is_zero(n: &i64) -> bool {
    *n == 0
}
fn is_zero_f(n: &f64) -> bool {
    *n == 0.0
}
fn is_zero_u64(n: &u64) -> bool {
    *n == 0
}
fn is_zero_u8(n: &u8) -> bool {
    *n == 0
}
fn is_false(b: &bool) -> bool {
    !*b
}
fn hex_trace<S: Serializer>(n: &u128, s: S) -> Result<S::Ok, S::Error> {
    s.serialize_str(std::str::from_utf8(&telemetry::hex128(*n)).unwrap_or_default())
}
fn hex_span<S: Serializer>(n: &u64, s: S) -> Result<S::Ok, S::Error> {
    s.serialize_str(std::str::from_utf8(&telemetry::hex64(*n)).unwrap_or_default())
}
fn arc_str<S: Serializer>(v: &Option<Arc<str>>, s: S) -> Result<S::Ok, S::Error> {
    s.serialize_str(v.as_deref().unwrap_or_default())
}

/// Events waiting for the next sync. When the control plane is unreachable for
/// long enough to fill it, new events are dropped and counted.
const MAX_QUEUED: usize = 200_000;

/// The most captured content the queue holds. Past it an event keeps its
/// usage and loses its content, marked truncated, and is counted.
const CONTENT_BUDGET: usize = 64 << 20;

#[derive(Default)]
struct Queue {
    events: VecDeque<Event>,
    /// Content bytes the queued events hold.
    content: usize,
}

impl Queue {
    /// Count an event's content against the budget, or strip it.
    fn admit(&mut self, e: &mut Event) {
        self.admit_within(e, CONTENT_BUDGET);
    }

    fn admit_within(&mut self, e: &mut Event, budget: usize) {
        let Some(c) = e.content.as_mut() else { return };
        let n = c.bytes();
        if self.content + n > budget {
            **c = Content { content_truncated: true, ..Content::default() };
            CONTENT_STRIPPED.fetch_add(1, Ordering::Relaxed);
        } else {
            self.content += n;
        }
    }
}

fn queue() -> &'static Mutex<Queue> {
    static Q: OnceLock<Mutex<Queue>> = OnceLock::new();
    Q.get_or_init(Default::default)
}

pub static DROPPED: AtomicU64 = AtomicU64::new(0);
/// Events that lost their content because the queue's content budget was spent.
pub static CONTENT_STRIPPED: AtomicU64 = AtomicU64::new(0);

fn push(mut e: Event) {
    let mut q = queue().lock().unwrap();
    if q.events.len() >= MAX_QUEUED {
        drop(q);
        DROPPED.fetch_add(1, Ordering::Relaxed);
        return;
    }
    q.admit(&mut e);
    q.events.push_back(e);
}

/// Queued events, oldest first: at most `max_events`, and no more than about
/// `max_bytes` once encoded (always at least one).
pub fn take(max_events: usize, max_bytes: usize) -> Vec<Event> {
    let mut q = queue().lock().unwrap();
    let n = batch_len(q.events.iter().map(Event::encoded_size), max_events, max_bytes);
    let batch: Vec<Event> = q.events.drain(..n).collect();
    let freed: usize = batch.iter().filter_map(|e| e.content.as_ref()).map(|c| c.bytes()).sum();
    q.content -= freed;
    shrink(&mut q.events);
    batch
}

/// Content bytes the queue holds.
pub fn queued_content() -> usize {
    queue().lock().unwrap().content
}

/// The queue keeps the capacity a burst grew it to. Give most of it back once
/// the queue is mostly empty again, keeping enough for ordinary traffic.
const KEEP_CAPACITY: usize = 16_384;

fn shrink(q: &mut VecDeque<Event>) {
    if q.capacity() > KEEP_CAPACITY && q.len() < q.capacity() / 4 {
        q.shrink_to((q.len() * 2).max(KEEP_CAPACITY));
    }
}

/// How many leading items of the given sizes fit in one batch.
fn batch_len(sizes: impl Iterator<Item = usize>, max_events: usize, max_bytes: usize) -> usize {
    let mut n = 0;
    let mut bytes = 0;
    for size in sizes.take(max_events) {
        bytes += size;
        if n > 0 && bytes > max_bytes {
            break;
        }
        n += 1;
    }
    n
}

impl Event {
    /// About how many bytes the event takes as JSON: the fixed fields plus
    /// its strings and content. An estimate for sizing batches, not an exact
    /// count, but never under for content, which is most of a large event.
    pub fn encoded_size(&self) -> usize {
        self.content.as_ref().map_or(0, |c| c.encoded_size())
            + self.rule_id.as_ref().map_or(0, |r| r.len())
            + 900
            + self.request_id.len()
            + self.api_key_id.len()
            + self.user_id.len()
            + self.team_id.len()
            + self.model_name.len()
            + self.deployment_id.len()
            + self.provider_id.len()
            + self.error_code.len()
    }
}

/// Put events back at the front of the queue, in order, ahead of newer ones.
/// Events that no longer fit are dropped and counted.
pub fn requeue(events: Vec<Event>) {
    let mut q = queue().lock().unwrap();
    let room = MAX_QUEUED.saturating_sub(q.events.len());
    let dropped = events.len().saturating_sub(room);
    for mut e in events.into_iter().take(room).rev() {
        q.admit(&mut e);
        q.events.push_front(e);
    }
    drop(q);
    if dropped > 0 {
        DROPPED.fetch_add(dropped as u64, Ordering::Relaxed);
    }
}

pub fn queued() -> usize {
    queue().lock().unwrap().events.len()
}

/// How many events the queue has room for without growing: memory it holds
/// even when empty.
pub fn capacity() -> usize {
    queue().lock().unwrap().events.capacity()
}

// Anthropic's pricing rules on top of a model's rates (internal/pricing).
const CACHE_WRITE_5M: f64 = 1.25;
const CACHE_WRITE_1H: f64 = 2.0;
const US_INFERENCE: f64 = 1.1;
const FAST_MODE: f64 = 2.0;

/// Price a request (pricing.price): the deployment's own price, else the
/// catalog's, else unpriced.
fn price(cfg: &Config, d: &Deployment, u: Usage) -> Option<f64> {
    let p = match (d.input_per_1m, d.output_per_1m) {
        (Some(input_per_1m), Some(output_per_1m)) => Price {
            input_per_1m,
            output_per_1m,
            cached_input_per_1m: d.cached_input_per_1m,
            long_prompt_tokens: 0,
            long_prompt_multiplier: 1.0,
        },
        _ if !d.catalog_model_id.is_empty() => *cfg.prices.get(&d.catalog_model_id)?,
        _ => return None,
    };
    Some(cost(p, u))
}

/// Go: pricing.priceCost. Shared fixtures in internal/pricing/testdata/cost.json.
fn cost(p: Price, u: Usage) -> f64 {
    // The parts can't exceed the whole, whatever an upstream reports.
    let part = |n: i64, left: i64| n.min(left).max(0);
    let cached = part(u.cached, u.prompt);
    let write_5m = part(u.write_5m, u.prompt - cached);
    let write_1h = part(u.write_1h, u.prompt - cached - write_5m);
    let fresh = u.prompt - cached - write_5m - write_1h;

    let cached_rate = p.cached_input_per_1m.unwrap_or(p.input_per_1m); // no cached rate: reads bill as input
    let mut usd = fresh as f64 * p.input_per_1m
        + cached as f64 * cached_rate
        + write_5m as f64 * p.input_per_1m * CACHE_WRITE_5M
        + write_1h as f64 * p.input_per_1m * CACHE_WRITE_1H
        + u.completion as f64 * p.output_per_1m;
    if p.long_prompt_tokens > 0 && u.prompt > p.long_prompt_tokens {
        usd *= p.long_prompt_multiplier;
    }
    if u.us_inference {
        usd *= US_INFERENCE;
    }
    if u.fast {
        usd *= FAST_MODE;
    }
    usd / 1e6
}

/// Everything known about a request before its outcome, to emit its event once.
pub struct Emit {
    pub cfg: Arc<Config>,
    pub key: Arc<Key>,
    pub model: String,
    pub request_id: String,
    pub stream: bool,
    pub start: Instant,
    pub ts_ms: i64,
    pub deployment: Option<Arc<Deployment>>,
    pub attempts: i64,
    pub trace: Trace,
    pub operation: &'static str,
    pub verdict: Verdict,
    /// The request's content, once captured; the response text joins it at send.
    pub content: Option<Box<Content>>,
    pub output: String,
}

/// The fields of a request body whose content is captured, the last of each
/// as Go's encoding/json keeps it.
#[derive(Default)]
struct Fields<'a> {
    messages: Option<&'a RawValue>,
    system: Option<&'a RawValue>,
}

impl<'de: 'a, 'a> Deserialize<'de> for Fields<'a> {
    fn deserialize<D: serde::Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        struct V;
        impl<'de> serde::de::Visitor<'de> for V {
            type Value = Fields<'de>;
            fn expecting(&self, f: &mut std::fmt::Formatter) -> std::fmt::Result {
                f.write_str("an object")
            }
            fn visit_map<M: serde::de::MapAccess<'de>>(self, mut m: M) -> Result<Fields<'de>, M::Error> {
                let mut f = Fields::default();
                while let Some(k) = m.next_key::<std::borrow::Cow<str>>()? {
                    match k.as_ref() {
                        "messages" => f.messages = Some(m.next_value()?),
                        "system" => f.system = Some(m.next_value()?),
                        _ => {
                            m.next_value::<serde::de::IgnoredAny>()?;
                        }
                    }
                }
                Ok(f)
            }
        }
        d.deserialize_map(V)
    }
}

/// The request's input and system within `limit` bytes each (Go: capContent):
/// the input keeps its newest messages that fit, or the newest alone with its
/// text cut when not even that fits, and a larger system prompt is cut. Copied
/// out of the request, so a queued event never holds a large body.
/// content_truncated says something was left out.
fn capture_request(body: &[u8], with_system: bool, limit: usize) -> Content {
    let Ok(f) = serde_json::from_slice::<Fields>(body) else { return Content::default() };
    let mut c = Content::default();
    if let Some(system) = f.system.filter(|_| with_system) {
        if system.get().len() > limit {
            c.content_truncated = true;
            c.system = cut_json(system.get(), limit);
        } else {
            c.system = Some(system.to_owned());
        }
    }
    if let Some(input) = f.messages {
        if input.get().len() <= limit {
            c.input = Some(input.to_owned());
        } else {
            let msgs: Vec<&RawValue> = serde_json::from_str(input.get()).unwrap_or_default();
            let (mut n, mut i) = (2, msgs.len());
            while i > 0 && n + msgs[i - 1].get().len() < limit {
                n += msgs[i - 1].get().len() + 1;
                i -= 1;
            }
            c.content_truncated |= i > 0 || msgs.is_empty();
            if i < msgs.len() {
                let mut s = String::with_capacity(n);
                s.push('[');
                for (j, m) in msgs[i..].iter().enumerate() {
                    if j > 0 {
                        s.push(',');
                    }
                    s.push_str(m.get());
                }
                s.push(']');
                c.input = RawValue::from_string(s).ok();
            } else if let Some(newest) = msgs.last() {
                // Not even the newest message fits: keep it, its text cut.
                c.input = cut_json(newest.get(), limit.saturating_sub(2))
                    .and_then(|m| RawValue::from_string(format!("[{}]", m.get())).ok());
            }
        }
    }
    c
}

/// A JSON value cut to fit in `limit` bytes by shortening its strings, the
/// longest first, leaving "role" and "type" alone; None when even empty
/// strings don't fit. Only an oversized part takes this path: it is re-encoded.
fn cut_json(raw: &str, limit: usize) -> Option<Box<RawValue>> {
    fn strings<'a>(v: &'a mut serde_json::Value, out: &mut Vec<&'a mut String>) {
        match v {
            serde_json::Value::String(s) => out.push(s),
            serde_json::Value::Array(a) => a.iter_mut().for_each(|x| strings(x, out)),
            serde_json::Value::Object(m) => {
                for (k, x) in m.iter_mut() {
                    if k != "role" && k != "type" {
                        strings(x, out);
                    }
                }
            }
            _ => {}
        }
    }
    let mut v: serde_json::Value = serde_json::from_str(raw).ok()?;
    let mut excess = serde_json::to_string(&v).ok()?.len().saturating_sub(limit);
    let mut all = Vec::new();
    strings(&mut v, &mut all);
    all.sort_by_key(|s| std::cmp::Reverse(s.len()));
    // Every byte of text takes at least a byte encoded, so cutting n bytes
    // of text saves at least n.
    for s in all {
        if excess == 0 {
            break;
        }
        let before = s.len();
        cut_utf8(s, before.saturating_sub(excess));
        excess = excess.saturating_sub(before - s.len());
    }
    let out = serde_json::to_string(&v).ok()?;
    if out.len() > limit {
        return None;
    }
    RawValue::from_string(out).ok()
}

/// `s` cut to at most `n` bytes, at a character boundary.
fn cut_utf8(s: &mut String, n: usize) -> bool {
    if s.len() <= n {
        return false;
    }
    let mut n = n;
    while !s.is_char_boundary(n) {
        n -= 1;
    }
    s.truncate(n);
    true
}

impl Emit {
    /// Capture the request's content when its verdict asks for it: messages,
    /// and on the Anthropic surface its top-level system.
    pub fn capture_request(&mut self, body: &[u8], with_system: bool) {
        if self.verdict.capture > 0 {
            self.content = Some(Box::new(capture_request(body, with_system, self.verdict.capture)));
        }
    }

    /// The response text, for a verdict that captures content.
    pub fn set_output(&mut self, text: String) {
        if self.verdict.capture > 0 {
            self.output = text;
        }
    }

    /// The captured content, the output cut to the cap; None when there is none.
    fn take_content(&mut self) -> Option<Box<Content>> {
        let limit = self.verdict.capture;
        if limit == 0 {
            return None;
        }
        let mut c = self.content.take().unwrap_or_default();
        c.output = std::mem::take(&mut self.output);
        c.content_truncated |= cut_utf8(&mut c.output, limit);
        if c.input.is_none() && c.system.is_none() && c.output.is_empty() && !c.content_truncated {
            return None;
        }
        Some(c)
    }

    /// Queue the request's event and count its tokens and spend against its limits.
    pub fn send(mut self, status: u16, error_code: &'static str, u: Usage, ttft_ms: i64) {
        let content = self.take_content();
        let mut e = Event {
            ts_ms: self.ts_ms,
            request_id: self.request_id,
            api_key_id: self.key.id.clone(),
            user_id: self.key.user.as_ref().map(|u| u.id.clone()).unwrap_or_default(),
            team_id: self.key.team.as_ref().map(|t| t.id.clone()).unwrap_or_default(),
            model_name: self.model,
            deployment_id: String::new(),
            provider_id: String::new(),
            prompt_tokens: u.prompt,
            completion_tokens: u.completion,
            cached_tokens: u.cached,
            cache_write_5m: u.write_5m,
            cache_write_1h: u.write_1h,
            cost_usd: 0.0,
            unpriced: false,
            latency_ms: self.start.elapsed().as_millis() as i64,
            status_code: status,
            stream: self.stream,
            notional_cost_usd: 0.0,
            attempts: self.attempts,
            error_code,
            ttft_ms,
            trace_id: self.trace.id,
            parent_span_id: self.trace.parent,
            operation: self.operation,
            inference_geo: u.geo,
            speed: u.speed,
            cost_basis: 0,
            export: self.verdict.decision.export,
            capture_content: self.verdict.decision.capture_content,
            rule_id: self.verdict.decision.rule_id,
            content,
        };
        if let Some(d) = &self.deployment {
            e.deployment_id = d.id.clone();
            e.provider_id = d.provider.id.clone();
            e.cost_basis = 1;
            let listed = price(&self.cfg, d, u);
            if d.provider.auth_mode == AuthMode::OAuthPassthrough {
                // Billed to the caller's own plan: no real money, but a list value.
                // A wildcard has no price of its own: use the model asked for.
                e.unpriced = true;
                let by_model = || self.cfg.prices.get(&format!("anthropic/{}", e.model_name)).map(|p| cost(*p, u));
                e.notional_cost_usd = listed.or_else(by_model).unwrap_or(0.0);
            } else {
                e.unpriced = listed.is_none();
                e.cost_usd = listed.unwrap_or(0.0);
            }
        }
        let (cost, tokens) = (e.cost_usd, u.prompt + u.completion);
        metrics::latency(e.latency_ms);
        push(e);
        limits::record(&self.key, tokens, cost);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::Wire;

    fn event(id: &str) -> Event {
        Event { request_id: id.into(), model_name: "requeue-test".into(), status_code: 200, ..Event::default() }
    }

    #[test]
    fn requeued_events_go_out_first_in_order() {
        let _q = QUEUE.lock().unwrap();
        push(event("c"));
        requeue(vec![event("a"), event("b")]);
        let ids: Vec<_> = take(usize::MAX, usize::MAX)
            .into_iter()
            .filter(|e| e.model_name == "requeue-test")
            .map(|e| e.request_id)
            .collect();
        assert_eq!(ids, ["a", "b", "c"]);
    }

    fn content(input: &str, system: Option<&str>, output: &str) -> Option<Box<Content>> {
        Some(Box::new(Content {
            input: Some(RawValue::from_string(input.into()).unwrap()),
            system: system.map(|s| RawValue::from_string(s.into()).unwrap()),
            output: output.into(),
            content_truncated: false,
        }))
    }

    /// Over the budget an event keeps its usage and loses its content, marked
    /// truncated; what leaves the queue frees budget for what comes next.
    #[test]
    fn content_over_the_budget_is_stripped_not_the_event() {
        let mut q = Queue::default();
        let stripped = CONTENT_STRIPPED.load(Ordering::Relaxed);
        let big = "x".repeat(400);
        let mut events: Vec<Event> = (0..3)
            .map(|i| Event { request_id: format!("r{i}"), content: content("[]", None, &big), ..event("") })
            .collect();
        for e in &mut events {
            q.admit_within(e, 1000);
        }
        assert_eq!(q.content, 2 * 402);
        assert_eq!(events[1].content.as_ref().unwrap().output, big);
        let c = events[2].content.as_ref().unwrap();
        assert!(c.content_truncated && c.output.is_empty() && c.input.is_none());
        assert!(CONTENT_STRIPPED.load(Ordering::Relaxed) > stripped);
        assert_eq!(events[2].request_id, "r2", "the event itself stays");
        let v: serde_json::Value = serde_json::from_slice(&serde_json::to_vec(&events[2]).unwrap()).unwrap();
        assert_eq!(v["content_truncated"], true);
        assert!(v.get("output").is_none() && v.get("input").is_none());
    }

    /// Tests that use the process-wide queue, one at a time.
    static QUEUE: Mutex<()> = Mutex::new(());

    #[test]
    fn the_queue_counts_its_content() {
        let _q = QUEUE.lock().unwrap();
        let before = queued_content();
        push(Event { model_name: "budget-test".into(), content: content("[1]", Some(r#""s""#), "out"), ..event("b") });
        // other tests share the queue: only ours is known to be there
        assert!(queued_content() >= before + 3 + 3 + 3);
        let mut left = 0;
        for e in take(usize::MAX, usize::MAX) {
            if e.model_name != "budget-test" {
                left += e.content.as_ref().map_or(0, |c| c.bytes());
                requeue(vec![e]);
            }
        }
        assert_eq!(queued_content(), left);
    }

    /// Batches stay under the control plane's 32 MiB limit only if no event
    /// is larger than its estimate, content included.
    #[test]
    fn size_estimates_cover_content() {
        let nasty = "\"\\\u{1}\n<é>".repeat(5000);
        let input =
            serde_json::to_string(&vec![serde_json::json!({"role": "user", "content": nasty.clone()}); 3]).unwrap();
        for e in [
            event("plain"),
            Event {
                request_id: "req_0123456789abcdef".into(),
                api_key_id: "4b7c3a1e-0000-4000-8000-000000000000".into(),
                user_id: "4b7c3a1e-0000-4000-8000-000000000001".into(),
                team_id: "4b7c3a1e-0000-4000-8000-000000000002".into(),
                deployment_id: "4b7c3a1e-0000-4000-8000-000000000003".into(),
                provider_id: "4b7c3a1e-0000-4000-8000-000000000004".into(),
                model_name: "claude-sonnet-4-5-20250929".into(),
                prompt_tokens: i64::MAX,
                completion_tokens: i64::MIN,
                cached_tokens: i64::MAX,
                cache_write_5m: i64::MAX,
                cache_write_1h: i64::MAX,
                cost_usd: f64::MAX,
                notional_cost_usd: f64::MIN_POSITIVE,
                latency_ms: i64::MAX,
                attempts: i64::MAX,
                ttft_ms: i64::MAX,
                error_code: "upstream_rate_limited",
                trace_id: u128::MAX,
                parent_span_id: u64::MAX,
                operation: "text_completion",
                inference_geo: Tag::new(&"g".repeat(40)),
                speed: Tag::new("standard"),
                cost_basis: 1,
                export: true,
                capture_content: true,
                rule_id: Some("4b7c3a1e-0000-4000-8000-000000000005".into()),
                content: content(&input, Some(&serde_json::to_string(&nasty).unwrap()), &nasty),
                ..event("")
            },
        ] {
            let actual = serde_json::to_vec(&e).unwrap().len();
            assert!(e.encoded_size() >= actual, "estimate {} < actual {actual}", e.encoded_size());
        }
    }

    #[test]
    fn serializes_trace_and_content() {
        let e = Event {
            trace_id: 0x4bf92f3577b34da6a3ce929d0e0e4736,
            operation: "chat",
            inference_geo: Tag::new("us"),
            rule_id: Some("r1".into()),
            export: true,
            content: content(r#"[{"role":"user","content":"hi"}]"#, None, "answer"),
            ..event("s")
        };
        let v: serde_json::Value = serde_json::from_slice(&serde_json::to_vec(&e).unwrap()).unwrap();
        assert_eq!(v["trace_id"], "4bf92f3577b34da6a3ce929d0e0e4736");
        assert!(v.get("parent_span_id").is_none() && v.get("speed").is_none() && v.get("system").is_none());
        assert_eq!(v["operation"], "chat");
        assert_eq!(v["inference_geo"], "us");
        assert_eq!((v["export"].clone(), v["rule_id"].clone()), (true.into(), "r1".into()));
        assert!(v.get("capture_content").is_none() && v.get("cost_basis").is_none());
        assert_eq!(v["input"][0]["content"], "hi");
        assert_eq!(v["output"], "answer");
        let e = Event { parent_span_id: 0x00f067aa0ba902b7, ..event("p") };
        let v: serde_json::Value = serde_json::from_slice(&serde_json::to_vec(&e).unwrap()).unwrap();
        assert_eq!(v["parent_span_id"], "00f067aa0ba902b7");
        assert!(v.get("input").is_none() && v.get("content_truncated").is_none());
    }

    /// Go: capContent. Each part within the cap: the input keeps its newest
    /// messages that fit, or the newest cut; a larger system prompt is cut.
    #[test]
    fn captures_requests_within_the_cap() {
        let body = r#"{"model":"m","system":"you are terse","messages":[{"role":"user","content":"first"}, {"role":"assistant","content":"second"},{"role":"user","content":"third"}]}"#;
        let all = capture_request(body.as_bytes(), true, 1 << 10);
        assert_eq!(all.system.as_ref().unwrap().get(), r#""you are terse""#);
        assert!(all.input.as_ref().unwrap().get().starts_with(r#"[{"role":"user","content":"first"}, {"#));
        assert!(!all.content_truncated);
        assert!(capture_request(body.as_bytes(), false, 1 << 10).system.is_none(), "OpenAI surface: no system");

        let last = r#"{"role":"user","content":"third"}"#;
        let cut = capture_request(body.as_bytes(), true, last.len() + 3);
        assert_eq!(cut.input.as_ref().unwrap().get(), format!("[{last}]"));
        assert!(cut.content_truncated);
        // Not even the newest message fits: it stays, its text cut to fit.
        let cut = capture_request(body.as_bytes(), true, 32);
        assert_eq!(cut.input.as_ref().unwrap().get(), r#"[{"content":"th","role":"user"}]"#);
        assert_eq!(cut.system.as_ref().unwrap().get(), r#""you are terse""#);
        assert!(cut.content_truncated);
        let cut = capture_request(body.as_bytes(), true, 10);
        assert_eq!(cut.system.as_ref().unwrap().get(), r#""you are ""#);
        assert!(cut.input.is_none(), "a message whose structure alone is over the cap");
        assert!(cut.content_truncated);

        // Blocks: the longest text gives way first; role and type never do.
        let big = "é".repeat(400);
        let body = serde_json::json!({"system": [{"type": "text", "text": big}],
            "messages": [{"role": "user", "content": [{"type": "text", "text": "keep"}, {"type": "text", "text": big}]}]})
        .to_string();
        let cut = capture_request(body.as_bytes(), true, 200);
        let (input, system) = (cut.input.unwrap(), cut.system.unwrap());
        assert!(input.get().len() <= 200 && system.get().len() <= 200, "{} {}", input.get(), system.get());
        let v: serde_json::Value = serde_json::from_str(input.get()).unwrap();
        assert_eq!(v[0]["role"], "user");
        assert_eq!(v[0]["content"][0]["text"], "keep");
        let text = v[0]["content"][1]["text"].as_str().unwrap();
        assert!(!text.is_empty() && big.starts_with(text));
        let v: serde_json::Value = serde_json::from_str(system.get()).unwrap();
        assert_eq!(v[0]["type"], "text");
        assert!(cut.content_truncated);
        // Escapes count: a cut text still fits once encoded.
        let quotes = serde_json::json!({"messages": [{"role": "user", "content": "\"\n".repeat(300)}]}).to_string();
        let input = capture_request(quotes.as_bytes(), false, 100).input.unwrap();
        assert!(input.get().len() <= 100, "{}", input.get().len());

        let none = capture_request(br#"{"model":"m","messages":"not a list of messages at all"}"#, true, 10);
        assert!(none.input.is_none() && none.content_truncated);
        // the last of a duplicated field, as Go reads it
        let dup = capture_request(br#"{"messages":[1],"messages":[2]}"#, true, 100);
        assert_eq!(dup.input.unwrap().get(), "[2]");
        assert!(capture_request(b"not json", true, 100).input.is_none());

        let mut s = "héllo".to_string();
        assert!(cut_utf8(&mut s, 2));
        assert_eq!(s, "h");
        assert!(!cut_utf8(&mut s, 2));
    }

    #[test]
    fn a_drained_queue_gives_back_what_a_burst_grew() {
        let mut q: VecDeque<Event> = VecDeque::with_capacity(KEEP_CAPACITY * 16);
        q.push_back(event("x"));
        shrink(&mut q);
        assert!(q.capacity() >= KEEP_CAPACITY && q.capacity() < KEEP_CAPACITY * 2, "{}", q.capacity());
        assert_eq!(q.len(), 1);
        let cap = q.capacity();
        shrink(&mut q);
        assert_eq!(q.capacity(), cap, "never below what ordinary traffic needs");
    }

    #[test]
    fn batches_are_capped_by_count_and_bytes() {
        assert_eq!(batch_len([100; 10].into_iter(), 4, 1 << 20), 4, "event cap");
        assert_eq!(batch_len([100; 10].into_iter(), 100, 350), 3, "byte cap");
        assert_eq!(batch_len([1000, 10].into_iter(), 100, 500), 1, "one oversized event still goes out alone");
        assert_eq!(batch_len(std::iter::empty(), 100, 500), 0);
    }

    #[test]
    fn prices_like_go() {
        let w: Wire = serde_json::from_str(
            r#"{"schema":1,"version":1,"group":{"id":"g","name":"g"},
            "providers":[{"id":"p","name":"p","type":"openai_compatible","auth_mode":"none"}],
            "deployments":[
              {"id":"custom","provider_id":"p","model_name":"a","upstream_name":"a","input_per_1m":2,"output_per_1m":8,"cached_input_per_1m":0.5},
              {"id":"cat","provider_id":"p","model_name":"b","upstream_name":"b","catalog_model_id":"cat-1"},
              {"id":"none","provider_id":"p","model_name":"c","upstream_name":"c","catalog_model_id":"missing"}
            ],
            "prices":{"cat-1":{"input_per_1m":1,"output_per_1m":2}}}"#,
        )
        .unwrap();
        let c = Config::build(w).unwrap();
        let d = |m: &str| c.resolve(m).unwrap().deployments[0].clone();
        let u = Usage { prompt: 1_000_000, completion: 500_000, cached: 400_000, ..Usage::default() };
        // 600k at $2 + 400k at $0.5 + 500k at $8
        assert!((price(&c, &d("a"), u).unwrap() - (1.2 + 0.2 + 4.0)).abs() < 1e-9);
        // no cached rate: all prompt tokens at the input rate
        assert!((price(&c, &d("b"), u).unwrap() - (1.0 + 1.0)).abs() < 1e-9);
        assert!(price(&c, &d("c"), u).is_none());
    }

    /// The same cases internal/pricing/pricing_test.go checks.
    #[test]
    fn cost_fixtures() {
        #[derive(serde::Deserialize, Default)]
        #[serde(default)]
        struct Tokens {
            prompt: i64,
            completion: i64,
            cached: i64,
            write_5m: i64,
            write_1h: i64,
            us: bool,
            fast: bool,
        }
        #[derive(serde::Deserialize)]
        struct Case {
            name: String,
            rates: Vec<f64>,
            #[serde(default)]
            long: Vec<f64>,
            tokens: Tokens,
            want: f64,
        }
        let raw = std::fs::read("../internal/pricing/testdata/cost.json").unwrap();
        let cases: Vec<Case> = serde_json::from_slice(&raw).unwrap();
        assert!(!cases.is_empty());
        for c in cases {
            let p = Price {
                input_per_1m: c.rates[0],
                output_per_1m: c.rates[1],
                cached_input_per_1m: c.rates.get(2).copied(),
                long_prompt_tokens: c.long.first().map_or(0, |t| *t as i64),
                long_prompt_multiplier: c.long.get(1).copied().unwrap_or(1.0),
            };
            let t = c.tokens;
            let got = cost(
                p,
                Usage {
                    prompt: t.prompt,
                    completion: t.completion,
                    cached: t.cached,
                    write_5m: t.write_5m,
                    write_1h: t.write_1h,
                    us_inference: t.us,
                    fast: t.fast,
                    ..Usage::default()
                },
            );
            assert!((got - c.want).abs() < 1e-12, "{}: cost = {got}, want {}", c.name, c.want);
        }
    }
}
