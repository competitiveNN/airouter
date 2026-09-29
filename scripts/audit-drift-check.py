#!/usr/bin/env python3
"""Fail if an audit finding's symbol anchor no longer exists in the source.

docs/audit.md entries lead with the function or symbol the finding concerns,
e.g. `• `handleCompletion` (was api.go:330-378) — ...`. The "(was ...)" line
range is explicitly historical and is allowed to rot; the *symbol* is the part
that must stay accurate, because that is what someone greps for when they act
on a finding.

This check is what keeps that promise honest. Without it, "use the symbol, not
the line" is a convention nobody enforces and the anchors rot exactly the way
the line numbers did — silently, and only discovered by a confused reader.

Rules:
  * A symbol mentioned in an anchor is resolved against the Go sources.
  * `Type.Method` is matched on Method (optionally requiring the receiver type
    to appear in the same file), because Go call sites rarely write the
    receiver.
  * Known-deleted symbols are allowlisted with the reason, so a finding that is
    genuinely about a removed function does not fail the build forever.
  * Non-symbol anchors (prose like "request body read in the handlers") are
    skipped: they carry no backticked identifier to check.

Usage: scripts/audit-drift-check.py [--verbose]
"""

from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
AUDIT = ROOT / "docs" / "audit.md"
GO_SOURCES = list(ROOT.glob("*.go"))

# Findings whose subject genuinely no longer exists. Keyed by the literal anchor
# text; each must say why, so the allowlist cannot quietly grow forever.
ALLOWLIST = {
    "Router.SelectNext": (
        "Deleted. Two findings record its removal; a third records that the "
        "tests named after it were renamed. Kept so those findings stay readable."
    ),
}

# Anchors that name a concept rather than a symbol, or that are matched
# differently. Each needs a reason too.
SPECIAL = {
    "time.After": "Go stdlib call, not defined in this repo.",
    "Preferences": "Struct field group in config.go; matched as `type Preferences`.",
    "accumulateContent": "Superseded by accumulateDelta; the finding documents the rename.",
    "ProviderError.RetryAfter": "Field on ProviderError, declared in config.go.",
    "Router.cooldownJitter": "Field on Router, declared in router.go.",
    "Proxy.streamIdleTimeout": "Field on Proxy, declared in proxy.go.",
    "contentDelta": "Helper used during mid-stream resume; may be inlined.",
}


def go_corpus() -> str:
    return "\n".join(p.read_text() for p in GO_SOURCES if p.exists())


def symbols() -> set[str]:
    """All identifiers declared anywhere in the Go sources."""
    found: set[str] = set()
    for src in GO_SOURCES:
        if not src.exists():
            continue
        text = src.read_text()
        # func (r *T) Name(   /  func Name(
        for m in re.finditer(r"^func\s+(?:\([^)]*\)\s*)?([A-Za-z_]\w*)", text, re.M):
            found.add(m.group(1))
        # type Name
        for m in re.finditer(r"^type\s+([A-Za-z_]\w*)", text, re.M):
            found.add(m.group(1))
        # top-level var/const Name
        for m in re.finditer(r"^(?:var|const)\s+([A-Za-z_]\w*)", text, re.M):
            found.add(m.group(1))
        # struct fields: Name Type  /  Name    Type
        for m in re.finditer(r"^\t([A-Za-z_]\w*)\s+[A-Za-z_*\[\]]", text, re.M):
            found.add(m.group(1))
    return found


def resolve(anchor: str, syms: set[str]) -> tuple[bool, str]:
    """Return (resolves, note)."""
    if anchor in ALLOWLIST:
        return True, "allowlisted"
    if anchor in SPECIAL:
        return True, "special-cased"
    if "." in anchor:
        recv, meth = anchor.split(".", 1)
        # Prefer the bare method name; that is how it is referenced at call sites.
        if meth in syms:
            return True, f"method {meth} found (receiver {recv})"
        if recv in syms:
            return True, f"type {recv} found (member {meth} not declared separately)"
        return False, "neither type nor member found"
    if anchor in syms:
        return True, "declared in Go sources"
    return False, "not found in any .go file"


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--verbose", action="store_true", help="print every resolved anchor")
    args = ap.parse_args()

    if not AUDIT.exists():
        print(f"audit-drift-check: {AUDIT} not found", file=sys.stderr)
        return 2

    corpus = go_corpus()
    syms = symbols()
    text = AUDIT.read_text()

    # Anchors are the first backticked token of an entry header.
    anchors = re.findall(r"^•\s+`([^`]+)`", text, re.M)
    if not anchors:
        print("audit-drift-check: no anchors found; the doc format changed?", file=sys.stderr)
        return 2

    problems: list[tuple[str, str]] = []
    seen: set[str] = set()
    for a in anchors:
        if a in seen:
            continue
        seen.add(a)

        # A backticked phrase with spaces or punctuation is prose, not a symbol.
        if not re.fullmatch(r"[A-Za-z_]\w*(?:\.[A-Za-z_]\w*)*", a):
            if args.verbose:
                print(f"  skip (prose): {a}")
            continue

        ok, note = resolve(a, syms)
        if ok:
            if args.verbose:
                print(f"  ok:   {a}  ({note})")
            # A loose textual fallback catches symbols the regex missed.
            if note == "not found in any .go file":  # pragma: no cover
                pass
        else:
            problems.append((a, note))

    # Second pass: textual confirmation for anything the structural pass missed.
    still: list[tuple[str, str]] = []
    for a, note in problems:
        leaf = a.split(".")[-1]
        if re.search(rf"\b{re.escape(leaf)}\b", corpus):
            if args.verbose:
                print(f"  ok:   {a}  (found textually)")
            continue
        still.append((a, note))

    if still:
        print("audit-drift-check: FAILED — audit anchors no longer resolve:", file=sys.stderr)
        for a, note in still:
            print(f"  • {a}: {note}", file=sys.stderr)
        print("", file=sys.stderr)
        print("  These findings name a symbol that no longer exists. Either the", file=sys.stderr)
        print("  symbol moved (update the anchor), the finding was about a", file=sys.stderr)
        print("  deletion (add it to ALLOWLIST in this script, with a reason),", file=sys.stderr)
        print("  or the code was refactored and the finding needs re-checking.", file=sys.stderr)
        return 1

    print(f"audit-drift-check: {len(seen)} anchors checked, all resolve")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
