#!/usr/bin/env python3
"""Regression tests for two gate-integrity mechanisms in scripts/gate.sh.

Both were added on 2026-09-30 and both were, until this file existed, verified
by hand: run once, look at the output, believe it. That is exactly the practice
this repository exists to stop. A guard that is only exercised by hand is
indistinguishable from no guard -- it is silent when it is broken, and a broken
guard means the gate reports green while checking nothing. Both mechanisms below
were proven once in a transcript and are now proven by this file on every run.

WHAT IS TESTED

1. The stage-accounting guard (scripts/gate.sh). Every stage is registered
   through run / run_live / fmt_ok and bumps one of three counters. The guard
   refuses to report a verdict unless passed+failed+skipped equals the number
   of registered stages, and refuses a registered count of zero -- without that
   second check, 0+0+0 == 0 and a gate whose stage-grep stopped matching would
   report success having run nothing.

2. The untracked NOTICE sweep in scripts/shell-lint.sh. shell-lint enforces on
   `git ls-files '*.sh'` only, deliberately, so a scratch file cannot fail the
   build. The cost is that a newly written script is unlinted until committed.
   The sweep closes that blind spot as a NOTICE that cannot change the exit
   code -- an invariant that, asserted only in a comment, is not an invariant.

HOW THE GUARD IS EXERCISED

The guard is a fragment of gate.sh, not a function, so these tests extract the
real lines out of the real file and drive them. Copying the logic into the test
would test the copy: if someone deletes or rewrites the guard in gate.sh, these
tests must notice. `extract_guard` therefore fails when it cannot find both
markers, and `run_guard` sources the extracted text rather than a paraphrase.
"""

from __future__ import annotations

import os
import re
import shlex
import shutil
import subprocess
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parent.parent
GATE = REPO / "scripts" / "gate.sh"
SHELL_LINT = REPO / "scripts" / "shell-lint.sh"
SHELL_RULES = REPO / "scripts" / "shell_lint_rules.py"
ROUTING_CHECK = REPO / "scripts" / "opencode-routing-check.sh"

# A snippet the linter is known to reject: `while` takes `do`, never `then`.
# This is the exact construct shell_lint_rules.py exists for.
VIOLATION = "#!/usr/bin/env bash\nwhile [ 1 -lt 2 ]; then\n  echo hi\ndone\n"
CLEAN = "#!/usr/bin/env bash\nwhile [ 1 -lt 2 ]; do\n  echo hi\ndone\n"


def extract_fingerprint() -> str:
    """gate.sh's tree_fingerprint(), verbatim.

    This is the function behind the gate's INVALID RUN verdict, and behind the
    decision that the routing harness must keep its log OUTSIDE the repository.
    An untracked path inside the tree changes the fingerprint, so a log file
    written in-tree would make every gate run exit 3. That is documented in
    prose; these tests make it executable.
    """
    lines = _gate_lines()
    start = next((i for i, l in enumerate(lines) if l.startswith("tree_fingerprint() {")), None)
    if start is None:
        raise AssertionError("gate.sh has no tree_fingerprint(); the layout changed")
    for i in range(start + 1, len(lines)):
        if lines[i] == "}":
            block = "\n".join(lines[start:i + 1]) + "\n"
            assert "porcelain" in block and "-uall" in block, (
                "tree_fingerprint no longer includes untracked paths; the INVALID "
                "RUN guarantee depends on `git status --porcelain -uall`"
            )
            return block
    raise AssertionError("tree_fingerprint() is never closed in gate.sh")


def _fingerprint_of(repo: Path) -> str:
    """Run the extracted tree_fingerprint against `repo`."""
    script = f'REPO={shlex.quote(str(repo))}\n' + extract_fingerprint() + '\ntree_fingerprint\n'
    proc = subprocess.run(["bash", "-c", script], capture_output=True, text=True, cwd=repo)
    assert proc.returncode == 0, f"tree_fingerprint failed: {proc.stderr}"
    return proc.stdout.strip()


