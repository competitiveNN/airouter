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
    """Tag models that never got a real probe, so the defect is visible downstream.

    `verified` is left UNSET rather than set to False. The generator's own
    contract (regenerate_config.py) is three-valued:

        verified is True   -> live    (a real call came back)
        verified is False  -> dead    (the provider REFUSED the call)
        verified is None   -> unknown (not probed, or the probe could not reach)

    and it maps False to 'dead' while treating None as 'unknown'. So writing
    False here declared models dead that were never asked, which made a run
    without a key -- or one that got rate-limited -- silently drop exactly the
    endpoints the probe exists to protect. `unverified_reason` still carries the
    explanation for whoever reads the list.
    """
    for m in models:
        m.pop("verified", None)
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
        # Left empty on purpose: opencode_headers() is applied at the call site
        # so the session id is minted canonically (ses_<12hex><14base62>).
        # The hardcoded `ses_probe` that used to live here failed the gate, so
        # this probe recorded a perfectly working model as permanently dead.
        {},
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
        # The OpenCode gate requires stream=true, so that probe answers with SSE
        # rather than a single JSON object. Without this branch the SSE body
        # fails json.loads and a working model is recorded `unknown`, which the
        # generator then treats as "unprobed" rather than live.
        if body.lstrip()[:5] in (b"data:", b"event:"):
            return "live" if _sse_has_content(body) else "unknown"
        try:
            parsed = json.loads(body)
        except (json.JSONDecodeError, UnicodeDecodeError):
            return "unknown"
        return "dead" if isinstance(parsed, dict) and parsed.get("error") else "live"
    return "unknown"


def _sse_has_content(body: bytes) -> bool:
    """True unless an SSE stream carries nothing but [DONE] / lifecycle events.

    An SSE stream of pure bookkeeping means the model emitted no text, which is
    the empty-completion artifact the gateway retries rather than a verdict
    about the model.
    """
    try:
        text = body.decode("utf-8", "replace")
    except Exception:  # noqa: BLE001 - undecodable bytes are not content
        return False
    for line in text.splitlines():
        line = line.strip()
        if not line.startswith("data:"):
            continue
        payload = line[len("data:"):].strip()
        if not payload or payload == "[DONE]":
            continue
        try:
            parsed = json.loads(payload)
        except json.JSONDecodeError:
            continue
        if not isinstance(parsed, dict):
            continue
        for delta in parsed.get("choices") or []:
            d = delta.get("delta") or {}
            # reasoning_content counts: a reasoning model spends its whole
            # budget there, and `big-pickle` answers with that alone under a
            # small max_tokens. Reading only `content` would call a working model
            # silent.
            if d.get("content") or d.get("reasoning_content"):
                return True
        # Responses-API chunks carry text under output_text / delta.
        if parsed.get("type") in {"response.output_text.delta"} and parsed.get("delta"):
            return True
    return False


def probe_endpoint(endpoint: str, model_id: str, api_key: str,
                   extra_headers: dict[str, str] | None = None,
                   opener: Any = None,
                   opencode_gate: bool = False) -> dict[str, Any]:
    """Send one 1-token chat completion and return a verdict record.

    Never raises: a probe that cannot reach the provider is `unknown`, not an
    exception, because this runs inside the nightly sync and a hard failure
    would lose the whole model list. `opener` is injectable so the timeout and
    the error classification can be exercised without a socket or a real wait.

    `opencode_gate` swaps the body for one that satisfies OpenCode's free-tier
    gate (stream + the bash/read tool pair). Without it the call 403s on its own
    request shape and `big-pickle` is recorded as permanently dead, which is a
    statement about the probe, not about the model.
    """
    if opencode_gate:
        body = opencode_gate_bodies(model_id)[0][1]
    else:
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
        is_opencode = provider == "opencode"
        result = probe_endpoint(
            endpoint, model_id, api_key,
            opencode_headers(api_key) if is_opencode else extra,
            opencode_gate=is_opencode,
        )
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


# ---------------------------------------------------------------------------
# OpenCode free-tier gate
#
# Bisected live against https://opencode.ai/zen/v1 on 2026-10-01. The gateway
# answers `403 FreeTierError: OpenCode's free tier can only be used from within
# OpenCode` unless ALL FOUR of these hold:
#
#   1. User-Agent starts with `opencode/`
#   2. x-opencode-session matches ^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$
#   3. `tools` contains BOTH `bash` and `read`
#   4. `stream` is true
#
# `x-opencode-client`, `x-opencode-request` and `x-opencode-project` are NOT
# checked (verified by removing each one: still HTTP 200).
#
# This matters because the previous probe sent NONE of 3 or 4 — it posted a bare
# `{"model", "messages", "max_tokens"}` body — so it failed the gate on its own
# request shape and then reported the model as unusable. That deleted 8 working
# models from the chains in one sync. "Listed" != "usable", but neither does
# "rejected by my own malformed probe" != "dead".
#
# Items 1, 3 and 4 are also why the gateway needs them at request time; item 2 is
# the same constraint airouterRequestHeaders() in config.go has to satisfy.
#
# The version is not cosmetic. OpenCode asks third-party gateways to send "the
# correct, official User-Agent string (matching opencode/<version>)", and the
# fix it documents for its own frontends is "update past <version>" — so a UA
# that trails the published release is not the official string. Bump it from
# https://registry.npmjs.org/opencode-ai/latest, and keep it equal to the
# User-Agent in config.go: test_opencode_ua_matches_the_gateway asserts the two
# agree, because nothing else stops them drifting apart.
OPENCODE_UA = "opencode/1.18.34/cli"

# Where the published version is read from. The dist-tags endpoint, NOT the
# package document: https://registry.npmjs.org/opencode-ai is ~5MB of every
# version ever published, and parsing that on every sync to read one string is
# absurd. The same reason there is a dedicated `latest` tag rather than taking
# the max: the tags list also carries `latest-0`, `latest-1` and dozens of
# `snapshot-*` builds, and "highest version wins" over all of them would pick a
# snapshot.
OPENCODE_DIST_TAGS_URL = "https://registry.npmjs.org/-/package/opencode-ai/dist-tags"


def opencode_ua_version(ua: str = OPENCODE_UA) -> str:
    """The `opencode/<version>/cli` version out of a User-Agent string.

    Tolerates the surrounding shape rather than assuming it: if the string is
    ever edited into something this cannot parse, the caller reports "unknown"
    instead of claiming a version that was never there.
    """
    m = re.match(r"^opencode/([^/]+)/", ua or "")
    return m.group(1) if m else ""


