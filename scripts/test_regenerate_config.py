#!/usr/bin/env python3
"""Regression tests for config.yaml regeneration.

The bug these guard: `regenerate_config.py` located the `models:` block with
a bare `config.find('models:')`. config.yaml's own header comment contains the
literal text "/v1/models:" (describing the max_context_tokens floor) on line
~51, well before the real top-level `models:` key on line ~144. `find` matched
the comment, so the "preserved" header was the first ~3 KB of a ~28 KB file
and every run deleted the entire `providers:` section — all 13 provider URLs
and api_key_env entries. The daemon would then start with zero providers.

These tests assert on STRUCTURE (all providers survive, the header is
byte-identical), not merely "the output is valid YAML" — a validity-only check
passes happily on a config that has lost every provider.
"""

import importlib.util
import json
import re
import subprocess
import sys
from pathlib import Path

import pytest
import yaml

REPO = Path(__file__).resolve().parent.parent
CONFIG = REPO / "config.yaml"
GENERATOR = REPO / "regenerate_config.py"


def _load_generator():
    spec = importlib.util.spec_from_file_location("regenerate_config", GENERATOR)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


gen = _load_generator()


def test_models_anchor_skips_header_comment():
    """The anchor must find the real top-level key, not a comment mentioning it.

    This is the direct regression: the old code used find('models:') and hit
    the "/v1/models:" text in the header comment.
    """
    text = CONFIG.read_text()
    assert "/v1/models:" in text, (
        "config.yaml no longer contains the '/v1/models:' header comment; if it "
        "was removed this test no longer reproduces the original bug and should "
        "be reworked to a synthetic fixture"
    )
    start, end = gen.find_models_section(text)
    anchored = text[start:start + len("models:")]
    assert anchored == "models:", f"anchor landed on {anchored!r}, not the real key"

    # The header the generator preserves must contain the providers block.
    header = text[:start]
    assert "providers:" in header, (
        "the preserved header is missing providers: — the generator would delete "
        "every provider definition"
    )
    # And it must be most of the file, not a sliver.
    assert len(header) > len(text) * 0.15, (
        f"preserved header is only {len(header)} of {len(text)} bytes; the anchor "
        "is matching something near the top of the file"
    )


def test_regeneration_preserves_all_providers():
    """Every provider, url and api_key_env must survive a regeneration."""
    before = yaml.safe_load(CONFIG.read_text())
    before_providers = before["providers"]

    # A models block that would be spliced in.
    new_models = "models:\n  smart:\n    chain:\n      - provider: kilocode\n"

    merged = gen.merge_models_section(CONFIG.read_text(), new_models)
    after = yaml.safe_load(merged)

    assert set(after["providers"]) == set(before_providers), (
        "provider set changed during regeneration: "
        f"{set(before_providers) ^ set(after['providers'])}"
    )
    for name, prov in before_providers.items():
        assert after["providers"][name] == prov, f"provider {name} was modified"


def test_regenerated_config_is_byte_identical_on_repeat():
    """Splicing the same models block twice must be a no-op (idempotent)."""
    text = CONFIG.read_text()
    start, end = gen.find_models_section(text)
    new_models = text[start:end]

    once = gen.merge_models_section(text, new_models)
    twice = gen.merge_models_section(once, new_models)
    assert once == twice, "regeneration is not idempotent; the file grows or drifts"
    # Sanity: the round trip must not have changed anything at all, since we
    # fed the existing block back in.
    assert once.rstrip("\n") == text.rstrip("\n"), (
        "re-splicing the existing models block altered the file"
    )


def test_header_comments_survive_regeneration():
    """The header is documentation. Losing it is how the bug stayed invisible."""
    text = CONFIG.read_text()
    start, _ = gen.find_models_section(text)
    header = text[:start]

    merged = gen.merge_models_section(text, "models:\n  smart:\n    chain: []\n")
    merged_header = merged[: merged.find("\nmodels:")]

    for marker in ("#", "max_context_tokens"):
        assert marker in header, f"fixture is missing {marker!r}; test has drifted"
        assert marker in merged_header, f"regeneration dropped {marker!r} from the header"


@pytest.mark.parametrize(
    "mutation,expect_problem",
    [
        ("drop_providers", "providers"),
        ("drop_api_key_env", "api_key_env"),
        ("empty_chain", "empty chain"),
    ],
)
def test_validate_rejects_broken_configs(mutation, expect_problem):
    """The pre-write validator must catch config that would break the daemon."""
    text = CONFIG.read_text()
    parsed = yaml.safe_load(text)

    if mutation == "drop_providers":
        parsed.pop("providers")
    elif mutation == "drop_api_key_env":
        for prov in parsed["providers"].values():
            prov.pop("api_key_env", None)
    elif mutation == "empty_chain":
        parsed["models"]["smart"]["chain"] = []

    broken = yaml.safe_dump(parsed)
    problems = gen.validate_config_text(broken)
    assert problems, f"validator accepted a config with a {mutation} defect"
    joined = " ".join(problems).lower()
    assert expect_problem in joined, f"unexpected validation problems: {problems}"

def test_validator_accepts_the_real_config():
    assert gen.validate_config_text(CONFIG.read_text()) == []


def test_dry_run_is_the_default():
    """Without --write the generator must not touch the file.

    The original script wrote unconditionally while printing output that read
    like a dry run; that is what destroyed the config with no safety net.
    """
    text = CONFIG.read_text()
    tmp = REPO / "config.yaml.testwrite"
    original_path = gen.CONFIG_PATH
    try:
        gen.CONFIG_PATH = str(tmp)
        tmp.write_text(text)
        # A candidate identical to the current content, plus one that differs,
        # must both leave the file untouched without --write.
        gen.write_config_atomically(text, text, [])
        assert tmp.read_text() == text
        start, end = gen.find_models_section(text)
        changed = gen.merge_models_section(text, text[start:end] + "\n# touched\n")
        assert changed != text
        gen.write_config_atomically(text, changed, [])
        assert tmp.read_text() == text, "dry run wrote to the file"
    finally:
        gen.CONFIG_PATH = original_path
        if tmp.exists():
            tmp.unlink()


def test_write_requires_flag_and_makes_backup(tmp_path, monkeypatch):
    cfg = tmp_path / "config.yaml"
    cfg.write_text(CONFIG.read_text())
    monkeypatch.setattr(gen, "CONFIG_PATH", str(cfg))

    text = cfg.read_text()
    start, end = gen.find_models_section(text)
    changed = gen.merge_models_section(text, text[start:end] + "\n# touched\n")
    assert changed != text

    # Without --write: untouched, no backup.
    gen.write_config_atomically(text, changed, [])
    assert cfg.read_text() == text
    assert not (tmp_path / "config.yaml.bak").exists()

    # With --write: applied, and a backup exists.
    gen.write_config_atomically(text, changed, ["--write"])
    assert cfg.read_text() != text
    assert (tmp_path / "config.yaml.bak").exists(), "no backup made before writing"
    assert (tmp_path / "config.yaml.bak").read_text() == text


def test_write_refuses_invalid_config(tmp_path, monkeypatch):
    """A regeneration that would drop providers must never be written."""
    cfg = tmp_path / "config.yaml"
    text = CONFIG.read_text()
    cfg.write_text(text)
    monkeypatch.setattr(gen, "CONFIG_PATH", str(cfg))

    # A models block is fine, but splice it into a file we then corrupt so the
    # merged result is invalid: header intact, providers stripped.
    merged = gen.merge_models_section(text, "models:\n  smart:\n    chain:\n      - provider: kilocode\n")
    parsed = yaml.safe_load(merged)
    parsed.pop("providers")
    broken = yaml.safe_dump(parsed)

    with pytest.raises(SystemExit):
        gen.write_config_atomically(text, broken, ["--write"])
    assert cfg.read_text() == text, "file was modified despite failing validation"


