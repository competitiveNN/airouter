#!/usr/bin/env python3
"""Tests for scripts/mutation-check.py — the harness that grades the tests.

A mutation harness that can report green while doing nothing is worse than no
harness: it is a check whose own correctness is never checked, which is exactly
how the hand-run version of this one reported MISSED for a defect that was never
planted (the anchor had moved) and MISSED for a test that no longer existed (a
scripted rewrite had dropped it).

So the harness's own guarantees are asserted here, against a temp copy of a real
source file — never against the repository, since these tests deliberately make
the harness edit things.
"""

import importlib.util
import json
import re
import shutil
import subprocess
import sys
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parent.parent
HARNESS = REPO / "scripts" / "mutation-check.py"


def _load():
    spec = importlib.util.spec_from_file_location("mutation_check", HARNESS)
    module = importlib.util.module_from_spec(spec)
    # Register before executing: the harness defines a frozen dataclass, and
    # dataclasses resolves field types through sys.modules[cls.__module__].
    # Without this, collection dies with an AttributeError that a wrapper around
    # pytest reports as "No tests collected" — which is a very quiet way to
    # ship a test file that never runs.
    sys.modules["mutation_check"] = module
    spec.loader.exec_module(module)
    return module


mc = _load()


def test_the_graded_suites_still_hold_their_tests():
    """Losing even one test function fails, and says what to do if it was meant.

    The floors live in scripts/mutation-check.py next to test_inventory(), so
    the count and the floor are maintained in one place.
    """
    inventory = mc.test_inventory()
    for suite, floor in mc.SUITE_TEST_FLOORS.items():
        assert inventory[suite] >= floor, (
            f"{suite} has {inventory[suite]} tests, floor is {floor}. Tests were "
            f"lost — check for a write that reported success without landing, or a "
            f"rewrite from a stale read. If the removal was deliberate, lower the "
            f"floor in scripts/mutation-check.py in the same commit and say why."
        )


def test_every_test_suite_has_a_floor():
    """A new suite must not be able to skip the floor by not being listed.

    Otherwise `scripts/test_fetch_free_models.py` could be deleted outright and
    the inventory would simply stop mentioning it: a floor nobody is compared
    against is a comment, not a check.
    """
    on_disk = {
        f"scripts/{p.name}"
        for p in sorted((REPO / "scripts").glob("test_*.py"))
    }
    assert on_disk == set(mc.SUITE_TEST_FLOORS), (
        f"suites on disk {sorted(on_disk)} vs floored "
        f"{sorted(mc.SUITE_TEST_FLOORS)} — add the new suite (and its floor) to "
        f"SUITE_TEST_FLOORS in scripts/mutation-check.py"
    )


def test_preflight_passes_on_the_current_tree():
    """Every mutation must be anchored, or the sweep would report fiction."""
    assert mc.preflight() == []
    inventory = mc.test_inventory()
    assert sum(inventory.values()) >= 50, inventory
    assert len(mc.MUTATIONS) >= 10, "the sweep has shrunk; is that deliberate?"


def test_preflight_fails_when_a_suite_is_below_its_floor(monkeypatch):
    """`--preflight` must ENFORCE the floors, not just print a hold-count.

    This is a false-green the harness was caught in on 2026-09-30: with three
    test functions deleted from a suite whose floor was 22, `--preflight` printed
    "suites hold 102 tests" and exited 0. It reported a number it never compared
    to anything, and SUITE_TEST_FLOORS lived in the same file, one function
    away, unread.

    The floor is raised to an impossible value instead of deleting files, so the
    check runs against the real tree and edits nothing.
    """
    monkeypatch.setitem(mc.SUITE_TEST_FLOORS, "scripts/test_mutation_check.py", 10_000)
    shortfalls = mc.floor_shortfalls(mc.test_inventory())
    assert shortfalls, "floor_shortfalls found no shortfall at a floor of 10000"
    assert any("test_mutation_check" in s for s in shortfalls), shortfalls
    assert mc.main(["--preflight"]) == 1, "preflight exited 0 with an impossible floor"


