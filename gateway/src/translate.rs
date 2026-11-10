//! OpenAI <-> Anthropic translation, ported from internal/gateway/anthropic.go
//! and internal/gateway/anthropic_upstream.go. Go is the reference: the shared
//! fixtures in internal/gateway/testdata/translate are its output.
//!
//! Streams are fed one server-sent event at a time: the event's lines joined
//! with "\n", without the blank line that ends it. Converters append complete
//! events (each ending in "\n\n") to `out`.
//!
//! Parsing follows Go's encoding/json where it matters for real traffic: null
//! reads as the zero value, unknown fields are ignored, and a wrongly typed
//! known field fails the whole document. Text is copied through as raw JSON
//! string literals (see [`JStr`]) and output is written straight into a buffer.

use std::borrow::Cow;
use std::io::Write as _;

use memchr::memmem;
use serde::Deserialize;
use serde::de::{self, Deserializer, IgnoredAny};
use serde_json::error::Category;
use serde_json::value::RawValue;

/// Token counts read from an upstream response. `prompt` includes cached and
/// cache-creation tokens; `cached` is the cache reads alone.
#[derive(Debug, Default, Clone, Copy, PartialEq, Eq)]
pub struct Usage {
    pub prompt: i64,
    pub completion: i64,
    pub cached: i64,
    /// Parts of `prompt` that cost more than plain input (Go: usageCounts).
    pub write_5m: i64,
    pub write_1h: i64,
    /// Request options that raise every rate.
    pub us_inference: bool,
    pub fast: bool,
    /// inference_geo and speed as an Anthropic upstream reported them.
    pub geo: Tag,
    pub speed: Tag,
}

impl Usage {
    /// The input side of a streamed usage event: message_start has it, and Anthropic
    /// repeats it in message_delta without the options or the 5m/1h split of its
    /// cache writes, so an unchanged total keeps the split already known.
    fn set_input(&mut self, n: Usage) {
        (self.prompt, self.cached) = (n.prompt, n.cached);
        if n.write_1h > 0 || n.write_5m + n.write_1h != self.write_5m + self.write_1h {
            (self.write_5m, self.write_1h) = (n.write_5m, n.write_1h);
        }
        self.us_inference |= n.us_inference;
        self.fast |= n.fast;
        if !n.geo.is_empty() {
            self.geo = n.geo;
        }
        if !n.speed.is_empty() {
            self.speed = n.speed;
        }
    }
}

/// A short string an upstream reports, such as inference_geo, kept inline so
/// [`Usage`] stays `Copy` and reading it allocates nothing. Longer values are
/// cut at 31 bytes.
#[derive(Clone, Copy, Default, PartialEq, Eq)]
pub struct Tag {
    len: u8,
    buf: [u8; 31],
}

impl Tag {
    pub fn new(s: &str) -> Tag {
        let mut n = s.len().min(31);
        while !s.is_char_boundary(n) {
            n -= 1;
        }
        let mut t = Tag { len: n as u8, buf: [0; 31] };
        t.buf[..n].copy_from_slice(&s.as_bytes()[..n]);
        t
    }

    pub fn as_str(&self) -> &str {
        std::str::from_utf8(&self.buf[..self.len as usize]).unwrap_or_default()
    }

    pub fn is_empty(&self) -> bool {
        self.len == 0
    }
}

impl std::fmt::Debug for Tag {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        self.as_str().fmt(f)
    }
}

impl serde::Serialize for Tag {
    fn serialize<S: serde::Serializer>(&self, s: S) -> Result<S::Ok, S::Error> {
        s.serialize_str(self.as_str())
    }
}

impl<'de> Deserialize<'de> for Tag {
    /// A string, or null for none.
    fn deserialize<D: Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        struct V;
        impl de::Visitor<'_> for V {
            type Value = Tag;
            fn expecting(&self, f: &mut std::fmt::Formatter) -> std::fmt::Result {
                f.write_str("a string")
            }
            fn visit_str<E: de::Error>(self, v: &str) -> Result<Tag, E> {
                Ok(Tag::new(v))
            }
            fn visit_unit<E: de::Error>(self) -> Result<Tag, E> {
                Ok(Tag::default())
            }
            fn visit_none<E: de::Error>(self) -> Result<Tag, E> {
                Ok(Tag::default())
            }
        }
        d.deserialize_any(V)
    }
}

/// Response text kept for telemetry: at most `cap` bytes, cut at a
/// character. Go keeps whole deltas while fewer than `cap` bytes are kept and
/// cuts the text to the cap when the event is sent; this keeps the same text
/// without ever holding more, and its buffer never grows past the cap. A cap
/// of 0 keeps nothing and costs nothing.
#[derive(Default)]
pub struct Capture {
    cap: usize,
    text: String,
    /// Some text was left out.
    cut: bool,
}

impl Capture {
    pub fn new(cap: usize) -> Capture {
        Capture { cap, ..Capture::default() }
    }

    #[inline]
    fn wants(&self) -> bool {
        !self.cut && self.text.len() < self.cap
    }

    fn push(&mut self, s: JStr) {
        let inner = s.inner();
        let room = self.cap.saturating_sub(self.text.len());
        // Escaped text is never shorter than its value: this is room enough.
        self.grow(inner.len().min(room));
        if inner.contains('\\') {
            self.cut |= unescape_into(inner, &mut self.text, room);
        } else if inner.len() > room {
            self.text.push_str(&inner[..floor_boundary(inner, room)]);
            self.cut = true;
        } else {
            self.text.push_str(inner);
        }
    }

    /// Room for `n` more bytes: doubling as a String does, up to the cap.
    fn grow(&mut self, n: usize) {
        let need = self.text.len() + n;
        if need > self.text.capacity() {
            let want = (self.text.capacity() * 2).clamp(need, self.cap.max(need));
            self.text.reserve_exact(want - self.text.len());
        }
    }

    /// The text kept, and whether any was left out.
    pub fn into_parts(self) -> (String, bool) {
        (self.text, self.cut)
    }
}

/// A request body the gateway could not translate; the message goes back to
/// the client with a 400.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct BadRequest(pub String);

