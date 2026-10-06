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
import re
import sys
import urllib.error
from datetime import UTC, date, datetime, timedelta
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
                        lambda endpoint, model_id, key, extra=None, opener=None,
                        opencode_gate=False:
                        real_probe(endpoint, model_id, key, extra,
                                   opener=lambda *a, **k: FakeResponse(),
                                   opencode_gate=opencode_gate))
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

def _opener(routes):
    """Build a urlopen stand-in. `routes` maps a URL suffix to a callable
    (req) -> (status, body) or an exception instance to raise."""
    class _Resp:
        def __init__(self, status, payload):
            self.status = status
            self._payload = payload

        def read(self, *_a):
            return self._payload

        def __enter__(self):
            return self

        def __exit__(self, *a):
            return False

    def _open(req, timeout=None):
        for suffix, action in routes.items():
            if req.full_url.endswith(suffix):
                if isinstance(action, BaseException):
                    raise action
                status, payload = action(req)
                return _Resp(status, payload)
        raise AssertionError(f"unexpected URL {req.full_url}")

    return _open


def _err(status, message):
    import urllib.error
    import json as _json
    return urllib.error.HTTPError(
        "https://opencode.ai/zen/v1", status, "e", {},
        __import__("io").BytesIO(_json.dumps({"error": {"message": message}}).encode()),
    )


def test_opencode_session_id_is_canonical():
    """The gateway rejects any session shape but this one.

    Measured live 2026-10-01: `ses_`+32hex -- what config.go used to emit -- is
    answered 403 FreeTierError, so a regression here is a production 403, not a
    cosmetic drift.
    """
    pattern = re.compile(r"^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$")
    for _ in range(50):
        sid = ff.opencode_session_id()
        assert pattern.match(sid), sid
    assert not pattern.match("ses_" + "a" * 32), "old shape must not validate"
    assert not pattern.match("ctx:abcdef0123456789"), "gateway id must not validate"


def test_opencode_gate_bodies_satisfy_every_documented_condition():
    """stream=true plus both `bash` and `read` in tools, per envelope.

    Measured: `read`+`edit`+`glob`, one `bash`, and two arbitrary names were all
    403. So this asserts the exact pair, not merely that tools is non-empty.
    """
    paths = dict(ff.opencode_gate_bodies("m"))
    assert set(paths) == {"/chat/completions", "/responses"}
    for path, raw in paths.items():
        body = json.loads(raw)
        assert body["stream"] is True, path
        assert body["model"] == "m", path
        names = set()
        for tool in body["tools"]:
            if path == "/responses":
                names.add(tool["name"])
            else:
                names.add(tool["function"]["name"])
        assert names == {"bash", "read"}, (path, names)


def test_opencode_probe_accepts_either_protocol():
    """A model may answer 400 on one protocol and 200 on the other.

    `muse-spark-1.3-contributor-free` -- the model in oh-my-pi#12306 -- answers
    `400 Model does not support this protocol` on /chat/completions and 200 on
    /responses. Probing only the first reports a working model as dead.
    """
    def routes(req):
        if req.full_url.endswith("/chat/completions"):
            return 400, b'{"error":{"message":"Model does not support this protocol."}}'
        return 200, b'{"type":"response.created"}'

    verdict, detail = ff.opencode_probe("muse-spark-1.3-contributor-free", "k",
                                        opener=_opener({"": routes}))
    assert verdict == "ok", detail
    assert "/responses" in detail, detail


def test_opencode_probe_403_is_freetier_but_400_is_only_an_error():
    """403 means "key is not entitled"; 400 means "I asked it the wrong question".

    Collapsing the second into the first is what deleted 8 working models from
    the chains.
    """
    verdict, _ = ff.opencode_probe("m", "k", opener=_opener({
        "": lambda req: (_ for _ in ()).throw(_err(403, "free tier"))}))
    assert verdict == "freetier"

    verdict, detail = ff.opencode_probe("m", "k", opener=_opener({
        "": lambda req: (_ for _ in ()).throw(_err(400, "Model is unavailable."))}))
    assert verdict == "error", detail
    assert "Model is unavailable" in detail, detail


def test_opencode_probe_aborts_the_sweep_on_429(monkeypatch):
    """A 429 is a property of the KEY, so it must abort rather than grind on.

    Retrying per candidate is what turned a rate-limited run into a ~40 minute
    hang: 12 candidates x 2 protocols x 3 attempts, each with a backoff sleep and
    a 30s request timeout. One exhausted 429 on the FIRST protocol has to be
    enough, and it must not surface as any verdict at all.
    """
    slept = []
    monkeypatch.setattr(ff.time, "sleep", slept.append)
    calls = []

    def routes(req):
        calls.append(req.full_url)
        return 429, b'{"error":{"message":"Rate limit exceeded."}}'

    with pytest.raises(ff.OpenCodeRateLimited):
        ff.opencode_probe("m", "k", opener=_opener({"": routes}))
    # One protocol only, and it gives up after OPENCODE_429_RETRIES attempts.
    assert len(calls) == ff.OPENCODE_429_RETRIES, len(calls)
    # A sleep BETWEEN retries, but not a pointless one after the last attempt.
    assert len(slept) == ff.OPENCODE_429_RETRIES - 1, slept