def test_preflight_passes_when_every_suite_meets_its_floor():
    """The other direction: a healthy tree must not be made to fail.

    A guard that always fires is not a guard, it is an outage. This is the
    counterpart to the test above and matters just as much.
    """
    assert mc.floor_shortfalls(mc.test_inventory()) == []
    assert mc.main(["--preflight"]) == 0


def test_preflight_reports_a_mutation_whose_anchor_moved():
    """A refactor that moves the line must fail loudly, not silently miss."""
    # The replacement text has to be absent from the tree too, or preflight
    # correctly reads this as "a killed sweep left the mutation applied"
    # rather than "the anchor moved" — two different problems, and this test is
    # about the second one.
    stale = mc.Mutation(
        "anchor that no longer exists", "regenerate_config.py",
        "this text is not in the file", "<not present anywhere in the tree>",
        "a defect whose anchor moved",
    )
    mc.MUTATIONS.append(stale)
    try:
        problems = mc.preflight()
    finally:
        mc.MUTATIONS.remove(stale)
    assert any("anchor not found" in p for p in problems), problems


def test_preflight_reports_a_non_unique_anchor():
    """An anchor that appears twice makes the patch ambiguous."""
    dup = mc.Mutation(
        "ambiguous anchor", "regenerate_config.py",
        "        return 'unknown'", "        return 'live'",
        "an anchor that is not unique",
    )
    mc.MUTATIONS.append(dup)
    try:
        problems = mc.preflight()
    finally:
        mc.MUTATIONS.remove(dup)
    assert any("must be unique" in p for p in problems), problems


# One removed test per suite, asserted to fail the floor. Parametrised so a
# suite cannot be added to the floors without also being covered here: a floor
# that is only ever exercised against one file is a floor that quietly stops
# meaning anything for the others.
#
# Each case edits the real suite file, so the finally that restores it is load
# bearing — a red run must never leave a test suite with a test missing.
@pytest.mark.parametrize("suite,anchor", [
    ("scripts/test_regenerate_config.py", "def test_sort_models_returns_best_first():"),
    ("scripts/test_fetch_free_models.py", "def test_a_timeout_is_unknown_not_dead():"),
    ("scripts/test_mutation_check.py", "def test_preflight_passes_on_the_current_tree():"),
])
def test_removing_one_test_from_any_suite_fails_the_floor(suite, anchor):
    path = REPO / suite
    original = path.read_text()
    start = original.index(anchor)
    end = original.index("\ndef ", start + 1) + 1
    try:
        path.write_text(original[:start] + original[end:])
        with pytest.raises(AssertionError, match="Tests were lost"):
            test_the_graded_suites_still_hold_their_tests()
    finally:
        path.write_text(original)
    # And the floor passes again once the test is back.
    test_the_graded_suites_still_hold_their_tests()


def _with_mutations(replacements):
    """Swap MUTATIONS for the duration of a test, then put the real list back.

    `.clear()` looked tidy and was wrong: MUTATIONS is the harness's catalogue
    for the whole session, so clearing it made every later test in the file see
    an empty one and fail in a way that had nothing to do with what it was
    testing. Order dependence in a test about self-checks is a fine way to
    waste an afternoon.
    """
    saved = list(mc.MUTATIONS)
    mc.MUTATIONS[:] = replacements
    return lambda: mc.MUTATIONS.__setitem__(slice(None), saved)


def _sandbox(tmp_path):
    """A writable copy of a real source file the harness is allowed to edit."""
    target = tmp_path / "victim.py"
    shutil.copyfile(REPO / "regenerate_config.py", target)
    return str(target)


def test_a_no_op_patch_is_refused_rather_than_reported_missed(tmp_path):
    """The failure that made the hand-run sweep lie.

    `old == new` produces identical bytes. The suite then passes, the sweep
    prints MISSED, and a reader concludes the tests cannot catch the defect —
    when in fact there was no defect at all. The harness must stop instead.
    """
    restore = _with_mutations([mc.Mutation(
        "no-op patch", _sandbox(tmp_path),
        "AUTO_PROBE_MAX_AGE_DAYS = 7", "AUTO_PROBE_MAX_AGE_DAYS = 7",
        "proves nothing",
    )])
    try:
        with pytest.raises(SystemExit, match="no-op"):
            mc.main(["--only", "no-op patch"])
    finally:
        restore()