/// Anthropic Messages request -> OpenAI chat-completions body (Go: anthToOpenAI,
/// then "model" set to the upstream name). `model` is the upstream model name.
pub fn anth_req_to_oai(body: &[u8], model: &str) -> Result<Vec<u8>, BadRequest> {
    let req: AnthRequest = parse(body).map_err(|e| BadRequest(format!("invalid JSON: {e}")))?;
    let mut out = Vec::with_capacity(body.len() + 256);
    let o = &mut out;
    put(o, r#"{"model":"#);
    esc(o, model);
    put(o, r#","messages":["#);
    if let Some(system) = req.system {
        put(o, r#"{"role":"system","content":"#);
        put(o, &flatten_text(Some(system)));
        put(o, "}");
    }
    for m in &req.messages {
        match parse_content(m.content) {
            Ok(Content::Text(text)) => {
                comma(o);
                put(o, r#"{"role":"#);
                put(o, m.role.raw());
                put(o, r#","content":"#);
                put(o, text.raw());
                put(o, "}");
            }
            Ok(Content::Blocks(blocks)) if m.role.is("assistant") => anth_assistant(o, &blocks),
            Ok(Content::Blocks(blocks)) => anth_user(o, &blocks),
            Err(e) => return Err(BadRequest(format!("invalid message content: {e}"))),
        }
    }
    put(o, r#"],"max_tokens":"#);
    int(o, req.max_tokens);
    if req.stream {
        put(o, r#","stream":true,"stream_options":{"include_usage":true}"#);
    }
    if let Some(t) = req.temperature {
        put(o, r#","temperature":"#);
        num(o, t);
    }
    if let Some(p) = req.top_p {
        put(o, r#","top_p":"#);
        num(o, p);
    }
    if !req.stop_sequences.is_empty() {
        put(o, r#","stop":["#);
        for s in &req.stop_sequences {
            comma(o);
            put(o, s.raw());
        }
        put(o, "]");
    }
    if !req.tools.is_empty() {
        put(o, r#","tools":["#);
        for t in &req.tools {
            comma(o);
            put(o, r#"{"type":"function","function":{"name":"#);
            put(o, t.name.raw());
            put(o, r#","description":"#);
            put(o, t.description.raw());
            put(o, r#","parameters":"#);
            put(o, t.input_schema.map_or("null", RawValue::get));
            put(o, "}}");
        }
        put(o, "]");
    }
    // a tool_choice that doesn't parse is dropped, as in Go
    let choice = req.tool_choice.and_then(|r| parse::<AnthToolChoice>(r.get().as_bytes()).ok());
    if let Some(tc) = choice {
        match tc.typ.value().as_ref() {
            "auto" => put(o, r#","tool_choice":"auto""#),
            "any" => put(o, r#","tool_choice":"required""#),
            "none" => put(o, r#","tool_choice":"none""#),
            "tool" => {
                put(o, r#","tool_choice":{"type":"function","function":{"name":"#);
                put(o, tc.name.raw());
                put(o, "}}");
            }
            _ => {}
        }
    }
    put(o, "}");
    Ok(out)
}

fn anth_assistant(o: &mut Vec<u8>, blocks: &[AnthBlock]) {
    let texts = || blocks.iter().filter(|b| b.typ.is("text")).map(|b| b.text.raw());
    comma(o);
    put(o, r#"{"role":"assistant","content":"#);
    if texts().next().is_some() {
        put(o, &join(texts(), ""));
    } else {
        put(o, "null");
    }
    let mut calls = blocks.iter().filter(|b| b.typ.is("tool_use")).peekable();
    if calls.peek().is_some() {
        put(o, r#","tool_calls":["#);
        for b in calls {
            comma(o);
            put(o, r#"{"id":"#);
            put(o, b.id.raw());
            put(o, r#","type":"function","function":{"name":"#);
            put(o, b.name.raw());
            put(o, r#","arguments":"#);
            esc(o, b.input.map_or("{}", RawValue::get));
            put(o, "}}");
        }
        put(o, "]");
    }
    put(o, "}");
}

fn anth_user(o: &mut Vec<u8>, blocks: &[AnthBlock]) {
    // tool results become their own role:"tool" messages, ahead of the rest
    for b in blocks.iter().filter(|b| b.typ.is("tool_result")) {
        comma(o);
        put(o, r#"{"role":"tool","tool_call_id":"#);
        put(o, b.tool_use_id.raw());
        put(o, r#","content":"#);
        put(o, &flatten_text(b.content));
        put(o, "}");
    }
    let image = |b: &AnthBlock| b.typ.is("image") && b.source.as_ref().is_some_and(|s| s.typ.is("base64"));
    let mut parts = blocks.iter().filter(|b| b.typ.is("text") || image(b)).peekable();
    if parts.peek().is_none() {
        return;
    }
    comma(o);
    put(o, r#"{"role":"user","content":["#);
    for b in parts {
        comma(o);
        match &b.source {
            Some(s) if !b.typ.is("text") => {
                // escaped bodies concatenate into a valid literal
                put(o, r#"{"type":"image_url","image_url":{"url":"data:"#);
                put(o, s.media_type.inner());
                put(o, ";base64,");
                put(o, s.data.inner());
                put(o, r#""}}"#);
            }
            _ => {
                put(o, r#"{"type":"text","text":"#);
                put(o, b.text.raw());
                put(o, "}");
            }
        }
    }
    put(o, "]}");
}

/// OpenAI chat-completions request -> Anthropic Messages body (Go: oaiToAnthBody).
pub fn oai_req_to_anth(body: &[u8], model: &str) -> Result<Vec<u8>, BadRequest> {
    let req: OaiRequest = parse(body).map_err(|e| {
        BadRequest(match e.classify() {
            Category::Data => format!("unparseable request: {e}"),
            _ => format!("invalid JSON body: {e}"),
        })
    })?;
    let mut out = Vec::with_capacity(body.len() + 256);
    let o = &mut out;
    put(o, r#"{"model":"#);
    esc(o, model);
    put(o, r#","messages":["#);
    let mut system = Vec::new();
    let mut open = None;
    for m in &req.messages {
        match m.role.value().as_ref() {
            "system" | "developer" => system.push(oai_content_text(m.content)),
            "assistant" => {
                let text = oai_content_text(m.content);
                let has_text = text != EMPTY;
                if !has_text && m.tool_calls.is_empty() {
                    continue;
                }
                turn(o, &mut open, "assistant");
                if has_text {
                    comma(o);
                    put(o, r#"{"type":"text","text":"#);
                    put(o, &text);
                    put(o, "}");
                }
                for tc in &m.tool_calls {
                    comma(o);
                    put(o, r#"{"type":"tool_use","id":"#);
                    put(o, tc.id.raw());
                    put(o, r#","name":"#);
                    put(o, tc.function.name.raw());
                    put(o, r#","input":"#);
                    tool_input(o, tc.function.arguments);
                    put(o, "}");
                }
            }
            "tool" => {
                turn(o, &mut open, "user");
                comma(o);
                put(o, r#"{"type":"tool_result","tool_use_id":"#);
                put(o, m.tool_call_id.raw());
                put(o, r#","content":"#);
                put(o, &oai_content_text(m.content));
                put(o, "}");
            }
            _ => {
                turn(o, &mut open, "user");
                oai_content_blocks(o, m.content);
            }
        }
    }
    if open.is_some() {
        put(o, "]}");
    }
    // Anthropic requires max_tokens; OpenAI clients may omit it
    let max_tokens = [req.max_tokens, req.max_completion_tokens].into_iter().find(|&n| n != 0);
    put(o, r#"],"max_tokens":"#);
    int(o, max_tokens.unwrap_or(4096));
    if !system.is_empty() {
        put(o, r#","system":"#);
        put(o, &join(system.iter().map(|s| s.as_ref()), r"\n\n"));
    }
    if req.stream.is_some_and(|s| s.get() == "true") {
        put(o, r#","stream":true"#);
    }
    if let Some(t) = req.temperature {
        put(o, r#","temperature":"#);
        num(o, t);
    }
    if let Some(p) = req.top_p {
        put(o, r#","top_p":"#);
        num(o, p);
    }
    if let Some(stop) = req.stop.map(RawValue::get) {
        // Go keeps what its decoder filled in before a type error: an
        // element that is not a string becomes ""
        let stops = if stop.starts_with('"') {
            vec![stop]
        } else {
            let elems = parse::<Vec<&RawValue>>(stop.as_bytes()).unwrap_or_default();
            elems.into_iter().map(|e| if e.get().starts_with('"') { e.get() } else { EMPTY }).collect()
        };
        if !stops.is_empty() {
            put(o, r#","stop_sequences":["#);
            for s in stops {
                comma(o);
                put(o, s);
            }
            put(o, "]");
        }
    }
    if !req.tools.is_empty() {
        put(o, r#","tools":["#);
        for t in &req.tools {
            let f = &t.function;
            comma(o);
            put(o, r#"{"name":"#);
            put(o, f.name.raw());
            put(o, r#","description":"#);
            put(o, f.description.raw());
            put(o, r#","input_schema":"#);
            put(o, f.parameters.map_or(r#"{"type":"object"}"#, RawValue::get));
            put(o, "}");
        }
        put(o, "]");
    }
    if let Some(tc) = req.tool_choice.map(RawValue::get) {
        if tc.starts_with('"') {
            match JStr(Some(tc)).value().as_ref() {
                "auto" => put(o, r#","tool_choice":{"type":"auto"}"#),
                "required" => put(o, r#","tool_choice":{"type":"any"}"#),
                "none" => put(o, r#","tool_choice":{"type":"none"}"#),
                _ => {}
            }
        } else if let Ok(c) = parse::<OaiToolChoice>(tc.as_bytes())
            && !c.function.name.is_empty()
        {
            put(o, r#","tool_choice":{"type":"tool","name":"#);
            put(o, c.function.name.raw());
            put(o, "}");
        }
    }
    put(o, "}");
    Ok(out)
}

/// Opens a `role` message unless one is already open, since Anthropic wants
/// consecutive same-role turns merged.
fn turn(o: &mut Vec<u8>, open: &mut Option<&'static str>, role: &'static str) {
    if *open == Some(role) {
        return;
    }
    if open.is_some() {
        put(o, "]}");
    }
    comma(o);
    put(o, r#"{"role":""#);
    put(o, role);
    put(o, r#"","content":["#);
    *open = Some(role);
}

/// Tool-call arguments as an Anthropic input: the parsed arguments, or {}
/// when they are empty or not JSON.
fn tool_input(o: &mut Vec<u8>, args: JStr) {
    match serde_json::from_str::<&RawValue>(&args.value()) {
        Ok(v) => put(o, v.get()),
        Err(_) => put(o, "{}"),
    }
}

/// OpenAI content (string or parts) flattened to a text literal (Go: oaiContentText).
fn oai_content_text(raw: Option<&RawValue>) -> Cow<'_, str> {
    match raw.map(RawValue::get) {
        Some(s) if s.starts_with('"') => Cow::Borrowed(s),
        Some(s) => match parse::<Vec<TextPart>>(s.as_bytes()) {
            Ok(parts) => join(parts.iter().filter(|p| p.typ.is("text")).map(|p| p.text.raw()), ""),
            Err(_) => Cow::Borrowed(EMPTY),
        },
        None => Cow::Borrowed(EMPTY),
    }
}

/// OpenAI user content -> Anthropic blocks, written into the open content
/// array; never empty (Go: oaiContentBlocks).
fn oai_content_blocks(o: &mut Vec<u8>, raw: Option<&RawValue>) {
    let start = o.len();
    match raw.map(RawValue::get) {
        Some(s) if s.starts_with('"') => {
            comma(o);
            put(o, r#"{"type":"text","text":"#);
            put(o, s);
            put(o, "}");
        }
        Some(s) => {
            for p in parse::<Vec<OaiPart>>(s.as_bytes()).unwrap_or_default() {
                if p.typ.is("text") {
                    comma(o);
                    put(o, r#"{"type":"text","text":"#);
                    put(o, p.text.raw());
                    put(o, "}");
                } else if p.typ.is("image_url") {
                    let url = p.image_url.url.value();
                    // data:media/type;base64,... -> anthropic base64 image block
                    if let Some((media_type, data)) = parse_data_url(&url) {
                        comma(o);
                        put(o, r#"{"type":"image","source":{"type":"base64","media_type":"#);
                        esc(o, media_type);
                        put(o, r#","data":"#);
                        esc(o, data);
                        put(o, "}}");
                    }
                }
            }
        }
        None => {}
    }
    if o.len() == start {
        comma(o);
        put(o, r#"{"type":"text","text":""}"#);
    }
}

fn parse_data_url(u: &str) -> Option<(&str, &str)> {
    let (meta, data) = u.strip_prefix("data:")?.split_once(',')?;
    Some((meta.strip_suffix(";base64").unwrap_or(meta), data))
}

/// OpenAI chat-completion JSON -> Anthropic message JSON (Go: writeAnthResponse).
/// `model` is the name the client asked for; `req_id` makes the message id.
pub fn oai_resp_to_anth(body: &[u8], model: &str, req_id: &str) -> (Vec<u8>, Usage) {
    let resp = match parse::<OaiResponse>(body) {
        Ok(r) if !r.choices.is_empty() => r,
        _ => return (ANTH_UNPARSEABLE.into(), Usage::default()),
    };
    let usage = resp.usage.as_ref().map_or_else(Usage::default, OaiUsage::counts);
    let choice = &resp.choices[0];
    let mut out = Vec::with_capacity(body.len() + 128);
    let o = &mut out;
    put(o, r#"{"id":"#);
    esc(o, &format!("msg_{req_id}"));
    put(o, r#","type":"message","role":"assistant","model":"#);
    esc(o, model);
    put(o, r#","content":["#);
    if !choice.message.content.is_empty() {
        put(o, r#"{"type":"text","text":"#);
        put(o, choice.message.content.raw());
        put(o, "}");
    }
    for tc in &choice.message.tool_calls {
        comma(o);
        put(o, r#"{"type":"tool_use","id":"#);
        put(o, tc.id.raw());
        put(o, r#","name":"#);
        put(o, tc.function.name.raw());
        put(o, r#","input":"#);
        tool_input(o, tc.function.arguments);
        put(o, "}");
    }
    put(o, r#"],"stop_reason":""#);
    put(o, anth_stop_reason(choice.finish_reason));
    put(o, r#"","stop_sequence":null,"usage":{"input_tokens":"#);
    int(o, usage.prompt);
    put(o, r#","output_tokens":"#);
    int(o, usage.completion);
    put(o, "}}");
    (out, usage)
}

/// Anthropic message JSON -> OpenAI chat-completion JSON (Go: anthRespToOAI).
pub fn anth_resp_to_oai(body: &[u8], model: &str, req_id: &str) -> (Vec<u8>, Usage) {
    let Ok(resp) = parse::<AnthResponse>(body) else {
        return (OAI_UNPARSEABLE.into(), Usage::default());
    };
    let usage = resp.usage.as_ref().map_or_else(Usage::default, AnthUsage::counts);
    let text = join(resp.content.iter().filter(|b| b.typ.is("text")).map(|b| b.text.raw()), "");
    let mut calls = resp.content.iter().filter(|b| b.typ.is("tool_use")).peekable();
    let mut out = Vec::with_capacity(body.len() + 128);
    let o = &mut out;
    put(o, r#"{"id":"#);
    esc(o, &format!("chatcmpl-{req_id}"));
    put(o, r#","object":"chat.completion","model":"#);
    esc(o, model);
    put(o, r#","choices":[{"index":0,"message":{"role":"assistant","content":"#);
    if calls.peek().is_some() {
        put(o, if text == EMPTY { "null" } else { text.as_ref() });
        put(o, r#","tool_calls":["#);
        for b in calls {
            comma(o);
            put(o, r#"{"id":"#);
            put(o, b.id.raw());
            put(o, r#","type":"function","function":{"name":"#);
            put(o, b.name.raw());
            put(o, r#","arguments":"#);
            esc(o, b.input.map_or("{}", RawValue::get));
            put(o, "}}");
        }
        put(o, "]");
    } else {
        put(o, &text);
    }
    put(o, r#"},"finish_reason":""#);
    put(o, oai_finish_reason(resp.stop_reason));
    put(o, r#""}],"usage":"#);
    oai_usage(o, usage);
    put(o, "}");
    (out, usage)
}

/// Usage of an OpenAI chat-completion JSON (Go: copyJSON).
pub fn oai_json_usage(body: &[u8]) -> Usage {
    match parse::<OaiJson>(body) {
        Ok(OaiJson { usage: Some(u), .. }) => u.counts(),
        _ => Usage::default(),
    }
}

/// Usage of an Anthropic message JSON (Go: passthroughAnthJSON).
pub fn anth_json_usage(body: &[u8]) -> Usage {
    match parse::<AnthJson>(body) {
        Ok(AnthJson { usage: Some(u), .. }) => u.counts(),
        _ => Usage::default(),
    }
}

/// The assistant text of an OpenAI chat-completion JSON, at most `cap` bytes
/// of it, for telemetry (Go: copyJSON, writeAnthResponse).
pub fn oai_json_text(body: &[u8], cap: usize) -> Capture {
    let mut c = Capture::new(cap);
    if let Ok(r) = parse::<OaiResponse>(body)
        && let Some(choice) = r.choices.first()
        && c.wants()
    {
        c.push(choice.message.content);
    }
    c
}

/// The text blocks of an Anthropic message JSON, joined, at most `cap` bytes
/// of them, for telemetry (Go: passthroughAnthJSON, anthRespToOAI).
pub fn anth_json_text(body: &[u8], cap: usize) -> Capture {
    let mut c = Capture::new(cap);
    if let Ok(r) = parse::<AnthResponse>(body) {
        for b in r.content.iter().filter(|b| b.typ.is("text")) {
            if !c.wants() {
                c.cut |= !b.text.is_empty();
                continue;
            }
            c.push(b.text);
        }
    }
    c
}

/// A streaming converter, fed one event at a time.
pub trait SseTransform: Send {
    /// One upstream event (lines joined by "\n", no trailing blank line).
    fn event(&mut self, ev: &[u8], out: &mut Vec<u8>);
    /// The upstream stream ended; write whatever closes the client's stream.
    fn finish(&mut self, out: &mut Vec<u8>);
    /// Usage seen so far.
    fn usage(&self) -> Usage;
    /// The response text captured for telemetry; empty unless asked for.
    fn take_output(&mut self) -> Capture {
        Capture::default()
    }
}

/// OpenAI SSE passthrough that reads usage (Go: streamSSE). With
/// `drop_usage_chunk`, the usage-only chunk the gateway asked for itself is
/// not forwarded.
pub struct OaiPassthrough {
    drop_usage_chunk: bool,
    usage: Usage,
    capture: Capture,
}

impl OaiPassthrough {
    pub fn new(drop_usage_chunk: bool) -> Self {
        Self { drop_usage_chunk, usage: Usage::default(), capture: Capture::default() }
    }

    /// Keep up to `cap` bytes of the response text for telemetry.
    pub fn capturing(mut self, cap: usize) -> Self {
        self.capture = Capture::new(cap);
        self
    }
}

impl SseTransform for OaiPassthrough {
    fn event(&mut self, ev: &[u8], out: &mut Vec<u8>) {
        let start = out.len();
        for line in lines(ev) {
            if self.capture.wants()
                && let Some(data) = line.strip_prefix(b"data: ")
                && data != b"[DONE]"
                && memmem::find(data, br#""content""#).is_some()
                && let Ok(chunk) = parse::<DeltaChunk>(data)
                && let Some(c) = chunk.choices.first()
            {
                self.capture.push(c.delta.content);
            }
            if let Some(data) = line.strip_prefix(b"data: ")
                && data != b"[DONE]"
                && memmem::find(data, br#""usage""#).is_some()
                && let Ok(chunk) = parse::<UsageChunk>(data)
                && let Some(u) = chunk.usage
            {
                self.usage = u.counts();
                if self.drop_usage_chunk && chunk.choices.is_empty() {
                    continue;
                }
            }
            out.extend_from_slice(line);
            out.push(b'\n');
        }
        if out.len() > start {
            out.push(b'\n');
        }
    }
    fn finish(&mut self, _out: &mut Vec<u8>) {}
    fn usage(&self) -> Usage {
        self.usage
    }
    fn take_output(&mut self) -> Capture {
        std::mem::take(&mut self.capture)
    }
}

/// Anthropic SSE passthrough that reads usage (Go: streamAnthPassthrough).
pub struct AnthPassthrough {
    usage: Usage,
    capture: Capture,
}

impl AnthPassthrough {
    pub fn new() -> Self {
        Self { usage: Usage::default(), capture: Capture::default() }
    }

    /// Keep up to `cap` bytes of the response text for telemetry.
    pub fn capturing(mut self, cap: usize) -> Self {
        self.capture = Capture::new(cap);
        self
    }
}

impl Default for AnthPassthrough {
    fn default() -> Self {
        Self::new()
    }
}

impl SseTransform for AnthPassthrough {
    fn event(&mut self, ev: &[u8], out: &mut Vec<u8>) {
        let start = out.len();
        for line in lines(ev) {
            if let Some(data) = line.strip_prefix(b"data: ")
                && (memmem::find(data, br#""input_tokens""#).is_some()
                    || memmem::find(data, br#""output_tokens""#).is_some())
                && let Ok(ev) = parse::<AnthUsageEvent>(data)
            {
                if let Some(u) = &ev.message.usage {
                    // message_start
                    let c = u.counts();
                    self.usage.set_input(c);
                }
                if let Some(u) = &ev.usage {
                    // message_delta: some upstreams repeat the input side here,
                    // others send only output_tokens, so don't clobber with zero
                    let c = u.counts();
                    if c.prompt > 0 {
                        self.usage.set_input(c);
                    }
                    if u.output_tokens > 0 {
                        self.usage.completion = u.output_tokens;
                    }
                }
            }
            if self.capture.wants()
                && let Some(data) = line.strip_prefix(b"data: ")
                && memmem::find(data, br#""text_delta""#).is_some()
                && let Ok(bd) = parse::<DeltaChoice>(data)
            {
                self.capture.push(bd.delta.text);
            }
            out.extend_from_slice(line);
            out.push(b'\n');
        }
        if out.len() > start {
            out.push(b'\n');
        }
    }
    fn finish(&mut self, _out: &mut Vec<u8>) {}
    fn usage(&self) -> Usage {
        self.usage
    }
    fn take_output(&mut self) -> Capture {
        std::mem::take(&mut self.capture)
    }
}

/// OpenAI SSE upstream -> Anthropic SSE client (Go: streamAnthropic).
pub struct OaiToAnthStream {
    /// message_start, sent ahead of whatever the first event produces.
    start: Option<Vec<u8>>,
    /// The open content block; -1 before the first.
    index: i64,
    in_text: bool,
    /// OpenAI tool_call index -> Anthropic block index.
    tools: Vec<(i64, i64)>,
    stop_reason: &'static str,
    usage: Usage,
    capture: Capture,
}

impl OaiToAnthStream {
    pub fn new(model: &str, req_id: &str) -> Self {
        let mut start = Vec::with_capacity(256);
        put(&mut start, "event: message_start\ndata: ");
        put(&mut start, r#"{"type":"message_start","message":{"id":"#);
        esc(&mut start, &format!("msg_{req_id}"));
        put(&mut start, r#","type":"message","role":"assistant","model":"#);
        esc(&mut start, model);
        put(&mut start, r#","content":[],"stop_reason":null,"usage":{"input_tokens":0,"output_tokens":0}}}"#);
        put(&mut start, "\n\n");
        Self {
            start: Some(start),
            index: -1,
            in_text: false,
            tools: Vec::new(),
            stop_reason: "end_turn",
            usage: Usage::default(),
            capture: Capture::default(),
        }
    }

    /// Keep up to `cap` bytes of the response text for telemetry.
    pub fn capturing(mut self, cap: usize) -> Self {
        self.capture = Capture::new(cap);
        self
    }

    fn started(&mut self, out: &mut Vec<u8>) {
        if let Some(start) = self.start.take() {
            out.extend_from_slice(&start);
        }
    }

    fn close_block(&mut self, out: &mut Vec<u8>) {
        if self.index >= 0 {
            put(out, "event: content_block_stop\ndata: ");
            put(out, r#"{"type":"content_block_stop","index":"#);
            int(out, self.index);
            put(out, "}\n\n");
        }
        self.in_text = false;
    }

    fn block_start(&mut self, out: &mut Vec<u8>) {
        self.close_block(out);
        self.index += 1;
        put(out, "event: content_block_start\ndata: ");
        put(out, r#"{"type":"content_block_start","index":"#);
        int(out, self.index);
        put(out, r#","content_block":"#);
    }

    fn data(&mut self, data: &[u8], out: &mut Vec<u8>) {
        let Ok(chunk) = parse::<OaiChunk>(data) else {
            return;
        };
        if let Some(u) = &chunk.usage {
            self.usage = u.counts();
        }
        let Some(c) = chunk.choices.first() else {
            return;
        };
        if !c.finish_reason.is_empty() {
            self.stop_reason = anth_stop_reason(c.finish_reason);
        }
        if !c.delta.content.is_empty() {
            if self.capture.wants() {
                self.capture.push(c.delta.content);
            }
            if !self.in_text {
                self.block_start(out);
                put(out, r#"{"type":"text","text":""}}"#);
                put(out, "\n\n");
                self.in_text = true;
            }
            put(out, "event: content_block_delta\ndata: ");
            put(out, r#"{"type":"content_block_delta","index":"#);
            int(out, self.index);
            put(out, r#","delta":{"type":"text_delta","text":"#);
            put(out, c.delta.content.raw());
            put(out, "}}\n\n");
        }
        for tc in &c.delta.tool_calls {
            let block = match self.tools.iter().find(|t| t.0 == tc.index) {
                Some(&(_, block)) => block,
                None => {
                    self.block_start(out);
                    put(out, r#"{"type":"tool_use","id":"#);
                    put(out, tc.id.raw());
                    put(out, r#","name":"#);
                    put(out, tc.function.name.raw());
                    put(out, r#","input":{}}}"#);
                    put(out, "\n\n");
                    self.tools.push((tc.index, self.index));
                    self.index
                }
            };
            if !tc.function.arguments.is_empty() {
                put(out, "event: content_block_delta\ndata: ");
                put(out, r#"{"type":"content_block_delta","index":"#);
                int(out, block);
                put(out, r#","delta":{"type":"input_json_delta","partial_json":"#);
                put(out, tc.function.arguments.raw());
                put(out, "}}\n\n");
            }
        }
    }
}

impl SseTransform for OaiToAnthStream {
    fn event(&mut self, ev: &[u8], out: &mut Vec<u8>) {
        self.started(out);
        for line in lines(ev) {
            if let Some(data) = line.strip_prefix(b"data: ")
                && data != b"[DONE]"
            {
                self.data(data, out);
            }
        }
    }
    fn finish(&mut self, out: &mut Vec<u8>) {
        self.started(out);
        self.close_block(out);
        put(out, "event: message_delta\ndata: ");
        put(out, r#"{"type":"message_delta","delta":{"stop_reason":""#);
        put(out, self.stop_reason);
        put(out, r#"","stop_sequence":null},"usage":{"input_tokens":"#);
        int(out, self.usage.prompt);
        put(out, r#","output_tokens":"#);
        int(out, self.usage.completion);
        put(out, "}}\n\n");
        put(out, "event: message_stop\ndata: ");
        put(out, r#"{"type":"message_stop"}"#);
        put(out, "\n\n");
    }
    fn usage(&self) -> Usage {
        self.usage
    }
    fn take_output(&mut self) -> Capture {
        std::mem::take(&mut self.capture)
    }
}

/// Anthropic SSE upstream -> OpenAI SSE client (Go: anthStreamToOAI).
pub struct AnthToOaiStream {
    /// Every chunk's opening, up to the inside of "choices".
    head: Vec<u8>,
    started: bool,
    /// The last "event:" name; like Go it carries over to later events.
    event: Event,
    finish_reason: &'static str,
    tool_index: i64,
    /// Anthropic block index -> OpenAI tool_calls index.
    tools: Vec<(i64, i64)>,
    usage: Usage,
    capture: Capture,
}

#[derive(Clone, Copy)]
enum Event {
    None,
    MessageStart,
    BlockStart,
    BlockDelta,
    MessageDelta,
    Other,
}

impl AnthToOaiStream {
    pub fn new(model: &str, req_id: &str) -> Self {
        let mut head = Vec::with_capacity(128);
        put(&mut head, r#"{"id":"#);
        esc(&mut head, &format!("chatcmpl-{req_id}"));
        put(&mut head, r#","object":"chat.completion.chunk","model":"#);
        esc(&mut head, model);
        put(&mut head, r#","choices":["#);
        Self {
            head,
            started: false,
            event: Event::None,
            finish_reason: "stop",
            tool_index: -1,
            tools: Vec::new(),
            usage: Usage::default(),
            capture: Capture::default(),
        }
    }

    /// Keep up to `cap` bytes of the response text for telemetry.
    pub fn capturing(mut self, cap: usize) -> Self {
        self.capture = Capture::new(cap);
        self
    }

    /// Writes a chunk's opening; the caller writes the delta, then `chunk_end`.
    fn chunk(&self, out: &mut Vec<u8>) {
        put(out, "data: ");
        out.extend_from_slice(&self.head);
        put(out, r#"{"index":0,"delta":"#);
    }

    fn started(&mut self, out: &mut Vec<u8>) {
        if !self.started {
            self.started = true;
            self.chunk(out);
            put(out, r#"{"role":"assistant","content":""}"#);
            chunk_end(out, None);
        }
    }

    fn data(&mut self, data: &[u8], out: &mut Vec<u8>) {
        match self.event {
            Event::MessageStart => {
                if let Ok(ev) = parse::<AnthUsageEvent>(data)
                    && let Some(u) = &ev.message.usage
                {
                    // one definition of "prompt": cache writes included
                    let c = u.counts();
                    self.usage.set_input(c);
                }
            }
            Event::BlockStart => {
                if let Ok(bs) = parse::<AnthBlockStart>(data)
                    && bs.content_block.typ.is("tool_use")
                {
                    self.tool_index += 1;
                    match self.tools.iter_mut().find(|t| t.0 == bs.index) {
                        Some(t) => t.1 = self.tool_index,
                        None => self.tools.push((bs.index, self.tool_index)),
                    }
                    self.chunk(out);
                    put(out, r#"{"tool_calls":[{"index":"#);
                    int(out, self.tool_index);
                    put(out, r#","id":"#);
                    put(out, bs.content_block.id.raw());
                    put(out, r#","type":"function","function":{"name":"#);
                    put(out, bs.content_block.name.raw());
                    put(out, r#","arguments":""}}]}"#);
                    chunk_end(out, None);
                }
            }
            Event::BlockDelta => {
                let Ok(bd) = parse::<AnthBlockDelta>(data) else {
                    return;
                };
                if bd.delta.typ.is("text_delta") {
                    if self.capture.wants() {
                        self.capture.push(bd.delta.text);
                    }
                    self.chunk(out);
                    put(out, r#"{"content":"#);
                    put(out, bd.delta.text.raw());
                    put(out, "}");
                    chunk_end(out, None);
                } else if bd.delta.typ.is("input_json_delta")
                    && let Some(&(_, tool)) = self.tools.iter().find(|t| t.0 == bd.index)
                {
                    self.chunk(out);
                    put(out, r#"{"tool_calls":[{"index":"#);
                    int(out, tool);
                    put(out, r#","function":{"arguments":"#);
                    put(out, bd.delta.partial_json.raw());
                    put(out, "}}]}");
                    chunk_end(out, None);
                }
            }
            Event::MessageDelta => {
                if let Ok(md) = parse::<AnthMessageDelta>(data) {
                    if !md.delta.stop_reason.is_empty() {
                        self.finish_reason = oai_finish_reason(md.delta.stop_reason);
                    }
                    if let Some(u) = &md.usage {
                        self.usage.completion = u.output_tokens;
                    }
                }
            }
            Event::None | Event::Other => {}
        }
    }
}

impl SseTransform for AnthToOaiStream {
    fn event(&mut self, ev: &[u8], out: &mut Vec<u8>) {
        self.started(out);
        for line in lines(ev) {
            if let Some(name) = line.strip_prefix(b"event: ") {
                self.event = match name {
                    b"" => Event::None,
                    b"message_start" => Event::MessageStart,
                    b"content_block_start" => Event::BlockStart,
                    b"content_block_delta" => Event::BlockDelta,
                    b"message_delta" => Event::MessageDelta,
                    _ => Event::Other,
                };
            } else if let Some(data) = line.strip_prefix(b"data: ") {
                self.data(data, out);
            }
        }
    }
    fn finish(&mut self, out: &mut Vec<u8>) {
        self.started(out);
        self.chunk(out);
        put(out, "{}");
        chunk_end(out, Some(self.finish_reason));
        // usage chunk in OpenAI include_usage style, then DONE
        put(out, "data: ");
        out.extend_from_slice(&self.head);
        put(out, r#"],"usage":"#);
        oai_usage(out, self.usage);
        put(out, "}\n\ndata: [DONE]\n\n");
    }
    fn usage(&self) -> Usage {
        self.usage
    }
    fn take_output(&mut self) -> Capture {
        std::mem::take(&mut self.capture)
    }
}

fn chunk_end(out: &mut Vec<u8>, finish_reason: Option<&str>) {
    put(out, r#","finish_reason":"#);
    match finish_reason {
        Some(f) => {
            put(out, "\"");
            put(out, f);
            put(out, "\"");
        }
        None => put(out, "null"),
    }
    put(out, "}]}\n\n");
}

fn anth_stop_reason(finish_reason: JStr) -> &'static str {
    match finish_reason.value().as_ref() {
        "length" => "max_tokens",
        "tool_calls" | "function_call" => "tool_use",
        _ => "end_turn",
    }
}

fn oai_finish_reason(stop_reason: JStr) -> &'static str {
    match stop_reason.value().as_ref() {
        "max_tokens" => "length",
        "tool_use" => "tool_calls",
        _ => "stop",
    }
}

fn oai_usage(o: &mut Vec<u8>, u: Usage) {
    put(o, r#"{"prompt_tokens":"#);
    int(o, u.prompt);
    put(o, r#","completion_tokens":"#);
    int(o, u.completion);
    put(o, r#","total_tokens":"#);
    int(o, u.prompt.wrapping_add(u.completion));
    put(o, "}");
}

const EMPTY: &str = r#""""#;
const ANTH_UNPARSEABLE: &str =
    r#"{"type":"error","error":{"type":"api_error","message":"unparseable upstream response"}}"#;
const OAI_UNPARSEABLE: &str =
    r#"{"error":{"message":"unparseable upstream response","type":"server_error","code":"upstream_error"}}"#;

// ---- writing ----

fn put(o: &mut Vec<u8>, s: &str) {
    o.extend_from_slice(s.as_bytes());
}

/// Writes `s` as a JSON string.
fn esc(o: &mut Vec<u8>, s: &str) {
    let _ = serde_json::to_writer(&mut *o, s);
}

fn int(o: &mut Vec<u8>, n: i64) {
    let _ = write!(o, "{n}");
}

fn num(o: &mut Vec<u8>, n: f64) {
    let _ = serde_json::to_writer(&mut *o, &n);
}

/// Separates array elements: every element starts with this, and the first
/// directly follows the '['.
fn comma(o: &mut Vec<u8>) {
    if o.last() != Some(&b'[') {
        o.push(b',');
    }
}

/// Joins JSON string literals into one literal. Escaped bodies concatenate
/// into a valid body, so nothing is unescaped; `sep` is already escaped.
fn join<'a>(mut parts: impl Iterator<Item = &'a str>, sep: &str) -> Cow<'a, str> {
    let Some(first) = parts.next() else {
        return Cow::Borrowed(EMPTY);
    };
    let Some(second) = parts.next() else {
        return Cow::Borrowed(first);
    };
    let mut s = String::with_capacity(first.len() + second.len());
    s.push_str(&first[..first.len() - 1]);
    for p in std::iter::once(second).chain(parts) {
        s.push_str(sep);
        s.push_str(&p[1..p.len() - 1]);
    }
    s.push('"');
    Cow::Owned(s)
}

/// The lines of one event, carriage returns dropped. Blank lines are skipped,
/// so `event(b"", out)` only writes what a stream opens with.
fn lines(ev: &[u8]) -> impl Iterator<Item = &[u8]> {
    ev.split(|&b| b == b'\n').map(|l| l.strip_suffix(b"\r").unwrap_or(l)).filter(|l| !l.is_empty())
}

// ---- parsing ----

/// Parses like Go's json.Unmarshal into a struct: a top-level null is the zero value.
fn parse<'a, T: Deserialize<'a> + Default>(b: &'a [u8]) -> serde_json::Result<T> {
    serde_json::from_slice::<Option<T>>(b).map(Option::unwrap_or_default)
}

/// For fields where Go would leave the zero value on null.
fn nul<'de, D: Deserializer<'de>, T: Deserialize<'de> + Default>(d: D) -> Result<T, D::Error> {
    Ok(Option::<T>::deserialize(d)?.unwrap_or_default())
}

/// For json.RawMessage fields: present-but-null stays Some("null"), as in Go.
fn raw<'de: 'a, 'a, D: Deserializer<'de>>(d: D) -> Result<Option<&'a RawValue>, D::Error> {
    <&RawValue>::deserialize(d).map(Some)
}

/// A JSON string kept as its raw literal, quotes and escapes included, so
/// text is copied through without being unescaped and escaped again. null
/// and absent read as "", like a Go string field.
#[derive(Clone, Copy, Default)]
struct JStr<'a>(Option<&'a str>);

impl<'a> JStr<'a> {
    fn raw(self) -> &'a str {
        self.0.unwrap_or(EMPTY)
    }

    fn is_empty(self) -> bool {
        self.raw() == EMPTY
    }

    /// The escaped body, without the quotes.
    fn inner(self) -> &'a str {
        let r = self.raw();
        &r[1..r.len() - 1]
    }

    /// The text. A surrogate escape without its pair, which serde_json
    /// refuses, reads as U+FFFD, as Go's encoding/json reads it.
    fn value(self) -> Cow<'a, str> {
        let inner = self.inner();
        if !inner.contains('\\') {
            return Cow::Borrowed(inner);
        }
        Cow::Owned(serde_json::from_str(self.raw()).unwrap_or_else(|_| {
            let mut s = String::with_capacity(inner.len());
            unescape_into(inner, &mut s, usize::MAX);
            s
        }))
    }

    fn is(self, s: &str) -> bool {
        self.value() == s
    }
}

/// Appends the text of an escaped JSON string body to `out`, at most `max`
/// bytes of it, cut at a character; whether any was left out. Lenient as Go's
/// encoding/json is: an unpaired surrogate escape, or any escape that isn't
/// JSON, reads as U+FFFD and the rest of the text is kept.
pub(crate) fn unescape_into(inner: &str, out: &mut String, max: usize) -> bool {
    let b = inner.as_bytes();
    let start = out.len();
    let room = |out: &String| max - (out.len() - start);
    let mut i = 0;
    while i < b.len() {
        let end = memchr::memchr(b'\\', &b[i..]).map_or(b.len(), |n| i + n);
        if end > i {
            let run = &inner[i..end];
            if run.len() > room(out) {
                out.push_str(&run[..floor_boundary(run, room(out))]);
                return true;
            }
            out.push_str(run);
            i = end;
            continue;
        }
        let (c, next) = match b.get(i + 1) {
            Some(b'"') => ('"', i + 2),
            Some(b'\\') => ('\\', i + 2),
            Some(b'/') => ('/', i + 2),
            Some(b'b') => ('\u{8}', i + 2),
            Some(b'f') => ('\u{c}', i + 2),
            Some(b'n') => ('\n', i + 2),
            Some(b'r') => ('\r', i + 2),
            Some(b't') => ('\t', i + 2),
            Some(b'u') => match hex4(b, i + 2) {
                Some(hi @ 0xd800..0xdc00) => match (b.get(i + 6..i + 8), hex4(b, i + 8)) {
                    (Some(br"\u"), Some(lo @ 0xdc00..0xe000)) => {
                        let c = char::from_u32(0x10000 + ((hi - 0xd800) << 10) + (lo - 0xdc00));
                        (c.unwrap_or(char::REPLACEMENT_CHARACTER), i + 12)
                    }
                    _ => (char::REPLACEMENT_CHARACTER, i + 6),
                },
                // a trailing surrogate on its own is no char either
                Some(u) => (char::from_u32(u).unwrap_or(char::REPLACEMENT_CHARACTER), i + 6),
                None => (char::REPLACEMENT_CHARACTER, i + 2),
            },
            // not JSON: drop the backslash, keep what follows
            _ => (char::REPLACEMENT_CHARACTER, i + 1),
        };
        if c.len_utf8() > room(out) {
            return true;
        }
        out.push(c);
        i = next;
    }
    false
}

/// Four hex digits at `at`.
fn hex4(b: &[u8], at: usize) -> Option<u32> {
    let digits = b.get(at..at + 4)?;
    digits.iter().try_fold(0u32, |n, &c| Some(n << 4 | (c as char).to_digit(16)?))
}

/// The longest prefix of `s` of at most `n` bytes that ends at a character.
fn floor_boundary(s: &str, n: usize) -> usize {
    let mut n = n.min(s.len());
    while !s.is_char_boundary(n) {
        n -= 1;
    }
    n
}

impl<'de: 'a, 'a> Deserialize<'de> for JStr<'a> {
    fn deserialize<D: Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        let raw = <&RawValue>::deserialize(d)?.get();
        match raw.as_bytes()[0] {
            b'"' => Ok(JStr(Some(raw))),
            b'n' => Ok(JStr(None)),
            _ => Err(de::Error::custom(format_args!("expected a string, found {raw:.32}"))),
        }
    }
}

/// Anthropic content: a string or blocks (Go: parseContent).
enum Content<'a> {
    Text(JStr<'a>),
    Blocks(Vec<AnthBlock<'a>>),
}

fn parse_content(raw: Option<&RawValue>) -> serde_json::Result<Content<'_>> {
    match raw.map(RawValue::get) {
        None => Ok(Content::Text(JStr(None))),
        Some(s) if s.starts_with('"') => Ok(Content::Text(JStr(Some(s)))),
        Some(s) => parse(s.as_bytes()).map(Content::Blocks),
    }
}

/// String-or-blocks content as a text literal; content that is neither comes
/// through as its JSON text (Go: flattenText).
fn flatten_text(raw: Option<&RawValue>) -> Cow<'_, str> {
    match parse_content(raw) {
        Ok(Content::Text(s)) => Cow::Borrowed(s.raw()),
        Ok(Content::Blocks(blocks)) => join(blocks.iter().filter(|b| b.typ.is("text")).map(|b| b.text.raw()), ""),
        Err(_) => Cow::Owned(serde_json::to_string(raw.map_or("", RawValue::get)).unwrap_or_default()),
    }
}

#[derive(Deserialize, Default)]
struct AnthRequest<'a> {
    #[serde(default, borrow, rename = "model")]
    _model: JStr<'a>,
    #[serde(default, deserialize_with = "nul")]
    max_tokens: i64,
    #[serde(default, borrow, deserialize_with = "raw")]
    system: Option<&'a RawValue>,
    #[serde(default, borrow, deserialize_with = "nul")]
    messages: Vec<AnthMessage<'a>>,
    #[serde(default, borrow, deserialize_with = "nul")]
    tools: Vec<AnthTool<'a>>,
    #[serde(default, borrow, deserialize_with = "raw")]
    tool_choice: Option<&'a RawValue>,
    temperature: Option<f64>,
    top_p: Option<f64>,
    #[serde(default, borrow, deserialize_with = "nul")]
    stop_sequences: Vec<JStr<'a>>,
    #[serde(default, deserialize_with = "nul")]
    stream: bool,
}

#[derive(Deserialize, Default)]
struct AnthMessage<'a> {
    #[serde(default, borrow)]
    role: JStr<'a>,
    #[serde(default, borrow, deserialize_with = "raw")]
    content: Option<&'a RawValue>,
}

#[derive(Deserialize, Default)]
struct AnthBlock<'a> {
    #[serde(default, borrow, rename = "type")]
    typ: JStr<'a>,
    #[serde(default, borrow)]
    text: JStr<'a>,
    #[serde(default, borrow)]
    id: JStr<'a>,
    #[serde(default, borrow)]
    name: JStr<'a>,
    #[serde(default, borrow, deserialize_with = "raw")]
    input: Option<&'a RawValue>,
    #[serde(default, borrow)]
    tool_use_id: JStr<'a>,
    #[serde(default, borrow, deserialize_with = "raw")]
    content: Option<&'a RawValue>,
    #[serde(default, rename = "is_error", deserialize_with = "nul")]
    _is_error: bool,
    #[serde(default, borrow)]
    source: Option<AnthImageSource<'a>>,
}

#[derive(Deserialize, Default)]
struct AnthImageSource<'a> {
    #[serde(default, borrow, rename = "type")]
    typ: JStr<'a>,
    #[serde(default, borrow)]
    media_type: JStr<'a>,
    #[serde(default, borrow)]
    data: JStr<'a>,
}

#[derive(Deserialize, Default)]
struct AnthTool<'a> {
    #[serde(default, borrow)]
    name: JStr<'a>,
    #[serde(default, borrow)]
    description: JStr<'a>,
    #[serde(default, borrow, deserialize_with = "raw")]
    input_schema: Option<&'a RawValue>,
}

#[derive(Deserialize, Default)]
struct AnthToolChoice<'a> {
    #[serde(default, borrow, rename = "type")]
    typ: JStr<'a>,
    #[serde(default, borrow)]
    name: JStr<'a>,
}

#[derive(Deserialize, Default)]
struct AnthUsage {
    #[serde(default, deserialize_with = "nul")]
    input_tokens: i64,
    #[serde(default, deserialize_with = "nul")]
    output_tokens: i64,
    #[serde(default, deserialize_with = "nul")]
    cache_read_input_tokens: i64,
    #[serde(default, deserialize_with = "nul")]
    cache_creation_input_tokens: i64,
    #[serde(default)]
    cache_creation: Option<CacheCreation>,
    #[serde(default)]
    inference_geo: Tag,
    #[serde(default)]
    speed: Tag,
}

#[derive(Deserialize, Default)]
struct CacheCreation {
    #[serde(default, deserialize_with = "nul")]
    ephemeral_1h_input_tokens: i64,
}

impl AnthUsage {
    /// Both cache buckets are prompt tokens the upstream read; only reads get
    /// the cached price. A write without the 5m/1h breakdown is the default 5
    /// minute kind (Go: anthUsage.counts).
    fn counts(&self) -> Usage {
        let write_1h = self.cache_creation.as_ref().map_or(0, |c| c.ephemeral_1h_input_tokens);
        Usage {
            prompt: self
                .input_tokens
                .wrapping_add(self.cache_read_input_tokens)
                .wrapping_add(self.cache_creation_input_tokens),
            completion: self.output_tokens,
            cached: self.cache_read_input_tokens,
            write_5m: self.cache_creation_input_tokens.wrapping_sub(write_1h),
            write_1h,
            us_inference: self.inference_geo.as_str() == "us",
            fast: self.speed.as_str() == "fast",
            geo: self.inference_geo,
            speed: self.speed,
        }
    }
}

#[derive(Deserialize, Default)]
struct AnthResponse<'a> {
    #[serde(default, borrow, deserialize_with = "nul")]
    content: Vec<AnthBlock<'a>>,
    #[serde(default, borrow)]
    stop_reason: JStr<'a>,
    usage: Option<AnthUsage>,
}

/// What passthroughAnthJSON parses: it validates content, so bad blocks lose the usage.
#[derive(Deserialize, Default)]
struct AnthJson<'a> {
    #[serde(default, borrow, rename = "content", deserialize_with = "nul")]
    _content: Vec<AnthBlock<'a>>,
    usage: Option<AnthUsage>,
}

/// message_start carries usage in "message", message_delta at the top.
#[derive(Deserialize, Default)]
struct AnthUsageEvent {
    #[serde(default, deserialize_with = "nul")]
    message: AnthUsageHolder,
    usage: Option<AnthUsage>,
}

#[derive(Deserialize, Default)]
struct AnthUsageHolder {
    usage: Option<AnthUsage>,
}

#[derive(Deserialize, Default)]
struct AnthBlockStart<'a> {
    #[serde(default, deserialize_with = "nul")]
    index: i64,
    #[serde(default, borrow, deserialize_with = "nul")]
    content_block: AnthBlock<'a>,
}

#[derive(Deserialize, Default)]
struct AnthBlockDelta<'a> {
    #[serde(default, deserialize_with = "nul")]
    index: i64,
    #[serde(default, borrow, deserialize_with = "nul")]
    delta: AnthDelta<'a>,
}

#[derive(Deserialize, Default)]
struct AnthDelta<'a> {
    #[serde(default, borrow, rename = "type")]
    typ: JStr<'a>,
    #[serde(default, borrow)]
    text: JStr<'a>,
    #[serde(default, borrow)]
    partial_json: JStr<'a>,
}

#[derive(Deserialize, Default)]
struct AnthMessageDelta<'a> {
    #[serde(default, borrow, deserialize_with = "nul")]
    delta: AnthStop<'a>,
    usage: Option<AnthUsage>,
}

#[derive(Deserialize, Default)]
struct AnthStop<'a> {
    #[serde(default, borrow)]
    stop_reason: JStr<'a>,
}

#[derive(Deserialize, Default)]
struct OaiRequest<'a> {
    #[serde(default, borrow, rename = "model")]
    _model: JStr<'a>,
    /// Go reads it leniently: anything but true is false.
    #[serde(default, borrow, deserialize_with = "raw")]
    stream: Option<&'a RawValue>,
    #[serde(default, borrow, deserialize_with = "nul")]
    messages: Vec<OaiMessage<'a>>,
    #[serde(default, borrow, deserialize_with = "nul")]
    tools: Vec<OaiTool<'a>>,
    #[serde(default, borrow, deserialize_with = "raw")]
    tool_choice: Option<&'a RawValue>,
    #[serde(default, deserialize_with = "nul")]
    max_tokens: i64,
    #[serde(default, deserialize_with = "nul")]
    max_completion_tokens: i64,
    temperature: Option<f64>,
    top_p: Option<f64>,
    #[serde(default, borrow, deserialize_with = "raw")]
    stop: Option<&'a RawValue>,
}

#[derive(Deserialize, Default)]
struct OaiMessage<'a> {
    #[serde(default, borrow)]
    role: JStr<'a>,
    #[serde(default, borrow, deserialize_with = "raw")]
    content: Option<&'a RawValue>,
    #[serde(default, borrow, deserialize_with = "nul")]
    tool_calls: Vec<OaiToolCall<'a>>,
    #[serde(default, borrow)]
    tool_call_id: JStr<'a>,
}

#[derive(Deserialize, Default)]
struct OaiToolCall<'a> {
    #[serde(default, borrow)]
    id: JStr<'a>,
    #[serde(default, borrow, rename = "type")]
    _typ: JStr<'a>,
    #[serde(default, borrow, deserialize_with = "nul")]
    function: OaiFunction<'a>,
}

#[derive(Deserialize, Default)]
struct OaiFunction<'a> {
    #[serde(default, borrow)]
    name: JStr<'a>,
    #[serde(default, borrow)]
    arguments: JStr<'a>,
}

#[derive(Deserialize, Default)]
struct OaiTool<'a> {
    #[serde(default, borrow, deserialize_with = "nul")]
    function: OaiToolFunction<'a>,
}

#[derive(Deserialize, Default)]
struct OaiToolFunction<'a> {
    #[serde(default, borrow)]
    name: JStr<'a>,
    #[serde(default, borrow)]
    description: JStr<'a>,
    #[serde(default, borrow, deserialize_with = "raw")]
    parameters: Option<&'a RawValue>,
}

#[derive(Deserialize, Default)]
struct OaiToolChoice<'a> {
    #[serde(default, borrow, deserialize_with = "nul")]
    function: OaiFunctionName<'a>,
}

#[derive(Deserialize, Default)]
struct OaiFunctionName<'a> {
    #[serde(default, borrow)]
    name: JStr<'a>,
}

/// A content part as oaiContentText reads it.
#[derive(Deserialize, Default)]
struct TextPart<'a> {
    #[serde(default, borrow, rename = "type")]
    typ: JStr<'a>,
    #[serde(default, borrow)]
    text: JStr<'a>,
}

/// A content part as oaiContentBlocks reads it.
#[derive(Deserialize, Default)]
struct OaiPart<'a> {
    #[serde(default, borrow, rename = "type")]
    typ: JStr<'a>,
    #[serde(default, borrow)]
    text: JStr<'a>,
    #[serde(default, borrow, deserialize_with = "nul")]
    image_url: OaiImageUrl<'a>,
}

#[derive(Deserialize, Default)]
struct OaiImageUrl<'a> {
    #[serde(default, borrow)]
    url: JStr<'a>,
}

#[derive(Deserialize, Default)]
struct OaiUsage {
    #[serde(default, deserialize_with = "nul")]
    prompt_tokens: i64,
    #[serde(default, deserialize_with = "nul")]
    completion_tokens: i64,
    #[serde(default, deserialize_with = "nul")]
    prompt_tokens_details: OaiUsageDetails,
}

#[derive(Deserialize, Default)]
struct OaiUsageDetails {
    #[serde(default, deserialize_with = "nul")]
    cached_tokens: i64,
}

impl OaiUsage {
    fn counts(&self) -> Usage {
        Usage {
            prompt: self.prompt_tokens,
            completion: self.completion_tokens,
            cached: self.prompt_tokens_details.cached_tokens,
            ..Usage::default()
        }
    }
}

#[derive(Deserialize, Default)]
struct OaiResponse<'a> {
    #[serde(default, borrow, deserialize_with = "nul")]
    choices: Vec<OaiChoice<'a>>,
    usage: Option<OaiUsage>,
}

#[derive(Deserialize, Default)]
struct OaiChoice<'a> {
    #[serde(default, borrow, deserialize_with = "nul")]
    message: OaiRespMessage<'a>,
    #[serde(default, borrow)]
    finish_reason: JStr<'a>,
}

#[derive(Deserialize, Default)]
struct OaiRespMessage<'a> {
    #[serde(default, borrow)]
    content: JStr<'a>,
    #[serde(default, borrow, deserialize_with = "nul")]
    tool_calls: Vec<OaiToolCall<'a>>,
}

/// What copyJSON parses: it validates message content, so a non-string
/// content loses the usage.
#[derive(Deserialize, Default)]
struct OaiJson<'a> {
    usage: Option<OaiUsage>,
    #[serde(default, borrow, rename = "choices", deserialize_with = "nul")]
    _choices: Vec<OaiJsonChoice<'a>>,
}

#[derive(Deserialize, Default)]
struct OaiJsonChoice<'a> {
    #[serde(default, borrow, rename = "message", deserialize_with = "nul")]
    _message: OaiJsonMessage<'a>,
}

#[derive(Deserialize, Default)]
struct OaiJsonMessage<'a> {
    #[serde(default, borrow, rename = "content")]
    _content: JStr<'a>,
}

#[derive(Deserialize, Default)]
struct OaiChunk<'a> {
    #[serde(default, borrow, deserialize_with = "nul")]
    choices: Vec<OaiChunkChoice<'a>>,
    usage: Option<OaiUsage>,
}

#[derive(Deserialize, Default)]
struct OaiChunkChoice<'a> {
    #[serde(default, borrow, deserialize_with = "nul")]
    delta: OaiDelta<'a>,
    #[serde(default, borrow)]
    finish_reason: JStr<'a>,
}

#[derive(Deserialize, Default)]
struct OaiDelta<'a> {
    #[serde(default, borrow)]
    content: JStr<'a>,
    #[serde(default, borrow, deserialize_with = "nul")]
    tool_calls: Vec<OaiToolDelta<'a>>,
}

#[derive(Deserialize, Default)]
struct OaiToolDelta<'a> {
    #[serde(default, deserialize_with = "nul")]
    index: i64,
    #[serde(default, borrow)]
    id: JStr<'a>,
    #[serde(default, borrow, deserialize_with = "nul")]
    function: OaiFunction<'a>,
}

/// The text of a streamed delta, as the Go gateway captures it: an OpenAI
/// chunk's choices[].delta.content, an Anthropic text_delta's delta.text.
#[derive(Deserialize, Default)]
struct DeltaChunk<'a> {
    #[serde(default, borrow, deserialize_with = "nul")]
    choices: Vec<DeltaChoice<'a>>,
}

#[derive(Deserialize, Default)]
struct DeltaChoice<'a> {
    #[serde(default, borrow, deserialize_with = "nul")]
    delta: DeltaText<'a>,
}

#[derive(Deserialize, Default)]
struct DeltaText<'a> {
    #[serde(default, borrow)]
    content: JStr<'a>,
    #[serde(default, borrow)]
    text: JStr<'a>,
}

#[derive(Deserialize, Default)]
struct UsageChunk {
    usage: Option<OaiUsage>,
    #[serde(default, deserialize_with = "nul")]
    choices: Vec<IgnoredAny>,
}

#[cfg(test)]
mod tests {
    use super::*;

    fn jstr(raw: &str) -> String {
        JStr(Some(raw)).value().into_owned()
    }

    /// What serde_json reads, it reads the same; what it refuses for an
    /// unpaired surrogate, it reads as Go does, U+FFFD in its place.
    #[test]
    fn unescapes_like_go() {
        for s in ["", "plain", "a\"b\\c/d\u{8}\u{c}\n\r\t", "\u{1}\u{1f}é😀\u{ffff}", "x\u{7f}y", "\\u0041"] {
            let raw = serde_json::to_string(s).unwrap();
            assert_eq!(jstr(&raw), s, "{raw}");
            let mut out = String::new();
            assert!(!unescape_into(&raw[1..raw.len() - 1], &mut out, usize::MAX));
            assert_eq!(out, s, "{raw}");
        }
        let mut out = String::new();
        unescape_into(r"é😀\/", &mut out, usize::MAX);
        assert_eq!(out, "é😀/");
        assert_eq!(jstr(r#""a\ud83d""#), "a\u{fffd}");
        assert_eq!(jstr(r#""\ude00 tail""#), "\u{fffd} tail");
        assert_eq!(jstr(r#""\ud83d😀!""#), "\u{fffd}😀!");
        assert_eq!(jstr(r#""\ud83dx\n""#), "\u{fffd}x\n");
        // not JSON at all, but never a panic
        assert_eq!(jstr(r#""\q\u12""#), "\u{fffd}q\u{fffd}12");
        let mut out = String::new();
        unescape_into("é\\", &mut out, usize::MAX);
        assert_eq!(out, "é\u{fffd}");
    }

    /// A surrogate pair split across two deltas: each delta keeps its text.
    #[test]
    fn a_split_surrogate_keeps_the_delta() {
        let mut t = OaiPassthrough::new(false).capturing(100);
        let mut out = Vec::new();
        for d in [r"a\ud83d", r"\ude00 tail"] {
            t.event(format!(r#"data: {{"choices":[{{"delta":{{"content":"{d}"}}}}]}}"#).as_bytes(), &mut out);
        }
        assert_eq!(t.take_output().into_parts(), ("a\u{fffd}\u{fffd} tail".to_string(), false));
    }

    fn run(t: &mut dyn SseTransform, events: &[String]) -> (String, bool) {
        let mut out = Vec::new();
        for e in events {
            t.event(e.as_bytes(), &mut out);
        }
        t.finish(&mut out);
        t.take_output().into_parts()
    }

    fn oai_chunk(content: &str) -> String {
        format!(r#"data: {{"choices":[{{"index":0,"delta":{{"content":{content}}}}}]}}"#)
    }

    fn anth_delta(index: usize, delta: &str) -> String {
        format!(
            "event: content_block_delta\ndata: {{\"type\":\"content_block_delta\",\"index\":{index},\"delta\":{delta}}}"
        )
    }

    /// Every converter captures the response text, unescaped, non-ASCII
    /// intact, and nothing of tool calls, thinking or a second choice.
    #[test]
    fn converters_capture_the_text() {
        let want = "a\"b\n\u{e9}😀 ü tail";
        let oai: Vec<String> = vec![
            oai_chunk(r#""a\"b\n\u00e9\ud83d\ude00""#),
            oai_chunk(r#"" ü""#),
            r#"data: {"choices":[{"index":0,"delta":{"content":null,"tool_calls":[{"index":0,"id":"t","function":{"name":"f","arguments":"{\"content\":1}"}}]}}]}"#.into(),
            oai_chunk(r#"" tail""#),
            r#"data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":7}}"#.into(),
            "data: [DONE]".into(),
        ];
        let anth: Vec<String> = vec![
            anth_delta(0, r#"{"type":"thinking_delta","thinking":"hmm"}"#),
            anth_delta(1, r#"{"type":"text_delta","text":"a\"b\n\u00e9\ud83d\ude00"}"#),
            anth_delta(1, r#"{"type":"text_delta","text":" ü"}"#),
            anth_delta(2, r#"{"type":"input_json_delta","partial_json":"{\"text_delta\":1}"}"#),
            anth_delta(1, r#"{"type":"text_delta","text":" tail"}"#),
        ];
        let full = |t: &mut dyn SseTransform, evs: &[String]| run(t, evs);
        assert_eq!(full(&mut OaiPassthrough::new(true).capturing(1000), &oai), (want.into(), false));
        assert_eq!(full(&mut OaiToAnthStream::new("m", "r").capturing(1000), &oai), (want.into(), false));
        assert_eq!(full(&mut AnthPassthrough::new().capturing(1000), &anth), (want.into(), false));
        assert_eq!(full(&mut AnthToOaiStream::new("m", "r").capturing(1000), &anth), (want.into(), false));

        // cut at the cap, at a character: "a\"b\n" is 4 bytes, é 2
        let cut = ("a\"b\n".to_string(), true);
        assert_eq!(full(&mut OaiPassthrough::new(true).capturing(5), &oai), cut);
        assert_eq!(full(&mut OaiToAnthStream::new("m", "r").capturing(5), &oai), cut);
        assert_eq!(full(&mut AnthPassthrough::new().capturing(5), &anth), cut);
        assert_eq!(full(&mut AnthToOaiStream::new("m", "r").capturing(5), &anth), cut);
        // not asked for: nothing
        assert_eq!(full(&mut OaiPassthrough::new(true), &oai), (String::new(), false));
        assert_eq!(full(&mut AnthPassthrough::new(), &anth), (String::new(), false));

        // A response that only calls tools has no text to capture.
        let tools_only = [r#"data: {"choices":[{"index":0,"delta":{"role":"assistant","content":null,"tool_calls":[{"index":0,"id":"t","type":"function","function":{"name":"f","arguments":"{\"content\":\"x\"}"}}]}}]}"#.to_string()];
        assert_eq!(full(&mut OaiPassthrough::new(false).capturing(100), &tools_only), (String::new(), false));
        assert_eq!(full(&mut OaiToAnthStream::new("m", "r").capturing(100), &tools_only), (String::new(), false));
        let tools_only = [anth_delta(0, r#"{"type":"input_json_delta","partial_json":"{\"text\":\"x\"}"}"#)];
        assert_eq!(full(&mut AnthPassthrough::new().capturing(100), &tools_only), (String::new(), false));
        assert_eq!(full(&mut AnthToOaiStream::new("m", "r").capturing(100), &tools_only), (String::new(), false));
    }

    #[test]
    fn json_responses_capture_the_text() {
        let oai = br#"{"choices":[{"message":{"content":"x\u00e9\ud83d\ude00 \"q\""}}]}"#;
        assert_eq!(oai_json_text(oai, 100).into_parts(), ("xé😀 \"q\"".into(), false));
        assert_eq!(oai_json_text(oai, 2).into_parts(), ("x".into(), true));
        let tools = br#"{"choices":[{"message":{"content":null,"tool_calls":[{"id":"t","function":{"name":"f","arguments":"{}"}}]}}]}"#;
        assert_eq!(oai_json_text(tools, 100).into_parts(), (String::new(), false));
        let anth = r#"{"content":[{"type":"thinking","thinking":"hm"},{"type":"text","text":"x\u00e9"},
            {"type":"tool_use","id":"t","name":"f","input":{"text":"in"}},{"type":"text","text":"ü"}]}"#;
        let anth = anth.as_bytes();
        assert_eq!(anth_json_text(anth, 100).into_parts(), ("xéü".into(), false));
        assert_eq!(anth_json_text(anth, 3).into_parts(), ("xé".into(), true));
        assert_eq!(anth_json_text(anth, 2).into_parts(), ("x".into(), true));
        let tools = br#"{"content":[{"type":"tool_use","id":"t","name":"f","input":{"text":"in"}}]}"#;
        assert_eq!(anth_json_text(tools, 100).into_parts(), (String::new(), false));
        assert_eq!(oai_json_text(b"not json", 100).into_parts(), (String::new(), false));
    }

    /// However long the response, what is kept, and the memory it holds,
    /// stays within the cap.
    #[test]
    fn captures_never_hold_more_than_the_cap() {
        let cap = 10_000;
        let mut t = OaiPassthrough::new(false).capturing(cap);
        let mut out = Vec::new();
        for _ in 0..5_000 {
            t.event(oai_chunk(r#""0123456789\n\u00e9""#).as_bytes(), &mut out);
            assert!(t.capture.text.capacity() <= cap, "{}", t.capture.text.capacity());
        }
        let (text, cut) = t.take_output().into_parts();
        assert!(cut && text.len() <= cap && text.len() > cap - 4, "{}", text.len());
        assert!(text.capacity() <= cap);

        let long = format!(r#"{{"choices":[{{"message":{{"content":"{}"}}}}]}}"#, "y".repeat(2 << 20));
        let (text, cut) = oai_json_text(long.as_bytes(), 64 << 10).into_parts();
        assert!(cut && text.len() == 64 << 10 && text.capacity() == 64 << 10);
    }

    #[test]
    fn unescapes_within_a_limit() {
        let mut out = String::from("ab");
        assert!(unescape_into(r"cé\n", &mut out, 2));
        assert_eq!(out, "abc", "é doesn't fit in what is left");
        let mut out = String::new();
        assert!(unescape_into(r"\n😀", &mut out, 4));
        assert_eq!(out, "\n");
        let mut out = String::new();
        assert!(!unescape_into(r"\n😀", &mut out, 5));
        assert_eq!(out, "\n😀");
    }
}
