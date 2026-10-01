#!/usr/bin/env python3
"""Tests for fetch-free-models.py's auto-fallback liveness probe.

The probe exists to give regenerate_config.py evidence about the two chain
terminators. The evidence has to be trustworthy in both directions:

  - `dead` must mean the provider REFUSED the call, so the generator can act
    on it and abort a regeneration rather than write a known-dead endpoint;
  - `unknown` must mean "we could not find out", so a 429, a 5xx, a timeout or
    a dropped connection can neither move the terminator nor block a sync.

Getting that second half wrong is invisible until the day the network hiccups
during the nightly sync and every regeneration stops — or worse, a real
outage is filed as transient and the sync keeps writing a dead terminator.
Neither failure is reachable from a live probe in a test suite, because the
outages are the thing being classified, so the verdict table is tested
directly, with no socket.
"""

import importlib.util
import io
import json
import sys
import urllib.error
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parent.parent
FETCHER = REPO / "fetch-free-models.py"


def _load_fetcher():
    sys.path.insert(0, str(REPO))  # fetch-free-models imports model_utils
    try:
        spec = importlib.util.spec_from_file_location("fetch_free_models", FETCHER)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module
    finally:
        sys.path.remove(str(REPO))


ff = _load_fetcher()


@pytest.mark.parametrize("status", [400, 401, 402, 403, 404])
def test_refusal_statuses_are_dead(status):
    """4xx is a statement about entitlement or existence, so it is actionable."""
    assert ff.classify_probe_status(status) == "dead"


@pytest.mark.parametrize("status", [429, 500, 502, 503, 504, 301, 101])
def test_transient_statuses_are_unknown(status):
    """429 is a working key with an empty bucket; 5xx is upstream. Neither is a
    verdict about the endpoint, and treating either as `dead` would abort syncs
    for reasons that pass by."""
    assert ff.classify_probe_status(status) == "unknown"


def test_429_is_never_in_the_dead_set():
    """Pin the constant, not just the outcome.

    The explicit `status == 429` branch happens to be redundant today, because
    the fall-through also returns `unknown` — which is why mutating it away is
    an equivalent mutant and no behavioural test can catch it. What is NOT
    redundant is 429 staying out of PROBE_DEAD_STATUSES: adding it there later
    would turn every rate-limited probe into "this endpoint is permanently
    gone", and every such probe into a blocked sync.
    """
    assert 429 not in ff.PROBE_DEAD_STATUSES
    assert ff.classify_probe_status(429, b'{"error":{"message":"rate limited"}}') == "unknown"


def test_2xx_with_a_completion_is_live_and_2xx_with_an_error_envelope_is_dead():
    assert ff.classify_probe_status(200, b'{"choices":[{"message":{"content":"x"}}]}') == "live"
    assert ff.classify_probe_status(200, b"") == "live"
    assert ff.classify_probe_status(200, b'{"error":{"message":"no such model"}}') == "dead"


def test_unreadable_body_is_unknown_not_live():
    """A 200 we cannot parse is not evidence that the endpoint works."""
    assert ff.classify_probe_status(200, b"<html>gateway</html>") == "unknown"


def _record(**over):
    base = {
        "id": "kilo-auto/free",
        "name": "Auto Free",
        "provider": "kilocode",
        "context_length": 256000,
        "intelligence": None,
        "capabilities": {"vision": False},
        "source": "kilocode",
    }
    base.update(over)
    return base


def test_probe_stamps_verdict_on_a_record_that_already_exists(monkeypatch):
    models = [_record()]
    monkeypatch.setitem(
        __import__("os").environ, "KILOCODE_API_KEY", "k-test"
    )
    monkeypatch.setattr(ff, "probe_endpoint", lambda *a, **k: {
        "verdict": "live", "status": 200, "detail": "1-token completion returned",
        "checked_at": "2026-09-30T00:00:00Z",
    })
    out = ff.probe_auto_fallbacks(models, environ={"KILOCODE_API_KEY": "k-test"})
    assert out[0]["verified"] is True
    assert out[0]["auto_probe"]["verdict"] == "live"
    assert len(out) == 1, "an existing record must not be duplicated"