def check_opencode_ua_version() -> str | None:
    """Warn when OPENCODE_UA trails the published opencode release.

    Returns the version found to be behind, or None when it is current, the
    check is disabled, or the answer could not be established.

    WHY THIS EXISTS
    test_opencode_ua_matches_the_gateway proves config.go and this file agree.
    It cannot prove either is *current*: the repo sat on 1.18.31 while the
    published release reached 1.18.34, both copies agreeing perfectly the whole
    time. Nothing but a lookup against the real registry catches that.

    OPT-OUT, not opt-in. This runs on every invocation, including the scheduled
    sync, because the failure it exists to catch is silent and slow-moving —
    a check you have to remember to run is the check that does not run. Set
    SKIP_OPENCODE_UA_CHECK=1 to suppress it, for air-gapped machines and for
    runs where a registry round trip is not wanted.

    Advisory only. It never fails the run and never affects the model list:
    a lagging UA is a thing to fix, not a reason to refuse to sync, and this
    script's exit code is consumed by the sync timer.
    """
    if os.environ.get("SKIP_OPENCODE_UA_CHECK") == "1":
        return None

    have = opencode_ua_version()
    if not have:
        print(
            f"OpenCode UA check: cannot parse a version out of {OPENCODE_UA!r}; "
            "expected opencode/<version>/cli",
            file=sys.stderr,
        )
        return None

    tags = fetch_json(OPENCODE_DIST_TAGS_URL)
    latest = tags.get("latest") if isinstance(tags, dict) else None
    if not isinstance(latest, str) or not latest:
        # Offline, rate limited, or the registry changed shape. Staying quiet
        # is right: a sync must not fail because a version lookup did.
        return None

    if latest == have:
        return None

    # Only ever nag about being behind. A newer-looking `latest` (a rollback, a
    # re-publish) is not something to act on blindly, and the warning text tells
    # the reader to look rather than to overwrite.
    print("", file=sys.stderr)
    print("=" * 72, file=sys.stderr)
    print(f"OpenCode UA is stale: sending opencode/{have}, published is {latest}",
          file=sys.stderr)
    print("", file=sys.stderr)
    print("  OpenCode asks third-party gateways for \"the correct, official",
          file=sys.stderr)
    print("  User-Agent string (matching opencode/<version>)\".", file=sys.stderr)
    print("", file=sys.stderr)
    print("  Bump BOTH of these, then re-run scripts/check-rules.py:", file=sys.stderr)
    print(f"    fetch-free-models.py  OPENCODE_UA = \"opencode/{latest}/cli\"",
          file=sys.stderr)
    print(f"    config.go             \"User-Agent\": \"opencode/{latest}/cli\"",
          file=sys.stderr)
    print("", file=sys.stderr)
    print("  test_opencode_ua_matches_the_gateway will fail if only one is bumped.",
          file=sys.stderr)
    print("  Suppress with SKIP_OPENCODE_UA_CHECK=1.", file=sys.stderr)
    print("=" * 72, file=sys.stderr)
    print("", file=sys.stderr)
    return latest
# The gate counts tool NAMES and ignores the schemas. `bash`+`read` is the
# minimal passing set: `read`+`edit`+`glob`, a single `bash`, and two arbitrary
# names (`foo`,`bar`) were each measured as 403.
OPENCODE_CORE_TOOLS = ("bash", "read")
_BASE62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
# 429 is a rate limit on a working key, never a verdict about the model.
#
# It is also a property of the CREDENTIAL, not of the model being probed: once
# the key is limited, every remaining candidate is limited too. An earlier
# version retried independently per candidate -- 3 attempts x 2 protocols x 12
# candidates, with a backoff sleep on each, plus a 30s request timeout -- which
# turned a single rate-limited run into a ~40 minute hang inside the nightly
# sync. So the first exhausted 429 aborts the sweep, and the untried candidates
# are emitted UNVERIFIED rather than rejected: a rate limit must never be able
# to look like "these models are dead".
OPENCODE_429_RETRIES = 2
OPENCODE_429_BACKOFF = 5.0
# Wall clock for the whole opencode sweep, so a slow or throttled gateway cannot
# stall the sync indefinitely. Exceeding it leaves the rest unverified.
OPENCODE_PROBE_BUDGET = 240.0
# Small pause between candidates. Twelve back-to-back calls are themselves enough
# to trip the very limit the code then has to wait out.
OPENCODE_CANDIDATE_PAUSE = 1.0
# Per-request socket timeout for the gate probe. This bounds "time to first
# byte" only — the stream is abandoned as soon as one byte arrives — which is why
# it is a genuine wall-clock bound here in a way the listing TIMEOUT was not.
OPENCODE_PROBE_TIMEOUT = 20

# ── OpenCode probe cache ──────────────────────────────────────────────────────
#
# The gate probe answers "does this key still reach this model", which changes
# on the provider's schedule, not ours. Re-asking on every sync is pure cost:
# 12 candidates x up to 2 protocols is the slowest phase of the fetcher, and it
# is also the phase that makes the sync fragile, since a slow upstream turns
# into an over-budget run that leaves models unverified.
#
# Verdicts are cached PER MODEL and expire independently, because the two
# directions have very different costs of being wrong:
#   - `ok` cached long: a model that answered keeps answering. Stale-true costs
#     a chain entry that might 403 for a few days, and the gateway's cooldown
#     handles that at request time.
#   - a negative verdict cached SHORT: entitlements change, and models do come
#     back. Caching "freetier" for a week would keep a working model out of the
#     chains for a week -- the exact regression this probe was rebuilt to undo.
OPENCODE_PROBE_CACHE_FILE = Path(__file__).resolve().parent / "opencode-probe-cache.json"
OPENCODE_PROBE_OK_TTL_HOURS = int(os.environ.get("OPENCODE_PROBE_OK_TTL_HOURS", "168"))
OPENCODE_PROBE_NEG_TTL_HOURS = int(os.environ.get("OPENCODE_PROBE_NEG_TTL_HOURS", "24"))


def opencode_probe_cache_ttl_hours(verdict: str) -> int:
    """How long a cached verdict of this kind stays trustworthy."""
    return OPENCODE_PROBE_OK_TTL_HOURS if verdict == "ok" else OPENCODE_PROBE_NEG_TTL_HOURS


def load_opencode_probe_cache() -> dict[str, dict[str, Any]]:
    """Read the probe cache, dropping expired and malformed entries.

    Never raises: a corrupt cache must cost one live probe round, not the model
    list. Anything unusable is discarded rather than repaired, because the whole
    point is that a cache miss is always safe.
    """
    if not OPENCODE_PROBE_CACHE_FILE.exists():
        return {}
    try:
        raw = json.loads(OPENCODE_PROBE_CACHE_FILE.read_text())
        probes = raw.get("probes") if isinstance(raw, dict) else None
        if not isinstance(probes, dict):
            return {}
    except (json.JSONDecodeError, OSError):
        return {}
    live: dict[str, dict[str, Any]] = {}
    now = datetime.now(UTC)
    for model_id, entry in probes.items():
        if not isinstance(entry, dict):
            continue
        verdict = entry.get("verdict")
        if verdict not in ("ok", "freetier", "error"):
            continue
        try:
            checked = datetime.fromisoformat(str(entry.get("checked_at", "")).replace("Z", "+00:00"))
        except ValueError:
            continue
        if (now - checked).total_seconds() > opencode_probe_cache_ttl_hours(verdict) * 3600:
            continue
        live[model_id] = entry
    return live


