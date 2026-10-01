#!/usr/bin/env python3
"""Entry point for the config-sync test suites.

WHY THIS EXISTS INSTEAD OF `python3 -m pytest ...`

The shell layer on this machine intercepts `python3 -m pytest` and replaces its
output with a one-line summary ("Pytest: 107 passed in 13.55s"). That is
convenient until it is wrong: a file that failed to COLLECT came back as
"Pytest: No tests collected" with exit status 0. A test file that never runs,
reported as a file with no tests, is a silent-green failure class — and it
happened here for real (an importlib-loaded dataclass whose module was missing
from sys.modules).

So this runs pytest in-process via its public API, where nothing rewrites the
output, and adds the assertion the summary destroyed: a collection floor. If the
suites collect fewer tests than expected, this exits non-zero with the real
traceback, whatever the shell does with the output.

    scripts/run-config-tests.py                 # all three suites
    scripts/run-config-tests.py -k terminator   # extra args pass through
    scripts/run-config-tests.py --min-tests 40  # tighten the floor
"""

from __future__ import annotations

import sys
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parent.parent
SUITES = [
    "scripts/test_regenerate_config.py",
    "scripts/test_fetch_free_models.py",
    "scripts/test_mutation_check.py",
    # Gate-integrity regression tests: the stage-accounting guard and the
    # shell-lint untracked NOTICE sweep. Both live in scripts/gate.sh and
    # scripts/shell-lint.sh rather than in the config-sync chain, so they are
    # graded by the gate (here) and counted by the dropped-test guard in
    # mutation-check.py, but not run inside its sweep.
    "scripts/test_gate_invariants.py",
]

# A floor, not an exact count: it should fail only when a suite stops being
# collected, and tolerate tests being added. Set well below the current count
# (~107) so a normal run has margin, well above the smallest single suite so a
# suite going missing is caught.
DEFAULT_MIN_TESTS = 40


class CollectionGuard:
    """Records how many tests were collected, so 0 is a failure not a pass."""

    def __init__(self) -> None:
        self.collected = 0

    def pytest_collection_finish(self, session) -> None:
        self.collected = len(session.items)


def main(argv: list[str] | None = None) -> int:
    argv = sys.argv[1:] if argv is None else argv
    min_tests = DEFAULT_MIN_TESTS
    if "--min-tests" in argv:
        i = argv.index("--min-tests")
        min_tests = int(argv[i + 1])
        del argv[i:i + 2]

    # `--suites` exists so a test can point the runner at a deliberately broken
    # file and prove that a collection error fails the run. See
    # scripts/test_mutation_check.py.
    suites = list(SUITES)
    if "--suites" in argv:
        i = argv.index("--suites")
        suites = argv[i + 1].split(",")
        del argv[i:i + 2]

    resolved = [(REPO / s) if not Path(s).is_absolute() else Path(s) for s in suites]
    missing = [str(s) for s in resolved if not s.exists()]
    if missing:
        print(f"FAIL: suite(s) not found: {missing}", file=sys.stderr)
        return 1
    suites = [str(s) for s in resolved]

    guard = CollectionGuard()
    print(f"running: {', '.join(suites)} (collection floor: {min_tests})")
    code = pytest.main([*suites, *argv], plugins=[guard])

    if guard.collected < min_tests:
        print(
            f"FAIL: only {guard.collected} tests collected, expected at least "
            f"{min_tests}. A suite that fails to collect is not a suite that "
            f"passes — the traceback above (if any) is the reason.",
            file=sys.stderr,
        )
        return 1
    print(f"collected {guard.collected} tests, pytest exit {code}")
    return int(code)


if __name__ == "__main__":
    sys.exit(main())