def test_probe_adds_a_probe_only_record_when_the_router_is_not_listed(monkeypatch):
    """opencode/big-pickle is not advertised as free, so it has no record.

    The verdict still has to reach the generator, which is the only thing that
    reads it. The placeholder carries no score, so it cannot be mistaken for a
    routable model.
    """
    out = ff.probe_auto_fallbacks([], environ={"OPENCODE_API_KEY": "o-test"})
    monkey_patched = {m["id"]: m for m in out}
    assert "big-pickle" in monkey_patched
    rec = monkey_patched["big-pickle"]
    assert rec["probe_only"] is True
    assert rec["intelligence"] is None
    assert rec["verified"] is False  # the real verdict, 403 in production


def test_unknown_verdict_leaves_verified_unset(monkeypatch):
    """`verified` is the field the generator acts on.

    A transport failure must leave it absent (None) rather than set it to
    False, because False means confirmed-dead and aborts a regeneration.
    """
    models = [_record(verified=None)]
    monkeypatch.setattr(ff, "probe_endpoint", lambda *a, **k: {
        "verdict": "unknown", "status": 429, "detail": "no verdict (HTTP 429)",
        "checked_at": "2026-09-30T00:00:00Z",
    })
    out = ff.probe_auto_fallbacks(models, environ={"KILOCODE_API_KEY": "k-test"})
    assert "verified" not in out[0]
    assert "429" in out[0]["unverified_reason"]


def test_missing_key_is_skipped_not_assumed_dead(capsys):
    """No key means no probe, which is `unknown` — never `dead`."""
    out = ff.probe_auto_fallbacks([], environ={})
    assert out == []
    assert "unset" in capsys.readouterr().err


def test_probe_is_idempotent_across_runs(monkeypatch):
    """Two runs over the same list must not grow it.

    Records are looked up by (provider, id) before any append. Appending
    unconditionally would add one record per router per run — the JSON is saved
    and re-read by every later sync, so the growth is unbounded and the
    duplicate verdicts would disagree with each other over time.
    """
    monkeypatch.setattr(ff, "probe_endpoint", lambda *a, **k: {
        "verdict": "dead", "status": 403, "detail": "provider refused the model (HTTP 403)",
        "checked_at": "2026-09-30T00:00:00Z",
    })
    env = {"KILOCODE_API_KEY": "k", "OPENCODE_API_KEY": "o"}
    models = [_record()]
    first = ff.probe_auto_fallbacks(list(models), environ=env)
    count = len(first)
    assert count == 2, "expected the kilocode record plus one probe-only record"

    # Round-trip through JSON, exactly as the nightly sync does.
    reloaded = json.loads(json.dumps(first))
    second = ff.probe_auto_fallbacks(reloaded, environ=env)
    assert len(second) == count, "a second probe run appended a duplicate"
    assert len({(m["provider"], m["id"]) for m in second}) == count

    # And the re-stamp is visible on the same records, not new ones.
    assert [m["verified"] for m in second if m["id"] == "kilo-auto/free"] == [False]


def test_skip_auto_probe_leaves_the_list_untouched(capsys, monkeypatch):
    """The offline path: no probe, no verdict, no record — and no surprise.

    CI has no egress and no keys. SKIP_AUTO_PROBE=1 must therefore be a
    complete no-op on the list, so a fixture-driven run produces exactly the
    same bytes as one where the probe was never requested.
    """
    def explode(*a, **k):
        raise AssertionError("the probe must not run when SKIP_AUTO_PROBE=1")

    monkeypatch.setattr(ff, "probe_endpoint", explode)
    before = [_record(verified=True, auto_probe={"verdict": "live", "checked_at": "x"})]
    after = ff.probe_auto_fallbacks(json.loads(json.dumps(before)),
                                   environ={"SKIP_AUTO_PROBE": "1", "KILOCODE_API_KEY": "k"})
    assert after == before
    assert "SKIP_AUTO_PROBE" in capsys.readouterr().err


