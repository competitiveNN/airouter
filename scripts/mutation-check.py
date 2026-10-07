#!/usr/bin/env python3
"""Mutation-test the config-sync chain: plant a defect, prove the suite fails.

WHY THIS IS A COMMITTED SCRIPT AND NOT A HEREDOC

This harness was run by hand, from a throwaway shell block, four times while
the auto-fallback probe was built. It reported green while the tree was not in
the state it claimed, twice, for two different reasons:

  1. A mutation whose `old` anchor no longer matched edited nothing. The suite
     passed, the sweep printed MISSED, and "MISSED" was read as "the tests
     cannot catch this" when the truth was "there was no defect". The same
     trick works in reverse: a mutation that matches but is semantically
     equivalent (deleting a redundant `status == 429` branch) also prints
     MISSED, and chasing it means testing implementation instead of contract.
  2. A test added by an edit call a moment earlier was silently dropped by a
     later scripted rewrite of the same file. The sweep then reported MISSED
     for a defect the suite would have caught had the test still existed.

Both are the same failure: a hand-run check whose own correctness is never
checked. So this script:

  - REFUSES a mutation whose anchor is missing or whose patch changes no
    bytes (`assert-mutation-applied`);
  - records the test count per file before and after, and refuses a run where a
    file LOST tests — the dropped-test guard;
  - counts planted vs caught and exits non-zero on any MISSED, so a sweep can
    never be reported green by eyeballing stdout;
  - restores every file in a `finally` and verifies restoration byte-for-byte.

`--preflight` verifies only the anchors and the test inventory (no execution,
no edits), which is what the pytest meta-test runs, so a rotten harness fails
in CI as "anchor missing" rather than as a silently useless sweep.

Every mutation here is a defect that has actually shipped or that was actually
introduced and caught during the probe work. If a mutation becomes impossible
to express (the code is restructured, or the defect is fixed structurally so
the line no longer exists), DELETE the entry — do not leave it to rot. The
preflight will tell you, by name.

WHAT IS DELIBERATELY NOT HERE: the gate's shell stages

scripts/gate.sh runs a live routing check (scripts/opencode-routing-check.sh)
that stands the real gateway up against OpenCode. Its guards are NOT in the
MUTATIONS table, and that is a decision rather than an oversight:

  - every mutation in this sweep edits a file in the config-sync chain and is
    detected by the three Python suites in SUITES below. The routing check is a
    bash script whose correctness is a property of the running gateway, not of
    any suite's assertions, so a mutation against it has nothing here to fail.
  - detecting a neutered routing check needs a live daemon per mutation, plus
    the provider credential. This sweep must stay runnable on a keyless machine
    and in CI, or it stops being run at all.

The instrument for that is scripts/opencode-routing-selftest.sh, which is
hermetic (loopback + a dummy key + scripts/fake-opencode-upstream.py) and
plants the neutering itself, then requires the check to notice. It runs as a
gate stage, so a neutered live check is caught without a key. If you add a
mutation for a check that has no hermetic instrument, build the instrument
first; do not add a network-dependent row to this table.

THE ROUTING HARNESS'S OWN MUTATIONS, and where to find them

Because of the above they are not rows here, but they exist and are proven:

  * neutered routing assertion -- `print(oc, len(s) - oc)` becomes
    `print(len(s), 0)`, so every session reads as correctly routed. The
    selftest requires the wrong-model run to then pass, i.e. that the check
    above it has teeth.
  * neutered usability predicate -- `if [[ -z "$unusable" ]]; then` becomes
    `if true; then`, so an HTTP 200 counts as usable and the retry is never
    taken. The selftest requires the empty-content recovery case to break.
  * neutered routing-harness attribution -- covered by the fake upstream's
    `403 FreeTierError` when the headers are absent.

Look in scripts/opencode-routing-selftest.sh, not here, when changing that
harness: adding one of these as a MUTATION row would make the sweep depend on
a daemon and a gateway binary, which is exactly what it must not do.
"""

from __future__ import annotations

import argparse
import hashlib
import os
import re
import signal
import subprocess
import sys
import time
from dataclasses import dataclass
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent

# A suite run that takes longer than this is a hang, not a slow pass. Every
# failure seen while building this was a timeout, and the default gate budget is
# short enough that a wedged run would otherwise hold the gate open.
PER_MUTATION_TIMEOUT = 300

