# MERA — Session Status

**Read this first in a new session.** Living status doc: where we are, what's next, and the environment facts you need to run anything. The other docs are reference material this points into.

Last substantive update: 2026-10-22 — **nameless-change-units bug fixed and verified on both repos** (see §3, first entry). Before that, 2026-08-23: Stage 8 complete and verified end to end against the real Team Server repo.

## The doc map

| Doc | What it's for |
|---|---|
| `MERA-redesign-architecture.md` | The design spine (rev 2). All six decisions, all five agent definitions, the finding schema, the domain model, phasing. |
| `MERA-implementation-manual.md` | The build guide. §1.3 (mx version matrix + Dockerfile), §1.4 (the frozen `/extract` REST contract), §1.5 (the 14-step extraction algorithm). **Parts 2–9 are the entire, still-unbuilt Mendix side** — that is where the work goes next. |
| `MERA-stage8.md` | The completed record of Stage 8: the proving run, confirmed `mx` tool facts, the named failure modes, and what each step landed. |
| `MERA-extractor-design-notes.md` | Why the extractor's Go code is shaped the way it is. Package split, concurrency, mxcli/git gotchas, the `/extract` pipeline. No source listings — the repo is the source of truth for code. |

---

## 1. Where things stand

**The extractor sidecar is feature-complete for its first real job.** A Go HTTP service with `GET /health`, `POST /describe`, `POST /clone`, `POST /extract`. `POST /extract` now does **real change detection**: it clones two commits as git worktrees, diffs them with `mx diff`, resolves each changed unit's GUID to a qualified name via `mx dump-mpr`, renders before/after MDL for only the changed units via `mxcli describe`, and adds per-file text diffs for `javasource`/`theme`/`deployment`/`*.json`.

Proven on 2026-08-23 against the real repo: **2 change units instead of 1,037**, both named, both with real MDL, in 35 seconds. Full output in `MERA-stage8.md` §1.

**The naive full-enumeration path still exists** behind the explicit `units`/`modules` escape hatch, running against the head worktree and returning the pre-Stage-8 response shape unchanged.

**The mx binary acquisition / trim / Docker track is CLOSED.** `scripts/fetch-mx.sh add-trimmed-version <version>` downloads, trims and validates a Mendix version in one call, driven per line of `mx-versions.txt` from a dedicated `mx-fetch` Docker build stage. 1.6G tarball → 164M finalized for 11.13.0. The script's own `TRIM_CANDIDATES` header comment holds the full list of what was cut, what is confirmed load-bearing and must not be re-attempted, and how each was validated. **Don't re-derive any of that — read the script.**

**Concurrency is done and confirmed.** A single global semaphore in `internal/mxcli` gates every subprocess call process-wide. Race-tested. Story and reasoning: `MERA-extractor-design-notes.md` §3.

**No work has started on the Mendix side of MERA.** Everything so far is the extractor sidecar.

---

## 2. Next

**The Mendix side is the whole remaining project.** Manual Parts 2 onward. Two things worth doing first:

1. **Manual §M2 — the Anthropic SDK smoke test.** A twenty-minute job that de-risks the entire Java side. Nothing depends on it, so it can happen in parallel with anything.
2. **Cheap Stage 8 follow-ups**, each one integration run (see `MERA-stage8.md` §6):
   - ~~A commit pair touching a **page** settles whether `mx diff` speaks `Forms$Page` or `Pages$Page`~~ — **settled 2026-10-22: `Forms$Page`.** Remaining: delete the now-dead `Pages$Page`/`Pages$Snippet`/`Pages$Layout`/`Pages$BuildingBlock` keys from `diffTypeToMxcli` (check `TestTypeTablesAreCoherent` still passes).
   - A commit pair touching **javasource** proves `textDiffs` end to end — it has never returned a non-empty result from the real repo.
   - `mxcli.Version` should return `v0.19.0`, not `"mxcli version v0.19.0 (2026-08-21T13:13:26Z)\n"`.

---

## 3. Open items

