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
`dist/` carry the 23 commits that `origin/master` is missing, so nothing is lost
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

The 23 unpushed commits, oldest first:

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

The middle two are the self-reference loop described at the top of this file:
`fa9aeeb` committed a bundle, `8715b82` committed a refreshed one that could not
contain itself, and `c746484` replaced the approach. They are kept because
rewriting published history is worse than three honest commits, but nothing
needs doing with them.

The commit count has the same self-reference problem in miniature: stating 23
creates the commit that makes it 24. Read the number off
`git rev-list --count origin/master..HEAD` rather than trusting this line. The
bundle and patch in this directory are always regenerated, so they are never
stale.

`cb4e826` audits `b7cccda`: it found the fuzz gate's documented `FROZEN=1`
invocation was the one that failed (fabricating a counterexample from a usage
error), that a committed corpus made the gate permanently red, that discovery
was root-only, and that the threshold study was measuring a code path that never
executes. No production code changed.

## Verification

Both artifacts were checked, not just written: the bundle was fetched into a
fresh clone and the patch was applied with `git am --3way` onto `origin/master`.
Both produce a tree that builds and passes the full test suite.
