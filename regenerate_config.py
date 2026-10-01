#!/usr/bin/env python3
"""
Regenerate the models section in config.yaml from free-models.json
"""

import argparse
import json
import os
import re
import shutil
import sys
from dataclasses import dataclass
from datetime import UTC, datetime
from typing import Any


def _parse_release_date(value: str | int | None) -> datetime | None:
    """Parse a release date string/int into a datetime, or None.

    Accepts "YYYY-MM-DD", "YYYY-MM", Unix timestamps, and ISO strings.
    Returns None for anything unparseable so the caller can fall back to
    the "no date" sentinel.
    """
    if value is None:
        return None
    if isinstance(value, int):
        if value <= 0:
            return None
        try:
            return datetime.fromtimestamp(value, tz=UTC)
        except (ValueError, OSError):
            return None
    if isinstance(value, str):
        s = value.strip()
        if not s:
            return None
        for fmt in ("%Y-%m-%d", "%Y-%m", "%Y-%m-%dT%H:%M:%SZ", "%Y-%m-%dT%H:%M:%S%z"):
            try:
                return datetime.strptime(s, fmt).replace(tzinfo=UTC)
            except ValueError:
                continue
        # ISO-8601 with fractional seconds and/or a numeric offset, which is
        # what `datetime.now(UTC).isoformat()` produces. It matters: the
        # auto-router probe writes exactly that form, and a parser that returns
        # None for it silently downgrades every probe verdict to "unprobed",
        # which disables the terminator guard without any error anywhere.
        try:
            parsed = datetime.fromisoformat(s.replace("Z", "+00:00"))
        except ValueError:
            return None
        return parsed if parsed.tzinfo else parsed.replace(tzinfo=UTC)
    return None


# ── Size-tier heuristics ──────────────────────────────────────────────────
# When two models share the same intelligence score AND the same release
# date (or both lack a date), the size-tier suffix acts as a final
# tie-breaker: models whose names suggest a smaller/cheaper variant
# (mini, tiny, flash, lite, ...) sort AFTER the larger/stronger ones
# (pro, max, ultra, big, ...).  This mirrors how the upstream providers
# themselves position their lineups and prevents a "mini" model from
# shadowing its larger sibling in a fallback chain.
#
# The match is case-insensitive and operates on the model id with the
# org prefix stripped, so "nex-agi/nex-n2.5-mini:free" matches "mini".
# Suffixes are matched as whole tokens (separated by - or _) so a model
# named "minimax-m3" is not mistaken for "mini".

_SMALL_SUFFIXES: tuple[str, ...] = (
    # Obvious size words
    "mini", "tiny", "small", "micro", "nano",
    # Speed/cheap tiers
    "fast", "flash", "lite", "light", "quick", "swift",
    # Entry-level / distilled variants
    "base", "distil",
    "air", "express", "eco", "budget", "cheap",
    # Specific product-line small tiers
    "flash-lite", "flash-lightning",
    "lfm", "lightning", "omni-mini",
    # OpenCode / Kilo specific small tiers
    "flash-free", "contributor-free",
    # Gemma size tiers — match on the parameter-count token so that
    # "gemma-4-31b-it" (tokens: gemma, 4, 31b, it) is recognised as small
    # relative to a hypothetical "gemma-4-128b-it".  We use the bare
    # parameter token rather than the full family name so the heuristic
    # generalises to future Gemma releases.
    "gemma-4-2b", "gemma-4-9b", "gemma-4-26b-a4b", "gemma-4-31b",
    # Poolside Laguna small tiers — "laguna-xs-2.1" tokenises to
    # laguna, xs, 2, 1, so we match on the "xs" token; "laguna-s-2.1"
    # tokenises to laguna, s, 2, 1, so we match on the "s" token too.
    "laguna-xs", "laguna-s-2.1", "xs", "s",
    # Mistral size tiers
    "medium", "medium-3.5",
    # OpenAI GPT-OSS size tiers
    "gpt-oss-20b",
    # NVIDIA NIM size tiers
    "nemotron-3-super-120b-a12b", "nemotron-3-ultra-550b-a55b",
    # Gemma parameter-count tokens — matched against the bare token
    # (e.g. "31b", "2b") so that "gemma-4-31b-it" and "gemma-4-2b-it"
    # are recognised as small.  Only the low-count Gemma variants are
    # listed here; higher counts (128b, 200b, ...) are neutral.
    "2b", "9b", "26b", "26b-a4b", "31b",
)

