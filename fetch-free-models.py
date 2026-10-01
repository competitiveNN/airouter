#!/usr/bin/env python3
"""
Fetch currently available free models from Kilo Code, OpenCode, Ollama Cloud, Google AI Studio, and NVIDIA NIM APIs.

Usage:
    python fetch-free-models.py --table
    python fetch-free-models.py --json --save free-models.json
    python fetch-free-models.py --csv --save free-models.csv
    python fetch-free-models.py --kilocode-only --table
    python fetch-free-models.py --opencode-only --json
    python fetch-free-models.py --ollama-only --table
    python fetch-free-models.py --google-ai-studio-only --table
    python fetch-free-models.py --nvidia-nim-only --table

Ollama Cloud free-tier models (hard-coded — there is no public endpoint
that exposes the free/metered flag):
    gemma4:31b, nemotron-3-super, nemotron-3-ultra, minimax-m3
Anything else returned by ollama.com/v1/models is treated as metered.
"""

import argparse
import csv
import json
import os
import re
import sys
import time
import urllib.error
import urllib.request
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from model_utils import (  # noqa: F401
    CACHE_FILE,
    CACHE_TTL_HOURS,
    HIDE_MODELS,
    fuzzy_match_slug,
    is_model_hidden,
    load_cache,
    normalize_slug,
    save_cache,
)

# ── Configuration ──────────────────────────────────────────────────────────────
KILO_ENDPOINT = "https://api.kilo.ai/api/gateway/v1/models"
OPENCODE_ENDPOINT = "https://opencode.ai/zen/v1"
OPENCODE_MODELS_URL = OPENCODE_ENDPOINT + "/models"
GOOGLE_AI_STUDIO_ENDPOINT = "https://generativelanguage.googleapis.com/v1beta/models"
NVIDIA_NIM_ENDPOINT = "https://integrate.api.nvidia.com/v1/models"
COMMANDCODE_ENDPOINT = "https://api.commandcode.ai/provider/v1/models"
ARTIFICIAL_ANALYSIS_ENDPOINT = "https://artificialanalysis.ai/api/v2/data/llms/models"
# arena.ai does not publish a public API for its leaderboard; this mirrors the
# exact snapshot archived at the repo below (the code-arena leaderboard at
# https://arena.ai/leaderboard/code). ELO is used as a FALLBACK intelligence
# signal when Artificial Analysis has no score for a model.
ARENA_CODE_LEADERBOARD = "https://arena.ai/leaderboard/code"
ARENA_CODE_LATEST_URL = (
    "https://raw.githubusercontent.com/oolong-tea-2026/arena-ai-leaderboards/main/data/latest.json"
)
ARENA_CODE_DATA_URL = (
    "https://raw.githubusercontent.com/oolong-tea-2026/arena-ai-leaderboards/main/data/{date}/code.json"
)
ARENA_CACHE_FILE = Path(__file__).resolve().parent / "arena-code-cache.json"
ARENA_CACHE_TTL_HOURS = 24
# arena code-ELO band (~1440-1700) is mapped onto the AA intelligence scale
# (~0-63) so the two signals are comparable when ELO is used as a fallback.
# 1200 == ELO baseline (random), 1700 ~= AA ceiling (63).
ELO_BASE = 1200.0
ELO_TO_INTELLIGENCE = 63.0 / 500.0
# Models with no Artificial Analysis intelligence score AND no arena.ai
# code-leaderboard ELO receive the "smart floor" (25.0) -- the lowest value
# that still lets them enter the smart/work/large chains. This is a
# deterministic default, not a hand-picked override: it keeps unscored models
# visible in the chains without claiming a benchmark score they never earned.
SMART_FLOOR = 25.0

# First-seen cache: when a model appears in the free-models list for the
# first time, we record the fetch date as its "release date" fallback.  The
# upstream APIs (OpenCode, CommandCode, Kilo Code) return a single
# placeholder `created` timestamp for every model rather than the actual
# model release date, and Artificial Analysis does not always have a
# release date for every model either.  Without this cache, models with no
# AA date would have `released=None` and would sort to the bottom of every
# tie band, making them permanently appear "oldest" -- which is wrong when
# they are genuinely new models that simply haven't been scored yet.
#
# The cache key is the model id; the value is the ISO date string of the
# first run that observed the model.  Once recorded, the date never changes
# for that model (it is the model's true first-appearance date in this
# pipeline, not a moving target).
FIRST_SEEN_CACHE_FILE = Path(__file__).resolve().parent / "first-seen-cache.json"
FIRST_SEEN_TTL_HOURS = 24 * 365  # effectively permanent; only pruned manually


def load_first_seen_cache() -> dict[str, str]:
    """Load the first-seen cache, or return {} if absent/unreadable.

    The cache file is a JSON object with two keys:
      - ``fetched_at``: ISO-8601 timestamp of the last save
      - ``first_seen``: the actual ``{model_id: date_str}`` map
    """
    if not FIRST_SEEN_CACHE_FILE.exists():
        return {}
    try:
        data = json.loads(FIRST_SEEN_CACHE_FILE.read_text())
        if not isinstance(data, dict):
            return {}
        inner = data.get("first_seen")
        if not isinstance(inner, dict):
            return {}
        return {str(k): str(v) for k, v in inner.items()}
    except (json.JSONDecodeError, OSError):
        return {}


def save_first_seen_cache(cache: dict[str, str]) -> None:
    """Persist the first-seen cache with a timestamp for debugging."""
    payload = {
        "fetched_at": datetime.now(UTC).isoformat().replace("+00:00", "Z"),
        "first_seen": cache,
    }
    FIRST_SEEN_CACHE_FILE.write_text(json.dumps(payload, indent=2, sort_keys=True))


# Meta-router auto-fallback models. They are never real candidates, so they
# stay unscored (null intelligence) and only ever appear as the trailing
# chain entry -- never floored into the concrete pool.
AUTO_FALLBACK_MODELS = {"kilo-auto/free", "big-pickle"}

# Models that are unscored (no AA intelligence, no arena ELO) but are NOT
# usable routing candidates. They are excluded BEFORE the smart floor is
# applied so they never enter the chains. Each entry has a concrete reason;
# this is a curated negative list, not a score threshold:
#   - meta-routers (openrouter/free): selects random models, not a model
#   - guardrail / content-safety models: not general-purpose coding
#   - opaque zero-metadata models: no name/description/context, unknown
#     quality, and no way to review them
#   - small instruction models (gemma-4-9b-it): a 9B model floored at 25.0
#     would sit ahead of genuinely scored 24.x models, misrepresenting it as
#     smart-tier; it is a legitimate free model but not a floor candidate
UNSCORED_EXCLUDE: set[str] = {
    "openrouter/free",
    "nvidia/nemotron-3.5-content-safety:free",
    "jev-1.13-free",
    "mimo-v2.5-free",
    "meta/llama2-70b",
    "nvidia/llama-3.1-nemotron-70b-instruct",
    "deepseek-ai/deepseek-coder-6.7b-instruct",
    "ibm/granite-3.0-3b-a800m-instruct",
    "ibm/granite-3.0-8b-instruct",
    "poolside/laguna-xs-2.1",
    "gemma-4-9b-it",
}
TIMEOUT = 30
MAX_RETRIES = 3
BACKOFF_BASE = 2

# Authoritative free-tier list for Ollama Cloud (curated by user).  Ollama
# does not expose this flag in /v1/models or any chat-completions header,
# so the only reliable signal is a user-curated marklist.  Anything NOT
# in this set is treated as METERED — we never silently route to a paid
# model.  Update this set when Ollama adds new free tiers.
OLLAMA_FREE_MODELS: set[str] = {
    "gemma4:31b",
    "nemotron-3-super",
    "nemotron-3-ultra",
    "minimax-m3",
}

# Google AI Studio free model patterns (prefix match).
# Models matching these prefixes are considered free tier.
# Pro/Ultra models and image/video/audio generation models are NOT free.
# For gemini-* family, ONLY keep *-latest versions (handled in fetch function).
GOOGLE_AI_STUDIO_FREE_PREFIXES: tuple[str, ...] = (
    # Gemma models (open weights) - keep all
    "gemma-4-",
    # Flash experimental/preview - keep *-latest only (filtered in fetch function)
    # NOTE: gemini-flash-latest is blacklisted (see GOOGLE_AI_STUDIO_FREE_MODELS)
    "gemini-flash-lite-latest",
)

# *-latest aliases are opaque to the AA index and must never be matched by
# name similarity ("gemini-flash-latest" used to inherit gemini-3-flash's
# score while actually aliasing the newest generation).  Each alias resolves
# through the v1beta/models endpoint to the newest STABLE numbered version
# (no preview/experimental/tts/image/audio variants) and matches AA via that.
_GOOGLE_VERSION = r"(\d+(?:\.\d+)*)"
GOOGLE_LATEST_ALIASES: dict[str, re.Pattern[str]] = {
    "gemini-flash-lite-latest": re.compile(rf"^gemini-{_GOOGLE_VERSION}-flash-lite(?:-\d+)?$"),
}

