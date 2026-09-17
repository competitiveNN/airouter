"""Shared utilities for model fetching and assignment: caching, hide list, and slug matching."""

import json
import re
from datetime import UTC, datetime
from difflib import SequenceMatcher
from pathlib import Path
from typing import Any

# Cache for AA enrichment data (intelligence scores + release dates).
# Avoids redundant API calls when the data is still fresh.
CACHE_FILE = Path(__file__).resolve().parent / "free-models-cache.json"
CACHE_TTL_HOURS = 24

# Models to hide — specialized, outdated, or low-quality models
# that we would never use in assignments.
HIDE_MODELS: set[str] = {
    # Embedding / retrieval models
    "nvidia/nv-embed-v1",
    "nvidia/nv-embed-v2",
    "nvidia/nemoretriever-1b",
    "nvidia/nemoretriever-4b",
    "nvidia/nemoguard-3b",
    "nvidia/nemoguard-8b",
    # Vision-only models (not useful for text auxiliary tasks)
    "nvidia/neva-22b",
    "nvidia/vila",
    "microsoft/kosmos-2",
    "adept/fuyu-8b",
    # Outdated / superseded models with low intelligence
    "nvidia/mistral-nemo-minitron-8b-8k-instruct",
    "nvidia/nemotron-mini-4b-instruct",
    # Translation / speech models
    "nvidia/translate",
    "nvidia/tts",
    # Code models that are too specialized or outdated
    "mistralai/codestral-22b-instruct-v0.1",
    "google/codegemma-7b",
    "google/codegemma-1.1-7b",
    # Too old (released >12 months ago with no recent update)
    "nvidia/llama-3.1-nemotron-51b-instruct",
    "nvidia/nemotron-4-340b-instruct",
    "nvidia/llama-3.3-nemotron-super-49b-v1",
    "nvidia/llama-3.3-nemotron-super-49b-v1.5",
    # Low-intelligence specialized models
    "nvidia/nemotron-3.5-content-safety:free",
    "nvidia/nemotron-3-nano-omni-30b-a3b-reasoning:free",
    # Writer/Palmyra — creative/financial models, not generalist coding
    "writer/palmyra-creative-122b",
    "writer/palmyra-fin-70b-32k",
    "writer/palmyra-med-70b",
    "writer/palmyra-med-70b-32k",
}

# Minimum intelligence score for a model to be considered usable without recency check.
MIN_INTELLIGENCE_SCORE = 10.0

# Maximum model age in months for models without an explicit tier.
MAX_MODEL_AGE_MONTHS = 12


def load_cache() -> dict[str, Any] | None:
    """Load cached AA enrichment data if it exists and is still fresh."""
    if not CACHE_FILE.exists():
        return None
    try:
        data = json.loads(CACHE_FILE.read_text())
        fetched_at = datetime.fromisoformat(data.get("fetched_at", "").replace("Z", "+00:00"))
        age = datetime.now(UTC) - fetched_at
        if age.total_seconds() > CACHE_TTL_HOURS * 3600:
            return None
        enrichment = data.get("enrichment", {})
        return enrichment if isinstance(enrichment, dict) else None
    except (json.JSONDecodeError, ValueError, OSError):
        return None


def save_cache(enrichment: dict[str, Any]) -> None:
    """Save AA enrichment data to cache with current timestamp."""
    cache = {
        "fetched_at": datetime.now(UTC).isoformat().replace("+00:00", "Z"),
        "enrichment": enrichment,
    }
    CACHE_FILE.write_text(json.dumps(cache, indent=2))