@pytest.fixture
def scratch_repo(tmp_path):
    """A git repo with one commit, so HEAD resolves and status is meaningful."""
    subprocess.run(["git", "init", "-q"], cwd=tmp_path, check=True)
    (tmp_path / "tracked.txt").write_text("hello\n")
    subprocess.run(["git", "add", "tracked.txt"], cwd=tmp_path, check=True)
    subprocess.run(
        ["git", "-c", "user.email=t@e", "-c", "user.name=t", "commit", "-q", "-m", "init"],
        cwd=tmp_path, check=True,
    )
    return tmp_path


def test_fingerprint_is_stable_when_nothing_changes(scratch_repo):
    """The baseline: an untouched tree fingerprints identically every time."""
    assert _fingerprint_of(scratch_repo) == _fingerprint_of(scratch_repo)


def test_an_untracked_file_changes_the_fingerprint(scratch_repo):
    """The trap, stated as a test.

    This is why scripts/opencode-routing-check.sh writes its retry log to
    ${TMPDIR:-/tmp} and not into the repository. A log file created in-tree is an
    untracked path, `git status --porcelain -uall` reports it, the end-of-run
    fingerprint no longer matches the start, and the gate exits 3 INVALID RUN --
    on every run, for a file the harness itself created.
    """
    before = _fingerprint_of(scratch_repo)
    (scratch_repo / "airouter-opencode-routing.log").write_text("a run happened\n")
    after = _fingerprint_of(scratch_repo)
    assert before != after, (
        "an untracked log file did not change the fingerprint; writing the "
        "routing log in-tree would no longer trip INVALID RUN, so the "
        "out-of-tree path is now unnecessary rather than required"
    )


def test_touching_a_file_does_not_change_the_fingerprint(scratch_repo):
    """mtime is invisible to the fingerprint; only content is.

    Verified by hand on 2026-09-30 and then nearly lost: the hand probe
    appended a line to a file that was ALREADY modified for unrelated reasons,
    and the fingerprint did not move, which read as a bizarre result rather than
    as a signal that the probe was measuring the wrong thing. The claim is
    encoded here so it survives without anyone re-running it by hand.

    It matters practically: the gate's dist-freshness check touches files in
    dist/, and a recent mtime on a tracked file is therefore not evidence that
    the tree moved during a run.
    """
    before = _fingerprint_of(scratch_repo)
    (scratch_repo / "tracked.txt").touch()          # mtime only, no content change
    after = _fingerprint_of(scratch_repo)
    assert before == after, (
        "touching a file changed the fingerprint, so the fingerprint is "
        "mtime-sensitive; INVALID RUN verdicts are then at the mercy of editor "
        "and tool timestamps rather than content"
    )


def test_a_write_then_restore_leaves_the_fingerprint_unchanged(scratch_repo):
    """Pins a KNOWN BLIND SPOT, so the known-limit comment stays true.

    This is the case that cost a real triage on 2026-09-30: five gate stages
    failed at once because a tracked file was rewritten mid-run and then
    restored byte-identically, which the fingerprint cannot see -- it compares
    the tree at the start with the tree at the end.

    The assertion is deliberately the *undesirable* behaviour. Nobody wants a
    fingerprint blind to concurrent writes; this test exists so that the
    blindness is a measured, documented property rather than a surprise, and so
    that closing it cannot happen silently. If this test starts FAILING, the
    fingerprint has gained mtime or write-awareness, which is an improvement --
    update the KNOWN LIMIT comment on tree_fingerprint() and the triage note at
    the same time, because both currently promise this test passes.

    The obvious "fix" (any tracked mtime advancing => INVALID RUN) is NOT
    available: measured, the gate's own `config pytest` stage advances three
    tracked sources with byte-identical content, and a full run advances five.
    Such a rule would fire on every run, which is worse than the blind spot.
    """
    before = _fingerprint_of(scratch_repo)
    target = scratch_repo / "tracked.txt"
    original = target.read_text()
    target.write_text("rewritten mid-run\n")
    assert _fingerprint_of(scratch_repo) != before, (
        "the fingerprint did not move while a tracked file held different "
        "content; if this is new behaviour, the blind spot has been closed"
    )
    target.write_text(original)          # restored byte-identically
    assert _fingerprint_of(scratch_repo) == before, (
        "a write-then-restore no longer returns the fingerprint to its starting "
        "value. That is the blind spot being closed: update the KNOWN LIMIT "
        "comment on tree_fingerprint() in scripts/gate.sh, which currently "
        "states that this case is invisible."
    )