def test_a_missed_mutation_fails_the_run(tmp_path):
    """A defect the suite does not catch must exit non-zero.

    This is the assertion the hand-run version never made: it printed a count
    and relied on a human reading it correctly.
    """
    path = _sandbox(tmp_path)
    text = Path(path).read_text()
    # Touch a docstring: the suite is green before and after, so this is a
    # mutation the tests genuinely cannot see.
    doc_anchor = "# ── Size-tier heuristics"
    assert doc_anchor in text
    restore = _with_mutations([mc.Mutation(
        "docstring only", path, doc_anchor, "# a harmless comment change",
        "not observable by the suite",
    )])
    try:
        assert mc.main(["--only", "docstring only"]) == 1
    finally:
        restore()


def test_the_sweep_restores_every_file_it_touches():
    """Byte-for-byte, checked after the fact rather than assumed.

    This one mutates the REAL file, because the suite imports the real file and
    a copy would be green either way — which is the mistake the previous
    version of this test made. The finally block is belt and braces: if the
    harness ever failed to restore, this test puts the tree back before failing,
    so a red run never leaves a sabotaged repo behind.
    """
    path = REPO / "regenerate_config.py"
    original = path.read_text()
    restore = _with_mutations([mc.Mutation(
        "caught defect", "regenerate_config.py",
        "        allow_abbrev=False,", "        allow_abbrev=True,",
        "a typo'd flag would be accepted",
    )])
    try:
        assert mc.main(["--only", "caught defect"]) == 0
    finally:
        restore()
        if path.read_text() != original:
            path.write_text(original)
            pytest.fail("the harness left regenerate_config.py modified")
    assert path.read_text() == original


def test_a_suite_that_is_not_green_blocks_the_sweep(tmp_path, monkeypatch):
    """If the baseline is red, "the suite failed" proves nothing about a mutation."""
    restore = _with_mutations([mc.Mutation(
        "anything", _sandbox(tmp_path), "x = 1", "x = 2", "irrelevant",
    )])
    monkeypatch.setattr(mc, "run_suite", lambda **kw: type(
        "R", (), {"returncode": 1, "stdout": "pre-existing failure"}
    )())
    try:
        assert mc.main([]) == 1
    finally:
        restore()


# --------------------------------------------------------------------------
# The check-rules CLI must be a gate, not a report.
#
# check-rules.py is what the nightly sync consults before accepting a config, so
# every diagnostic class it can emit needs a planted violation that proves the
# class still fails the run and still names itself. A new rule added to the
# checker without such a case would otherwise be shipped untested: it would
# report nothing, and nothing is indistinguishable from "everything is fine".
# Each case asserts the exit code AND the message prefix, so a rule that fires
# with a useless diagnostic fails here too.
# --------------------------------------------------------------------------

def _in_profile(text, profile, old, new):
    """Apply a replacement inside ONE profile's chain block.

    Several of the strings below (a model id, a provider line) occur in more
    than one chain, and a case that silently lands in the wrong profile is a
    case that tests nothing — one of them was a no-op for exactly this reason.
    """
    pattern = re.compile(rf"(?ms)(^  {re.escape(profile)}:\n    chain:\n)(.*?)(?=^  \w+:|\Z)")
    m = pattern.search(text)
    assert m, f"no block for {profile}"
    block = m.group(2)
    assert old in block, f"{old!r} not in the {profile} chain"
    return text[:m.start(2)] + block.replace(old, new, 1) + text[m.end(2):]