def is_model_hidden(
    model_id: str,
    intelligence: float | None,
    released: str | None,
    provider: str | None = None,
) -> bool:
    """Check if a model should be hidden based on the hide list and quality thresholds.

    Only applies filtering for nvidia-nim models. All other providers
    (kilocode, opencode, ollama-cloud, google-ai-studio) always display
    all free models without filtering.
    """
    if provider != "nvidia-nim":
        return False
    if model_id in HIDE_MODELS:
        return True
    # If we have an intelligence score, use it as primary filter
    if intelligence is not None:
        if intelligence < MIN_INTELLIGENCE_SCORE:
            return True
        # High intelligence models are kept regardless of age
        return False
    # No intelligence score available - filter by recency
    if released and MAX_MODEL_AGE_MONTHS > 0:
        try:
            # Handle both "YYYY-MM" string format and Unix timestamp (int)
            if isinstance(released, int):
                rel_date = datetime.fromtimestamp(released, tz=UTC)
            else:
                rel_year, rel_month = released.split("-")
                rel_date = datetime(int(rel_year), int(rel_month), 1, tzinfo=UTC)
            age_months = (datetime.now(UTC) - rel_date).days / 30.44
            if age_months > MAX_MODEL_AGE_MONTHS:
                return True
        except (ValueError, IndexError, OSError):
            pass
    return False





# ── Slug normalization & fuzzy matching ────────────────────────────────────────

# Variant suffixes that carry no identity (fine-tune/chat variants of the same
# model). Stripped repeatedly so "-it-instruct" style stacks collapse too.
_VARIANT_SUFFIXES = ("-chat", "-instruct", "-it", "-free", "-latest", "-max", "-high")

# Tokens marking opaque/stealth placeholder ids whose real identity is unknown
# (e.g. "x-preview-f-free"). These must never be fuzzy-matched.
_OPAQUE_TOKEN_RE = re.compile(r"(?:^|-)(?:x|stealth|preview)(?:-|$)")

# Folded-alnum length below which fuzzy/prefix matching is too ambiguous.
_MIN_FUZZY_LEN = 8
# Similarity required to accept a difflib match (was 0.7 — far too loose:
# it matched "x-preview-f" -> "o1-preview").
_FUZZY_CUTOFF = 0.85
# If the top two candidates are closer than this, the match is ambiguous.
_AMBIGUITY_MARGIN = 0.02


def _alnum_fold(s: str) -> str:
    """Fold a normalized slug to bare alphanumeric characters."""
    return re.sub(r"[^a-z0-9]", "", s.lower())


def _is_subsequence(needle: str, haystack: str) -> bool:
    """True if needle's characters appear in haystack in order (insertions only).

    Rejects character SUBSTITUTIONS, which is what makes difflib alone unsafe:
    "gemma49b" is not a subsequence of "gemma34b" (generation swap), while
    "qwen3397ba17b" IS a subsequence of "qwen35397ba17b" (point release).
    """
    it = iter(haystack)
    return all(ch in it for ch in needle)


def _is_version_only_diff(shorter: str, longer: str) -> bool:
    """True if longer differs from shorter only by digit insertions adjacent
    to existing digits — i.e. a version-number difference (n2 vs n2.5).

    This prevents the fuzzy matcher from conflating different model versions
    that share the same base name (e.g. "nexn2mini" vs "nexn25mini").
    """
    if len(longer) <= len(shorter):
        return False
    # Walk both strings; every extra char in longer must be a digit
    # adjacent to an existing digit in shorter.
    i = j = 0
    while i < len(shorter) and j < len(longer):
        if shorter[i] == longer[j]:
            i += 1
            j += 1
        elif longer[j].isdigit() and (i > 0 and shorter[i - 1].isdigit()):
            j += 1  # version digit inserted
        else:
            return False
    # Remaining chars in longer must all be digits adjacent to digits
    while j < len(longer):
        if not (longer[j].isdigit() and i > 0 and shorter[i - 1].isdigit()):
            return False
        j += 1
    return True


def normalize_slug(slug: str) -> str:
    """Normalize a model slug for exact matching against AA API slugs.

    Lowercases, strips the org prefix ("org/model"), replaces dots with
    hyphens, strips variant suffixes (-chat, -instruct, -it, -free, ...) and
    cleans up leftover separators.

    Parameter sizes are deliberately KEPT: "gemma-4-31b" and "gemma-4-12b"
    are different models with different intelligence scores and must never
    collapse onto one key (the old weight-stripping behavior caused exactly
    that collision).
    """
    slug = slug.lower().strip()
    if "/" in slug:
        slug = slug.split("/", 1)[1]
    slug = slug.replace(".", "-")
    # Strip :free suffix used by Kilo / OpenCode
    if slug.endswith(":free"):
        slug = slug[:-5]
    changed = True
    while changed and slug:
        changed = False
        for suffix in _VARIANT_SUFFIXES:
            if slug.endswith(suffix):
                slug = slug[: -len(suffix)]
                changed = True
                break
    # Clean up repeated/leading/trailing separators left behind
    slug = re.sub(r"-+", "-", slug).strip("-_: .")
    return slug


