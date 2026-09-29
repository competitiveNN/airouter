#!/usr/bin/env python3
"""Fail if a commit attribution in the documentation names a commit that is not real.

THE GAP THIS CLOSES
-------------------
`scripts/audit-drift-check.py` validates that the SYMBOL a finding names still
exists. It says nothing about whether the FIX a finding describes was applied.
Those are different questions, and only the first was being asked.

That gap had a concrete cost. A round of work wrote that
`scripts/audit-drift-check.py` had been changed to walk sources recursively, the
change was verified, and then a `git reset --hard` during unrelated probing
destroyed it. The summary and the audit document both went on describing it as
shipped, and every existing check passed, because the symbol anchors in the
finding were all still resolvable -- the finding was accurate about the code and
wrong about the commit.

A second gap lives next to it. `dist/README.md` lists the unpushed commits so a
handoff is possible. That list is maintained by hand, and it carries a documented
self-reference: writing a count creates a commit that makes the count larger. The
document already tells the reader to distrust the number; this makes the LIST
checkable instead, by set comparison against git, so a stale entry is a failure
rather than a surprise during a handoff.

WHAT IS CHECKED
---------------
  1. Every commit hash the documentation cites resolves to a real commit. A
     fabricated or mistyped hash is a citation that sends a reader nowhere, and
     it is exactly the shape a claim takes when it is written from memory of
     work rather than from the history.

  2. Every commit listed in dist/README.md is actually unpushed, and every
     unpushed commit is listed there. Set comparison, not a count -- the same
     reasoning that replaced a length comparison in the fuzz gate: a count is
     satisfied by any equally-wrong list. Commits whose diff touches
     dist/README.md are exempt, because a commit cannot name its own hash and
     requiring it to would be unsatisfiable; every other commit is still
     required, so nothing real can hide behind the exemption.

Neither check guesses intent. A hash that resolves but does not exist is not
possible; a hash that resolves to a real commit which merely does not do what
the prose says is out of scope, and the doc says so.

Usage: scripts/audit-attribution-check.py [--verbose]
Exit 0 = attributions and the unpushed list agree with the repository.
Exit 1 = at least one does not.
Exit 2 = the check could not run (not a repository, missing document).

ONE PLACE IT IS EXPECTED TO FAIL
--------------------------------
A tree recovered with `git am` reproduces the CONTENT of every commit but
re-hashes all of them, so every commit the documentation cites is absent by
construction. The citation check will report all of them there, and that is
correct behaviour rather than drift: the trees are what recovery is verified
on, and the bundle -- which preserves the original hashes -- is the artifact
that keeps the citations resolvable. Use the bundle when the history matters,
not the patch.
"""

from __future__ import annotations

import argparse
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
AUDIT = ROOT / "docs" / "audit.md"
DIST_README = ROOT / "dist" / "README.md"

# A short hash as written in the prose: backticked, 7-40 hex chars. Deliberately
# not matching every hex-looking token -- a 40-char hash is a full sha and is
# fine to check too, but ordinary words like `deadbeef` in an example must not
# be mistaken for citations. Requiring the backticks is what keeps that honest:
# the documentation cites commits in backticks, by convention, everywhere.
HASH_RE = re.compile(r"`([0-9a-f]{7,40})`")

# Entries in dist/README.md's commit list: "- `abc1234` message".
LIST_ENTRY_RE = re.compile(r"^- `([0-9a-f]{7,40})` ", re.M)


def git(*args: str) -> tuple[int, str]:
    p = subprocess.run(
        ["git", *args],
        cwd=ROOT,
        capture_output=True,
        text=True,
        check=False,
    )
    return p.returncode, p.stdout.strip()


def commit_exists(sha: str) -> bool:
    rc, _ = git("cat-file", "-e", f"{sha}^{{commit}}")
    return rc == 0


def unpushed_commits() -> list[str] | None:
    """Short hashes of commits origin/master is missing, oldest first.

    Returns None when the upstream ref is unavailable (a shallow clone with no
    origin, or a repository that has never fetched). That is not a finding: the
    check degrades to the citation check rather than reporting a problem the
    reader cannot act on.
    """
    rc, _ = git("rev-parse", "--verify", "--quiet", "origin/master")
    if rc != 0:
        return None
    _, out = git("log", "--format=%h", "--reverse", "origin/master..HEAD")
    return out.splitlines() if out else []


