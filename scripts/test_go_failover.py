#!/usr/bin/env python3
"""Python wrapper to run Go tests in the mutation harness context."""
import os
import subprocess
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
GO = "/var/home/fra/.local/gosdk/go/bin/go"


def go_test(pattern: str, timeout: int = 120):
    env = os.environ.copy()
    env["PATH"] = "/var/home/fra/.local/gosdk/go/bin:" + env.get("PATH", "")
    return subprocess.run(
        [GO, "test", "-run", pattern, "-count=1", "-timeout", f"{timeout}s", "."],
        cwd=str(REPO), capture_output=True, text=True, env=env,
    )


def _run(name):
    r = go_test(name)
    assert r.returncode == 0, r.stderr or r.stdout


def test_E2EFailoverReasonInstrumented():
    _run("TestE2EFailoverReasonInstrumented")


def test_E2E500FallbackNeverReturns500():
    _run("TestE2E500FallbackNeverReturns500")


def test_E2EStreaming500FallbackNeverReturns500():
    _run("TestE2EStreaming500FallbackNeverReturns500")


def test_E2EResponses500FallbackNeverReturns500():
    _run("TestE2EResponses500FallbackNeverReturns500")


def test_E2EResponsesSurfaceNeverReturns500():
    _run("TestE2EResponsesSurfaceNeverReturns500")


def test_E2EResponsesSurfaceStreamingNeverReturns500():
    _run("TestE2EResponsesSurfaceStreamingNeverReturns500")


def test_E2EStreamStallPastTimeoutFailsOverNeverReturns500():
    _run("TestE2EStreamStallPastTimeoutFailsOverNeverReturns500")


def test_E2ENonStreamingStallPastTimeoutFailsOverNeverReturns500():
    _run("TestE2ENonStreamingStallPastTimeoutFailsOverNeverReturns500")


def test_E2ETlsResetFailsOverNeverReturns500():
    _run("TestE2ETlsResetFailsOverNeverReturns500")


def test_E2EChaoticNetworkLoad():
    _run("TestE2EChaoticNetworkLoad")


def test_E2EChaoticStreamingLoad():
    _run("TestE2EChaoticStreamingLoad")


def test_E2EDeterministicFaultSequence():
    _run("TestE2EDeterministicFaultSequence")


def test_E2EStreamingResetReplays():
    _run("TestE2EStreamingResetReplays")


def test_E2EDialRefusedNeverReturns500():
    _run("TestE2EDialRefusedNeverReturns500")


def test_E2DAllDeadNeverReturns500():
    _run("TestE2DAllDeadNeverReturns500")


def test_E2EConcurrentDeterministicFaultSequence():
    _run("TestE2EConcurrentDeterministicFaultSequence")


def test_E2EDialRefusedFailoverReasonIsTransport():
    _run("TestE2EDialRefusedFailoverReasonIsTransport")


def test_E2EFallbackLogLinePresent():
    _run("TestE2EFallbackLogLinePresent")


def test_E2EMidStreamFallbackLogLinePresent():
    _run("TestE2EMidStreamFallbackLogLinePresent")
