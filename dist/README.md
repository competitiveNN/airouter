# Unpushed commits

> The `.bundle` and `.patch` files here are **generated, not committed**. Run
> `scripts/export-unpushed.sh` to (re)create them. Committing them is
> self-referential — committing the bundle creates a commit the bundle does not
> contain, so refreshing it creates another one and it can never converge. An
> earlier attempt in this repo did exactly that and needed two "refresh the
> bundle" commits to undo; the script exists so the next person does not repeat
> it. `dist/README.md` and the script are the committed parts.

`origin` is `https://github.com/competitiveNN/airouter`. For most of this work's
life the token in use authenticated but had `push: false`, so these commits existed
only here and `dist/` was the only copy of them. That is no longer reliably true:
another account or process is advancing `origin/master`, and during the round that
wrote this sentence the unpushed set went from 37 commits to 0 with no edit to the
commits or to this file.

Two consequences, both worth internalising before using anything below:

  - The artifacts are still the point. They carry the commits the remote does not
    have yet, and the script below regenerates them in seconds.
  - **A list of "what is unpushed" written in a document is a snapshot of a thing
    that moves.** Do not read the list; run the check that compares it against the
    repository (`./scripts/audit-attribution-check.py`), or ask git directly
    (`git rev-list --reverse origin/master..HEAD`). The checker exists so that a
    stale list is a failure, not a surprise handed to the next person.

> **Do not embed a token in a remote URL to push.** `.git/config` is plaintext,
> and the token then appears in every `git remote -v` and in the process table of
> every push. This repository has already had to remediate one left there; the
> remediation was undone by following the push instructions below with a token in
> the URL instead of a username. Use a credential helper, or let git prompt.

## Before you trust either file

Both artifacts are **generated, untracked, and only as good as the last
refresh**. A stale one is not obviously stale: it still exists, still passes
`git bundle verify`, and carries a plausible set of commits — it just describes
an older `HEAD`, so a handoff built from it silently omits everything since.

Run both gates before relying on these files. Neither modifies them.

```sh
./scripts/dist-freshness-check.sh --verbose   # are they current w.r.t. HEAD?
./scripts/recovery-check.sh                   # do they actually recover?
```

`dist-freshness-check.sh` compares the bundle tip, the patch's last commit and
the patch's commit count against `HEAD`; it reports and never regenerates, so a
broken generator stays visible instead of being papered over. `recovery-check.sh`
goes further and proves it: it fetches the bundle into a throwaway repo, replays
the patch with `git am --3way` onto the real upstream base, asserts both
recovered trees are byte-identical to this one, and builds and tests the result.
Use `--quick` to skip the build.

If either reports stale, regenerate first:

```sh
./scripts/export-unpushed.sh
```

## Files

| File | What it is |
|---|---|
| `airouter-unpushed.bundle` | A git bundle of `origin/master..HEAD`. Preserves full history, authors and messages. |
| `airouter-unpushed.patch` | The same commits as a mailbox-format patch, for `git am`. |

Regenerate both with:

```sh
./scripts/export-unpushed.sh
```

> Applying the patch needs the true upstream base. A clone of this
> repository's *path* tracks the local `master`, which already contains these
> commits — replaying onto that applies on top of commits that are already
> present and reproduces nothing. Use a fresh clone of the GitHub URL, as
> above.

## Applying the bundle (preferred — keeps authors and messages)

> The ref name below is not arbitrary. `git bundle create` records whatever
> ref it was given, and a bundle made with the `A..HEAD` shorthand is stored
> under the ref `HEAD` — which `git fetch` then rejects outright with
> `fatal: couldn't find remote ref HEAD`. That is not hypothetical: these
> instructions once said `'HEAD:refs/heads/frombundle'` and did not work.
> `export-unpushed.sh` names the tip `airouter-unpushed-export` precisely so
> there is a real ref to fetch. If you exported under a custom `EXPORT_REF`,
> substitute that name — or ask the bundle, which is always correct:
> `git bundle list-heads /path/to/airouter-unpushed.bundle`.