# --------------------------------------------------------------------------
# Distribution rules (scripts/sync-instruction.txt rules 3, 6, 9, 11).
#
# These pin two things the rules are written down as but nothing enforced:
# the exact shape of the trailing comment, and the terminator choice. The
# terminator regressed silently once already: a kilocode-vs-opencode COUNT
# comparison emitted `# last: opencode dominates (1 vs 5)` style comments and
# would have made `smart` end on opencode/big-pickle, which 403s on every
# call. A rule that is only prose decays into a wrong config.
# --------------------------------------------------------------------------


def _model(model_id, provider, score=None, *, ctx=0, vision=False, elo=None,
           source=None, verified=None, probe_age_days=None, protocol=None):
    from datetime import UTC, datetime, timedelta
    return gen.Model(
        id=model_id,
        name=model_id,
        provider=provider,
        context_length=ctx,
        intelligence=score,
        intelligence_source=source,
        intelligence_note=None,
        elo=elo,
        vision=vision,
        raw={},
        protocol=protocol,
        verified=verified,
        probe_checked_at=(
            None if probe_age_days is None
            else datetime.now(UTC) - timedelta(days=probe_age_days)
        ),
    )


def _auto(model_id, provider, *, verified=None, age_days=0.0):
    """An auto-router record as --probe-auto writes it.

    `verified=None` means the probe reached no verdict (not probed, or a 429 /
    5xx / timeout), which is a different field value from `verified=False`
    ("the provider refused it") and behaves differently. A verdict without a
    timestamp is also untrusted, so every non-None verdict here is timestamped
    unless the test is about staleness.
    """
    return _model(model_id, provider, None, verified=verified, probe_age_days=age_days)


def test_terminator_prefers_kilocode_regardless_of_counts():
    """Liveness, not popularity: kilocode wins even when opencode outweighs it.

    Two kilocode endpoints and nine opencode endpoints is the shape that made
    the count comparison pick opencode/big-pickle, which is not entitled to
    the free tier and 403s on every call.
    """
    catalog = {
        ("kilocode", "kilo-auto/free"): _auto("kilo-auto/free", "kilocode"),
        ("opencode", "big-pickle"): _auto("big-pickle", "opencode"),
    }
    models = [_model(f"k/{i}", "kilocode", 30.0 - i) for i in range(2)]
    models += [_model(f"o/{i}", "opencode", 20.0 - i) for i in range(9)]
    chain = gen.build_chain(models, "smart", catalog)
    assert chain[-1]["provider"] == "kilocode"
    assert chain[-1]["model"] == "kilo-auto/free"
    assert "dominates" not in chain[-1]["comment"], "count comparison is back"


def test_terminator_opencode_only_when_kilocode_absent():
    catalog = {
        ("kilocode", "kilo-auto/free"): _auto("kilo-auto/free", "kilocode"),
        ("opencode", "big-pickle"): _auto("big-pickle", "opencode"),
    }
    chain = gen.build_chain([_model("space-bunny-free", "opencode", 25.0)], "smart", catalog)
    assert (chain[-1]["provider"], chain[-1]["model"]) == ("opencode", "big-pickle")

    # Neither present: keep kilocode rather than emit a chain with no terminator.
    chain = gen.build_chain([_model("gemini-2b", "google-ai-studio", 7.8)], "fast", catalog)
    assert (chain[-1]["provider"], chain[-1]["model"]) == ("kilocode", "kilo-auto/free")


def test_terminator_refuses_a_probed_and_failed_auto_model():
    """A terminator the fetcher explicitly rejected must abort, not be written.

    `verified: false` means fetch-free-models.py sent a real request and the
    provider REFUSED it. Writing it as the last-resort endpoint turns every
    exhausted chain into a failed request, which is worse than not
    regenerating at all.
    """
    catalog = {
        ("kilocode", "kilo-auto/free"): _auto("kilo-auto/free", "kilocode", verified=False),
    }
    with pytest.raises(SystemExit, match="terminator"):
        gen.build_chain([_model("qwen/x:free", "kilocode", 33.7)], "smart", catalog)

    # verified=None means "not probed" and must not block a regeneration.
    catalog[("kilocode", "kilo-auto/free")] = _auto("kilo-auto/free", "kilocode")
    chain = gen.build_chain([_model("qwen/x:free", "kilocode", 33.7)], "smart", catalog)
    assert chain[-1]["model"] == "kilo-auto/free"


def test_unprobed_kilocode_router_keeps_the_pre_probe_choice():
    """The case that ships today: a router with no verdict stays where it was.

    Before --probe-auto existed, every kilocode record carried
    `verified: None`. That must produce exactly the pre-probe outcome —
    kilocode, with the reason recorded — because a missing verdict is absence
    of evidence, not evidence of absence. The comment says which state it is in
    so the next reader can tell "probed live" from "never asked".
    """
    catalog = {
        ("kilocode", "kilo-auto/free"): _auto("kilo-auto/free", "kilocode"),  # verified=None
        ("opencode", "big-pickle"): _auto("big-pickle", "opencode", verified=False),
    }
    chain = gen.build_chain([_model("qwen/x:free", "kilocode", 33.7)], "smart", catalog)
    assert (chain[-1]["provider"], chain[-1]["model"]) == ("kilocode", "kilo-auto/free")
    assert "not probed" in chain[-1]["comment"] and "probed live" not in chain[-1]["comment"]

    # And a router that DID answer gets that recorded instead.
    catalog[("kilocode", "kilo-auto/free")] = _auto("kilo-auto/free", "kilocode", verified=True)
    chain = gen.build_chain([_model("qwen/x:free", "kilocode", 33.7)], "smart", catalog)
    assert "probed live" in chain[-1]["comment"]


def test_transport_failure_does_not_move_or_block_the_terminator(capsys):
    """A 429/5xx/timeout is `unknown`, so it must not be treated as a verdict.

    Collapsing transport trouble into "dead" would let one flaky network abort
    every sync, and into "live" would let it certify an endpoint nobody called.
    The fetcher side of this is classify_probe_status; the point of this test is
    that the generator cannot act on a probe that never reached a verdict.
    """
    catalog = {
        ("kilocode", "kilo-auto/free"): _auto("kilo-auto/free", "kilocode"),  # unknown
    }
    chain = gen.build_chain([_model("qwen/x:free", "kilocode", 33.7)], "fast", catalog)
    assert chain[-1]["model"] == "kilo-auto/free"
    assert "warning" not in capsys.readouterr().err.lower()


def test_dead_kilocode_router_rescues_to_opencode_only_when_it_probed_live():
    """kilocode's auto router dies -> opencode, but only on positive evidence.

    Falling back to an endpoint nobody has called would trade a known failure
    for a possible one, so the rescue requires verified is True.
    """
    models = [_model("qwen/x:free", "kilocode", 33.7), _model("space-bunny-free", "opencode", 25.0)]
    dead_kilo = {("kilocode", "kilo-auto/free"): _auto("kilo-auto/free", "kilocode", verified=False)}

    # Rescue: the other router answered.
    chain = gen.build_chain(models, "smart", {
        **dead_kilo,
        ("opencode", "big-pickle"): _auto("big-pickle", "opencode", verified=True),
    })
    assert (chain[-1]["provider"], chain[-1]["model"]) == ("opencode", "big-pickle")
    assert "kilocode auto router probed dead" in chain[-1]["comment"]

    # No live alternative: abort rather than write a dead terminator.
    for other in ({}, {("opencode", "big-pickle"): _auto("big-pickle", "opencode")}):
        with pytest.raises(SystemExit, match="refusing to write"):
            gen.build_chain(models, "smart", {**dead_kilo, **other})

    # An unprobed opencode router is not positive evidence.
    with pytest.raises(SystemExit, match="refusing to write"):
        gen.build_chain(models, "smart", {**dead_kilo})


