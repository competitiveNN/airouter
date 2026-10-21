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
import time
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


def _batch_machinery_lines() -> list[str]:
    """gate.sh's batch machinery: its variables and its five helpers, verbatim.

    Everything from the banner that introduces it to just before `PASSED=0`.
    Extracted rather than restated so a test that drives the batch path is
    driving the real thing: if someone deletes a helper, or reorders the
    variables so they no longer precede the counters, this fails.
    """
    lines = _gate_lines()
    prefix = "# ---- WHERE A STAGE'S VERDICT COMES FROM"
    banner = next(
        i for i, l in enumerate(lines) if l.startswith(prefix)
    )
    counters = next(
        i for i, l in enumerate(lines) if l.startswith("PASSED=0")
    )
    assert counters > banner, "the batch machinery does not precede the counters"
    body = lines[banner:counters]
    for name in ("gate_defer", "gate_batch_begin", "gate_batch_end", "gate_report", "gate_batch_kill"):
        assert body.count(f"{name}() {{") == 1, f"the batch machinery lost {name}()"
    return body


def extract_stage_machine_parts(include_batch: bool = False) -> tuple[str, str]:
    """(prefix, summary) — the gate's arithmetic, verbatim, in two halves.

    include_batch prepends the concurrent-batch machinery (its variables and
    its five helpers) to the prefix, so a test can drive a stage table through
    the BATCH path rather than only the inline one. The default stays False
    because every existing test drives the inline path, and the inline path is
    the one the accounting guard's own synthetic table must not depend on the
    batch for -- otherwise a gate with no batch at all would fail them.

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
    batch_machine = []
    if include_batch:
        batch_machine = _batch_machinery_lines()

    prefix = "\n".join(
        batch_machine
        + lines[counters_start:registered + 1]
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

# ------------------------------------------------- the concurrent batch ----
#
# Everything above drives gate.sh's stage helpers INLINE. That is the path the
# accounting guard and the synthetic stage table exercise, and it is still the
# path a stage takes when it is not in a batch. The batch is a second path to
# the same verdicts, and a second path to the same verdicts is exactly where a
# gate grows a way to report green having checked nothing -- so it gets its
# own tests, and they compare the two paths against each other rather than
# against a paraphrase of either.

# The stages that are allowed to run inside the concurrent batch.
#
# This is an ALLOWLIST, not a denylist, and that is the whole point. A denylist
# of the stages known to write into the tree would let a NEW writer into the
# batch silently, which is the mistake that matters: a stage that rewrites a
# tracked file while another stage reads it produces a failure that looks like
# a defect and is not one. With an allowlist, a new stage has to be added here
# deliberately, and the diff says so.
#
# Each entry is safe because it READS the working tree and writes nothing into
# it. The two that pick their own listening port are here too: they are
# serialised against each other by $PORT_LOCK, which the test below checks.
BATCH_ALLOWLIST = {
    "go vet ./...",
    "gofmt -l . is clean",
    "go test -count=1",
    "go test -race -count=1",
    "fuzz-gate-selftest",
    # The audit selftests and checks sat here until their input went away:
    # the findings layer read docs/audit.md, an ephemeral artifact no audit
    # file is ever committed, and secret-scan moved to CI (.github/workflows/
    # ci.yml, where actions/checkout gives it a clean .git/config). What is
    # left of the audit story is the gate itself not going red for a finding
    # nobody can fix -- see the security comment in gate.sh. A stage that is
    # gone from gate.sh must leave this list too, or the list stops describing
    # the batch and the staleness test below stays red for that reason alone.
    "dist-freshness-selftest",
    "recovery-check-selftest",
    "test-suite-selftest",
    "opencode-routing-selftest",
    "shell-lint",
    "shell-lint-control",
    "smoke dashboard",
    "dist-freshness-check",
    "config rules check",
    "test-suite-check",
}

# The stages that WRITE into the working tree, and so must run alone. Listed
# here so the test can assert they are all OUTSIDE the batch, and that they
# are all present -- a stage that quietly disappeared is a stage that stopped
# being run, which is what the accounting guard exists to catch but which this
# list also catches from the other side.
EXCLUSIVE_STAGES = {
    "doc-verify-selftest",
    "sse-negative-check",
    "config pytest",
    "mutation preflight",
    "mutation-check",
    "doc-verify",
    "recovery-check (full)",
}


def _batch_region() -> list[str]:
    """The lines between gate_batch_begin and gate_batch_end, verbatim.

    The two call sites are matched as whole lines, so the DEFINITIONS
    (`gate_batch_begin() {`) are not mistaken for them.
    """
    lines = _gate_lines()
    begin = None
    end = None
    for i, line in enumerate(lines):
        if line.strip() == "gate_batch_begin":
            begin = i
        elif line.strip() == "gate_batch_end":
            end = i
            break
    assert begin is not None, "gate.sh never opens a batch"
    assert end is not None, "gate.sh never closes a batch"
    assert end > begin, "the batch is closed before it is opened"
    return lines[begin:end]


def _stages_in(lines: list[str]) -> list[str]:
    """The stage names registered in these lines, in order."""
    return re.findall(r'^(?:run|run_live|fmt_ok) "([^"]+)"', "\n".join(lines), re.M)


def test_only_read_only_stages_run_inside_the_batch():
    """The batch may contain only stages that write nothing into the tree.

    The failure this prevents is the expensive one to debug: a stage that
    rewrites a tracked file while a concurrent stage is reading it, producing
    a red gate that has nothing to do with the code and that reproduces only
    sometimes. An allowlist turns that from "who thought of that" into "the
    diff says the stage was added to the list".
    """
    inside = set(_stages_in(_batch_region()))
    unexpected = inside - BATCH_ALLOWLIST
    assert not unexpected, (
        f"these stages are inside the concurrent batch but are not on the "
        f"allowlist of read-only stages: {sorted(unexpected)}. A stage that "
        f"writes into the working tree must run alone."
    )
    # And the allowlist must not rot: a stage that left the batch has to be
    # taken off the list, or the list stops describing the batch.
    stale = BATCH_ALLOWLIST - inside
    assert not stale, (
        f"the batch allowlist names stages that are no longer in the batch: "
        f"{sorted(stale)}. Remove them, or the list stops being a description "
        f"of what runs concurrently."
    )


def test_every_exclusive_stage_is_outside_the_batch():
    """The stages that write into the tree must all be outside the batch."""
    inside = set(_stages_in(_batch_region()))
    assert not (inside & EXCLUSIVE_STAGES), (
        f"tree-writing stages are inside the concurrent batch: "
        f"{sorted(inside & EXCLUSIVE_STAGES)}"
    )
    # ...and they must all still be registered somewhere. A stage that vanished
    # from the file is a stage that stopped running, and the accounting guard
    # would notice the total but not the intent.
    all_stages = set(_stages_in(_gate_lines()))
    missing = EXCLUSIVE_STAGES - all_stages
    assert not missing, f"these exclusive stages are no longer registered: {sorted(missing)}"


def test_the_port_picking_stages_are_serialised_against_each_other():
    """Two stages that pick their own free port must not run at the same time.

    Each of them binds port 0, reads the port the kernel gave it, and closes
    it. Two of them doing that in the same instant can be handed the SAME
    port, and the loser fails with "port already in use" -- a verdict about the
    machine, not about the code, and a red gate that reproduces only sometimes.
    $PORT_LOCK is what keeps them apart.
    """
    text = GATE.read_text()
    for stage in ("smoke dashboard", "opencode-routing-selftest"):
        line = next(
            (l for l in text.splitlines() if l.startswith(f'run "{stage}" ')),
            None,
        )
        assert line is not None, f"the stage {stage!r} is not registered"
        assert 'flock -x "$PORT_LOCK"' in line, (
            f"the stage {stage!r} picks its own port and is not serialised "
            f"against the other one: {line!r}"
        )


def test_the_port_lock_is_not_the_gate_lock():
    """The port lock must be its own file, or it would serialise the whole run.

    $LOCK is held for the entire gate run, so a stage that took it would wait
    for the run that is waiting for it.
    """
    text = GATE.read_text()
    m = re.search(r'^PORT_LOCK="([^"]+)"', text, re.M)
    assert m, "gate.sh has no PORT_LOCK"
    l = re.search(r'^LOCK="([^"]+)"', text, re.M)
    assert l, "gate.sh has no LOCK"
    assert m.group(1) != l.group(1), "the port lock and the gate lock are the same file"
    assert m.group(1) != "${TMPDIR:-/tmp}/airouter-gate.lock", (
        "the port lock is the gate lock"
    )


def _drive_batch(prefix: str, summary: str, body: str, registered: int):
    """Compose a runnable script with a batch around the body."""
    script = (
        "#!/usr/bin/env bash\n"
        'P="gate:"\nSTART_FP=same\nEND_FP=same\n'
        + prefix.replace("REGISTERED=__N__", f"REGISTERED={registered}")
        + body
        + summary
    )
    return subprocess.run(["bash", "-c", script], capture_output=True, text=True)


def run_stage_table_batched(table: list[tuple[str, str]]):
    """Run a synthetic stage table through the BATCH path of the real helpers."""
    prefix, summary = extract_stage_machine_parts(include_batch=True)
    body = (
        "gate_batch_begin\n"
        + "".join(f'run "{name}" bash -c {shlex.quote(snippet)}\n' for name, snippet in table)
        + "gate_batch_end\n"
    )
    proc = _drive_batch(prefix, summary, body, len(table))
    return proc.returncode, proc.stdout + proc.stderr


def test_the_batch_path_and_the_inline_path_agree_on_every_verdict():
    """The two paths must be indistinguishable, stage for stage.

    The batch exists to save wall-clock time. It must not change WHAT the gate
    says. The strongest form of that claim is a comparison: the same table,
    through both paths, must produce the same summary and the same per-stage
    lines. Anything else -- a stage the batch forgets to count, a failure the
    batch reports as a pass -- shows up here as a difference.
    """
    table = [
        ("alpha", "true"),
        ("beta", "false"),
        ("gamma", "true"),
        ("delta", "echo boom >&2; exit 3"),
    ]
    serial_rc, serial_out = run_stage_table(table)
    batch_rc, batch_out = run_stage_table_batched(table)
    assert serial_rc == batch_rc, (
        f"the batch changed the verdict: inline exited {serial_rc}, batch {batch_rc}\n"
        f"inline:\n{serial_out}\nbatch:\n{batch_out}"
    )
    for line in ("gate: 2 passed, 2 failed (4 stages registered)", "failed: beta delta"):
        assert line in serial_out, f"the inline path lost {line!r}:\n{serial_out}"
        assert line in batch_out, f"the batch path lost {line!r}:\n{batch_out}"
    # And the per-stage verdicts, in order.
    def verdicts(out: str) -> list[str]:
        return [l.strip() for l in out.splitlines() if l.strip().startswith(("PASS", "FAIL", "SKIP"))]
    assert verdicts(batch_out) == verdicts(serial_out), (
        f"the batch printed different verdicts:\ninline: {verdicts(serial_out)}\n"
        f"batch: {verdicts(batch_out)}"
    )


def test_a_failing_stage_in_a_batch_fails_the_run_and_is_named():
    """A red stage inside a batch must still be red, and still be named."""
    rc, out = run_stage_table_batched([("alpha", "true"), ("beta", "false")])
    assert rc == 1, f"a failing batch stage did not fail the run:\n{out}"
    assert "gate: 1 passed, 1 failed (2 stages registered)" in out, out
    assert "failed: beta" in out, f"the failing batch stage was not named:\n{out}"


def test_a_skipped_live_stage_in_a_batch_is_still_a_skip():
    """run_live's 77 must stay a skip inside a batch.

    A skip counted as a pass is how a live stage stops being run at all, and
    the batch has its own verdict path, so the skip rule has to be proven
    there too rather than only on the inline path.
    """
    prefix, summary = extract_stage_machine_parts(include_batch=True)
    body = "gate_batch_begin\nrun_live \"live\" bash -c 'exit 77'\ngate_batch_end\n"
    proc = _drive_batch(prefix, summary, body, 1)
    out = proc.stdout + proc.stderr
    assert "SKIP" in out, f"a 77 exit inside a batch was not a skip:\n{out}"
    assert "1 skipped" in out, f"the skip was not counted as a skip:\n{out}"
    assert "1 passed" not in out, f"the skip was counted as a pass:\n{out}"


def test_the_batch_actually_runs_stages_concurrently():
    """The batch must be concurrent, or the whole change is decoration.

    Four stages that each sleep 0.6s take 2.4s one after another and about
    0.6s together. The threshold sits between the two with room on both sides:
    a serial batch fails it, and a loaded machine does not trip it.
    """
    table = [(f"slow{i}", "sleep 0.6") for i in range(4)]
    prefix, summary = extract_stage_machine_parts(include_batch=True)
    body = (
        "gate_batch_begin\n"
        + "".join(f'run "{name}" bash -c {shlex.quote(snippet)}\n' for name, snippet in table)
        + "gate_batch_end\n"
    )
    started = time.monotonic()
    proc = _drive_batch(prefix, summary, body, len(table))
    elapsed = time.monotonic() - started
    assert proc.returncode == 0, f"the batched run failed:\n{proc.stdout}\n{proc.stderr}"
    assert elapsed < 1.5, (
        f"four 0.6s stages took {elapsed:.2f}s in a batch; that is the serial "
        f"time, so the batch is not running them concurrently"
    )


def test_the_batch_counts_a_stage_that_never_ran():
    """A stage that cannot start must not vanish from the accounting.

    If the batch lost a stage, the total would be short and the accounting
    guard would catch it -- but the guard is at the END of the file, after
    everything else has already been decided. A batch that dropped a stage
    would still print the other verdicts. So the total is checked here too.
    """
    prefix, summary = extract_stage_machine_parts(include_batch=True)
    body = (
        "gate_batch_begin\n"
        'run "alpha" bash -c true\n'
        'run "beta" bash -c false\n'
        "gate_batch_end\n"
    )
    proc = _drive_batch(prefix, summary, body, 2)
    out = proc.stdout + proc.stderr
    assert "gate: 1 passed, 1 failed (2 stages registered)" in out, out
    assert "ACCOUNTING MISMATCH" not in out, out
