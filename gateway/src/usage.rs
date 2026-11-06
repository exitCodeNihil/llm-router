//! Usage events and their cost (internal/usage, internal/pricing). Events queue
//! here and the sync loop uploads them; the control plane writes them to
//! Postgres and decides what telemetry to export.

use std::collections::VecDeque;
use std::sync::atomic::{AtomicU64, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::Instant;

use serde::{Deserialize, Serialize, Serializer};
use serde_json::value::RawValue;

use crate::config::{AuthMode, Config, Deployment, Key, Price};
use crate::telemetry::{self, Operation, Trace, Verdict};
use crate::translate::{Capture, Tag, Usage};
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
    /// Bytes, big-endian, not a u128: that would align every queued event
    /// to 16 bytes.
    #[serde(serialize_with = "hex_trace")]
    pub trace_id: [u8; 16],
    #[serde(skip_serializing_if = "is_zero_u64", serialize_with = "hex_span")]
    pub parent_span_id: u64,
    /// gen_ai.operation.name.
    pub operation: Operation,
    /// inference_geo and speed, boxed: most upstreams report neither, and
    /// every queued event would carry their room.
    #[serde(flatten, skip_serializing_if = "Option::is_none")]
    pub reported: Option<Box<Reported>>,
    /// The pricing cost_usd was computed with (usage.CostBasisCurrent).
    #[serde(skip_serializing_if = "is_zero_u8")]
    pub cost_basis: u8,
    /// Captured content: only when the gateway's verdict captures it. The
    /// verdict itself doesn't travel: the control plane decides export,
    /// content and rule again for every event it receives, and its decision
    /// alone counts.
    #[serde(flatten, skip_serializing_if = "Option::is_none")]
    pub content: Option<Box<Content>>,
}