_LARGE_SUFFIXES: tuple[str, ...] = (
    # Obvious size words
    "max", "big", "large", "huge", "mega", "giant", "ultra",
    "pro", "plus", "xl", "xxl",
    # Performance / flagship tiers
    "premium", "elite", "supreme", "power", "turbo",
    # Specific product-line large tiers
    "pro-max", "ultra-pro", "max-pro",
    # OpenRouter / Kilo specific large tiers
    "big-pickle",
    # Mistral size tiers
    "mistral-large", "mistral-large-2",
    # OpenAI GPT-OSS size tiers
    "gpt-oss-120b",
    # NVIDIA NIM size tiers
    "nemotron-3-super-120b-a12b", "nemotron-3-ultra-550b-a55b",
)


def _size_tier(model_id: str) -> int:
    """Return a size-tier score for a model id.

    Returns:
        1  if the id suggests a LARGE/strong model (sorts first)
        0  if the id is neutral (no size suffix)
        -1 if the id suggests a SMALL/cheap model (sorts last)

    The org prefix is stripped before matching so "nex-agi/nex-n2.5-mini"
    and "nvidia/nemotron-3-nano" both match correctly.
    """
    # Strip org prefix and :free / :latest suffixes
    name = model_id.lower()
    if "/" in name:
        name = name.split("/", 1)[1]
    for suffix in (":free", ":latest", "-free"):
        if name.endswith(suffix):
            name = name[: -len(suffix)]
    # Tokenize on - _ . :
    tokens = re.split(r"[-_.:\s]+", name)
    # Also check the full name for multi-word suffixes (e.g. "flash-lite")
    full = name

    # Check large suffixes first — a model named "pro-max-mini" is still pro
    for suf in _LARGE_SUFFIXES:
        if suf in tokens or full.endswith(suf):
            return 1
    for suf in _SMALL_SUFFIXES:
        if suf in tokens or full.endswith(suf):
            return -1
    return 0


# How long an auto-router probe verdict stays actionable. A `live` verdict
# pins the terminator, so a router that dies between syncs would otherwise be
# trusted indefinitely: the fetcher stops running, the last verdict stays in the
# list, and the config keeps pointing at a dead endpoint with no signal. Past
# this age a verdict decays to `unknown` — which is not the same as `dead`, so
# it does NOT move the terminator off a working provider or block a sync; it
# only stops being evidence for a rescue. A week is generous for a probe that
# the nightly sync runs daily, and it is deliberately not a short number: a
# weekend or a holiday must not invalidate the config.
AUTO_PROBE_MAX_AGE_DAYS = 7

# Endpoints already reported as having an untimestamped verdict, so four chains
# asking the same question do not print the same warning four times.
_WARNED_NO_TIMESTAMP: set[tuple[str, str]] = set()


@dataclass
class Model:
    id: str
    name: str
    provider: str  # original provider from JSON
    context_length: int
    intelligence: float | None
    intelligence_source: str | None
    intelligence_note: str | None
    elo: float | None
    vision: bool
    raw: dict
    released: str | int | None = None
    # fetch-free-models.py sets verified=True on a candidate that answered a
    # real 1-token completion and verified=False on one it probed and REJECTED.
    # None means no verdict: not probed, or the probe could not reach one (429,
    # 5xx, timeout) — see classify_probe_status in fetch-free-models.py.
    verified: bool | None = None
    # True for the minimal record --probe-auto appends when an auto router has
    # no free-tier listing to stamp (opencode/big-pickle). It exists so the
    # terminator guard can see a verdict for it; it is never a chain candidate.
    probe_only: bool = False
    # When the auto-router probe ran, from auto_probe.checked_at. A verdict
    # without a timestamp cannot be shown to be fresh, so it is not trusted.
    probe_checked_at: datetime | None = None

    @property
    def score(self) -> float | None:
        """Effective intelligence score (AA, Arena ELO, or the smart floor)."""
        return self.intelligence

    @property
    def release_dt(self) -> datetime | None:
        """Parsed release date, or None if unknown."""
        return _parse_release_date(self.released)

    @property
    def mapped_provider(self) -> str | None:
        """Map JSON provider to config provider"""
        mapping = {
            'kilocode': 'kilocode',
            'opencode': 'opencode',
            'ollama-cloud': 'ollama',
            'google-ai-studio': 'gemini',
            'nvidia-nim': 'nvidia',  # will be expanded to trio
            'commandcode': 'commandcode',  # expanded to commandcode/commandcode2 pair
        }
        return mapping.get(self.provider)

    @property
    def is_nvidia_nim(self) -> bool:
        return self.provider == 'nvidia-nim'

    @property
    def is_excluded(self) -> bool:
        """Check if model should be excluded per rule 8"""
        # Never include: stealth/*, anything matching *content-safety*, or x-preview-f-free
        id_lower = self.id.lower()
        # The stealth/* exclusion targets opaque OpenRouter placeholder models
        # whose real identity is unknown. CommandCode publishes a legitimate
        # free deal on `stealth/space-bunny-alpha` (a named, scored-as-unknown
        # preview with a documented 1M ctx and $0/M pricing), so it is allowed
        # through for that provider only.
        if 'stealth' in id_lower and self.provider != 'commandcode':
            return True
        if 'content-safety' in id_lower:
            return True
        if 'x-preview-f-free' in id_lower:
            return True
        return False

    @property
    def is_auto_fallback(self) -> bool:
        """Check if this is an auto-fallback model"""
        return self.id in ('kilo-auto/free', 'big-pickle')

    @property
    def is_chain_candidate(self) -> bool:
        """May this model be placed in a chain as a concrete endpoint?

        False for the --probe-auto placeholder records: they carry a liveness
        verdict and nothing else, and the terminator is the only place a meta
        router belongs. Every profile filter also requires a non-null score, so
        this is belt and braces — but the exclusion is a property worth having
        in one place rather than a coincidence of four filters.
        """
        return not self.probe_only and not self.is_auto_fallback

    def get_comment(self) -> str:
        """Generate trailing comment with score and elo (no ctx — it is a
        real field now, surfaced as max_tokens by /v1/models)."""
        parts = []
        if self.score is not None:
            parts.append(f"{self.score:.1f}")
            if self.intelligence_source == "smart floor":
                parts.append("floor")
        if self.elo is not None:
            parts.append(f"elo={int(self.elo)}")
        return f"# {'  '.join(parts)}" if parts else ""


