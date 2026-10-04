#!/usr/bin/env python3
"""Fake Anthropic Messages API for local testing (the `foundry-stub` provider).

    python3 scripts/anthropic-stub.py [port]     # default 9901

Answers POST */v1/messages (any prefix, e.g. /anthropic/v1/messages) with a
canned reply, streaming SSE when "stream": true. No dependencies.
"""
import json
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

REPLY = "Hello from the claude stub! I echo so the gateway pipeline can be tested end to end."


def sse(event, data):
    return f"event: {event}\ndata: {json.dumps(data)}\n\n".encode()


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        if not self.path.endswith("/v1/messages"):
            self.send_error(404)
            return
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0)) or 0) or b"{}")
        model = body.get("model", "claude-fake-1")
        usage = {"input_tokens": 25, "output_tokens": len(REPLY.split())}

        if body.get("stream"):
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            mid = "msg_stub_1"
            self.wfile.write(sse("message_start", {"type": "message_start", "message": {
                "id": mid, "type": "message", "role": "assistant", "model": model,
                "content": [], "usage": {"input_tokens": usage["input_tokens"], "output_tokens": 0}}}))
            self.wfile.write(sse("content_block_start", {"type": "content_block_start", "index": 0,
                                                         "content_block": {"type": "text", "text": ""}}))
            for word in REPLY.split(" "):
                self.wfile.write(sse("content_block_delta", {"type": "content_block_delta", "index": 0,
                                                             "delta": {"type": "text_delta", "text": word + " "}}))
                self.wfile.flush()
                time.sleep(0.02)
            self.wfile.write(sse("content_block_stop", {"type": "content_block_stop", "index": 0}))
            self.wfile.write(sse("message_delta", {"type": "message_delta",
                                                   "delta": {"stop_reason": "end_turn", "stop_sequence": None},
                                                   "usage": {"output_tokens": usage["output_tokens"]}}))
            self.wfile.write(sse("message_stop", {"type": "message_stop"}))
        else:
            out = json.dumps({"id": "msg_stub_1", "type": "message", "role": "assistant", "model": model,
                              "content": [{"type": "text", "text": REPLY}],
                              "stop_reason": "end_turn", "stop_sequence": None, "usage": usage}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(out)))
            self.end_headers()
            self.wfile.write(out)

    def log_message(self, fmt, *args):
        sys.stderr.write("stub: %s\n" % (fmt % args))


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 9901
    print(f"anthropic stub listening on :{port}")
    ThreadingHTTPServer(("", port), Handler).serve_forever()
