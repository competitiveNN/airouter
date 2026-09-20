#!/usr/bin/env python3
"""
Regenerate the models section in config.yaml from free-models.json
"""

import json
import re
from dataclasses import dataclass
from typing import Any

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

    @property
    def score(self) -> float | None:
        """Effective intelligence score (AA, Arena ELO, or the smart floor)."""
        return self.intelligence

    @property
    def mapped_provider(self) -> str | None:
        """Map JSON provider to config provider"""
        mapping = {
            'kilocode': 'kilocode',
            'opencode': 'opencode',
            'ollama-cloud': 'ollama',
            'google-ai-studio': 'gemini',
            'nvidia-nim': 'nvidia',  # will be expanded to trio
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
        if 'stealth' in id_lower:
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
            raw=item.get('raw', {})
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


def main():
    models = load_models('/tmp/free-models.json')
    unique = get_unique_models(models)
    model_list = list(unique.values())
    
    print(f"Total unique models (after exclusions): {len(model_list)}")
    
    # Filter for each profile
    smart_models = filter_smart(model_list)
    work_models = filter_work(model_list)
    fast_models = filter_fast(model_list)
    large_models = filter_large(model_list)
    
    # Sort each
    smart_models.sort(key=lambda x: x.score or 0, reverse=True)
    work_models.sort(key=lambda x: x.score or 0, reverse=True)
    fast_models.sort(key=lambda x: x.score or 0, reverse=True)
    large_models.sort(key=lambda x: (x.context_length, x.score or 0), reverse=True)
    
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
    header = config[:models_start].rstrip('\n')
    new_config = header + '\n\n' + new_models
    
    with open('/var/home/fra/dev/airouter/config.yaml', 'w') as f:
        f.write(new_config)
    
    print("\n=== SUMMARY ===")
    for name, chain in [('smart', smart_chain), ('work', work_chain), ('fast', fast_chain), ('large', large_chain)]:
        concrete = [e for e in chain if not e['model'].startswith('kilo-auto') and e['model'] != 'big-pickle']
        print(f"{name:6s}: {len(chain)} entries ({len(concrete)} concrete), head={concrete[0]['model'] if concrete else 'N/A'}, tail={chain[-1]['model']}")

if __name__ == '__main__':
    main()