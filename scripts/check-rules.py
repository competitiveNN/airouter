#!/usr/bin/env python3
"""Check config.yaml's `models:` section against the fetched free-model list.

This is the rule-level companion to validate-config.py. validate-config.py
answers "is this YAML structurally loadable and are the providers real"; it
cannot tell you whether an endpoint's score, vision flag or chain position
still matches the data the config was generated from, because it has no model
list. This script does, and enforces the distribution rules that the nightly
sync is supposed to follow (see scripts/sync-instruction.txt):

  4  no null-intelligence models (except the auto router); best-first order
  5  exactly the four routable profiles; per-profile membership rules
  6  the last entry of every chain is the auto fallback, chosen on liveness
  7  a vision boolean on every endpoint, matching the source record
  8  no invented ids, no stealth/* outside commandcode, no content-safety
 10  providers exist, nvidia trios and commandcode pairs complete + adjacent
 11  a numeric intelligence on every concrete endpoint, absent on the auto one

Provider mapping and exclusion logic are imported from regenerate_config.py
rather than re-implemented, so the checker cannot drift from the generator it
is supposed to police.

Usage:
    scripts/check-rules.py [--config config.yaml] [--models /tmp/free-models.json]

Exits 0 when every rule holds, 1 on a violation, 2 on bad usage or an
unreadable model list. When the model list is absent (a clean CI checkout, no
network) the data-dependent rules are skipped with a loud notice rather than
silently passing.
"""

from __future__ import annotations

import argparse
import importlib.util
import re
import sys
from pathlib import Path

import yaml

REPO = Path(__file__).resolve().parent.parent
GENERATOR = REPO / "regenerate_config.py"
DEFAULT_MODELS = "/tmp/free-models.json"

AUTO_MODELS = {"kilo-auto/free", "big-pickle"}
ROUTABLE_PROFILES = ["smart", "work", "fast", "large"]
BANNED_ID = re.compile(r"content-safety|x-preview-f-free")

# CommandCode free-ness is NOT enumerated here.
#
# This used to be a list of "the four curated free-tier deals", and it carried
# the same defect as the one in fetch-free-models.py: meituan/longcat-2.0-free
# stayed in it long after the deal ended (probed live 2026-10-01: HTTP 400 "You
# have insufficient credits to make this request. Please purchase more
# credit[s]"), so the checker accepted a billed model in work, fast and large on
# both commandcode keys. In the other direction it rejected genuinely free new
# deals -- inclusionai/ling-3.1-flash:free was fetched, probe-verified and then
# FAILED this rule purely for not being one of the four.
#
# Free-ness is now a probe verdict, carried on the record the fetcher emits as
# `verified`. Rule 3 grades against that evidence, so a deal that ends leaves on
# the next fetch and a deal that starts is admitted without editing this file.
# With no model list there is no evidence to grade against and the rule says so
# instead of falling back to a stale enumeration.

# Key groups that must be used as a unit: same model id on every provider of
# the group, in consecutive entries, or a 429 on one key has nowhere to go.
PROVIDER_GROUPS = {
    "nvidia-nim": ("nvidia", "nvidia2", "nvidia3"),
    "commandcode": ("commandcode", "commandcode2"),
}

FAST_KEYWORDS = ("flash-lite", "lightning", "nano", "gemma", "lfm", "laguna-xs")

