#!/usr/bin/env bash
# Prove scripts/test-suite-check.py can fail, and that it fails for the reasons
# it claims -- including the two ways IT was wrong while being written.
#
# WHY THIS EXISTS
#
# test-suite-check.py exists because `go test ./...` reports success equally for
# "every test passed" and "there are forty fewer tests than yesterday", and this
# repository has already lost test code to truncation once. The check was then
# run, came back green, and that green was worthless -- during those first
# minutes it had already produced two false reports, both of which would have
# cost the next reader real time:
#
#   - it reported a test as MISSING FROM THE REPOSITORY. The test is sitting in
#     metrics_cardinality_test.go. A brace-matching body scan had swallowed a
#     thousand lines of the file into a raw string of Prometheus exposition text.
#   - it reported two cooldown tests as UNABLE TO FAIL. Both contain t.Fatalf.
#     Their bodies embed JSON in a raw string, and gofmt cannot stop a raw string
#     from containing a line that starts with '}', which is exactly where the
#     body scan decided the function had ended.
#
# Both were caught by reading the flagged code, not by any mechanism. A checker
# that invents missing tests and invents vacuous ones is worse than no checker:
# every false report sends someone to hunt a bug that does not exist. Cases 5 and
# 6 below pin those two bugs and assert the check does NOT fire on them.
#
# Cases 2-4 are the other half: a check that cannot report a lost test, or cannot
# report a test that stopped asserting, only ever says "fine".
#
# Every case mutates a FIXTURE in a temporary directory, never this repository's
# test files. Mutating main_test.go to prove a point -- a 182 KB file, with a
# 205-test suite depending on it -- is not a trade worth making.
#
# Exit 0 = every case behaved as specified. Exit 1 = at least one did not.
# Exit 2 = the harness could not run.
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHECK="$REPO/scripts/test-suite-check.py"

#   case 1 (2)  a sound suite passes, for the right reason (it saw all 6 tests)
#   case 2 (2)  deleting a test is caught and named
#   case 3 (2)  ADDING a test is not a failure -- the contract is a superset, so
#               it never taxes the person writing a test
#   case 4 (2)  a test stripped of every failure path is caught and named
#   case 5 (2)  the two bugs do not fire and the suite is clean again: asserting
#               through a helper that takes more parameters than just t, and a
#               body that follows a line beginning with '}' inside a raw string
#   case 6 (1)  a truncated test file names every test that went missing
#   case 7 (1)  no baseline: the check refuses rather than reporting success
#   case 8 (1)  a suite that does not build: refuses, rather than reporting that
#               the suite shrank
EXPECTED=13

command -v python3 >/dev/null 2>&1 || { echo "test-suite-selftest: python3 not found" >&2; exit 2; }
command -v go >/dev/null 2>&1 || { echo "test-suite-selftest: go not found; the fixture cannot be listed" >&2; exit 2; }
[ -r "$CHECK" ] || { echo "test-suite-selftest: cannot read $CHECK" >&2; exit 2; }

TMP=$(mktemp -d "${TMPDIR:-/tmp}/test-suite-selftest.XXXXXX") || exit 2
trap 'rm -rf "$TMP"' EXIT

PASSED=0
FAILED=0
ok()  { PASSED=$((PASSED + 1)); printf '  ok   %s\n' "$1"; }
bad() { FAILED=$((FAILED + 1)); printf '  FAIL %s\n' "$1"; }

FX="$TMP/fixture"
mkdir -p "$FX/scripts" || exit 2
cp "$CHECK" "$FX/scripts/" || exit 2

cat > "$FX/go.mod" <<'EOF'
module fixturesuite

go 1.21
EOF

cat > "$FX/a.go" <<'EOF'
package fixture

func Add(a, b int) int { return a + b }

func Split(n int) []int { return []int{n, n} }
EOF

# The fixture's tests. Each is load-bearing for one case, and the import block
# matters: a mutation that stops a test from using os or strings makes the
# fixture stop BUILDING, and the check then correctly exits 2 for the wrong
# reason -- which is a fixture bug wearing the costume of a result.
cat > "$FX/a_test.go" <<'EOF'
package fixture

import (
	"os"
	"strings"
	"testing"
)

func TestAdd(t *testing.T) {
	if Add(1, 2) != 3 {
		t.Errorf("Add(1,2) = %d, want 3", Add(1, 2))
	}
}