# Offline fallback when v1beta/models is unreachable — bump manually when
# Google ships a new generation (same philosophy as the curated list below).
GOOGLE_LATEST_FALLBACK: dict[str, str] = {
    "gemini-flash-lite-latest": "gemini-3.5-flash-lite",
}

# Models that are explicitly NOT free (paid/enterprise)
GOOGLE_AI_STUDIO_PAID_PATTERNS: tuple[str, ...] = (
    "pro",
    "ultra",
    "imagen",
    "veo",
    "lyria",
    "embedding",
    "aqa",
    "robotics",
    "deep-research",
    "antigravity",
    "computer-use",
    "tts",
    "nano-banana",
    "image",
    "video",
    "audio",
)

# Context-length marklists.  Ollama's /v1/models does not expose
# context_length, OpenCode's does not either.  Populate from live lookups
# so the ctx column is never 0 in the table.
OLLAMA_FREE_MODELS_CTX: dict[str, int] = {
    "gemma4:31b":       262_144,
    "minimax-m3":       262_144,
    "nemotron-3-super": 262_144,
    "nemotron-3-ultra": 1_000_000,
}

OPENCODE_FREE_MODELS_CTX: dict[str, int] = {
    "deepseek-v4-flash-free":   131_072,
    "laguna-s-2.1-free":        131_072,
    "ling-3.0-flash-free":      131_072,
    "mimo-v2.5-free":           131_072,
    "nemotron-3-ultra-free":  1_000_000,
    "north-mini-code-free":     131_072,
    "big-pickle":               131_072,
}

# OpenCode models that are free but do NOT carry the "-free" suffix.
# The API exposes no free flag, so these must be user-curated.  Add new
# free-but-un-suffixed models here.  Anything else is treated as METERED.
OPENCODE_FREE_MODELS: set[str] = {
    "big-pickle",
}

GOOGLE_AI_STUDIO_FREE_MODELS_CTX: dict[str, int] = {
    "gemini-flash-lite-latest":   1_048_576,
    "gemma-4-31b-it":             262_144,
    "gemma-4-26b-a4b-it":         262_144,
    "gemma-4-9b-it":              262_144,
    "gemma-4-2b-it":              262_144,
}

# Curated free model list for Google AI Studio (used when API key not available)
GOOGLE_AI_STUDIO_FREE_MODELS: set[str] = {
    # gemini-flash-latest is BLACKLISTED (intentionally excluded)
    "gemini-flash-lite-latest",
    "gemma-4-31b-it",
    "gemma-4-26b-a4b-it",
    "gemma-4-9b-it",
    "gemma-4-2b-it",
}

# NVIDIA NIM context length estimates by model family
NVIDIA_NIM_CTX: dict[str, int] = {
    "nemotron-3-ultra": 1_000_000,
    "nemotron-3-super": 262_144,
    "nemotron-3-nano": 262_144,
    "nemotron-4-340b": 1_000_000,
    "nemotron-mini": 131_072,
    "nemotron-nano": 131_072,
    "mistral-nemo": 131_072,
    "mistral-large": 131_072,
    "mistral-medium": 131_072,
    "codestral": 131_072,
    "llama-3.3": 131_072,
    "llama-3.2": 131_072,
    "llama-3.1": 131_072,
    "llama-3": 131_072,
    "codellama": 131_072,
    "granite": 131_072,
    "phi-3": 131_072,
    "starcoder": 131_072,
    "deepseek": 131_072,
    "dbrx": 131_072,
    "yi-large": 131_072,
    "jamba": 131_072,
    "sea-lion": 131_072,
    "glm": 131_072,
    "zamba": 131_072,
    "palmyra": 131_072,
    "step": 131_072,
    "inkling": 131_072,
    "qwen": 131_072,
    "gpt-oss": 131_072,
}

HEADERS = {
    "User-Agent": "fetch-free-models/1.0 (+https://github.com/defnlnotme/models)",
    "Accept": "application/json",
}


# NVIDIA NIM release date mapping: model_id -> "YYYY-MM"
# These are the ORIGINAL MODEL RELEASE DATES, not when NVIDIA added them to NIM.
# Only models released within the last 4 months are considered recent (from current date).
# Format: "YYYY-MM"
NVIDIA_NIM_RELEASE_DATES: dict[str, str] = {
    # Add known release dates here as discovered
    "nvidia/nemotron-3-nano-30b-a3b": "2025-12",
    "nvidia/nemotron-3-super-120b-a12b": "2026-03",
    "nvidia/nemotron-3-ultra-550b-a55b": "2026-06",
    "google/gemma-4-31b-it": "2026-03",
    "google/gemma-4-26b-a4b-it": "2026-03",
    "01-ai/yi-large": "2024-05",
    "baai/bge-m3": "2024-01",
    "bigcode/starcoder2-15b": "2024-02",
    "google/deplot": "2023-01",
    "ibm/granite-34b-code-instruct": "2024-04",
    "ibm/granite-8b-code-instruct": "2024-04",
    "meta/llama-guard-4-12b": "2024-04",
    "microsoft/phi-3-vision-128k-instruct": "2024-04",
    "microsoft/phi-3.5-moe-instruct": "2024-08",
    "mistralai/mistral-7b-instruct-v0.3": "2024-05",
    "mistralai/mistral-nemotron": "2024-07",
    "nvidia/llama3-chatqa-1.5-70b": "2024-06",
    "poolside/laguna-xs-2.1": "2026-07",
    "z-ai/glm-5.2": "2026-06",
    "zyphra/zamba2-7b-instruct": "2024-05",
    "deepseek-ai/deepseek-v4-flash": "2026-04",
    "deepseek-ai/deepseek-v4-pro": "2026-04",
    "google/gemma-3-12b-it": "2025-03",
    "google/gemma-3-4b-it": "2025-03",
    "google/codegemma-1.1-7b": "2024-04",
    "google/codegemma-7b": "2024-04",
    "meta/llama-3.1-70b-instruct": "2024-07",
    "meta/llama-3.1-8b-instruct": "2024-07",
    "meta/llama-3.2-11b-vision-instruct": "2024-09",
    "meta/llama-3.2-1b-instruct": "2024-09",
    "meta/llama-3.2-3b-instruct": "2024-09",
    "meta/llama-3.2-90b-vision-instruct": "2024-09",
    "meta/llama-3.3-70b-instruct": "2024-12",
    "meta/codellama-70b": "2024-01",
    "mistralai/codestral-22b-instruct-v0.1": "2024-05",
    "mistralai/mistral-large": "2024-02",
    "mistralai/mistral-large-2-instruct": "2024-07",
    "mistralai/mistral-medium-3.5-128b": "2026-03",
    "mistralai/mixtral-8x22b-v0.1": "2024-04",
    "moonshotai/kimi-k2.6": "2026-04",
    "nv-mistralai/mistral-nemo-12b-instruct": "2024-07",
    "nvidia/llama-3.1-nemotron-51b-instruct": "2024-10",
    "nvidia/llama-3.1-nemotron-70b-instruct": "2024-10",
    "nvidia/llama-3.1-nemotron-nano-8b-v1": "2024-10",
    "nvidia/llama-3.1-nemotron-nano-vl-8b-v1": "2024-10",
    "nvidia/llama-3.1-nemotron-ultra-253b-v1": "2024-10",
    "nvidia/llama-3.3-nemotron-super-49b-v1": "2024-12",
    "nvidia/llama-3.3-nemotron-super-49b-v1.5": "2025-01",
    "nvidia/mistral-nemo-minitron-8b-8k-instruct": "2024-07",
    "nvidia/nemotron-3-nano-omni-30b-a3b-reasoning": "2026-04",
    "nvidia/nemotron-4-340b-instruct": "2024-06",
    "nvidia/nemotron-mini-4b-instruct": "2024-06",
    "nvidia/nemotron-nano-12b-v2-vl": "2024-10",
    "nvidia/nemotron-nano-3-30b-a3b": "2024-10",
    "nvidia/nemotron-parse": "2024-10",
    "nvidia/nvidia-nemotron-nano-9b-v2": "2024-10",
    "openai/gpt-oss-120b": "2024-08",
    "openai/gpt-oss-20b": "2024-08",
    "stepfun-ai/step-3.7-flash": "2026-10",
    "thinkingmachines/inkling": "2026-07",
    # Note: Add more mappings as needed
}