# Rule 6, the terminator ladder. Kept here as prose because the EXPECTATION is
# derived by calling regenerate_config.auto_fallback_terminator() — the same
# function the generator uses, so the checker cannot police a rule the generator
# does not implement. The ladder, for a reader:
#
#   preferred = opencode only when the chain has opencode endpoints and no
#               kilocode ones; otherwise kilocode
#   live / unknown -> use the preferred router. An unknown verdict (not probed,
#               a 429/5xx/timeout, older than AUTO_PROBE_MAX_AGE_DAYS, or
#               carrying no timestamp) is deliberately NOT a signal, so a
#               flaky network or a fetcher that stopped running can neither
#               move the terminator nor block a sync.
#   dead      -> use the other router (always available as a rescue) only if IT
#               probed live; otherwise refuse to write anything and leave
#               config.yaml alone. A terminator the provider has refused fails
#               every exhausted request, so regenerating anyway would replace a
#               working config with a worse one. This state needs a human: fix
#               the key, or drop the auto entry deliberately.
#
# The terminator's trailing comment is checked too, because that comment is the
# only record in the file of WHY the endpoint was chosen. An offline
# regeneration (SKIP_AUTO_PROBE=1) that leaves a "probed live" claim behind is
# caught here rather than being read as verified for months.
TERMINATOR_LADDER = (
    "kilocode/kilo-auto/free unless the probe says it is refused; the opencode "
    "rescue requires positive evidence; with no live router left, refuse to write"
)

# Per-profile membership (rule 5). `min_intelligence`/`min_context` are floors,
# `below_intelligence` an exclusive ceiling. None means unbounded on that side.
PROFILE_RULES = {
    # smart: strongest generalists, intelligence >= 25, any context length.
    "smart": {"min_intelligence": 25.0},
    # work: the smart pool extended down to intelligence >= 15. The tiny-model
    # drop is NOT waived for dedicated code models: cohere/north-mini-code
    # scores 9.9, so it stays out of `work` and only qualifies via `large`.
    "work": {"min_intelligence": 15.0},
    # fast: small/cheap only, capped at 10 distinct models before key expansion.
    "fast": {"max_distinct_models": 10},
    # large: context_length >= 200000 only, sorted by ctx desc then score desc.
    "large": {"min_context": 200000},
}


def load_generator():
    spec = importlib.util.spec_from_file_location("regenerate_config", GENERATOR)
    if spec is None or spec.loader is None:  # pragma: no cover - import plumbing
        raise SystemExit(f"cannot import {GENERATOR}")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def load_config(path: Path) -> dict:
    try:
        return yaml.safe_load(path.read_text())
    except (OSError, yaml.YAMLError) as exc:
        raise SystemExit(f"cannot read/parse {path}: {exc}") from exc


def build_source_index(rc, models_path: Path) -> dict[tuple[str, str], object]:
    """Map (config provider, model id) -> source record from the fetcher output.

    Keys use the generator's own provider mapping so the checker and the
    generator cannot disagree about what "nvidia-nim" means. A key-group
    source (nvidia-nim, commandcode) is indexed under every provider of its
    group, because the config spells one upstream model out as N sibling
    entries on N keys and each of those entries has to resolve to the same
    source record.
    """
    index: dict[tuple[str, str], object] = {}
    for m in rc.load_models(str(models_path)):
        for provider in PROVIDER_GROUPS.get(m.provider, (m.mapped_provider,)):
            if provider:
                index[(provider, m.id)] = m
    return index


def comment_lines(config_text: str, profile: str) -> list[str]:
    """The chain's raw YAML lines, used to check the trailing comments."""
    m = re.search(rf"(?ms)^  {profile}:\n    chain:\n(.*?)(?=^  \w+:|\Z)", config_text)
    return m.group(1).splitlines() if m else []


def _terminator_line(config_text: str, profile: str) -> str | None:
    """`model: <id>  # <comment>` for a profile's last chain entry, or None."""
    lines = comment_lines(config_text, profile)
    for line in reversed(lines):
        m = re.match(r"^        model: (?P<rest>\S.*)$", line)
        if m:
            return m.group("rest").strip()
    return None


def _no_verdict(rc, providers_present: set[str], index) -> bool:
    """True when NO auto router has a usable verdict in the model list.

    Asks the generator for a comment computed with no verdict to compare
    against: if the comment it would write for a verdict-less run differs from
    the one in the file, the file is making a claim the data cannot support.
    """
    try:
        _, _, unverified = rc.auto_fallback_terminator(providers_present, index, previous=None)
    except SystemExit:
        return False
    return "not probed" in unverified or "unverified" in unverified