**2026-10-22 — `/extract` returned every change unit without name or MDL. Fixed (commit `dfae937`), verified.** Symptom: HTTP 200 with correct counts, but every unit had empty `qualifiedName`/`module` and no MDL. The prime suspect was the sparse checkout from `5aa371b`, and it turned out **innocent**. The real cause: for a page change, `mx diff` reports `Forms$Page` (storage name), and `resolveBothSides` passes the diff types to `dump-mpr --unit-type`. `dump-mpr` accepts only `Pages$Page`, so it rejected the *whole* call (exit 2), and every unit in the diff lost its name, not just the page. The cause was in the response warnings all along, but MERA didn't store them and the log only printed the count. Fix: `mx.dumpMprUnitTypes` rewrites `Forms$X` → `Pages$X`; a type `dump-mpr` still rejects is dropped and the call retried, so it costs only its own units. Every warning is now logged per line, and an `ERROR` line fires when no unit could be named. Verified: test-repo pair `13677c2 → c340a97` went from 6 warnings to 0; MERA app pair `f5b147e → ef25be9` went from 8 warnings and 17.7 KB to 0 warnings and 73 KB. New guarded test `TestExtract_SparseCheckoutNames_Integration` (needs `MERA_IT_NAMES_BASE_SHA`/`MERA_IT_NAMES_HEAD_SHA`, see §4). Details: design notes §6 correction.

**Open items left by that session — pick up here:**

1. **DomainModel MDL — needs a decision first.** `DomainModels$DomainModel` is in `knownNotDescribable`, with the reason "entity and association changes are reported as their own units". That is probably wrong: `mx diff` reports per storage unit, so an entity edit most likely arrives as one `DomainModels$DomainModel` Modified with no MDL at all. mxcli v0.27 has no MDL describe for a whole domain model (`domainmodel` renders an SVG/ELK diagram only). Option: describe each entity and association of the module (`entity`/`association` types exist) and concatenate them. That conflicts with the documented decision, so decide before building. **First step: capture a real diff of a commit that edits an entity**, to confirm how `mx diff` reports it.
2. **Flaky test:** `internal/workspace` `TestTrack_StopIsIdempotent` fails about 1 in 5 runs (`go test -count=20 -run TestTrack_StopIsIdempotent ./internal/workspace/` reproduces it): `TempDir RemoveAll cleanup: … clone-both-idem: directory not empty`. It looks like a heartbeat write racing the test's temp-dir cleanup after `stop()`. Unrelated to the fix above.
3. **Dead `Pages$` keys in `diffTypeToMxcli`** — see §2.
4. **Host mxcli is v0.19.0, container v0.27.0.** The integration tests run the host one, so they don't test what ships. Update `~/bin/mxcli`, or note the gap.
5. **`.env` has no `MERA_IT_NAMES_*` yet**, so the new integration test skips unless they're set by hand (values in §4).

**2026-10-18 — `/extract` timeout incident, fixed. Re-run verified 2026-10-21:** the marketplace pair `fb7a31b → 9062fac` completed in 44s, and `mx`/`mxcli` need nothing beyond the model files (see the 2026-10-22 entry above). A commit pair with several marketplace-module updates hit `cloneBothTimeout` (300s): the full base checkout pulled ~100 MB from Team Server at ~200 KB/s. Then the handler hung past its deadline, because the killed `git worktree add`'s descendants kept downloading and held the output pipe. Three fixes: (1) git runs in its own process group, killed whole on timeout, with `WaitDelay` as a backstop; compose gets `init: true`. (2) Worktrees are sparse, model only (`*.mpr`, `mprcontents/`), with a credentialed prefetch of exactly the blobs `TextDiffs` needs; `MERA_CLONE_TIMEOUT` makes the deadline configurable. (3) The healthcheck called `wget`, which the image doesn't have, so every container had always shown "unhealthy" — **correction, logged:** compose presented that check as working; it never was. Now it uses `curl`. Details in design notes. **Still to verify:** re-run that same commit pair from Mendix and confirm `mx`/`mxcli` need nothing beyond the model files.

**Not blocking, but real:**