func TestRawStringBoundary(t *testing.T) {
	// Content containing a line that begins with '}'. gofmt cannot prevent
	// that, and a body scan looking for "\n}" stops here instead of at the
	// assertions below.
	doc := `example
}
still inside the string
`
	if err := os.WriteFile("raw.txt", []byte(doc), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !strings.Contains(doc, "still inside") {
		t.Errorf("content lost: %q", doc)
	}
}

func TestThroughHelper(t *testing.T) {
	assertLen(t, Split(7), 2)
}

// assertLen takes more parameters than just t. A pattern anchored on the
// closing paren of the signature does not see it, and then reports its caller
// as unable to fail.
func assertLen(t *testing.T, got []int, want int) {
	t.Helper()
	if len(got) != want {
		t.Fatalf("len(%v) = %d, want %d", got, len(got), want)
	}
}
EOF

# A second file so case 6 can truncate one file while the package still builds.
# Its first 9 lines are self-contained on purpose: head -n 9 is a valid Go file.
cat > "$FX/b_test.go" <<'EOF'
package fixture

import "testing"

func TestNumbers(t *testing.T) {
	if Add(1, 1) != 2 {
		t.Errorf("bad")
	}
}

func TestLetters(t *testing.T) {
	if Add(2, 2) != 4 {
		t.Errorf("bad")
	}
}

func TestBounds(t *testing.T) {
	if len(Split(1)) != 2 {
		t.Errorf("bad")
	}
}
EOF

echo "test-suite-selftest: building the fixture baseline"
if ! ( cd "$FX" && python3 scripts/test-suite-check.py --update >/dev/null 2>&1 ); then
  echo "test-suite-selftest: the fixture could not generate a baseline; nothing below would prove anything" >&2
  exit 2
fi

run_check() {
  local out got
  out=$( cd "$FX" && python3 scripts/test-suite-check.py 2>&1 ); got=$?
  printf '%s' "$out"
  return "$got"
}

# apply a mutation and require that it actually landed. A mutation that matches
# nothing leaves the check green and the case vacuous -- the round-10 lesson,
# which has now been relearned here once per round.
mutate() {
  python3 - "$@"
}

# ------------------------------------------------------------------------
echo "test-suite-selftest: case 1 -- a sound suite passes, and is seen to be sound"
out=$(run_check); got=$?
if [ "$got" -eq 0 ]; then
  ok "a fixture suite with nothing wrong passes"
else
  bad "the fixture FAILED (exit $got)"
  printf '%s\n' "$out" | tail -5 | sed 's/^/         | /'
fi
if printf '%s' "$out" | grep -q '^test-suite-check: 6 discovered by the toolchain, 6 in source files'; then
  ok "and it saw all 6 tests, so the pass is not an empty verdict"
else
  bad "it did not report 7 discovered tests; a pass here would prove nothing"
  printf '%s\n' "$out" | head -2 | sed 's/^/         | /'
fi

# ------------------------------------------------------------------------
echo "test-suite-selftest: case 2 -- a deleted test is caught and named"
if mutate "$FX/a_test.go" <<'PY'
import re, sys
p = sys.argv[1]
s = open(p).read()
out = re.sub(r"func TestAdd\(t \*testing\.T\) \{.*?\n\}\n\n", "", s, flags=re.S)
if out == s:
    sys.exit("TestAdd not removed")
open(p, 'w').write(out)
PY
then
  ok "the case-2 mutation applied"
else
  bad "the case-2 mutation did NOT apply; the case would prove nothing"
fi
out=$(run_check); got=$?
if [ "$got" -ne 0 ] && printf '%s' "$out" | grep -q 'the suite lost TestAdd'; then
  ok "losing TestAdd is caught, and the report names the test and its file"
else
  bad "deleting a test did not fail the check (exit $got)"
  printf '%s\n' "$out" | tail -5 | sed 's/^/         | /'
fi

# ------------------------------------------------------------------------
echo "test-suite-selftest: case 3 -- adding a test is free"
if mutate "$FX/a_test.go" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
if "func TestAdd" in s:
    sys.exit("TestAdd already present")
add = ('func TestAdd(t *testing.T) {\n'
       '\tif Add(1, 2) != 3 {\n\t\tt.Errorf("wrong")\n\t}\n}\n\n'
       'func TestBrandNew(t *testing.T) {\n'
       '\tif Add(1, 1) != 2 {\n\t\tt.Errorf("wrong")\n\t}\n}\n\n')
open(p, 'w').write(s.replace("func TestRawStringBoundary", add + "func TestRawStringBoundary", 1))
PY
then
  ok "the case-3 mutation applied"
else
  bad "the case-3 mutation did NOT apply; the case would prove nothing"
fi
out=$(run_check); got=$?
if [ "$got" -eq 0 ] && printf '%s' "$out" | grep -q 'TestBrandNew'; then
  ok "a new test does not fail the contract, and is reported as new"
else
  bad "adding tests made the check fail (exit $got); a contract that taxes additions gets edited until it means nothing"
  printf '%s\n' "$out" | tail -5 | sed 's/^/         | /'
fi

# ------------------------------------------------------------------------
echo "test-suite-selftest: case 4 -- a test stripped of its assertions is caught"
# The exact shape a lost assertion takes: the name survives, the check does not.
# os and strings stay in use, or the fixture stops building and the case would
# report a compile error as a verdict about the test.
if mutate "$FX/a_test.go" <<'PY'
import re, sys
p = sys.argv[1]
s = open(p).read()
out = re.sub(r"func TestRawStringBoundary\(t \*testing\.T\) \{.*?\n\}\n\n",
             "func TestRawStringBoundary(t *testing.T) {\n"
             "\tdoc := `example\n}\nstill inside the string\n`\n"
             "\t_ = os.Getpid()\n"
             "\t_ = strings.ToUpper(doc)\n"
             "\tt.Log(doc)\n"
             "}\n\n", s, flags=re.S)
if out == s:
    sys.exit("TestRawStringBoundary not rewritten")
open(p, 'w').write(out)
PY
then
  ok "the case-4 mutation applied"
else
  bad "the case-4 mutation did NOT apply; the case would prove nothing"
fi
out=$(run_check); got=$?
if [ "$got" -ne 0 ] && printf '%s' "$out" | grep -q 'TestRawStringBoundary.*no path on which it can report'; then
  ok "a test whose assertions were deleted, with its name left behind, is caught"
else
  bad "a test with no failure path passed the check (exit $got)"
  printf '%s\n' "$out" | tail -5 | sed 's/^/         | /'
fi

# ------------------------------------------------------------------------
echo "test-suite-selftest: case 5 -- the two false accusations this check made while being written"
# Put TestRawStringBoundary back into an asserting form that STILL uses os and
# strings and STILL contains a line starting with '}' inside a raw string. The
# check must go back to green: it must not flag that test (its assertions sit
# after the trap line) and it must not flag TestThroughHelper either, whose only
# failure path runs through a helper taking (t, got, want). Both were false
# accusations the first version made; together they are the whole case.
if mutate "$FX/a_test.go" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
start = s.index("func TestRawStringBoundary")
end = s.index("func TestThroughHelper")
body = ("func TestRawStringBoundary(t *testing.T) {\n"
        "\tdoc := `example\n}\nstill inside the string\n`\n"
        "\tif err := os.WriteFile(\"raw.txt\", []byte(doc), 0o644); err != nil {\n"
        "\t\tt.Fatalf(\"write: %v\", err)\n\t}\n"
        "\tif !strings.Contains(doc, \"still inside\") {\n"
        "\t\tt.Errorf(\"lost: %q\", doc)\n\t}\n"
        "}\n\n")
open(p, 'w').write(s[:start] + body + s[end:])
PY
then
  ok "the case-5 mutation applied"
else
  bad "the case-5 mutation did NOT apply; the case would prove nothing"
fi
out=$(run_check); got=$?
if [ "$got" -eq 0 ] && ! printf '%s' "$out" | grep -q 'no path on which'; then
  ok "neither the helper-asserting test nor the one after a '}' in a string is flagged"
else
  bad "a test that can fail was accused (exit $got)"
  printf '%s\n' "$out" | grep -E 'FAIL|discovered' | head -5 | sed 's/^/         | /'
fi

# ------------------------------------------------------------------------
echo "test-suite-selftest: case 6 -- a truncated file names what it lost"
# head -n 9 keeps the file valid Go and drops two tests, which is the shape the
# real incident had: the file still exists, still compiles, still contributes
# tests, and `go test ./...` is green.
head -n 9 "$FX/b_test.go" > "$TMP/b.truncated" && cp "$TMP/b.truncated" "$FX/b_test.go"
out=$(run_check); got=$?
lost=$(printf '%s' "$out" | grep -c 'the suite lost')
if [ "$got" -ne 0 ] && [ "$lost" -eq 2 ] \
   && printf '%s' "$out" | grep -q 'the suite lost TestLetters' \
   && printf '%s' "$out" | grep -q 'the suite lost TestBounds'; then
  ok "a truncated test file names exactly the two tests that went missing"
else
  bad "truncating a file did not produce exactly two named losses (got $lost, exit $got)"
  printf '%s\n' "$out" | tail -5 | sed 's/^/         | /'
fi

# ------------------------------------------------------------------------
echo "test-suite-selftest: case 7 -- no baseline is not a pass"
mv "$FX/scripts/test-suite-baseline.txt" "$TMP/baseline.saved"
out=$(run_check); got=$?
if [ "$got" -eq 2 ]; then
  ok "a missing baseline exits 2: the check could not run, so it withholds success"
else
  bad "a missing baseline produced exit $got, not 2"
fi
mv "$TMP/baseline.saved" "$FX/scripts/test-suite-baseline.txt"

# ------------------------------------------------------------------------
echo "test-suite-selftest: case 8 -- an unbuilt suite is not a shrunken suite"
printf 'package fixture\n\nthis is not go\n' > "$FX/broken_test.go"
out=$(run_check); got=$?
if [ "$got" -eq 2 ] && printf '%s' "$out" | grep -qi 'does not build'; then
  ok "a suite that does not build exits 2 and says why, instead of reporting lost tests"
else
  bad "a suite that does not build produced exit $got (expected 2, with the reason)"
  printf '%s\n' "$out" | tail -4 | sed 's/^/         | /'
fi

echo ""
echo "----------------------------------------"
printf 'test-suite-selftest: %d passed, %d failed\n' "$PASSED" "$FAILED"
[ "$PASSED" -eq "$EXPECTED" ] || bad "assertion count matches the contract (got $PASSED, expected $EXPECTED)"
[ "$FAILED" -eq 0 ] || exit 1
exit 0