def test_probe_uses_the_pinned_timeout_and_a_fresh_injectable_client():
    """The timeout is its own decision, and the HTTP client is injectable.

    Both matter for testability: the `unknown` bucket is defined by what
    happens when the call does not come back in time, and that cannot be
    demonstrated against a real provider on demand.
    """
    seen = {}

    class FakeResponse:
        status = 200

        def read(self):
            return b'{"choices":[]}'

        def __enter__(self):
            return self

        def __exit__(self, *a):
            return False

    def opener(req, timeout=None):
        seen["timeout"] = timeout
        seen["url"] = req.full_url
        return FakeResponse()

    result = ff.probe_endpoint("https://api.example/v1", "m", "key", opener=opener)
    assert result["verdict"] == "live"
    assert seen["timeout"] == ff.PROBE_TIMEOUT
    assert seen["url"].endswith("/chat/completions")


def test_a_timeout_is_unknown_not_dead():
    """socket.timeout is an alias of TimeoutError, but spell it out.

    The third bucket is the one that keeps a flaky network from aborting every
    sync, so it needs a test that actually raises the timeout rather than
    describing it.
    """
    def timing_out(req, timeout=None):
        raise TimeoutError("timed out")

    result = ff.probe_endpoint("https://api.example/v1", "m", "k", opener=timing_out)
    assert result["verdict"] == "unknown"
    assert result["status"] is None
    assert "timed out" in result["detail"]


def test_probe_never_raises_on_a_transport_error(monkeypatch):
    """It runs inside the nightly sync: an exception here loses the whole list."""

    def exploding(*a, **k):
        raise OSError("network is unreachable")

    monkeypatch.setattr(ff.urllib.request, "urlopen", exploding)
    result = ff.probe_endpoint("https://example.invalid/v1", "m", "k")
    assert result["verdict"] == "unknown"
    assert "network is unreachable" in result["detail"]


def test_probe_survives_an_unexpected_error_type(monkeypatch):
    """A non-OSError from the HTTP layer must not escape either.

    ssl.SSLError, http.client.RemoteDisconnected and friends are not all
    OSError subclasses in every Python build; the nightly sync should not
    discover that difference at 06:00 with a traceback instead of a model list.
    """

    def weird(*a, **k):
        raise ValueError("unexpected decoder state")

    monkeypatch.setattr(ff.urllib.request, "urlopen", weird)
    result = ff.probe_endpoint("https://example.invalid/v1", "m", "k")
    assert result["verdict"] == "unknown"


def test_json_output_round_trips_with_the_extra_fields():
    """The record shape the generator loads must survive a save/load cycle."""
    models = [_record(verified=True, auto_probe={"verdict": "live", "status": 200})]
    tmp = Path("/tmp/probe-roundtrip.json")
    ff.output_json(models, str(tmp))
    try:
        loaded = json.loads(tmp.read_text())
    finally:
        tmp.unlink(missing_ok=True)
    assert loaded[0]["verified"] is True
    assert loaded[0]["auto_probe"]["verdict"] == "live"


def test_timestamp_contract_between_the_producer_and_the_consumer():
    """THE contract test: what the fetcher writes, the generator must read.

    Two tools, one string format, no shared code — and the failure mode is
    silent. The generator's verdict lookup parses `auto_probe.checked_at`; a
    parse miss returns None, which the ladder reads as "no verdict", which reads
    as "not probed". Every probe guard in the system then quietly stops firing
    while the config still claims the terminator was verified. That is not
    hypothetical: it is exactly what happened when the fetcher emitted
    fractional seconds and the parser knew only `%Y-%m-%dT%H:%M:%SZ`.

    Both directions are pinned: the string the producer emits today parses, the
    near-miss variants a future "simplification" might produce also parse, and
    values that are not dates still do not — otherwise the parser becomes a
    rubber stamp that makes every stale verdict look fresh.
    """
    import importlib.util as _ilu
    spec = _ilu.spec_from_file_location("regenerate_config_contract", REPO / "regenerate_config.py")
    gen = _ilu.module_from_spec(spec)
    spec.loader.exec_module(gen)

    class FakeResponse:
        status = 200

        def read(self):
            return b'{"choices":[]}'

        def __enter__(self):
            return self

        def __exit__(self, *a):
            return False

    produced = ff.probe_endpoint("https://api.example/v1", "m", "k",
                                 opener=lambda *a, **k: FakeResponse())["checked_at"]
    assert produced.endswith("Z"), produced
    assert gen._parse_release_date(produced) is not None, produced

    for variant in (
        produced.replace(".000000", ""),   # seconds precision
        "2026-09-30T10:48:21Z",            # already accepted before
        "2026-09-30T10:48:21+00:00",       # numeric offset instead of Z
        "2026-09-30",                      # date only
    ):
        assert gen._parse_release_date(variant) is not None, variant

    for junk in ("", "garbage", "0000-00-00", "2026-13-45T99:99:99Z", None):
        assert gen._parse_release_date(junk) is None, junk


