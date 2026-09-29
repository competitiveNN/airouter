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
     satisfied by any equally-wrong list. A commit is exempt from the list only
     when its ENTIRE diff is confined to the handoff artifacts under dist/,
     because a commit cannot name its own hash and requiring it to would be
     unsatisfiable. The exemption is confinement, not mere presence: a code
     commit that also tweaks the README is still required, so a one-line doc
     edit cannot launder a real change out of the handoff.

Neither check guesses intent. A hash that resolves but does not exist is not
possible; a hash that resolves to a real commit which merely does not do what
the prose says is out of scope, and the doc says so.

Usage: scripts/audit-attribution-check.py [--verbose]
Exit 0 = attributions and the unpushed list agree with the repository.
Exit 1 = at least one does not.
Exit 2 = the check could not run (not a repository, missing document).

IN A `git am`-RECOVERED TREE
--------------------------
`git am` reproduces the CONTENT of every commit but re-hashes all of them, so
by hash alone every citation the documentation makes is absent, and the check
reports every one. That is not drift: recovery is verified on trees, and the
bundle -- which preserves the original hashes -- is the artifact that keeps the
citations resolvable.

Because a check that can only ever fail is not a useful check, `--recovered
--against <original-repo>` makes it work there instead of merely explaining it.
It matches each cited hash to the commit that carried the same CHANGE, using
`git patch-id --stable`, which fingerprints content rather than metadata and so
survives re-hashing. An honest citation still matches; a fabricated one still
matches nothing, and still fails. Use the bundle when the history matters.
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
    """True if `sha` names a commit that is REACHABLE from HEAD.

    `git cat-file -e` is not enough on its own. It succeeds for any object
    still in the object database, including a DANGLING one -- a commit that was
    reset away, or left behind by a rebase. So a citation naming such a commit
    passes in the working tree where it was written and fails in every clone,
    which is the worst split a check can have: green here, red everywhere the
    work is actually handed off to.

    That is not hypothetical. A round-12 note about a stray empty commit that
    had just been removed produced exactly this: the check passed in the working
    tree and failed on a fresh clone.

    Reachability is the property a citation actually needs. Someone who clones
    the repository can only resolve a hash reachable from some ref, so
    requiring reachability is what makes "this resolves" mean the same thing in
    both places.
    """
    rc, _ = git("cat-file", "-e", f"{sha}^{{commit}}")
    if rc != 0:
        return False
    # A short hash is only a prefix; expand it to the full one before testing
    # ancestry, or the comparison is against a literal string.
    rc, full = git("rev-parse", "--verify", "--quiet", f"{sha}^{{commit}}")
    if rc != 0 or not full:
        return False
    rc, _ = git("merge-base", "--is-ancestor", full, "HEAD")
    return rc == 0


def content_fingerprints(ref_range: str = "HEAD", cwd: Path | None = None) -> dict[str, str]:
    """Map every commit reachable from ref_range to a stable CONTENT fingerprint.

    `git patch-id --stable` hashes a commit's DIFF rather than its metadata, so
    two commits carrying the same change share an id even when the commits
    themselves have entirely different hashes. That is the property needed to
    recognise a re-hashed commit: `git am` rebuilds every commit with a new
    hash and a new committer date, but the change it carries is the same
    change.

    Returns {full_commit_hash: patch_id}. An empty dict means the map could not
    be built, and callers must treat that as "cannot verify", never as "clean".

    Note: the `git log -p` output is captured into memory and fed to `git
    patch-id` over a pipe rather than piping the two commands directly. Piping
    `git log ... | git patch-id` truncates badly on a large history -- on this
    repository it reported 1 fingerprint instead of 77 -- because patch-id stops
    reading once the producer closes early. Capturing first is not a style
    preference; it is the difference between a working map and a nearly empty
    one that would look like "these commits genuinely have no counterpart".
    """
    workdir = str(cwd) if cwd else str(ROOT)
    log = subprocess.run(
        ["git", "log", "--format=%H", "-p", "--no-merges", ref_range],
        cwd=workdir,
        capture_output=True,
        text=True,
        check=False,
    )
    if log.returncode != 0 or not log.stdout.strip():
        return {}
    pid = subprocess.run(
        ["git", "patch-id", "--stable"],
        cwd=workdir,
        input=log.stdout,
        capture_output=True,
        text=True,
        check=False,
    )
    if pid.returncode != 0:
        return {}
    result: dict[str, str] = {}
    for line in pid.stdout.splitlines():
        parts = line.split()
        if len(parts) == 2:
            result[parts[1]] = parts[0]
    return result


def _content_present(sha: str, local_ids: set[str], reference: dict[str, str]) -> bool:
    """True if a cited sha carries content that also exists in the local tree.

    `sha` is a hash cited in the documentation, which in a recovered clone
    refers to a commit in the ORIGINAL history. `reference` maps that original
    repository's full hashes to their content fingerprints; the short hash is
    expanded there. If the local tree (fingerprinted into `local_ids`) contains
    a commit with the same content, the citation is honoured despite the
    re-hash.
    """
    # Expand the short hash against the reference repository's full hashes.
    for full, pid in reference.items():
        if full.startswith(sha):
            return pid in local_ids
    return False


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


