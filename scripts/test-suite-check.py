#!/usr/bin/env python3
"""Fail when the Go test suite has quietly gotten smaller, or when a test in it
can no longer fail.

WHY THIS EXISTS
---------------

`go test ./...` reports success for two very different situations: every test
passed, and there were fewer tests than there were yesterday. The second one is
the shape of every lost-work incident this repository has actually had. A
200-test suite truncated to 170 is green. So is a test whose assertions were
deleted and whose name was left behind -- and the name being left behind is what
makes it survive review, because the file still says `TestCooldownSurvives` and
nothing in the diff looks like a removal.

The gate counts assertions everywhere else. scripts/doc-verify-selftest.sh
carries `EXPECTED=n` and goes red when an assertion disappears. The Go suite --
by volume the largest body of verification here -- had no such contract, so the
only thing standing between a truncated main_test.go and a green gate was someone
eyeballing a diff.

WHAT IS CHECKED
---------------

  1. Every test name in the baseline is still DISCOVERED by the toolchain. Not
     "still present in a file": discovered by `go test -list`, which is the set
     that actually runs. A test hidden behind a build constraint, or renamed, or
     deleted, is absent from it, and is named.

  2. A test that exists in a source file but is not discovered is reported,
     unless its file declares a build constraint. Otherwise a platform-gated test
     would be reported as lost on every platform where it correctly does not run.

  3. Every test body contains a path on which it can report failure: a t.Error /
     t.Fatal / t.Fail / t.Skip call, a panic, or a call to a local helper that
     itself makes those calls. `t.Log` is deliberately not in that list; a test
     that only logs passes forever.

The baseline is a SUPERSET contract, not a count. A count is satisfied by any
equally-wrong set -- the same reasoning that made scripts/audit-attribution-check
compare commit sets instead of lengths. `EXPECTED=205` would be satisfied by 205
tests of which 40 were different tests, would need editing every time a test is
added, and would name nothing when it failed. Here, adding tests is free, and
losing one prints its name and the file it used to live in.

WHAT IT DELIBERATELY DOES NOT CLAIM
-----------------------------------

Rule 3 is textual, so it is approximate. It answers "does this body contain any
way to fail", not "do its assertions test the thing its name promises". A test
that calls t.Fatal on an unrelated condition satisfies rule 3 and is still a bad
test; that is out of scope, and the exemption list below is where a body that is
non-vacuous by construction -- a Fuzz target, a benchmark that only measures --
is recorded WITH a reason rather than by loosening the rule for everything.

Usage: scripts/test-suite-check.py [--verbose] [--update]
Exit 0 = the suite is as large as it was and every test can fail.
Exit 1 = at least one test disappeared, became undiscoverable, or became vacuous.
Exit 2 = the check could not run (no go toolchain, suite does not build, no
         baseline). A check that cannot run must not report success.
"""
import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
BASELINE = Path(__file__).resolve().parent / "test-suite-baseline.txt"

VERBOSE = "--verbose" in sys.argv
UPDATE = "--update" in sys.argv
PREFIX = "test-suite-check:"

# Names go here ONLY with a reason, and only when the function is not meant to
# assert anything. It is keyed by name, so an exemption cannot survive the test
# it exempts being deleted and reappearing as a free pass for a new test of the
# same kind.
EXEMPT = {
    "BenchmarkFallbackLatency":
        "a benchmark, not an assertion. It measures fallback latency and reports "
        "a number; testing.B is in the discovered set and a benchmark that never "
        "fails is what a benchmark is. Exempted BY NAME so the exemption cannot "
        "silently cover the next benchmark added to this file.",
}

TEST_RE = re.compile(r"^func\s+(Test|Fuzz|Benchmark)([A-Z0-9_][\w]*)\s*\(", re.M)
# Anything that can make the test binary report a failure. t.Log and t.Logf are
# excluded on purpose: they cannot fail anything. The receiver may be t or b, so
# a benchmark that does check its setup is not mistaken for a vacuous one.
FAILING_RE = re.compile(r"\b[bt]\.(Error|Fatal|Skip|Fail)\w*\s*\(|\bpanic\s*\(")
# A *testing.T / *testing.B parameter, wherever it sits in the parameter list --
# a helper with MORE parameters than just t is the common case, and a pattern
# anchored on the closing paren matched only a helper that takes nothing else,
# which made every assert* helper in this repository invisible to the scan and
# reported their callers as vacuous.
TB_RE = re.compile(r"\*\s*testing\.[TB]\b")
BUILD_TAG_RE = re.compile(r"^\s*//go:build|^\s*// \+build", re.M)

discovered = set()
problems = []
notes = []


def say(msg):
    if VERBOSE:
        print("  " + msg)


def fail(msg):
    problems.append(msg)
    print("  FAIL %s" % msg)