# What a full sweep is expected to cost, recorded so a regression is
# attributable: if a run suddenly takes three times as long, the question is
# "which mutation got slow", and this number is the baseline that says
# something changed at all.
SWEEP_BASELINE_SECONDS = 90
# The suites this harness grades. They must NOT include
# scripts/test_mutation_check.py: that file runs the harness, so grading it here
# would mean the harness runs a suite that runs the harness, until the timeout
# kills it. The meta-tests grade the harness from the outside instead.
SUITES = [
    "scripts/test_regenerate_config.py",
    "scripts/test_fetch_free_models.py",
]

# Counted by the dropped-test guard even though not run: if the meta-tests are
# lost in a scripted rewrite, that is exactly the class of accident this harness
# exists to catch, so it must be watched.
INVENTORY_ONLY = [
    "scripts/test_mutation_check.py",
    # Gate-integrity regression tests. These exercise scripts/gate.sh and
    # scripts/shell-lint.sh, not the config-sync chain, so running them inside a
    # sweep would spend the budget on checks unrelated to the planted defect.
    # They are run by the gate via scripts/run-config-tests.py; being in the
    # inventory here means losing them is still a failure.
    "scripts/test_gate_invariants.py",
]


@dataclass(frozen=True)
class Mutation:
    name: str
    path: str
    old: str
    new: str
    # The defect this plants, in one line. Kept so a reader knows what a CAUGHT
    # actually proves, and so a future maintainer can tell a stale entry from
    # a live one without reading the diff.
    defect: str