- **`/clone`'s workDir is never reaped.** Sits on disk until the container restarts. Fine for local testing, must be solved before unattended operation — this is what manual §1.8's leased workspaces exist for. `/extract` is fine; its whole lifecycle fits in one request.
- **`/health` queues behind the mxcli semaphore.** Arguably correct backpressure, but needs a decision if a deployment platform's liveness probe has a tight timeout.
- **`internal/mxcli` has only a concurrency test.** Its `run()` banner-stripping and error behaviour deserve a correctness test.
- **The error envelope and request auth shape don't match manual §1.4** — `{"error": "..."}` rather than `{requestId, error, detail, retryable}`, and flat `username`/`pat` rather than `auth: {kind, username, secret}`. Deliberately deferred; they are contract work in their own right.
- **`storageFormat` is emitted empty** rather than fabricated. `mx diff`'s output has top-level `base`/`mine` fields that `DiffResult` discards — worth checking whether they carry it.
- **The finalized `.mx-binaries/11.13.0/` copy has not been re-diff-tested with all four trim rounds applied together.** Every real-diff validation happened incrementally against the scratch path. Before treating it as production-ready: `add-trimmed-version 11.13.0 --replace`, then re-run the diff tests against *that* exact copy.
- **The five `mx` subprocess calls in `/extract` run sequentially** and dominate the 35s. Both sides of `PrepareMpr` and `ResolveQualifiedNames` are independent and could run concurrently.
- **CI egress:** per manual §1.10, confirm whatever runner builds the image has real network access to `cdn.mendix.com` — the `mx-fetch` stage depends on it at build time.

**Open investigation — does the version matrix actually need to be per-version?**

Working hypothesis: `mx` may not meaningfully change within a major version, which would shrink or eliminate the matrix. The confirmed `-l` / `--loose-version-check` flag on `mx diff` is the direct lever. `mx analyze-mpr` is already confirmed version-agnostic (an 11.13.0 binary read an 11.10.0-authored file correctly).

1. ~~Download 11.13.0~~ — done.
2. ~~Download a second 11.x version~~ — done, 11.12.0 is installed alongside it.
3. Run `mx diff` against the same `.mpr` with both binaries, with and without `-l` — note whether the "wrong" binary refuses, silently misreads, or auto-converts.
4. Time permitting, repeat with a 10.x binary against a 10.x-era `.mpr`. The test app is 11.x, so this half likely needs a second, older test repo — flag that as a possible blocker rather than assuming one is on hand.
5. Record the result here and in manual §1.3.

Empirical by nature; nothing here can be reasoned out from documentation. Doesn't block anything if 11.13.0 covers the versions currently in scope.

---

## 4. Test data

- **Repo:** `https://git.api.mendix.com/b12ab91d-b0f7-42fa-b404-a2e86aa7f674.git`
- **App shape:** **1,037 units** (was 608 earlier in the project — it has grown). 16 modules, several from the Marketplace. `ReviewManagement` ~90 units, `Administration` ~19 (the escape-hatch integration test uses it because it is small).
- **Validated commit pairs:** one adding a single microflow; one combining an image add with the microflow add. The second is what proved Stage 8: it yields `Images$ImageCollection` (Modified) and `Microflows$Microflow` (Added), the latter resolving to `MxCliExtractor.ACT_Test_newMicroflow`.
- **Page-touching pair (2026-10-22):** `13677c27691d5c3536c4acd8cea20487ba7bfce9` → `c340a979835ba403fad4ac5cfd56b09b921f2937` (both Mendix 11.13.0): `Forms$Page` `ApplicationAdministration.Home_Web` Modified, 2 microflows (Modified + Added), 1 image collection. Set as `MERA_IT_NAMES_BASE_SHA`/`MERA_IT_NAMES_HEAD_SHA` for `TestExtract_SparseCheckoutNames_Integration`. Earlier commits (`02bb305` and before) are Mendix 11.11, so a diff across that boundary fails (exit 4).
- **No validated pair touches `javasource` or edits an entity** — the `textDiffs` follow-up in §2 and open item 1 in §3.
- **MERA's own app** (`cf4fd06d-…`, Mendix 11.12.x) is a second real repo; the `.env` PAT cannot read it.
- **Known-good commit:** `02bb30548e869706ee856a340e26354e5760e0d0`.
- **Avoid:** any commit capturing a Studio Pro version upgrade *in progress*. Note this is NOT the same as an app that merely contains a `Projects$ProjectConversion` unit — this app does, and diffs fine. See `MERA-stage8.md` §3.
- **Local clone for manual poking:** `~/mera/mera-repo`, with `git worktree add` checkouts at `/tmp/mera-base` and `/tmp/mera-head`.

---

## 5. Environment notes

