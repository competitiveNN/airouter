#!/usr/bin/env python3
"""Validate airouter config.yaml structure after a model sync."""

import sys

import yaml


def main() -> int:
    try:
        with open("config.yaml") as f:
            cfg = yaml.safe_load(f)
    except (OSError, yaml.YAMLError) as e:
        print(f"FAIL: cannot read/parse config.yaml: {e}")
        return 1

    providers = cfg.get("providers", {})
    models = cfg.get("models", {})
    problems: list[str] = []

    if not providers:
        problems.append("no providers configured")

    # The gateway's LogicalModels is exactly [smart work fast large]. A key
    # outside that set parses fine but is permanently unroutable —
    # /v1/chat/completions answers "Unknown model". The nightly maki sync
    # added a `test:` profile this way before it was caught, so reject any
    # extra profile at the validation step, where it is cheap to fix.
    extra = set(models) - {"smart", "work", "fast", "large"}
    if extra:
        problems.append(
            f"unroutable profile(s) {sorted(extra)}: models may only contain "
            "smart/work/fast/large (see LogicalModels in config.go)"
        )

    # Models that the opencode key provably cannot call. Verified 2026-09-29:
    # 11 of 12 return 403 FreeTierError, paid big-pickle included; the
    # attribution headers do not change it. Listing them in a chain costs a
    # guaranteed round trip per request that reaches them.
    OPENCODE_DEAD_MODELS = {
        "big-pickle",
        "jev-1.13-free",
        "deepseek-v4-flash-free",
        "muse-spark-1.2-contributor-free",
        "muse-spark-1.3-contributor-free",
        "mimo-v2.5-free",
        "mimo-v2.6-flash-free",
        "longcat-2.5-preview-free",
        "nemotron-3-ultra-free",
        "nemotron-3.5-lightning-free",
        "ling-3.0-flash-fin-free",
    }
    for name, mc in models.items():
        for i, ep in enumerate((mc or {}).get("chain") or []):
            if str(ep.get("provider", "")).startswith("opencode"):
                if ep.get("model") in OPENCODE_DEAD_MODELS:
                    problems.append(
                        f"{name}[{i}]: opencode/{ep['model']} returns 403 "
                        "FreeTierError and can never serve traffic"
                    )

    # Provider groups: models from these sources must appear on every provider
    # in their group (same model id, consecutive entries) so a rate-limited
    # key falls through to the next key on the same model.
    provider_groups: dict[str, tuple[str, ...]] = {
        "nvidia-nim": ("nvidia", "nvidia2", "nvidia3"),
        "commandcode": ("commandcode", "commandcode2"),
    }

    for name in ("smart", "work", "fast", "large"):
        if name not in models:
            problems.append(f"missing profile {name}")
            continue
        chain = models[name].get("chain") or []
        if not chain:
            problems.append(f"{name}: empty chain")
            continue
        for i, ep in enumerate(chain):
            provider = ep.get("provider")
            if provider not in providers:
                problems.append(f"{name}[{i}]: unknown provider {provider!r}")
            if not ep.get("model"):
                problems.append(f"{name}[{i}]: missing model")
            vision = ep.get("vision")
            if vision is not None and not isinstance(vision, bool):
                problems.append(f"{name}[{i}]: vision must be a boolean, got {vision!r}")

        last_model = chain[-1].get("model")
        if last_model not in ("kilo-auto/free", "big-pickle"):
            problems.append(
                f"{name}: last chain entry must be kilo-auto/free or "
                f"big-pickle, got {last_model!r}"
            )

        # Enforce provider-group completeness: if a model from a grouped
        # source appears on any provider in the group, it must appear on
        # ALL providers in that group (same model id) so rate-limit
        # fallback has a sibling key to fall through to.
        for source, group in provider_groups.items():
            # Map provider -> set of model ids used in this chain
            prov_models: dict[str, set[str]] = {p: set() for p in group}
            for ep in chain:
                prov = ep.get("provider", "")
                if prov in prov_models:
                    mid = ep.get("model", "")
                    if mid not in ("kilo-auto/free", "big-pickle"):
                        prov_models[prov].add(mid)
            # Union of all model ids across the group
            all_models: set[str] = set()
            for mids in prov_models.values():
                all_models.update(mids)
            if not all_models:
                continue
            # Every provider in the group must carry the full set
            for prov, mids in prov_models.items():
                missing = all_models - mids
                if missing:
                    problems.append(
                        f"{name}: {source} provider {prov} missing model(s) "
                        f"{sorted(missing)} present on other {source} provider(s)"
                    )

    if problems:
        for p in problems:
            print(f"FAIL: {p}")
        return 1

    print(f"OK: {len(models)} profiles validated")
    return 0


if __name__ == "__main__":
    sys.exit(main())