def test_a_modified_tracked_file_changes_the_fingerprint(scratch_repo):
    before = _fingerprint_of(scratch_repo)
    (scratch_repo / "tracked.txt").write_text("changed\n")
    assert _fingerprint_of(scratch_repo) != before


def test_invalid_run_names_the_paths_that_were_rewritten(scratch_repo):
    """The MTIME_SNAP diagnostic must actually NAME the rewritten paths.

    Unproven code in the one function that declares results void is the worst
    place for it: if the naming silently stopped working, whoever has to work out
    what moved the tree gets a bare "INVALID RUN" and starts guessing.

    The first version of this test asserted only that the exit status was 3 and
    that "INVALID RUN" was printed -- and it PASSED with the diagnostic neutered,
    because the name it checked was computed by the test itself rather than read
    out of gate.sh. So the assertion here is on gate.sh's OWN OUTPUT, and the
    real `tracked_mtimes` helper and the real summary block are driven against a
    real git repo, with the snapshot taken the way gate.sh takes it.
    """
    before = _fingerprint_of(scratch_repo)

    # Snapshot exactly as gate.sh does: the extracted helper, into a file.
    snap_file = scratch_repo / "mtime.snapshot"
    # Define AND invoke: leaving the call off silently produces an EMPTY snapshot
    # file, and `[ -s "$MTIME_SNAP" ]` then skips the whole diagnostic -- which is
    # exactly what happened the first time this test was written.
    snap_script = (
        f'REPO={shlex.quote(str(scratch_repo))}\n'
        + _tracked_mtimes_helper()
        + "tracked_mtimes\n"
    )
    proc = subprocess.run(["bash", "-c", snap_script], capture_output=True, text=True)
    assert proc.returncode == 0, f"the snapshot helper failed: {proc.stderr}"
    assert proc.stdout.strip(), "the snapshot came back empty, so the scenario is vacuous"
    snap_file.write_text(proc.stdout)

    target = scratch_repo / "tracked.txt"
    original = target.read_text()
    # Rewrite AND restore, then bump the mtime deterministically with os.utime.
    # Relying on wall-clock here was a race: two writes a few ms apart land in
    # the same clock tick often enough to make the test flaky in the direction
    # that HIDES a real regression. An explicit timestamp removes the race and
    # still models the write-then-restore the diagnostic is meant to name.
    target.write_text("rewritten during the run\n")
    _bump_mtime(target)
    after = _fingerprint_of(scratch_repo)
    assert after != before, "precondition: the write is visible to the fingerprint"

    prefix, summary = extract_stage_machine_parts()
    script = (
        "#!/usr/bin/env bash\n"
        f'P="gate:"\nSTART_FP={shlex.quote(before)}\nEND_FP={shlex.quote(after)}\n'
        f'REPO={shlex.quote(str(scratch_repo))}\n'
        f'MTIME_SNAP={shlex.quote(str(snap_file))}\n'
        + _tracked_mtimes_helper()
        + prefix.replace("REGISTERED=__N__", "REGISTERED=1")
        + 'run "a stage" true\n'
        + summary
    )
    proc = subprocess.run(["bash", "-c", script], capture_output=True, text=True)
    out = proc.stdout + proc.stderr

    assert proc.returncode == 3, f"a moved tree did not exit 3: {proc.returncode}\n{out}"
    assert "INVALID RUN" in out, out
    # The assertion that matters, and the one the first version got wrong: the
    # rewritten path must be named by GATE.SH's diagnostic, not by this test.
    assert "paths rewritten during the run" in out, (
        "the INVALID RUN path did not print the mtime diagnostic at all:\n" + out
    )
    assert "tracked.txt" in out, (
        "the diagnostic ran but did not name the rewritten path (so it is "
        "useless to whoever has to investigate):\n" + out
    )
    target.write_text(original)
    _bump_mtime(target)