# ── Helpers ────────────────────────────────────────────────────────────────
# ── Helpers ────────────────────────────────────────────────────────────────────
def fetch_json(
    url: str,
    attempt: int = 1,
    headers: dict[str, str] | None = None,
) -> dict[str, Any] | list[Any] | None:
    """Fetch JSON with retry on 429/5xx and timeout handling."""
    req = urllib.request.Request(url, headers=headers if headers is not None else HEADERS)
    try:
        with urllib.request.urlopen(req, timeout=TIMEOUT) as resp:
            result = json.loads(resp.read().decode("utf-8"))
            return result if isinstance(result, (dict, list)) else None
    except urllib.error.HTTPError as e:
        if e.code == 429 and attempt <= MAX_RETRIES:
            retry_after = int(e.headers.get("Retry-After", str(BACKOFF_BASE ** attempt)))
            print(f"Rate limited (429), waiting {retry_after}s... (attempt {attempt}/{MAX_RETRIES})", file=sys.stderr)
            time.sleep(retry_after)
            return fetch_json(url, attempt + 1)
        if 500 <= e.code < 600 and attempt <= MAX_RETRIES:
            wait = BACKOFF_BASE ** attempt
            print(f"Server error {e.code}, retrying in {wait}s... (attempt {attempt}/{MAX_RETRIES})", file=sys.stderr)
            time.sleep(wait)
            return fetch_json(url, attempt + 1)
        print(f"HTTP {e.code}: {e.reason}", file=sys.stderr)
        if e.code == 401:
            print("Check auth credentials", file=sys.stderr)
        elif e.code == 403:
            print("Access forbidden — check permissions", file=sys.stderr)
    except urllib.error.URLError as e:
        print(f"Network error: {e.reason}", file=sys.stderr)
    except json.JSONDecodeError as e:
        print(f"Invalid JSON: {e}", file=sys.stderr)
    except TimeoutError:
        print(f"Request timed out after {TIMEOUT}s", file=sys.stderr)
    return None


def normalize_kilo(model: dict[str, Any]) -> dict[str, Any] | None:
    """Convert Kilo model format to common schema.

    The Kilo Code API returns a placeholder `created` value (0) for every
    model rather than the actual model release date.  Leave `released` as
    None so the Artificial Analysis enrichment can supply the real date.
    """
    pricing = model.get("pricing", {})
    def to_float(v: Any) -> float:
        try:
            return float(v)
        except (TypeError, ValueError):
            return 0.0

    return {
        "id": model.get("id"),
        "name": model.get("name"),
        "provider": "kilocode",
        "context_length": model.get("context_length", 0),
        "intelligence": None,
        "elo": None,
        "released": None,
        "pricing": {
            "input": to_float(pricing.get("prompt", 0)),
            "output": to_float(pricing.get("completion", 0)),
            "cache_read": to_float(pricing.get("input_cache_read", 0)),
            "cache_write": to_float(pricing.get("input_cache_write", 0)),
        },
        "capabilities": {
            "reasoning": "reasoning" in model.get("supported_parameters", []),
            "vision": "image" in model.get("architecture", {}).get("input_modalities", []),
            "open_weights": False,
        },
        "source": "kilocode",
        "fetched_at": datetime.now(UTC).isoformat().replace("+00:00", "Z"),
        "raw": model,
    }


def normalize_opencode(model: dict[str, Any]) -> dict[str, Any] | None:
    """Convert OpenCode model format to common schema.

    The OpenCode API returns a single `created` timestamp for every model
    (the moment the listing was generated), NOT the actual model release
    date.  Using it as `released` would make every OpenCode model appear
    brand-new and would defeat recency-based filtering.  We therefore leave
    `released` as None and let the Artificial Analysis enrichment fill in
    the real release date when available.
    """
    model_id = model.get("id", "")
    return {
        "id": model_id,
        "name": model_id,
        "provider": "opencode",
        "context_length": OPENCODE_FREE_MODELS_CTX.get(model_id, 0),
        "intelligence": None,
        "elo": None,
        "released": None,
        "pricing": {
            "input": 0,
            "output": 0,
            "cache_read": 0,
            "cache_write": 0,
        },
        "capabilities": {
            "reasoning": False,
            "vision": False,
            "open_weights": False,
        },
        "source": "opencode",
        "fetched_at": datetime.now(UTC).isoformat().replace("+00:00", "Z"),
        "raw": {"id": model_id},
    }


def normalize_ollama(model_id: str) -> dict[str, Any] | None:
    """Convert curated Ollama Cloud free-tier entry to common schema."""
    return {
        "id": model_id,
        "name": model_id,
        "provider": "ollama-cloud",
        "context_length": OLLAMA_FREE_MODELS_CTX.get(model_id, 0),
        "intelligence": None,
        "elo": None,
        "released": None,
        "pricing": {
            "input": 0,
            "output": 0,
            "cache_read": 0,
            "cache_write": 0,
        },
        "capabilities": {
            "reasoning": False,
            "vision": "gemma4:31b" in model_id,
            "open_weights": False,
        },
        "source": "ollama-cloud",
        "fetched_at": datetime.now(UTC).isoformat().replace("+00:00", "Z"),
        "raw": {"id": model_id},
    }


def is_google_ai_studio_free(model_id: str) -> bool:
    """Determine if a Google AI Studio model is free tier."""
    model_lower = model_id.lower()

    # Explicitly paid patterns - return False immediately
    for pattern in GOOGLE_AI_STUDIO_PAID_PATTERNS:
        if pattern in model_lower:
            return False

    # Free patterns - must match at least one
    for pattern in GOOGLE_AI_STUDIO_FREE_PREFIXES:
        if pattern in model_lower:
            return True

    return False


def is_gemini_latest(model_id: str) -> bool:
    """Check if a gemini-* model is a *-latest version (preferred alias)."""
    model_lower = model_id.lower()
    return model_lower.startswith("gemini-") and model_lower.endswith("-latest")


def normalize_google_ai_studio_curated(model_id: str) -> dict[str, Any] | None:
    """Convert curated Google AI Studio free-tier entry to common schema.

    Uses the curated marklist (no API key needed) with context from GOOGLE_AI_STUDIO_FREE_MODELS_CTX.
    """
    return {
        "id": model_id,
        "name": model_id,
        "provider": "google-ai-studio",
        "context_length": GOOGLE_AI_STUDIO_FREE_MODELS_CTX.get(model_id, 0),
        "intelligence": None,
        "elo": None,
        "released": None,
        "pricing": {
            "input": 0,
            "output": 0,
            "cache_read": 0,
            "cache_write": 0,
        },
        "capabilities": {
            "reasoning": "3.6" in model_id.lower() or "pro" in model_id.lower(),
            "vision": "gemini" in model_id.lower() or "gemma4" in model_id.lower(),
            "open_weights": "gemma" in model_id.lower(),
        },
        "source": "google-ai-studio",
        "fetched_at": datetime.now(UTC).isoformat().replace("+00:00", "Z"),
        "raw": {"id": model_id},
    }


def is_nvidia_nim_recent_coding(model_id: str) -> bool:
    """Check if a NVIDIA NIM model is a generalist coding model released within the last 24 months."""
    model_lower = model_id.lower()

    # Exclude embedding, retriever, safety, translation, vision-specialized, etc.
    # Only exclude truly specialized/non-generalist models here.
    # Outdated models are handled by HIDE_MODELS + recency check in is_model_hidden.
    exclude_patterns = (
        "embed",
        "nemoretriever",
        "nemoguard",
        "safety",
        "topic-control",
        "content-safety",
        "translate",
        "riva-translate",
        "nvclip",
        "vila",
        "cosmos",
        "ising",
        "detector",
        "reward",
        "neva",
        "diffusion",
        "video",
        "audio",
        "speech",
        "tts",
        "voice",
        "writer/",
        "gemma-2",
        "recurrentgemma",
          "nemotron-nano",
        "nemotron-parse",
      )
    for pattern in exclude_patterns:
        if pattern in model_lower:
            return False

    # Recency check: only include models released within the last 24 months
    # Use the hard-coded release date mapping
    released = NVIDIA_NIM_RELEASE_DATES.get(model_id)
    if released:
        try:
            rel_year, rel_month = released.split("-")
            rel_date = datetime(int(rel_year), int(rel_month), 1, tzinfo=UTC)
            age_months = (datetime.now(UTC) - rel_date).days / 30.44
            if age_months > 24:
                return False
        except (ValueError, IndexError):
            pass

    # If not excluded, assume it's a generalist LLM (text-based)
    return True