- **PAT:** never written down here. `export MERA_PAT="..."` or `source ~/.mera-secrets.sh` each session. Clone via the interactive `pat` / `$MERA_PAT` username/password prompt — **never** embedded in the clone URL.
- **Go** via the official tarball at `/usr/local/go`. `mxcli` at `~/bin/mxcli`, currently **v0.19.0**. Work happens in `~/mera/` (WSL2 native filesystem).
- **`nproc` is 8** — the empirical ceiling for mxcli concurrency (25 workers performed no better).
- **`scripts/fetch-mx.sh`** scratch lives in `~/.mx-fetch-scratch` (override with `$MX_FETCH_SCRATCH`). Subcommands: `inspect`, `probe`, `trial-trim`, `deep-inspect`, `finalize [--replace]`, `add-trimmed-version <version> [--replace]`, `restore`, `clean`, `list`. Its health check uses `--help` (this CLI's parser rejects `--version`) and judges success by scanning for crash signatures rather than trusting the exit code, which is non-zero even for a valid `--help`.
- **`mx-versions.txt`** at the repo root is the version-matrix source of truth. **`.mx-binaries/` and `*.tar.gz` are gitignored.**
- **ICU:** Jord's Ubuntu offers only `libicu78`; the container base (`debian:bookworm-slim`) uses **`libicu72`** — don't assume 78 transfers. `DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1` is a structural dead end for this app: it hardcodes loading the real `en-us` culture at startup. Real ICU data is the only fix.
- **Docker:** builds and runs locally, not yet pushed or wired to CI. Use `docker buildx build` (or `DOCKER_BUILDKIT=1`) — it also builds the `build` and `mx-fetch` stages in parallel. Pass `-e MERA_MXCLI_CONCURRENCY=<n>` when testing concurrency; it isn't baked into the image.

### Running the tests

```bash
go test ./...                                  # hermetic, offline, ~10s

set -a; source .env; set +a                    # KEY=value, NO spaces around the =
export MERA_IT=1                               # assert this run must hit the network
go test ./... -count=1 -v -run Integration     # ~50s, real repo, real binaries
```

- **Nothing in Go reads `.env`** — not the standard library, not `go test`. Whatever loads yours in `main.go` does not apply here. `set -a` requires real shell syntax; `KEY = value` with spaces is not an assignment and silently gives you nothing. Verify with `env | grep '^MERA_'`.
- **`MERA_IT=1` belongs in the shell, not `.env`.** Without it a missing variable silently *skips*, and a skipped test prints `ok` in milliseconds — indistinguishable from one that passed. With it, a missing variable fails and names itself.
- **`MERA_MX_ROOT` should be an absolute path.** `go test` runs each package's binary with that package's own source directory as cwd, so a relative value resolves differently per package. The integration helper resolves relative values against the module root and validates `<root>/<version>/modeler/mx` before doing anything expensive, but absolute avoids the question.
- Optional: `MERA_IT_EXPECT_UNIT=MxCliExtractor.ACT_Test_newMicroflow` pins a known unit for the microflow-add pair.
- The integration run writes the full `/extract` response to `$TMPDIR/mera-extract-integration.json` — that is how to eyeball real MDL.

**WSL2 traps:**

- **Never work under `/mnt/c/`** — roughly 10× slower and confusing permission behaviour in containers. Stay in `~/`. Use VS Code's WSL extension for Windows-side editing.
- **CRLF line endings** break shell scripts inside containers. `git config --global core.autocrlf input` inside WSL.
- **`localhost` inside a container is the container.** To reach a service on the host, use `host.docker.internal`.
- **`docker build` caches aggressively.** If a change appears to have no effect, `--no-cache`. Always confirm which build a running container's tag points at before treating a missing file as a real bug.
- **Disk grows fast.** `docker system prune -a` when WSL's virtual disk balloons.

---

## 6. A standing note for future sessions

These docs accumulate session narration fast. When a track of work closes, fold its conclusions into the reference docs and delete the blow-by-blow rather than appending another "this session, continued further still" section. §1–§3 above should stay a snapshot, not a log.

Stage 8 produced three lessons worth carrying beyond it, all recorded in `MERA-extractor-design-notes.md`:

- **Overlapping observations are not confirming observations.** The claim that `mx diff` and `mx dump-mpr` share one type vocabulary was "proved" using the two types where both vocabularies happen to agree.
- **Detect on failure, don't predict it.** A pre-check derived from a proxy signal rejected every commit of a healthy app; the authoritative signal was the tool actually failing.
- **A test that skips looks exactly like a test that passes.** And a test built on an imagined output format will happily validate a parser that doesn't care about format either.