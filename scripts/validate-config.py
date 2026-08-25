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

        last_model = chain[-1].get("model")
        if last_model not in ("kilo-auto/free", "big-pickle"):
            problems.append(
                f"{name}: last chain entry must be kilo-auto/free or "
                f"big-pickle, got {last_model!r}"
            )

    if problems:
        for p in problems:
            print(f"FAIL: {p}")
        return 1

    print(f"OK: {len(models)} profiles validated")
    return 0


if __name__ == "__main__":
    sys.exit(main())
