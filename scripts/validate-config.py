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