def normalize_nvidia_nim(model: dict[str, Any], model_id: str) -> dict[str, Any] | None:
    """Convert NVIDIA NIM model from API response to common schema."""
    model_lower = model_id.lower()

    # Determine context length — prefer API value, fall back to hard-coded
    ctx = model.get("context_length") or 0
    if ctx == 0:
        for key, val in NVIDIA_NIM_CTX.items():
            if key in model_lower:
                ctx = val
                break

    # Determine capabilities
    has_reasoning = any(x in model_lower for x in ["nemotron", "reasoning", "thinking", "r1", "r1-", "super", "ultra", "nano-omni", "qwen3", "deepseek", "glm-5", "kimi-k2", "step-3"])
    has_vision = any(x in model_lower for x in ["vision", "vl", "vlm", "vila", "neva", "kosmos", "phi-3-vision", "omni", "nano-omni", "nano-vl", "multimodal"])
    has_tool_call = "instruct" in model_lower or "chat" in model_lower or "coder" in model_lower or "nemotron" in model_lower or "llama" in model_lower

    # Get release date from API "created" field, fall back to hard-coded mapping
    released = model.get("created") or model.get("release_date") or model.get("published_at")
    # The NVIDIA NIM API returns a placeholder timestamp (735790403 = 1993-04-26)
    # for all models. Ignore it and use hard-coded mapping instead.
    if isinstance(released, int) and released < 1577836800:  # before 2020-01-01
        released = None
    if not released:
        released = NVIDIA_NIM_RELEASE_DATES.get(model_id)
    # Convert Unix timestamp to "YYYY-MM" format for consistency
    if isinstance(released, int):
        released = datetime.fromtimestamp(released, tz=UTC).strftime("%Y-%m")

    return {
        "id": model_id,
        "name": model_id,
        "provider": "nvidia-nim",
        "context_length": ctx,
        "intelligence": None,
        "elo": None,
        "released": released,
        "pricing": {
            "input": 0,
            "output": 0,
            "cache_read": 0,
            "cache_write": 0,
        },
        "capabilities": {
            "reasoning": has_reasoning,
            "vision": has_vision,
            "open_weights": False,
        },
        "source": "nvidia-nim",
        "fetched_at": datetime.now(UTC).isoformat().replace("+00:00", "Z"),
        "raw": model,
    }


def fetch_kilo() -> list[dict[str, Any]]:
    """Fetch and normalize free models from Kilo API."""
    print("Fetching from Kilo Code API...", file=sys.stderr)
    data = fetch_json(KILO_ENDPOINT)
    if not data or not isinstance(data, dict) or "data" not in data:
        print("Kilo: no data or unexpected format", file=sys.stderr)
        return []

    free_models = []
    for model in data["data"]:
        model_id = model.get("id", "")
        is_free = (model.get("isFree") is True) or model_id.endswith(":free")
        if is_free:
            normalized = normalize_kilo(model)
            if normalized:
                free_models.append(normalized)

    print(f"Kilo: found {len(free_models)} free models", file=sys.stderr)
    return free_models


def _mark_unverified(models: list[dict[str, Any]], reason: str) -> list[dict[str, Any]]:
    """Tag models that never got a real probe, so the defect is visible downstream."""
    for m in models:
        m["verified"] = False
        m["unverified_reason"] = f"opencode probe skipped: {reason}"
    return models


# ── Auto-fallback liveness probe (--probe-auto) ─────────────────────────────────
#
# Every chain ends on a meta-router (kilocode `kilo-auto/free`, opencode
# `big-pickle`). regenerate_config.py refuses to write a terminator it knows to
# be dead, but it can only honour that if the fetcher says something about the
# two auto routers — and the ordinary opencode pass only covers models the
# listing advertises as free, which excludes opencode/big-pickle entirely. Left
# unprobed, the generator had no evidence for the provider it actually picks and
# its guard could never fire.
#
# The probe answers one question per auto router: can this key make ONE 1-token
# call to it right now? The verdict vocabulary is deliberately three-valued:
#
#   live     a real completion came back. The endpoint can serve.
#   dead     the provider REFUSED it: 400/401/402/403/404, or a 200 whose body
#            is an error envelope. This is a statement about entitlement or
#            existence, not about today, so it is safe to act on.
#   unknown  we could not find out: 429 (the key is fine, the bucket is empty),
#            any 5xx, a timeout, a DNS/TLS/connection error, or an unreadable
#            body. A transport blip says nothing about the endpoint.
#
# Collapsing `unknown` into `dead` is the bug this split exists to prevent: it
# would make one flaky network turn into "this endpoint is permanently gone",
# and regenerate_config.py aborts the whole regeneration on a confirmed-dead
# terminator. A dropped connection must never be able to block a sync.
AUTO_PROBE_TARGETS = (
    # (provider, models endpoint, model id, api key env, extra headers)
    ("kilocode", "https://api.kilo.ai/api/gateway/v1", "kilo-auto/free", "KILOCODE_API_KEY", {}),
    (
        "opencode",
        OPENCODE_ENDPOINT,
        "big-pickle",
        "OPENCODE_API_KEY",
        {
            # Mirrors opencodeRequestHeaders() in config.go.
            "User-Agent": "opencode/1.18.31/cli",
            "x-opencode-client": "cli",
            "x-opencode-session": "ses_probe",
            "x-opencode-request": "msg_probe",
            "x-opencode-project": "default",
        },
    ),
)

# HTTP statuses that mean "this provider will not serve this model", as opposed
# to "ask again later". 429 is deliberately absent: it is a rate limit on a
# working key, which is transient by definition.
PROBE_DEAD_STATUSES = frozenset({400, 401, 402, 403, 404})

# Probe timeout, deliberately shorter than the listing TIMEOUT (30s). A probe is
# a liveness check for ONE 1-token call on the critical path of the nightly
# sync: if the provider is that slow, the honest answer is `unknown`, and
# waiting longer only delays the model list to learn nothing. Pinned as its own
# constant so the third bucket is a decision, not an accident of the listing
# fetcher's timeout.
PROBE_TIMEOUT = 20


def classify_probe_status(status: int, body: bytes = b"") -> str:
    """Map an HTTP response to "live" / "dead" / "unknown".

    Split out from the network call so the verdict table is unit-testable
    without a socket — the distinction that matters (403 is a verdict, 429 is
    not) is exactly the one a live probe cannot demonstrate reliably.
    """
    if status == 429 or status >= 500:
        return "unknown"
    if status in PROBE_DEAD_STATUSES:
        return "dead"
    if 200 <= status < 300:
        if not body:
            return "live"
        try:
            parsed = json.loads(body)
        except (json.JSONDecodeError, UnicodeDecodeError):
            return "unknown"
        return "dead" if isinstance(parsed, dict) and parsed.get("error") else "live"
    return "unknown"


def probe_endpoint(endpoint: str, model_id: str, api_key: str,
                   extra_headers: dict[str, str] | None = None,
                   opener: Any = None) -> dict[str, Any]:
    """Send one 1-token chat completion and return a verdict record.

    Never raises: a probe that cannot reach the provider is `unknown`, not an
    exception, because this runs inside the nightly sync and a hard failure
    would lose the whole model list. `opener` is injectable so the timeout and
    the error classification can be exercised without a socket or a real wait.
    """
    body = json.dumps({
        "model": model_id,
        "messages": [{"role": "user", "content": "ping"}],
        "max_tokens": 1,
    }).encode()
    headers = {
        **HEADERS,
        "Content-Type": "application/json",
        "Authorization": f"Bearer {api_key}",
        **(extra_headers or {}),
    }
    checked_at = datetime.now(UTC).isoformat().replace("+00:00", "Z")
    req = urllib.request.Request(endpoint + "/chat/completions", data=body, headers=headers)
    open_url = opener or urllib.request.urlopen
    try:
        with open_url(req, timeout=PROBE_TIMEOUT) as resp:
            payload = resp.read()
            verdict = classify_probe_status(resp.status, payload)
            status: int | None = resp.status
    except urllib.error.HTTPError as e:
        try:
            payload = e.read()
        except Exception:  # noqa: BLE001 - body is best-effort context only
            payload = b""
        verdict = classify_probe_status(e.code, payload)
        status = e.code
    except (urllib.error.URLError, TimeoutError, OSError) as e:
        return {
            "verdict": "unknown",
            "status": None,
            "detail": f"transport: {e}",
            "checked_at": checked_at,
        }
    except Exception as e:  # noqa: BLE001 - a probe must not lose the model list
        # ssl.SSLError, http.client.RemoteDisconnected and friends are not
        # OSError subclasses on every Python build, and this runs inside the
        # nightly sync. Any failure to reach a verdict is `unknown`; the stderr
        # line below is what makes a systematically broken probe visible.
        print(f"Auto probe: unexpected {type(e).__name__}: {e}", file=sys.stderr)
        return {
            "verdict": "unknown",
            "status": None,
            "detail": f"unexpected {type(e).__name__}: {e}",
            "checked_at": checked_at,
        }
    detail = {
        "live": "1-token completion returned",
        "dead": f"provider refused the model (HTTP {status})",
        "unknown": f"no verdict (HTTP {status})",
    }[verdict]
    return {"verdict": verdict, "status": status, "detail": detail, "checked_at": checked_at}