def test_gate_reports_invalid_run_when_the_tree_moved(scratch_repo):
    """A start/end fingerprint mismatch must exit 3, not 1 and not 0.

    3 is distinct from 1 on purpose: the checks may all have passed, but they
    describe a repository state that no longer exists, and reporting that as a
    pass or a fail is equally wrong.
    """
    before = _fingerprint_of(scratch_repo)
    (scratch_repo / "sneaky.txt").write_text("appeared mid-run\n")
    after = _fingerprint_of(scratch_repo)
    assert before != after
    prefix, summary = extract_stage_machine_parts()
    script = (
        "#!/usr/bin/env bash\n"
        f'P="gate:"\nSTART_FP={shlex.quote(before)}\nEND_FP={shlex.quote(after)}\n'
        + prefix.replace("REGISTERED=__N__", "REGISTERED=1")
        + 'run "a stage" true\n'
        + summary
    )
    proc = subprocess.run(["bash", "-c", script], capture_output=True, text=True)
    out = proc.stdout + proc.stderr
    assert proc.returncode == 3, f"a moved tree did not exit 3, got {proc.returncode}:\n{out}"
    assert "INVALID RUN" in out, out


def test_the_routing_log_is_written_outside_the_repository():
    """The harness must not default its log anywhere inside the checkout.

    Asserted against the script text because the failure it prevents is not
    observable from a passing run -- the run passes either way, and the gate
    turns INVALID RUN only on the NEXT run. Reading the default is the only way
    to catch it before it costs someone a confusing red gate.
    """
    text = ROUTING_CHECK.read_text()
    match = re.search(r'^ROUTING_LOG="\$\{OPENCODE_ROUTING_LOG:-\$\{TMPDIR:-/tmp\}/([^"]+)\}"',
                      text, re.M)
    assert match, (
        "the ROUTING_LOG default changed shape; it must stay overridable and must "
        "default to a path outside the checkout"
    )
    default_path = f"${{TMPDIR:-/tmp}}/{match.group(1)}"
    assert not default_path.startswith(str(REPO)), default_path
    assert "/logs/" not in default_path and not default_path.endswith("/logs"), (
        f"the routing log defaults to {default_path}, which is a plausible place "
        "for a repo-relative logs/ directory to creep back in"
    )


# ---------------------------------------------------------------- gate.sh ----

def _gate_lines() -> list[str]:
    return GATE.read_text().splitlines()


def extract_guard() -> str:
    """The accounting guard exactly as it appears in scripts/gate.sh.

    Spans from the ACCOUNTED assignment to the end of the second `fi`, which
    closes the REGISTERED==0 branch followed by the mismatch branch. Returns the
    verbatim text; the caller runs it.
    """
    lines = _gate_lines()
    start = None
    closes = 0
    for i, line in enumerate(lines):
        if start is None and line.startswith("ACCOUNTED=$(("):
            start = i
            continue
        if start is not None and line == "fi":
            closes += 1
            if closes == 2:
                block = "\n".join(lines[start:i + 1]) + "\n"
                assert "ACCOUNTING MISMATCH" in block, (
                    "extracted guard has no mismatch message; the marker moved"
                )
                assert "NO STAGES REGISTERED" in block, (
                    "extracted guard has no zero-stages branch; it was removed"
                )
                return block
    raise AssertionError(
        "could not extract the accounting guard from scripts/gate.sh: no "
        "ACCOUNTED=$(( assignment followed by two `fi` closers. The guard was "
        "deleted or restructured."
    )


def _bump_mtime(path, seconds=1):
    """Advance a file's mtime deterministically, so a test never races the clock."""
    st = path.stat()
    os.utime(path, ns=(st.st_atime_ns, st.st_mtime_ns + seconds * 10**9))


def _tracked_mtimes_helper() -> str:
    """gate.sh's tracked_mtimes(), verbatim, including its REPO-dependent body."""
    text = GATE.read_text()
    start = text.find("tracked_mtimes() {")
    if start == -1:
        raise AssertionError("gate.sh has no tracked_mtimes(); the layout changed")
    end = text.find("\n}", start)
    if end == -1:
        raise AssertionError("tracked_mtimes() is never closed")
    return text[start:end + 2] + "\n"


