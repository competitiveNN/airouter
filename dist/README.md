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
`dist/` carry the 13 commits that `origin/master` is missing, so nothing is lost
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

The 13 unpushed commits, in order:

- `9a6485d` test: rename stale SelectNext tests, add secret scan and SSE negative check
- `5f9a6fa` test: add audit anchor drift check and harden the SSE negative check
- `d04f86d` fix: bound metrics label cardinality, escape label values, drop key leakage
- `a0daa56` fix: recycle metrics labels instead of starving, fix malformed overflow sample
- `7a34f23` fix: remove the two-key-convention bug class, bound the status maps

- `0100387` fix: start airouter with provider API keys in its environment
- `a515364` feat: resilient cooldown state, DELETE /admin/cooldowns, bounded fallbacks
- `0cc4e34` fix: stop config regeneration destroying the config header
- `b097b1f` fix: never truncate or double-terminate an SSE stream
- `dd167d7` style: gofmt the four drifted files
- `d834215` test: cover the mid-stream fallback [DONE] path and an SSE integration
- `8f65531` fix: bound the Responses API fallback walk by wall clock
- `1943655` feat: mount /metrics, add attempt telemetry, and assert the SSE contract

## Verification

Both artifacts were checked, not just written: the bundle was fetched into a
fresh clone and the patch was applied with `git am --3way` onto `origin/master`.
Both produce a tree that builds and passes the full test suite.