def save_opencode_probe_cache(cache: dict[str, dict[str, Any]]) -> None:
    """Persist the probe cache. Best-effort: a write failure is not a sync failure."""
    payload = {
        "fetched_at": datetime.now(UTC).isoformat().replace("+00:00", "Z"),
        "probes": cache,
    }
    try:
        OPENCODE_PROBE_CACHE_FILE.write_text(json.dumps(payload, indent=2, sort_keys=True))
    except OSError as e:
        print(f"OpenCode: could not write the probe cache: {e}", file=sys.stderr)


class OpenCodeRateLimited(Exception):
    """The key is rate-limited, so every remaining candidate is UNKNOWN.

    Carries the per-attempt detail purely for the stderr line; nothing consumes
    it programmatically.
    """


# The two paths opencode_gate_bodies() tries, and the config-side name for each.
# "chat" is the default everywhere (config.go's protocolFor falls back to it), so
# it is only ever written when it was PROVED, which is what keeps the emitted
# config free of a field that says nothing.
PROBE_PATH_PROTOCOLS = {
    "/chat/completions": "chat",
    "/responses": "responses",
}


def protocol_for_probe_detail(detail: str) -> str | None:
    """The protocol a probe detail says answered, or None when it says nothing.

    Only a 200 names a shape. A detail made of refusals ("/chat/completions 400
    Model does not support this protocol.; /responses 200") does name one, and
    that is the case that matters: the model is fine, it just speaks the other
    wire shape, and writing that down is the difference between a working chain
    and a failed round trip on every process start.
    """
    if not detail:
        return None
    for path, protocol in PROBE_PATH_PROTOCOLS.items():
        if f"{path} 200" in detail:
            return protocol
    return None


def opencode_session_id() -> str:
    """Mint a canonical `ses_<12 hex><14 base62>` id.

    The gateway rejects a session id in any other shape — including the
    `ses_` + 32 hex that config.go used to send, which was a 403 on every
    proxied request. 6 random bytes give the hex part; the other 14 bytes are
    folded through base62 one char each.
    """
    raw = os.urandom(20)
    return "ses_" + raw[:6].hex() + "".join(_BASE62[b % 62] for b in raw[6:20])


def opencode_gate_tools(protocol: str) -> list[dict[str, Any]]:
    """The two core tool definitions, shaped for the envelope they go into.

    The two protocols disagree on where the name lives and getting it wrong is
    NOT cosmetic: sent flat to /chat/completions, the upstream answers
    `tools[0].function must be an object`, which looks like a model fault.
    """
    def tool(name: str) -> dict[str, Any]:
        params: dict[str, Any] = {
            "type": "object", "properties": {}, "additionalProperties": False,
        }
        if protocol == "responses":
            return {"type": "function", "name": name,
                    "description": f"The {name} tool", "parameters": params}
        return {"type": "function",
                "function": {"name": name, "description": f"The {name} tool",
                             "parameters": params}}

    return [tool(n) for n in OPENCODE_CORE_TOOLS]


def opencode_gate_bodies(model_id: str) -> list[tuple[str, bytes]]:
    """Gate-passing request bodies, one per protocol the listing can map to.

    Both are tried: several models answer `400 Model does not support this
    protocol` on the wrong one. That 400 means the free-tier gate was PASSED and
    the request reached the model layer, which is exactly the distinction the
    old probe could not see.
    """
    chat = {
        "model": model_id,
        "messages": [{"role": "user", "content": "ping"}],
        # 16 is plenty: the verdict is the HTTP status, and _opencode_post reads
        # ONE byte to prove the stream opened, then closes. An earlier version
        # drained the whole stream to completion (max_tokens=64) and a reasoning
        # model trickling tokens kept resetting the per-read socket timeout, so
        # nemotron-3.5-lightning-free alone took 67s of a 12-candidate sweep.
        "max_tokens": 16,
        "stream": True,
        "tools": opencode_gate_tools("chat"),
    }
    responses = {
        "model": model_id,
        "input": [{"role": "user", "content": "ping"}],
        "max_output_tokens": 16,
        "stream": True,
        "tools": opencode_gate_tools("responses"),
    }
    return [
        ("/chat/completions", json.dumps(chat).encode()),
        ("/responses", json.dumps(responses).encode()),
    ]


def opencode_headers(api_key: str, session: str | None = None) -> dict[str, str]:
    """Headers satisfying gate items 1 and 2. Mirrors opencodeRequestHeaders()."""
    return {
        **HEADERS,
        "Content-Type": "application/json",
        "Authorization": f"Bearer {api_key}",
        "User-Agent": OPENCODE_UA,
        "x-opencode-session": session or opencode_session_id(),
        "x-opencode-client": "cli",
        "x-opencode-request": "msg_" + os.urandom(16).hex(),
        "x-opencode-project": "default",
    }


def _opencode_post(path: str, body: bytes, headers: dict[str, str],
                   opener: Any = None) -> tuple[int, str]:
    """One POST. Returns (status, error message) and never raises.

    `opener` is injectable so the verdict classification can be exercised
    without a socket, a credential, or the 429 backoff wait.
    """
    req = urllib.request.Request(
        OPENCODE_ENDPOINT + path, data=body, headers=headers,
    )
    open_url = opener or urllib.request.urlopen
    try:
        with open_url(req, timeout=OPENCODE_PROBE_TIMEOUT) as resp:
            # ONE byte, then close. The verdict is the HTTP status: 200 means
            # the gate passed and the model opened a stream, which is exactly
            # "usable". Draining the stream instead is what made this probe
            # appear to hang — `timeout` is applied per socket read, so a model
            # emitting tokens slowly resets it indefinitely and read() blocks
            # until the completion finishes. Measured: 67s for a single
            # reasoning model, versus well under a second to first byte.
            resp.read(1)
            return resp.status, ""
    except urllib.error.HTTPError as e:
        try:
            payload = e.read(65536)
        except Exception:  # noqa: BLE001 - the body is context only
            payload = b""
        try:
            err = json.loads(payload).get("error", {})
            msg = err.get("message") if isinstance(err, dict) else err
        except Exception:  # noqa: BLE001 - not our parse problem
            msg = ""
        return e.code, str(msg or "").replace("\n", " ")[:120]
    except Exception as e:  # noqa: BLE001 - URLError, timeout, TLS, decode...
        return 0, f"{type(e).__name__}: {e}"[:120]