def _drop_provider_entry(text, profile, provider):
    """Remove the first `provider` entry from `profile`'s chain.

    Derived from the live config rather than hardcoded. Three of these cases
    used to spell out the model id, score and context of the entry they removed,
    which made them silently no-ops the moment a regeneration changed chain
    composition -- a case that plants nothing and then "passes" the suite it was
    supposed to be breaking is worse than no case at all.
    """
    pattern = re.compile(rf"(?ms)(^  {re.escape(profile)}:\n    chain:\n)(.*?)(?=^  \w+:|\Z)")
    m = pattern.search(text)
    assert m, f"no block for {profile}"
    block = m.group(2)
    entry = re.compile(
        r"(?m)^      - provider: " + re.escape(provider) + r"\n"
        r"(?:^(?!      - provider: ).*\n)*"
    )
    new_block, n = entry.subn("", block, count=1)
    assert n == 1, f"{provider} has no entry in the {profile} chain to drop"
    return text[:m.start(2)] + new_block + text[m.end(2):]


def _score_below_floor(text, profile, floor):
    """Rewrite the first scored entry of `profile` to an intelligence below `floor`.

    Picks a real entry out of the live chain instead of naming one, so the case
    keeps planting a violation as the generator's composition changes.
    """
    pattern = re.compile(rf"(?ms)(^  {re.escape(profile)}:\n    chain:\n)(.*?)(?=^  \w+:|\Z)")
    m = pattern.search(text)
    assert m, f"no block for {profile}"
    block = m.group(2)
    entry = re.compile(
        r"(?ms)^(      - provider: \w+\n        model: [^\n]*\n"
        r"        vision: (?:true|false)\n        intelligence: )([0-9.]+)"
    )
    m2 = entry.search(block)
    assert m2, f"no scored entry in the {profile} chain to push below {floor}"
    lowered = float(m2.group(2)) - floor - 0.1
    new_block = block[:m2.start(2)] + repr(round(lowered, 1)) + block[m2.end(2):]
    return text[:m.start(2)] + new_block + text[m.end(2):]