def probe_auto_fallbacks(
    models: list[dict[str, Any]],
    environ: dict[str, str] | None = None,
) -> list[dict[str, Any]]:
    """Probe every auto router and stamp the verdict onto the model list.

    A router that is already in the list (kilocode's `kilo-auto/free` is: it is
    a free-tier model) gets its `verified` field set in place. A router the
    provider does not offer for free (opencode's `big-pickle`, which the
    listing hides behind a paywall) has no record to stamp, so a minimal
    `probe_only` record is appended: the JSON is a list because every consumer
    expects one, and this is the only way the generator can see a verdict for
    the endpoint it is about to write. Probe-only records carry no score, so
    they can never be selected as a chain candidate, and regenerate_config.py
    re-checks that explicitly.

    Re-running is idempotent: records are looked up by (provider, id) before any
    append, so a second run over a list that already carries them stamps in
    place. Appending unconditionally would grow the file by one record per
    router per run, for as long as the list is kept.

    Set SKIP_AUTO_PROBE=1 for offline or fixture-driven runs (CI, a local
    regeneration with no egress). Nothing is probed and no verdict is touched,
    so an existing list keeps the verdicts it already had and a fresh list has
    none — which the generator reads as "unprobed", never as "dead".
    """
    env = os.environ if environ is None else environ
    if env.get("SKIP_AUTO_PROBE") == "1":
        print(
            "Auto probe: !!! SKIP_AUTO_PROBE=1 — no terminator verdicts recorded. "
            "The generator will treat both routers as unprobed, which keeps "
            "kilocode (never opencode) but cannot detect an outage. Do not leave "
            "this set in a scheduled sync.",
            file=sys.stderr,
        )
        return models
    by_id = {(m.get("provider"), m.get("id")): m for m in models}
    for provider, endpoint, model_id, key_env, extra in AUTO_PROBE_TARGETS:
        api_key = env.get(key_env, "")
        if not api_key:
            print(
                f"Auto probe: {key_env} unset, skipping {provider}/{model_id}",
                file=sys.stderr,
            )
            continue
        result = probe_endpoint(endpoint, model_id, api_key, extra)
        record = by_id.get((provider, model_id))
        if record is None:
            record = {
                "id": model_id,
                "name": model_id,
                "provider": provider,
                "context_length": 0,
                "intelligence": None,
                "elo": None,
                "released": None,
                "pricing": {"input": 0.0, "output": 0.0, "cache_read": 0.0, "cache_write": 0.0},
                "capabilities": {"reasoning": False, "vision": False, "open_weights": False},
                "source": provider,
                "fetched_at": result["checked_at"],
                "raw": {},
                "probe_only": True,
            }
            models.append(record)
            by_id[(provider, model_id)] = record
        record["auto_probe"] = result
        # `verified` is the field the generator acts on, and False must mean
        # "confirmed dead" only — an unknown verdict leaves it unset (None).
        if result["verdict"] == "live":
            record["verified"] = True
        elif result["verdict"] == "dead":
            record["verified"] = False
        else:
            record.pop("verified", None)
            record["unverified_reason"] = f"auto probe: {result['detail']}"
        print(
            f"Auto probe: {provider}/{model_id} -> {result['verdict']} ({result['detail']})",
            file=sys.stderr,
        )
    return models


def opencode_probe(model_id: str, api_key: str) -> str:
    """Send one minimal chat completion to OpenCode and classify the result.

    Returns "ok", "freetier" (403 FreeTierError — key is authenticated but not
    entitled), or "error".

    The /zen/v1/models listing is NOT a reliable free-tier oracle. It happily
    lists models the current key cannot actually call: as of 2026-09-29 the key
    resolved 5 models (big-pickle, ling-3.0-flash-fin-free, mimo-v2.5-free,
    mimo-v2.6-flash-free, muse-spark-1.2-contributor-free) and ALL of them
    returned 403 FreeTierError, paid ones included. The attribution headers
    airouter sends (x-opencode-client, x-opencode-session, ...) do not change
    the outcome. So "listed" != "usable" and only a real call can tell.
    """
    body = json.dumps({
        "model": model_id,
        "messages": [{"role": "user", "content": "ping"}],
        "max_tokens": 1,
    }).encode()
    headers = {
        **HEADERS,
        "Content-Type": "application/json",
        "Authorization": f"Bearer {api_key}",
        # Mirrors opencodeRequestHeaders() in config.go: OpenCode grants
        # free-tier access to clients that identify as the OpenCode CLI.
        "User-Agent": "opencode/1.18.31/cli",
        "x-opencode-client": "cli",
        "x-opencode-session": "ses_probe",
        "x-opencode-request": "msg_probe",
        "x-opencode-project": "default",
    }
    req = urllib.request.Request(
        OPENCODE_ENDPOINT + "/chat/completions", data=body, headers=headers,
    )
    try:
        with urllib.request.urlopen(req, timeout=TIMEOUT) as resp:
            return "ok" if resp.status == 200 else "error"
    except urllib.error.HTTPError as e:
        if e.code == 403:
            return "freetier"
        return "error"
    except (urllib.error.URLError, TimeoutError, json.JSONDecodeError):
        return "error"


def fetch_opencode() -> list[dict[str, Any]]:
    """Fetch OpenCode models; free ones end in -free or are in OPENCODE_FREE_MODELS.

    Every candidate is then verified with a real 1-token chat completion, so we
    only emit endpoints the gateway can actually serve. Without this the
    nightly sync kept re-adding 19 opencode endpoints across the four profile
    chains (7 in smart, 8 in work, 2 in fast, 1 in large) that 403 on every
    call, costing a guaranteed round trip per request that reached them.

    Set SKIP_OPENCODE_PROBE=1 to skip verification and keep the old
    listing-only behaviour.
    """
    print("Fetching from OpenCode API...", file=sys.stderr)
    data = fetch_json(OPENCODE_MODELS_URL)
    if not data or not isinstance(data, dict) or "data" not in data:
        print("OpenCode: no data or unexpected format", file=sys.stderr)
        return []

    candidates = []
    for model in data.get("data", []):
        model_id = model.get("id", "")
        if model_id.endswith("-free") or model_id in OPENCODE_FREE_MODELS:
            candidates.append(model)

    api_key = os.environ.get("OPENCODE_API_KEY", "")
    if not api_key:
        print(
            "OpenCode: OPENCODE_API_KEY not set — cannot verify, "
            f"emitting {len(candidates)} UNVERIFIED candidates",
            file=sys.stderr,
        )
        return _mark_unverified(
            [n for n in (normalize_opencode(m) for m in candidates) if n],
            "OPENCODE_API_KEY unset",
        )

    if os.environ.get("SKIP_OPENCODE_PROBE") == "1":
        # Escape hatch that re-enables the exact defect this probe exists to
        # prevent (11 of 12 opencode models 403), so make it loud and mark the
        # output so downstream consumers can see it was never checked.
        print(
            "OpenCode: !!! SKIP_OPENCODE_PROBE=1 — emitting UNVERIFIED models. "
            "Most will 403 FreeTierError and cost a round trip per request. "
            "Do not leave this set in a scheduled sync.",
            file=sys.stderr,
        )
        return _mark_unverified(
            [n for n in (normalize_opencode(m) for m in candidates) if n],
            "SKIP_OPENCODE_PROBE=1",
        )

    free_models = []
    rejected = []
    for model in candidates:
        verdict = opencode_probe(model.get("id", ""), api_key)
        if verdict == "ok":
            normalized = normalize_opencode(model)
            if normalized:
                normalized["verified"] = True
                free_models.append(normalized)
        else:
            rejected.append(f"{model.get('id', '?')}({verdict})")

    if rejected:
        print(
            "OpenCode: rejected unusable models — " + ", ".join(rejected),
            file=sys.stderr,
        )
    print(f"OpenCode: {len(free_models)}/{len(candidates)} candidates verified usable", file=sys.stderr)
    return free_models


# CommandCode free-tier models.  The provider models list at
# https://api.commandcode.ai/provider/v1/models is the full catalog and does
# NOT itself mark free/paid — every model there is billed at its per-token
# rate unless an active "deal" makes it free.  The authoritative free set is
# the one published on the CommandCode pricing page
# (https://commandcode.ai/docs/resources/pricing-limits), which lists exactly
# four free models, each with a "Free while capacity lasts" / "Free while the
# stealth preview lasts" deal:
#   - stealth/space-bunny-alpha        (stealth preview, 1M ctx, free)
#   - poolside/laguna-s-2.1-free       (free while capacity lasts, 256K ctx)
#   - inclusionai/ling-3.0-flash-sante:free  (free while it lasts, 262K ctx)
#   - meituan/longcat-2.0-free         (free while it lasts, 1M ctx)
# Anything else returned by the provider endpoint is metered (per-token),
# so we never route to it as a free model.  Update this set when CommandCode
# adds or removes a free deal.
COMMANDCODE_FREE_MODELS: set[str] = {
    "stealth/space-bunny-alpha",
    "poolside/laguna-s-2.1-free",
    "inclusionai/ling-3.0-flash-sante:free",
    "meituan/longcat-2.0-free",
}

