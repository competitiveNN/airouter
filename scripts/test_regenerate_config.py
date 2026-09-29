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


if __name__ == "__main__":
    sys.exit(pytest.main([__file__, "-v"]))