def test_rate_limited_sweep_keeps_models_unverified_not_dead(monkeypatch, capsys):
    """The dangerous failure is a throttled run silently deleting models.

    That is exactly the regression this probe was rebuilt to undo, so a 429 has
    to leave the untried candidates in the list marked unverified.
    """
    monkeypatch.setattr(ff, "fetch_json", lambda url, headers=None: {
        "data": [{"id": f"m{i}-free"} for i in range(6)],
    })
    monkeypatch.setenv("OPENCODE_API_KEY", "k")
    monkeypatch.setattr(ff.time, "sleep", lambda *_: None)

    def _always_429(model_id, key, opener=None):
        raise ff.OpenCodeRateLimited("429")

    monkeypatch.setattr(ff, "opencode_probe", _always_429)
    out = ff.fetch_opencode()
    assert len(out) == 6, [m["id"] for m in out]
    assert all(m.get("unverified_reason") for m in out), out
    assert not any(m.get("verified") is False for m in out), \
        "a rate limit must never be recorded as a dead model"
    err = capsys.readouterr().err
    assert "rate limited" in err, err
    assert "rejected unusable" not in err, err


def test_sweep_stops_at_the_probe_budget(monkeypatch, capsys):
    """A slow gateway must not stall the nightly sync forever, and overrunning
    the budget must again leave the remainder unverified rather than rejected."""
    monkeypatch.setattr(ff, "fetch_json", lambda url, headers=None: {
        "data": [{"id": f"m{i}-free"} for i in range(5)],
    })
    monkeypatch.setenv("OPENCODE_API_KEY", "k")
    monkeypatch.setattr(ff, "OPENCODE_PROBE_BUDGET", -1.0)
    monkeypatch.setattr(ff.time, "sleep", lambda *_: None)
    monkeypatch.setattr(ff, "opencode_probe",
                        lambda *a, **k: pytest.fail("must not probe past the budget"))

    out = ff.fetch_opencode()
    assert len(out) == 5, [m["id"] for m in out]
    assert all(m.get("unverified_reason") for m in out), out
    err = capsys.readouterr().err
    assert "budget" in err, err
    assert "rejected unusable" not in err, err


def test_opencode_headers_carry_the_gate_preconditions():
    h = ff.opencode_headers("k")
    assert h["User-Agent"].startswith("opencode/")
    assert re.match(r"^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$", h["x-opencode-session"])
    assert h["Authorization"] == "Bearer k"


def test_opencode_ua_matches_the_gateway():
    """The gateway and this fetcher must present the same official UA.

    OpenCode asks third-party gateways for "the correct, official User-Agent
    string (matching opencode/<version>)". The version lives in two files in two
    languages, and nothing else keeps them equal -- the proxy probes and the
    gateway serves requests with independently hardcoded copies, so bumping one
    silently leaves the other claiming to be a version that no longer exists.
    """
    go = (REPO / "config.go").read_text()
    m = re.search(r'"User-Agent":\s*"(opencode/[^"]+)"', go)
    assert m, "config.go no longer sets a User-Agent for the opencode gateway"
    assert m.group(1) == ff.OPENCODE_UA, (
        f"config.go sends {m.group(1)} but the fetcher sends {ff.OPENCODE_UA}; "
        "bump both from https://registry.npmjs.org/opencode-ai/latest"
    )


def test_opencode_ua_version_parses_the_shape():
    """The parser must not invent a version out of a string it cannot read."""
    assert ff.opencode_ua_version("opencode/1.18.34/cli") == "1.18.34"
    assert ff.opencode_ua_version(ff.OPENCODE_UA) == ff.OPENCODE_UA.split("/")[1]
    for bad in ("", "Go-http-client/1.1", "opencode/", "opencode"):
        assert ff.opencode_ua_version(bad) == "", bad


def _ua_registry(monkeypatch, payload):
    monkeypatch.setattr(ff, "fetch_json", lambda *a, **k: payload)


def test_opencode_ua_check_reports_a_stale_version(monkeypatch, capsys):
    monkeypatch.delenv("SKIP_OPENCODE_UA_CHECK", raising=False)
    monkeypatch.setattr(ff, "opencode_ua_version", lambda *a: "1.18.31")
    _ua_registry(monkeypatch, {"latest": "1.18.34"})
    assert ff.check_opencode_ua_version() == "1.18.34"
    err = capsys.readouterr().err
    assert "stale" in err
    # The warning has to be actionable, or it is just noise.
    assert "1.18.31" in err and "1.18.34" in err
    assert "OPENCODE_UA" in err and "config.go" in err


def test_opencode_ua_check_is_silent_when_current(monkeypatch, capsys):
    monkeypatch.delenv("SKIP_OPENCODE_UA_CHECK", raising=False)
    monkeypatch.setattr(ff, "opencode_ua_version", lambda *a: "1.18.34")
    _ua_registry(monkeypatch, {"latest": "1.18.34"})
    assert ff.check_opencode_ua_version() is None
    assert capsys.readouterr().err == ""


def test_opencode_ua_check_is_opt_out(monkeypatch, capsys):
    """SKIP_OPENCODE_UA_CHECK=1 must suppress the check AND its network call.

    It is opt-out rather than opt-in because the failure it hunts is silent and
    slow-moving: a check you have to remember to run is the check that does not
    run. The opt-out has to be complete, though — an air-gapped machine must not
    pay a registry round trip just to be told nothing.
    """
    monkeypatch.setenv("SKIP_OPENCODE_UA_CHECK", "1")
    monkeypatch.setattr(
        ff, "fetch_json",
        lambda *a, **k: pytest.fail("opt-out must not reach the network"),
    )
    assert ff.check_opencode_ua_version() is None
    assert capsys.readouterr().err == ""