def load_models(json_path: str) -> list[Model]:
    with open(json_path) as f:
        data = json.load(f)

    models = []
    for item in data:
        caps = item.get('capabilities', {})
        m = Model(
            id=item['id'],
            name=item.get('name', ''),
            provider=item.get('provider', ''),
            context_length=item.get('context_length') or 0,
            intelligence=item.get('intelligence'),
            intelligence_source=item.get('intelligence_source'),
            intelligence_note=item.get('intelligence_note'),
            elo=item.get('elo'),
            vision=caps.get('vision', False),
            raw=item.get('raw', {}),
            released=item.get('released'),
            verified=item.get('verified'),
            probe_only=bool(item.get('probe_only', False)),
            probe_checked_at=_parse_release_date(
                (item.get('auto_probe') or {}).get('checked_at')
            ),
        )
        models.append(m)
    return models


def get_unique_models(models: list[Model]) -> dict[str, Model]:
    """Get unique models by (mapped_provider, id), preferring the one with score"""
    unique = {}
    for m in models:
        if m.is_excluded:
            continue
        mp = m.mapped_provider
        if not mp:
            continue
        key = (mp, m.id)
        if key not in unique:
            unique[key] = m
        else:
            # Prefer model with score
            if m.score is not None and unique[key].score is None:
                unique[key] = m
            # If both have scores, prefer higher
            elif m.score is not None and unique[key].score is not None:
                if m.score > unique[key].score:
                    unique[key] = m
    return unique


def sort_models(
    models: list[Model],
    newest_first_on_tie: bool = True,
    context_first: bool = False,
) -> list[Model]:
    """Sort models by intelligence score descending.

    When `newest_first_on_tie` is true (the configured preference), models
    with the same intelligence score are ordered newest-first by release
    date.  Models with no known release date sort after dated ones within
    the same score band (their relative order is preserved from the
    source enumeration).

    When `context_first` is true (the `large` profile), the primary sort
    key is context_length descending, with intelligence descending as the
    secondary key — and the newest-first tie-break still applies within
    equal intelligence bands.
    """
    if not newest_first_on_tie:
        if context_first:
            models.sort(
                key=lambda x: (x.context_length, x.score or 0),
                reverse=True,
            )
        else:
            models.sort(key=lambda x: x.score or 0, reverse=True)
        return models

    unknown = datetime.min.replace(tzinfo=UTC)
    if context_first:
        models.sort(
            key=lambda x: (
                x.context_length,
                x.score or 0,
                x.release_dt if x.release_dt is not None else unknown,
                # Size tier: larger models sort first within a tie band.
                # _size_tier returns 1 for large, -1 for small, 0 for neutral.
                # Negate so that 1 (large) comes before -1 (small) when
                # the sort is reversed.
                -_size_tier(x.id),
            ),
            reverse=True,
        )
    else:
        models.sort(
            key=lambda x: (
                x.score or 0,
                x.release_dt if x.release_dt is not None else unknown,
                -_size_tier(x.id),
            ),
            reverse=True,
        )
    return models


def filter_smart(models: list[Model]) -> list[Model]:
    """smart: strongest generalists, intelligence >= 25, any context length"""
    return [m for m in models
            if m.score is not None and m.score >= 25 and m.is_chain_candidate]