# CommandCode context windows, taken from the live provider list.  The
# endpoint reports context_length per model; this marklist is the offline
# fallback used when the API is unreachable so the ctx column is never 0.
COMMANDCODE_FREE_MODELS_CTX: dict[str, int] = {
    "stealth/space-bunny-alpha":  1_000_000,
    "poolside/laguna-s-2.1-free":   256_000,
    "inclusionai/ling-3.0-flash-sante:free": 262_144,
    "meituan/longcat-2.0-free":    1_048_576,
}


def canonicalize_commandcode_id(model_id: str) -> str:
    """Map a CommandCode provider endpoint id to its canonical deal id.

    The provider endpoint (https://api.commandcode.ai/provider/v1/models)
    returns the raw catalog ids, which differ from the deal ids published in
    the CLI docs / pricing page in two ways:
      - casing:  "meituan/LongCat-2.0" -> "meituan/longcat-2.0-free"
      - suffix:  the endpoint omits the "-free" deal suffix on the LongCat
        free deal (the other three free models carry it verbatim).
    This folds the endpoint id onto the canonical form so the curated free
    set matches regardless of how the upstream spells it.
    """
    if not model_id:
        return model_id
    if model_id in COMMANDCODE_FREE_MODELS:
        return model_id
    org, _, name = model_id.partition("/")
    name = name.lower().replace("_", "-").replace(" ", "-")
    name = re.sub(r"-+", "-", name).strip("-")
    canonical = f"{org.lower()}/{name}"
    if canonical in COMMANDCODE_FREE_MODELS:
        return canonical
    if canonical + "-free" in COMMANDCODE_FREE_MODELS:
        return canonical + "-free"
    return model_id


def normalize_commandcode(model: dict[str, Any]) -> dict[str, Any] | None:
    """Convert CommandCode provider model format to common schema.

    Like OpenCode, the CommandCode provider endpoint returns a single
    `created` timestamp for every model (the moment the listing was
    generated), not the actual model release date.  Leave `released` as
    None so the Artificial Analysis enrichment can supply the real date.
    """
    model_id = canonicalize_commandcode_id(model.get("id", ""))
    return {
        "id": model_id,
        "name": model.get("name") or model_id,
        "provider": "commandcode",
        "context_length": model.get("context_length") or COMMANDCODE_FREE_MODELS_CTX.get(model_id, 0),
        "intelligence": None,
        "elo": None,
        "released": None,
        "pricing": {
            "input": 0,
            "output": 0,
            "cache_read": 0,
            "cache_write": 0,
        },
        "capabilities": {
            "reasoning": False,
            "vision": False,
            "open_weights": False,
        },
        "source": "commandcode",
        "fetched_at": datetime.now(UTC).isoformat().replace("+00:00", "Z"),
        "raw": model,
    }


def fetch_commandcode() -> list[dict[str, Any]]:
    """Fetch CommandCode free models from the provider models endpoint.

    The provider endpoint lists the full catalog (paid and free alike) with
    no free flag, so the free set is intersected against the curated
    COMMANDCODE_FREE_MODELS deal list.  The endpoint is unauthenticated in
    practice, but the Provider API key (COMMANDCODE_API_KEY) is sent when
    available so rate limits / availability follow the authenticated plan.
    """
    print("Fetching from CommandCode provider API...", file=sys.stderr)
    headers = dict(HEADERS)
    cc_key = os.getenv("COMMANDCODE_API_KEY")
    if cc_key:
        headers["Authorization"] = f"Bearer {cc_key}"
    data = fetch_json(COMMANDCODE_ENDPOINT, headers=headers)
    if not data or not isinstance(data, dict) or "data" not in data:
        print("CommandCode: no data or unexpected format", file=sys.stderr)
        return []

    free_models = []
    for model in data.get("data", []):
        model_id = canonicalize_commandcode_id(model.get("id", ""))
        if model_id not in COMMANDCODE_FREE_MODELS:
            continue
        normalized = normalize_commandcode(model)
        if normalized:
            free_models.append(normalized)

    print(
        f"CommandCode: found {len(free_models)} free models "
        f"(of {len(data.get('data', []))} listed; free set is the curated deal list)",
        file=sys.stderr,
    )
    return free_models


def fetch_ollama() -> list[dict[str, Any]]:
    """Return the curated Ollama Cloud free-tier marklist."""
    print(
        f"Ollama: emitting {len(OLLAMA_FREE_MODELS)} curated free models "
        f"(no API call)",
        file=sys.stderr,
    )
    return [m for m in (normalize_ollama(mid) for mid in sorted(OLLAMA_FREE_MODELS)) if m is not None]


def resolve_google_latest_versions() -> dict[str, str]:
    """Map each *-latest alias to the newest stable numbered version.

    Queries the Google AI Studio v1beta/models endpoint for the live list and
    picks, per alias family, the highest version whose id matches the strict
    numbered pattern ("gemini-3.7-flash", not "...-preview" / "...-tts" /
    "...-image").  Falls back to GOOGLE_LATEST_FALLBACK when the API or key is
    unavailable so scheduled runs stay deterministic offline.

    Returns:
        alias -> concrete versioned model id (e.g. "gemini-flash-latest" ->
        "gemini-3.7-flash").  Aliases that cannot be resolved are absent.
    """
    api_key = os.getenv("GEMINI_API_KEY")
    if api_key:
        req = urllib.request.Request(
            f"{GOOGLE_AI_STUDIO_ENDPOINT}?pageSize=1000",
            headers={"x-goog-api-key": api_key},
        )
        try:
            with urllib.request.urlopen(req, timeout=TIMEOUT) as resp:
                data = json.loads(resp.read().decode("utf-8"))
            api_ids = [
                m.get("name", "").removeprefix("models/")
                for m in data.get("models", [])
            ]
        except Exception as e:
            print(f"Google latest-version lookup failed ({e}); using fallback", file=sys.stderr)
            api_ids = []
    else:
        print("GEMINI_API_KEY not set; using fallback latest versions", file=sys.stderr)
        api_ids = []

    if not api_ids:
        return dict(GOOGLE_LATEST_FALLBACK)

    resolved: dict[str, str] = {}
    for alias, pattern in GOOGLE_LATEST_ALIASES.items():
        best: tuple[tuple[int, ...], int, str] | None = None
        for mid in api_ids:
            mobj = pattern.match(mid)
            if not mobj:
                continue
            version = tuple(int(p) for p in mobj.group(1).split("."))
            # Highest version wins; on ties prefer the bare release over
            # dated snapshots (-002 etc.), then the lexicographically
            # greatest id for determinism.
            cand = (version, -mid.count("-"), mid)
            if best is None or cand > best:
                best = cand
        if best is not None:
            resolved[alias] = best[2]
        else:
            print(f"No numbered version found for {alias}; using fallback", file=sys.stderr)
    return {**GOOGLE_LATEST_FALLBACK, **resolved}


def fetch_google_ai_studio() -> list[dict[str, Any]]:
    """Fetch and normalize free models from Google AI Studio API.
    Note: We always use the curated list because the API does not provide
    the models in the form we want (with -latest suffixes) and we want to show
    only the specific models in the curated list.

    *-latest aliases additionally get a "match_id" pointing at the newest
    numbered version (resolved from the live endpoint), which is what the AA
    enrichment matches against.  Unresolvable aliases get an empty match_id,
    meaning "never guess a score for this one".
    """
    resolved = resolve_google_latest_versions()
    result = []
    for mid in sorted(GOOGLE_AI_STUDIO_FREE_MODELS):
        m = normalize_google_ai_studio_curated(mid)
        if m is None:
            continue
        if mid.endswith("-latest"):
            if mid in resolved:
                m["match_id"] = resolved[mid]
            else:
                # Future alias families must be added to GOOGLE_LATEST_ALIASES;
                # until then refuse to guess rather than name-match.
                print(f"warning: {mid} has no resolver entry; skipping AA match "
                      "(add it to GOOGLE_LATEST_ALIASES)", file=sys.stderr)
                m["match_id"] = ""
        result.append(m)
    print(f"Google AI Studio: found {len(result)} free models", file=sys.stderr)
    return result


def fetch_nvidia_nim() -> list[dict[str, Any]]:
    """Fetch and normalize recent generalist coding models from NVIDIA NIM API."""
    print("Fetching from NVIDIA NIM API...", file=sys.stderr)
    data = fetch_json(NVIDIA_NIM_ENDPOINT)

    if not data or not isinstance(data, dict) or "data" not in data:
        print("NVIDIA NIM: no data or unexpected format", file=sys.stderr)
        return []

    free_models = []
    for model in data["data"]:
        model_id = model.get("id", "")
        if not model_id:
            continue

        if is_nvidia_nim_recent_coding(model_id):
            normalized = normalize_nvidia_nim(model, model_id)
            if normalized:
                free_models.append(normalized)

    print(f"NVIDIA NIM: found {len(free_models)} recent generalist coding models", file=sys.stderr)
    return free_models