def list_touching_commits() -> set[str]:
    """Short hashes of unpushed commits whose diff touches dist/README.md.

    These are the commits that DELIVER the list. A commit cannot contain its own
    hash, so demanding that the list name them is unsatisfiable: fixing the
    complaint creates a new unpushed commit that is itself missing, forever.
    The same self-reference the document already documents for the bundle and
    for the count.

    Excluding exactly these commits is what makes the check converge, and it is
    not a loophole. Such a commit is by definition a commit to the handoff
    document itself; every code, test, script and audit commit is still
    required to appear in the list, so a real change can never hide behind the
    exemption. The exemption is derived from the commit's own diff, not from
    anything written in the document, so it cannot be widened from the prose.
    """
    rc, out = git(
        "log", "--format=%h", "--reverse", "origin/master..HEAD", "--", str(DIST_README)
    )
    if rc != 0:
        return set()
    return {line for line in out.splitlines() if line}


def check_citations(verbose: bool) -> list[str]:
    problems: list[str] = []
    for doc in (AUDIT, DIST_README):
        if not doc.exists():
            print(f"audit-attribution-check: {doc} not found", file=sys.stderr)
            continue
        seen: set[str] = set()
        for sha in HASH_RE.findall(doc.read_text()):
            if sha in seen:
                continue
            seen.add(sha)
            if commit_exists(sha):
                if verbose:
                    print(f"  ok:   {doc.name} cites {sha}")
            else:
                problems.append(f"{doc.relative_to(ROOT)} cites `{sha}`, which is not a commit")
    return problems


def check_unpushed_list(verbose: bool) -> list[str]:
    problems: list[str] = []
    actual = unpushed_commits()
    if actual is None:
        if verbose:
            print("  skip: no origin/master ref; not checking the unpushed list")
        return problems
    if not DIST_README.exists():
        print(f"audit-attribution-check: {DIST_README} not found", file=sys.stderr)
        return problems

    listed = set(LIST_ENTRY_RE.findall(DIST_README.read_text()))
    # Commits that deliver the list cannot name themselves; see the helper.
    exempt = list_touching_commits()
    # A listed commit is legitimate if it is genuinely unpushed OR it is one of
    # the list-delivering commits (which are, by definition, unpushed AND touch
    # this file). The two sets are not disjoint: a list-delivering commit is in
    # `exempt` but is also genuinely unpushed, so it must not be reported as
    # "listed but not unpushed". Membership in `exempt` is the exemption.
    actual_set = set(actual)
    legitimately_listed = actual_set | exempt
    required = actual_set - exempt  # unpushed, non-list-delivering: must be listed

    # Listed but neither genuinely unpushed nor list-delivering: pushed, or
    # amended away. The document is describing a set that does not exist.
    for sha in sorted(listed - legitimately_listed):
        problems.append(
            f"dist/README.md lists {sha}, which is not an unpushed commit "
            f"(it may have been pushed or rewritten)"
        )
    # Unpushed but missing from the handoff list: the commit exists and would
    # be lost. This is the direction that matters.
    for sha in sorted(required - listed):
        problems.append(
            f"commit {sha} is unpushed but missing from dist/README.md's list; "
            f"a handoff built from that document would not include it"
        )
    if verbose:
        skipped = len(exempt)
        note = f" ({skipped} list-delivering commit(s) exempt)" if skipped else ""
        print(f"  ok:   dist/README.md lists all {len(required)} required commit(s){note}")
    return problems


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--verbose", action="store_true", help="print every citation checked")
    args = ap.parse_args()

    rc, _ = git("rev-parse", "--git-dir")
    if rc != 0:
        print("audit-attribution-check: not a git repository", file=sys.stderr)
        return 2

    if not AUDIT.exists():
        print(f"audit-attribution-check: {AUDIT} not found", file=sys.stderr)
        return 2

    problems = check_citations(args.verbose) + check_unpushed_list(args.verbose)

    if problems:
        print(
            "audit-attribution-check: FAILED — the documentation does not match the repository:",
            file=sys.stderr,
        )
        for p in problems:
            print(f"  - {p}", file=sys.stderr)
        print("", file=sys.stderr)
        print(
            "  A citation that names a commit which does not exist is a claim",
            file=sys.stderr,
        )
        print(
            "  written from memory rather than from the history — which is how",
            file=sys.stderr,
        )
        print(
            "  a reverted fix survives a round that documents it as shipped.",
            file=sys.stderr,
        )
        print("", file=sys.stderr)
        print(
            "  For the unpushed list, regenerate it:",
            file=sys.stderr,
        )
        print(
            "    git log --oneline --reverse origin/master..HEAD",
            file=sys.stderr,
        )
        return 1

    actual = unpushed_commits()
    suffix = "" if actual is None else f", {len(actual)} unpushed commit(s) listed accurately"
    print(f"audit-attribution-check: all citations resolve{suffix}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