def filter_work(models: list[Model]) -> list[Model]:
    """work: coding workhorses. Same pool as smart plus dedicated code models (e.g. cohere/north-mini-code). Drop tiny models (intelligence < 15).

    'Same pool as smart' means the broader eligible pool (all non-null-intelligence,
    non-excluded, non-auto-fallback models), extending down to intelligence >= 15 to
    include more workhorse models. Dedicated code models (e.g. cohere/north-mini-code)
    are included when they meet the >= 15 threshold.
    """
    return [m for m in models
            if m.score is not None and m.score >= 15
            and not m.is_excluded and m.is_chain_candidate]


def filter_fast(models: list[Model]) -> list[Model]:
    """fast: small/cheap models only: intelligence < 25, or ids containing flash-lite / lightning / nano / gemma / lfm / laguna-xs. Cap at 10 entries."""
    fast_keywords = ['flash-lite', 'lightning', 'nano', 'gemma', 'lfm', 'laguna-xs']
    candidates = []
    for m in models:
        if m.score is None or not m.is_chain_candidate:
            continue
        id_lower = m.id.lower()
        is_fast = m.score < 25 or any(kw in id_lower for kw in fast_keywords)
        if is_fast:
            candidates.append(m)
    # Sort by intelligence desc, take top 10
    candidates.sort(key=lambda x: x.score, reverse=True)
    return candidates[:10]


def filter_large(models: list[Model]) -> list[Model]:
    """large: context_length >= 200000 only. Sort by context_length desc, then intelligence desc.
    Per rule 4: skip null-intelligence models; auto models only appear as the trailing entry."""
    candidates = [m for m in models
                  if m.context_length >= 200000
                  and m.score is not None
                  and m.is_chain_candidate]
    candidates.sort(key=lambda x: (x.context_length, x.score or 0), reverse=True)
    return candidates


def auto_fallback_terminator(
    providers_present: set[str],
    catalog: dict[tuple[str, str], Model] | None = None,
    previous: tuple[str, str, str] | None = None,
) -> tuple[str, str, str]:
    """Pick the chain terminator and return (provider, model, comment).

    Liveness, never popularity. The opencode key authenticates but is not
    entitled to the free tier: 11 of its 12 models 403 FreeTierError (paid
    big-pickle included), so a kilocode-vs-opencode COUNT comparison is not
    evidence of anything — it once appended opencode/big-pickle as the
    terminator of `smart` while that endpoint failed on every call.

    `catalog` is the full (mapped_provider, id) -> Model catalog, built from
    fetch-free-models.py --probe-auto output. Verdict per candidate, from that
    record's `verified` field:

        live     verified is True  — a 1-token completion came back
        dead     verified is False — the provider REFUSED the call (4xx)
        unknown  verified is None  — not probed, or the probe could not reach
                 a verdict (429, 5xx, timeout). Says nothing about the endpoint.

    The ladder, in full:

        1. Preferred = opencode only when the chain contains opencode endpoints
           and no kilocode ones; otherwise kilocode. The rescue is always the
           other router, whether or not the chain already uses that provider:
           a chain with no kilocode endpoint at all still needs a terminator,
           and a live opencode router beats a refused kilocode one even where
           it introduces a provider the chain did not have.
        2. live or unknown -> use the preferred router. `unknown` deliberately
           does not change today's behaviour: a flaky network must never be
           able to move the terminator off a working provider, and a stale
           verdict (older than AUTO_PROBE_MAX_AGE_DAYS) counts as unknown for
           the same reason.
        3. dead -> use the other router ONLY if it probed live. Promoting an
           unverified endpoint over a confirmed-dead one would be trading a
           known failure for a possible one.
        4. dead with no live alternative -> abort, leaving the existing config
           in place. Emitting a terminator the provider has refused turns every
           exhausted chain into a failed request, and regenerating anyway would
           overwrite a working config with a worse one. This is a state a human
           has to resolve (fix the key, or drop the auto entry deliberately);
           the previous behaviour was to write the dead endpoint and hope.

    The decision is per chain but the abort is global, because the generator
    writes all four chains at once. That is why the rescue is unconditional:
    a single degenerate chain (say a `fast` profile with no qualifying model)
    must not be able to strand the other three.

    With no catalog at all (a caller that cannot probe) step 2 always applies,
    so the choice degrades to the pre-probe rule rather than to a hard failure.

    `previous` is the (provider, model, comment) this chain already carries, if
    any. When the verdict is unknown AND the endpoint is unchanged, the existing
    comment is kept verbatim: a run that could not check the claim must not
    rewrite it into a different claim. A regeneration from a verdict-less list
    (SKIP_AUTO_PROBE=1, a fixture, a fetcher that never probed) would otherwise
    overwrite a recorded "probed live" with "not probed" — destroying the only
    provenance the file has — and the next run would do it again, so the diff
    would be permanent noise. scripts/check-rules.py reports an unverifiable
    claim as its own problem instead, which is where an operator will look.
    Any real change of endpoint, or a fresh verdict, still writes a fresh
    comment.
    """
    available = {
        'kilocode': ('kilocode', 'kilo-auto/free'),
        'opencode': ('opencode', 'big-pickle'),
    }

    def verdict(provider: str) -> str:
        rec = (catalog or {}).get(available[provider])
        if rec is None:
            return 'unknown'
        if rec.verified is None:
            return 'unknown'
        # Freshness. A verdict is a fact about one moment; a `live` from three
        # weeks ago is evidence that the endpoint existed, not that it works
        # now, and a `dead` from three weeks ago may since have been fixed. Both
        # decay to `unknown`, which keeps the preferred router and cannot
        # trigger the rescue. A verdict with no timestamp at all is treated the
        # same way: unverifiable freshness is not freshness.
        checked = rec.probe_checked_at
        if checked is None:
            key = available[provider]
            if key not in _WARNED_NO_TIMESTAMP:
                _WARNED_NO_TIMESTAMP.add(key)
                print(
                    f'warning: {key[0]}/{key[1]} carries a verdict with no probe '
                    f'timestamp; treating it as unprobed',
                    file=sys.stderr,
                )
            return 'unknown'
        age_days = (datetime.now(UTC) - checked).total_seconds() / 86400.0
        if age_days > AUTO_PROBE_MAX_AGE_DAYS:
            return 'unknown'
        return 'live' if rec.verified is True else 'dead'

    if 'opencode' in providers_present and 'kilocode' not in providers_present:
        preferred, rescue = 'opencode', 'kilocode'
        why = 'kilocode absent from chain'
    elif 'kilocode' in providers_present:
        preferred, rescue = 'kilocode', 'opencode'
        why = 'opencode auto not entitled to free tier'
    else:
        preferred, rescue = 'kilocode', 'opencode'
        why = 'no auto provider in chain'

    if verdict(preferred) != 'dead':
        provider, model = available[preferred]
        state = verdict(preferred)
        if state == 'unknown' and previous is not None \
                and previous[:2] == (provider, model) and previous[2]:
            return provider, model, previous[2]
        # The probe verdict is stable text, not the probe timestamp: a date here
        # would rewrite every chain on every nightly run for no added signal,
        # and "probed live" is the part worth having in the file.
        if state == 'live':
            note = f'auto router probed live; {why}'
        else:
            note = why + ('; auto router not probed (unknown verdict)'
                          if catalog is not None else '')
        return provider, model, f'# last: {preferred} ({note})'

    if verdict(rescue) == 'live':
        provider, model = available[rescue]
        print(
            f'warning: {preferred} auto router is dead in the model list; '
            f'falling back to {rescue}/{model}, which probed live',
            file=sys.stderr,
        )
        return provider, model, f'# last: {rescue} ({preferred} auto router probed dead)'

    provider, model = available[preferred]
    alternative = (
        f'{rescue}/{available[rescue][1]} is not live either ({verdict(rescue)})'
    )
    raise SystemExit(
        f'refusing to write {provider}/{model} as the chain terminator: the probe '
        f'record in the model list says the provider refuses it, and {alternative}. '
        f'A dead last-resort endpoint turns every exhausted chain into a failed '
        f'request. config.yaml has been left unchanged — fix the key, re-fetch '
        f'with --probe-auto, or drop the auto entry deliberately.'
    )


