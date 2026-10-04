Build a ai gateway which main job is expose four models (smart, work, fast, large)
- smart: the most intelligent
- work: the "workhorse" that does the coding
- fast: for small, quick or verbose tasks that are simple enough for a small model
- large: for tasks that require very large context windows and for context compression

The gateway plugs into as many open ai compatible endpoints and exposes an open ai compatible endpoint.
Each of the 4 models can be configured to automatically route to a predefined list of models (fallback chain).
The sessions are ofc persistent, and same session are always routed to the same model.
If api rate limits are met mid session, the context is moved over to the next model in the fallback chain list.
Models that incur in errors receive a cooldown period. Cooldown time is based on error time (e.g. 429 have low cooldown, 404 have high cooldown, and so on)
We must account for all kind of possible errors, we want to catch any error that can happen even mid streaming the response, such that even during stream we can catch the error, move the session to the next model and continue streaming, such that the client never experiences any error. (from the point of the client the stream would experience just a slightly delay while the router waits for the next model in the chain to resume streaming).
Authentication is performed using env var api keys. Custom providers with custom urls can be defined and the configuration is stored and read from storage.

Verification (read before trusting a test result on this machine)
- The shell layer rewrites `python3 -m pytest` and `go test` output into a one-line summary, and it reports a suite that FAILED TO COLLECT as "No tests collected" with exit status 0. Never read a pass from that form: a test file that never runs looks exactly like a file with no tests.
- Any new test entry point goes through `scripts/run-config-tests.py`. It calls pytest's in-process API (where nothing rewrites the output) and asserts a collection floor, so "collected 0" is a failure. The real binary, when a failure mode is opaque, is `/home/fra/.local/bin/pytest`.
- Run everything with `scripts/gate.sh`. It is serialized with flock, fingerprints the tree, and refuses to call a run valid if the tree changed underneath it.
- `scripts/mutation-check.py` plants a defect in the sync chain, proves the suites go red, restores the tree, and exits non-zero if any planted defect went unnoticed. Run it after changing the config pipeline, the fetcher, or the rules checker — a test that cannot fail is not evidence.
- A write can report success and not land. After editing a test file, grep for the new test by name before believing a result that depends on it; `SUITE_TEST_FLOORS` in `scripts/mutation-check.py` fails the build if a suite loses even one test.
- Never `git checkout -- <path>` to undo your own edit. It discards the WHOLE working-tree file, so on a path that already had uncommitted work it silently destroys that work, and the result is indistinguishable from a clean tree afterwards — no gate check can catch it, because the tree state is identical. Check `git status --porcelain -- <path>` BEFORE editing, and restore from a copy you made (`cp` before, `cp` back after). This cost eight lines of uncommitted work on 2026-09-30.
- Model list changes: `python3 fetch-free-models.py --cooldowns-report` first (reports which free-tier models the gateway is being refused on, from `cooldowns.json`), then `python3 fetch-free-models.py --json --probe-auto --save /tmp/free-models.json`, then `python3 regenerate_config.py --write`, then `python3 scripts/check-rules.py`. `--probe-auto` records whether each chain terminator actually answers; the generator refuses to write a terminator the provider has refused. The same fetch also vetoes every endpoint `cooldowns.json` records as repeatedly refused for free-tier reasons (402 billing / 403 free-tier wording / 404 gone), which is the only record of that class of failure — no provider API reports it. Never hard-code a model's free status into a curated list to work around it; the veto expires on its own.