def test_opencode_ua_check_survives_a_dead_registry(monkeypatch, capsys):
    """A sync must not fail, or nag, because a version lookup did.

    The registry being unreachable is not evidence about the UA, and this
    script's exit code is consumed by the sync timer.
    """
    monkeypatch.delenv("SKIP_OPENCODE_UA_CHECK", raising=False)
    for payload in (None, {}, {"latest": None}, {"latest": 7}, ["nope"]):
        _ua_registry(monkeypatch, payload)
        assert ff.check_opencode_ua_version() is None, payload
    assert capsys.readouterr().err == ""


def test_probe_endpoint_uses_the_gate_body_for_opencode():
    """`big-pickle` was recorded permanently dead because its probe 403'd on its
    own request shape."""
    sent = {}

    def opener(req, timeout=None):
        sent["body"] = json.loads(req.data)
        sent["url"] = req.full_url
        class _R:
            status = 200
            def read(self, *_a): return b"{}"
            def __enter__(self): return self
            def __exit__(self, *a): return False
        return _R()

    res = ff.probe_endpoint("https://opencode.ai/zen/v1", "big-pickle", "k",
                            ff.opencode_headers("k"), opener=opener,
                            opencode_gate=True)
    assert res["verdict"] == "live", res
    assert sent["body"]["stream"] is True, sent["body"]
    names = {t["function"]["name"] for t in sent["body"]["tools"]}
    assert names == {"bash", "read"}, names


def test_mark_unverified_leaves_verified_unset():
    """Never-probed must mean `unknown`, not `dead`.

    regenerate_config.py reads the field three ways: True -> live, False -> dead
    (the provider refused), absent/None -> unknown (never asked). Writing False
    for a model that was never probed declared it dead, so a run without a key
    dropped the very endpoints the probe exists to protect.
    """
    out = ff._mark_unverified([{"id": "m"}], "test")
    assert out[0].get("verified") is None, out
    assert "verified" not in out[0], f"the key must be absent, not False: {out}"
    assert "test" in out[0]["unverified_reason"]


if __name__ == "__main__":
    sys.exit(pytest.main([__file__, "-v"]))


def test_probe_cache_round_trips(tmp_path, monkeypatch):
    """A steady-state sync must spend no probe calls at all."""
    cache_file = tmp_path / "opencode-probe-cache.json"
    monkeypatch.setattr(ff, "OPENCODE_PROBE_CACHE_FILE", cache_file)
    monkeypatch.setattr(ff, "fetch_json", lambda url, headers=None: {
        "data": [{"id": "m1-free"}, {"id": "m2-free"}],
    })
    monkeypatch.setenv("OPENCODE_API_KEY", "k")
    monkeypatch.setattr(ff.time, "sleep", lambda *_: None)

    probed = []
    monkeypatch.setattr(ff, "opencode_probe",
                        lambda m, k, opener=None: (probed.append(m), ("ok", "/chat/completions 200"))[1])

    first = ff.fetch_opencode()
    assert len(first) == 2 and probed == ["m1-free", "m2-free"]
    assert cache_file.exists(), "the first sweep must persist its verdicts"

    probed.clear()
    second = ff.fetch_opencode()
    assert len(second) == 2
    assert probed == [], f"a cached sweep must not call the network: {probed}"
    assert all(m.get("verified") is True for m in second), second


def test_probe_cache_expires_negatives_far_faster_than_positives(tmp_path, monkeypatch):
    """Asymmetric TTL is the safety property, not an optimisation.

    A cached `ok` going stale costs a chain entry that may 403 briefly. A cached
    `freetier` going stale for a week would keep a working model OUT of the
    chains for a week -- exactly the regression this probe was rebuilt to undo.
    """
    monkeypatch.setattr(ff, "OPENCODE_PROBE_OK_TTL_HOURS", 168)
    monkeypatch.setattr(ff, "OPENCODE_PROBE_NEG_TTL_HOURS", 24)
    assert ff.opencode_probe_cache_ttl_hours("ok") == 168
    assert ff.opencode_probe_cache_ttl_hours("freetier") == 24
    assert ff.opencode_probe_cache_ttl_hours("error") == 24

    old = datetime.now(UTC) - timedelta(hours=30)
    stamp = old.isoformat().replace("+00:00", "Z")
    cache_file = tmp_path / "c.json"
    cache_file.write_text(json.dumps({"probes": {
        "good-free": {"verdict": "ok", "checked_at": stamp},
        "gone-free": {"verdict": "freetier", "checked_at": stamp},
    }}))
    monkeypatch.setattr(ff, "OPENCODE_PROBE_CACHE_FILE", cache_file)

    live = ff.load_opencode_probe_cache()
    assert "good-free" in live, "a 30h-old positive verdict must survive"
    assert "gone-free" not in live, "a 30h-old negative verdict must be re-probed"


def test_probe_cache_ignores_garbage_and_never_raises(tmp_path, monkeypatch):
    """A cache miss is always safe; a cache read must never cost the model list."""
    bad = tmp_path / "c.json"
    for payload in ("not json at all", "[]", '{"probes": "nope"}',
                    '{"probes": {"m": {"verdict": "bogus"}}}',
                    '{"probes": {"m": {"verdict": "ok", "checked_at": "nonsense"}}}'):
        bad.write_text(payload)
        monkeypatch.setattr(ff, "OPENCODE_PROBE_CACHE_FILE", bad)
        assert ff.load_opencode_probe_cache() == {}, payload

    missing = tmp_path / "absent.json"
    monkeypatch.setattr(ff, "OPENCODE_PROBE_CACHE_FILE", missing)
    assert ff.load_opencode_probe_cache() == {}