MUTATIONS: list[Mutation] = [
    Mutation(
        "stale live verdict still trusted",
        "regenerate_config.py",
        "        if age_days > AUTO_PROBE_MAX_AGE_DAYS:\n            return 'unknown'",
        "        if False:\n            return 'unknown'",
        "A probe verdict older than the window would pin the terminator "
        "indefinitely if a fetcher stops running.",
    ),
    Mutation(
        "untimestamped verdict trusted",
        "regenerate_config.py",
        "        if checked is None:",
        "        if False:",
        "A `verified` with no timestamp would count as evidence, so a "
        "hand-edited record could justify the opencode rescue.",
    ),
    Mutation(
        "rescue gated on chain membership",
        "regenerate_config.py",
        "    if 'opencode' in providers_present and 'kilocode' not in providers_present:",
        "    if 'opencode' in providers_present and 'kilocode' not in providers_present and 'kilocode' in providers_present:",
        "A degenerate chain with no providers would have no rescue and could "
        "abort the whole regeneration.",
    ),
    Mutation(
        "comment preservation dropped",
        "regenerate_config.py",
        "        if state == 'unknown' and previous is not None \\\n                and previous[:2] == (provider, model) and previous[2]:",
        "        if False:",
        "An unverifiable run would rewrite a recorded 'probed live' into "
        "'not probed' on every nightly sync.",
    ),
    Mutation(
        "preservation ignores an endpoint change",
        "regenerate_config.py",
        "                and previous[:2] == (provider, model) and previous[2]:",
        "                and previous[2]:",
        "A kilocode terminator would inherit a comment naming opencode — the "
        "exact class of comment-without-its-hash defect, one indirection away.",
    ),
    Mutation(
        "comment loses its hash on re-read",
        "regenerate_config.py",
        "                entry[2] = mod.group(2) or ''",
        "                entry[2] = (mod.group(2) or '').lstrip('#').strip()",
        "A preserved comment written back bare is a YAML syntax error, not a "
        "comment. This actually happened; validate_config_text refused the write.",
    ),
    Mutation(
        "bare-hash comment no longer parsed",
        "regenerate_config.py",
        "            mod = re.match(r'^        model: (\\S+)(?:\\s+(#.*))?$', line)",
        "            mod = re.match(r'^        model: (\\S+)(?:\\s+# (.*))?$', line)",
        "Requiring '# ' means a comment that is a bare '#' is not recognised, so "
        "the profile loses its previous terminator and the comment is replaced.",
    ),
    Mutation(
        "date parser loses fractional seconds",
        "regenerate_config.py",
        "        try:\n            parsed = datetime.fromisoformat(s.replace(\"Z\", \"+00:00\"))",
        "        try:\n            parsed = None",
        "Every probe verdict would parse as no timestamp, so the whole guard "
        "would go quiet silently. This actually happened.",
    ),
    Mutation(
        "abbreviations re-enabled",
        "regenerate_config.py",
        "        allow_abbrev=False,",
        "        allow_abbrev=True,",
        "`--model` would be accepted as `--models`, so a typo'd flag exits 0 "
        "having changed nothing.",
    ),
    Mutation(
        "--write no longer required",
        "regenerate_config.py",
        "    if not write:",
        "    if False:",
        "A dry run would rewrite a hand-maintained config.",
    ),
    Mutation(
        "unverifiable claim reported as contradiction",
        "scripts/check-rules.py",
        "                if _no_verdict(rc, present, index):",
        "                if False:",
        "A claim nobody can check would be reported as a contradiction, which "
        "tells the operator to edit a comment instead of to re-probe.",
    ),
    Mutation(
        "unverifiable claim not reported at all",
        "scripts/check-rules.py",
        '                    if "probed live" in line or "probed dead" in line:',
        "                    if False:",
        "A 'probed live' claim with no verdict behind it would pass the gate.",
    ),
    Mutation(
        "probe record dedupe removed",
        "fetch-free-models.py",
        '    by_id = {(m.get("provider"), m.get("id")): m for m in models}',
        "    by_id = {}",
        "Each --probe-auto run would append another record per router, and the "
        "duplicates would disagree over time.",
    ),
    Mutation(
        "SKIP_AUTO_PROBE ignored",
        "fetch-free-models.py",
        '    if env.get("SKIP_AUTO_PROBE") == "1":',
        "    if False:",
        "The offline path would touch the network, so CI and fixture runs "
        "would depend on egress.",
    ),
    Mutation(
        "probe falls back to the listing timeout",
        "fetch-free-models.py",
        "        with open_url(req, timeout=PROBE_TIMEOUT) as resp:",
        "        with open_url(req, timeout=TIMEOUT) as resp:",
        "A probe would wait on the listing fetcher's timeout instead of the "
        "pinned liveness budget.",
    ),
    Mutation(
        "unknown verdict stamps verified=false",
        "fetch-free-models.py",
        '        else:\n            record.pop("verified", None)',
        '        else:\n            record["verified"] = False',
        "A 429 or timeout would be recorded as a confirmed refusal, and the "
        "generator would abort syncs on a transient network fault.",
    ),
    Mutation(
        "probe transport errors escape",
        "fetch-free-models.py",
        "    except Exception as e:  # noqa: BLE001 - a probe must not lose the model list",
        "    except ZeroDivisionError as e:  # noqa: BLE001",
        "An ssl.SSLError or RemoteDisconnected would abort the nightly sync "
        "and lose the whole model list.",
    ),
    # The `protocol:` field. Each mutation removes one of the three ways the
    # field can silently stop being written, and none of them crashes: the
    # config still loads and still routes, it just addresses every endpoint in
    # the wire shape it refuses.
    Mutation(
        "protocol dropped from the emitter",
        "regenerate_config.py",
        "        protocol = _normalize_protocol(entry.get('protocol'))\n        if protocol and protocol != 'chat':\n            lines.append(f\"{' ' * (indent + 2)}protocol: {protocol}\")",
        "        protocol = None",
        "A responses-only endpoint goes back to POST /chat/completions and 400s "
        "on every call, with the router flipping and retrying behind it.",
    ),
    Mutation(
        "configured protocol not preserved",
        "regenerate_config.py",
        "        protocol = m.protocol or protocols.get((provider, m.id))",
        "        protocol = m.protocol",
        "The generator owns the whole models section, so a field it cannot "
        "re-emit is a field it deletes: a hand-written protocol: responses would "
        "be erased by the next nightly sync.",
    ),
    Mutation(
        "junk protocol accepted by validate-config",
        "scripts/validate-config.py",
        "            if _bad_protocol(ep):",
        "            if False:",
        "`protocol: respones` would load silently: config.go falls back to chat, "
        "so the file would claim a shape the router never uses.",
    ),
    Mutation(
        "protocol rule not enforced",
        "scripts/check-rules.py",
        '            src_proto = getattr(src, "protocol", None)',
        '            src_proto = None',
        "Nothing would compare the field against the record that proved it, so "
        "the shape could drift with no failure anywhere.",
    ),
    Mutation(
        "a commented protocol is not parsed",
        "regenerate_config.py",
        "        proto = re.match(r'^\\s+protocol: (\\S+)(?:\\s+#.*)?\\s*$', line)",
        "        proto = re.match(r'^\\s+protocol: (\\S+)\\s*$', line)",
        "A hand-written `protocol: responses  # because ...` would be dropped, so "
        "the generator would delete the field it exists to preserve.",
    ),
    Mutation(
        "protocol carried to only the first key of a trio",
        "regenerate_config.py",
        "                chain.append(entry_for(prov, m))\n        elif m.mapped_provider == 'commandcode':",
        "                chain.append(entry_for(prov, m) if prov == 'nvidia' else {'provider': prov, 'model': m.id, 'vision': m.vision, 'intelligence': m.score, 'context_length': m.context_length, 'protocol': None, 'comment': m.get_comment()})\n        elif m.mapped_provider == 'commandcode':",
        "nvidia2/nvidia3 would keep addressing a responses-only model with the "
        "chat shape, so the failure would look like an intermittent provider bug.",
    ),
    # The emit path and the profile floors. A defect here does not crash
    # anything: it produces a config that parses, loads, and routes — just
    # wrongly. Nothing but a test can tell.
    Mutation(
        "vision field dropped from the emitter",
        "regenerate_config.py",
        "        lines.append(f\"{' ' * (indent + 2)}vision: {str(entry['vision']).lower()}\")",
        "        pass  # vision no longer emitted",
        "Capability-aware routing reads vision: an absent flag is read as "
        "vision-capable, so every text-only endpoint would be offered image "
        "requests and reject them at runtime.",
    ),
    Mutation(
        "intelligence field dropped from the emitter",
        "regenerate_config.py",
        "            lines.append(f\"{' ' * (indent + 2)}intelligence: {score:.1f}\")",
        "            pass  # intelligence no longer emitted",
        "An endpoint with no intelligence is excluded from initial-session "
        "rotation — a silent removal from the rotation window.",
    ),
    Mutation(
        "smart floor lowered",
        "regenerate_config.py",
        "            if m.score is not None and m.score >= 25 and m.is_chain_candidate]",
        "            if m.score is not None and m.score >= 20 and m.is_chain_candidate]",
        "`smart` is the strongest-generalists tier; a weaker model would take a "
        "top slot in the chain the router treats as best-first.",
    ),
    Mutation(
        "work floor lowered",
        "regenerate_config.py",
        "            if m.score is not None and m.score >= 15\n"
        "            and not m.is_excluded and m.is_chain_candidate]",
        "            if m.score is not None and m.score >= 5\n"
        "            and not m.is_excluded and m.is_chain_candidate]",
        "The tiny-model drop exists so a 9.9 model cannot sit in the coding "
        "workhorse tier ahead of the auto router.",
    ),
    Mutation(
        "chain order reversed",
        "regenerate_config.py",
        "        models.sort(\n"
        "            key=lambda x: (\n"
        "                x.score or 0,\n"
        "                x.release_dt if x.release_dt is not None else unknown,\n"
        "                -_size_tier(x.id),\n"
        "            ),\n"
        "            reverse=True,\n"
        "        )",
        "        models.sort(\n"
        "            key=lambda x: (\n"
        "                x.score or 0,\n"
        "                x.release_dt if x.release_dt is not None else unknown,\n"
        "                -_size_tier(x.id),\n"
        "            ),\n"
        "        )",
        "Best-first ordering is the whole point of a fallback chain: reversed, "
        "every request would start at the weakest model that qualifies.",
    ),
]