```sh
git clone https://github.com/competitiveNN/airouter airouter
cd airouter
git fetch /path/to/airouter-unpushed.bundle \
  'refs/heads/airouter-unpushed-export:refs/heads/frombundle'
git checkout frombundle
```

Or into an existing clone:

```sh
git fetch /path/to/airouter-unpushed.bundle \
  'refs/heads/airouter-unpushed-export:refs/heads/frombundle'
git merge --ff-only frombundle
```

## Applying the patch

```sh
git am --3way /path/to/airouter-unpushed.patch
```

`--3way` is worth using: it falls back to a three-way merge if any context has
drifted, instead of failing outright.

## Pushing

Nothing needs rebasing — the history is linear on `origin/master`. That sentence
is an assertion about the shape of the history, and it is now *executed* rather
than asserted: `scripts/doc-verify.sh` fetches the bundle into a clone standing
on the base and runs the `--ff-only` merge below, which fails outright if the
history turns out to be divergent. A plain merge would have accepted that by
adding a merge commit, which is exactly what this sentence promises will not
happen.

Write access used to be the blocker, and "nobody has it" is no longer reliably
teither way, so check rather than assume:

```sh
gh api repos/competitiveNN/airouter --jq .permissions.push
```

Then:

```sh
git push origin master
```

If the configured account cannot push, grant it write access, or push over a
remote you control. Put a **username** in the URL, not a token — see the warning
at the top of this file:

```sh
git remote set-url --push origin https://<user>@github.com/competitiveNN/airouter
git push origin master
```

## Contents

What `origin/master` is missing, oldest first:

- `d0cae7e` A protocol refusal means "use the other shape", not "this endpoint is broken"
- `5c401fe` validate-config: only call a model dead if BOTH protocols refuse it
- `4669b1d` sync-models: regenerate config deterministically instead of driving an agent
- `92ae86a` config.yaml: regenerated by the sync, and record the new tests


That is the whole required list as of this writing — commits that touch only
`dist/` are exempt by design, so the commits maintaining this file are not
repeated in it. The list is checked rather than trusted:
`scripts/audit-attribution-check.py` compares it as a **set** against
`git rev-list origin/master..HEAD` -- not as a count, because a count is satisfied
by any equally-wrong list of the same length -- and fails when the two disagree in
either direction. `gate.sh` runs it, so a stale list here is a red gate.

Two properties make any such list awkward, and neither is a bug to be fixed:

  - A commit that updates this list cannot contain its own hash. The checker
    exempts commits whose entire diff is confined to `dist/`, so the commits that
    maintain this file are self-exempt. The exemption is by *confinement*, not by
    presence: a code change that also edits this file is still required to be
    listed, so nothing real can be laundered out of the handoff.
  - `origin/master` can advance underneath the list. When it does, the checker
    reports every entry as "not an unpushed commit (it may have been pushed or
    rewritten)". That is the check being right, and it means something far more
    mundane than a corrupted handoff: someone else pushed.

To rebuild the list from the repository rather than from this file:

```sh
git rev-list --reverse --pretty=format:'- `%h` %s' origin/master..HEAD
```

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

`push` permission has been `false` for most of this work's life, which is the
whole reason `dist/` exists. It is no longer safe to assume that in either
direction — someone with access is pushing, occasionally from another process.
Verify before relying on either story, and check these artifacts against the
checkout in the same breath, because a stale bundle looks exactly as
authoritative as a fresh one:

```sh
./scripts/dist-freshness-check.sh --verbose   # exits 1 if they are stale
```

It compares the bundle's tip, the patch's tail, and the patch's commit count
against `HEAD`, and tells you to run `./scripts/export-unpushed.sh` if they
disagree. It reports rather than regenerating on purpose: a check that silently
rewrites the artifacts reports success for work it never verified.

The one-line version, if you want it inline:

```sh
git bundle list-heads dist/airouter-unpushed.bundle   # must equal git rev-parse HEAD
```

A bundle whose tip is not `HEAD` is missing whatever was committed since, and
the mismatch is the only symptom.
