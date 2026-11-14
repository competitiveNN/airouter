#!/usr/bin/env python3
"""Permanent blacklist: models gone upstream never reappear.

A cooldown row (404 / "not found") with a release date older than two weeks
=> model is permanently blacklisted. They never appear in fetch-free-models.py
output and never make it into config.yaml.

The gateway records runtime refusals in cooldowns.db (SQLite). It used to
write cooldowns.json, so the reader below accepts both: an existing JSON file
is still parsed (and is what the first daemon start migrates away from).
"""
import json
import sqlite3
import sys
from datetime import datetime, timezone, timedelta
from pathlib import Path

REPO = Path(__file__).resolve().parent
BLACKLIST_FILE = REPO / "blacklisted-models.json"
# Current state store. The constant keeps its historical name so the fetcher's
# import does not need a second spelling.
COOLDOWNS_FILE = REPO / "cooldowns.db"
LEGACY_COOLDOWNS_FILE = REPO / "cooldowns.json"
RUNTIME_MIN_AGE_DAYS = 14


def load_cooldown_entries(path=None):
    """Read cooldown rows from the SQLite state database, or a legacy JSON file.

    Returns {"provider:model": {"status_code": int|None, "last_error": str}}.
    A missing or unreadable store is "no evidence" (empty dict), never an
    exception: the sync is a read-only consumer of state the daemon owns.
    """
    p = Path(path or COOLDOWNS_FILE)
    if not p.exists() and p == COOLDOWNS_FILE and LEGACY_COOLDOWNS_FILE.exists():
        p = LEGACY_COOLDOWNS_FILE
    if not p.exists():
        return {}
    try:
        with p.open("rb") as fh:
            header = fh.read(16)
    except OSError:
        return {}

    if not header.startswith(b"SQLite format 3\x00"):
        # Legacy JSON: either {"cooldowns": {...}, "circuits": {...}} or a
        # bare map written by releases that predate circuits.
        try:
            raw = json.loads(p.read_text())
        except (json.JSONDecodeError, OSError):
            return {}
        if isinstance(raw, dict) and "cooldowns" in raw:
            raw = raw["cooldowns"]
        return raw if isinstance(raw, dict) else {}

    try:
        conn = sqlite3.connect(f"file:{p}?mode=ro", uri=True, timeout=5)
        try:
            rows = conn.execute(
                "SELECT key, status_code, last_error FROM cooldowns"
            ).fetchall()
        finally:
            conn.close()
    except sqlite3.Error:
        return {}
    return {
        str(key): {"status_code": code, "last_error": err or ""}
        for key, code, err in rows
    }


def load_blacklist(path=None, include_pending=False):
    """Load blacklist entries and optionally pending streak tracking from JSON file.

    By default (include_pending=False) returns only the entries dict for backward compatibility.
    When include_pending=True, returns dict with 'entries' and 'pending' keys.
    """
    path = Path(path or BLACKLIST_FILE)
    if not path.exists():
        if include_pending:
            return {"entries": {}, "pending": {}}
        return {}
    try:
        data = json.load(path.open())
    except (json.JSONDecodeError, OSError):
        if include_pending:
            return {"entries": {}, "pending": {}}
        return {}
    if not isinstance(data, dict):
        if include_pending:
            return {"entries": {}, "pending": {}}
        return {}
    entries = data.get("entries", {})
    pending = data.get("pending", {})
    if not isinstance(entries, dict):
        entries = {}
    if not isinstance(pending, dict):
        pending = {}
    for k in list(entries):
        if not isinstance(entries[k], dict):
            entries[k] = {"reason": str(entries[k]), "date": datetime.now(timezone.utc).date().isoformat()}
    if include_pending:
        return {"entries": entries, "pending": pending}
    return entries


def save_blacklist(entries, path=None, pending=None):
    path = Path(path or BLACKLIST_FILE)
    out = {"entries": entries, "pending": pending or {}}
    path.write_text(json.dumps(out, indent=2, sort_keys=True) + "\n")


def _parse_release_date(value):
    if not value:
        return None
    try:
        if isinstance(value, str):
            if value.count("-") >= 2 and len(value) == 10:
                return datetime.strptime(value, "%Y-%m-%d").date()
            if value.count("-") == 1 and len(value) == 7:
                return datetime.strptime(value, "%Y-%m").date()
        if isinstance(value, (int, float)):
            return datetime.fromtimestamp(value, tz=timezone.utc).date()
    except (ValueError, OSError, TypeError):
        pass
    return None