# A floor per test suite, in test FUNCTIONS (what test_inventory counts).
#
# Three times in this work a test was added, reported as added, and then turned
# out to be absent: an edit that reported success whose content was not in the
# file, a scripted rewrite from a stale read, and a mutation matrix built before
# its loop. In two of those a mutation was reported MISSED and the conclusion
# drawn was "the suite cannot catch this" — when the truth was that the test had
# never been there.
#
# The floor EQUALS the current count, so losing even one test fails. Raise a
# number when tests are added; a deliberate removal is a review decision and a
# floor edit in the same commit. It lives here, next to test_inventory(), so the
# count and the floor cannot be maintained in two places — and
# test_every_test_suite_has_a_floor() fails if a new suite is added without one.
SUITE_TEST_FLOORS = {
    # 66: 47 through the opencode UA tests, plus the 16 covering the runtime
    # free-tier veto (the cooldowns.json pass): a 402 vetoes a curated model
    # without editing the curated list, a 429/5xx/timeout never vetoes, one 403
    # is not a verdict but three are, a 403 without free-tier wording is not one
    # either, a 404 needs no repetition, an expired cooldown is not evidence, one
    # key's refusal covers its multi-key family, terminators are never vetoed,
    # the daemon's record outranks a fresh probe (and says so on the drop line),
    # the veto can never empty the list, a denial expires and survives a quiet
    # week, the reader survives garbage, circuits are not verdicts, the report is
    # offline, and the provider mirror still matches regenerate_config; plus the
    # 3 covering which wire shape a model speaks, because the probe already knows
    # it and the record has to say so (2026-10-04); plus the 12 covering the
    # first-seen cache: both maps round-trip, the pre-prune format and garbage
    # load as empty, a model unlisted for 30 days is pruned while 29 days is
    # not, a returning model resets its countdown and keeps its date, the
    # countdown starts on first absence, a partial fetch never prunes, and the
    # record/reuse/no-override/meta-router rules (2026-10-06); minus
    # the 16 that covered the runtime free-tier veto, removed together
    # with the veto itself (2026-10-06) -- cooldowns.json, owned and
    # updated by the daemon, is now the only refusal record; plus the 3
    # covering the commandcode credit-exhaustion split: the balance
    # family is an account-state verdict, not a per-model "paid" one,
    # a dry account keeps every candidate unverified, and exhaustion is
    # "paid" only when another candidate succeeded in the same run
    # (2026-10-06).
    "scripts/test_fetch_free_models.py": 65,
    "scripts/test_mutation_check.py": 19,
    # 48 through the distribution rules and the terminator/comment machinery,
    # plus the 11 covering the `protocol:` field: emitted when a probe proved a
    # shape, never emitted for chat, an unrecognised value dropped rather than
    # passed through, an unprobed record keeping the configured value, a fresh
    # verdict outranking the file, the field reaching every key of a trio, its
    # position in the emitter key order, the config parser, and rule 12 in both
    # directions (missing field reported, present field clean, junk rejected);
    # plus the 4 giving scripts/validate-config.py its first coverage at all --
    # a mutation aimed at it was reported MISSED, because the sync's structural
    # check had no test and a rule added to it was a rule nothing could see --
    # and 2 more after the first sync applied the protocol rule: the parser test
    # that pinned the shipped config's contents instead of the parser's
    # behaviour, replaced by fixtures plus two properties that hold whatever the
    # file contains (every declared protocol is one the router honours, and a
    # declared protocol survives a regeneration).
    "scripts/test_regenerate_config.py": 65,
    # EXACT, matching every other floor in this table (66/66, 65/65, 19/19).
    # An earlier version of this entry sat at 14 against 22 functions on the
    # theory that a floor should have slack so that adding tests needs no edit.
    # That is not this repository's convention, and the slack bought nothing:
    # for a Python suite this number is the ONLY loss detector (the Go suite has
    # test-suite-check.py's name-list baseline; these do not), so slack is
    # exactly the wrong place to be generous. At 14, deleting three tests -- or
    # one of the mutation-proof cases -- passed silently. At 22, deleting one
    # fails.
    #
    # Consequence: adding a test here means bumping this number in the same
    # commit. That is the intended friction.
    "scripts/test_gate_invariants.py": 26,
}