/// What an Anthropic upstream reported about where and how it served a request.
#[derive(Debug, Default, Serialize)]
pub struct Reported {
    #[serde(skip_serializing_if = "Tag::is_empty")]
    pub inference_geo: Tag,
    #[serde(skip_serializing_if = "Tag::is_empty")]
    pub speed: Tag,
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
    /// Its encoded size, worked out once before it is queued: batches are
    /// sized under the queue's lock.
    #[serde(skip)]
    encoded: usize,
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
        if self.encoded > 0 { self.encoded } else { self.measure() }
    }

    /// Work out the encoded size now, to keep it.
    fn seal(&mut self) {
        self.encoded = self.measure();
    }

    fn measure(&self) -> usize {
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
fn hex_trace<S: Serializer>(n: &[u8; 16], s: S) -> Result<S::Ok, S::Error> {
    s.serialize_str(std::str::from_utf8(&telemetry::hex128(u128::from_be_bytes(*n))).unwrap_or_default())
}
fn hex_span<S: Serializer>(n: &u64, s: S) -> Result<S::Ok, S::Error> {
    s.serialize_str(std::str::from_utf8(&telemetry::hex64(*n)).unwrap_or_default())
}

/// Events waiting for the next sync. When the control plane is unreachable for
/// long enough to fill it, new events are dropped and counted.
const MAX_QUEUED: usize = 200_000;

/// The most captured content the gateway holds: queued, in the batch being
/// sent, and reserved by requests in flight. Past it a request is not captured, and an event put
/// back after a failed sync loses its content; either keeps its usage, marked
/// truncated, and is counted.
const CONTENT_BUDGET: usize = 64 << 20;

/// Content bytes reserved by requests in flight that capture it: the most each
/// can hold until its event is queued.
static RESERVED: AtomicUsize = AtomicUsize::new(0);
/// The queue's content, readable without its lock.
static QUEUED_CONTENT: AtomicUsize = AtomicUsize::new(0);
/// Content in the batch being sent: taken from the queue, not yet
/// acknowledged, dropped or put back.
static SENDING_CONTENT: AtomicUsize = AtomicUsize::new(0);

/// A request's share of the content budget, given back when dropped: once its
/// event is queued, where the queue counts the content instead, or when the
/// request ends without one.
#[derive(Debug, Default)]
pub struct Reservation(usize);

impl Reservation {
    /// `n` bytes of the budget, if what is queued and reserved leaves them.
    fn take(n: usize) -> Option<Reservation> {
        let held = QUEUED_CONTENT.load(Ordering::Relaxed) + SENDING_CONTENT.load(Ordering::Relaxed);
        let before = RESERVED.fetch_add(n, Ordering::Relaxed);
        if before + n + held > CONTENT_BUDGET {
            RESERVED.fetch_sub(n, Ordering::Relaxed);
            return None;
        }
        Some(Reservation(n))
    }

    /// Give back all but `n` bytes.
    fn keep(&mut self, n: usize) {
        if n < self.0 {
            RESERVED.fetch_sub(self.0 - n, Ordering::Relaxed);
            self.0 = n;
        }
    }
}

impl Drop for Reservation {
    fn drop(&mut self) {
        if self.0 > 0 {
            RESERVED.fetch_sub(self.0, Ordering::Relaxed);
        }
    }
}

/// Content bytes requests in flight have reserved.
pub fn reserved_content() -> usize {
    RESERVED.load(Ordering::Relaxed)
}

/// A batch's content, counted against the budget from when it leaves the
/// queue until the batch is acknowledged or dropped (dropping this) or its
/// events go back in the queue (drop this first, so they fit again).
#[derive(Debug, Default)]
pub struct Sending(usize);

impl Sending {
    pub fn of(events: &[Event]) -> Sending {
        let n = events.iter().filter_map(|e| e.content.as_ref()).map(|c| c.bytes()).sum();
        SENDING_CONTENT.fetch_add(n, Ordering::Relaxed);
        Sending(n)
    }
}

impl Drop for Sending {
    fn drop(&mut self) {
        if self.0 > 0 {
            SENDING_CONTENT.fetch_sub(self.0, Ordering::Relaxed);
        }
    }
}

/// Content bytes in the batch being sent.
pub fn sending_content() -> usize {
    SENDING_CONTENT.load(Ordering::Relaxed)
}

#[derive(Default)]
struct Queue {
    events: VecDeque<Event>,
    /// Content bytes the queued events hold.
    content: usize,
}

impl Queue {
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
/// Events that lost their content, or were never captured, because the
/// content budget was spent, or because a proxy refused even the smallest
/// batch with it.
pub static CONTENT_STRIPPED: AtomicU64 = AtomicU64::new(0);

fn push(mut e: Event) {
    if let Some(c) = e.content.as_mut() {
        c.seal();
    }
    let mut q = queue().lock().unwrap();
    if q.events.len() >= MAX_QUEUED {
        drop(q);
        DROPPED.fetch_add(1, Ordering::Relaxed);
        return;
    }
    // A captured event's reservation still covers its content: it fits.
    q.admit_within(&mut e, CONTENT_BUDGET);
    QUEUED_CONTENT.store(q.content, Ordering::Relaxed);
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
    QUEUED_CONTENT.store(q.content, Ordering::Relaxed);
    shrink(&mut q.events);
    batch
}

/// Content bytes the queue holds.
pub fn queued_content() -> usize {
    QUEUED_CONTENT.load(Ordering::Relaxed)
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
    /// count: the batch cap's 2x margin covers numbers far longer than real
    /// ones, but content, most of a large event, is never underestimated.
    pub fn encoded_size(&self) -> usize {
        self.content.as_ref().map_or(0, |c| c.encoded_size())
            + self.reported.as_ref().map_or(0, |_| 100)
            + if self.parent_span_id != 0 { 38 } else { 0 }
            + 560
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
    // what requests in flight reserved, and the batch being sent, keep theirs
    let budget = CONTENT_BUDGET
        .saturating_sub(RESERVED.load(Ordering::Relaxed))
        .saturating_sub(SENDING_CONTENT.load(Ordering::Relaxed));
    for mut e in events.into_iter().take(room).rev() {
        q.admit_within(&mut e, budget);
        q.events.push_front(e);
    }
    QUEUED_CONTENT.store(q.content, Ordering::Relaxed);
    drop(q);
    if dropped > 0 {
        DROPPED.fetch_add(dropped as u64, Ordering::Relaxed);
    }
}

/// Take the captured content out of events, keeping their usage, marked
/// truncated, and count them; how many had any.
pub fn strip_content(events: &mut [Event]) -> usize {
    let mut n = 0;
    for c in events.iter_mut().filter_map(|e| e.content.as_mut()) {
        if c.input.is_some() || c.system.is_some() || !c.output.is_empty() {
            **c = Content { content_truncated: true, ..Content::default() };
            n += 1;
        }
    }
    CONTENT_STRIPPED.fetch_add(n as u64, Ordering::Relaxed);
    n
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
    pub operation: Operation,
    pub verdict: Verdict,
    /// The request's content, once captured; the response text joins it at send.
    pub content: Option<Box<Content>>,
    pub output: Capture,
    /// The content budget the capture holds until the event is queued.
    pub reserved: Reservation,
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
/// strings don't fit. Only an oversized part takes this path: it is
/// re-encoded, keys in their order, numbers as written.
fn cut_json(raw: &str, limit: usize) -> Option<Box<RawValue>> {
    let mut v = Node::parse(raw, 0)?;
    let mut excess = v.encode().len().saturating_sub(limit);
    let mut all = Vec::new();
    v.strings(&mut all);
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
    let out = v.encode();
    if out.len() > limit {
        return None;
    }
    RawValue::from_string(out).ok()
}

/// A JSON value whose strings can be cut. Everything else keeps its text: a
/// number read into an f64 and written back can change (0.1000000000000000055
/// to 0.1) or not be written at all (1e999).
enum Node<'a> {
    Text(String),
    /// A number, true, false or null, as written.
    Raw(&'a str),
    List(Vec<Node<'a>>),
    Object(Vec<(String, Node<'a>)>),
}

/// Deeper than this is refused, as serde_json refuses it.
const MAX_DEPTH: usize = 128;

impl<'a> Node<'a> {
    fn parse(raw: &'a str, depth: usize) -> Option<Node<'a>> {
        let raw = raw.trim_matches([' ', '\t', '\n', '\r']);
        if depth > MAX_DEPTH {
            return None;
        }
        Some(match raw.as_bytes().first()? {
            b'"' => Node::Text(serde_json::from_str(raw).unwrap_or_else(|_| {
                let mut s = String::new();
                crate::translate::unescape_into(&raw[1..raw.len() - 1], &mut s, usize::MAX);
                s
            })),
            b'[' => Node::List(
                serde_json::from_str::<Vec<&RawValue>>(raw)
                    .ok()?
                    .into_iter()
                    .map(|x| Node::parse(x.get(), depth + 1))
                    .collect::<Option<_>>()?,
            ),
            b'{' => Node::Object(
                serde_json::from_str::<Entries>(raw)
                    .ok()?
                    .0
                    .into_iter()
                    .map(|(k, x)| Some((k, Node::parse(x.get(), depth + 1)?)))
                    .collect::<Option<_>>()?,
            ),
            _ => Node::Raw(raw),
        })
    }

    fn strings<'n>(&'n mut self, out: &mut Vec<&'n mut String>) {
        match self {
            Node::Text(s) => out.push(s),
            Node::Raw(_) => {}
            Node::List(a) => a.iter_mut().for_each(|x| x.strings(out)),
            Node::Object(m) => {
                for (k, x) in m.iter_mut() {
                    if k != "role" && k != "type" {
                        x.strings(out);
                    }
                }
            }
        }
    }

    fn encode(&self) -> String {
        let mut out = Vec::new();
        self.write(&mut out);
        String::from_utf8(out).unwrap_or_default()
    }

    fn write(&self, o: &mut Vec<u8>) {
        match self {
            Node::Text(s) => {
                let _ = serde_json::to_writer(&mut *o, s);
            }
            Node::Raw(r) => o.extend_from_slice(r.as_bytes()),
            Node::List(a) => {
                o.push(b'[');
                for (i, x) in a.iter().enumerate() {
                    if i > 0 {
                        o.push(b',');
                    }
                    x.write(o);
                }
                o.push(b']');
            }
            Node::Object(m) => {
                o.push(b'{');
                for (i, (k, x)) in m.iter().enumerate() {
                    if i > 0 {
                        o.push(b',');
                    }
                    let _ = serde_json::to_writer(&mut *o, k);
                    o.push(b':');
                    x.write(o);
                }
                o.push(b'}');
            }
        }
    }
}

/// An object's members in order, duplicates kept, values unparsed.
struct Entries<'a>(Vec<(String, &'a RawValue)>);

impl<'de: 'a, 'a> Deserialize<'de> for Entries<'a> {
    fn deserialize<D: serde::Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        struct V;
        impl<'de> serde::de::Visitor<'de> for V {
            type Value = Entries<'de>;
            fn expecting(&self, f: &mut std::fmt::Formatter) -> std::fmt::Result {
                f.write_str("an object")
            }
            fn visit_map<M: serde::de::MapAccess<'de>>(self, mut m: M) -> Result<Entries<'de>, M::Error> {
                let mut out = Vec::new();
                while let Some(e) = m.next_entry()? {
                    out.push(e);
                }
                Ok(Entries(out))
            }
        }
        d.deserialize_map(V)
    }
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
    /// and on the Anthropic surface its top-level system. It reserves what the
    /// capture can grow to, the response text up to the cap included, against
    /// the content budget; when that is spent, nothing is captured and the
    /// event is marked truncated.
    pub fn capture_request(&mut self, body: &[u8], with_system: bool) {
        let cap = self.verdict.capture;
        if cap == 0 {
            return;
        }
        // Input and system are parts of the body, each within the cap.
        let request = body.len().min(if with_system { cap.saturating_mul(2) } else { cap });
        let Some(mut reserved) = Reservation::take(request.saturating_add(cap)) else {
            CONTENT_STRIPPED.fetch_add(1, Ordering::Relaxed);
            self.verdict.capture = 0;
            self.content = Some(Box::new(Content { content_truncated: true, ..Content::default() }));
            return;
        };
        let c = capture_request(body, with_system, cap);
        reserved.keep(c.bytes() + cap);
        self.content = Some(Box::new(c));
        self.reserved = reserved;
    }

    /// The response text, for a verdict that captures content.
    pub fn set_output(&mut self, text: Capture) {
        if self.verdict.capture > 0 {
            self.output = text;
        }
    }

    /// The captured content, the output within the cap; None when there is
    /// none. It holds no more memory than its bytes: those are what the
    /// queue's content budget counts.
    fn take_content(&mut self) -> Option<Box<Content>> {
        let limit = self.verdict.capture;
        if limit == 0 {
            // none, or none for want of budget
            return self.content.take();
        }
        let mut c = self.content.take().unwrap_or_default();
        let (output, cut) = std::mem::take(&mut self.output).into_parts();
        c.output = output;
        c.content_truncated |= cut | cut_utf8(&mut c.output, limit);
        c.output.shrink_to_fit();
        if c.input.is_none() && c.system.is_none() && c.output.is_empty() && !c.content_truncated {
            return None;
        }
        Some(c)
    }

    /// Queue the request's event and count its tokens and spend against its limits.
    pub fn send(mut self, status: u16, error_code: &'static str, u: Usage, ttft_ms: i64) {
        let content = self.take_content();
        let reserved = std::mem::take(&mut self.reserved);
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
            trace_id: self.trace.id.to_be_bytes(),
            parent_span_id: self.trace.parent,
            operation: self.operation,
            reported: (!u.geo.is_empty() || !u.speed.is_empty())
                .then(|| Box::new(Reported { inference_geo: u.geo, speed: u.speed })),
            cost_basis: 0,
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
        drop(reserved); // the queue counts the content now
        limits::record(&self.key, tokens, cost);
    }
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use crate::config::Wire;

    fn event(id: &str) -> Event {
        Event { request_id: id.into(), model_name: "requeue-test".into(), status_code: 200, ..Event::default() }
    }

    #[test]
    fn requeued_events_go_out_first_in_order() {
        let _q = hold_queue();
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
            encoded: 0,
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

    /// Tests that use the process-wide queue or content budget, one at a time.
    static QUEUE: Mutex<()> = Mutex::new(());

    /// Hold the process-wide queue; a test that failed holding it doesn't
    /// fail the others.
    pub(crate) fn hold_queue() -> std::sync::MutexGuard<'static, ()> {
        QUEUE.lock().unwrap_or_else(|e| e.into_inner())
    }

    #[test]
    fn the_queue_counts_its_content() {
        let _q = hold_queue();
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

    /// Batches stay under the control plane's 32 MiB limit only if events
    /// are no larger than their estimate. Content, most of a large event, is
    /// never underestimated, escapes included, and overestimated by no more
    /// than a few bytes; the rest of an ordinary event not at all, and of one
    /// with every number at its longest by under 2x, the batch cap's margin.
    #[test]
    fn size_estimates_cover_content() {
        let ids = |e: Event| Event {
            request_id: "req_0123456789abcdef".into(),
            api_key_id: "4b7c3a1e-0000-4000-8000-000000000000".into(),
            user_id: "4b7c3a1e-0000-4000-8000-000000000001".into(),
            team_id: "4b7c3a1e-0000-4000-8000-000000000002".into(),
            deployment_id: "4b7c3a1e-0000-4000-8000-000000000003".into(),
            provider_id: "4b7c3a1e-0000-4000-8000-000000000004".into(),
            model_name: "claude-sonnet-4-5-20250929".into(),
            error_code: "upstream_rate_limited",
            trace_id: [0xff; 16],
            parent_span_id: u64::MAX,
            operation: Operation::TextCompletion,
            reported: Some(Box::new(Reported {
                inference_geo: Tag::new(&"g".repeat(40)),
                speed: Tag::new("standard"),
            })),
            cost_basis: 1,
            stream: true,
            unpriced: true,
            ..e
        };
        let ordinary = || {
            ids(Event {
                ts_ms: 1_793_975_436_295,
                prompt_tokens: 1_234_567,
                completion_tokens: 123_456,
                cached_tokens: 1_000_000,
                cache_write_5m: 200_000,
                cache_write_1h: 30_000,
                cost_usd: 0.123456789012345,
                notional_cost_usd: 1.23456789012345,
                latency_ms: 123_456,
                attempts: 3,
                ttft_ms: 12_345,
                status_code: 200,
                ..Event::default()
            })
        };
        let longest = || {
            ids(Event {
                ts_ms: i64::MIN,
                prompt_tokens: i64::MAX,
                completion_tokens: i64::MIN,
                cached_tokens: i64::MIN,
                cache_write_5m: i64::MIN,
                cache_write_1h: i64::MIN,
                cost_usd: -f64::MAX,
                notional_cost_usd: -f64::MIN_POSITIVE,
                latency_ms: i64::MIN,
                attempts: i64::MIN,
                ttft_ms: i64::MIN,
                status_code: u16::MAX,
                ..Event::default()
            })
        };
        let size = |e: &Event| serde_json::to_vec(e).unwrap().len();

        // without content
        for (e, margin) in [(event("plain"), 1), (ordinary(), 1), (longest(), 2)] {
            assert!(margin * e.encoded_size() >= size(&e), "estimate {} vs {}", e.encoded_size(), size(&e));
        }

        // content, every part at its hardest to encode
        let nasty = "\"\\\u{1}\n\u{7f}<é>\u{2028}😀".repeat(2000);
        let input =
            serde_json::to_string(&vec![serde_json::json!({"role": "user", "content": nasty.clone()}); 3]).unwrap();
        let system = serde_json::to_string(&nasty).unwrap();
        for (input, system, output) in [
            (&*input, Some(&*system), &*nasty),
            ("[]", None, ""),
            ("[]", None, "\u{0}\u{1f}"),
            (&*input, None, "plain"),
        ] {
            for base in [event("plain"), ordinary(), longest()] {
                let without = size(&base);
                let e = Event { content: content(input, system, output), ..base };
                let (estimate, added) = (e.content.as_ref().unwrap().encoded_size(), size(&e) - without);
                assert!(estimate >= added, "content estimate {estimate} < {added}");
                assert!(estimate <= added + 64, "content estimate {estimate} far over {added}");
            }
        }
    }

    /// The size is worked out once, before the queue's lock is taken: a
    /// batch is sized under it.
    #[test]
    fn content_size_is_worked_out_once() {
        let mut c = content("[1]", None, "a\nb").unwrap();
        let measured = c.encoded_size();
        assert_eq!(measured, 64 + 3 + 4);
        c.seal();
        c.output = "x".repeat(1000);
        assert_eq!(c.encoded_size(), measured, "kept, not counted again");
        let _q = hold_queue();
        push(Event { model_name: "sealed-test".into(), content: content("[1]", None, "a\nb"), ..event("s") });
        let (mine, rest): (Vec<Event>, Vec<Event>) =
            take(usize::MAX, usize::MAX).into_iter().partition(|e| e.model_name == "sealed-test");
        requeue(rest);
        assert_eq!(mine[0].content.as_ref().unwrap().encoded, measured);
    }

    /// The queue holds up to 200,000 of them.
    #[test]
    fn events_stay_small() {
        assert_eq!(std::mem::align_of::<Event>(), 8);
        assert!(std::mem::size_of::<Event>() <= 320, "{}", std::mem::size_of::<Event>());
    }

    #[test]
    fn serializes_trace_and_content() {
        let e = Event {
            trace_id: 0x4bf92f3577b34da6a3ce929d0e0e4736u128.to_be_bytes(),
            operation: Operation::Chat,
            reported: Some(Box::new(Reported { inference_geo: Tag::new("us"), ..Reported::default() })),
            content: content(r#"[{"role":"user","content":"hi"}]"#, None, "answer"),
            ..event("s")
        };
        let v: serde_json::Value = serde_json::from_slice(&serde_json::to_vec(&e).unwrap()).unwrap();
        assert_eq!(v["trace_id"], "4bf92f3577b34da6a3ce929d0e0e4736");
        assert!(v.get("parent_span_id").is_none() && v.get("speed").is_none() && v.get("system").is_none());
        assert_eq!(v["operation"], "chat");
        assert_eq!(v["inference_geo"], "us");
        assert!(v.get("export").is_none() && v.get("rule_id").is_none(), "the control plane decides those");
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
        assert_eq!(cut.input.as_ref().unwrap().get(), r#"[{"role":"user","content":"th"}]"#);
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

    pub(crate) fn emit_capturing(model: &str, capture: usize) -> Emit {
        let w: Wire = serde_json::from_str(r#"{"schema":2,"version":1,"group":{"id":"g","name":"g"}}"#).unwrap();
        Emit {
            cfg: Arc::new(Config::build(w).unwrap()),
            key: Arc::new(Key {
                id: "k".into(),
                user_id: String::new(),
                team_id: String::new(),
                user: None,
                team: None,
                allowed_models: None,
                expires_at: 0,
                scope: None,
                tags: Vec::new(),
            }),
            model: model.into(),
            request_id: "r".into(),
            stream: false,
            start: Instant::now(),
            ts_ms: 0,
            deployment: None,
            attempts: 0,
            trace: Trace::new(),
            operation: Operation::Chat,
            verdict: Verdict {
                decision: telemetry::Decision { export: true, capture_content: true, rule_id: None },
                capture,
            },
            content: None,
            output: Capture::default(),
            reserved: Reservation::default(),
        }
    }

    /// A request that captures reserves what its capture can grow to against
    /// the budget the queue's content counts in; past it the request isn't
    /// captured. The reservation is given back when its event is queued, or
    /// when the request ends without one.
    #[test]
    fn requests_in_flight_reserve_their_content() {
        let _q = hold_queue();
        let body = br#"{"system":"s","messages":[{"role":"user","content":"hello"}]}"#;
        let (before, queued) = (reserved_content(), queued_content());
        assert!(before + queued < CONTENT_BUDGET / 4, "{before} {queued}");
        let cap = CONTENT_BUDGET / 2;
        let mut a = emit_capturing("reserve-a", cap);
        a.capture_request(body, true);
        let held = body.len() - "{,}".len() - r#""system":"messages":"#.len() + cap;
        assert_eq!(reserved_content(), before + held, "the request as captured, the response up to the cap");
        assert!(a.content.as_ref().unwrap().input.is_some());

        // over the budget: usage only, marked, and counted
        let stripped = CONTENT_STRIPPED.load(Ordering::Relaxed);
        let mut b = emit_capturing("reserve-b", cap);
        b.capture_request(body, true);
        assert_eq!(reserved_content(), before + held);
        assert_eq!(b.verdict.capture, 0, "nor is the response captured");
        b.set_output(crate::translate::oai_json_text(br#"{"choices":[{"message":{"content":"x"}}]}"#, cap));
        assert!(CONTENT_STRIPPED.load(Ordering::Relaxed) > stripped);
        b.send(200, "", Usage::default(), 0);

        // a request that ends without an event gives its room back
        drop(a);
        assert_eq!(reserved_content(), before);
        let mut c = emit_capturing("reserve-c", cap);
        c.capture_request(body, true);
        assert!(reserved_content() > before);
        c.set_output(crate::translate::oai_json_text(br#"{"choices":[{"message":{"content":"answer"}}]}"#, cap));
        c.send(200, "", Usage::default(), 0);
        assert_eq!(reserved_content(), before, "the queue counts it now");

        let (mine, rest): (Vec<Event>, Vec<Event>) =
            take(usize::MAX, usize::MAX).into_iter().partition(|e| e.model_name.starts_with("reserve-"));
        requeue(rest);
        assert_eq!(queued_content(), queued);
        let [b, c] = &mine[..] else { panic!("{mine:?}") };
        let b = b.content.as_ref().unwrap();
        assert!(b.content_truncated && b.input.is_none() && b.output.is_empty());
        let c = c.content.as_ref().unwrap();
        assert_eq!(
            (c.input.as_ref().unwrap().get(), c.output.as_str()),
            (r#"[{"role":"user","content":"hello"}]"#, "answer")
        );
    }

    /// The batch being sent keeps its content counted until it is
    /// acknowledged or dropped; put back in the queue, it keeps its content.
    #[test]
    fn the_batch_being_sent_counts_against_the_budget() {
        let _q = hold_queue();
        let big = format!("\"{}\"", "x".repeat(CONTENT_BUDGET / 2));
        let events = vec![Event { model_name: "sending-test".into(), content: content(&big, None, ""), ..event("s") }];
        let n = events[0].content.as_ref().unwrap().bytes();
        let before = sending_content();
        let sending = Sending::of(&events);
        assert_eq!(sending_content(), before + n);

        let free = CONTENT_BUDGET - queued_content() - sending_content() - reserved_content();
        assert!(Reservation::take(free + 1).is_none(), "the batch's content is spoken for");
        assert!(Reservation::take(free).is_some());

        drop(sending);
        assert_eq!(sending_content(), before);
        assert!(Reservation::take(free + n).is_some(), "acknowledged, its room is free again");

        // A failed sync puts the batch back: released first, it fits.
        let sending = Sending::of(&events);
        drop(sending);
        requeue(events);
        let (mine, rest): (Vec<Event>, Vec<Event>) =
            take(usize::MAX, usize::MAX).into_iter().partition(|e| e.model_name == "sending-test");
        requeue(rest);
        assert!(!mine[0].content.as_ref().unwrap().content_truncated);
    }

    /// What is queued holds no more memory than the bytes the budget counts.
    #[test]
    fn captured_output_holds_only_its_bytes() {
        let mut em = emit_capturing("capacity-test", 1000);
        // text kept at a larger cap than the verdict's is cut here
        let body = format!(r#"{{"choices":[{{"message":{{"content":"{}"}}}}]}}"#, "é".repeat(1500));
        em.set_output(crate::translate::oai_json_text(body.as_bytes(), 3000));
        let c = em.take_content().unwrap();
        assert!(c.content_truncated);
        assert_eq!(c.output.len(), 1000);
        assert_eq!(c.output.capacity(), c.output.len());
    }

    /// A cut part keeps every number as written, its keys in their order.
    #[test]
    fn cutting_keeps_numbers_as_written() {
        let nums = r#"{"z":1e999,"f":0.1000000000000000055511151231257827,"big":123456789012345678901234567890,"n":-0,"e":1E+2,"t":true,"x":null}"#;
        let raw = format!(
            r#"{{"role":"user","content":"{}","meta":{nums},"list":[1.50,"{}"]}}"#,
            "a".repeat(300),
            "b".repeat(50)
        );
        let cut = cut_json(&raw, 260).unwrap();
        let got = cut.get();
        assert!(got.len() <= 260, "{got}");
        assert!(got.starts_with(r#"{"role":"user","content":"a"#), "{got}");
        assert!(got.contains(&format!(r#""meta":{nums},"list":[1.50,"b"#)), "{got}");
        // an unpaired surrogate in the text is no reason to lose the part
        let cut = cut_json(&format!(r#"["\ud83d{}"]"#, "c".repeat(100)), 20).unwrap();
        assert_eq!(cut.get(), format!("[\"\u{fffd}{}\"]", "c".repeat(13)));
        // nesting serde_json would refuse is refused, not a stack overflow
        let deep = format!("{}\"{}\"{}", "[".repeat(100_000), "d".repeat(10), "]".repeat(100_000));
        assert!(cut_json(&deep, 10).is_none());
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
