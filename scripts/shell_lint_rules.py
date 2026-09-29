#!/usr/bin/env python3
"""Rules for scripts/shell-lint.sh.

Kept separate from the driver so the logic can be tested directly with known
bad and known good snippets, which is the only way to know a linter works.

Exits 0 when clean, 1 when it has violations (printed to stdout), 2 when the
file cannot be read.
"""
import re
import sys


def code_only(line, in_single, in_double):
    """Reduce a line to the parts that are actually CODE.

    String contents are BLANKED, not preserved, and comments are dropped. That
    matters for both rules: a `while ... then` inside a string is a literal, not
    code, and flagging it would make the linter wrong on correct scripts. A
    regex run over the raw line cannot tell the difference; this can.

    Returns (code, in_single, in_double) where the quote-state flags carry
    across lines, since a shell string may span lines.
    """
    out = []
    i = 0
    n = len(line)
    while i < n:
        c = line[i]
        if in_single:
            if c == "'":
                in_single = False
            out.append(" ")
            i += 1
            continue
        if in_double:
            if c == "\\" and i + 1 < n:
                out.append("  ")
                i += 2
                continue
            if c == '"':
                in_double = False
            out.append(" " if c != '"' else '"')
            i += 1
            continue
        if c == "'":
            in_single = True
            out.append("'")
        elif c == '"':
            in_double = True
            out.append('"')
        elif c == "#" and (i == 0 or line[i - 1] in " \t|&;()"):
            break
        else:
            out.append(c)
        i += 1
    return "".join(out), in_single, in_double


# `while`/`until` must be terminated by `do`. Matches both the `while [ ... ];`
# and the arithmetic `while (( ... ));` forms, and `until` likewise. `select` and
# `function` also take `do` but are not constructs anyone writes wrong here, and
# `if`/`case` legitimately take `then`, so neither appears below.
BAD_TERMINATOR = re.compile(r"\b(?:while|until)\b[^;#]*;\s*then\b")


def check(path):
    try:
        with open(path, "r", encoding="utf-8", errors="replace") as fh:
            lines = fh.read().split("\n")
    except OSError as exc:
        print("shell-lint: cannot read %s: %s" % (path, exc), file=sys.stderr)
        return 2

    violations = []
    in_single = False
    in_double = False
    for lineno, raw in enumerate(lines, 1):
        code, in_single, in_double = code_only(raw, in_single, in_double)
        if BAD_TERMINATOR.search(code):
            violations.append((lineno, "`while`/`until` terminated by `then`"))

    for lineno, msg in violations:
        print("%s:%d: %s" % (path, lineno, msg))
        print("    `while` and `until` take `do`; use `; do` not `; then`")
    return 1 if violations else 0


def main():
    if len(sys.argv) != 2:
        print("usage: shell_lint_rules.py <file>", file=sys.stderr)
        return 2
    return check(sys.argv[1])


if __name__ == "__main__":
    sys.exit(main())
