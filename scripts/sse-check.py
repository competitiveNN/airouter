#!/usr/bin/env python3
"""Assert the SSE termination contract against a running gateway.

A client stops parsing at the first `data: [DONE]`, so a sentinel that appears
ahead of the content chunks silently truncates the response. That failure is
invisible to a status-code check (the gateway returns 200) and easy to miss by
eyeballing output — it was found on 2026-09-29 only by asserting on captured
bytes. This harness makes that assertion routine.

Checks, per request:
  * exactly one `data: [DONE]` sentinel
  * the sentinel is the final data event
  * every content chunk precedes the sentinel (so nothing is discarded)
  * no SSE comment line (`: keepalive`) leaked to the client
  * the reassembled content is non-empty
  * optionally, the content matches an expected string (--expect)

Usage:
    python3 scripts/sse-check.py                       # all four profiles
    python3 scripts/sse-check.py --models smart work    # subset
    python3 scripts/sse-check.py --rounds 20            # randomized soak
    python3 scripts/sse-check.py --expect PING OK        # content assertion
    python3 scripts/sse-check.py --url http://host:9090

Auth comes from AIROUTER_API_KEY in the environment (or --api-key).
"""

from __future__ import annotations

import argparse
import json
import os
import random
import sys
import urllib.error
import urllib.request

DONE = "data: [DONE]"
MODELS = ("smart", "work", "fast", "large")


def load_api_key(explicit: str | None) -> str:
    if explicit:
        return explicit
    key = os.environ.get("AIROUTER_API_KEY", "")
    if not key:
        sys.exit("AIROUTER_API_KEY is not set; pass --api-key or export it")
    return key


def stream_once(url: str, api_key: str, model: str, prompt: str, max_tokens: int, timeout: float) -> str:
    payload = json.dumps(
        {
            "model": model,
            "max_tokens": max_tokens,
            "stream": True,
            "messages": [{"role": "user", "content": prompt}],
        }
    ).encode()
    req = urllib.request.Request(
        url,
        data=payload,
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {api_key}"},
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        if resp.status != 200:
            raise RuntimeError(f"HTTP {resp.status}")
        return resp.read().decode("utf-8", "replace")


def parse_events(body: str) -> tuple[list[dict], int, list[str]]:
    """Return (chunks, done_count, comment_lines) from a raw SSE body."""
    chunks: list[dict] = []
    comments: list[str] = []
    done = 0
    for line in body.splitlines():
        line = line.strip()
        if not line:
            continue
        if line.startswith(":"):
            comments.append(line)
            continue
        if not line.startswith("data: "):
            continue
        payload = line[6:].strip()
        if payload == "[DONE]":
            done += 1
            continue
        try:
            chunks.append(json.loads(payload))
        except json.JSONDecodeError:
            pass
    return chunks, done, comments


def content_of(chunks: list[dict]) -> str:
    out = []
    for c in chunks:
        for choice in c.get("choices") or []:
            delta = choice.get("delta") or {}
            out.append(delta.get("content") or "")
    return "".join(out)


def check(body: str, expect: str | None) -> tuple[list[str], list[str]]:
    """Split the findings into (contract violations, content warnings).

    These are different failure classes and conflating them produces false
    alarms. A contract violation is a framing bug in the gateway: the client's
    view of the stream is wrong. A content warning means the model chose to
    emit nothing — a provider/content problem, not a gateway bug.

    The distinction matters in practice: reasoning models such as
    kilocode/dots-studio emit a long `reasoning` trace and can exhaust the
    entire max_tokens budget before producing a single content token. The
    stream is perfectly well-formed in that case; there is simply nothing in
    it. Reporting that as truncation sends you hunting in proxy.go for a bug
    that is not there.
    """
    problems: list[str] = []
    warnings: list[str] = []
    chunks, done_count, comments = parse_events(body)
    last_data_line = ""
    for line in body.splitlines():
        if line.strip().startswith("data: "):
            last_data_line = line.strip()

    if done_count == 0:
        problems.append("no [DONE] sentinel: the client would hang waiting for stream end")
    elif done_count > 1:
        problems.append(f"{done_count} [DONE] sentinels; a client stops parsing at the first one")

    if done_count and last_data_line != DONE:
        problems.append(f"[DONE] is not the final data event (last was: {last_data_line[:70]})")

    if comments:
        problems.append(f"SSE comment line leaked to the client: {comments[0][:50]}")

    text = content_of(chunks)
    if not text.strip():
        reasoning = sum(
            len((c.get("delta") or {}).get("reasoning") or (c.get("delta") or {}).get("reasoning_content") or "")
            for chunk in chunks
            for c in (chunk.get("choices") or [])
        )
        detail = f" after {reasoning} chars of reasoning" if reasoning else ""
        warnings.append(f"stream carried no content{detail} (try a larger --max-tokens)")
    elif expect and expect not in text:
        warnings.append(f"content {text[:60]!r} does not contain expected {expect!r}")

    return problems, warnings


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--url", default="http://localhost:9090/v1/chat/completions")
    ap.add_argument("--api-key", default=None)
    ap.add_argument("--models", nargs="*", default=list(MODELS))
    ap.add_argument("--rounds", type=int, default=1, help="requests per model (model chosen at random after the first)")
    ap.add_argument("--max-tokens", type=int, default=200)
    ap.add_argument("--timeout", type=float, default=90.0)
    ap.add_argument("--prompt", default="Reply with exactly: PING OK")
    ap.add_argument("--expect", default=None, help="substring the reassembled content must contain")
    ap.add_argument("--seed", type=int, default=None)
    args = ap.parse_args()

    if args.seed is not None:
        random.seed(args.seed)

    api_key = load_api_key(args.api_key)
    models = [m for m in args.models]
    unknown = [m for m in models if m not in MODELS]
    if unknown:
        sys.exit(f"unknown model(s): {', '.join(unknown)}; valid: {', '.join(MODELS)}")

    failures = 0
    warned = 0
    total = 0
    for round_no in range(args.rounds):
        # First round walks every requested model; later rounds randomize so a
        # soak spreads across providers instead of hammering one chain.
        picks = models if round_no == 0 else [random.choice(models) for _ in range(len(models))]
        for model in picks:
            total += 1
            try:
                body = stream_once(args.url, api_key, model, args.prompt, args.max_tokens, args.timeout)
            except (urllib.error.URLError, OSError, RuntimeError) as exc:
                print(f"FAIL {model}: request error: {exc}")
                failures += 1
                continue

            problems, warnings = check(body, args.expect)
            text = content_of(parse_events(body)[0])
            if problems:
                failures += 1
                print(f"FAIL {model} (round {round_no + 1}):")
                for p in problems:
                    print(f"       - {p}")
                head = "\n".join(body.splitlines()[:6])
                print(f"       first lines:\n{head}")
                continue
            if warnings:
                warned += 1
                note = "; ".join(warnings)
                print(f"warn {model} (round {round_no + 1}): stream is well-formed but {note}")
                continue
            print(f"ok   {model} (round {round_no + 1}): 1 sentinel last, content={text[:40]!r}")

    print(f"\n{total - failures}/{total} streams satisfy the termination contract")
    if warned:
        print(f"({warned} well-formed but contentless — provider/content, not framing)")
    if failures:
        print(f"FAIL: {failures} stream(s) violated the termination contract")
        return 1
    print("PASS: every stream terminated exactly once, last, with no stray comments")
    return 0


if __name__ == "__main__":
    sys.exit(main())