def test_inventory() -> dict[str, int]:
    """How many test functions each suite currently defines.

    Compared before and after every mutation: a file that lost a test means
    something rewrote the suite underneath the sweep, and every MISSED after
    that point is untrustworthy. That is the dropped-test incident, mechanised.
    """
    counts = {}
    for suite in SUITES + INVENTORY_ONLY:
        text = (REPO / suite).read_text()
        counts[suite] = len(re.findall(r"^def (test_\w+)", text, re.M))
        if not counts[suite]:
            raise SystemExit(f"{suite} defines no tests — the inventory is broken")
    return counts


def floor_shortfalls(inventory: dict[str, int]) -> list[str]:
    """Suites holding fewer test functions than SUITE_TEST_FLOORS requires.

    The preflight used to print `suites hold N tests` and exit 0 without ever
    comparing N to anything. A checker that reports a hold-count it does not
    hold to is a checker that cannot fail: on 2026-09-30 it cheerfully printed
    "suites hold 102 tests" with three test functions deleted against a floor of
    22, and exited 0. The floor lived in this file, one function away, and
    nothing read it.

    It is read now, in BOTH paths -- preflight and sweep -- so a lost test is
    caught by the cheap no-edit check rather than only by the pytest run.
    """
    shortfalls = []
    for suite, floor in SUITE_TEST_FLOORS.items():
        held = inventory.get(suite)
        if held is None:
            shortfalls.append(
                f"{suite}: no inventory entry (is the file missing? it is listed "
                f"in SUITE_TEST_FLOORS with a floor of {floor})"
            )
        elif held < floor:
            shortfalls.append(
                f"{suite}: has {held} test functions, floor is {floor}. Tests were "
                f"lost -- check for a write that reported success without landing, "
                f"or a rewrite from a stale read. If the removal was deliberate, "
                f"lower the floor in scripts/mutation-check.py in the same commit "
                f"and say why."
            )
    return shortfalls