def extract_stage_machine_parts() -> tuple[str, str]:
    """(prefix, summary) — the gate's arithmetic, verbatim, in two halves.

    Together these are what registers a stage, what each helper counts, and what
    the summary line reports. Extracting them lets a test run a synthetic stage
    table through the real code instead of reimplementing the counting -- which
    is the only version that notices when the real counting is wrong.

    Two halves, not one block, because the summary ENDS the file with `exit 0`.
    Concatenated as a single string and then handed stage invocations, the
    script exits at the summary and the stages never run: the accounting guard
    reports 0 of N accounted for and the test fails for a reason that has
    nothing to do with what it is testing. Stages belong between the two.
    """
    lines = _gate_lines()

    def index_of(predicate, what):
        for i, line in enumerate(lines):
            if predicate(line):
                return i
        raise AssertionError(f"gate.sh has no {what}; the layout changed")

    def function(name: str) -> list[str]:
        """One helper's body, brace-matched from `name() {` to the first `^}`.

        Brace matching rather than a line range on purpose. Taking everything
        between the helper definitions and the summary ALSO takes every
        `run "..."` stage invocation in between, so the extracted "machine"
        re-ran the entire gate -- including recovery-check and mutation-check --
        and the test timed out rather than failing. Selecting the three named
        functions explicitly cannot pick up a stage call.
        """
        start = index_of(lambda l, n=name: l.startswith(f"{n}() {{"), f"{name}() helper")
        for i in range(start + 1, len(lines)):
            if lines[i] == "}":
                return lines[start:i + 1]
        raise AssertionError(f"{name}() is never closed in gate.sh")

    counters_start = index_of(lambda l: l.startswith("PASSED=0"), "PASSED=0 counter init")
    registered = index_of(lambda l: l.startswith("REGISTERED="), "REGISTERED computation")
    summary_start = index_of(
        lambda l: l.startswith("printf 'gate: %d passed"), "summary printf"
    )
    assert counters_start < registered < summary_start, (
        f"gate.sh sections are out of the expected order: counters={counters_start} "
        f"registered={registered} summary={summary_start}"
    )
    prefix = "\n".join(
        lines[counters_start:registered + 1]
        + ["REGISTERED=__N__"]          # the real grep counts gate.sh's own stages
        + function("run")
        + function("fmt_ok")
        + function("run_live")
    )
    summary = "\n".join(lines[summary_start:])
    for name, text in (("prefix", prefix), ("summary", summary)):
        assert "ACCOUNTING MISMATCH" in text or name == "prefix", (
            f"the {name} lost the accounting guard"
        )
    assert "ACCOUNTING MISMATCH" in summary, "summary block lost the accounting guard"
    assert "stages registered" in summary, "summary no longer reports the stage total"
    # A stage invocation would mean the extraction is no longer just the machine.
    for text in (prefix, summary):
        leaked = [l for l in text.splitlines()
                  if re.match(r'^(?:run|run_live|fmt_ok) "', l)]
        assert not leaked, f"stage invocations leaked into the extraction: {leaked}"
    return prefix + "\n", summary + "\n"


def extract_stage_machine() -> str:
    """Both halves concatenated. For parse checks only -- do not append stages."""
    prefix, summary = extract_stage_machine_parts()
    return prefix + summary


def _drive(prefix: str, summary: str, body: str, registered: int) -> subprocess.CompletedProcess:
    """Compose a runnable script: prefix, then `body` (the stages), then summary.

    `body` is placed between the halves because the summary exits. The fake
    fingerprint pair keeps the INVALID RUN branch out of the way.
    """
    script = (
        "#!/usr/bin/env bash\n"
        'P="gate:"\nSTART_FP=same\nEND_FP=same\n'
        + prefix.replace("REGISTERED=__N__", f"REGISTERED={registered}")
        + body
        + summary
    )
    return subprocess.run(["bash", "-c", script], capture_output=True, text=True)


def run_stage_table(table: list[tuple[str, str]]) -> tuple[int, str]:
    """Run a synthetic stage table through gate.sh's real helpers and summary.

    `table` is (stage name, shell snippet) pairs; the snippet's exit status is
    what the helper would see. Returns (exit status, combined output). The
    stage total is set to len(table), so the accounting guard sees a run whose
    bookkeeping is internally consistent and only reports on the verdicts.
    """
    prefix, summary = extract_stage_machine_parts()
    body = "".join(
        f'run "{name}" bash -c {shlex.quote(snippet)}\n' for name, snippet in table
    )
    proc = _drive(prefix, summary, body, len(table))
    return proc.returncode, proc.stdout + proc.stderr