def cannot(msg):
    """Stop, loudly, with a distinct exit code.

    Exit 1 says "the suite has a problem", which is a different and more
    alarming thing than "I could not look". The first version passed its message
    straight to sys.exit(), which prints to stderr and exits 1 -- so a fixture
    with no baseline and a fixture with a lost test were indistinguishable, and
    the self-test caught that inside a minute.
    """
    print("%s %s" % (PREFIX, msg), file=sys.stderr)
    sys.exit(2)


def code_view(text):
    """The file with whole-line comments blanked.

    Not a Go lexer, deliberately. The first version HAD one, and it lost track of
    itself on a raw string holding Prometheus exposition text: roughly a thousand
    lines of a test file silently became a string literal, and the test defined
    after it was reported as missing from the repository. A tool that invents a
    missing test is worse than no tool, because the reader goes looking for a
    deleted test that was never deleted.

    What is needed here is far weaker than lexing: a comment must not be able to
    stand in as a test's failure path. Whole-line comments are where a stray
    `t.Fatal` gets written in prose; inline trailing comments are left alone,
    since they cannot introduce anything a scan would mistake for a real call.
    """
    return "\n".join("" if ln.lstrip().startswith("//") else ln
                     for ln in text.split("\n"))


BODY_END_RE = re.compile(r"\n\}[^\S\n]*(?://[^\n]*)?\n")
NEXT_TOP_RE = re.compile(r"(?:^|\n)(func|var|const|type|package|\}|//|\n)")


def body_of(text, brace):
    """The text from '{' to the line that closes this top-level function, or
    None when no line plausibly closes it.

    Uses gofmt's guarantee -- a top-level body closes with '}' in column 1 --
    instead of tracking brace depth, because depth counting is what the broken
    first version did and one brace inside a raw string ends the body early.
    That version of the bug is still reachable: gofmt cannot stop a RAW STRING
    from containing a line that starts with '}', and two of the cooldown tests
    embed JSON that does exactly that. So a closing line must also be empty
    after the brace and be followed by something that legally follows a function
    at top level.

    When nothing matches, None is returned and the caller says so. The failure
    mode this protects against is the one the first version had: a truncated body
    reads as a test with no assertions, and the reader goes looking for a bug in
    a test that has one.
    """
    for m in BODY_END_RE.finditer(text, brace):
        rest = text[m.end():m.end() + 400]
        if rest.strip() == "" or NEXT_TOP_RE.match(rest.lstrip("\n")):
            return text[brace:m.end()]
    return None


def discover():
    """The set of tests the toolchain would actually run, per package."""
    try:
        p = subprocess.run(["go", "test", "-list", ".*", "./..."],
                           cwd=str(ROOT), capture_output=True, text=True, timeout=600)
    except FileNotFoundError:
        cannot("%s the go toolchain is not on PATH; this check cannot run" % PREFIX)
    except subprocess.TimeoutExpired:
        cannot("%s 'go test -list' timed out; this check cannot run" % PREFIX)
    if p.returncode != 0:
        # A suite that does not build has no test set to compare. Reporting that
        # as "the suite shrank" would send the reader to the wrong file.
        cannot("%s 'go test -list' failed (exit %d); the suite does not build "
                 "or list, so its contents cannot be read:\n%s"
                 % (PREFIX, p.returncode, (p.stdout + p.stderr)[-1500:]))
    names = set()
    for line in p.stdout.splitlines():
        tok = line.strip()
        if re.fullmatch(r"(Test|Fuzz|Benchmark)([A-Z0-9_]\w*)", tok):
            names.add(tok)
    return names


def sources():
    """name -> (file, body, gated) for every test-shaped function on disk."""
    out = {}
    for f in sorted(ROOT.rglob("*_test.go")):
        rel = str(f.relative_to(ROOT))
        if any(part in (".git", "dist", "testdata") for part in f.parts):
            continue
        text = f.read_text(encoding="utf-8", errors="replace")
        # Build constraints are comments, so they are read from the raw text;
        # everything structural is read from the literal-stripped text.
        gated = bool(BUILD_TAG_RE.search(text))
        clean = code_view(text)
        for m in TEST_RE.finditer(clean):
            name = m.group(1) + m.group(2)
            brace = clean.find("{", m.end())
            out[name] = (rel, body_of(clean, brace) if brace != -1 else None, gated)
    return out


def helpers():
    """Test-file helpers that can fail the test, so a body that asserts through
    a helper is not mistaken for a vacuous one."""
    names = set()
    for f in ROOT.rglob("*_test.go"):
        if any(part in (".git", "dist", "testdata") for part in f.parts):
            continue
        clean = code_view(f.read_text(encoding="utf-8", errors="replace"))
        for m in re.finditer(r"^func\s+(?:\(\s*\w+\s+\*testing\.[TB]\s*\)\s*)?(\w+)\s*\(", clean, re.M):
            fname = m.group(1)
            brace = clean.find("{", m.end())
            if brace == -1:
                continue
            if TB_RE.search(clean[m.end():brace]):
                hbody = body_of(clean, brace)
                if hbody is not None and FAILING_RE.search(hbody):
                    names.add(fname)
    return names