def test_producer_and_consumer_agree_end_to_end_through_json(monkeypatch, tmp_path):
    """The same contract through the real write/read path.

    A parse miss here would make `probe_checked_at` None, and the ladder would
    read the verdict as stale-or-unprobed — the guard going quiet with no error
    anywhere. So freshness has to survive the round trip, not just parse.
    """
    import importlib.util as _ilu
    spec = _ilu.spec_from_file_location("regenerate_config_contract2", REPO / "regenerate_config.py")
    gen = _ilu.module_from_spec(spec)
    spec.loader.exec_module(gen)

    real_probe = ff.probe_endpoint

    class FakeResponse:
        status = 200

        def read(self):
            return b'{"choices":[]}'

        def __enter__(self):
            return self

        def __exit__(self, *a):
            return False

    monkeypatch.setattr(ff, "probe_endpoint",
                        lambda endpoint, model_id, key, extra=None, opener=None:
                        real_probe(endpoint, model_id, key, extra,
                                   opener=lambda *a, **k: FakeResponse()))
    models = ff.probe_auto_fallbacks([_record()], environ={"KILOCODE_API_KEY": "k"})
    path = tmp_path / "models.json"
    ff.output_json(models, str(path))
    rec = next(m for m in gen.load_models(str(path)) if m.id == "kilo-auto/free")
    assert rec.verified is True
    assert rec.probe_checked_at is not None
    assert (gen.datetime.now(gen.UTC) - rec.probe_checked_at).total_seconds() < 60



# ── CommandCode free-ness is probed, not declared ──────────────────────────────
#
# Regression tests for the hardcoded-free-list defect. COMMANDCODE_FREE_MODELS
# was the authoritative free set, so a deal that ended stayed in the chains:
# meituan/longcat-2.0-free was probed live on 2026-10-01 and answered HTTP 400
# "You have insufficient credits to make this request. Please purchase more
# credit[s]", while it still appeared in work, fast and large on both commandcode
# providers. Free-ness is now decided by commandcode_probe(), and these tests pin
# the distinction that makes that work.


class _Resp:
    def __init__(self, status=200):
        self.status = status

    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False


def test_commandcode_candidate_heuristic_covers_suffixes_and_seeds():
    """Candidates come from the provider's own suffix convention plus seeds."""
    # Advertised free deals, by suffix.
    assert ff.commandcode_is_free_candidate("poolside/laguna-s-2.1-free")
    assert ff.commandcode_is_free_candidate("inclusionai/ling-3.0-flash-sante:free")
    assert ff.commandcode_is_free_candidate("inclusionai/ling-3.1-flash:free")
    # A seed with no suffix at all: the stealth preview.
    assert ff.commandcode_is_free_candidate("stealth/space-bunny-alpha")
    # Paid catalog entries must not be candidates.
    assert not ff.commandcode_is_free_candidate("z-ai/glm-5.3")
    assert not ff.commandcode_is_free_candidate("meituan/LongCat-2.0-foo")
    assert not ff.commandcode_is_free_candidate("")


def _probe_with(body_bytes, code):
    def opener(*a, **k):
        if code >= 400:
            raise urllib.error.HTTPError("u", code, "e", {}, io.BytesIO(body_bytes))
        return _Resp(code)
    return opener


@pytest.mark.parametrize("message", [
    "You have insufficient credits to make this request. Please purchase more credits.",
    "Insufficient balance on the account",
    "Payment required for this model",
])
def test_billing_language_is_a_paid_verdict_even_on_http_400(message, monkeypatch):
    """The provider says 400, not 402, for insufficient credits.

    Keying on the status code alone misses the exact case that shipped, so the
    message is inspected instead. A paid verdict must be DEFINITIVE: it is the
    only thing that removes a model from the chains.
    """
    monkeypatch.setattr(ff.urllib.request, "urlopen",
                        _probe_with(message.encode(), 400))
    assert ff.commandcode_probe("meituan/longcat-2.0-free", "k") == "paid"


