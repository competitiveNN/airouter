#!/usr/bin/env python3
"""
Regenerate the models section in config.yaml from free-models.json
"""

import json
import re
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

    def get_comment(self) -> str:
        """Generate trailing comment with score and context"""
        parts = []
        if self.score is not None:
            parts.append(f"{self.score:.1f}")
            if self.intelligence_source == "smart floor":
                parts.append("floor")
        if self.elo is not None:
            parts.append(f"elo={int(self.elo)}")
        if self.context_length and self.context_length > 0:
            ctx = f"{self.context_length:,}"
            parts.append(f"{ctx} ctx")
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
    return [m for m in models if m.score is not None and m.score >= 25]


def filter_work(models: list[Model]) -> list[Model]:
    """work: coding workhorses. Same pool as smart plus dedicated code models (e.g. cohere/north-mini-code). Drop tiny models (intelligence < 15).

    'Same pool as smart' means the broader eligible pool (all non-null-intelligence,
    non-excluded, non-auto-fallback models), extending down to intelligence >= 15 to
    include more workhorse models. Dedicated code models (e.g. cohere/north-mini-code)
    are included when they meet the >= 15 threshold.
    """
    return [m for m in models
            if m.score is not None and m.score >= 15
            and not m.is_excluded and not m.is_auto_fallback]


def filter_fast(models: list[Model]) -> list[Model]:
    """fast: small/cheap models only: intelligence < 25, or ids containing flash-lite / lightning / nano / gemma / lfm / laguna-xs. Cap at 10 entries."""
    fast_keywords = ['flash-lite', 'lightning', 'nano', 'gemma', 'lfm', 'laguna-xs']
    candidates = []
    for m in models:
        if m.score is None:
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
                  and not m.is_auto_fallback]
    candidates.sort(key=lambda x: (x.context_length, x.score or 0), reverse=True)
    return candidates


def build_chain(models: list[Model], profile: str) -> list[dict]:
    """Build fallback chain for a profile"""
    chain = []
    kilocode_count = 0
    opencode_count = 0
    
    for m in models:
        if m.mapped_provider == 'nvidia':
            # Emit trio: nvidia, nvidia2, nvidia3
            for prov in ['nvidia', 'nvidia2', 'nvidia3']:
                chain.append({
                    'provider': prov,
                    'model': m.id,
                    'vision': m.vision,
                    'comment': m.get_comment()
                })
            if m.provider == 'kilocode':
                kilocode_count += 3
            elif m.provider == 'opencode':
                opencode_count += 3
        elif m.mapped_provider == 'commandcode':
            # Emit pair: commandcode, commandcode2 (two API keys on the same
            # upstream proxy) so a rate-limited key falls through to the next.
            for prov in ['commandcode', 'commandcode2']:
                chain.append({
                    'provider': prov,
                    'model': m.id,
                    'vision': m.vision,
                    'comment': m.get_comment()
                })
            if m.provider == 'kilocode':
                kilocode_count += 2
            elif m.provider == 'opencode':
                opencode_count += 2
        else:
            chain.append({
                'provider': m.mapped_provider,
                'model': m.id,
                'vision': m.vision,
                'comment': m.get_comment()
            })
            if m.provider == 'kilocode':
                kilocode_count += 1
            elif m.provider == 'opencode':
                opencode_count += 1
    
    # Determine auto fallback
    if kilocode_count >= opencode_count:  # ties go to kilocode
        auto_provider = 'kilocode'
        auto_model = 'kilo-auto/free'
        auto_comment = f"# last: kilocode dominates ({kilocode_count} vs {opencode_count})"
    else:
        auto_provider = 'opencode'
        auto_model = 'big-pickle'
        auto_comment = f"# last: opencode dominates ({opencode_count} vs {kilocode_count})"
    
    # Add auto fallback with vision: true
    chain.append({
        'provider': auto_provider,
        'model': auto_model,
        'vision': True,
        'comment': auto_comment
    })
    
    return chain


def format_chain_yaml(chain: list[dict], indent: int = 6) -> str:
    """Format chain as YAML"""
    lines = []
    for entry in chain:
        lines.append(f"{' ' * indent}- provider: {entry['provider']}")
        lines.append(f"{' ' * (indent + 2)}model: {entry['model']}  {entry['comment']}")
        lines.append(f"{' ' * (indent + 2)}vision: {str(entry['vision']).lower()}")
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


def main():
    models = load_models('/tmp/free-models.json')
    unique = get_unique_models(models)
    model_list = list(unique.values())

    print(f"Total unique models (after exclusions): {len(model_list)}")

    # Read the configured tie-break preference from config.yaml.
    newest_first = load_preference_newest_first_on_tie(
        '/var/home/fra/dev/airouter/config.yaml'
    )
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
    
    # Build chains
    smart_chain = build_chain(smart_models, 'smart')
    work_chain = build_chain(work_models, 'work')
    fast_chain = build_chain(fast_models, 'fast')
    large_chain = build_chain(large_models, 'large')
    
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
    
    # Read current config and replace models section
    with open('/var/home/fra/dev/airouter/config.yaml', 'r') as f:
        config = f.read()

    # Find where the models section begins.  The header may end with one or
    # more blank lines; normalize to exactly one blank line before `models:`
    # so regeneration is idempotent (re-running never grows the file).
    models_start = config.find('models:')
    if models_start == -1:
        raise SystemExit("Could not find 'models:' section in config.yaml")
    # Preserve everything after the models section verbatim (e.g. the
    # `test:` block that follows `large:`), so regeneration never drops it.
    header = config[:models_start].rstrip('\n')
    new_config = header + '\n\n' + new_models
    # Re-append any trailing top-level keys that were present in the
    # original config after `models:` (e.g. the `test:` block).
    rest = config[models_start + len('models:'):]
    m = re.search(r'(?m)^([a-zA-Z][a-zA-Z0-9_-]*):', rest)
    if m:
        new_config += '\n' + rest[m.start():]
    
    with open('/var/home/fra/dev/airouter/config.yaml', 'w') as f:
        f.write(new_config)
    
    print("\n=== SUMMARY ===")
    for name, chain in [('smart', smart_chain), ('work', work_chain), ('fast', fast_chain), ('large', large_chain)]:
        concrete = [e for e in chain if not e['model'].startswith('kilo-auto') and e['model'] != 'big-pickle']
        print(f"{name:6s}: {len(chain)} entries ({len(concrete)} concrete), head={concrete[0]['model'] if concrete else 'N/A'}, tail={chain[-1]['model']}")

if __name__ == '__main__':
    main()