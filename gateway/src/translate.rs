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

/// A streaming converter, fed one event at a time.
pub trait SseTransform: Send {
    /// One upstream event (lines joined by "\n", no trailing blank line).
    fn event(&mut self, ev: &[u8], out: &mut Vec<u8>);
    /// The upstream stream ended; write whatever closes the client's stream.
    fn finish(&mut self, out: &mut Vec<u8>);
    /// Usage seen so far.
    fn usage(&self) -> Usage;
}

/// OpenAI SSE passthrough that reads usage (Go: streamSSE). With
/// `drop_usage_chunk`, the usage-only chunk the gateway asked for itself is
/// not forwarded.
pub struct OaiPassthrough {
    drop_usage_chunk: bool,
    usage: Usage,
}

impl OaiPassthrough {
    pub fn new(drop_usage_chunk: bool) -> Self {
        Self { drop_usage_chunk, usage: Usage::default() }
    }
}

impl SseTransform for OaiPassthrough {
    fn event(&mut self, ev: &[u8], out: &mut Vec<u8>) {
        let start = out.len();
        for line in lines(ev) {
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
}

/// Anthropic SSE passthrough that reads usage (Go: streamAnthPassthrough).
pub struct AnthPassthrough {
    usage: Usage,
}

impl AnthPassthrough {
    pub fn new() -> Self {
        Self { usage: Usage::default() }
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
                    (self.usage.prompt, self.usage.cached) = (c.prompt, c.cached);
                }
                if let Some(u) = &ev.usage {
                    // message_delta: some upstreams repeat the input side here,
                    // others send only output_tokens, so don't clobber with zero
                    let c = u.counts();
                    if c.prompt > 0 {
                        (self.usage.prompt, self.usage.cached) = (c.prompt, c.cached);
                    }
                    if u.output_tokens > 0 {
                        self.usage.completion = u.output_tokens;
                    }
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
        }
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
        }
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
                    (self.usage.prompt, self.usage.cached) = (c.prompt, c.cached);
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

    fn value(self) -> Cow<'a, str> {
        let inner = self.inner();
        if inner.contains('\\') {
            Cow::Owned(serde_json::from_str(self.raw()).unwrap_or_default())
        } else {
            Cow::Borrowed(inner)
        }
    }

    fn is(self, s: &str) -> bool {
        self.value() == s
    }
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
}

impl AnthUsage {
    /// Both cache buckets are prompt tokens the upstream read; only reads get
    /// the cached price (Go: anthUsage.counts).
    fn counts(&self) -> Usage {
        Usage {
            prompt: self
                .input_tokens
                .wrapping_add(self.cache_read_input_tokens)
                .wrapping_add(self.cache_creation_input_tokens),
            completion: self.output_tokens,
            cached: self.cache_read_input_tokens,
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

#[derive(Deserialize, Default)]
struct UsageChunk {
    usage: Option<OaiUsage>,
    #[serde(default, deserialize_with = "nul")]
    choices: Vec<IgnoredAny>,
}