# Paths a commit may touch and still count as "delivering the handoff document".
# The exemption is deliberately narrow: a commit is exempt from the unpushed-list
# requirement only when its ENTIRE diff is confined to these artifact paths.
DIST_PREFIX = "dist/"


def _changed_paths(sha: str) -> set[str] | None:
    """Repo-relative paths changed by a commit, or None if that cannot be read."""
    rc, out = git("show", "--pretty=format:", "--name-only", sha)
    if rc != 0:
        return None
    return {line.strip() for line in out.splitlines() if line.strip()}


def is_list_delivery_commit(sha: str) -> bool:
    """True if this commit's whole diff is confined to the tracked handoff artifacts.

    These are the commits that DELIVER the list. A commit cannot contain its own
    hash, so demanding the list name them is unsatisfiable: fixing the complaint
    creates a new unpushed commit that is itself missing, forever. The same
    self-reference the document already documents for the bundle and the count.

    WHY THIS IS NARROWER THAN "touches dist/README.md"
    --------------------------------------------------
    The first version of this exemption was "the diff touches dist/README.md".
    That is trivially gameable: add a one-line README tweak to an otherwise-real
    code commit and the whole commit escapes the list, so a `main.go` change could
    be dropped from the handoff by editing a sentence. The exemption is therefore
    CONFINEment, not presence: EVERY path the commit touches must be a tracked
    handoff artifact (dist/README.md and the generated bundle/patch). Touch even
    one code, test, script, CI, or audit file and the commit must be listed like
    any other.

    This is decided from the commit's own diff against the real repository, never
    from anything the document says, so the prose cannot widen it. In practice it
    means a delivery commit may edit the list and regenerate the artifacts, and
    nothing else -- which is exactly what the generated-artifact policy in
    dist/README.md already requires.
    """
    changed = _changed_paths(sha)
    if not changed:
        # No readable diff: fail closed and require it in the list. An unreadable
        # commit must never be the reason a real one escapes the handoff.
        return False
    return all(p == "dist/README.md" or p.startswith(DIST_PREFIX) for p in changed)


def list_touching_commits() -> set[str]:
    """Short hashes of unpushed commits that are pure handoff-delivery commits."""
    rc, out = git("log", "--format=%h", "--reverse", "origin/master..HEAD")
    if rc != 0:
        return set()
    return {line for line in out.splitlines() if line and is_list_delivery_commit(line)}


def check_citations(
    verbose: bool,
    recovered: bool = False,
    reference_repo: Path | None = None,
) -> list[str]:
    """Every cited hash must resolve to a real commit.

    In --recovered mode a cited hash is additionally allowed to resolve by
    CONTENT. `git am` re-hashes every commit it replays, so by hash alone every
    citation fails in a recovered clone and the check can tell a reader
    nothing. Matching on the change a commit carries, rather than the hash it
    happens to have, keeps the check meaningful there: an honest citation still
    matches, and a fabricated one still matches nothing.
    """
    problems: list[str] = []
    here: set[str] = set()
    reference: dict[str, str] = {}
    if recovered:
        here = set(content_fingerprints("HEAD").values())
        # The reference repository supplies the patch-ids for the hashes the
        # documentation cites, which live in the original history this clone
        # does not have.
        reference = content_fingerprints("HEAD", cwd=reference_repo)
        if not here or not reference:
            print(
                "audit-attribution-check: could not build content fingerprints; "
                "falling back to hash comparison",
                file=sys.stderr,
            )
            recovered = False
        elif verbose:
            print(
                f"  ok:   indexed {len(here)} commit(s) here and "
                f"{len(reference)} in the reference by content"
            )

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
            elif recovered and _content_present(sha, here, reference):
                if verbose:
                    print(
                        f"  ok:   {doc.name} cites {sha} (matched by content; "
                        f"this tree re-hashed it)"
                    )
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
    ap.add_argument(
        "--recovered",
        action="store_true",
        help=(
            "this tree was recovered with `git am`, so commits are re-hashed; "
            "match citations by CONTENT instead of by hash"
        ),
    )
    ap.add_argument(
        "--against",
        metavar="REPO",
        help=(
            "the ORIGINAL repository this tree was recovered from; supplies the "
            "content fingerprints for the hashes the documentation cites. "
            "Required by --recovered."
        ),
    )
    args = ap.parse_args()

    rc, _ = git("rev-parse", "--git-dir")
    if rc != 0:
        print("audit-attribution-check: not a git repository", file=sys.stderr)
        return 2

    if not AUDIT.exists():
        print(f"audit-attribution-check: {AUDIT} not found", file=sys.stderr)
        return 2

    reference_repo: Path | None = None
    if args.recovered:
        if not args.against:
            print(
                "audit-attribution-check: --recovered requires --against pointing at "
                "the original repository; without it the cited hashes are unknown "
                "here and content matching has nothing to compare against",
                file=sys.stderr,
            )
            return 2
        reference_repo = Path(args.against).resolve()
        if not (reference_repo / ".git").exists():
            print(
                f"audit-attribution-check: --against {reference_repo} is not a "
                "git repository",
                file=sys.stderr,
            )
            return 2

    problems = check_citations(
        args.verbose, recovered=args.recovered, reference_repo=reference_repo
    ) + check_unpushed_list(args.verbose)

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