def parse_terminators(config_text: str) -> dict[str, tuple[str, str, str]]:
    """Extract each profile's current terminator as (provider, model, comment).

    The comment is needed verbatim: when a run cannot verify the probe verdict,
    the terminator decision keeps whatever the file already says rather than
    replacing a recorded claim with a different one (see
    auto_fallback_terminator).
    """
    out: dict[str, tuple[str, str, str]] = {}
    for profile in ('smart', 'work', 'fast', 'large'):
        m = re.search(
            rf'(?ms)^  {profile}:\n    chain:\n(.*?)(?=^  \w+:|\Z)', config_text
        )
        if not m:
            continue
        entry = None
        for line in m.group(1).splitlines():
            prov = re.match(r'^      - provider: (\S+)\s*$', line)
            if prov:
                entry = [prov.group(1), '', '']
            # Capture the comment INCLUDING its hash, exactly as written, so
            # format_chain_yaml can write it back byte-for-byte. Rebuilding a
            # '#' around stripped text is how the bare-comment case turned into
            # a YAML syntax error, and requiring '# ' (hash AND space) meant a
            # bare '#' was not recognised as a comment at all.
            mod = re.match(r'^        model: (\S+)(?:\s+(#.*))?$', line)
            if mod and entry is not None:
                entry[1] = mod.group(1)
                entry[2] = mod.group(2) or ''
        if entry and entry[1]:
            out[profile] = (entry[0], entry[1], entry[2])
    return out


