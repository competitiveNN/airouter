#!/usr/bin/env python3
"""A minimal, deterministic SSE upstream for testing the gateway.

CI cannot reach real providers, but the SSE termination contract the gateway
must uphold (one `data: [DONE]`, last, no leaked comment lines) is decided
entirely by how the gateway frames the stream — not by which model answered.
So the contract is fully testable against a fake that emits a known-good
stream, and the fake is deliberately nastier than a real provider:

  * `: keepalive` comment lines are interleaved *between* data chunks, which
    is the exact input that used to make the gateway release its buffer
    early and emit a stream starting with `[DONE]`
  * one final data event is sent with no trailing blank line, covering the
    "upstream closed without a final delimiter" path
  * a role-only delta precedes the content, so the release heuristic cannot
    pass trivially on the first event

Usage:
    python3 scripts/fake-sse-upstream.py --port 9099
"""

from __future__ import annotations

import argparse
import json
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

CHUNKS = ("PING", " OK")


def event(payload: dict) -> bytes:
    return b"data: " + json.dumps(payload).encode() + b"\n\n"


def chunk(model: str, delta: dict, finish: str | None = None) -> bytes:
    return event(
        {
            "id": "chatcmpl-fake",
            "object": "chat.completion.chunk",
            "created": 1700000000,
            "model": model,
            "choices": [{"index": 0, "delta": delta, "finish_reason": finish}],
        }
    )


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt: str, *args) -> None:  # keep CI output readable
        pass

    def do_POST(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
        model = "fake-model"
        try:
            length = int(self.headers.get("Content-Length") or 0)
            req = json.loads(self.rfile.read(length) or b"{}")
            model = req.get("model") or model
        except Exception:
            pass

        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")
        self.end_headers()

        body = b": keepalive\n\n"
        body += chunk(model, {"role": "assistant", "content": ""})
        for text in CHUNKS:
            body += b": keepalive\n\n"
            body += chunk(model, {"content": text})
        body += chunk(model, {}, "stop")
        # Usage is sent before [DONE] so the router can report TPS.
        body += event(
            {
                "id": "chatcmpl-fake",
                "object": "chat.completion.chunk",
                "created": 1700000000,
                "model": model,
                "choices": [],
                "usage": {"completion_tokens": 3, "total_tokens": 12},
            }
        )
        body += b"data: [DONE]\n\n"
        # Final event with no trailing blank line: the upstream simply hung up.
        body += b": keepalive\n\n"
        self.wfile.write(body)
        self.wfile.flush()
        self.close_connection = True


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--port", type=int, default=9099)
    ap.add_argument("--delay", type=float, default=0.0, help="seconds to wait before responding")
    args = ap.parse_args()

    if args.delay:
        time.sleep(args.delay)

    srv = ThreadingHTTPServer(("127.0.0.1", args.port), Handler)
    srv.daemon_threads = True
    print(f"fake-sse-upstream listening on http://127.0.0.1:{args.port}", flush=True)
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