def test_refresh_env_forces_a_live_probe(tmp_path, monkeypatch):
    monkeypatch.setattr(ff, "OPENCODE_PROBE_CACHE_FILE", tmp_path / "c.json")
    (tmp_path / "c.json").write_text(json.dumps({"probes": {
        "m1-free": {"verdict": "ok", "checked_at":
                    ff.datetime.now(ff.UTC).isoformat().replace("+00:00", "Z")},
    }}))
    monkeypatch.setattr(ff, "fetch_json", lambda url, headers=None: {"data": [{"id": "m1-free"}]})
    monkeypatch.setenv("OPENCODE_API_KEY", "k")
    monkeypatch.setenv("REFRESH_OPENCODE_PROBE", "1")
    monkeypatch.setattr(ff.time, "sleep", lambda *_: None)

    probed = []
    monkeypatch.setattr(ff, "opencode_probe",
                        lambda m, k, opener=None: (probed.append(m), ("ok", "200"))[1])
    ff.fetch_opencode()
    assert probed == ["m1-free"], "REFRESH_OPENCODE_PROBE=1 must ignore the cache"


def test_rate_limited_run_does_not_persist_a_half_finished_pass(tmp_path, monkeypatch):
    """A sweep that stopped early must not become a week of cached verdicts."""
    cache_file = tmp_path / "c.json"
    monkeypatch.setattr(ff, "OPENCODE_PROBE_CACHE_FILE", cache_file)
    monkeypatch.setattr(ff, "fetch_json", lambda url, headers=None: {
        "data": [{"id": "m1-free"}, {"id": "m2-free"}, {"id": "m3-free"}],
    })
    monkeypatch.setenv("OPENCODE_API_KEY", "k")
    monkeypatch.setattr(ff.time, "sleep", lambda *_: None)

    def _first_only(model_id, key, opener=None):
        if model_id == "m1-free":
            return "ok", "/chat/completions 200"
        raise ff.OpenCodeRateLimited("429")

    monkeypatch.setattr(ff, "opencode_probe", _first_only)
    out = ff.fetch_opencode()
    assert len(out) == 3, [m["id"] for m in out]
    saved = json.loads(cache_file.read_text())["probes"]
    assert set(saved) == {"m1-free"}, saved


# ── Runtime free-tier veto (cooldowns.json) ────────────────────────────────────
#
# Every other fetch_* test asks "does the provider say this model is free?". These
# ask the question none of them can: "does the GATEWAY get to use it?", which is
# the only place the three real losses were visible — ollama:minimax-m3 (402 x4,
# from a CURATED list that makes no API call at all), opencode:muse-spark-1.3
# (403 FreeTierError x42, which the fetcher's own probe passes because its body
# carries the stream+tools the provider's gate requires) and
# nvidia*:moonshotai/kimi-k2.6 (404 on a deleted function still listed upstream).

def _cd(status, errors=3, body="", hours=24):
    """A cooldowns.json entry as the daemon writes it."""
    return {
        "expiry": (datetime.now(UTC) + timedelta(hours=hours)).isoformat(),
        "status_code": status,
        "error_count": errors,
        "last_error": body,
    }


def _write_cooldowns(tmp_path, entries, circuits=None):
    path = tmp_path / "cooldowns.json"
    path.write_text(json.dumps({"cooldowns": entries, "circuits": circuits or {}}))
    return path


def _free_tier_fixture(tmp_path, monkeypatch, entries):
    """A veto wired to a temp cooldowns file and a temp denial file."""
    monkeypatch.setattr(ff, "RUNTIME_DENIAL_FILE", tmp_path / "free-tier-denied.json")
    return ff.runtime_free_tier_denials(_write_cooldowns(tmp_path, entries))


def test_free_tier_refusal_vetoes_a_curated_model(tmp_path, monkeypatch):
    """The Ollama case: no provider API can report it, so the veto must.

    fetch_ollama() emits a hand-curated marklist and makes no call at all, so a
    model that leaves the free tier survives every sync forever. Before the veto
    this stayed in smart, work and large while all four calls came back 402.
    """
    monkeypatch.setattr(ff, "RUNTIME_DENIAL_FILE", tmp_path / "free-tier-denied.json")
    denials = _free_tier_fixture(tmp_path, monkeypatch, {
        "ollama:minimax-m3": _cd(402, 4, "this model is not included in your free usage"),
        # An ollama id carries its own colons, so the key is "provider:id" with a
        # colon in the id too. Splitting on the wrong one silently loses it.
        "ollama:gemma4:31b": _cd(402, 4, "not included in your free usage"),
    })
    assert "minimax-m3" in ff.OLLAMA_FREE_MODELS, "the curated list is the fixture"

    models = ff.fetch_ollama()
    assert any(m["id"] == "minimax-m3" for m in models), "precondition: it is emitted"

    kept = ff.apply_runtime_denials(models, denials)
    assert "minimax-m3" in ff.OLLAMA_FREE_MODELS, (
        "the veto must not edit the curated list: undoing the evidence would then "
        "need a code change, and a later upstream change would leave it wrong"
    )
    assert {"minimax-m3", "gemma4:31b"}.isdisjoint({m["id"] for m in kept}), kept
    assert len(kept) == len(ff.OLLAMA_FREE_MODELS) - 2, [m["id"] for m in kept]


