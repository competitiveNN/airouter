# Unpushed commits

> The `.bundle` and `.patch` files here are **generated, not committed**. Run
> `scripts/export-unpushed.sh` to (re)create them. Committing them is
> self-referential — committing the bundle creates a commit the bundle does not
> contain, so refreshing it creates another one and it can never converge. An
> earlier attempt in this repo did exactly that and needed two "refresh the
> bundle" commits to undo; the script exists so the next person does not repeat
> it. `dist/README.md` and the script are the committed parts.

`origin` is `https://github.com/competitiveNN/airouter`. The token in use
authenticates but has `push: false` on the repository, and neither
`defnlnotme` nor `papersplx` has write access — checked with
`gh api repos/competitiveNN/airouter --jq .permissions.push`.

So the work is committed locally and **not** on the remote. These two files in
`dist/` carry the commits that `origin/master` is missing, so nothing is lost
if this checkout goes away.

## Files

| File | What it is |
|---|---|
| `airouter-unpushed.bundle` | A git bundle of `origin/master..HEAD`. Preserves full history, authors and messages. |
| `airouter-unpushed.patch` | The same commits as a mailbox-format patch, for `git am`. |

Regenerate both with:

```sh
./scripts/export-unpushed.sh
```

## Applying the bundle (preferred — keeps authors and messages)

```sh
git clone https://github.com/competitiveNN/airouter airouter
cd airouter
git fetch /path/to/airouter-unpushed.bundle 'HEAD:refs/heads/frombundle'
git checkout frombundle
```

Or into an existing clone:

```sh
git fetch /path/to/airouter-unpushed.bundle 'HEAD:refs/heads/frombundle'
git merge --ff-only frombundle
```

## Applying the patch

```sh
git am --3way /path/to/airouter-unpushed.patch
```

`--3way` is worth using: it falls back to a three-way merge if any context has
drifted, instead of failing outright.

## Pushing once write access exists

Nothing needs rebasing — the history is linear on `origin/master`. Get write
access granted for whichever account should push, then:

```sh
git push origin master
```

If you prefer to push from here instead, grant write access to the account whose
token is configured, or add a token with the `repo` scope to a remote that
already has write access:

```sh
git remote set-url --push origin https://<user>@github.com/competitiveNN/airouter
git push origin master
```

## Contents

The unpushed commits, oldest first:

- `0100387` fix: start airouter with provider API keys in its environment
- `a515364` feat: resilient cooldown state, DELETE /admin/cooldowns, bounded fallback
- `0cc4e34` fix: stop config regeneration destroying the config header and re-adding dead models
- `b097b1f` fix: never truncate or double-terminate an SSE stream
- `dd167d7` style: gofmt the four drifted files
- `d834215` test: cover the mid-stream fallback [DONE] path and add an SSE integrity harness
- `8f65531` fix: bound the Responses API fallback walk by wall clock
- `1943655` feat: mount /metrics, add attempt telemetry, and assert the SSE contract in CI
- `9a6485d` test: rename stale SelectNext tests, add secret scan and SSE negative check
- `5f9a6fa` test: add audit anchor drift check and harden the SSE negative check
- `d04f86d` fix: bound metrics label cardinality, escape label values, drop key leakage
- `a0daa56` fix: recycle metrics labels instead of starving, fix malformed overflow sample
- `7a34f23` fix: remove the two-key-convention bug class, bound the status maps
- `fa9aeeb` docs: add README config reference; ship bundle+patch for the unpushed commits
- `8715b82` chore: refresh unpushed bundle to include the README commit
- `c746484` chore: make the unpushed bundle a generated artifact, not a committed one
- `a861dd5` docs: list unpushed commits in dist/README, and note the two loop commits
- `12f6319` Stop exporting the overflow bucket as a histogram; fix unfetchable bundle
- `4c03ed2` Assert the histogram invariants across the whole exposition, not one family
- `647c3d3` Harden the histogram guard: pin its threshold, kill no-op mutations, fuzz it
- `b7cccda` Make the histogram guard's coverage durable: golden fixtures + CI fuzzing
- `cb4e826` Audit the gate: FROZEN=1 was the broken invocation, not the working one
- `c40f0eb` docs: refresh dist/README for the unpushed commits
- `ba4a6c4` docs: note that the unpushed count in dist/README is self-referential
- `119e998` Fuzz the target's own package: the recursive gate could never have worked
- `c941f23` docs: refresh dist/README for the fuzz-gate package fix
- `5437846` Test the gate in the layout that hid its bugs
- `979dcf6` docs: refresh dist/README for the fuzz-gate self-test
- `56dfbdf` Walk every .go file, not just the ones in the root
- `aaad4dd` Correct the round-10 record: the drift fix shipped in 56dfbdf, not here
- `ee4bf69` docs: refresh dist/README for the audit-drift-check glob fix
- `b950b3a` Close the two gaps round 10 named, and stop the guard going blind in CI
- `6ca056f` Make the attribution checker's output ASCII, and record where it must fail