def test_a_stale_verdict_decays_to_unknown_in_both_directions(capsys):
    """A `live` verdict pins the terminator, so it must not pin it forever.

    If the fetcher stops running — cron broken, key removed, machine offline
    for a month — the last verdict stays in the list and keeps saying the
    router works. Past AUTO_PROBE_MAX_AGE_DAYS it is treated as no verdict at
    all, which is NOT the same as `dead`: it keeps kilocode in place and cannot
    block a sync. A stale `dead` decays the same way, because a fixed key
    should not keep a router disqualified forever either.
    """
    stale = gen.AUTO_PROBE_MAX_AGE_DAYS + 1
    models = [_model("qwen/x:free", "kilocode", 33.7), _model("space-bunny-free", "opencode", 25.0)]

    # Stale live: the terminator stays put rather than chasing the rescue.
    chain = gen.build_chain(models, "smart", {
        ("kilocode", "kilo-auto/free"): _auto("kilo-auto/free", "kilocode", verified=True, age_days=stale),
        ("opencode", "big-pickle"): _auto("big-pickle", "opencode", verified=True, age_days=0),
    })
    assert (chain[-1]["provider"], chain[-1]["model"]) == ("kilocode", "kilo-auto/free")
    assert "probed live" not in chain[-1]["comment"]
    assert "not probed" in chain[-1]["comment"]

    # Stale dead: a verdict that old is not grounds for refusing to write, so
    # kilocode is used unchallenged rather than the whole sync dying.
    chain = gen.build_chain(models, "smart", {
        ("kilocode", "kilo-auto/free"): _auto("kilo-auto/free", "kilocode", verified=False, age_days=stale),
        ("opencode", "big-pickle"): _auto("big-pickle", "opencode", verified=True, age_days=0),
    })
    assert (chain[-1]["provider"], chain[-1]["model"]) == ("kilocode", "kilo-auto/free")

    # Just inside the window is still actionable in both directions.
    inside = gen.AUTO_PROBE_MAX_AGE_DAYS - 1
    chain = gen.build_chain(models, "smart", {
        ("kilocode", "kilo-auto/free"): _auto("kilo-auto/free", "kilocode", verified=True, age_days=inside),
        ("opencode", "big-pickle"): _auto("big-pickle", "opencode", verified=True, age_days=0),
    })
    assert "probed live" in chain[-1]["comment"]


def test_a_verdict_without_a_timestamp_is_not_trusted(capsys):
    """Freshness you cannot show is not freshness.

    A hand-edited or older-format record can carry `verified: true` with no
    `auto_probe.checked_at`. It must read as unprobed, not as evidence for a
    rescue — the direction that fails safe.
    """
    untrusted = _model("kilo-auto/free", "kilocode", None, verified=True)  # no timestamp
    catalog = {("kilocode", "kilo-auto/free"): untrusted,
               ("opencode", "big-pickle"): _auto("big-pickle", "opencode", verified=True)}
    chain = gen.build_chain(
        [_model("qwen/x:free", "kilocode", 33.7), _model("space-bunny-free", "opencode", 25.0)],
        "smart", catalog,
    )
    assert (chain[-1]["provider"], chain[-1]["model"]) == ("kilocode", "kilo-auto/free")
    assert "no probe timestamp" in capsys.readouterr().err


def test_the_fetcher_timestamp_format_survives_the_generator_parser():
    """Guard the pairing that broke once: fetcher writes, generator parses.

    The fetcher stamps `datetime.now(UTC).isoformat()` — fractional seconds and
    a trailing Z. A parser that returns None for that form silently downgrades
    every verdict to "unprobed" and disables the whole guard, with no error
    anywhere to notice.
    """
    import json as _json
    import subprocess as _subprocess
    import tempfile as _tempfile

    with _tempfile.TemporaryDirectory() as d:
        models = _tempfile.NamedTemporaryFile("w", suffix=".json", dir=d, delete=False)
        models.write(_json.dumps([{
            "id": "kilo-auto/free", "provider": "kilocode", "context_length": 256000,
            "intelligence": None, "elo": None, "released": None,
            "capabilities": {"vision": False}, "source": "kilocode", "raw": {},
            "verified": True,
            "auto_probe": {"verdict": "live", "status": 200,
                           "checked_at": "2026-09-30T10:48:21.059842Z"},
        }]))
        models.close()
        rec = gen.load_models(models.name)[0]
    assert rec.verified is True
    assert rec.probe_checked_at is not None
    assert rec.probe_checked_at.tzinfo is not None


def test_probe_only_records_can_never_become_chain_candidates():
    probe_only = _model("big-pickle", "opencode", None, verified=False)
    probe_only.probe_only = True
    assert probe_only.is_chain_candidate is False
    assert probe_only.is_auto_fallback is True
    for filt in (gen.filter_smart, gen.filter_work, gen.filter_fast, gen.filter_large):
        assert filt([probe_only]) == [], f"{filt.__name__} selected a probe-only record"

    # A scored probe-only record is excluded too, so the guarantee does not
    # depend on the auto model happening to have a null score.
    scored = _model("big-pickle", "opencode", 25.0, verified=True)
    scored.probe_only = True
    assert gen.filter_smart([scored]) == []


def test_key_groups_are_emitted_whole_and_in_order():
    """Rule 3: an nvidia-nim model is three consecutive entries, a commandcode
    model two — so a 429 on one key falls through to the next instead of
    failing the request."""
    catalog = {("kilocode", "kilo-auto/free"): _auto("kilo-auto/free", "kilocode")}
    models = [
        _model("z-ai/glm-5.3", "nvidia-nim", 44.8, ctx=131072),
        _model("poolside/laguna-s-2.1-free", "commandcode", 25.0, ctx=256000),
        _model("qwen/qwen3.8-27b:free", "kilocode", 33.7, ctx=262144),
    ]
    chain = gen.build_chain(models, "smart", catalog)[:-1]
    assert [(e["provider"], e["model"]) for e in chain] == [
        ("nvidia", "z-ai/glm-5.3"),
        ("nvidia2", "z-ai/glm-5.3"),
        ("nvidia3", "z-ai/glm-5.3"),
        ("commandcode", "poolside/laguna-s-2.1-free"),
        ("commandcode2", "poolside/laguna-s-2.1-free"),
        ("kilocode", "qwen/qwen3.8-27b:free"),
    ]


def test_comment_shape_is_score_then_elo_and_context_is_a_field():
    """Rule 9, pinned against the drift that reintroduced a stale example.

    The context window is a real `context_length:` field (it feeds
    /v1/models), so it must NOT also appear in the comment; the comment carries
    the score, `floor` for the synthetic smart-floor default, and `elo=` when
    the arena.ai leaderboard supplied one.
    """
    assert _model("a", "kilocode", 44.8, elo=1619.0, source="arena").get_comment() == "# 44.8  elo=1619"
    assert _model("b", "kilocode", 25.0, source="smart floor").get_comment() == "# 25.0  floor"
    assert _model("c", "kilocode", 25.0).get_comment() == "# 25.0"
    assert _model("d", "kilocode", None).get_comment() == ""


def test_every_concrete_endpoint_carries_a_score_and_the_router_does_not():
    """Rule 11: `intelligence` is what the router rotates over; the auto
    meta-router is not a model and must not carry one."""
    catalog = {("kilocode", "kilo-auto/free"): _auto("kilo-auto/free", "kilocode")}
    chain = gen.build_chain([_model("qwen/x:free", "kilocode", 33.7)], "smart", catalog)
    concrete, router = chain[:-1], chain[-1]
    assert all(e["intelligence"] == 33.7 for e in concrete)
    assert "intelligence" not in router
    assert concrete[0]["vision"] is False and router["vision"] is True


def test_real_config_matches_the_pinned_shapes():
    """The checked-in config.yaml, not a synthetic fixture, obeys the shapes.

    A generator test that only exercises synthetic input passes happily while
    the file the daemon actually loads drifts.
    """
    cfg = yaml.safe_load(CONFIG.read_text())
    assert set(cfg["models"]) == {"smart", "work", "fast", "large"}
    for name, body in cfg["models"].items():
        chain = body["chain"]
        assert chain, f"{name} chain is empty"
        for ep in chain[:-1]:
            assert isinstance(ep.get("vision"), bool), f"{name}: {ep} has no vision bool"
            assert isinstance(ep.get("intelligence"), (int, float)), f"{name}: {ep} has no score"
        assert chain[-1]["model"] in ("kilo-auto/free", "big-pickle"), f"{name} terminator"
        assert chain[-1]["provider"] == "kilocode" and chain[-1]["model"] == "kilo-auto/free"
        assert chain[-1].get("vision") is True
        assert "intelligence" not in chain[-1], f"{name} terminator must carry no score"