# (label, mutation applied to config.yaml, expected substring in the output)
CHECK_RULE_CASES = [
    (
        "extra profile",
        lambda t: t + "  test:\n    chain:\n      - provider: kilocode\n"
                  "        model: kilo-auto/free\n        vision: true\n",
        "rule5: models section is",
    ),
    (
        "empty chain",
        lambda t: re.sub(r"(?ms)^  fast:\n    chain:\n.*?(?=^  \w+:|\Z)",
                         "  fast:\n    chain: []\n", t),
        "chain is empty",
    ),
    (
        "unknown provider",
        lambda t: t.replace("      - provider: ollama\n        model: minimax-m3",
                            "      - provider: notaprovider\n        model: minimax-m3", 1),
        "undefined provider",
    ),
    (
        "dropped nvidia key",
        lambda t: _drop_provider_entry(t, "smart", "nvidia3"),
        "rule3: smart nvidia3 is missing",
    ),
    (
        "dropped commandcode key",
        lambda t: _drop_provider_entry(t, "smart", "commandcode2"),
        "rule3: smart commandcode2 is missing",
    ),
    (
        "terminator replaced by a model",
        lambda t: re.sub(
            r"(?m)^      - provider: kilocode\n        model: kilo-auto/free  # last:.*$",
            "      - provider: kilocode\n        model: qwen/qwen3.8-27b:free  # 33.7", t),
        "terminator is",
    ),
    (
        "terminator given a score",
        lambda t: t.replace(
            "        model: kilo-auto/free  # last: kilocode (auto router probed live; "
            "opencode auto not entitled to free tier)\n        vision: true",
            "        model: kilo-auto/free  # last: kilocode (auto router probed live; "
            "opencode auto not entitled to free tier)\n        vision: true\n"
            "        intelligence: 25.0", 1),
        "the auto router is not a model",
    ),
    (
        "vision removed",
        lambda t: t.replace("        vision: false\n        intelligence: 44.8",
                            "        intelligence: 44.8", 1),
        "rule7/11",
    ),
    (
        "intelligence removed",
        lambda t: t.replace(
            "        vision: false\n        intelligence: 44.8\n        context_length: 131072",
            "        vision: false\n        context_length: 131072", 1),
        "no numeric intelligence field",
    ),
    (
        "invented model id",
        lambda t: t.replace("        model: qwen/qwen3.8-27b:free", "        model: qwen/qwen9.9-27b:free", 1),
        "is not in the fetched model list",
    ),
    (
        "content-safety id",
        lambda t: t.replace("        model: stepfun/step-3.7-flash:free", "        model: nvidia/nemotron-3.5-content-safety:free", 1),
        "permanently excluded id",
    ),
    (
        "large chain re-sorted",
        # Scoped to `large`, and a context change rather than a model swap: the
        # profile is sorted by (ctx desc, intelligence desc), so dropping the
        # head entry's window below the next one is what breaks the order while
        # keeping every entry above the 200,000 floor.
        lambda t: _in_profile(
            t, "large",
            "        model: thinkingmachines/inkling-small:free  # 25.7\n"
            "        vision: true\n        intelligence: 25.7\n        context_length: 1048576",
            "        model: thinkingmachines/inkling-small:free  # 25.7\n"
            "        vision: true\n        intelligence: 25.7\n        context_length: 262144"),
        "not sorted by context_length",
    ),
    (
        "large entry below the context floor",
        lambda t: _in_profile(
            t, "large",
            "        model: thinkingmachines/inkling-small:free  # 25.7\n"
            "        vision: true\n        intelligence: 25.7\n        context_length: 1048576",
            "        model: thinkingmachines/inkling-small:free  # 25.7\n"
            "        vision: true\n        intelligence: 25.7\n        context_length: 131072"),
        "below context_length",
    ),
    (
        "work entry below the intelligence floor",
        # The floor is read from the `intelligence:` FIELD, so the field has to
        # move; renaming the model alone changes nothing a rule can see, and the
        # comment is kept in sync so the comment/field check does not fire first.
        lambda t: _in_profile(
            t, "work",
            "        model: gemma-4-26b-a4b-it  # 16.7\n"
            "        vision: false\n        intelligence: 16.7",
            "        model: gemma-4-26b-a4b-it  # 9.9\n"
            "        vision: false\n        intelligence: 9.9"),
        "below intelligence 15.0",
    ),
    (
        "smart entry below the intelligence floor",
        lambda t: _score_below_floor(t, "smart", 25.0),
        "below intelligence 25.0",
    ),
]


# A model list derived from the config itself. The data-dependent rules (8 "not
# in the fetched model list", 7 vision, 11 value sync) need a list, and borrowing
# /tmp/free-models.json would make this suite depend on a machine that happens
# to have run the fetcher. Deriving one keeps every case self-contained AND
# gives a useful control: the unmutated config must be clean against it.
_JSON_PROVIDER = {
    "nvidia": "nvidia-nim", "nvidia2": "nvidia-nim", "nvidia3": "nvidia-nim",
    "commandcode": "commandcode", "commandcode2": "commandcode",
    "gemini": "google-ai-studio", "ollama": "ollama-cloud",
    "kilocode": "kilocode", "opencode": "opencode",
}