def build_chain(
    models: list[Model],
    profile: str,
    records: dict[tuple[str, str], Model] | None = None,
    previous: tuple[str, str, str] | None = None,
) -> list[dict]:
    """Build fallback chain for a profile.

    `records` is the full (mapped_provider, id) -> Model catalog used for the
    terminator liveness check; see auto_fallback_terminator. `previous` is this
    profile's current terminator, so an unverifiable run preserves its comment.
    """
    if records is None:
        records = {}
    chain = []

    for m in models:
        if m.mapped_provider == 'nvidia':
            # Emit trio: nvidia, nvidia2, nvidia3
            for prov in ['nvidia', 'nvidia2', 'nvidia3']:
                chain.append({
                    'provider': prov,
                    'model': m.id,
                    'vision': m.vision,
                    'intelligence': m.score,
                    'context_length': m.context_length,
                    'comment': m.get_comment()
                })
        elif m.mapped_provider == 'commandcode':
            # Emit pair: commandcode, commandcode2 (two API keys on the same
            # upstream proxy) so a rate-limited key falls through to the next.
            for prov in ['commandcode', 'commandcode2']:
                chain.append({
                    'provider': prov,
                    'model': m.id,
                    'vision': m.vision,
                    'intelligence': m.score,
                    'context_length': m.context_length,
                    'comment': m.get_comment()
                })
        else:
            chain.append({
                'provider': m.mapped_provider,
                'model': m.id,
                'vision': m.vision,
                'intelligence': m.score,
                'context_length': m.context_length,
                'comment': m.get_comment()
            })

    # The last-resort endpoint must itself be callable, so the choice is a
    # liveness question, never a popularity one.
    auto_provider, auto_model, auto_comment = auto_fallback_terminator(
        {e['provider'] for e in chain}, records, previous
    )

    # Add auto fallback with vision: true
    chain.append({
        'provider': auto_provider,
        'model': auto_model,
        'vision': True,
        'comment': auto_comment
    })
    
    return chain


def format_chain_yaml(chain: list[dict], indent: int = 6) -> str:
    """Format chain as YAML.

    The intelligence score and context window are emitted as real fields,
    not just trailing comments: the gateway reads them for initial-model
    rotation and surfaces the context window as the logical model's
    `max_tokens` ceiling in /v1/models.
    """
    lines = []
    for entry in chain:
        lines.append(f"{' ' * indent}- provider: {entry['provider']}")
        lines.append(f"{' ' * (indent + 2)}model: {entry['model']}  {entry['comment']}")
        lines.append(f"{' ' * (indent + 2)}vision: {str(entry['vision']).lower()}")
        score = entry.get('intelligence')
        if score is not None:
            lines.append(f"{' ' * (indent + 2)}intelligence: {score:.1f}")
        ctx = entry.get('context_length')
        if ctx and ctx > 0:
            lines.append(f"{' ' * (indent + 2)}context_length: {ctx}")
    return '\n'.join(lines)


def load_preference_newest_first_on_tie(config_path: str) -> bool:
    """Read the `preferences.newest_first_on_tie` flag from config.yaml.

    Defaults to True when the section is absent so regeneration stays
    deterministic even on older configs.
    """
    try:
        with open(config_path) as f:
            text = f.read()
        m = re.search(r'(?ms)^preferences:\s*\n(.*?)(?=^\S|\Z)', text)
        if not m:
            return True
        block = m.group(1)
        m2 = re.search(r'(?m)^\s*newest_first_on_tie:\s*(true|false|yes|no|on|off)\s*$', block)
        if not m2:
            return True
        return m2.group(1).strip().lower() in ('true', 'yes', 'on')
    except OSError:
        return True


CONFIG_PATH = '/var/home/fra/dev/airouter/config.yaml'
DEFAULT_MODELS_PATH = '/tmp/free-models.json'

# Top-level keys that config.yaml owns. The generator replaces the `models:`
# block and must leave everything else byte-for-byte intact.
PROTECTED_TOP_LEVEL_KEYS = ('providers', 'preferences')


def find_models_section(config: str) -> tuple[int, int]:
    """Return (start, end) offsets of the top-level `models:` block.

    Anchoring on a bare `config.find('models:')` is wrong: config.yaml's own
    header comments contain the literal text `/v1/models:` (describing the
    max_context_tokens floor), which occurs around line 51, well before the real
    key near line 144. The old find() therefore treated that comment as the
    start of the block and emitted a 3 KB "header" — silently deleting the
    entire `providers:` section and every api_key_env entry, after which the
    daemon started with no provider configuration at all.

    Match the real key instead: `models:` at column 0, not inside a comment.
    """
    start = re.search(r'(?m)^models:[ \t]*$', config)
    if not start:
        raise SystemExit("Could not find a top-level 'models:' key in config.yaml")
    # The block ends at the next top-level key (or EOF).
    nxt = re.search(r'(?m)^[a-zA-Z][a-zA-Z0-9_-]*:[ \t]*$', config[start.end():])
    end = start.end() + (nxt.start() if nxt else len(config) - start.end())
    return start.start(), end