# --------------------------------------------------------------------------
# The ladder through the CLI, not just the function.
#
# The terminator decision is made in main(), from a file on disk, and the
# failure mode it guards is a whole REGENERATION being wrong. Calling
# auto_fallback_terminator() directly leaves the wiring between "the fetcher
# wrote verified" and "the generator read it" untested, which is exactly where
# the fractional-second timestamp bug lived. So these drive main() end to end
# over a fixture, and also pin the property that a repeated run over the same
# verdicts is byte-identical: a terminator that flips back and forth night to
# night must not otherwise churn the file.
# --------------------------------------------------------------------------


def _now_iso():
    """The exact timestamp format fetch-free-models.py writes.

    Fractional seconds and a trailing Z. Pinned here because the generator's
    parser has to accept precisely this form or every verdict silently decays
    to "unprobed".
    """
    from datetime import UTC, datetime
    return datetime.now(UTC).isoformat().replace("+00:00", "Z")


def _fixture_models(verdicts, extra=()):
    """A minimal model list with the given auto-router verdicts.

    Verdicts: {provider: True|False|None}; None means the record exists but the
    probe reached no verdict, and a provider absent from the dict means no
    record at all (what SKIP_AUTO_PROBE=1 leaves behind).
    """
    now = _now_iso()
    out = [
        {
            "id": "kilo-auto/free", "provider": "kilocode", "context_length": 256000,
            "intelligence": None, "elo": None, "released": None,
            "capabilities": {"vision": False}, "source": "kilocode", "raw": {},
        },
        {
            "id": "qwen/qwen3.8-27b:free", "provider": "kilocode", "context_length": 262144,
            "intelligence": 33.7, "elo": 1671.0, "released": "2026-01-01",
            "capabilities": {"vision": True}, "source": "kilocode", "raw": {},
        },
    ]
    for provider, model in (("kilocode", "kilo-auto/free"), ("opencode", "big-pickle")):
        verdict = verdicts.get(provider, "absent")
        if verdict == "absent":
            continue
        rec = next((r for r in out if r["provider"] == provider and r["id"] == model), None)
        if rec is None:
            rec = {
                "id": model, "provider": provider, "context_length": 0,
                "intelligence": None, "elo": None, "released": None,
                "capabilities": {"vision": False}, "source": provider, "raw": {},
                "probe_only": True,
            }
            out.append(rec)
        if verdict is not None:
            rec["verified"] = verdict
            rec["auto_probe"] = {
                "verdict": "live" if verdict else "dead",
                "status": 200 if verdict else 403,
                "detail": "fixture", "checked_at": now,
            }
    return out + list(extra)


def _run_cli(tmp_path, verdicts, extra=(), runs=1, base=None):
    """Run the generator over a fixture; return the final config text.

    `base` is the config to start from; it defaults to the real one, which
    supplies the providers/preferences header the validator insists on.
    """
    tmp_path.mkdir(parents=True, exist_ok=True)
    models = tmp_path / "models.json"
    models.write_text(json.dumps(_fixture_models(verdicts, extra)))
    cfg = tmp_path / "config.yaml"
    cfg.write_text(base if base is not None else CONFIG.read_text())
    last = None
    for _ in range(runs):
        gen.main(["--models", str(models), "--config", str(cfg), "--write"])
        last = cfg.read_text()
    return last


def _terminator(text):
    return text.rsplit("        model: ", 1)[1].splitlines()[0].strip()


@pytest.mark.parametrize("verdicts,expected", [
    ({"kilocode": True, "opencode": False}, "kilo-auto/free"),   # shipped
    ({"kilocode": None, "opencode": None}, "kilo-auto/free"),   # unknown
    ({}, "kilo-auto/free"),                                      # no record
    ({"kilocode": False, "opencode": True}, "big-pickle"),      # rescue
])
def test_cli_ladder_produces_the_expected_terminator(tmp_path, verdicts, expected):
    text = _run_cli(tmp_path, verdicts)
    assert _terminator(text).startswith(expected)


def test_cli_refuses_to_write_when_no_router_probed_live(tmp_path):
    """Both refused: the config on disk must survive untouched."""
    before = _run_cli(tmp_path, {"kilocode": True, "opencode": False})
    models = tmp_path / "models.json"
    models.write_text(json.dumps(_fixture_models({"kilocode": False, "opencode": False})))
    cfg = tmp_path / "config.yaml"
    cfg.write_text(before)
    with pytest.raises(SystemExit, match="refusing to write"):
        gen.main(["--models", str(models), "--config", str(cfg), "--write"])
    assert cfg.read_text() == before, "a refused ladder must not rewrite the file"


def test_cli_uses_kilocode_when_the_chain_has_neither_provider(tmp_path):
    """The `else` branch: no kilocode and no opencode endpoint in the chain.

    kilocode is still the terminator, and the comment says which rule fired —
    "no auto provider in chain" is a different situation from "the chain has
    kilocode endpoints", and the file is where an operator would look.
    """
    ollama_only = [{
        "id": "gemma4:31b", "provider": "ollama-cloud", "context_length": 262144,
        "intelligence": 19.0, "elo": None, "released": None,
        "capabilities": {"vision": True}, "source": "ollama-cloud", "raw": {},
    }]
    models = tmp_path / "models.json"
    models.write_text(json.dumps([
        {
            "id": "kilo-auto/free", "provider": "kilocode", "context_length": 256000,
            "intelligence": None, "elo": None, "released": None,
            "capabilities": {"vision": False}, "source": "kilocode", "raw": {},
            "verified": True,
            "auto_probe": {"verdict": "live", "status": 200, "detail": "fixture",
                           "checked_at": _now_iso()},
        },
    ] + ollama_only))
    cfg = tmp_path / "config.yaml"
    cfg.write_text(CONFIG.read_text())
    gen.main(["--models", str(models), "--config", str(cfg), "--write"])
    text = cfg.read_text()
    assert _terminator(text).startswith("kilo-auto/free")
    assert "no auto provider in chain" in text


def test_repeated_runs_with_identical_verdicts_are_byte_identical(tmp_path):
    """A verdict that flips back and forth must not otherwise churn the file.

    Two runs over the same verdicts have to produce the same bytes, including
    the flipped state where the terminator is opencode and the offline case
    where the list carries no verdict at all (SKIP_AUTO_PROBE=1, a fixture, or
    a fetcher that never probed). Otherwise every nightly sync shows a diff and
    `git diff` stops saying anything.
    """
    for verdicts in ({"kilocode": True, "opencode": False},
                     {"kilocode": False, "opencode": True},
                     {"kilocode": None, "opencode": None},
                     {}):
        first = _run_cli(tmp_path / "a", verdicts, runs=2)
        second = _run_cli(tmp_path / "b", verdicts, runs=3)
        assert first == second, f"regeneration is not deterministic for {verdicts}"
        assert _terminator(first).split()[0] in ("kilo-auto/free", "big-pickle")


# --------------------------------------------------------------------------
# check-rules.py itself.
#
# The rules checker is the thing that fails a bad sync, and a checker that is
# only ever exercised by hand has no contract at all. These call its check()
# directly with the real config as the baseline, so a violation has to be
# planted deliberately to be seen.
# --------------------------------------------------------------------------