def _not_found_check(e):
    """True when a cooldown entry is a 404 or 'not found' error."""
    if not isinstance(e, dict):
        return False
    code = e.get("status_code")
    if code is not None and code == 404:
        return True
    msg = (e.get("last_error") or "").lower()
    if "not found" in msg:
        return True
    # also look in raw error text like {"status":404,"title":"Not Found",...}
    raw = str(e.get("last_error") or "")
    if '404' in raw and 'not' in raw.lower():
        return True
    return False


def _find_model(models_by_id, model_id):
    return models_by_id.get(model_id)


def update_blacklist_from_cooldowns(models, cooldowns_path=None, blacklist_path=None):
    """Scan the cooldown store, add 404/not-found models older than 2 weeks,
    requiring 404s on 2 consecutive days (tracked via streak/date in blacklist entry).

    Returns the list of newly blacklisted keys.
    """
    cooldowns = load_cooldown_entries(cooldowns_path)
    if not cooldowns:
        return []

    # ---- read existing blacklist ----
    bp = Path(blacklist_path or BLACKLIST_FILE)
    bl_data = load_blacklist(bp, include_pending=True)
    entries = bl_data.get("entries", {})
    pending = bl_data.get("pending", {})

    # ---- index models from the fetched list ----
    index = {}
    for m in models:
        prov = m.get("provider", "")
        mid = m.get("id", "")
        index.setdefault((prov, mid), m)

    # ---- multi-endpoint providers: require 404 on at least 2 endpoints ----
    MULTI_ENDPOINT_PREFIXES = ("nvidia", "gemini", "opencode", "commandcode")
    not_found_by_model = {}
    for key, entry in cooldowns.items():
        if not isinstance(entry, dict):
            continue
        if not _not_found_check(entry):
            continue
        if ":" in key:
            prov_cfg, model_id = key.split(":", 1)
        else:
            prov_cfg, model_id = "", key
        src = source_for_config_provider(prov_cfg)
        not_found_by_model.setdefault((src, model_id), []).append(prov_cfg)

    today = datetime.now(timezone.utc).date()
    today_iso = today.isoformat()
    cutoff = today - timedelta(days=RUNTIME_MIN_AGE_DAYS)
    added = []

    # ---- scan each cooldown entry ----
    for key, entry in cooldowns.items():
        if not isinstance(entry, dict):
            continue
        if not _not_found_check(entry):
            continue
        if ":" in key:
            prov_cfg, model_id = key.split(":", 1)
        else:
            prov_cfg, model_id = "", key
        src = source_for_config_provider(prov_cfg)
        m = None
        m = index.get((src, model_id))
        if m is None:
            for (p, mid), rec in index.items():
                if mid == model_id:
                    m = rec
                    break
        if m is None:
            continue
        if m.get("verified") is True:
            continue
        if m.get("provider") == "commandcode":
            mid = m.get("id", "")
            if mid.lower().startswith("stealth/") or mid.lower().endswith("-free") or mid.lower().endswith(":free"):
                continue

        # OpenCode never answers a genuine model removal with 404: a missing
        # model comes back as 400 "Model is unavailable". A 404 with a generic
        # "Not Found"/"page not found" body is the upstream HOST answering for a
        # route it does not know (wrong base URL, misrouted request) -- not a
        # model-not-found verdict, so it must never feed the blacklist.
        if src == "opencode":
            msg = str(entry.get("last_error") or "").strip().lower()
            if msg in ("", "not found", "page not found") and entry.get("status_code") == 404:
                continue

        # For multi-endpoint providers, require 404 on at least 2 endpoints
        # before even tracking a pending streak.
        is_multi = any(prov_cfg.lower().startswith(p) for p in MULTI_ENDPOINT_PREFIXES)
        if is_multi:
            endpoints_with_404 = set(not_found_by_model.get((src, model_id), []))
            if len(endpoints_with_404) < 2:
                continue

        released = _parse_release_date(m.get("released") or m.get("provider_released"))
        if released is None:
            continue
        if released <= cutoff:
            blkey = f"{m.get('provider', src)}:{model_id}"

            # Already blacklisted, nothing to do
            if blkey in entries:
                continue

            # Track consecutive 404 days in pending
            p = pending.get(blkey)
            if p is None:
                # First time seeing 404 for this model - start streak
                pending[blkey] = {"first_seen": today_iso, "last_seen": today_iso, "streak": 1}
            else:
                last_seen = datetime.strptime(p.get("last_seen", today_iso), "%Y-%m-%d").date()
                # If last_seen is yesterday, increment streak
                if last_seen == today - timedelta(days=1):
                    new_streak = p.get("streak", 1) + 1
                # If last_seen is today, keep same streak (don't double-count same day)
                elif last_seen == today:
                    new_streak = p.get("streak", 1)
                # If gap > 1 day, reset streak
                else:
                    new_streak = 1
                pending[blkey] = {"first_seen": p.get("first_seen", today_iso), "last_seen": today_iso, "streak": new_streak}

                # If streak >= 2 consecutive days, promote to blacklist
                if new_streak >= 2:
                    # collect all cooldown keys for this model across endpoints
                    all_keys = [k for k in cooldowns if (":" in k and k.split(":", 1)[1] == model_id) or (":" not in k and k == model_id)]
                    entries[blkey] = {"reason": "not found (older than 2 weeks)",
                                     "date": today_iso,
                                     "model_id": model_id,
                                     "provider": m.get("provider", src),
                                     "released": (str(released) if released else None),
                                     "cooldown_keys": all_keys if all_keys else [key],
                                     "blacklisted_at": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
                                     "status_code": entry.get("status_code")}
                    added.append(blkey)
                    # Remove from pending since now permanently blacklisted
                    del pending[blkey]

    # ---- save blacklist with updated pending tracking ----
    save_blacklist(entries, bp, pending)
    return added