def merge_models_section(config: str, new_models: str) -> str:
    """Splice a freshly generated models block into the existing config.

    Everything before the models block (providers, comments, preferences that
    precede it) and everything after it is preserved verbatim.
    """
    start, end = find_models_section(config)
    header = config[:start].rstrip('\n')
    tail = config[end:].strip('\n')
    merged = header + '\n\n' + new_models.strip('\n') + '\n'
    if tail:
        merged += '\n' + tail + '\n'
    return merged


def validate_config_text(text: str) -> list[str]:
    """Return a list of problems with a candidate config; empty means OK."""
    import yaml  # local import: keeps the module importable without pyyaml
    problems: list[str] = []
    try:
        parsed = yaml.safe_load(text)
    except Exception as exc:  # noqa: BLE001 - report any parse failure
        return [f"YAML parse error: {exc}"]
    if not isinstance(parsed, dict):
        return ['config did not parse to a mapping']
    # Every provider the old config had must survive regeneration. Losing one
    # is the failure this function exists to prevent.
    providers = parsed.get('providers')
    if not isinstance(providers, dict) or not providers:
        problems.append('providers section missing or empty')
    else:
        for name, prov in providers.items():
            if not isinstance(prov, dict) or 'url' not in prov:
                problems.append(f'provider {name} has no url')
            # Losing api_key_env is how the 401 incident started: the daemon
            # starts fine, /v1/models answers, and every provider call fails at
            # runtime with nothing in the config to explain why. As fatal as a
            # missing url and just as invisible, so refuse to write without it.
            elif 'api_key_env' not in prov:
                problems.append(
                    f'provider {name} has no api_key_env (the daemon would start '
                    'with no credentials and every call would 401)'
                )
    for key in PROTECTED_TOP_LEVEL_KEYS:
        if key not in parsed:
            problems.append(f'top-level {key!r} key was lost')
    models = parsed.get('models')
    if not isinstance(models, dict) or not models:
        problems.append('models section missing or empty')
    else:
        for name, mc in models.items():
            chain = (mc or {}).get('chain') or []
            if not chain:
                problems.append(f'profile {name!r} has an empty chain')
    return problems


def write_config_atomically(
    config: str,
    new_config: str,
    write: bool,
    config_path: str | None = None,
) -> None:
    """Validate, back up, then atomically replace config.yaml.

    Safe by default: with write=False this only reports what would change, so a
    "looks like a dry run" invocation can never rewrite a hand-maintained file.
    `config_path` defaults to the module-level CONFIG_PATH, which tests
    monkeypatch.
    """
    target = config_path or CONFIG_PATH
    problems = validate_config_text(new_config)
    if problems:
        print('\n!!! REFUSING TO WRITE: regenerated config is invalid', file=sys.stderr)
        for p in problems:
            print(f'    - {p}', file=sys.stderr)
        raise SystemExit(1)

    before = validate_config_text(config)
    # A pre-existing problem is worth flagging but is not this tool's doing.
    if before:
        print('warning: existing config.yaml already has issues:', file=sys.stderr)
        for p in before:
            print(f'    - {p}', file=sys.stderr)

    if new_config == config:
        print('\nconfig.yaml already up to date (no write needed)')
        return

    diff = sum(1 for a, b in zip(config.splitlines(), new_config.splitlines()) if a != b)
    print(f'\nconfig.yaml would change (~{diff} differing lines)')

    if not write:
        print('DRY RUN — nothing written. Re-run with --write to apply.')
        return

    backup = f'{target}.bak'
    shutil.copyfile(target, backup)
    print(f'WRITING {target} (backup: {backup})')

    # Write to a sibling temp file then rename, so a crash or a full disk
    # cannot leave a truncated config behind. os.replace is atomic within a
    # filesystem, which a plain open(path, 'w') is not.
    tmp = f'{target}.tmp.{os.getpid()}'
    try:
        with open(tmp, 'w') as f:
            f.write(new_config)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, target)
    except Exception:
        if os.path.exists(tmp):
            os.unlink(tmp)
        raise
    print(f'wrote {target} ({len(new_config)} bytes)')