@pytest.mark.parametrize("status,errors,body", [
    (429, 40, "rate limit exceeded"),
    (500, 30, "internal server error"),
    (503, 30, "upstream unavailable"),
    (0, 22, "context deadline exceeded"),
    (401, 9, "invalid api key"),
])
def test_transient_cooldown_is_never_a_veto(tmp_path, monkeypatch, status, errors, body):
    """The key or the network talking says nothing about the model.

    Vetoing here would strip the chains during an outage, which is a worse
    incident than the rate limit it replaced — and status_code 0 with
    "context deadline exceeded" is what nvidia's slowest models produce on an
    ordinary bad afternoon.
    """
    denials = _free_tier_fixture(tmp_path, monkeypatch, {
        "nvidia:some-model": _cd(status, errors, body),
    })
    assert denials == {}, denials
    assert ff.classify_cooldown(_cd(status, errors, body), datetime.now(UTC)) is None


def test_a_lone_403_is_not_a_verdict_but_a_repeated_one_is():
    """One 403 is a blip; the router itself escalates at 3.

    OpenCode's gate also refuses on the REQUEST SHAPE, so a low count may just
    be one odd client. Requiring recurrence is what keeps a single mis-shaped
    request from deleting a model from every chain.
    """
    body = '{"type":"FreeTierError","error":{"type":"FreeTierError"}}'
    now = datetime.now(UTC)
    assert ff.classify_cooldown(_cd(403, 1, body), now) is None
    assert ff.classify_cooldown(_cd(403, 2, body), now) is None
    verdict = ff.classify_cooldown(_cd(403, 3, body), now)
    assert verdict is not None and verdict[0] == "not-free", verdict


def test_403_without_free_tier_wording_is_not_a_veto():
    """A 403 that never mentions the free tier is about the key, not the model.

    Rotated or revoked credentials produce exactly this, and it must not empty
    the chains — that would turn an auth problem into a routing outage.
    """
    verdict = ff.classify_cooldown(
        _cd(403, 40, '{"error":{"message":"permission denied for this project"}}'),
        datetime.now(UTC))
    assert verdict is None, verdict


def test_404_needs_no_repetition():
    """A 404 is a statement about the id: baseCooldownForError gives it 7 days
    at error_count 1, so one is authoritative here too."""
    verdict = ff.classify_cooldown(_cd(404, 1, "Not found for account 'x'"),
                                   datetime.now(UTC))
    assert verdict is not None and verdict[0] == "gone", verdict


def test_expired_cooldown_is_not_evidence():
    """The daemon deletes an entry when it expires and on the first success."""
    now = datetime.now(UTC)
    expired = _cd(402, 40, "not included in your free usage", hours=-1)
    assert ff.classify_cooldown(expired, now) is None
    assert ff.classify_cooldown(_cd(402, 40, "not included in your free usage",
                                    hours=24), now) is not None
    assert ff.classify_cooldown(_cd(402, 40, "x", hours=24) | {"expiry": "nonsense"},
                                now) is None


def test_one_key_refusal_vetoes_the_whole_multi_key_family(tmp_path, monkeypatch):
    """nvidia2 is not a different model, so it must not be a different verdict.

    The trio shares one model id across three keys, so a denial recorded
    against whichever key happened to fail has to reach the whole record.
    """
    denials = _free_tier_fixture(tmp_path, monkeypatch, {
        "nvidia2:z-ai/glm-9": _cd(404, 1, "Function 'x': Not found"),
    })
    for source, expected_gone in (("nvidia-nim", True), ("commandcode", False),
                                  ("ollama-cloud", False), ("opencode", False)):
        # Paired with a model nothing denies: a veto must never be able to empty
        # the list, so a one-model fixture would test that guard instead.
        kept = ff.apply_runtime_denials(
            [{"id": "z-ai/glm-9", "provider": source},
             {"id": "control-free", "provider": "kilocode"}], denials)
        assert ("z-ai/glm-9" in {m["id"] for m in kept}) is not expected_gone, (
            source, [m["id"] for m in kept])
        assert [m["id"] for m in kept if m["id"] != "z-ai/glm-9"] == ["control-free"]


def test_terminators_are_never_vetoed(tmp_path, monkeypatch):
    """The auto routers are decided by rule 6's probe, which switches routers on
    a refusal. Removing one here would leave regenerate_config.py with no
    terminator at all, which makes it refuse to write the file."""
    denials = _free_tier_fixture(tmp_path, monkeypatch, {
        "opencode:big-pickle": _cd(403, 50, "FreeTierError"),
        "kilocode:kilo-auto/free": _cd(403, 50, "FreeTierError"),
    })
    models = [{"id": "big-pickle", "provider": "opencode"},
              {"id": "kilo-auto/free", "provider": "kilocode"}]
    assert ff.apply_runtime_denials(models, denials) == models


def test_runtime_refusal_outranks_a_fresh_probe(tmp_path, monkeypatch, capsys):
    """The muse-spark case, and the ranking that has to be stated out loud.

    The fetcher's probe sends stream=true plus a tools array with bash and read,
    which is exactly what OpenCode's gate requires, so it answers 200 for a
    model the gateway is refused on 42 times in a row. The daemon serves real
    client bodies, so its record wins — and the conflict is reported rather than
    resolved silently in either direction.
    """
    denials = _free_tier_fixture(tmp_path, monkeypatch, {
        "opencode:muse-spark-1.3-contributor-free": _cd(
            403, 42, "OpenCode's free tier can only be used from within OpenCode"),
    })
    models = [{"id": "muse-spark-1.3-contributor-free", "provider": "opencode",
               "verified": True},
              {"id": "muse-spark-1.2-contributor-free", "provider": "opencode",
               "verified": True}]
    kept = ff.apply_runtime_denials(models, denials)
    assert [m["id"] for m in kept] == ["muse-spark-1.2-contributor-free"], kept
    out = capsys.readouterr().err
    assert "muse-spark-1.3-contributor-free" in out and "42x" in out, out
    assert "probe answered OK" in out, (
        "a model dropped while this run's probe said OK is the one case an "
        "operator must see; silently dropping it hides a gate disagreement"
    )


