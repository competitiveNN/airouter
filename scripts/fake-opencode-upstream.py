#!/usr/bin/env python3
"""A fake OpenCode gateway that enforces the client-attribution headers.

WHY THIS EXISTS

The real `opencode.ai/zen/v1` cannot be used to test that airouter sends the
OpenCode client-attribution headers, for two independent reasons:

  1. `space-bunny-free` is the only model this key is entitled to, and it
     answers HTTP 200 **with or without** the headers. A live check asserting
     a 200 therefore passes even if `opencodeRequestHeaders()` were deleted.
  2. Every other model answers `403 FreeTierError` regardless, so there is no
     second model to make the headers load-bearing.

This fake closes that hole hermetically. With `--require-headers` (the default)
it reproduces the real upstream's contract: no (or malformed) attribution
headers => 403 FreeTierError, exactly as opencode.ai does. So a green
end-to-end run against this fake *is* the proof that the headers are emitted on
the wire, with no credential and no network.

It also serves both response modes, unlike scripts/fake-sse-upstream.py, which
always streams and so cannot back the non-stream half of the routing check.

Usage:
    python3 scripts/fake-opencode-upstream.py --port 9099
    python3 scripts/fake-opencode-upstream.py --port 9099 --no-require-headers
"""

from __future__ import annotations

import argparse
import json
import re
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

# The contract the real gateway enforces, and the one airouter must satisfy.
REQUIRED_HEADERS = {
    "x-opencode-client": "cli",
    "x-opencode-project": "default",
}
# Generated per request by airouter, so only the shape is checked, not a value.
REQUIRED_PATTERNS = {
    "x-opencode-request": re.compile(r"^msg_[0-9a-f]{32}$"),
}
# Either a generated ses_<hex> or the gateway's own pinned ctx:<hex>.
SESSION_PATTERN = re.compile(r"^(ses_[0-9a-f]{32}|ctx:[0-9a-f]{16,})$")

CHUNKS = ("PING", " OK")
DEFAULT_MODEL = "space-bunny-free"

# Request counter for --empty-first. Module-level so the handler (one instance,
# possibly on several threads) can share it without threading a value through.
_SERVED = 0


def _next_serve_index() -> int:
    global _SERVED
    _SERVED += 1
    return _SERVED


def free_tier_error(message: str) -> bytes:
    """The exact body opencode.ai returns, so a 403 here is recognisable."""
    return json.dumps(
        {
            "type": "error",
            "error": {
                "type": "FreeTierError",
                "message": message,
            },
        }
    ).encode()


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
    require_headers = True
    empty_first = 0

    def log_message(self, fmt: str, *args) -> None:  # noqa: A003 - keep output readable
        pass

    def _attribution_problem(self) -> str | None:
        """Return why this request would be rejected upstream, or None."""
        ua = self.headers.get("User-Agent", "")
        if not ua.startswith("opencode/"):
            return f"User-Agent {ua!r} is not an opencode client"
        for name, want in REQUIRED_HEADERS.items():
            got = self.headers.get(name, "")
            if got != want:
                return f"{name}={got!r}, want {want!r}"
        for name, pattern in REQUIRED_PATTERNS.items():
            got = self.headers.get(name, "")
            if not pattern.match(got):
                return f"{name}={got!r} does not match {pattern.pattern}"
        session = self.headers.get("x-opencode-session", "")
        if not SESSION_PATTERN.match(session):
            return f"x-opencode-session={session!r} is neither ses_<hex> nor a pinned ctx:<hex>"
        return None

    def do_GET(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
        body = json.dumps(
            {"object": "list", "data": [{"id": DEFAULT_MODEL, "object": "model",
                                         "created": 1700000000, "owned_by": "opencode"}]}
        ).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
        model = DEFAULT_MODEL
        stream = False
        try:
            length = int(self.headers.get("Content-Length") or 0)
            req = json.loads(self.rfile.read(length) or b"{}")
            model = req.get("model") or model
            stream = bool(req.get("stream"))
        except Exception:
            pass

        if self.require_headers:
            problem = self._attribution_problem()
            if problem:
                body = free_tier_error(
                    "OpenCode's free tier can only be used from within OpenCode"
                )
                self.send_response(403)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
                return

        if stream:
            self._stream(model)
        else:
            self._complete(model)

    def _served_empty(self) -> bool:
        """True when this request should come back 200 with no content.

        Reproduces an artifact seen live on 2026-09-30 against
        opencode.ai/zen/v1: HTTP 200, no cooldown, no timeout, and no usable
        content. A harness that only treats non-200 as failure calls that a
        routing fault; it is an upstream artifact and must be retried.
        """
        if not self.empty_first:
            return False
        return _next_serve_index() <= self.empty_first

    def _complete(self, model: str) -> None:
        empty = self._served_empty()
        message = {"role": "assistant", "content": "" if empty else "".join(CHUNKS)}
        if empty:
            # What the real gateway sends when a reasoning model spends the whole
            # budget before emitting anything.
            message["reasoning_content"] = "thinking"
        body = json.dumps(
            {
                "id": "chatcmpl-fake",
                "object": "chat.completion",
                "created": 1700000000,
                "model": model,
                "choices": [
                    {
                        "index": 0,
                        "finish_reason": "length" if empty else "stop",
                        "message": message,
                    }
                ],
                "usage": {"prompt_tokens": 12, "completion_tokens": 32 if empty else 3,
                          "total_tokens": 44 if empty else 15},
            }
        ).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _stream(self, model: str) -> None:
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")
        self.end_headers()
        body = b""
        body += chunk(model, {"role": "assistant", "content": ""})
        for text in CHUNKS:
            body += chunk(model, {"content": text})
        body += chunk(model, {}, "stop")
        body += event(
            {
                "id": "chatcmpl-fake",
                "object": "chat.completion.chunk",
                "created": 1700000000,
                "model": model,
                "choices": [],
                "usage": {"completion_tokens": 3, "total_tokens": 15},
            }
        )
        body += b"data: [DONE]\n\n"
        self.wfile.write(body)
        self.wfile.flush()
        self.close_connection = True


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--port", type=int, default=9099)
    ap.add_argument("--delay", type=float, default=0.0, help="seconds to wait before responding")
    ap.add_argument(
        "--no-require-headers",
        action="store_true",
        help="serve every request, as space-bunny-free does; the negative control "
             "for the attribution assertion",
    )
    ap.add_argument(
        "--empty-first",
        type=int,
        default=0,
        metavar="N",
        help="answer the first N non-streaming requests with HTTP 200 and no "
             "content, reproducing the empty-completion artifact seen live",
    )
    args = ap.parse_args()

    if args.delay:
        time.sleep(args.delay)

    Handler.require_headers = not args.no_require_headers
    Handler.empty_first = args.empty_first
    srv = ThreadingHTTPServer(("127.0.0.1", args.port), Handler)
    srv.daemon_threads = True
    mode = "requiring attribution headers" if Handler.require_headers else "accepting anything"
    print(f"fake-opencode-upstream on http://127.0.0.1:{args.port} ({mode})", flush=True)
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