def run_guard(block: str, passed: int, failed: int, skipped: int, registered: int):
    """Drive the extracted guard with the four counters and report (rc, output)."""
    script = (
        "#!/usr/bin/env bash\n"
        f'PASSED={passed}\nFAILED={failed}\nSKIPPED={skipped}\n'
        f'REGISTERED={registered}\nP="gate:"\n'
        + block
    )
    proc = subprocess.run(["bash", "-c", script], capture_output=True, text=True)
    return proc.returncode, proc.stdout + proc.stderr


def test_gate_accounting_guard_is_present_and_parses():
    """The guard must exist and be syntactically valid, or it protects nothing."""
    block = extract_guard()
    proc = subprocess.run(["bash", "-n"], input=block, capture_output=True, text=True)
    assert proc.returncode == 0, f"extracted guard does not parse: {proc.stderr}"


@pytest.mark.parametrize(
    "passed,failed,skipped,registered",
    [
        (28, 0, 0, 28),   # the real shape: everything passed
        (27, 0, 1, 28),   # one live stage skipped for want of credentials
        (27, 1, 0, 28),   # one failure
        (26, 1, 1, 28),   # a failure and a skip together
    ],
)
def test_accounting_guard_accepts_every_legitimate_outcome(passed, failed, skipped, registered):
    """Every combination the helpers can actually produce must stay quiet."""
    block = extract_guard()
    rc, out = run_guard(block, passed, failed, skipped, registered)
    assert rc == 0, f"{passed}/{failed}/{skipped} of {registered} was rejected:\n{out}"
    assert "MISMATCH" not in out and "NO STAGES" not in out, out


def test_accounting_guard_rejects_an_uncounted_stage():
    """A stage that ran without bumping a counter must not report as a pass."""
    block = extract_guard()
    rc, out = run_guard(block, 28, 0, 0, 29)
    assert rc == 1, f"an uncounted stage was accepted:\n{out}"
    assert "ACCOUNTING MISMATCH" in out, out
    assert "29 stages are registered" in out, out


def test_accounting_guard_rejects_zero_registered_stages():
    """0+0+0 == 0, so the mismatch test cannot see a gate that ran nothing.

    The reachable way to get here is the stage-grep failing to match -- a renamed
    helper, or registrations moved out of column 0 -- which silently disarms
    every stage while the summary still reads green. This is the check that
    makes that a loud failure instead.
    """
    block = extract_guard()
    rc, out = run_guard(block, 0, 0, 0, 0)
    assert rc == 1, f"a gate that registered nothing was accepted:\n{out}"
    assert "NO STAGES REGISTERED" in out, out


def test_registered_count_matches_the_helper_call_sites():
    """The gate computes REGISTERED with this exact grep; keep them in step.

    The 27-vs-28 confusion that prompted this guard was a reviewer's grep
    matching run and run_live and forgetting fmt_ok. Comparing a computed total
    against the call sites is the version that cannot drift.
    """
    text = GATE.read_text()
    counted = len(re.findall(r"^(?:run |run_live |fmt_ok )\"", text, re.M))
    proc = subprocess.run(
        ["grep", "-cE", "^(run |run_live |fmt_ok )\"", str(GATE)],
        capture_output=True, text=True,
    )
    assert proc.returncode == 0, f"the gate's own stage-count grep failed: {proc.stderr}"
    assert int(proc.stdout.strip()) == counted, (
        f"the gate would compute {proc.stdout.strip()} but the call sites number "
        f"{counted}; the grep and the registrations have diverged"
    )
    assert counted > 0, "no stages are registered at all"