def test_the_veto_can_never_empty_the_model_list(tmp_path, monkeypatch, capsys):
    """An empty list means regenerate_config.py has no chains to write.

    A mis-set threshold, or a cooldowns.json that says the gateway refused the
    whole catalog because it is DOWN, must cost a warning and nothing else.
    """
    denials = {f"kilocode:m{i}-free": {"verdict": "gone", "reason": "404",
                                       "last_seen": "x", "until": "y"}
               for i in range(3)}
    models = [{"id": f"m{i}-free", "provider": "kilocode"} for i in range(3)]
    assert ff.apply_runtime_denials(models, denials) == models
    assert "veto NOT applied" in capsys.readouterr().err


def test_a_denial_expires_so_a_restored_entitlement_comes_back(tmp_path, monkeypatch):
    """Nothing else may be needed to undo this, ever.

    The whole design rests on the veto being self-clearing: a quota that is
    restored, or a key that is fixed, must put the model back in the chains
    without a code change.
    """
    monkeypatch.setattr(ff, "RUNTIME_DENIAL_FILE", tmp_path / "free-tier-denied.json")
    monkeypatch.setattr(ff, "RUNTIME_DENIAL_TTL_HOURS", 1)
    _free_tier_fixture(tmp_path, monkeypatch, {
        "ollama:minimax-m3": _cd(402, 4, "not included in your free usage"),
    })
    assert "ollama:minimax-m3" in ff.load_runtime_denials(datetime.now(UTC))

    later = datetime.now(UTC) + timedelta(hours=2)
    assert ff.load_runtime_denials(later) == {}
    # An empty cooldowns.json later on must not resurrect it either.
    assert ff.runtime_free_tier_denials(_write_cooldowns(tmp_path, {}), now=later) == {}


def test_denials_persist_across_a_quiet_week(tmp_path, monkeypatch):
    """cooldowns.json forgets; the denial file must not.

    An entry only exists while it is unexpired, so a model nobody routed to for
    a week loses its evidence — and the next sync would put a paid endpoint
    straight back in the chains.
    """
    monkeypatch.setattr(ff, "RUNTIME_DENIAL_FILE", tmp_path / "free-tier-denied.json")
    _free_tier_fixture(tmp_path, monkeypatch, {
        "opencode:muse-spark-1.3-contributor-free": _cd(403, 42, "FreeTierError"),
    })
    later = datetime.now(UTC) + timedelta(days=1)
    assert ff.runtime_free_tier_denials(_write_cooldowns(tmp_path, {}), now=later), (
        "an empty cooldowns.json means no traffic, not a restored entitlement"
    )
    saved = json.loads((tmp_path / "free-tier-denied.json").read_text())["denials"]
    entry = saved["opencode:muse-spark-1.3-contributor-free"]
    assert entry["verdict"] == "not-free" and "FreeTierError" in entry["last_error"]


@pytest.mark.parametrize("payload", [
    "not json at all",
    "[]",
    '"a string"',
    '{"cooldowns": "nope"}',
    '{"cooldowns": {"no-colon": {"status_code": 402}}}',
])
def test_cooldowns_reader_ignores_garbage_and_never_raises(tmp_path, monkeypatch, payload):
    """The file belongs to another process; unreadable means no evidence."""
    monkeypatch.setattr(ff, "RUNTIME_DENIAL_FILE", tmp_path / "free-tier-denied.json")
    bad = tmp_path / "cooldowns.json"
    bad.write_text(payload)
    assert ff.runtime_free_tier_denials(bad) == {}

    assert ff.runtime_free_tier_denials(tmp_path / "absent.json") == {}


def test_circuits_are_not_verdicts(tmp_path, monkeypatch, capsys):
    """state 1 (open) and 2 (half-open) describe a probe in flight.

    Reading them as refusals would veto whatever half-open circuit happened to
    be open — the exact opposite of what a probe means.
    """
    monkeypatch.setattr(ff, "RUNTIME_DENIAL_FILE", tmp_path / "free-tier-denied.json")
    denials = ff.runtime_free_tier_denials(_write_cooldowns(
        tmp_path, {}, circuits={"ollama:minimax-m3": {"state": 1, "opened_at": "x",
                                                       "probes_sent": 2}}))
    assert denials == {}, denials
    models = [{"id": "minimax-m3", "provider": "ollama-cloud"}]
    assert ff.apply_runtime_denials(models, denials) == models


def test_cooldowns_report_is_offline(tmp_path, monkeypatch, capsys):
    """Step 0 of the sync must answer when every provider is unreachable.

    The nightly sync runs this before the fetch, so it is the one part of the
    pipeline that has to work during an outage — and it is what makes the sync
    log say which endpoints the gateway is refusing rather than dropping them
    silently.
    """
    monkeypatch.setattr(ff, "RUNTIME_DENIAL_FILE", tmp_path / "free-tier-denied.json")
    monkeypatch.setattr(ff, "fetch_json", lambda *a, **k: pytest.fail("touched the network"))
    monkeypatch.setattr(ff, "fetch_ollama", lambda: pytest.fail("did provider work"))
    path = _write_cooldowns(tmp_path, {
        "ollama:minimax-m3": _cd(402, 4, "not included in your free usage"),
        "nvidia:z-ai/glm-5.3-flash": _cd(0, 22, "context deadline exceeded"),
    })
    monkeypatch.setattr(sys, "argv", ["fetch-free-models.py", "--cooldowns-report",
                                      "--cooldowns", str(path)])
    ff.main()
    out = capsys.readouterr().out
    assert "ollama:minimax-m3" in out and "not-free" in out, out
    assert "glm-5.3-flash" not in out, "a timeout is not a free-tier statement"


