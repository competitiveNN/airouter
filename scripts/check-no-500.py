#!/usr/bin/env python3
"""CI guard: no production handler may write or pass through HTTP 500."""
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
bad = False
for path in ROOT.glob("*.go"):
    if path.name.endswith("_test.go"):
        continue
    for i, line in enumerate(path.read_text().splitlines(), 1):
        s = line.strip()
        if "http.StatusInternalServerError" in s and ">=" not in s and "<=" not in s:
            print(f"{path}:{i}: forbidden http.StatusInternalServerError: {s}")
            bad = True
        for pat in (r"WriteHeader\(500\)", r"writeAPIError\(w,\s*500", r"w\.WriteHeader\(500\)"):
            if re.search(pat, s):
                print(f"{path}:{i}: forbidden client-visible 500: {s}")
                bad = True
if bad:
    sys.exit(1)
print("ok: no client-visible HTTP 500 in production code")