#!/usr/bin/env python3
"""Validate airouter config.yaml structure after a model sync."""

import sys

import yaml


def _bad_protocol(ep: dict) -> bool:
    """Whether an endpoint declares a `protocol:` the router would not honour.

    Presence, not truthiness: `protocol:` with no value parses to None, which is
    indistinguishable from an absent field by value alone, and a field with no
    value is a claim with no content. config.go's protocolFor maps anything it
    does not recognise to chat, so both a typo and an empty value produce a file
    that says one thing and routes another.
    """
    if "protocol" not in ep:
        return False
    value = ep.get("protocol")
    return value is None or str(value).strip().lower() not in ("chat", "responses")


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

    # Models that the opencode key provably cannot serve, with the reason each
    # one fails. Listing such a model in a chain costs a guaranteed round trip
    # per request that reaches it.
    #
    # MEASURED 2026-10-02 with an AGENT-SHAPED request -- stream:true, a tools
    # array carrying both `bash` and `read`, and the full client-attribution
    # header set. The shape matters, and getting it wrong is exactly how this
    # list came to be wrong in the first place.
    #
    # The previous version held 11 models, justified by "verified 2026-09-29:
    # 11 of 12 return 403 FreeTierError". That verification used a BARE probe
    # -- no stream, no tools -- and a bare probe is rejected by opencode's
    # free-tier gate REGARDLESS of whether the model is entitled. Re-measured
    # with the correct shape, 6 of those 11 answer HTTP 200:
    #
    #   big-pickle, mimo-v2.5-free, mimo-v2.6-flash-free,
    #   longcat-2.5-preview-free, nemotron-3-ultra-free, nemotron-3.5-lightning-free
    #
    # They were being blocked out of every chain, and because the generator
    # keeps emitting them (correctly -- they ARE available) the nightly sync
    # then failed validation and restored the previous config, so the model list
    # could never update at all. One wrong list froze the whole pipeline.
    #
    # The same bare-probe defect was fixed in fetch-free-models.py by 8edfffb;
    # this copy of it survived there.
    #
    # Note the failures below are NOT all 403. Three refuse the protocol
    # outright and one has been removed upstream, so the reason is recorded per
    # model rather than asserted as a blanket FreeTierError.
    # Only models that refuse BOTH shapes are dead. The gateway speaks
    # /chat/completions and /responses and switches between them on an explicit
    # "does not support this protocol" refusal, so a model that answers on
    # either one is usable and must not be blocked.
    OPENCODE_DEAD_MODELS = {
        "jev-1.13-free": "400 ModelProtocolUnsupported on both",
        "deepseek-v4-flash-free": "400 Model is unavailable on both",
        "ling-3.0-flash-fin-free": "404 on chat; 400 ProtocolUnsupported on /responses",
        "fledge-alpha-free": "403, not available in this region, on both",
    }
    for name, mc in models.items():
        for i, ep in enumerate((mc or {}).get("chain") or []):
            if str(ep.get("provider", "")).startswith("opencode"):
                reason = OPENCODE_DEAD_MODELS.get(ep.get("model"))
                if reason:
                    problems.append(
                        f"{name}[{i}]: opencode/{ep['model']} answers {reason} "
                        "and can never serve traffic"
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
            # The wire shape. Not a style rule: config.go's protocolFor maps
            # anything it does not recognise to chat, so a typo here is a file
            # that says `responses` while the router posts to /chat/completions.
            if _bad_protocol(ep):
                problems.append(
                    f"{name}[{i}]: protocol must be 'chat' or 'responses', got "
                    f"{ep.get('protocol')!r} (config.go falls back to chat for "
                    f"anything else, so the file would claim a shape the router "
                    f"ignores)"
                )

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

    # preferences.max_label_cardinality bounds the metrics label registry.
    # Out of range is rejected here, at config time, because the Go loader
    # silently falls back to the default — a mistyped cap would otherwise look
    # like it took effect while the per-endpoint series quietly aggregate into
    # __overflow__ (or, worse, stay at the default and the operator concludes the
    # setting is broken). Keep these bounds in sync with minLabelValues /
    # maxAllowedLabels in metrics.go; TestValidateConfigRangeMatchesGo asserts it.
    MIN_LABEL_CARDINALITY = 16
    MAX_LABEL_CARDINALITY = 65536
    prefs = cfg.get("preferences") or {}
    cap = prefs.get("max_label_cardinality")
    if cap is not None:
        if not isinstance(cap, int) or isinstance(cap, bool):
            problems.append(
                f"preferences.max_label_cardinality must be an integer, got {cap!r}"
            )
        elif not (MIN_LABEL_CARDINALITY <= cap <= MAX_LABEL_CARDINALITY):
            problems.append(
                f"preferences.max_label_cardinality must be between "
                f"{MIN_LABEL_CARDINALITY} and {MAX_LABEL_CARDINALITY}, got {cap}"
            )

    if problems:
        for p in problems:
            print(f"FAIL: {p}")
        return 1

    print(f"OK: {len(models)} profiles validated")
    return 0


if __name__ == "__main__":
    sys.exit(main())