def test_provider_group_mirror_matches_regenerate_config():
    """The veto matches a cooldown key against a model, and the mapping from a
    JSON source to a config provider name lives in TWO files.

    regenerate_config.py writes the config, fetch-free-models.py decides which
    models may go into it, and a mismatch is silent: the veto would look up
    "ollama-cloud:x" where the daemon wrote "ollama:x" and never fire.
    """
    import importlib.util

    spec = importlib.util.spec_from_file_location("regenerate_config",
                                                  REPO / "regenerate_config.py")
    gen = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(gen)

    for source in sorted(ff.CONFIG_PROVIDER_FOR_SOURCE):
        model = gen.Model(id="m", name="m", provider=source, context_length=0,
                          intelligence=None, intelligence_source=None,
                          intelligence_note=None, elo=None, vision=False, raw={})
        assert ff.CONFIG_PROVIDER_FOR_SOURCE[source] == model.mapped_provider, (
            f"{source}: fetch-free-models.py says "
            f"{ff.CONFIG_PROVIDER_FOR_SOURCE[source]!r}, regenerate_config.py writes "
            f"{model.mapped_provider!r}; the veto would silently stop matching"
        )
        assert ff.config_provider_group(source)[0] == model.mapped_provider


# ── Which wire shape a model speaks ───────────────────────────────────────────

@pytest.mark.parametrize("detail,want", [
    ("/chat/completions 200", "chat"),
    ("/responses 200", "responses"),
    # Defensive, not the format opencode_probe returns today: on success it
    # returns only the path that answered. A cached detail written by an older
    # fetcher, or a future probe that keeps the refusals in the detail, must
    # still yield the answer rather than lose it to the refusals.
    ("/chat/completions 400 Model does not support this protocol.; /responses 200",
     "responses"),
    ("/responses 400 Model does not support this protocol.; /chat/completions 200",
     "chat"),
    # No 200 anywhere: nothing was proved, and absence is the only honest value.
    ("/chat/completions 400; /responses 429", None),
    ("", None),
    ("200", None),
])
def test_probe_detail_names_the_protocol_that_answered(detail, want):
    assert ff.protocol_for_probe_detail(detail) == want


def test_verified_opencode_record_carries_the_protocol_that_answered(tmp_path, monkeypatch):
    """The probe already knows the shape; the record has to say so out loud.

    Measured on opencode.ai 2026-10-02: muse-spark-1.{2,3}-contributor-free
    answer 400 ModelProtocolUnsupported on /chat/completions and 200 on
    /responses, big-pickle is the other way round. Without this the shape is
    rediscovered at runtime by a failed round trip on every process start, and
    nothing reaches regenerate_config.py, which is what writes `protocol:`.
    """
    monkeypatch.setattr(ff, "OPENCODE_PROBE_CACHE_FILE", tmp_path / "c.json")
    monkeypatch.setattr(ff, "fetch_json", lambda url, headers=None: {
        "data": [{"id": "resp-only-free"}, {"id": "chat-only-free"}],
    })
    monkeypatch.setenv("OPENCODE_API_KEY", "k")
    monkeypatch.setattr(ff.time, "sleep", lambda *_: None)
    monkeypatch.setattr(ff, "opencode_probe", lambda m, k, opener=None: (
        "ok", "/responses 200" if m == "resp-only-free" else "/chat/completions 200"))

    got = {m["id"]: m.get("protocol") for m in ff.fetch_opencode()}
    assert got == {"resp-only-free": "responses", "chat-only-free": "chat"}, got


def test_an_unprobed_opencode_record_claims_no_protocol(tmp_path, monkeypatch):
    """A record nobody probed has no verdict, so it must not name a shape.

    The alternative is inventing a protocol, which turns "the router will
    discover this at runtime for one wasted round trip" into "every request to
    this endpoint fails". regenerate_config.py preserves whatever config.yaml
    already declares, which is the honest fallback.
    """
    monkeypatch.setattr(ff, "OPENCODE_PROBE_CACHE_FILE", tmp_path / "c.json")
    monkeypatch.setattr(ff, "fetch_json", lambda url, headers=None: {
        "data": [{"id": "untried-free"}],
    })
    monkeypatch.setenv("OPENCODE_API_KEY", "k")

    def _limited(model_id, key, opener=None):
        raise ff.OpenCodeRateLimited("429")

    monkeypatch.setattr(ff, "opencode_probe", _limited)
    out = ff.fetch_opencode()
    assert [m["id"] for m in out] == ["untried-free"]
    assert "protocol" not in out[0], out[0]


# ── First-seen cache ─────────────────────────────────────────────────────
# The cache is committed to the repo, so it must round-trip exactly,
# tolerate the format it had before the prune rule existed, and prune
# only on the rule the sync depends on: absent from a full census for
# FIRST_SEEN_MISSING_DAYS.


def test_first_seen_state_round_trips_both_maps(tmp_path, monkeypatch):
    monkeypatch.setattr(ff, "FIRST_SEEN_CACHE_FILE", tmp_path / "fs.json")
    ff.save_first_seen_cache(
        {"a-free": "2026-09-26"}, {"b-free": "2026-10-01"}
    )
    assert ff.load_first_seen_state() == (
        {"a-free": "2026-09-26"},
        {"b-free": "2026-10-01"},
    )