def fetch_artificial_analysis_data() -> dict[str, dict[str, Any]]:
    """Fetch intelligence scores and release dates from Artificial Analysis API.

    Returns a dict mapping model slugs to {"intelligence": float, "released": str | None}.
    Uses a local cache (free-models-cache.json) to avoid redundant API calls.
    Requires ARTIFICIAL_ANALYSIS_API_KEY environment variable.
    """
    # Try cache first
    cached = load_cache()
    if cached is not None:
        print(f"Artificial Analysis: using cached enrichment data ({len(cached)} models)", file=sys.stderr)
        return cached

    api_key = os.getenv("ARTIFICIAL_ANALYSIS_API_KEY")
    if not api_key:
        print("Artificial Analysis: API key not set, skipping enrichment (set ARTIFICIAL_ANALYSIS_API_KEY to enable)", file=sys.stderr)
        return {}

    print("Fetching intelligence scores and release dates from Artificial Analysis API...", file=sys.stderr)
    req = urllib.request.Request(
        ARTIFICIAL_ANALYSIS_ENDPOINT,
        headers={"Accept": "application/json", "x-api-key": api_key}
    )
    try:
        with urllib.request.urlopen(req, timeout=TIMEOUT) as resp:
            data = json.loads(resp.read().decode("utf-8"))
    except Exception as e:
        print(f"Artificial Analysis: fetch failed: {e}", file=sys.stderr)
        return {}

    if not data or not isinstance(data, dict) or "data" not in data:
        print("Artificial Analysis: no data or unexpected format", file=sys.stderr)
        return {}

    result = {}
    for model in data["data"]:
        slug = model.get("slug", "")
        if not slug:
            continue
        evaluations = model.get("evaluations", {})
        intelligence = evaluations.get("artificial_analysis_intelligence_index")
        released = model.get("released") or model.get("release_date") or model.get("published_at")
        entry: dict[str, Any] = {}
        if intelligence is not None:
            entry["intelligence"] = float(intelligence)
        if released:
            entry["released"] = str(released)
        if entry:
            result[slug] = entry

    print(f"Artificial Analysis: found {len(result)} models with data", file=sys.stderr)
    save_cache(result)
    return result


def load_arena_cache() -> dict[str, Any] | None:
    """Load cached arena code-ELO data if it is still fresh."""
    if not ARENA_CACHE_FILE.exists():
        return None
    try:
        data = json.loads(ARENA_CACHE_FILE.read_text())
        fetched_at = datetime.fromisoformat(data.get("fetched_at", "").replace("Z", "+00:00"))
        if (datetime.now(UTC) - fetched_at).total_seconds() > ARENA_CACHE_TTL_HOURS * 3600:
            return None
        enrichment = data.get("enrichment", {})
        return enrichment if isinstance(enrichment, dict) else None
    except (json.JSONDecodeError, ValueError, OSError):
        return None


def save_arena_cache(enrichment: dict[str, Any]) -> None:
    """Persist arena code-ELO data to cache."""
    ARENA_CACHE_FILE.write_text(json.dumps({
        "fetched_at": datetime.now(UTC).isoformat().replace("+00:00", "Z"),
        "enrichment": enrichment,
    }, indent=2))


def normalize_elo_to_intelligence(elo: float) -> float:
    """Approximate ELO -> AA intelligence index for fallback use only."""
    return round(max(elo - ELO_BASE, 0.0) * ELO_TO_INTELLIGENCE, 1)


def fetch_arena_code_data() -> dict[str, dict[str, Any]]:
    """Fetch ELO scores from the arena.ai code leaderboard snapshot.

    Returns a dict mapping arena model names to
    {"elo": float, "votes": int, "ci": float, "rank": int}.
    """
    cached = load_arena_cache()
    if cached is not None:
        print(f"Arena code: using cached ELO data ({len(cached)} models)", file=sys.stderr)
        return cached

    latest = fetch_json(ARENA_CODE_LATEST_URL)
    if not latest or not isinstance(latest, dict) or "date" not in latest:
        print("Arena code: could not resolve latest snapshot date", file=sys.stderr)
        return {}
    date = latest["date"]
    data = fetch_json(ARENA_CODE_DATA_URL.format(date=date))
    if not data or not isinstance(data, dict) or "models" not in data:
        print("Arena code: no data or unexpected format", file=sys.stderr)
        return {}

    result: dict[str, dict[str, Any]] = {}
    for m in data["models"]:
        name = m.get("model")
        score = m.get("score")
        if not name or score is None:
            continue
        result[name] = {
            "elo": float(score),
            "votes": int(m.get("votes") or 0),
            "ci": float(m.get("ci") or 0),
            "rank": int(m.get("rank") or 0),
        }
    print(f"Arena code: found {len(result)} models with ELO", file=sys.stderr)
    save_arena_cache(result)
    return result


# ── Output ─────────────────────────────────────────────────────────────────────
def output_json(data: list[dict], path: str | None) -> None:
    out = json.dumps(data, indent=2)
    if path:
        Path(path).write_text(out)
        print(f"Saved {len(data)} records to {path}", file=sys.stderr)
    else:
        print(out)


def output_csv(data: list[dict], path: str | None) -> None:
    if not data:
        return

    flat = []
    for d in data:
        caps = d.get("capabilities", {})
        pricing = d.get("pricing", {})
        row = {
            "id": d.get("id"),
            "provider": d.get("provider"),
            "context_length": d.get("context_length"),
            "intelligence": d.get("intelligence"),
            "intelligence_source": d.get("intelligence_source"),
            "intelligence_note": d.get("intelligence_note"),
            "released": d.get("released"),
            "elo": d.get("elo"),
            "elo_votes": d.get("elo_votes"),
            "pricing_input": pricing.get("input"),
            "pricing_output": pricing.get("output"),
            "pricing_cache_read": pricing.get("cache_read"),
            "pricing_cache_write": pricing.get("cache_write"),
            "reasoning": caps.get("reasoning"),
            "vision": caps.get("vision"),
            "open_weights": caps.get("open_weights"),
            "source": d.get("source"),
            "fetched_at": d.get("fetched_at"),
        }
        flat.append(row)

    fieldnames = list(flat[0].keys())
    out_io = sys.stdout if path is None else open(path, "w", newline="")
    try:
        writer = csv.DictWriter(out_io, fieldnames=fieldnames)
        writer.writeheader()
        writer.writerows(flat)
        if path:
            print(f"Saved {len(data)} records to {path}", file=sys.stderr)
    finally:
        if path:
            out_io.close()


def output_table(data: list[dict]) -> None:
    if not data:
        print("No records")
        return

    # Dynamically size the id column from the actual model slugs,
    # with the provider column as the next widest.
    id_width = max(len(d.get("id", "")) for d in data)
    id_width = max(id_width, len("id"))  # at least header width
    cols = [
        ("id", id_width),
        ("provider", 16),
        ("released", 12),
        ("intelligence", 12),
        ("elo", 8),
        ("ctx", 10),
        ("reason", 6),
        ("vision", 6),
    ]

    header = " | ".join(f"{name:<{w}}" for name, w in cols)
    print(header)
    print("-" * len(header))

    for d in data:
        caps = d.get("capabilities", {})
        ctx_val = d.get("context_length", 0) or 0
        ctx_disp = f"{ctx_val:,}" if ctx_val else "-"

        release_val = d.get("released")
        if release_val is not None:
            # Unix timestamps (Kilo/OpenCode carry full precision) -> YYYY-MM-DD.
            # Strings pass through unchanged ("YYYY-MM" for NVIDIA NIM marklists,
            # "YYYY-MM-DD" for AA enrichment).
            if isinstance(release_val, (int, float)) and release_val > 0:
                try:
                    dt = datetime.fromtimestamp(release_val, tz=UTC)
                    release_disp = dt.strftime("%Y-%m-%d")
                except (ValueError, OSError):
                    release_disp = str(release_val)
            else:
                release_disp = str(release_val)
        else:
            release_disp = "-"

        intelligence_val = d.get("intelligence")
        intelligence_disp = f"{intelligence_val:.1f}" if intelligence_val is not None else "-"

        elo_val = d.get("elo")
        elo_disp = f"{elo_val:.0f}" if elo_val is not None else "-"

        row = [
            d.get("id", ""),
            d.get("provider", ""),
            release_disp,
            intelligence_disp,
            elo_disp,
            ctx_disp,
            "Y" if caps.get("reasoning") else "N",
            "Y" if caps.get("vision") else "N",
        ]
        print(" | ".join(f"{v:<{w}}" for (_, w), v in zip(cols, row, strict=True)))