def fuzzy_match_slug(model_id: str, aa_slugs: dict[str, Any]) -> str | None:
    """Find the AA slug for a model ID using strict, conservative matching.

    Matching tiers, most to least trusted:
      1. Exact normalized match (sizes preserved).
      2. Exact separator-folded match ("gemma4:31b" == "gemma-4-31b").
      3. Contiguous containment: the shorter folded slug appears verbatim
         inside the longer one, so every extra character sits at the edges
         (brand prefixes like "nvidia-", size/variant suffixes like
         "-0731", "-contributor"). Least decoration wins; ties refuse.
      4. difflib similarity >= 0.85 restricted to pure insertion/deletion
         edits in either direction (no substitutions), minimum length 8,
         unambiguous winner, and never for opaque/stealth ids.

    Returns None rather than guessing: a wrong intelligence score is worse
    than no score.

    Args:
      model_id: The model ID from the provider API (e.g. "qwen/qwen3.5-397b-a17b").
      aa_slugs: Dict mapping AA slugs to their enrichment data.

    Returns:
      The best matching AA slug, or None if no confident match exists.
    """
    norm_id = normalize_slug(model_id)
    if not norm_id:
        return None

    aa_normalized: dict[str, str] = {}
    for slug in aa_slugs:
        aa_normalized.setdefault(normalize_slug(slug), slug)

    # Tier 1: exact normalized match
    hit = aa_normalized.get(norm_id)
    if hit:
        return hit

    # Tier 2: exact match ignoring all separators
    folded_id = _alnum_fold(norm_id)
    folded_map: dict[str, str] = {}
    for norm, slug in aa_normalized.items():
        folded_map.setdefault(_alnum_fold(norm), slug)
    hit = folded_map.get(folded_id)
    if hit:
        return hit

    # Guards shared by the approximate tiers
    if len(folded_id) < _MIN_FUZZY_LEN or _OPAQUE_TOKEN_RE.search(norm_id):
        return None

    # Tier 3: contiguous containment with edge-only decorations.
    # Verbatim-contiguous matching is what makes this safe: "nemotron3super"
    # sits intact inside "nvidianemotron3super120ba12b", while lookalikes
    # ("o3" inside "nemotron...") and mid-name splices ("omni") cannot
    # qualify because the core is broken across the insertion.
    decorated: list[tuple[int, str]] = []
    for fold, slug in folded_map.items():
        shorter, longer = (
            (fold, folded_id) if len(fold) <= len(folded_id) else (folded_id, fold)
        )
        if len(shorter) < 5:
            continue  # too generic to anchor a containment match
        if longer.find(shorter) == -1:
            continue
        decorated.append((len(longer) - len(shorter), slug))
    if decorated:
        decorated.sort()
        if len(decorated) == 1 or decorated[0][0] != decorated[1][0]:
            return decorated[0][1]
        return None

    # Tier 4: strict difflib similarity, insertions/deletions only
    scored: list[tuple[float, str]] = []
    for norm, slug in aa_normalized.items():
        fold = _alnum_fold(norm)
        ratio = SequenceMatcher(None, folded_id, fold).ratio()
        if ratio >= _FUZZY_CUTOFF and (
            _is_subsequence(folded_id, fold) or _is_subsequence(fold, folded_id)
        ):
            # Reject when the only difference is digit insertions adjacent
            # to existing digits — that is a version-number difference
            # (n2 vs n2.5), not a variant of the same model.
            shorter, longer = (
                (fold, folded_id) if len(fold) <= len(folded_id) else (folded_id, fold)
            )
            if _is_version_only_diff(shorter, longer):
                continue
            scored.append((ratio, slug))
    if not scored:
        return None
    scored.sort(reverse=True)
    if len(scored) > 1 and scored[0][0] - scored[1][0] < _AMBIGUITY_MARGIN:
        return None
    return scored[0][1]
