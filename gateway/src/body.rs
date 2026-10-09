//! Response bodies: whole buffers, or an upstream event stream run through a
//! translator. A body holds its request's in-flight slot until it is done, and
//! a stream emits its usage event when it ends or the client goes away.

use std::pin::Pin;
use std::task::{Context, Poll, ready};
use std::time::Instant;

use bytes::Bytes;
use http_body::{Body as HttpBody, Frame, SizeHint};
use hyper::body::Incoming;
use memchr::memchr;

use crate::metrics::IN_FLIGHT;
use crate::translate::SseTransform;
use crate::usage::Emit;

pub type Error = Box<dyn std::error::Error + Send + Sync>;

/// The largest upstream event the gateway will hold while waiting for its end.
const MAX_EVENT: usize = 10 << 20;

/// Counts a request as in flight until dropped.
pub struct InFlight(());

impl InFlight {
    /// Takes a slot and returns how many are now taken.
    pub fn take() -> (InFlight, i64) {
        let n = IN_FLIGHT.fetch_add(1, std::sync::atomic::Ordering::Relaxed) + 1;
        (InFlight(()), n)
    }
}

impl Drop for InFlight {
    fn drop(&mut self) {
        IN_FLIGHT.fetch_sub(1, std::sync::atomic::Ordering::Relaxed);
    }
}

pub struct Body {
    kind: Kind,
    _in_flight: Option<InFlight>,
}

enum Kind {
    Full(Option<Bytes>),
    Sse(Box<Sse>),
}

impl Body {
    pub fn full(b: impl Into<Bytes>) -> Body {
        Body { kind: Kind::Full(Some(b.into())), _in_flight: None }
    }

    pub fn empty() -> Body {
        Body { kind: Kind::Full(None), _in_flight: None }
    }

    pub fn sse(s: Sse) -> Body {
        Body { kind: Kind::Sse(Box::new(s)), _in_flight: None }
    }

    pub fn holding(mut self, slot: InFlight) -> Body {
        self._in_flight = Some(slot);
        self
    }
}

impl HttpBody for Body {
    type Data = Bytes;
    type Error = Error;

    fn poll_frame(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Option<Result<Frame<Bytes>, Error>>> {
        match &mut self.get_mut().kind {
            Kind::Full(b) => Poll::Ready(b.take().map(|b| Ok(Frame::data(b)))),
            Kind::Sse(s) => s.poll(cx),
        }
    }

    fn is_end_stream(&self) -> bool {
        matches!(self.kind, Kind::Full(None))
    }

    fn size_hint(&self) -> SizeHint {
        match &self.kind {
            Kind::Full(Some(b)) => SizeHint::with_exact(b.len() as u64),
            Kind::Full(None) => SizeHint::with_exact(0),
            Kind::Sse(_) => SizeHint::default(),
        }
    }
}

/// An upstream event stream, split into events and fed to a transform.
pub struct Sse {
    upstream: Incoming,
    transform: Box<dyn SseTransform>,
    carry: Vec<u8>,
    out: Vec<u8>,
    emit: Option<Emit>,
    start: Instant,
    ttft_ms: i64,
    ended: bool,
}

impl Sse {
    pub fn new(upstream: Incoming, transform: Box<dyn SseTransform>, emit: Emit) -> Sse {
        let start = emit.start;
        Sse {
            upstream,
            transform,
            carry: Vec::new(),
            out: Vec::new(),
            emit: Some(emit),
            start,
            ttft_ms: 0,
            ended: false,
        }
    }

    fn poll(&mut self, cx: &mut Context<'_>) -> Poll<Option<Result<Frame<Bytes>, Error>>> {
        loop {
            if self.ended {
                return Poll::Ready(None);
            }
            match ready!(Pin::new(&mut self.upstream).poll_frame(cx)) {
                Some(Ok(frame)) => {
                    let Ok(data) = frame.into_data() else { continue };
                    if self.ttft_ms == 0 {
                        self.ttft_ms = (self.start.elapsed().as_millis() as i64).max(1);
                    }
                    self.carry.extend_from_slice(&data);
                    self.split();
                    if self.carry.len() > MAX_EVENT {
                        self.ended = true;
                        self.finish_usage();
                        return Poll::Ready(Some(Err("upstream event exceeds 10MB".into())));
                    }
                    if !self.out.is_empty() {
                        return Poll::Ready(Some(Ok(Frame::data(Bytes::from(std::mem::take(&mut self.out))))));
                    }
                }
                Some(Err(e)) => {
                    self.ended = true;
                    self.finish_usage();
                    return Poll::Ready(Some(Err(e.into())));
                }
                None => {
                    self.ended = true;
                    // a last event the upstream didn't close with a blank line
                    let rest = std::mem::take(&mut self.carry);
                    let rest = trim_newlines(&rest);
                    if !rest.is_empty() {
                        feed(&mut *self.transform, rest, &mut self.out);
                    }
                    self.transform.finish(&mut self.out);
                    self.finish_usage();
                    if !self.out.is_empty() {
                        return Poll::Ready(Some(Ok(Frame::data(Bytes::from(std::mem::take(&mut self.out))))));
                    }
                    return Poll::Ready(None);
                }
            }
        }
    }

    /// Feed every complete event in `carry` to the transform. An event ends at
    /// an empty line; "\r\n" line endings count as "\n".
    fn split(&mut self) {
        let (carry, transform, out) = (&self.carry, &mut self.transform, &mut self.out);
        let mut start = 0;
        let mut pos = 0;
        while let Some(nl) = memchr(b'\n', &carry[pos..]) {
            let end = pos + nl;
            let line = &carry[pos..end];
            if line.is_empty() || line == b"\r" {
                let ev = trim_newlines(&carry[start..pos]);
                if !ev.is_empty() {
                    feed(&mut **transform, ev, out);
                }
                start = end + 1;
            }
            pos = end + 1;
        }
        self.carry.drain(..start);
    }

    fn finish_usage(&mut self) {
        if let Some(e) = self.emit.take() {
            e.send(200, "", self.transform.usage(), self.ttft_ms);
        }
    }
}

impl Drop for Sse {
    // The client went away mid-stream: dropping the upstream body ends the
    // upstream call, and the request still counts what it used.
    fn drop(&mut self) {
        self.finish_usage();
    }
}

fn feed(t: &mut dyn SseTransform, ev: &[u8], out: &mut Vec<u8>) {
    if memchr(b'\r', ev).is_some() {
        let clean: Vec<u8> = ev.iter().copied().filter(|&c| c != b'\r').collect();
        t.event(&clean, out);
    } else {
        t.event(ev, out);
    }
}

fn trim_newlines(mut b: &[u8]) -> &[u8] {
    while let [rest @ .., b'\n' | b'\r'] = b {
        b = rest;
    }
    b
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn trims() {
        assert_eq!(trim_newlines(b"data: x\r\n"), b"data: x");
        assert_eq!(trim_newlines(b"\n\n"), b"");
    }
}