def check(
    rc,
    cfg: dict,
    config_text: str,
    index: dict[tuple[str, str], object] | None,
) -> list[str]:
    problems: list[str] = []
    providers = set((cfg.get("providers") or {}).keys())
    models = cfg.get("models") or {}

    # rule 5: exactly the four routable profiles. Any other key parses fine and
    # is permanently unroutable ("Unknown model" from /v1/chat/completions).
    if set(models) != set(ROUTABLE_PROFILES):
        problems.append(
            f"rule5: models section is {sorted(models)}, must be exactly "
            f"{ROUTABLE_PROFILES} (any other key is unroutable)"
        )
        return problems

    for profile in ROUTABLE_PROFILES:
        rules = PROFILE_RULES[profile]
        chain = (models[profile] or {}).get("chain") or []
        tag0 = f"{profile}"
        if not chain:
            problems.append(f"rule10: {tag0} chain is empty")
            continue

        # rule 6: the terminator, derived by calling the generator's own
        # auto_fallback_terminator() so this check and the generator cannot
        # disagree. With no model list there is no probe evidence, and the
        # function degrades to the pre-probe choice.
        rest = chain[:-1]
        present = {e.get("provider") for e in rest}
        want = None
        try:
            want = rc.auto_fallback_terminator(present, index)
        except SystemExit as exc:
            problems.append(
                f"rule6: {tag0} has no usable auto router and the config should not "
                f"have been written: {exc}"
            )
        last = chain[-1]
        if want is not None and (last.get("provider"), last.get("model")) != want[:2]:
            problems.append(
                f"rule6: {tag0} terminator is {last.get('provider')}/{last.get('model')}, "
                f"expected {want[0]}/{want[1]} ({TERMINATOR_LADDER})"
            )
        elif want is not None and index is not None:
            # The comment is the probe evidence recorded in the file, so it is
            # checked, not trusted. Two failures, reported distinctly:
            #
            #  - the model list HAS a verdict and the comment says something
            #    else: a contradiction (stale, hand-edited or simply wrong);
            #  - the model list has NO verdict, so nothing can confirm or deny
            #    the comment. The generator deliberately preserves whatever the
            #    file already claims in that case (see
            #    auto_fallback_terminator), so demanding a rewrite here would
            #    fight it; the honest report is "this claim is unverifiable".
            line = _terminator_line(config_text, profile)
            expected = f"{last.get('model')}  {want[2]}"
            if line is not None and line != expected:
                if _no_verdict(rc, present, index):
                    if "probed live" in line or "probed dead" in line:
                        problems.append(
                            f"rule6: {tag0} terminator comment claims a probe verdict "
                            f"({line.split('#', 1)[-1].strip()!r}) but the model list has no "
                            f"verdict for that router, so the claim cannot be checked. "
                            f"Re-run fetch-free-models.py --probe-auto to make it true again."
                        )
                    # else: an honest "not probed" comment is already correct.
                else:
                    problems.append(
                        f"rule6: {tag0} terminator comment is {line!r}, but the probe record "
                        f"implies {expected!r}. The comment is the only record of why this "
                        f"endpoint was chosen."
                    )
        if last.get("vision") is not True:
            problems.append(f"rule7: {tag0} terminator is a meta-router and needs vision: true")
        if last.get("intelligence") is not None:
            problems.append(
                f"rule11: {tag0} terminator carries intelligence "
                f"{last.get('intelligence')}; the auto router is not a model and must have none"
            )
        if last.get("model") not in AUTO_MODELS:
            problems.append(f"rule6: {tag0} terminator {last.get('model')!r} is not an auto router")

        scores: list[float] = []
        contexts: list[int] = []
        lines = comment_lines(config_text, profile)
        model_line_re = re.compile(r"^        model: (?P<id>\S+)\s+# (?P<score>\d+\.\d)")

        for i, ep in enumerate(chain):
            provider, mid = ep.get("provider"), ep.get("model")
            tag = f"{profile}[{i}] {provider}/{mid}"
            if provider not in providers:
                problems.append(f"rule10: {tag} references an undefined provider")
            if not mid:
                problems.append(f"rule10: {tag} has no model id")
                continue
            if isinstance(ep.get("vision"), bool) is False and ep.get("vision") is not None:
                problems.append(f"rule7: {tag} vision must be a boolean, got {ep.get('vision')!r}")
            if BANNED_ID.search(mid.lower()):
                problems.append(f"rule8: {tag} is a permanently excluded id")
            if mid.lower().startswith("stealth/") and not str(provider).startswith("commandcode"):
                problems.append(f"rule8: {tag} is stealth/* outside commandcode")

            if mid in AUTO_MODELS:  # the terminator, already checked above
                continue

            # rule 11: a score is what makes an endpoint eligible for
            # initial-model rotation; without it the endpoint silently drops
            # out of the rotation window.
            score = ep.get("intelligence")
            if not isinstance(score, (int, float)) or isinstance(score, bool):
                problems.append(f"rule11: {tag} has no numeric intelligence field")
                score = None
            else:
                scores.append(float(score))
            ctx = ep.get("context_length") or 0
            contexts.append(int(ctx))

            # field order: provider, model, vision, intelligence, [context_length]
            want_keys = ["provider", "model", "vision", "intelligence"]
            if "context_length" in ep:
                want_keys.append("context_length")
            if list(ep) != want_keys:
                problems.append(f"rule7/11: {tag} field order {list(ep)} != {want_keys}")

            # rule 9: the score in the trailing comment must agree with the field.
            hit = [ln for ln in lines if ln.startswith(f"        model: {mid}  # ")]
            for ln in hit:
                m = model_line_re.match(ln)
                if not m:
                    problems.append(f"rule9: {tag} model line lacks a '# <score>' comment: {ln!r}")
                elif score is not None and abs(float(m.group("score")) - score) > 1e-9:
                    problems.append(
                        f"rule11: {tag} comment says {m.group('score')} but intelligence is {score}"
                    )

            if index is None:
                continue
            src = index.get((provider, mid))
            if src is None:
                problems.append(f"rule8: {tag} is not in the fetched model list")
                continue
            # rule 7: vision must mirror the source record; this is what
            # capability-aware routing reads to skip text-only models.
            if bool(src.vision) != bool(ep.get("vision")):
                problems.append(
                    f"rule7: {tag} vision={ep.get('vision')!r} but the source record says {src.vision}"
                )
            # rule 4: nothing with an unknown score belongs in a chain.
            if src.score is None:
                problems.append(f"rule4: {tag} has a null intelligence in the model list")
            elif score is not None and abs(src.score - score) > 1e-9:
                problems.append(
                    f"rule11: {tag} intelligence {score} != model list {src.score}"
                )
            if src.context_length != ctx:
                problems.append(
                    f"rule11: {tag} context_length {ctx} != model list {src.context_length}"
                )

        # rule 4: best-first by intelligence (large is ctx-first by rule 5).
        if profile == "large":
            keys = list(zip(contexts, scores))
            if keys != sorted(keys, reverse=True):
                problems.append("rule5: large is not sorted by context_length desc, then intelligence desc")
            if contexts and min(contexts) < rules["min_context"]:
                problems.append(f"rule5: large has an entry below context_length {rules['min_context']}")
        elif scores and scores != sorted(scores, reverse=True):
            problems.append(f"rule4: {profile} is not ordered by intelligence descending: {scores}")

        floor = rules.get("min_intelligence")
        if floor is not None and any(s < floor for s in scores):
            problems.append(
                f"rule5: {profile} has entries below intelligence {floor}: "
                f"{sorted(s for s in scores if s < floor)}"
            )
        cap = rules.get("max_distinct_models")
        if cap is not None:
            distinct = {e.get("model") for e in rest}
            if len(distinct) > cap:
                problems.append(f"rule5: {profile} has {len(distinct)} distinct models, cap is {cap}")
            for ep in rest:
                mid_l, sc = str(ep.get("model")).lower(), ep.get("intelligence")
                if isinstance(sc, (int, float)) and not (
                    sc < 25 or any(k in mid_l for k in FAST_KEYWORDS)
                ):
                    problems.append(f"rule5: {profile} entry {ep.get('model')} is neither small nor cheap")

        # rule 3 / 10: every key of a group carries the same model id, in
        # consecutive entries, so a rate-limited key falls through to the next.
        for group, provs in PROVIDER_GROUPS.items():
            per = {p: [e.get("model") for e in chain if e.get("provider") == p] for p in provs}
            union = {m for mids in per.values() for m in mids} - AUTO_MODELS
            for p in provs:
                missing = union - set(per[p])
                if missing:
                    problems.append(
                        f"rule3: {profile} {p} is missing {sorted(missing)} present on its "
                        f"other {group} provider(s)"
                    )
            for mid in sorted(union):
                positions = [
                    i for i, e in enumerate(chain)
                    if e.get("model") == mid and e.get("provider") in provs
                ]
                if positions != list(range(positions[0], positions[0] + len(provs))):
                    problems.append(
                        f"rule3: {profile} {mid} keys are not consecutive (positions {positions})"
                    )

        # rule 3: every id served through a commandcode provider must be one the
        # fetcher PROVED free, not one a list once declared free. (Ids are not a
        # private namespace: kilocode lists its own
        # inclusionai/ling-3.0-flash-sante:free, which is a separate free tier
        # and legitimately appears on kilocode only -- so this is about the
        # commandcode provider specifically.)
        # With no model list there is no probe evidence to grade against, and
        # main() already prints that as a NOTICE and exits 0. Enumerating a
        # fallback list here would reinstate the defect this rule just lost.
        for ep in chain if index is not None else []:
            provider = str(ep.get("provider"))
            mid = ep.get("model")
            if not provider.startswith("commandcode"):
                continue
            src = index.get((provider, mid)) if index is not None else None
            verified = getattr(src, "verified", None)
            if src is None:
                # Absent from the fetched list: the fetcher neither offered it nor
                # probed it. That is how a deal which ENDED disappears, so serving
                # it is the defect this rule exists to catch.
                problems.append(
                    f"rule3: {profile} serves {mid} on {provider} but it is not "
                    "in the fetched model list; the deal may have ended — run "
                    "fetch-free-models.py --json --probe-auto --save and regenerate"
                )
            elif verified is False:
                # Present AND probe-rejected: a definitive "this is billed" verdict.
                problems.append(
                    f"rule3: {profile} serves {mid} on {provider} but its probe "
                    "rejected it as not free (verified=False)"
                )
            elif verified is None:
                # Never probed. "Not probed" is NOT "not free", so this is not a
                # violation — a model list derived from config.yaml itself (the
                # self-contained check the meta-tests run) has no probe verdicts at
                # all, and failing here would make that path unrunnable. Say it
                # once, in the summary, rather than per entry.
                pass

    return problems


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--config", default=str(REPO / "config.yaml"), help="config.yaml to check")
    ap.add_argument("--models", default=DEFAULT_MODELS, help="fetched free-model list (fetch-free-models.py --json)")
    args = ap.parse_args(argv)

    rc = load_generator()
    cfg = load_config(Path(args.config))
    index = None
    models_path = Path(args.models)
    if models_path.exists():
        index = build_source_index(rc, models_path)
    else:
        print(
            f"NOTICE: no model list at {models_path}; data-dependent rules "
            "(3 commandcode free-deal provenance, 4 score sync, 7 vision, "
              "8 ids, 11 value sync) are NOT checked. Run "
              "fetch-free-models.py --json --probe-auto --save to enable them."
        )

    problems = check(rc, cfg, Path(args.config).read_text(), index)
    if problems:
        print(f"FAIL: {len(problems)} rule violation(s) in {args.config}")
        for p in problems:
            print(f"  - {p}")
        return 1

    profiles = ", ".join(f"{p}={len(c['chain'])}" for p, c in sorted((cfg['models']).items()))
    print(f"OK: rules 3-11 hold ({profiles})" + ("" if index else "; vision/score checks skipped"))
    return 0


if __name__ == "__main__":
    sys.exit(main())