def test_no_test_in_this_file_is_assertion_free():
    """Non-vacuity, for this suite, in the spirit of scripts/test-suite-check.py.

    `scripts/test-suite-check.py` applies that rule to the Go suite and nothing
    applies it to the Python ones: SUITE_TEST_FLOORS counts test FUNCTIONS, so a
    body emptied of its assertions still satisfies its floor. A test that cannot
    fail is not evidence, and it is worse than no test, because it is counted.

    The check is textual and therefore approximate -- it answers "does this body
    contain a way to fail", not "do its assertions test what its name claims".
    That approximation is the same one test-suite-check.py documents and accepts;
    the alternative, no rule at all, is strictly worse. A test that genuinely
    cannot assert (a fixture, a smoke call) would need an exemption here, and
    there are none today.
    """
    source = Path(__file__).read_text()
    names = re.findall(r"^def (test_\w+)", source, re.M)
    assert len(names) >= 20, f"only {len(names)} tests found; the parse is wrong"
    for name in names:
        # Body = up to the next top-level def/class/section banner.
        match = re.search(rf"^def {name}\(.*?(?=^def |^@|^# ---|\Z)", source, re.M | re.S)
        assert match, f"{name}: could not locate its body"
        body = match.group(0)
        has_failure_path = (
            "assert " in body
            or "pytest.raises" in body
            or "pytest.fail" in body
            or "pytest.skip" in body
        )
        assert has_failure_path, (
            f"{name} has no assert/raises/fail/skip in its body: it cannot fail, "
            f"so it is not evidence of anything"
        )


def test_every_stage_helper_is_defined():
    """A helper that is called but never defined fails the run with 'not found'."""
    text = GATE.read_text()
    for helper in ("run", "run_live", "fmt_ok"):
        assert re.search(rf"^{helper}\(\) \{{", text, re.M), (
            f"{helper}() is called as a stage helper but is not defined"
        )


def test_stage_machine_extracts_and_parses():
    """The extracted helpers must be valid bash before any of them is trusted."""
    proc = subprocess.run(["bash", "-n"], input=extract_stage_machine(),
                          capture_output=True, text=True)
    assert proc.returncode == 0, f"extracted stage machine does not parse: {proc.stderr}"


def test_gate_is_green_when_every_stage_passes():
    rc, out = run_stage_table([("alpha", "true"), ("beta", "true"), ("gamma", "true")])
    assert rc == 0, f"an all-green table was rejected:\n{out}"
    assert "gate: 3 passed, 0 failed (3 stages registered)" in out, out


def test_gate_exits_nonzero_when_a_single_stage_fails():
    """One red stage must fail the run, and must not be averaged away.

    This is the case a count-based summary is most able to hide: the run still
    "ran", and a summary that reported only totals could read 3 of 3 somewhere
    else. The verdict has to come from the per-stage result.
    """
    rc, out = run_stage_table([("alpha", "true"), ("beta", "false"), ("gamma", "true")])
    assert rc == 1, f"a failing stage did not fail the gate:\n{out}"
    assert "gate: 2 passed, 1 failed (3 stages registered)" in out, out
    assert "failed: beta" in out, f"the failing stage was not named: {out}"


@pytest.mark.parametrize(
    "table,expected",
    [
        ([("a", "true")], "gate: 1 passed, 0 failed (1 stages registered)"),
        ([("a", "true"), ("b", "true")], "gate: 2 passed, 0 failed (2 stages registered)"),
        ([("a", "false"), ("b", "false"), ("c", "true")],
         "gate: 1 passed, 2 failed (3 stages registered)"),
        ([("a", "false")] * 5, "gate: 0 passed, 5 failed (5 stages registered)"),
    ],
)
def test_summary_counts_follow_the_stage_results(table, expected):
    """The summary must be computed from the results, never a baked-in number."""
    rc, out = run_stage_table(table)
    assert expected in out, f"expected {expected!r} in:\n{out}"
    assert rc == (1 if any(s == "false" for _, s in table) else 0), out


def test_fmt_ok_stage_counts_a_quiet_check_as_a_pass():
    """fmt_ok inverts its command: success is no output. It must still count."""
    prefix, summary = extract_stage_machine_parts()
    proc = _drive(prefix, summary, 'fmt_ok "quiet" true\n', 1)
    assert proc.returncode == 0, proc.stderr
    assert "gate: 1 passed, 0 failed (1 stages registered)" in proc.stdout, proc.stdout


def test_a_noisy_fmt_ok_stage_fails_the_run():
    """The inverse: output where silence was expected must be a failure."""
    prefix, summary = extract_stage_machine_parts()
    proc = _drive(prefix, summary, 'fmt_ok "noisy" echo "unexpected output"\n', 1)
    assert "FAIL" in proc.stdout, f"a noisy fmt_ok stage did not fail: {proc.stdout}"
    assert "gate: 0 passed, 1 failed" in proc.stdout, proc.stdout