# CLI
if __name__ == "__main__":
    import argparse
    parser = argparse.ArgumentParser()
    parser.add_argument("--update", action="store_true")
    parser.add_argument("--cooldowns", default=str(COOLDOWNS_FILE))
    parser.add_argument("--models-json", default=None,
                        help="Path to free-models.json (list of dicts with provider/id/released)")
    parser.add_argument("--blacklist-path", default=str(BLACKLIST_FILE))
    parser.add_argument("--show", action="store_true")
    args = parser.parse_args()
    if args.show:
        bl_data = load_blacklist(args.blacklist_path)
        print(json.dumps(bl_data, indent=2))
        sys.exit(0)
    if args.update and args.models_json:
        models = json.load(open(args.models_json))
        added = update_blacklist_from_cooldowns(models, cooldowns_path=args.cooldowns,
                                               blacklist_path=args.blacklist_path)
        print(f"Newly blacklisted ({len(added)}): {added}")
    else:
        # default: just show
        bl_data = load_blacklist(args.blacklist_path)
        print(json.dumps(bl_data, indent=2))

def is_blacklisted(provider, model_id, entries):
    """Whether a (fetcher-provider, model-id) pair is in the blacklist."""
    if not entries:
        return False
    key = f"{provider}:{model_id}"
    if key in entries:
        return True
    for e in entries.values():
        if isinstance(e, dict) and e.get("model_id") == model_id:
            return True
    return False

def source_for_config_provider(provider: str) -> str:
    """Map a config provider to its fetcher-source counterpart.

    Pulled verbatim from the mapping in fetch-free-models.py's normaliser:
        nvidia*    -> nvidia-nim
        gemini*    -> google-ai-studio
        ollama*    -> ollama-cloud
        commandcode* -> commandcode
        kilocode   -> kilocode
        opencode   -> opencode
    """
    p = (provider or "").lower()
    if p.startswith("nvidia"):
        return "nvidia-nim"
    if p.startswith("gemini"):
        return "google-ai-studio"
    if p.startswith("ollama"):
        return "ollama-cloud"
    if p.startswith("commandcode"):
        return "commandcode"
    return p



def filter_models(models, blacklist):
    """Remove entries whose (provider, id) is permanently blacklisted."""
    if not blacklist:
        return list(models)
    out = []
    for m in models:
        prov = m.get("provider", "")
        mid = m.get("id", "")
        if is_blacklisted(prov, mid, blacklist):
            continue
        out.append(m)
    return out