The middle two are the self-reference loop described at the top of this file:
`fa9aeeb` committed a bundle, `8715b82` committed a refreshed one that could not
contain itself, and `c746484` replaced the approach. They are kept because
rewriting published history is worse than three honest commits, but nothing
needs doing with them.

The commit count has the same self-reference problem in miniature: writing a
number here creates the commit that makes it one larger. Read the number off
`git rev-list --count origin/master..HEAD` rather than trusting any number
written in this file. The bundle and patch in this directory are always
regenerated, so they are never stale.

`cb4e826` audits `b7cccda`: it found the fuzz gate's documented `FROZEN=1`
invocation was the one that failed (fabricating a counterexample from a usage
error), that a committed corpus made the gate permanently red, that discovery
was root-only, and that the threshold study was measuring a code path that never
executes. No production code changed.

`119e998` audits `cb4e826` the same way, and the result is that the recursive
discovery it added could never have worked: the fuzz phase still passed
`./...`, which Go refuses outright once a module has two packages, and the
refusal was reported as a counterexample pointing at a file that did not exist.
Three further defects sat behind it (a `dirname` result read as a stdlib import,
a crasher check scoped to the root instead of the package that owns it, and a
count comparison where a set diff was needed). It is the third commit in a row
whose findings are all in the gate rather than in the code, and each one was
only found by running the gate in the configuration it was written for.

`5437846` stops repeating the excuse. It adds `scripts/fuzz-gate-selftest.sh`,
which builds throwaway two-package repositories and asserts the gate's behaviour
in them, so the one condition that hid four consecutive rounds of gate defects is
reproduced on every CI run. It is verified to fail when each of those defects is
reintroduced, so a pass is evidence rather than an absence of output.

`56dfbdf` fixes the same root-only glob in `scripts/audit-drift-check.py`, which
reported a symbol that had merely moved to a subdirectory as "no longer exists" —
with a remediation (add it to the ALLOWLIST) that is both wrong and permanent. Note
the separate commit: that fix was written during `5437846`, then destroyed by a
`git reset --hard` run while probing a different script, and the round's summary
and the audit document both went on describing it as shipped. It was caught in
review, re-applied, and the record corrected in `aaad4dd`.

`audit-drift-check.py` was the one script here with no self-test guarding it, and
it is the one that had the bug. `fuzz-gate-selftest.sh` named that gap and spelled
out the extension; `audit-drift-selftest.sh` is the extension, and
`audit-attribution-check.py` closes the second gap it left — the one where a
finding is accurate about the code and wrong about the commit, because the anchor
check validates symbols and never checked that a fix actually shipped.

That second check needs full history. The `go` CI job now checks out with
`fetch-depth: 0`, because a shallow single-ref checkout has no `origin/master`
and the unpushed-list half would skip while still exiting 0 — a guard passing
because it could not see the thing it guards.

The list above omits the commits that deliver it, on purpose: a commit cannot
contain its own hash, so requiring it to would be unsatisfiable. `audit-attribution-check.py`
exempts exactly the commits whose diff touches this file, derived from their diffs
rather than from this prose, so nothing real can hide behind the exemption.

## Verification

Both artifacts were checked, not just written. The bundle was fetched into a
fresh clone and its tip compared against `HEAD`; the patch was applied with
`git am --3way` onto the commit `origin/master` points at and the resulting tree
compared against `HEAD`'s. (`git am` re-creates each commit, so the resulting
commits have different hashes — the trees are what must match, and they do.)
The recovered tree builds, and the full test suite passes there:

    go build ./... && go test -count=1 ./... && bash scripts/fuzz-gate-selftest.sh && bash scripts/audit-drift-selftest.sh && bash scripts/audit-attribution-selftest.sh

One check is excluded from the `git am` half on purpose, and the reason is
worth stating so it is not mistaken for drift later. `git am` re-hashes every
commit it replays, so by hash alone every citation the documentation makes is
absent in a patch-recovered tree. Rather than leave it broken there, the checker
grew a mode that matches citations by the change they carry rather than the
hash they happen to have:

```sh
python3 scripts/audit-attribution-check.py --recovered --against /path/to/original
```

It uses `git patch-id --stable`, which fingerprints a commit's diff rather than
its metadata, so it survives re-hashing. Verified on this repository: all 77
content fingerprints are identical between `HEAD` and its `git am` replay, and a
fabricated hash is still rejected in that mode. Use the bundle when the history
itself matters.

## Before trusting the recovery path

`push` is still `false`, so these commits exist only here and in `dist/`. If you
are reading this to pick the work up, check the artifacts against the checkout
before relying on them — they are regenerated per commit, and a stale bundle is
worse than none because it looks authoritative:

```sh
git bundle list-heads dist/airouter-unpushed.bundle   # must equal git rev-parse HEAD
./scripts/export-unpushed.sh                          # if it does not
```

The first command is the check. A bundle whose tip is not `HEAD` is missing
whatever was committed since, and the mismatch is the only symptom.