def test_a_skipped_live_stage_is_neither_a_pass_nor_a_failure():
    """run_live's 77 must be counted separately and still leave the run green.

    A skip reported as a pass is how a live stage stops being run at all; a skip
    reported as a failure would block every keyless machine. Both mistakes are
    visible in the summary line, which is what is asserted here.
    """
    prefix, summary = extract_stage_machine_parts()
    body = 'run_live "live" bash -c "exit 77"\n'
    proc = _drive(prefix, summary, body, 1)
    out = proc.stdout
    assert proc.returncode == 0, f"a skipped live stage failed the gate:\n{out}"
    assert "gate: 0 passed, 0 failed, 1 skipped (1 stages registered)" in out, out
    assert "did NOT run on this machine" in out, out


# ------------------------------------------------------------- shell-lint ----

def make_lint_repo(tmp_path: Path) -> Path:
    """A throwaway git repo holding a copy of shell-lint and its rules.

    shell-lint resolves the repository from its own location and shells out to
    `git ls-files`, so testing it hermetically means giving it a real repo of
    its own rather than mocking git.
    """
    scripts = tmp_path / "scripts"
    scripts.mkdir(parents=True)
    shutil.copy2(SHELL_LINT, scripts / "shell-lint.sh")
    shutil.copy2(SHELL_RULES, scripts / "shell_lint_rules.py")
    subprocess.run(["git", "init", "-q"], cwd=tmp_path, check=True)
    # The driver and the rules are tracked, so they are the enforced set.
    subprocess.run(["git", "add", "scripts/shell-lint.sh", "scripts/shell_lint_rules.py"],
                   cwd=tmp_path, check=True)
    return tmp_path


def run_lint(repo: Path):
    proc = subprocess.run(["bash", str(repo / "scripts" / "shell-lint.sh")],
                          cwd=repo, capture_output=True, text=True)
    return proc.returncode, proc.stdout + proc.stderr


def test_shell_lint_notices_an_untracked_violation_without_failing(tmp_path):
    """The blind spot is real; the NOTICE must surface it and stay non-fatal."""
    repo = make_lint_repo(tmp_path)
    (repo / "scripts" / "brand-new.sh").write_text(VIOLATION)
    # Deliberately NOT git-added: that is the state a just-written script is in.
    rc, out = run_lint(repo)
    assert rc == 0, f"an untracked scratch file must not fail the build, got {rc}:\n{out}"
    assert "NOTICE" in out, f"the violation was swallowed:\n{out}"
    assert "brand-new.sh" in out, out
    assert "while" in out, out


def test_shell_lint_is_quiet_about_a_clean_untracked_file(tmp_path):
    """A clean new script is linted and reported as clean, with no NOTICE."""
    repo = make_lint_repo(tmp_path)
    (repo / "scripts" / "brand-new.sh").write_text(CLEAN)
    rc, out = run_lint(repo)
    assert rc == 0, out
    assert "NOTICE" not in out, f"a clean file produced a NOTICE:\n{out}"


def test_shell_lint_still_fails_a_tracked_violation(tmp_path):
    """The sweep must not have weakened the enforced path.

    If this ever goes green the NOTICE work has broken the linter: a committed
    parse error must still be a failure, tracked or not.
    """
    repo = make_lint_repo(tmp_path)
    bad = repo / "scripts" / "committed.sh"
    bad.write_text(VIOLATION)
    subprocess.run(["git", "add", "scripts/committed.sh"], cwd=repo, check=True)
    rc, out = run_lint(repo)
    assert rc == 1, f"a tracked parse violation was not enforced:\n{out}"
    assert "committed.sh" in out, out


def test_shell_lint_notice_names_the_file_not_a_scratch_elsewhere(tmp_path):
    """Only scripts/ is swept, and the message must identify the offending file.

    A NOTICE that says "some untracked file has a problem" sends the reader
    hunting; naming the path is the entire value of reporting it.
    """
    repo = make_lint_repo(tmp_path)
    (repo / "scripts" / "nested").mkdir()
    (repo / "nested").mkdir()
    (repo / "nested" / "outside.sh").write_text(VIOLATION)
    rc, out = run_lint(repo)
    assert rc == 0, out
    assert "outside.sh" not in out, f"a file outside scripts/ was swept:\n{out}"