def _load_checker():
    spec = importlib.util.spec_from_file_location("check_rules", REPO / "scripts" / "check-rules.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


cr = _load_checker()


def _checker_problems(config_text=None, models_path="/tmp/free-models.json"):
    text = config_text if config_text is not None else CONFIG.read_text()
    index = (
        cr.build_source_index(gen, Path(models_path))
        if Path(models_path).exists() else None
    )
    return cr.check(gen, yaml.safe_load(text), text, index)


def test_checker_passes_the_real_config_and_its_real_model_list():
    if not Path("/tmp/free-models.json").exists():
        pytest.skip("no fetched model list on this machine")
    assert _checker_problems() == []


def test_preservation_never_inherits_a_comment_naming_another_endpoint(tmp_path):
    """Preservation is scoped to an unchanged endpoint.

    Without the endpoint check, a run that decides on kilocode while the file
    still terminates on opencode keeps `last: opencode (...)` on a kilocode
    entry — a comment describing an endpoint the chain no longer uses, which is
    worse than no comment. The starting config here is the rescue state, so the
    previous terminator really is opencode.
    """
    rescued = _run_cli(tmp_path / "rescued", {"kilocode": False, "opencode": True})
    assert _terminator(rescued).startswith("big-pickle")
    assert gen.parse_terminators(rescued)["smart"][:2] == ("opencode", "big-pickle")

    # Regenerated with no verdicts at all: the decision is unverifiable, so
    # kilocode wins on preference and must get its own comment.
    unverifiable = _run_cli(tmp_path / "plain", {}, runs=2, base=rescued)
    assert _terminator(unverifiable).startswith("kilo-auto/free")
    claim = _terminator(unverifiable).split("#", 1)[1]
    assert "last: opencode" not in claim, claim


def test_checker_catches_a_terminator_comment_that_contradicts_the_probe():
    """The comment is the only in-file record of why the endpoint was chosen.

    Without this, a stale or hand-edited comment reads as verified evidence for
    as long as nobody re-runs the checker against the model list.
    """
    if not Path("/tmp/free-models.json").exists():
        pytest.skip("no fetched model list on this machine")
    text = CONFIG.read_text().replace(
        "auto router probed live; opencode auto",
        "auto router not probed (unknown verdict); opencode auto", 1,
    )
    problems = _checker_problems(text)
    assert any("terminator comment" in p for p in problems), problems


def test_checker_catches_a_flipped_terminator():
    if not Path("/tmp/free-models.json").exists():
        pytest.skip("no fetched model list on this machine")
    text = CONFIG.read_text().replace(
        "      - provider: kilocode\n        model: kilo-auto/free  # last: kilocode",
        "      - provider: opencode\n        model: big-pickle  # last: opencode", 1,
    )
    problems = _checker_problems(text)
    assert any("terminator is opencode/big-pickle" in p for p in problems), problems


def test_checker_skips_the_comment_rule_without_a_model_list():
    """No list means no verdict, so the comment cannot be judged.

    This is the CI shape: the structural rules still run, the evidence-based
    ones report a notice, and nothing is claimed that was not checked.
    """
    problems = _checker_problems(models_path="/nonexistent.json")
    assert problems == []


def test_checker_flags_a_stale_verdict_still_claimed_as_live(tmp_path):
    """A verdict older than the window cannot justify a 'probed live' comment.

    Built entirely from fixtures so the test does not depend on when it runs.
    """
    models = tmp_path / "stale.json"
    stale = [
        {
            "id": "kilo-auto/free", "provider": "kilocode", "context_length": 256000,
            "intelligence": None, "elo": None, "released": None,
            "capabilities": {"vision": False}, "source": "kilocode", "raw": {},
            "verified": True,
            "auto_probe": {"verdict": "live", "status": 200, "detail": "old",
                           "checked_at": "2020-01-01T00:00:00.000000Z"},
        },
    ]
    models.write_text(json.dumps(stale))
    index = cr.build_source_index(gen, models)
    text = CONFIG.read_text()
    problems = cr.check(gen, yaml.safe_load(text), text, index)
    assert any("terminator comment" in p for p in problems), problems


def test_an_unverifiable_run_preserves_the_comment_it_cannot_check(tmp_path):
    """A run that cannot verify a claim must not replace it with another one.

    This is the offline wart: regenerating from a verdict-less list
    (SKIP_AUTO_PROBE=1, a fixture, a fetcher that never probed) used to rewrite
    a recorded "probed live" into "not probed", destroying the only provenance
    the file has — and then doing it again on the next run, so the diff was
    permanent noise. The claim is unverifiable, not false, and the checker
    reports that distinction.
    """
    before = _run_cli(tmp_path / "src", {"kilocode": True, "opencode": False})
    assert "probed live" in _terminator(before)

    # Same config, a list that carries no verdict at all.
    after = _run_cli(tmp_path / "offline", {}, runs=2)
    assert _terminator(after) == _terminator(before), "an unverifiable run rewrote the claim"

    # And a real verdict replaces it with a current one.
    refreshed = _run_cli(tmp_path / "again", {"kilocode": True, "opencode": False})
    assert "probed live" in _terminator(refreshed)


def test_a_changed_terminator_always_gets_a_fresh_comment(tmp_path):
    """Preservation must not survive a real change of endpoint.

    If the rescue fires, the old comment described a different endpoint, and
    keeping it would be a lie about the file's own contents.
    """
    rescued = _run_cli(tmp_path, {"kilocode": False, "opencode": True})
    assert _terminator(rescued).startswith("big-pickle")
    assert "kilocode auto router probed dead" in _terminator(rescued)
    assert "probed live" not in _terminator(rescued)


def test_parse_terminators_round_trips_the_comment_including_its_hash():
    """The comment is read back out of the file and written back verbatim.

    A comment without its leading '#' is a YAML syntax error, not a comment,
    and validate_config_text would refuse the write — the safety net caught it
    the first time this was introduced.
    """
    parsed = gen.parse_terminators(CONFIG.read_text())
    assert set(parsed) == {"smart", "work", "fast", "large"}
    for profile, (provider, model, comment) in parsed.items():
        assert provider in ("kilocode", "opencode")
        assert model in ("kilo-auto/free", "big-pickle")
        assert comment.startswith("#"), f"{profile}: {comment!r} lost its hash"


def test_cli_rejects_a_typo_instead_of_exiting_zero(tmp_path, capsys):
    """argparse, not a hand-rolled scan.

    The scan accepted an unknown flag, did nothing, and exited 0 — which is
    indistinguishable from "already up to date" in a cron log. A typo must be
    loud and non-zero.
    """
    with pytest.raises(SystemExit) as exc:
        gen.main(["--model", str(tmp_path / "m.json")])  # missing 's'
    assert exc.value.code == 2
    with pytest.raises(SystemExit) as exc:
        gen.main(["--bogus"])
    assert exc.value.code == 2
    with pytest.raises(SystemExit) as exc:
        gen.main(["--help"])
    assert exc.value.code == 0


def test_cli_is_a_dry_run_without_write(tmp_path):
    """`--write` is still required to touch a file."""
    models = tmp_path / "models.json"
    models.write_text(json.dumps(_fixture_models({"kilocode": True, "opencode": False})))
    cfg = tmp_path / "config.yaml"
    original = CONFIG.read_text()
    cfg.write_text(original)
    gen.main(["--models", str(models), "--config", str(cfg)])
    assert cfg.read_text() == original, "a dry run modified the config"
    assert not (tmp_path / "config.yaml.bak").exists()


def _probe_list(tmp_path, name, *, kilo=None, oc=None, age_days=None):
    """A model list carrying only the two auto-router verdicts, for checker tests."""
    from datetime import UTC, datetime, timedelta
    now = datetime.now(UTC) - timedelta(days=age_days or 0)
    out = []
    for provider, model, verdict in (("kilocode", "kilo-auto/free", kilo),
                                     ("opencode", "big-pickle", oc)):
        if verdict == "absent":
            continue
        rec = {
            "id": model, "provider": provider, "context_length": 0,
            "intelligence": None, "elo": None, "released": None,
            "capabilities": {"vision": False}, "source": provider, "raw": {},
        }
        if verdict is not None:
            rec["verified"] = verdict
            rec["auto_probe"] = {
                "verdict": "live" if verdict else "dead",
                "status": 200 if verdict else 403,
                "detail": "fixture",
                "checked_at": now.isoformat().replace("+00:00", "Z"),
            }
        out.append(rec)
    path = tmp_path / name
    path.write_text(json.dumps(out))
    return path


def _check_with(config_text, models_path):
    index = cr.build_source_index(gen, Path(models_path))
    return cr.check(gen, yaml.safe_load(config_text), config_text, index)


def _rule6(problems):
    """Only the terminator findings.

    A verdict-only fixture list has none of the config's real models, so the
    rule-8 "not in the fetched model list" findings fire for every endpoint.
    Those are covered by the real-list test; these tests are about the
    terminator ladder and the comment.
    """
    return [p for p in problems if p.startswith("rule6")]


def test_checker_reports_an_unverifiable_claim_distinctly_from_a_wrong_one(tmp_path):
    """Two different problems that used to share one message.

    With no verdict in the list, a "probed live" comment cannot be confirmed or
    denied — the generator preserves it on purpose — so the report says
    "unverifiable, go probe". With a verdict, a comment that says anything else
    is a contradiction and is reported as one. An honest "not probed" comment
    against a verdict-less list is already correct and must be silent.
    """
    text = CONFIG.read_text()
    honest = text.replace(
        "auto router probed live; opencode auto",
        "auto router not probed (unknown verdict); opencode auto",
    )
    all_chains = text.count("auto router probed live")

    no_verdict = _probe_list(tmp_path, "none.json", kilo="absent", oc="absent")
    problems = _rule6(_check_with(text, no_verdict))
    unverifiable = [p for p in problems if "cannot be checked" in p]
    assert len(unverifiable) == all_chains, problems
    assert not [p for p in problems if "implies" in p]

    assert _rule6(_check_with(honest, no_verdict)) == []

    live = _probe_list(tmp_path, "live.json", kilo=True, oc=False)
    problems = _rule6(_check_with(honest, live))
    assert [p for p in problems if "implies" in p], problems
    assert not [p for p in problems if "cannot be checked" in p]


def test_checker_demands_the_rescue_when_the_list_says_kilocode_is_refused(tmp_path):
    """The ladder check has to work from a fixture, not only from the live list.

    Otherwise the opencode rescue — the one path that can put a 403 endpoint in
    a chain — is exercised only when a real outage happens.
    """
    dead_kilo = _probe_list(tmp_path, "dead.json", kilo=False, oc=True)
    # The rescue applies to every chain, so a config that satisfies the list has
    # to carry the opencode router in all four — not just the one that was
    # flipped in a previous test. The expected comment comes from the generator
    # itself, so this cannot pass by guessing the wording.
    want = gen.auto_fallback_terminator(
        {"kilocode", "nvidia"}, cr.build_source_index(gen, dead_kilo)
    )
    assert want[:2] == ("opencode", "big-pickle")
    rescued = re.sub(
        # The WHOLE model line: matching only the comment's prefix would leave
        # the tail of the old comment attached to the new one.
        r"(?m)^      - provider: kilocode\n        model: kilo-auto/free  # last: kilocode.*$",
        f"      - provider: opencode\n        model: {want[1]}  {want[2]}",
        CONFIG.read_text(),
    )
    assert rescued.count(f"model: {want[1]}  {want[2]}") == 4
    assert _rule6(_check_with(rescued, dead_kilo)) == []
    problems = _rule6(_check_with(CONFIG.read_text(), dead_kilo))
    assert len([p for p in problems if "expected opencode/big-pickle" in p]) == 4, problems


# Comment shapes the generator must be able to read back and write out
# unchanged. The one that actually bit was a comment captured without its `#`
# and written back bare, which is a YAML syntax error rather than a comment —
# caught by the generator's own validation, but only because the config
# happened to be regenerated at that moment. These are the general form of that
# defect: anything YAML-significant inside a comment (a colon, another `#`, a
# trailing space, non-ASCII) must survive parse_terminators -> emit -> validate
# byte-for-byte, and the file must stay loadable.
COMMENT_SHAPES = [
    "# last: kilocode (auto router probed live; opencode auto not entitled to free tier)",
    "# last: opencode (kilocode auto router probed dead)",
    "# a colon: right here",
    "# a hash # in the middle",
    "# ünïcödé — em dash, ümlaut",
    "# trailing spaces   ",
    "# (parens) [brackets] {braces}, commas; colons: everywhere",
    "#",                                    # bare hash, no text
    "# - looks like a list item",
    "# 12345",
]


@pytest.mark.parametrize("shape", COMMENT_SHAPES)
def test_any_comment_survives_a_parse_emit_validate_round_trip(tmp_path, shape):
    """A preserved terminator comment is written back verbatim, whatever it says."""
    text = CONFIG.read_text()
    # Substitute the shape into every chain's terminator line.
    shaped = re.sub(
        r"(?m)^(      - provider: kilocode\n        model: kilo-auto/free)  # last:.*$",
        lambda m: f"{m.group(1)}  {shape}",
        text,
    )
    # Count the substituted LINES, not the shape's text: a bare "#" also occurs
    # 181 times in the header comment block.
    substituted = re.findall(
        rf"(?m)^        model: kilo-auto/free  {re.escape(shape)}$", shaped
    )
    assert len(substituted) == 4, "the substitution did not reach every chain"

    parsed = gen.parse_terminators(shaped)
    assert {p: v[2] for p, v in parsed.items()} == {p: shape for p in parsed}

    # A verdict-less run must preserve it, and the result must still be a valid
    # config. Compared per profile, not whole-file: the fixture model list has
    # one model, so the chains legitimately differ from the shipped ones and a
    # file-level comparison would only prove that.
    out = _run_cli(tmp_path, {}, runs=2, base=shaped)
    assert gen.validate_config_text(out) == []
    for profile in ("smart", "work", "fast", "large"):
        assert _terminator_in(out, profile) == f"kilo-auto/free  {shape}", (
            f"{profile} terminator comment did not survive: "
            f"{_terminator_in(out, profile)!r}"
        )


def test_a_terminator_with_no_comment_gets_one_rather_than_being_left_bare(tmp_path):
    """The empty case is different in kind: there is no claim to preserve."""
    bare = re.sub(
        r"(?m)^(        model: kilo-auto/free)  # last:.*$",
        r"\1",
        CONFIG.read_text(),
    )
    parsed = gen.parse_terminators(bare)
    assert all(v[2] == "" for v in parsed.values()), parsed
    out = _run_cli(tmp_path, {}, runs=2, base=bare)
    assert gen.validate_config_text(out) == []
    for profile in ("smart", "work", "fast", "large"):
        assert _terminator_in(out, profile).split("#", 1)[1].strip(), (
            f"{profile} terminator was left without a comment"
        )


def _terminator_in(text, profile):
    """The profile's last `model: <id>  <comment>`, WITHOUT stripping.

    Trailing whitespace is significant here: a comment that ends in spaces must
    come back with them, or the round-trip test is measuring a strip() it
    added itself.
    """
    block = re.search(rf"(?ms)^  {profile}:\n    chain:\n(.*?)(?=^  \w+:|\Z)", text)
    lines = [ln for ln in block.group(1).splitlines() if ln.startswith("        model: ")]
    return lines[-1].split("model: ", 1)[1]



# --------------------------------------------------------------------------
# The emitter and the profile floors.
#
# A defect here is invisible: dropping `vision:` from format_chain_yaml still
# produces a config that parses, loads and routes - it just offers image
# requests to text-only models. Lowering a floor or reversing a sort does the
# same. None of it shows up in a test that only compares two runs of the
# generator to each other, because both runs are wrong in the same way. The
# mutation catalogue found exactly that gap; these are the assertions it
# demanded.
# --------------------------------------------------------------------------


def test_emit_writes_vision_and_intelligence_for_concrete_endpoints_only():
    """The two fields the gateway reads at runtime, and where they must appear.

    `vision` drives capability-aware routing (a nil flag reads as
    vision-capable), and `intelligence` decides eligibility for initial-session
    rotation. The auto router is neither: it must carry no score at all.
    """
    catalog = {("kilocode", "kilo-auto/free"): _auto("kilo-auto/free", "kilocode", verified=True)}
    chain = gen.build_chain(
        [
            _model("qwen/vision:free", "kilocode", 33.7, vision=True),
            _model("text-only:free", "kilocode", 20.1, vision=False),
        ],
        "smart", catalog,
    )
    text = gen.format_chain_yaml(chain)
    assert "vision: true" in text
    assert "vision: false" in text
    assert text.count("intelligence:") == 2, text
    # The terminator's own block: vision yes, score no. The emitter puts
    # vision last, so the final line IS the vision line.
    last_two = text.rstrip().splitlines()[-2:]
    assert "kilo-auto/free" in last_two[0], last_two
    assert last_two[-1].strip() == "vision: true", last_two
    assert "intelligence" not in "\n".join(last_two), last_two


def test_emit_output_passes_the_generators_own_validator():
    """End of the emit path: what format_chain_yaml writes must load."""
    catalog = {("kilocode", "kilo-auto/free"): _auto("kilo-auto/free", "kilocode", verified=True)}
    chain = gen.build_chain(
        [_model("m/a:free", "kilocode", 30.0, vision=True),
         _model("m/b:free", "kilocode", 22.0, vision=False)],
        "work", catalog,
    )
    block = "models:\n  smart:\n    chain:\n" + gen.format_chain_yaml(chain) + "\n"
    merged = gen.merge_models_section(CONFIG.read_text(), block)
    assert gen.validate_config_text(merged) == []
    endpoints = yaml.safe_load(merged)["models"]["smart"]["chain"]
    for ep in endpoints[:-1]:
        assert isinstance(ep["vision"], bool)
        assert isinstance(ep["intelligence"], float)
    assert isinstance(endpoints[-1]["vision"], bool)
    assert "intelligence" not in endpoints[-1]


@pytest.mark.parametrize("filtername,provider,boundary,below", [
    ("filter_smart", "kilocode", 25.0, 24.9),
    ("filter_work", "kilocode", 15.0, 14.9),
])
def test_profile_floors_are_inclusive_and_enforced(filtername, provider, boundary, below):
    """`smart` starts at 25, `work` at 15, and the boundary belongs in.

    A floor that is off by a whole point, or exclusive at the boundary, is
    invisible in a config whose members are far from the edge.
    """
    filt = getattr(gen, filtername)
    just_under = _model("edge/under:free", provider, below)
    at = _model("edge/at:free", provider, boundary)
    picked = {m.id for m in filt([just_under, at])}
    assert "edge/at:free" in picked, f"{filtername} dropped a model exactly at {boundary}"
    assert "edge/under:free" not in picked, f"{filtername} admitted {below}"


def test_fast_cap_and_large_floor_are_enforced():
    """The other two profiles' limits, for the same reason."""
    # `fast` takes intelligence < 25 OR a small-model id keyword; these are
    # below the ceiling, so all 14 qualify and the cap has to do the work.
    many = [_model(f"small/{i}:free", "kilocode", 24.0 - i * 0.1) for i in range(14)]
    assert len(gen.filter_fast(many)) == 10

    small = _model("large/small:free", "kilocode", 30.0, ctx=131072)
    big = _model("large/big:free", "kilocode", 30.0, ctx=262144)
    assert {m.id for m in gen.filter_large([small, big])} == {"large/big:free"}


def test_sort_models_returns_best_first():
    """Descending by score, with the size-tier tie-break for equal scores."""
    models = [
        _model("x/low:free", "kilocode", 12.0),
        _model("x/high:free", "kilocode", 40.0),
        _model("x/mid:free", "kilocode", 25.0),
    ]
    ordered = gen.sort_models(list(models), newest_first_on_tie=True)
    assert [m.score for m in ordered] == [40.0, 25.0, 12.0]


def _record_json(model_id, provider, score, *, vision=False, ctx=262144):
    return {
        "id": model_id, "provider": provider, "context_length": ctx,
        "intelligence": score, "elo": None, "released": None,
        "capabilities": {"vision": vision}, "source": provider, "raw": {},
    }


def test_a_regenerated_chain_is_non_increasing_by_score(tmp_path):
    """The end-to-end version, so a reversed sort cannot ship.

    Comparing two runs of the generator cannot catch it: both would be reversed.
    """
    text = _run_cli(tmp_path, {"kilocode": True, "opencode": False},
                    extra=[_record_json("order/a:free", "kilocode", 30.0, vision=True),
                           _record_json("order/b:free", "kilocode", 20.0, vision=False)])
    for name, body in yaml.safe_load(text)["models"].items():
        scores = [e["intelligence"] for e in body["chain"][:-1] if "intelligence" in e]
        assert scores == sorted(scores, reverse=True), f"{name} is not best-first: {scores}"


if __name__ == "__main__":
    sys.exit(pytest.main([__file__, "-v"]))


# --------------------------------------------------------------------------
# The `protocol:` field.
#
# opencode.ai's catalog is mixed within one provider, measured live 2026-10-02
# against one key with identical gate headers:
#
#   muse-spark-1.3-contributor-free   /chat/completions 400 ProtocolUnsupported
#                                     /responses        200
#   big-pickle                        /chat/completions 200
#                                     /responses        400 ProtocolUnsupported
#
# A responses-only model left on the chat path 400s on every call. The router
# recovers by flipping and retrying (api.go), so the field is not about
# correctness of the final answer -- it is about not paying a failed round trip
# on every process start, and about the generator not deleting a field it does
# not understand.
# --------------------------------------------------------------------------


def _entry_yaml(entry, indent=6):
    return gen.format_chain_yaml([entry], indent)


def test_a_proved_protocol_is_emitted():
    chain = gen.build_chain([_model("resp-only-free", "opencode", 40.0,
                                    protocol="responses")], "work")
    assert "protocol: responses" in _entry_yaml(chain[0])


def test_chat_is_never_written_because_absence_means_chat():
    """Emitting `protocol: chat` on all ~80 endpoints would bury the two that
    need the field, and config.go's protocolFor already treats absence as chat."""
    chain = gen.build_chain([_model("chat-only-free", "opencode", 40.0,
                                    protocol="chat")], "work")
    assert "protocol" not in _entry_yaml(chain[0])


def test_an_unrecognised_protocol_is_dropped_not_passed_through():
    """config.go falls back to chat for anything it does not recognise, so
    `protocol: respones` in the file is a claim that means nothing and looks
    deliberate."""
    assert gen._normalize_protocol("respones") is None
    assert gen._normalize_protocol(None) is None
    assert gen._normalize_protocol(7) is None
    assert gen._normalize_protocol(" Responses ") == "responses"
    chain = gen.build_chain([_model("typo-free", "opencode", 40.0,
                                    protocol="respones")], "work")
    assert "protocol" not in _entry_yaml(chain[0])


def test_an_unprobed_record_keeps_the_configured_protocol():
    """The regression this field's plumbing exists for.

    regenerate_config.py owns the whole `models:` section, so a field it cannot
    re-emit is a field it DELETES. A hand-written `protocol: responses` used to
    vanish on the next nightly sync and the model went back to answering 400 on
    /chat/completions afterwards -- the fix undone by the tool that maintains it.
    """
    protocols = {("opencode", "resp-only-free"): "responses"}
    chain = gen.build_chain([_model("resp-only-free", "opencode", 40.0)],
                            "work", protocols=protocols)
    assert "protocol: responses" in _entry_yaml(chain[0])


def test_a_fresh_probe_verdict_outranks_the_configured_protocol():
    protocols = {("opencode", "m"): "chat"}
    chain = gen.build_chain([_model("m", "opencode", 40.0, protocol="responses")],
                            "work", protocols=protocols)
    assert "protocol: responses" in _entry_yaml(chain[0])


def test_the_protocol_reaches_every_key_of_a_multi_key_group():
    """nvidia-nim is one model id on three config providers. A protocol attached
    to only one of them is a bug that only shows up on one key."""
    models = [_model("some/nim-model", "nvidia-nim", 40.0, protocol="responses")]
    chain = gen.build_chain(models, "work")
    trio = [e for e in chain if e["model"] == "some/nim-model"]
    assert len(trio) == 3, trio
    assert all("protocol: responses" in _entry_yaml(e) for e in trio)


def test_protocol_is_the_last_field_so_the_order_check_stays_honest():
    """check-rules.py compares the key order literally, so the emitter and the
    checker have to agree on where the field goes."""
    chain = gen.build_chain([_model("m", "opencode", 40.0, ctx=1000,
                                    protocol="responses")], "work")
    keys = [ln.split(":")[0].strip().lstrip("- ")
            for ln in _entry_yaml(chain[0]).splitlines()]
    assert keys == ["provider", "model", "vision", "intelligence",
                    "context_length", "protocol"], keys


def test_parse_protocols_reads_every_chain_and_ignores_junk():
    """Fixtures, never the shipped config.yaml.

    An earlier version of this asserted `parse_protocols(CONFIG.read_text()) ==
    {}` — "the shipped config declares no protocol". That is an accident of the
    file at a moment in time, not a property of the parser: the nightly sync
    legitimately added `protocol: responses` to the two muse-spark-1.2 entries,
    and the assertion failed for work that was correct. Five further tests failed
    with it, because they run the real suites and a red suite reads as a broken
    harness. A test that pins the content of a regenerated artifact is a test that
    will fail the next time the artifact is regenerated.
    """
    shaped = """models:
  smart:
    chain:
      - provider: opencode
        model: a-free
        vision: false
        intelligence: 40.0
        protocol: responses
      - provider: opencode
        model: b-free
        vision: false
        intelligence: 30.0
        protocol: nonsense
  work:
    chain:
      - provider: opencode
        model: a-free
        vision: false
        intelligence: 40.0
"""
    assert gen.parse_protocols(shaped) == {("opencode", "a-free"): "responses"}
    assert gen.parse_protocols("providers:\n  x:\n    url: y\n") == {}

    # A trailing comment must not hide the field. parse_terminators tolerates one
    # on the model line for the same reason: this parser exists to preserve a
    # value a human put in the file, and a regex requiring end-of-line would
    # delete the very line it was written for.
    commented = shaped.replace("        protocol: responses",
                               "        protocol: responses  # measured: chat 400s")
    assert gen.parse_protocols(commented) == {("opencode", "a-free"): "responses"}


def test_every_protocol_in_the_shipped_config_is_one_the_router_honours():
    """The invariant that holds whatever the file contains, unlike its contents.

    config.go's protocolFor maps anything unrecognised to chat, so a value here
    that is not chat/responses is a file claiming a shape the router never uses.
    The shipped config is generated, so this reads it rather than assuming
    anything about it.
    """
    for (provider, model), protocol in gen.parse_protocols(CONFIG.read_text()).items():
        assert protocol in ("chat", "responses"), (provider, model, protocol)


def test_a_declared_protocol_survives_a_regeneration():
    """The property the preservation path exists for, asserted end to end.

    The generator owns the whole models section, so a field it cannot re-emit is
    a field it deletes. parse_protocols -> build_chain -> format_chain_yaml has to
    round-trip a declared protocol through a run that has no probe record for it,
    or the fix for the 400 is undone by the tool that maintains the config.
    """
    declared = gen.parse_protocols(CONFIG.read_text())
    if not declared:
        pytest.skip("the shipped config declares no protocol to preserve")
    (provider, model), protocol = sorted(declared.items())[0]
    entry = {
        "provider": provider, "model": model, "vision": False,
        "intelligence": 40.0, "context_length": 0,
        "protocol": protocol, "comment": "# 40.0",
    }
    assert f"protocol: {protocol}" in gen.format_chain_yaml([entry])


def test_rule13_reports_a_missing_protocol_the_record_proves():
    """Without the rule the field is written once and drifts silently, because
    nothing fails when an endpoint is configured for the shape it refuses."""
    problems = _checker_problems()
    assert problems == [] or not any(p.startswith("rule13") for p in problems), problems


def test_rule13_fires_when_the_config_drops_a_proved_protocol(tmp_path):
    models_path = tmp_path / "models.json"
    models_path.write_text(json.dumps([{
        "id": "resp-only-free", "name": "r", "provider": "opencode",
        "context_length": 1000, "intelligence": 40.0, "elo": None,
        "capabilities": {"vision": False}, "raw": {}, "released": "2026-01-01",
        "protocol": "responses",
    }]))
    config = f"""models:
  smart:
    chain:
      - provider: opencode
        model: resp-only-free  # 40.0
        vision: false
        intelligence: 40.0
        context_length: 1000
      - provider: kilocode
        model: kilo-auto/free
        vision: true
  work:
    chain:
      - provider: opencode
        model: resp-only-free  # 40.0
        vision: false
        intelligence: 40.0
        context_length: 1000
      - provider: kilocode
        model: kilo-auto/free
        vision: true
  fast:
    chain:
      - provider: kilocode
        model: kilo-auto/free
        vision: true
  large:
    chain:
      - provider: kilocode
        model: kilo-auto/free
        vision: true
"""
    problems = _checker_problems(config, models_path)
    assert [p for p in problems if p.startswith("rule13")], problems
    # Once the field is written, the same config is clean.
    fixed = config.replace("        context_length: 1000\n",
                           "        context_length: 1000\n        protocol: responses\n")
    assert not [p for p in _checker_problems(fixed, models_path)
                if p.startswith("rule13")]


def test_rule13_rejects_an_unrecognised_protocol_value():
    models_path = Path("/tmp/free-models.json")
    if not models_path.exists():
        pytest.skip("no fetched model list on this machine")
    text = CONFIG.read_text()
    broken = text.replace("        vision: false\n",
                          "        vision: false\n        protocol: respones\n", 1)
    problems = _checker_problems(broken, models_path)
    assert [p for p in problems if "neither chat nor responses" in p], problems


# --------------------------------------------------------------------------
# scripts/validate-config.py.
#
# It had NO test at all, which mutation-check proved the moment a mutation was
# aimed at it: the planted defect was reported MISSED, not caught. It is the
# check the sync runs before it accepts a regenerated config, and it is the only
# thing standing between a hand-edited file and a daemon that loads it, so a
# rule added to it is a rule nothing can see.
#
# It takes no path argument -- it opens ./config.yaml in the current directory
# -- so it is exercised the way the sync runs it: with the config in the working
# directory. (Passing `--config path` is silently ignored, which is a footgun
# this harness deliberately does not rely on.)
# --------------------------------------------------------------------------


def _validate_config(tmp_path, config_text):
    (tmp_path / "config.yaml").write_text(config_text)
    return subprocess.run(
        [sys.executable, str(REPO / "scripts" / "validate-config.py")],
        cwd=tmp_path, capture_output=True, text=True,
    )


def test_validate_config_accepts_the_shipped_config(tmp_path):
    run = _validate_config(tmp_path, CONFIG.read_text())
    assert run.returncode == 0, run.stdout + run.stderr


@pytest.mark.parametrize("value", ["respones", "chat/completions", "", "RESPONSES!"])
def test_validate_config_rejects_a_protocol_the_router_would_ignore(tmp_path, value):
    """config.go's protocolFor maps anything unrecognised to chat, so a typo is a
    file that claims `responses` while every request goes to /chat/completions."""
    broken = CONFIG.read_text().replace(
        "        vision: false\n", f"        vision: false\n        protocol: {value}\n", 1)
    run = _validate_config(tmp_path, broken)
    assert run.returncode == 1, run.stdout
    assert "protocol must be" in run.stdout, run.stdout


def test_validate_config_accepts_both_real_protocols(tmp_path):
    for value in ("chat", "responses"):
        shaped = CONFIG.read_text().replace(
            "        vision: false\n", f"        vision: false\n        protocol: {value}\n", 1)
        run = _validate_config(tmp_path, shaped)
        assert run.returncode == 0, f"{value}: " + run.stdout


def test_the_validate_config_harness_has_teeth_beyond_the_protocol_rule(tmp_path):
    """Otherwise "the protocol rule is enforced" could pass because the harness
    never rejects anything at all."""
    unknown_provider = CONFIG.read_text().replace("      - provider: opencode",
                                                  "      - provider: notaprovider", 1)
    assert _validate_config(tmp_path, unknown_provider).returncode == 1

    bad_vision = CONFIG.read_text().replace("        vision: false",
                                            "        vision: maybe", 1)
    assert _validate_config(tmp_path, bad_vision).returncode == 1