def read_baseline():
    entries = {}
    for line in BASELINE.read_text(encoding="utf-8").splitlines():
        line = line.rstrip("\n")
        if not line or line.startswith("#"):
            continue
        parts = line.split("\t")
        entries[parts[0]] = parts[1] if len(parts) > 1 else "?"
    return entries


def main():
    srcs = sources()
    if not srcs:
        cannot("%s no test functions found under %s; nothing to check" % (PREFIX, ROOT))

    discovered.update(discover())
    if not discovered:
        cannot("%s the toolchain discovered 0 tests; reporting success would be "
                 "the exact lie this check exists to catch" % PREFIX)

    gated_files = {n for n, (_r, _b, g) in srcs.items() if g}

    print("%s %d discovered by the toolchain, %d in source files"
          % (PREFIX, len(discovered), len(srcs)))

    # ---- 1 + 2: discovered vs on disk -------------------------------------
    for name in sorted(set(srcs) - discovered):
        if name in gated_files:
            say("skip  %s (declared in a file with a build constraint)" % name)
            continue
        fail("%s is in %s but the toolchain does not discover it, so it never runs "
             "(a build constraint, a malformed signature, or a name the test "
             "binary does not recognise)" % (name, srcs[name][0]))
    for name in sorted(discovered - set(srcs)):
        fail("%s is discovered but not found in any *_test.go under the repository" % name)

    # ---- 3: every test can fail ------------------------------------------
    asserter = helpers()
    vacuous = []
    for name in sorted(discovered):
        if name in EXEMPT:
            say("exempt %s -- %s" % (name, EXEMPT[name]))
            continue
        rel, body, _g = srcs.get(name, ("?", "", False))
        if name not in srcs:
            # Already reported above as discovered-but-absent; reporting it as
            # vacuous too would describe the same defect twice, in two shapes.
            continue
        if body is None:
            fail("%s in %s: the end of its body could not be located, so this check "
                 "cannot tell whether it can fail. Refusing to guess in either "
                 "direction" % (name, rel))
            continue
        if FAILING_RE.search(body):
            continue
        if any(re.search(r"\b%s\s*\(" % re.escape(h), body) for h in asserter):
            continue
        vacuous.append((name, rel))
    for name, rel in vacuous:
        fail("%s in %s has no path on which it can report a failure "
             "(no t.Error/t.Fatal/t.Fail/t.Skip, no panic, no calling helper that "
             "does); it passes no matter what the code does" % (name, rel))
    notes.append("%d helper(s) recognised as able to fail a test" % len(asserter))

    # ---- the superset contract -------------------------------------------
    if UPDATE:
        lines = ["# Generated by: scripts/test-suite-check.py --update",
                 "# Contract: every name here must still be DISCOVERED by the",
                 "# toolchain. Adding tests does not require editing this file;",
                 "# losing one fails and names the test and its last file."]
        lines += ["%s\t%s" % (n, srcs[n][0]) for n in sorted(discovered) if n in srcs]
        BASELINE.write_text("\n".join(lines) + "\n", encoding="utf-8")
        print("%s wrote %d entries to %s" % (PREFIX, len(discovered), BASELINE.name))
        return 1 if problems else 0

    if not BASELINE.is_file():
        cannot("%s %s does not exist; run with --update to create it" % (PREFIX, BASELINE))

    baseline = read_baseline()
    missing = sorted(set(baseline) - discovered)
    for name in missing:
        fail("the suite lost %s (last seen in %s). It is in the baseline and the "
             "toolchain no longer discovers it -- a smaller suite is not a "
             "failing suite, so nothing else here would notice"
             % (name, baseline[name]))
    extra = sorted(discovered - set(baseline))
    if extra:
        # Not a failure: the contract must not tax the addition of a test, or it
        # gets edited to match whatever happened, which is what a count invites.
        # Printed unconditionally rather than only under --verbose: drift that is
        # invisible in the normal output is drift that does not get recorded, and
        # the baseline only protects the suite while it tracks the suite.
        print("  note  %d test(s) are new since the baseline, run --update to "
              "record them: %s" % (len(extra), ", ".join(extra[:8])))

    for n in notes:
        say("note  " + n)
    if problems:
        print("%s FAILED -- %d problem(s)" % (PREFIX, len(problems)))
        return 1
    print("%s ok: %d discovered tests, all %d baselined tests still run, "
          "none vacuous" % (PREFIX, len(discovered), len(baseline)))
    return 0


sys.exit(main())