def opencode_probe(model_id: str, api_key: str, opener: Any = None) -> tuple[str, str]:
    """Send one gate-satisfying call and classify the result.

    Returns ("ok"|"freetier"|"error", detail).

    "freetier" is reserved for a genuine 403 FreeTierError on a request that
    satisfied the whole gate — that is a statement about the key's entitlement.
    A 400, a protocol mismatch or a transport failure is "error" and is reported
    with its detail, so a caller can tell "this model is gone" from "I asked it
    the wrong question".

    Raises OpenCodeRateLimited if the key is rate-limited, which is a fact about
    the credential rather than about this model. The caller must then treat every
    remaining candidate as unknown instead of dead.
    """
    session = opencode_session_id()
    headers = opencode_headers(api_key, session)
    seen: list[str] = []
    saw_403 = False
    for path, body in opencode_gate_bodies(model_id):
        for attempt in range(OPENCODE_429_RETRIES):
            code, detail = _opencode_post(path, body, headers, opener)
            if code == 200:
                # The detail names the shape that answered, and that is the whole
                # point: a model can be reachable only over one of the two
                # protocols. Measured on opencode.ai 2026-10-02 --
                # muse-spark-1.{2,3}-contributor-free answer
                # `400 ModelProtocolUnsupported` on /chat/completions and 200 on
                # /responses, big-pickle is the other way round. The router
                # recovers at runtime by flipping and retrying (api.go), but that
                # costs a failed round trip per endpoint per process start, and
                # nothing carried the answer into config.yaml. See
                # protocol_for_probe_detail().
                return "ok", f"{path} 200"
            if code == 429:
                seen.append(f"{path} 429")
                # No sleep after the final attempt: there is nothing left to wait
                # for, and that sleep is pure dead time before the sweep aborts.
                if attempt + 1 < OPENCODE_429_RETRIES:
                    time.sleep(OPENCODE_429_BACKOFF * (attempt + 1))
                continue
            seen.append(f"{path} {code} {detail}".strip())
            if code == 403:
                saw_403 = True
            break
        else:
            # Every attempt on this protocol was a 429. The limit is on the key,
            # not on this model or this protocol, so stop the whole sweep rather
            # than grinding through the remaining candidates and the second
            # protocol to reach the same 429.
            raise OpenCodeRateLimited("; ".join(seen))
    detail = "; ".join(seen)[:300]
    return ("freetier", detail) if saw_403 else ("error", detail)