def build_parser() -> argparse.ArgumentParser:
    """CLI surface.

    Paths are flags so the whole regeneration can be driven from a fixture (a
    saved model list, a scratch config) instead of only from /tmp and the live
    file. Hardcoding them made the terminator ladder untestable through the CLI,
    which is where it actually runs. argparse rather than a hand-rolled scan
    because the scan silently accepted a typo'd flag and exited 0 having changed
    nothing, which is indistinguishable from "already up to date".
    """
    ap = argparse.ArgumentParser(
        prog='regenerate_config.py',
        description='Regenerate the models: section of config.yaml from a '
                    'fetched free-model list.',
        # No abbreviations. `--model` is an unambiguous prefix of `--models`, so
        # argparse would silently accept the typo and read a path the caller
        # never meant to name. A typo has to be loud here: the alternative is a
        # sync that quietly regenerates from the wrong input, or does nothing and
        # exits 0 looking like "already up to date".
        allow_abbrev=False,
    )
    ap.add_argument(
        '--models', default=DEFAULT_MODELS_PATH, metavar='FILE',
        help=f'fetched model list (default: {DEFAULT_MODELS_PATH})',
    )
    ap.add_argument(
        '--config', default=CONFIG_PATH, metavar='FILE',
        help=f'config.yaml to rewrite (default: {CONFIG_PATH})',
    )
    ap.add_argument(
        '--write', action='store_true',
        help='apply the change; without it this is a dry run',
    )
    return ap


def main(argv: list[str] | None = None):
    argv = sys.argv[1:] if argv is None else argv
    args = build_parser().parse_args(argv)
    models_path, config_path = args.models, args.config

    models = load_models(models_path)
    unique = get_unique_models(models)
    model_list = list(unique.values())

    # Read the existing config up front: the tie-break preference comes from
    # it, and so do the current terminator comments (see below).
    with open(config_path) as f:
        config = f.read()

    print(f"Total unique models (after exclusions): {len(model_list)}")

    # Read the configured tie-break preference from config.yaml.
    newest_first = load_preference_newest_first_on_tie(config_path)
    print(f"Preference newest_first_on_tie: {newest_first}")

    # Filter for each profile
    smart_models = filter_smart(model_list)
    work_models = filter_work(model_list)
    fast_models = filter_fast(model_list)
    large_models = filter_large(model_list)

    # Sort each, applying the configured tie-break.
    smart_models = sort_models(smart_models, newest_first)
    work_models = sort_models(work_models, newest_first)
    fast_models = sort_models(fast_models, newest_first)
    # large: primary key is context_length desc, then intelligence desc,
    # then newest-first on the intelligence tie.
    large_models = sort_models(
        large_models,
        newest_first,
        context_first=True,
    )
    
    print(f"\nSmart candidates ({len(smart_models)}):")
    for m in smart_models:
        print(f"  {m.mapped_provider:12s} {m.id:50s} score={m.score} ctx={m.context_length} vision={m.vision}")
    
    print(f"\nWork candidates ({len(work_models)}):")
    for m in work_models:
        print(f"  {m.mapped_provider:12s} {m.id:50s} score={m.score} ctx={m.context_length} vision={m.vision}")
    
    print(f"\nFast candidates ({len(fast_models)}):")
    for m in fast_models:
        print(f"  {m.mapped_provider:12s} {m.id:50s} score={m.score} ctx={m.context_length} vision={m.vision}")
    
    print(f"\nLarge candidates ({len(large_models)}):")
    for m in large_models:
        print(f"  {m.mapped_provider:12s} {m.id:50s} score={m.score} ctx={m.context_length} vision={m.vision}")
    
    # Build chains. The full catalog is passed so the terminator can be checked
    # against the fetcher's probe verdict, not just counted.
    records = {(m.mapped_provider, m.id): m for m in model_list}
    # The current terminators, so a run that cannot verify a verdict keeps the
    # comment the file already carries instead of replacing a recorded claim
    # with a different one. See auto_fallback_terminator.
    previous = parse_terminators(config)
    smart_chain = build_chain(smart_models, 'smart', records, previous.get('smart'))
    work_chain = build_chain(work_models, 'work', records, previous.get('work'))
    fast_chain = build_chain(fast_models, 'fast', records, previous.get('fast'))
    large_chain = build_chain(large_models, 'large', records, previous.get('large'))
    
    # Generate new models section
    new_models = f"""models:
  smart:
    chain:
{format_chain_yaml(smart_chain)}
  work:
    chain:
{format_chain_yaml(work_chain)}
  fast:
    chain:
{format_chain_yaml(fast_chain)}
  large:
    chain:
{format_chain_yaml(large_chain)}
"""
    
    # Replace the models section, preserving everything else byte-for-byte.
    new_config = merge_models_section(config, new_models)

    print("\n=== SUMMARY ===")
    for name, chain in [('smart', smart_chain), ('work', work_chain), ('fast', fast_chain), ('large', large_chain)]:
        concrete = [e for e in chain if not e['model'].startswith('kilo-auto') and e['model'] != 'big-pickle']
        print(f"{name:6s}: {len(chain)} entries ({len(concrete)} concrete), head={concrete[0]['model'] if concrete else 'N/A'}, tail={chain[-1]['model']}")

    write_config_atomically(config, new_config, args.write, config_path)

if __name__ == '__main__':
    main()