def _model_list_from_config(config_text, tmp_path):
    """One record per endpoint, carrying the config's own values.

    The auto routers are included with the verdict their comment claims, so the
    terminator's comment check stays quiet in the control case and fires only
    when a case plants something.
    """
    import yaml
    from datetime import UTC, datetime, timedelta

    now = (datetime.now(UTC) - timedelta(days=1)).isoformat().replace("+00:00", "Z")
    records, seen = [], set()
    for body in yaml.safe_load(config_text)["models"].values():
        for ep in body["chain"]:
            if ep["model"] in ("kilo-auto/free", "big-pickle"):
                verdict = "probed live" in config_text
                rec = {
                    "id": ep["model"], "provider": _JSON_PROVIDER.get(ep["provider"], ep["provider"]),
                    "context_length": ep.get("context_length", 0),
                    "intelligence": None, "elo": None, "released": None,
                    "capabilities": {"vision": False}, "source": ep["provider"],
                    "raw": {}, "probe_only": True,
                }
                if verdict:
                    rec["verified"] = True
                    rec["auto_probe"] = {"verdict": "live", "status": 200,
                                         "detail": "derived", "checked_at": now}
                key = (ep["provider"], ep["model"])
                if key not in seen:
                    seen.add(key)
                    records.append(rec)
                continue
            key = (ep["provider"], ep["model"])
            if key in seen:
                continue
            seen.add(key)
            records.append({
                "id": ep["model"], "provider": _JSON_PROVIDER.get(ep["provider"], ep["provider"]),
                "context_length": ep.get("context_length", 0),
                "intelligence": ep.get("intelligence"), "elo": None,
                "released": None,
                "capabilities": {"vision": bool(ep.get("vision"))},
                "source": ep["provider"], "raw": {},
            })
    path = tmp_path / "derived-models.json"
    path.write_text(json.dumps(records))
    return path


def _check_cli(config_path, models_path=None):
    cmd = [sys.executable, str(REPO / "scripts" / "check-rules.py"),
           "--config", str(config_path)]
    if models_path:
        cmd += ["--models", str(models_path)]
    else:
        cmd += ["--models", "/nonexistent.json"]
    return subprocess.run(cmd, cwd=REPO, capture_output=True, text=True)


@pytest.mark.parametrize("label,mutate,expected", CHECK_RULE_CASES,
                         ids=[c[0] for c in CHECK_RULE_CASES])
def test_each_checker_rule_class_fails_the_cli(tmp_path, label, mutate, expected):
    """Every diagnostic class must be reachable, gating, and self-naming."""
    original = (REPO / "config.yaml").read_text()
    mutated = mutate(original)
    assert mutated != original, f"{label}: the planted violation changed nothing"
    path = tmp_path / "config.yaml"
    path.write_text(mutated)

    # The list is derived from the ORIGINAL config: that is the whole point of
    # the "invented model id" case — the data says one thing, the config
    # another. Deriving it from the mutated text would make every case vacuous.
    result = _check_cli(path, _model_list_from_config(original, tmp_path))
    assert result.returncode == 1, (
        f"{label}: check-rules exited {result.returncode} for a planted violation\n"
        f"{result.stdout}"
    )
    assert expected in result.stdout, (
        f"{label}: expected {expected!r} in the diagnostic, got:\n{result.stdout}"
    )


def test_checker_passes_the_real_config_through_its_own_cli(tmp_path):
    """And the same CLI is clean on the shipped file, so the cases above mean
    something: they are not passing because the check is a no-op.

    Checked twice: against a list derived from the config (self-contained, runs
    in CI) and against the real fetched list when this machine has one.
    """
    text = (REPO / "config.yaml").read_text()
    result = _check_cli(REPO / "config.yaml", _model_list_from_config(text, tmp_path))
    assert result.returncode == 0, result.stdout
    assert "OK: rules 3-11 hold" in result.stdout

    real = Path("/tmp/free-models.json")
    if real.exists():
        result = _check_cli(REPO / "config.yaml", real)
        assert result.returncode == 0, result.stdout


# --------------------------------------------------------------------------
# scripts/run-config-tests.py — the entry point that refuses to pass on a
# suite that failed to collect.
#
# The shell layer here rewrites `python3 -m pytest` output into a one-line
# summary, and a file that raised during COLLECTION came back as
# "Pytest: No tests collected" with exit status 0. A test file that never runs,
# reported as a file with no tests, is the silent-green failure class. The
# runner uses pytest's in-process API (which nothing rewrites) and asserts a
# collection floor, so the question is whether that actually holds.
# --------------------------------------------------------------------------


def _run_runner(suites, min_tests="1"):
    return subprocess.run(
        [sys.executable, str(REPO / "scripts" / "run-config-tests.py"),
         "--suites", ",".join(str(s) for s in suites), "--min-tests", min_tests],
        cwd=REPO, capture_output=True, text=True,
    )