def fetch_opencode() -> list[dict[str, Any]]:
    """Fetch OpenCode models; free ones end in -free or are in OPENCODE_FREE_MODELS.

    Every candidate is then verified with a real call that satisfies the
    gateway's free-tier gate (see the OPENCODE gate notes above), so we only
    emit endpoints the gateway can actually serve. Without verification the
    nightly sync kept re-adding 19 opencode endpoints across the four profile
    chains (7 in smart, 8 in work, 2 in fast, 1 in large) that 403 on every
    call, costing a guaranteed round trip per request that reached them.

    Verdicts are cached per model in OPENCODE_PROBE_CACHE_FILE and expire
    independently by direction, so a steady-state sync spends one listing call
    and no probe calls at all. Set REFRESH_OPENCODE_PROBE=1 to force a live
    re-probe of every candidate and rewrite the cache.

    Set SKIP_OPENCODE_PROBE=1 to skip verification and keep the old
    listing-only behaviour. That reintroduces the dead-endpoint problem, so
    the output is marked unverified and the reason is printed.
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
    untried: list[dict[str, Any]] = []
    refresh = os.environ.get("REFRESH_OPENCODE_PROBE") == "1"
    cache: dict[str, dict[str, Any]] = {} if refresh else load_opencode_probe_cache()
    cached_used = 0
    deadline = time.monotonic() + OPENCODE_PROBE_BUDGET
    for index, model in enumerate(candidates):
        if time.monotonic() > deadline:
            untried.extend(candidates[index:])
            print(
                f"OpenCode: probe budget of {OPENCODE_PROBE_BUDGET:.0f}s exhausted "
                f"after {index} candidates; {len(untried)} left UNVERIFIED",
                file=sys.stderr,
            )
            break
        model_id = model.get("id", "")
        entry = cache.get(model_id)
        if entry is not None:
            verdict, detail = entry["verdict"], entry.get("detail", "cached")
            cached_used += 1
        else:
            try:
                verdict, detail = opencode_probe(model_id, api_key)
            except OpenCodeRateLimited:
                # Includes the current candidate: it was never actually answered.
                untried.extend(candidates[index:])
                print(
                    f"OpenCode: key is rate limited (429) after {index} candidates; "
                    f"{len(untried)} left UNVERIFIED. A 429 says nothing about whether "
                    "those models work, so they are NOT treated as dead.",
                    file=sys.stderr,
                )
                break
            cache[model_id] = {
                "verdict": verdict,
                "detail": detail,
                "checked_at": datetime.now(UTC).isoformat().replace("+00:00", "Z"),
            }
        if verdict == "ok":
            normalized = normalize_opencode(model)
            if normalized:
                normalized["verified"] = True
                # Which wire shape answered travels with the record, so the
                # generator can write `protocol:` and the router does not have to
                # spend a 400 discovering it again on every process start.
                protocol = protocol_for_probe_detail(detail)
                if protocol:
                    normalized["protocol"] = protocol
                free_models.append(normalized)
        else:
            rejected.append(f"{model_id}({verdict}: {detail})")
        if entry is None and index + 1 < len(candidates):
            time.sleep(OPENCODE_CANDIDATE_PAUSE)

    # Only persist what this sweep actually produced a verdict for, so a run that
    # stopped early cannot promote a half-finished pass into a week of cache.
    probed = {m.get("id", "") for m in candidates} - {m.get("id", "") for m in untried}
    save_opencode_probe_cache(
        {k: v for k, v in cache.items() if k in probed and k}
    )

    if untried:
        # normalize_opencode never sets `protocol`, so an unprobed record cannot
        # claim a shape: absence is the only honest value, and regenerate_config
        # preserves whatever config.yaml already says for that endpoint.
        free_models.extend(_mark_unverified(
            [n for n in (normalize_opencode(m) for m in untried) if n],
            "opencode probe never reached this model (rate limited, or over the "
            "probe budget)",
        ))

    if rejected:
        print(
            "OpenCode: rejected unusable models — " + ", ".join(rejected),
            file=sys.stderr,
        )
    if cached_used:
        print(
            f"OpenCode: {cached_used}/{len(candidates)} verdicts served from "
            f"{OPENCODE_PROBE_CACHE_FILE.name} (negative verdicts expire after "
            f"{OPENCODE_PROBE_NEG_TTL_HOURS}h, positive after "
            f"{OPENCODE_PROBE_OK_TTL_HOURS}h)",
            file=sys.stderr,
        )
    print(f"OpenCode: {len(free_models)}/{len(candidates)} candidates verified usable", file=sys.stderr)
    return free_models


# CommandCode CANDIDATE free deals — seeds, not the answer.
#
# The provider models list at https://api.commandcode.ai/provider/v1/models is
# the full catalog and does NOT mark free/paid: every model there is billed at
# its per-token rate unless an active deal makes it free. So the id alone cannot
# answer "is this free?", and a hand-maintained list of free models goes stale
# the moment a deal ends -- which is exactly what happened:
#
#   meituan/longcat-2.0-free  stayed in this list long after the deal ended.
#   Probed live on 2026-10-01 it answers HTTP 400 "You have insufficient
#   credits to make this request. Please purchase more credit[s]" -- the
#   provider's unambiguous statement that the call is BILLED.
#
# So this set is now only the SEED list of ids worth probing: deals that do not
# carry the provider's own free suffix. Free-ness itself is decided by
# commandcode_probe() below, exactly as it already is for OpenCode, and a seed
# that stops being free drops out on the next sync without anyone editing this
# file. The suffix heuristic still catches the deals that advertise themselves
# (see commandcode_is_free_candidate).
COMMANDCODE_FREE_SEEDS: set[str] = {
    "stealth/space-bunny-alpha",   # stealth preview, 1M ctx
    "meituan/longcat-2.0-free",    # deal ended; probe now rejects it
}

# Ids that were free seeds historically and must stay probe-able (not silently
# dropped from the candidate set) so a resurrected deal is picked up again.
COMMANDCODE_FREE_MODELS = COMMANDCODE_FREE_SEEDS | {
    "poolside/laguna-s-2.1-free",
    "inclusionai/ling-3.0-flash-sante:free",
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


# ── CommandCode endpoint resolution ─────────────────────────────────────────────
# The probe has to hit the endpoint the GATEWAY will use, not the one this script
# happened to be written with. config.yaml routes commandcode at
# http://127.0.0.1:3050/v1, while COMMANDCODE_ENDPOINT points at
# https://api.commandcode.ai/provider/v1. Probing the public host measures a
# different server and answers 403 "Your Go plan doesn't include API access" for
# EVERY model — which looks exactly like "none of these are free" and would
# empty the chain. Measured 2026-10-01:
#
#   via api.commandcode.ai : space-bunny-alpha 403, laguna 403, longcat 400
#   via the routed endpoint: space-bunny-alpha 200, laguna 502, longcat 400
#
# Only the routed endpoint can tell a live deal from a dead one.


def commandcode_base_url() -> str:
    """The base URL config.yaml routes commandcode through, else the default."""
    config = Path(__file__).resolve().parent / "config.yaml"
    try:
        import yaml

        cfg = yaml.safe_load(config.read_text()) or {}
        url = ((cfg.get("providers") or {}).get("commandcode") or {}).get("url")
        if isinstance(url, str) and url.strip():
            return url.strip().rstrip("/")
    except Exception as exc:  # missing config, no yaml, unparseable
        print(
            f"CommandCode: could not read commandcode url from config.yaml ({exc}); "
            "falling back to the built-in endpoint, whose probe result may not "
            "reflect the routed server",
            file=sys.stderr,
        )
    # COMMANDCODE_ENDPOINT is a models URL; the base is everything before it.
    return COMMANDCODE_ENDPOINT[: -len("/models")] if COMMANDCODE_ENDPOINT.endswith("/models") else COMMANDCODE_ENDPOINT


def commandcode_models_url() -> str:
    return commandcode_base_url() + "/models"


def commandcode_chat_url() -> str:
    return commandcode_base_url() + "/chat/completions"


def commandcode_is_free_candidate(model_id: str) -> bool:
    """Whether a catalog id is worth probing, by the provider's own convention.

    Two ways an id advertises a free deal:

      * the free suffix — `-free` (poolside/laguna-s-2.1-free) or `:free`
        (inclusionai/ling-3.0-flash-sante:free). That is the provider's naming
        convention for a live free deal, so honour it as a heuristic rather than
        enumerating ids.
      * no suffix at all — the stealth preview `stealth/space-bunny-alpha` is
        free and never carried one. Those are the seeds.

    This is a CANDIDATE filter. Being a candidate is not being free; only
    commandcode_probe() decides that.
    """
    if not model_id:
        return False
    name = model_id.rsplit("/", 1)[-1].lower()
    return name.endswith("-free") or name.endswith(":free") or model_id in COMMANDCODE_FREE_SEEDS


# Verbs in a provider error body that mean "this call is billed", i.e. the model
# is NOT on a free deal. Matched case-insensitively against the message.
_COMMANDCODE_PAID_MARKERS = (
    "insufficient credit",
    "insufficient balance",
    "purchase more credit",
    "add credit",
    "payment required",
    "upgrade to",
    "billing",
)


def commandcode_probe(model_id: str, api_key: str) -> str:
    """Send one minimal chat completion and classify the result.

    Returns one of:

      "ok"      — the call succeeded, so the deal is live and free.
      "paid"    — the provider said the call is billed. This is a DEFINITIVE
                  verdict and the model is excluded.
      "unknown" — a transient or inconclusive condition (capacity, rate limit,
                  5xx, timeout, or a plan restriction). NOT evidence that the
                  deal ended, so it must not be treated as "paid".

    The distinction between "paid" and "unknown" is the whole point. Probed live
    on 2026-10-01 through the endpoint the gateway actually uses:

      meituan/longcat-2.0             400 insufficient credits   -> "paid"
      stealth/space-bunny-alpha       200                        -> "ok"
      poolside/laguna-s-2.1-free      502 providers at capacity  -> "unknown"

    Treating all three as one "unusable" bucket is what the old hardcoded list
    got wrong in the other direction: a capacity blip would look like a deal
    ending.
    """
    body = json.dumps({
        "model": model_id,
        "messages": [{"role": "user", "content": "ping"}],
        "max_tokens": 1,
    }).encode()
    headers = {**HEADERS, "Content-Type": "application/json"}
    if api_key:
        headers["Authorization"] = f"Bearer {api_key}"
    req = urllib.request.Request(
        commandcode_chat_url(), data=body, headers=headers,
    )
    try:
        with urllib.request.urlopen(req, timeout=TIMEOUT) as resp:
            return "ok" if resp.status == 200 else "unknown"
    except urllib.error.HTTPError as e:
        raw = b""
        try:
            raw = e.read()
        except Exception:
            pass
        text = raw.decode("utf-8", "replace").lower()
        # Billing language wins over the status code: this provider returns 400
        # (not 402) for "insufficient credits", so keying on 402 alone misses it.
        if any(marker in text for marker in _COMMANDCODE_PAID_MARKERS):
            return "paid"
        if e.code in (401, 403):
            # "Your Go plan doesn't include API access" — this endpoint is not
            # reachable on this plan, which says nothing about the deal.
            return "unknown"
        if e.code == 404:
            return "paid"      # the id is gone from the catalog entirely
        return "unknown"       # 429, 5xx, and everything else transient
    except (urllib.error.URLError, TimeoutError, json.JSONDecodeError):
        return "unknown"


def fetch_commandcode() -> list[dict[str, Any]]:
    """Fetch CommandCode free models, verified by a real completion.

    Candidates come from the provider's own free-suffix convention plus the
    seed list; free-ness is decided by commandcode_probe(). A deal that ends
    drops out on the next sync with no edit to this file, which is the property
    the hardcoded COMMANDCODE_FREE_MODELS list did not have.

    Set SKIP_COMMANDCODE_PROBE=1 to fall back to candidates-only (the old
    behaviour); it prints loudly and marks the output unverified.
    """
    print("Fetching from CommandCode provider API...", file=sys.stderr)
    headers = dict(HEADERS)
    cc_key = os.getenv("COMMANDCODE_API_KEY")
    if cc_key:
        headers["Authorization"] = f"Bearer {cc_key}"
    data = fetch_json(commandcode_models_url(), headers=headers)
    if not data or not isinstance(data, dict) or "data" not in data:
        print("CommandCode: no data or unexpected format", file=sys.stderr)
        return []

    candidates = []
    for model in data.get("data", []):
        model_id = canonicalize_commandcode_id(model.get("id", ""))
        if not commandcode_is_free_candidate(model_id):
            continue
        normalized = normalize_commandcode(model)
        if normalized:
            candidates.append(normalized)

    if not cc_key:
        print(
            "CommandCode: COMMANDCODE_API_KEY not set — cannot verify, "
            f"emitting {len(candidates)} UNVERIFIED candidates",
            file=sys.stderr,
        )
        return _mark_unverified(candidates, "COMMANDCODE_API_KEY unset")

    if os.environ.get("SKIP_COMMANDCODE_PROBE") == "1":
        print(
            "CommandCode: !!! SKIP_COMMANDCODE_PROBE=1 — emitting UNVERIFIED "
            "candidates. Deals that have ended will keep costing a failed round "
            "trip per request. Do not leave this set in a scheduled sync.",
            file=sys.stderr,
        )
        return _mark_unverified(candidates, "SKIP_COMMANDCODE_PROBE=1")

    free_models = []
    dropped = []
    unknown = []
    for model in candidates:
        verdict = commandcode_probe(model["id"], cc_key)
        if verdict == "ok":
            model["verified"] = True
            free_models.append(model)
        elif verdict == "paid":
            dropped.append(f"{model['id']}(paid)")
        else:
            # Inconclusive. Excluded from the emit (we cannot route to a model
            # we have not seen succeed) but reported, so a capacity blip is
            # visible instead of silently shrinking the chain.
            unknown.append(f"{model['id']}({verdict})")

    if dropped:
        print(
            "CommandCode: dropped no-longer-free deals — " + ", ".join(dropped),
            file=sys.stderr,
        )
    if unknown:
        print(
            "CommandCode: inconclusive, excluded this run (transient, NOT a "
            "verdict that the deal ended) — " + ", ".join(unknown),
            file=sys.stderr,
        )
    print(
        f"CommandCode: {len(free_models)}/{len(candidates)} candidates verified free",
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


# ── Runtime free-tier veto (the daemon's cooldowns.json) ──────────────────────
#
# Everything above asks the PROVIDERS what is free. Nothing asked the GATEWAY,
# which is the only party that sees what happens when real client traffic
# reaches an endpoint -- and on 2026-10-04 that is exactly where the free-tier
# losses were visible:
#
#   ollama:minimax-m3                        402 x4   "this model is not
#                                                     included in your free
#                                                     usage"
#   opencode:muse-spark-1.3-contributor-free 403 x42  FreeTierError
#   nvidia{,2,3}:moonshotai/kimi-k2.6        404 x1   Function '<uuid>': Not
#                                                     found for account '...'
#
# None of the three could be fixed by editing the lists above:
#
#   * Ollama's free set is CURATED here and fetch_ollama makes no API call at
#     all, so a model that stops being free survives every sync forever. The
#     router kept it in the `smart`, `work` and `large` chains while every call
#     came back 402.
#   * The OpenCode probe cannot fail for these. Its body carries stream=true and
#     a tools array holding bash and read, which is precisely what the provider's
#     free-tier gate requires (proxy.go sends the same contract, and
#     scripts/fake-opencode-upstream.py enforces it), so the probe answered
#     `/responses 200` for muse-spark while the gateway was refused 42 times in a
#     row on requests whose body lacked them. A probe that cannot fail is not
#     evidence -- see the evidence ranking below.
#   * NVIDIA lists moonshotai/kimi-k2.6 in /v1/models while every completion
#     404s on a deleted function. Only the gateway ever sees that.
#
# cooldowns.json is the daemon's own record of exactly this (main.go resolves it
# beside the config file and the router writes it atomically), so the fix is to
# READ it instead of trying to out-guess the provider with a synthetic call.
# The veto is deliberately one-directional and self-healing:
#
#   * It only ever removes a model from the OUTPUT. It never edits a curated
#     list, so removing the evidence can never require a code change to undo.
#   * A refusal has to be RECURRING and its status/body has to say the model is
#     no longer served for free. A 429, a 5xx, a timeout, or a lone 4xx is a
#     property of the key or the network, and vetoing on those would empty the
#     chains during an outage.
#   * `circuits` is ignored: state 1 (open) and state 2 (half-open) describe a
#     probe in flight, not a verdict about the model.
#   * Denials are persisted with a TTL, so an entitlement that comes back
#     re-enters the chains by itself, without a human editing anything.
#
# EVIDENCE RANKING. The daemon's record outranks this script's own probe,
# because the daemon serves real client bodies and the probe sends one
# synthetic body it knows the gate accepts. So a live probe does not reinstate a
# model the gateway has been refused on repeatedly; the drop is reported loudly
# with both facts on the line, so the conflict is visible instead of resolved
# silently in either direction.
COOLDOWNS_FILE = Path(__file__).resolve().parent / "cooldowns.json"
RUNTIME_DENIAL_FILE = Path(__file__).resolve().parent / "free-tier-denied.json"
# Consecutive refusals before a free-tier refusal counts. The router escalates
# at 3 and at 10, so 3 is the first count at which it itself treats the endpoint
# as more than a blip.
RUNTIME_MIN_ERRORS = int(os.environ.get("AIROUTER_MIN_COOLDOWN_ERRORS", "3"))
# How long a recorded denial keeps vetoing the model after the last sighting.
# Long enough to outlast a weekend with no traffic (which produces no cooldowns
# to refresh it), short enough that a restored entitlement is not a month away.
RUNTIME_DENIAL_TTL_HOURS = int(os.environ.get("AIROUTER_DENIAL_TTL_HOURS", "168"))
# 402 Payment Required is the provider stating that the call is billed; with the
# count gate it is never acted on from a single occurrence.
FREE_TIER_REFUSAL_MARKERS = (
    "not included in your free usage",
    "free tier can only be used",
    "freetier",
    "not available in your country",
    "insufficient credit",
    "add usage credits",
    "upgrade for included usage",
    "payment required",
    "requires a subscription",
)
# Which config provider a JSON "provider" value is written under. Mirrors
# Model.mapped_provider in regenerate_config.py; the test named for that mirror
# fails if the two ever disagree.
CONFIG_PROVIDER_FOR_SOURCE = {
    "kilocode": "kilocode",
    "opencode": "opencode",
    "ollama-cloud": "ollama",
    "google-ai-studio": "gemini",
    "nvidia-nim": "nvidia",
    "commandcode": "commandcode",
}
# Multi-key families are written as one provider plus numbered siblings, and the
# cooldown key names whichever key actually failed. Deriving the siblings from
# the canonical name covers every family the config uses (nvidia/nvidia2/
# nvidia3, commandcode/commandcode2, gemini..gemini5) without hard-coding a
# list that a new key would silently fall out of.
_KEY_GROUP_MAX = 9


def config_provider_group(source: str) -> tuple[str, ...]:
    """Every config provider name a JSON source can be emitted under."""
    canonical = CONFIG_PROVIDER_FOR_SOURCE.get(source, source)
    return (canonical, *(f"{canonical}{n}" for n in range(2, _KEY_GROUP_MAX + 1)))


def load_cooldowns(path: Path) -> tuple[dict[str, dict[str, Any]], int]:
    """Read the daemon's cooldown envelope as {key: entry}, plus a junk count.

    Never raises. cooldowns.json is written by another process, so a missing,
    truncated, or concurrently-replaced file is a normal condition and must cost
    the veto nothing rather than the whole model list. A legacy bare
    map[string]CooldownEntry (which loadCooldowns still accepts) is read too.

    The `circuits` section is deliberately not returned: open and half-open are
    states of a probe in flight, and a model under a probe has told us nothing
    yet.
    """
    if not path.exists():
        return {}, 0
    try:
        raw = json.loads(path.read_text())
    except (json.JSONDecodeError, OSError, UnicodeDecodeError) as e:
        print(f"Cooldowns: {path.name} unreadable ({e}); no runtime veto applied",
              file=sys.stderr)
        return {}, 1
    if not isinstance(raw, dict):
        print(f"Cooldowns: {path.name} is not an object; no runtime veto applied",
              file=sys.stderr)
        return {}, 1
    section = raw.get("cooldowns")
    if not isinstance(section, dict):
        # Legacy bare map: every value is an entry.
        section = {k: v for k, v in raw.items() if isinstance(v, dict)}
    entries = {k: v for k, v in section.items()
               if isinstance(k, str) and isinstance(v, dict) and ":" in k}
    return entries, len(section) - len(entries)


def _parse_expiry(value: Any) -> datetime | None:
    """RFC3339 expiry as an aware datetime, or None when unusable."""
    if not isinstance(value, str) or not value:
        return None
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return None
    return parsed if parsed.tzinfo else parsed.replace(tzinfo=UTC)


def classify_cooldown(entry: dict[str, Any], now: datetime) -> tuple[str, str] | None:
    """(verdict, reason) when this cooldown is evidence the model left the free
    tier, else None.

    The bar is deliberately asymmetric, because the statuses are not:

      * A 404 is a statement about the model id -- the router itself gives it a
        seven-day cooldown at error_count 1 (baseCooldownForError), so one is
        enough. Verdict `gone`.
      * A 402 says the call is billed, but a 403 does not: OpenCode's gate
        rejects on the REQUEST SHAPE, so a 403 can mean "this client is not
        inside OpenCode" rather than "this model is paid now". Both therefore
        need the free-tier wording in the body AND RUNTIME_MIN_ERRORS
        consecutive refusals. Verdict `not-free`.
      * status_code 0 (transport error, timeout, "context deadline exceeded"),
        429, 5xx, and anything else are the key or the network talking. None of
        them is evidence about the model, and vetoing on them would strip the
        chains during an outage -- which is how a rate limit turns into a much
        worse incident than the one it replaced.
    """
    status = entry.get("status_code")
    try:
        status = int(status)
    except (TypeError, ValueError):
        return None
    expiry = _parse_expiry(entry.get("expiry"))
    if expiry is None or expiry <= now:
        # The daemon deletes an entry when it expires and on any success, so an
        # expired entry is not a current statement about anything.
        return None
    body = entry.get("last_error")
    body = body if isinstance(body, str) else ""
    low = body.lower()
    try:
        errors = int(entry.get("error_count") or 0)
    except (TypeError, ValueError):
        errors = 0

    if status == 404:
        return ("gone", f"404 not found ({errors}x)")
    if status == 402 and errors >= RUNTIME_MIN_ERRORS:
        marker = next((m for m in FREE_TIER_REFUSAL_MARKERS if m in low), None)
        detail = marker or "402 payment required"
        return ("not-free", f"402 {detail} ({errors}x)")
    if status == 403 and errors >= RUNTIME_MIN_ERRORS:
        marker = next((m for m in FREE_TIER_REFUSAL_MARKERS if m in low), None)
        if marker:
            return ("not-free", f"403 {marker} ({errors}x)")
    return None


def load_runtime_denials(now: datetime) -> dict[str, dict[str, Any]]:
    """Previously recorded denials that have not expired yet."""
    if not RUNTIME_DENIAL_FILE.exists():
        return {}
    try:
        raw = json.loads(RUNTIME_DENIAL_FILE.read_text())
        entries = raw.get("denials") if isinstance(raw, dict) else None
        if not isinstance(entries, dict):
            return {}
    except (json.JSONDecodeError, OSError, UnicodeDecodeError):
        return {}
    live = {}
    for key, entry in entries.items():
        if not isinstance(key, str) or not isinstance(entry, dict):
            continue
        until = _parse_expiry(entry.get("until"))
        if until is None or until <= now:
            continue
        if key.split(":", 1)[-1] in AUTO_FALLBACK_MODELS:
            continue
        live[key] = entry
    return live


def save_runtime_denials(denials: dict[str, dict[str, Any]], now: datetime) -> None:
    """Persist the denial set. Best-effort: a write failure is not a sync failure."""
    payload = {
        "updated_at": now.isoformat().replace("+00:00", "Z"),
        "ttl_hours": RUNTIME_DENIAL_TTL_HOURS,
        "denials": denials,
    }
    try:
        RUNTIME_DENIAL_FILE.write_text(json.dumps(payload, indent=2, sort_keys=True))
    except OSError as e:
        print(f"Cooldowns: could not write {RUNTIME_DENIAL_FILE.name}: {e}", file=sys.stderr)


def runtime_free_tier_denials(
    cooldowns_path: Path | None = None, now: datetime | None = None,
) -> dict[str, dict[str, Any]]:
    """`provider:model` -> denial record, from cooldowns.json and from history.

    Two inputs, because either alone is incomplete. cooldowns.json only holds
    entries that are both unexpired and recent, so a model nobody routed to for a
    week would forget it was paid -- and the next sync would put it straight back
    in the chains. The persisted denials therefore outlive their evidence: an
    entry is refreshed while the daemon keeps reporting the refusal, and kept
    until `until` passes. Nothing clears a denial early, because the daemon
    deletes the entry on the first success too and an absent key cannot be told
    apart from "never tried".
    """
    now = now or datetime.now(UTC)
    denials = load_runtime_denials(now)
    entries, junk = load_cooldowns(cooldowns_path or COOLDOWNS_FILE)
    until = (now.timestamp() + RUNTIME_DENIAL_TTL_HOURS * 3600)
    until_iso = datetime.fromtimestamp(until, tz=UTC).isoformat().replace("+00:00", "Z")
    now_iso = now.isoformat().replace("+00:00", "Z")

    fresh = transient = 0
    for key, entry in entries.items():
        verdict = classify_cooldown(entry, now)
        if verdict is None:
            transient += 1
            continue
        kind, reason = verdict
        fresh += 1
        body = entry.get("last_error")
        denials[key] = {
            "verdict": kind,
            "reason": reason,
            "status_code": entry.get("status_code"),
            "error_count": entry.get("error_count"),
            "last_error": (body[:200] + "..." if isinstance(body, str) and len(body) > 200
                           else body),
            "last_seen": now_iso,
            "until": until_iso,
        }
    save_runtime_denials(denials, now)

    ignored = transient + junk
    print(
        f"Cooldowns: {len(entries)} cooldowns read from "
        f"{(cooldowns_path or COOLDOWNS_FILE).name}; {fresh} free-tier refusal(s), "
        f"{len(denials)} active denial(s)"
        + (f", {ignored} ignored (transient/malformed)" if ignored else "")
        + (f"; min {RUNTIME_MIN_ERRORS} consecutive refusals required" if transient else ""),
        file=sys.stderr,
    )
    return denials


def format_cooldown_report(denials: dict[str, dict[str, Any]], path: Path | None = None) -> str:
    """Human report of what the gateway has been refused, for the sync log."""
    path = path or COOLDOWNS_FILE
    if not denials:
        return (f"free-tier refusals: none recorded "
                f"({path.name} absent, empty, or nothing in it says a model left "
                f"the free tier)")
    lines = [f"free-tier refusals from {path.name} "
             f"({RUNTIME_DENIAL_TTL_HOURS}h veto window, "
             f"min {RUNTIME_MIN_ERRORS} consecutive refusals):"]
    for key in sorted(denials):
        d = denials[key]
        lines.append(f"  {key}: {d.get('verdict')} -- {d.get('reason')} "
                     f"(last seen {d.get('last_seen')}, until {d.get('until')})")
    return "\n".join(lines)


def apply_runtime_denials(models: list[dict[str, Any]],
                          denials: dict[str, dict[str, Any]]) -> list[dict[str, Any]]:
    """Drop every model the gateway has been refused on, and say which and why.

    Returns the input list untouched when there is nothing to drop or when
    dropping would empty it. An empty model list means regenerate_config.py has
    no chains to write, so a mis-set threshold or a mis-shaped cooldowns.json
    must cost a warning, never the config.
    """
    if not denials or not models:
        return models
    kept: list[dict[str, Any]] = []
    dropped = 0
    conflicts = 0
    for model in models:
        model_id = model.get("id", "")
        source = model.get("provider", "")
        if model_id in AUTO_FALLBACK_MODELS:
            # The chain terminators are decided on a probe verdict, which
            # already switches routers on a refusal (sync-instruction rule 6).
            # Removing one here would leave regenerate_config.py with no
            # terminator at all, which makes it refuse to write.
            kept.append(model)
            continue
        hit = next((f"{p}:{model_id}" for p in config_provider_group(source)
                    if f"{p}:{model_id}" in denials), None)
        if hit is None:
            kept.append(model)
            continue
        dropped += 1
        detail = denials[hit]
        note = ""
        if model.get("verified") is True:
            conflicts += 1
            note = ("  [this run's probe answered OK -- the daemon's record of real "
                    "traffic wins, see the evidence ranking in fetch-free-models.py]")
        print(f"Free tier: dropping {hit} ({detail.get('reason')}){note}", file=sys.stderr)
    if not kept:
        print("Cooldowns: !!! every model would be vetoed -- veto NOT applied. "
              "The gateway has refused the whole catalog, which is an outage or a "
              "mis-read cooldowns.json, not a free-tier change. Keeping the list.",
              file=sys.stderr)
        return models
    if dropped:
        # The conflict count belongs in the summary, not just on the drop lines:
        # a reader who only sees the last line should still learn that this run's
        # own probe disagreed with the daemon's record on N of them.
        clash = (f", {conflicts} of which this run's probe called live "
                 f"(see the evidence ranking in this file's header)"
                 if conflicts else "")
        print(f"Free tier: {dropped} model(s) vetoed by runtime cooldowns{clash}, "
              f"{len(kept)} kept", file=sys.stderr)
    return kept


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
    parser.add_argument(
        "--cooldowns",
        metavar="FILE",
        default=str(COOLDOWNS_FILE),
        help="The gateway's cooldowns.json. Endpoints it records as repeatedly "
             "refused for free-tier reasons (402 billing, 403 free-tier wording, "
             "404 gone) are vetoed from the model list whatever any provider "
             "listing says. Default: the cooldowns.json beside this script.",
    )
    parser.add_argument(
        "--no-cooldown-veto",
        action="store_true",
        help="Ignore cooldowns.json and emit every candidate the providers list. "
             "Re-admits models the running gateway is being refused on, so it must "
             "not be left set in a scheduled sync.",
    )
    parser.add_argument(
        "--cooldowns-report",
        action="store_true",
        help="Print what cooldowns.json says about free-tier availability and "
             "exit. No network, no provider work: this is the sync's pre-flight "
             "check, runnable when every provider is down.",
    )
    args = parser.parse_args()

    if args.cooldowns_report:
        # Before check_opencode_ua_version(): the report must be answerable when
        # the whole internet is unreachable, and the UA advisory needs the npm
        # registry to say anything at all.
        print(format_cooldown_report(runtime_free_tier_denials(Path(args.cooldowns))))
        return

    # Before any provider work: the UA version is independent of every provider
    # and its key, so this still reports on a run where nothing else is
    # reachable. Advisory — never affects the exit code or the model list.
    check_opencode_ua_version()

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

    # Runtime free-tier veto. Last of the filters and before the auto-probe, so
    # the candidates that survive it are exactly the ones the terminator probe
    # and the output see. Reading it here rather than per-provider is deliberate:
    # one hook covers every source, including the CURATED ones (Ollama makes no
    # API call, so nothing upstream could ever have reported minimax-m3's 402).
    if args.no_cooldown_veto:
        print("Cooldowns: !!! --no-cooldown-veto — emitting every candidate the "
              "providers list, including endpoints the gateway records as refused "
              "for free-tier reasons. Do not leave this set in a scheduled sync.",
              file=sys.stderr)
    else:
        all_models = apply_runtime_denials(
            all_models, runtime_free_tier_denials(Path(args.cooldowns))
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