# ── Main ───────────────────────────────────────────────────────────────────────
def main() -> None:
    parser = argparse.ArgumentParser(
        description="Fetch free models from Kilo Code, OpenCode, Ollama Cloud, Google AI Studio, NVIDIA NIM, and CommandCode APIs"
    )
    parser.add_argument("--json", action="store_true", help="Output as JSON")
    parser.add_argument("--csv", action="store_true", help="Output as CSV")
    parser.add_argument("--table", action="store_true", help="Output as table (default)")
    parser.add_argument("--save", metavar="FILE", help="Save output to file instead of stdout")
    parser.add_argument("--kilocode-only", action="store_true", help="Only fetch from Kilo Code API")
    parser.add_argument("--opencode-only", action="store_true", help="Only fetch from OpenCode API")
    parser.add_argument("--ollama-only", action="store_true", help="Only fetch from Ollama Cloud API")
    parser.add_argument("--google-ai-studio-only", action="store_true", help="Only fetch from Google AI Studio API")
    parser.add_argument("--nvidia-nim-only", action="store_true", help="Only fetch from NVIDIA NIM API")
    parser.add_argument("--commandcode-only", action="store_true", help="Only fetch from CommandCode API")
    parser.add_argument(
        "--probe-auto",
        action="store_true",
        help="Probe the two chain terminators (kilocode kilo-auto/free, opencode big-pickle) "
             "with a real 1-token completion and record a live/dead/unknown verdict for each. "
             "regenerate_config.py acts on the verdict: it never writes a terminator known to "
             "be dead, and only treats a verdict as evidence when the provider REFUSED the "
             "call (a 429, 5xx or timeout is recorded as unknown, never as dead).",
    )
    args = parser.parse_args()

    # Default to table if no format specified
    if not (args.json or args.csv or args.table):
        args.table = True

    # Validate mutually exclusive flags
    only_count = sum([args.kilocode_only, args.opencode_only, args.ollama_only, args.google_ai_studio_only, args.nvidia_nim_only, args.commandcode_only])
    if only_count > 1:
        print(
            "Error: --kilocode-only, --opencode-only, --ollama-only, --google-ai-studio-only, --nvidia-nim-only, and --commandcode-only are mutually exclusive",
            file=sys.stderr,
        )
        sys.exit(1)

    # Fetch data
    all_models = []

    if not args.opencode_only and not args.ollama_only and not args.google_ai_studio_only and not args.nvidia_nim_only and not args.commandcode_only:
        kilo_models = fetch_kilo()
        all_models.extend(kilo_models)

    if not args.kilocode_only and not args.ollama_only and not args.google_ai_studio_only and not args.nvidia_nim_only and not args.commandcode_only:
        opencode_models = fetch_opencode()
        all_models.extend(opencode_models)

    if not args.kilocode_only and not args.opencode_only and not args.google_ai_studio_only and not args.nvidia_nim_only and not args.commandcode_only:
        ollama_models = fetch_ollama()
        all_models.extend(ollama_models)

    if not args.kilocode_only and not args.opencode_only and not args.ollama_only:
        google_models = fetch_google_ai_studio()
        all_models.extend(google_models)

    if not args.kilocode_only and not args.opencode_only and not args.ollama_only and not args.google_ai_studio_only:
        nvidia_models = fetch_nvidia_nim()
        all_models.extend(nvidia_models)

    if not args.kilocode_only and not args.opencode_only and not args.ollama_only and not args.google_ai_studio_only and not args.nvidia_nim_only:
        commandcode_models = fetch_commandcode()
        all_models.extend(commandcode_models)

    if not all_models:
        print("No free models found", file=sys.stderr)
        sys.exit(1)

    # Enrich with Artificial Analysis data (intelligence scores + release dates)
    aa_data = fetch_artificial_analysis_data()
    if aa_data:
        for model in all_models:
            # Providers can override the AA lookup key: Google's *-latest
            # aliases resolve to a concrete numbered version, and aliases
            # that failed to resolve carry an empty match_id meaning "do
            # not guess" — name-similarity matching is forbidden for them.
            match_id = model.get("match_id")
            if match_id == "":
                continue
            model_id = match_id or model.get("id", "")
            # Try direct match first, then fuzzy match with weight-stripping
            aa_entry = aa_data.get(model_id)
            if not aa_entry:
                matched_slug = fuzzy_match_slug(model_id, aa_data)
                if matched_slug:
                    aa_entry = aa_data.get(matched_slug)
            if aa_entry:
                if "intelligence" in aa_entry:
                    model["intelligence"] = aa_entry["intelligence"]
                if "released" in aa_entry and model.get("released") is None:
                    model["released"] = aa_entry["released"]

    # Enrich with Arena code-leaderboard ELO (fallback intelligence signal).
    # arena.ai/leaderboard/code ELO is used ONLY when Artificial Analysis has no
    # intelligence score for the model; it is normalized onto the AA scale so
    # the existing best-first ordering and thresholds keep working.
    arena_data = fetch_arena_code_data()
    if arena_data:
        matched = 0
        for model in all_models:
            # Same provider-override rules as AA: *-latest aliases carry a
            # concrete match_id; unresolved aliases (match_id == "") are skipped
            # because name-similarity matching is forbidden for them.
            match_id = model.get("match_id")
            if match_id == "":
                continue
            model_id = match_id or model.get("id", "")
            arena_name = fuzzy_match_slug(model_id, arena_data)
            if arena_name:
                entry = arena_data[arena_name]
                model["elo"] = entry["elo"]
                model["elo_votes"] = entry.get("votes")
                matched += 1
                if model.get("intelligence") is None:
                    model["intelligence"] = normalize_elo_to_intelligence(entry["elo"])
        print(f"Arena code: matched ELO for {matched} models", file=sys.stderr)

    # Deterministic smart floor: any model that still has no intelligence
    # score (neither AA nor arena code ELO) gets the lowest value that lets
    # it enter the smart/work/large chains. Auto-fallback meta-router models
    # and curated unscored-exclude models stay unscored -- they only ever
    # appear as the trailing chain entry, never as concrete candidates.
    floored = 0
    excluded = 0
    for model in all_models:
        if model.get("intelligence") is not None:
            continue
        if model.get("id") in AUTO_FALLBACK_MODELS or model.get("id") in UNSCORED_EXCLUDE:
            excluded += 1
            continue
        model["intelligence"] = SMART_FLOOR
        model["intelligence_source"] = "smart floor"
        model["intelligence_note"] = (
            "No AA intelligence score and no arena.ai code-leaderboard ELO; "
            "assigned the smart floor (25.0) so the model is visible in the "
            "chains. This is a deterministic default, not a benchmark score."
        )
        floored += 1
    print(
        f"Smart floor: floored {floored} models, excluded {excluded} "
        f"(meta-router / curated unscored-exclude)",
        file=sys.stderr,
    )

    # Apply hide list — remove models we would never use
    # Only filters nvidia-nim models; all other providers always display
    before_hide = len(all_models)
    all_models = [
        m for m in all_models
        if not is_model_hidden(
            m.get("id", ""),
            m.get("intelligence"),
            m.get("released"),
            m.get("provider"),
        )
    ]
    if len(all_models) < before_hide:
        print(f"Hide list: filtered {before_hide - len(all_models)} models", file=sys.stderr)

    # First-seen cache: for models that still have no release date after AA
    # enrichment, record the current fetch date as their first-appearance
    # date.  This is the best available signal for models whose upstream
    # API returns a placeholder `created` and whose AA entry has no
    # `released` field.  Once recorded, the date is stable across runs.
    # Done AFTER the hide-list filter so we don't waste cache entries on
    # models we would never route to anyway.
    first_seen = load_first_seen_cache()
    today_str = datetime.now(UTC).strftime("%Y-%m-%d")
    newly_seen = 0
    for model in all_models:
        mid = model.get("id", "")
        if not mid or mid in AUTO_FALLBACK_MODELS:
            continue
        if model.get("released") is not None:
            continue
        if mid in first_seen:
            model["released"] = first_seen[mid]
            model["released_source"] = "first-seen cache"
        else:
            first_seen[mid] = today_str
            model["released"] = today_str
            model["released_source"] = "first-seen (this run)"
            newly_seen += 1
    save_first_seen_cache(first_seen)
    print(
        f"First-seen: {len(first_seen)} cached, {newly_seen} newly recorded "
        f"(date={today_str})",
        file=sys.stderr,
    )

    # Per-source breakdown in the summary line
    by_source: dict[str, int] = {}
    for m in all_models:
        by_source.setdefault(m.get("source", "?"), 0)
        by_source[m["source"]] += 1
    summary = ", ".join(f"{k}={v}" for k, v in sorted(by_source.items()))
    print(f"Total models: {len(all_models)} ({summary})", file=sys.stderr)

    # Auto-fallback liveness: runs last so a probe-only record never passes
    # through the enrichment, smart-floor, hide-list or first-seen passes.
    if args.probe_auto:
        all_models = probe_auto_fallbacks(all_models)

    # Output
    if args.json:
        output_json(all_models, args.save)
    elif args.csv:
        output_csv(all_models, args.save)
    else:
        output_table(all_models)


if __name__ == "__main__":
    main()