def test_runner_passes_the_real_suites():
    result = _run_runner(mc.SUITES, min_tests="40")
    assert result.returncode == 0, result.stdout[-2000:] + result.stderr[-2000:]
    assert "passed" in result.stdout
    assert "No tests collected" not in result.stdout


def test_runner_fails_loudly_on_a_suite_that_does_not_collect(tmp_path):
    """The regression: a collection error must be a failure with the real reason.

    The original incident was a dataclass defined in an importlib-loaded module
    that was never registered in sys.modules; pytest's introspection raised
    while collecting it, and the shell summary reported "No tests collected"
    with exit 0. That exact shape depends on CPython's dataclass internals
    (it no longer reproduces on 3.14), so this uses a collection error that is
    guaranteed on every version — an import that fails at module scope. Either
    way the file never runs, and that is the failure class under test.
    """
    broken = tmp_path / "broken_suite.py"
    broken.write_text(
        "import a_module_that_does_not_exist_airouter\n\n"
        "def test_never_runs():\n    assert True\n"
    )
    result = _run_runner([broken])
    assert result.returncode != 0, result.stdout
    combined = result.stdout + result.stderr
    assert "expected at least" in combined, combined[-2000:]
    # The real reason, not a summary line: the summary is what hid it.
    assert "ERROR collecting" in combined, combined[-2000:]
    assert "No tests collected" not in combined


def test_runner_reports_a_missing_suite_instead_of_running_nothing(tmp_path):
    result = _run_runner([tmp_path / "does_not_exist.py"])
    assert result.returncode == 1
    assert "not found" in result.stdout + result.stderr

def test_the_count_verdict_is_what_gates_not_the_output(tmp_path, capsys):
    """planted == caught is an assertion, not a line a human reads.

    With `run_suite` forced to report success, every planted defect is a MISSED
    and the run must exit 1. If the exit code depended on a human reading the
    table, this is where it would pass.
    """
    path = _sandbox(tmp_path)
    restore = _with_mutations([
        mc.Mutation("undetectable one", path, "AUTO_PROBE_MAX_AGE_DAYS = 7",
                    "AUTO_PROBE_MAX_AGE_DAYS = 9", "not observable"),
        mc.Mutation("undetectable two", path, "import argparse", "import argparse  # x",
                    "not observable"),
    ])
    original_run_suite = mc.run_suite
    mc.run_suite = lambda **kw: type("R", (), {"returncode": 0, "stdout": ""})()
    try:
        code = mc.main([])
    finally:
        mc.run_suite = original_run_suite
        restore()
    assert code == 1
    out = capsys.readouterr().out
    assert "planted=2 caught=0 missed=2" in out, out[-500:]
    assert "mutation-check OK" not in out


@pytest.mark.parametrize("mutation_name", [
    "abbreviations re-enabled",                 # long, multi-line replacement
    "probe record dedupe removed",              # short: `by_id = {}`
])
def test_preflight_reports_a_tree_left_dirty_by_a_killed_sweep(mutation_name):
    """A `finally` cannot cover SIGKILL, so the next run must notice.

    Not hypothetical: a baseline run killed by its timeout left
    `allow_abbrev=True` in the tree, and the next sweep failed with the deeply
    confusing "anchor not found" two mutations later. Preflight now says what
    actually happened, names the file, and says what to do about it.

    Both a long replacement and a short one are covered: the evidence is that
    the mutated text is present UNIQUELY, not that it is long.
    """
    entry = next(m for m in mc.MUTATIONS if m.name == mutation_name)
    path = REPO / entry.path
    original = path.read_text()
    try:
        assert entry.old in original
        path.write_text(original.replace(entry.old, entry.new, 1))
        problems = mc.preflight()
    finally:
        path.write_text(original)
    hit = [p for p in problems if "killed mid-mutation" in p]
    assert hit, problems
    assert entry.path in hit[0] and mutation_name in hit[0], hit[0]
    assert mc.preflight() == []


if __name__ == "__main__":
    sys.exit(pytest.main([__file__, "-v"]))