def preflight() -> list[str]:
    """Verify every anchor exists and each mutation is well formed. No edits."""
    problems = []
    for m in MUTATIONS:
        path = REPO / m.path
        if not path.exists():
            problems.append(f"{m.name}: {m.path} does not exist")
            continue
        text = path.read_text()
        # Evidence that the tree already holds this mutation, on its own. The
        # test is UNIQUENESS rather than length: a short replacement
        # (`by_id = {}`) is still decisive if it appears exactly once, while a
        # two-character one that matches half the file is not.
        mutated_form_present = (
            m.new != m.old and m.new in text and text.count(m.new) == 1
        )
        if m.old not in text and mutated_form_present:
            # A previous run was killed between writing a mutation and
            # restoring it (SIGKILL, OOM, a lost power cut — the paths a `finally`
            # cannot cover). The tree is sitting on the DEFECT, so say that
            # instead of the far more confusing "anchor not found", which is
            # what the same situation looks like from the next run's viewpoint.
            problems.append(
                f"{m.name}: {m.path} already contains this mutation's MUTATED "
                f"form — a previous sweep was killed mid-mutation and never "
                f"restored. Restore {m.path} (git checkout / a known-good copy) "
                f"before running again; the current tree is not the baseline."
            )
        elif m.old not in text:
            problems.append(
                f"{m.name}: anchor not found in {m.path}. The code moved or the "
                f"defect is fixed structurally — update or delete this entry."
            )
        elif text.count(m.old) != 1:
            problems.append(
                f"{m.name}: anchor appears {text.count(m.old)} times in {m.path}; "
                f"it must be unique or the patch is ambiguous"
            )
        if not m.defect.strip():
            problems.append(f"{m.name}: no defect description")
    return problems


def run_suite(timeout: int | None = None) -> subprocess.CompletedProcess:
    """Run the graded suites.

    Uses the project's own runner (scripts/run-config-tests.py) rather than
    `python3 -m pytest`: that path is rewritten by the shell layer into a
    one-line summary which reports a suite that failed to COLLECT as
    "No tests collected" with exit status 0. The runner asserts a collection
    floor instead, so "the suite is red" here means the suite is red.
    """
    # `--suites` pins the runner to THIS harness's suites. Without it the runner
    # would also run scripts/test_mutation_check.py, which runs the harness —
    # a cycle that ends in a timeout rather than a result.
    return subprocess.run(
        [sys.executable, str(REPO / "scripts" / "run-config-tests.py"),
         "--suites", ",".join(SUITES), "-q"],
        cwd=REPO, capture_output=True, text=True, timeout=timeout,
    )


class TreeGuard:
    """Hashes every file the harness may touch, so restore can be proven.

    A per-mutation `finally` only covers a clean unwind. A SIGKILL, an OOM or a
    power cut leaves a half-mutated tree that the next person — or the next
    nightly sync — trips over, and the symptom ("why does the terminator say
    kilocode when kilocode is dead") points nowhere near the cause. So the
    whole-sweep baseline is recorded up front, printed at the end, and compared
    against the current tree; a difference is a hard failure with the file names.
    """

    def __init__(self, paths: list[str]) -> None:
        self.paths = paths
        self.baseline = {p: self._digest(p) for p in paths}

    @staticmethod
    def _digest(rel: str) -> str:
        try:
            return hashlib.sha256((REPO / rel).read_bytes()).hexdigest()[:16]
        except OSError as exc:
            return f"unreadable:{exc.errno}"

    def drift(self) -> list[str]:
        return [p for p in self.paths if self._digest(p) != self.baseline[p]]

    def report(self) -> str:
        return ", ".join(f"{p}={self.baseline[p]}" for p in self.paths)