def test_first_seen_loader_returns_empty_when_absent(tmp_path, monkeypatch):
    monkeypatch.setattr(ff, "FIRST_SEEN_CACHE_FILE", tmp_path / "nope.json")
    assert ff.load_first_seen_state() == ({}, {})


def test_first_seen_loader_tolerates_the_pre_prune_format(tmp_path, monkeypatch):
    """The committed cache predates missing_since; it must still load."""
    path = tmp_path / "fs.json"
    path.write_text(json.dumps({
        "fetched_at": "2026-10-06T00:00:00Z",
        "first_seen": {"a-free": "2026-09-26"},
    }))
    monkeypatch.setattr(ff, "FIRST_SEEN_CACHE_FILE", path)
    assert ff.load_first_seen_state() == ({"a-free": "2026-09-26"}, {})


@pytest.mark.parametrize("payload", ["", "{not json", "[]", "42", '{"first_seen": 7}'])
def test_first_seen_loader_survives_garbage(tmp_path, monkeypatch, payload):
    path = tmp_path / "fs.json"
    path.write_text(payload)
    monkeypatch.setattr(ff, "FIRST_SEEN_CACHE_FILE", path)
    assert ff.load_first_seen_state() == ({}, {})


def test_first_seen_prunes_a_model_unlisted_for_a_month():
    """30 days without a listing deletes the entry and the countdown."""
    today = date(2026, 10, 6)
    first_seen = {"gone-free": "2026-09-01"}
    missing_since = {"gone-free": "2026-09-06"}  # exactly 30 days
    newly, pruned = ff.update_first_seen_cache(
        first_seen, missing_since, census=set(), models=[], today=today,
    )
    assert (newly, pruned) == (0, 1)
    assert first_seen == {}
    assert missing_since == {}


def test_first_seen_keeps_a_model_unlisted_under_a_month():
    """A day short of a month is still a possible transient outage."""
    today = date(2026, 10, 6)
    first_seen = {"gone-free": "2026-09-01"}
    missing_since = {"gone-free": "2026-09-07"}  # 29 days
    _, pruned = ff.update_first_seen_cache(
        first_seen, missing_since, census=set(), models=[], today=today,
    )
    assert pruned == 0
    assert first_seen == {"gone-free": "2026-09-01"}
    assert missing_since == {"gone-free": "2026-09-07"}


def test_first_seen_resets_the_countdown_when_a_model_is_listed_again():
    """A returning model keeps its original first-seen date."""
    today = date(2026, 10, 6)
    first_seen = {"back-free": "2026-09-01"}
    missing_since = {"back-free": "2026-09-20"}
    _, pruned = ff.update_first_seen_cache(
        first_seen, missing_since, census={"back-free"}, models=[],
        today=today,
    )
    assert pruned == 0
    assert missing_since == {}
    assert first_seen == {"back-free": "2026-09-01"}


def test_first_seen_starts_the_countdown_on_the_first_absence():
    today = date(2026, 10, 6)
    first_seen = {"gone-free": "2026-09-01"}
    missing_since = {}
    _, pruned = ff.update_first_seen_cache(
        first_seen, missing_since, census=set(), models=[], today=today,
    )
    assert pruned == 0
    assert first_seen == {"gone-free": "2026-09-01"}
    assert missing_since == {"gone-free": "2026-10-06"}


def test_first_seen_never_prunes_on_a_partial_fetch():
    """A --*-only run is not a census, so absence proves nothing."""
    today = date(2026, 10, 6)
    first_seen = {"gone-free": "2026-09-01"}
    missing_since = {"gone-free": "2026-08-01"}  # long overdue
    _, pruned = ff.update_first_seen_cache(
        first_seen, missing_since, census=set(), models=[],
        today=today, prune=False,
    )
    assert pruned == 0
    assert first_seen == {"gone-free": "2026-09-01"}
    assert missing_since == {"gone-free": "2026-08-01"}


def test_first_seen_records_then_reuses_the_first_date():
    today = date(2026, 10, 6)
    first_seen, missing_since = {}, {}
    models = [{"id": "new-free", "released": None}]
    newly, _ = ff.update_first_seen_cache(
        first_seen, missing_since, {"new-free"}, models, today,
    )
    assert newly == 1
    assert models[0]["released"] == "2026-10-06"
    assert models[0]["released_source"] == "first-seen (this run)"

    # The provider never grew a real date, so the cached one answers.
    again = [{"id": "new-free", "released": None}]
    newly, _ = ff.update_first_seen_cache(
        first_seen, missing_since, {"new-free"}, again, today,
    )
    assert newly == 0
    assert again[0]["released"] == "2026-10-06"
    assert again[0]["released_source"] == "first-seen cache"


def test_first_seen_never_overrides_a_release_date():
    """AA already dated the model; the cache must not stamp over it."""
    models = [{"id": "dated-free", "released": "2025-01-01"}]
    first_seen, missing_since = {}, {}
    newly, _ = ff.update_first_seen_cache(
        first_seen, missing_since, {"dated-free"}, models, date(2026, 10, 6),
    )
    assert newly == 0
    assert models[0]["released"] == "2025-01-01"
    assert "released_source" not in models[0]
    assert first_seen == {}


def test_first_seen_ignores_meta_router_models():
    """The auto-fallback terminators are never real candidates."""
    today = date(2026, 10, 6)
    for mid in ff.AUTO_FALLBACK_MODELS:
        first_seen, missing_since = {}, {}
        models = [{"id": mid, "released": None}]
        newly, _ = ff.update_first_seen_cache(
            first_seen, missing_since, {mid}, models, today,
        )
        assert newly == 0
        assert models[0]["released"] is None
        assert first_seen == {}