def test_longcat_is_not_free():
    """The specific regression, stated as its own case.

    If this starts passing without a real re-probe, the fetcher has gone back to
    trusting a list instead of the provider.
    """
    orig = ff.urllib.request.urlopen
    ff.urllib.request.urlopen = _probe_with(
        b'{"error":{"message":"You have insufficient credits to make this request."}}', 400)
    try:
        assert ff.commandcode_probe("meituan/longcat-2.0-free", "k") == "paid"
    finally:
        ff.urllib.request.urlopen = orig


@pytest.mark.parametrize("code,body", [
    (403, b'{"error":{"message":"Your Go plan doesn\'t include API access."}}'),
    (429, b'{"error":{"message":"slow down"}}'),
    (502, b'{"error":{"message":"providers at capacity"}}'),
])
def test_inconclusive_responses_are_unknown_not_paid(code, body):
    """A capacity blip or a plan wall must NOT look like a dead deal.

    Collapsing "unknown" into "unusable" is what made the old hardcoded list
    wrong in the other direction: a transient outage would prune a live deal.
    """
    orig = ff.urllib.request.urlopen
    ff.urllib.request.urlopen = _probe_with(body, code)
    try:
        assert ff.commandcode_probe("poolside/laguna-s-2.1-free", "k") == "unknown"
    finally:
        ff.urllib.request.urlopen = orig


def test_a_200_is_the_only_ok_verdict():
    orig = ff.urllib.request.urlopen
    ff.urllib.request.urlopen = lambda *a, **k: _Resp(200)
    try:
        assert ff.commandcode_probe("stealth/space-bunny-alpha", "k") == "ok"
    finally:
        ff.urllib.request.urlopen = orig


def test_a_removed_id_is_paid():
    """404 means the id is gone from the catalog; that is not a transient."""
    orig = ff.urllib.request.urlopen
    ff.urllib.request.urlopen = _probe_with(b'{"error":{"message":"not found"}}', 404)
    try:
        assert ff.commandcode_probe("meituan/gone-free", "k") == "paid"
    finally:
        ff.urllib.request.urlopen = orig


def test_probe_uses_the_endpoint_config_yaml_routes():
    """The probe must hit the server the gateway will use.

    config.yaml routes commandcode at 127.0.0.1:3050 while the built-in endpoint
    is api.commandcode.ai. Probing the public host answers 403 for every model,
    which reads as "none are free" and empties the chain.
    """
    url = ff.commandcode_base_url()
    assert url.endswith("/v1"), url
    assert "api.commandcode.ai" not in url, (
        "the probe fell back to the public endpoint, which answers 403 "
        "plan-restricted for every model and cannot judge any deal"
    )
    assert ff.commandcode_chat_url().endswith("/chat/completions")
    assert ff.commandcode_models_url().endswith("/models")


def test_fetch_commandcode_drops_paid_and_reports_inconclusive(monkeypatch):
    """End to end: a paid deal is absent from the output, and said so."""
    catalog = {"data": [
        {"id": "stealth/space-bunny-alpha"},
        {"id": "meituan/LongCat-2.0"},
        {"id": "poolside/laguna-s-2.1-free"},
        {"id": "z-ai/glm-5.3"},
    ]}
    monkeypatch.setattr(ff, "fetch_json", lambda url, headers=None: catalog)
    monkeypatch.setenv("COMMANDCODE_API_KEY", "k")

    def fake_probe(model_id, key):
        if "longcat" in model_id.lower():
            return "paid"
        if "laguna" in model_id.lower():
            return "unknown"
        return "ok"

    monkeypatch.setattr(ff, "commandcode_probe", fake_probe)
    out = ff.fetch_commandcode()
    ids = [m["id"] for m in out]
    assert not any("longcat" in i for i in ids), ids
    assert "stealth/space-bunny-alpha" in ids, ids
    # glm-5.3 is paid catalog noise and was never a candidate.
    assert not any("glm" in i for i in ids), ids
    assert all(m.get("verified") is True for m in out), out

if __name__ == "__main__":
    sys.exit(pytest.main([__file__, "-v"]))
