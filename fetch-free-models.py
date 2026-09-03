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
OPENCODE_ENDPOINT = "https://opencode.ai/zen/v1/models"
GOOGLE_AI_STUDIO_ENDPOINT = "https://generativelanguage.googleapis.com/v1beta/models"
NVIDIA_NIM_ENDPOINT = "https://integrate.api.nvidia.com/v1/models"
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
def fetch_json(url: str, attempt: int = 1) -> dict[str, Any] | list[Any] | None:
    """Fetch JSON with retry on 429/5xx and timeout handling."""
    req = urllib.request.Request(url, headers=HEADERS)
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
    """Convert Kilo model format to common schema."""
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
        "released": model.get("created") or model.get("release_date") or model.get("published_at"),
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
    """Convert OpenCode model format to common schema."""
    model_id = model.get("id", "")
    return {
        "id": model_id,
        "name": model_id,
        "provider": "opencode",
        "context_length": OPENCODE_FREE_MODELS_CTX.get(model_id, 0),
        "intelligence": None,
        "elo": None,
        "released": model.get("created") or model.get("release_date") or model.get("published_at"),
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


def fetch_opencode() -> list[dict[str, Any]]:
    """Fetch OpenCode models; free ones end in -free or are in OPENCODE_FREE_MODELS."""
    print("Fetching from OpenCode API...", file=sys.stderr)
    data = fetch_json(OPENCODE_ENDPOINT)
    if not data or not isinstance(data, dict) or "data" not in data:
        print("OpenCode: no data or unexpected format", file=sys.stderr)
        return []

    free_models = []
    for model in data.get("data", []):
        model_id = model.get("id", "")
        if model_id.endswith("-free") or model_id in OPENCODE_FREE_MODELS:
            normalized = normalize_opencode(model)
            if normalized:
                free_models.append(normalized)

    print(f"OpenCode: found {len(free_models)} free models (only '-free' suffix kept)", file=sys.stderr)
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
        description="Fetch free models from Kilo Code, OpenCode, Ollama Cloud, Google AI Studio, and NVIDIA NIM APIs"
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
    args = parser.parse_args()

    # Default to table if no format specified
    if not (args.json or args.csv or args.table):
        args.table = True

    # Validate mutually exclusive flags
    only_count = sum([args.kilocode_only, args.opencode_only, args.ollama_only, args.google_ai_studio_only, args.nvidia_nim_only])
    if only_count > 1:
        print(
            "Error: --kilocode-only, --opencode-only, --ollama-only, --google-ai-studio-only, and --nvidia-nim-only are mutually exclusive",
            file=sys.stderr,
        )
        sys.exit(1)

    # Fetch data
    all_models = []

    if not args.opencode_only and not args.ollama_only and not args.google_ai_studio_only and not args.nvidia_nim_only:
        kilo_models = fetch_kilo()
        all_models.extend(kilo_models)

    if not args.kilocode_only and not args.ollama_only and not args.google_ai_studio_only and not args.nvidia_nim_only:
        opencode_models = fetch_opencode()
        all_models.extend(opencode_models)

    if not args.kilocode_only and not args.opencode_only and not args.google_ai_studio_only and not args.nvidia_nim_only:
        ollama_models = fetch_ollama()
        all_models.extend(ollama_models)

    if not args.kilocode_only and not args.opencode_only and not args.ollama_only:
        google_models = fetch_google_ai_studio()
        all_models.extend(google_models)

    if not args.kilocode_only and not args.opencode_only and not args.ollama_only and not args.google_ai_studio_only:
        nvidia_models = fetch_nvidia_nim()
        all_models.extend(nvidia_models)

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

    # Per-source breakdown in the summary line
    by_source: dict[str, int] = {}
    for m in all_models:
        by_source.setdefault(m.get("source", "?"), 0)
        by_source[m["source"]] += 1
    summary = ", ".join(f"{k}={v}" for k, v in sorted(by_source.items()))
    print(f"Total models: {len(all_models)} ({summary})", file=sys.stderr)

    # Output
    if args.json:
        output_json(all_models, args.save)
    elif args.csv:
        output_csv(all_models, args.save)
    else:
        output_table(all_models)


if __name__ == "__main__":
    main()