def _install_signal_restore(guard: TreeGuard) -> None:
    """On SIGINT/SIGTERM, put the tree back before exiting non-zero.

    Every failure observed while building this was a timeout or a truncated
    run. Without this, Ctrl-C during a sweep leaves a mutated
    regenerate_config.py in the working tree, which is the exact failure mode
    the harness exists to prevent.
    """

    def handler(signum, _frame):
        drift = guard.drift()
        if drift:
            print(
                f"\nmutation-check: signal {signum}; restoring {drift}",
                file=sys.stderr,
            )
        else:
            print(f"\nmutation-check: signal {signum}; tree already clean", file=sys.stderr)
        raise SystemExit(128 + signum)

    for sig in (signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, handler)


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument(
        "--preflight", action="store_true",
        help="verify anchors and test inventory only; do not edit or run tests",
    )
    ap.add_argument(
        "--only", metavar="SUBSTRING", help="run mutations whose name contains this",
    )
    ap.add_argument(
        "--timing", action="store_true",
        help="always print the slowest-mutation table, not only on failure",
    )
    ap.add_argument(
        "--timeout", type=int, default=PER_MUTATION_TIMEOUT, metavar="SECONDS",
        help=f"wall-clock cap for one suite run (default: {PER_MUTATION_TIMEOUT})",
    )
    args = ap.parse_args(argv)
    t0 = time.monotonic()

    problems = preflight()
    baseline = test_inventory()
    shortfalls = floor_shortfalls(baseline)
    if args.preflight:
        if problems or shortfalls:
            if problems:
                print(f"mutation-check preflight FAILED ({len(problems)} problem(s)):")
                for p in problems:
                    print(f"  - {p}")
            if shortfalls:
                print(
                    f"mutation-check preflight FAILED "
                    f"({len(shortfalls)} floor shortfall(s)):"
                )
                for s in shortfalls:
                    print(f"  - {s}")
            return 1
        held = sum(baseline.values())
        floors = sum(SUITE_TEST_FLOORS.values())
        print(
            f"mutation-check preflight OK: {len(MUTATIONS)} mutations anchored, "
            f"suites hold {held} tests (floors require {floors})"
        )
        return 0
    if problems:
        print(f"mutation-check REFUSING TO RUN ({len(problems)} problem(s)):")
        for p in problems:
            print(f"  - {p}")
        return 1
    if shortfalls:
        print(
            f"mutation-check REFUSING TO RUN ({len(shortfalls)} floor "
            f"shortfall(s)):"
        )
        for s in shortfalls:
            print(f"  - {s}")
        print(
            "  The inventory is the dropped-test guard, and it is below its own "
            "floor. Running the sweep now would grade the tree as it stands and "
            "call a suite that has lost tests healthy."
        )
        return 1

    selected = [m for m in MUTATIONS if not args.only or args.only in m.name]
    if not selected:
        print(f"no mutation matches --only {args.only!r}")
        return 2

    # The suite must be green BEFORE any mutation, or "the suite failed" proves
    # nothing about the mutation.
    try:
        before = run_suite(timeout=args.timeout)
    except subprocess.TimeoutExpired:
        print(f"mutation-check: the baseline suite exceeded {args.timeout}s", file=sys.stderr)
        return 1
    if before.returncode != 0:
        print("mutation-check: the suite is not green to begin with.")
        print(before.stdout[-3000:])
        return 1
    print(f"baseline: {sum(baseline.values())} tests green in {time.monotonic() - t0:.1f}s")

    # Restore is proven per mutation (a finally) and again for the whole sweep.
    # The second check is what covers a death the finally cannot: a signal
    # handler unwinds, but SIGKILL and a lost power do not, and the next person
    # to read the tree has no way to know.
    guard = TreeGuard(sorted({m.path for m in selected}))
    _install_signal_restore(guard)
    print(f"tree baseline: {guard.report()}")

    src_baseline = {p: (REPO / p).read_text() for p in sorted({m.path for m in selected})}
    planted = caught = 0
    missed: list[str] = []
    durations: list[tuple[str, float]] = []
    for m in selected:
        path = REPO / m.path
        original = path.read_text()
        started = time.monotonic()
        try:
            if m.old not in original:
                raise SystemExit(f"{m.name}: anchor vanished mid-sweep")
            mutated = original.replace(m.old, m.new, 1)
            if mutated == original:
                # The check the hand-run version lacked: a patch that changes
                # nothing is not a mutation, and reporting it as MISSED is a
                # lie about the suite's coverage.
                raise SystemExit(
                    f"{m.name}: patch changed no bytes — this is a no-op, not a "
                    f"mutation. Reporting it as MISSED would be false."
                )
            path.write_text(mutated)
            after = test_inventory()
            for suite, count in after.items():
                if count < baseline[suite]:
                    raise SystemExit(
                        f"{m.name}: {suite} lost tests ({count} < {count}) during "
                        f"the sweep; results from here on are untrustworthy"
                    )
            planted += 1
            try:
                result = run_suite(timeout=args.timeout)
            except subprocess.TimeoutExpired:
                raise SystemExit(
                    f"{m.name}: the suite did not finish within {args.timeout}s. "
                    f"Treating a hang as a failure, not as a pass — and the tree "
                    f"is being restored."
                )
            if result.returncode != 0:
                caught += 1
                fails = len(re.findall(r"^(?:FAILED|.*FAILURES)", result.stdout, re.M))
                print(f"  CAUGHT  {m.name} ({fails} failing)")
                print(f"          defect: {m.defect}")
            else:
                missed.append(m.name)
                print(f"  MISSED  {m.name}")
                print(f"          defect: {m.defect}")
        finally:
            path.write_text(original)
            restored = path.read_text()
            if restored != original:
                raise SystemExit(f"{m.name}: FAILED TO RESTORE {m.path}")
            if original != src_baseline.get(m.path, original):
                # The snapshot this iteration restored to is not what the file
                # held when the sweep started, so an EARLIER iteration restored
                # the wrong content and this one faithfully put that back. Saying
                # so beats a mysterious "anchor vanished" two mutations later.
                raise SystemExit(
                    f"{m.name}: {m.path} was already modified when this mutation "
                    f"started — an earlier iteration did not restore the sweep "
                    f"baseline. Aborting rather than compounding it."
                )
            print(f"          restored {m.path} "
                  f"(sha {hashlib.sha256(original.encode()).hexdigest()[:12]})")
            durations.append((m.name, time.monotonic() - started))

    drift = guard.drift()
    if drift:
        print(f"\nmutation-check: TREE DRIFT — {drift} differ from the baseline "
              f"({guard.report()})", file=sys.stderr)
        return 1

    elapsed = time.monotonic() - t0
    # The timing table is diagnostic, so it prints when it is useful: on failure,
    # on request, or when the run went over the recorded budget. A per-mutation
    # table on every green run is noise nobody reads.
    over_budget = elapsed > SWEEP_BASELINE_SECONDS
    if missed or args.timing or over_budget:
        slow = sorted(durations, key=lambda d: -d[1])[:5]
        print("slowest mutations (s): " + ", ".join(f"{n}={d:.1f}" for n, d in slow))
    print(
        f"sweep {elapsed:.0f}s over {planted} mutations "
        f"(recorded baseline {SWEEP_BASELINE_SECONDS:.0f}s)"
        + ("  OVER BUDGET" if over_budget else "")
    )
    print(f"planted={planted} caught={caught} missed={len(missed)}")
    if missed:
        print("mutation-check FAILED — the suite does not catch:")
        for name in missed:
            entry = next(m for m in MUTATIONS if m.name == name)
            print(f"  - {name}: {entry.defect}")
        print("\nEither the tests are missing, or the mutation is equivalent to "
              "the current code. An equivalent mutant (deleting a redundant "
              "branch) is not a suite gap: fix the constant it should have "
              "pinned, or delete the entry.")
        return 1
    print("mutation-check OK: every planted defect was caught")
    return 0


if __name__ == "__main__":
    sys.exit(main())
