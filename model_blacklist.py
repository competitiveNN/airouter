#!/usr/bin/env python3
"""Permanent blacklist: models gone upstream never reappear.

A cooldowns.json entry 404 / "not found" with a release date older than
two weeks => model is permanently blacklisted. They never appear in
fetch-free-models.py output and never make it into config.yaml.
"""
import json
import sys
from datetime import datetime, timezone, timedelta
from pathlib import Path

REPO = Path(__file__).resolve().parent
BLACKLIST_FILE = REPO / "blacklisted-models.json"
COOLDOWNS_FILE = REPO / "cooldowns.json"
RUNTIME_MIN_AGE_DAYS = 14


def load_blacklist(path=None):
    path = Path(path or BLACKLIST_FILE)
    if not path.exists():
        return {}
    try:
        data = json.load(path.open())
    except (json.JSONDecodeError, OSError):
        return {}
    entries = data.get("entries", {}) if isinstance(data, dict) else {}
    if not isinstance(entries, dict):
        return {}
    for k in list(entries):
        if not isinstance(entries[k], dict):
            entries[k] = {"reason": str(entries[k]), "date": datetime.now(timezone.utc).date().isoformat()}
    return entries


def save_blacklist(entries, path=None):
    path = Path(path or BLACKLIST_FILE)
    out = {"entries": entries}
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
    """Scan cooldowns.json, add 404/not-found models older than 2 weeks to blacklist.

    Returns the list of newly blacklisted keys.
    """
    # ---- read cooldowns.json ----
    cp = Path(cooldowns_path or COOLDOWNS_FILE)
    if not cp.exists():
        return []
    try:
        raw = json.load(cp.open())
    except (json.JSONDecodeError, OSError):
        return []
    # two possible shapes:
    # { "provider:model": {...}, ... }
    # { "cooldowns": { "provider:model": {...}, ... }, "circuits": {...} }
    if isinstance(raw, dict) and "cooldowns" in raw:
        raw = raw["cooldowns"]
    if not isinstance(raw, dict):
        return []
    cooldowns = raw

    # ---- read existing blacklist ----
    bp = Path(blacklist_path or BLACKLIST_FILE)
    blacklist = load_blacklist(bp)

    # ---- index models from the fetched list ----
    # models is a list dicts with "provider" (source), "id", "released"
    index = {}
    for m in models:
        prov = m.get("provider", "")
        mid = m.get("id", "")
        index.setdefault((prov, mid), m)

    today = datetime.now(timezone.utc).date()
    cutoff = today - timedelta(days=RUNTIME_MIN_AGE_DAYS)
    added = []

    # ---- scan each cooldown entry ----
    for key, entry in cooldowns.items():
        if not isinstance(entry, dict):
            continue
        if not _not_found_check(entry):
            continue
        # key format: "<config-provider>:<model-id>"
        if ":" in key:
            prov_cfg, model_id = key.split(":", 1)
        else:
            prov_cfg, model_id = "", key
        # map config provider name to fetcher source name
        src = source_for_config_provider(prov_cfg)
        m = None
        # try exact source match first
        m = index.get((src, model_id))
        # fall back to any matching model id
        if m is None:
            for (p, mid), rec in index.items():
                if mid == model_id:
                    m = rec
                    break
        if m is None:
            continue
        # parse release date
        released = _parse_release_date(m.get("released") or m.get("provider_released"))
        if released is None:
            continue
        # older than two weeks => blacklist
        if released <= cutoff:
            # blacklist key uses source provider so it matches filter_models()
            blkey = f"{m.get('provider', src)}:{model_id}"
            if blkey not in blacklist:
                blacklist[blkey] = {"reason": "not found (older than 2 weeks)",
                                   "date": datetime.now(timezone.utc).date().isoformat(),
                                   "model_id": model_id,
                                   "provider": m.get("provider", src),
                                   "released": (str(released) if released else None),
                                   "cooldown_keys": [key],
                                   "blacklisted_at": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
                                   "status_code": entry.get("status_code")}
                added.append(blkey)

    # ---- save blacklist ----
    save_blacklist(blacklist, bp)
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
        print(json.dumps(load_blacklist(), indent=2))
        sys.exit(0)
    if args.update and args.models_json:
        models = json.load(open(args.models_json))
        added = update_blacklist_from_cooldowns(models, cooldowns_path=args.cooldowns,
                                               blacklist_path=args.blacklist_path)
        print(f"Newly blacklisted ({len(added)}): {added}")
    else:
        # default: just show
        bl = load_blacklist()
        print(json.dumps(bl, indent=2))

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

