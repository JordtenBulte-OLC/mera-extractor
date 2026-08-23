# MERA — Session Status

**Read this first in a new session.** Living status doc: where we are, what's next, and the environment facts you need to run anything. The other docs are reference material this points into.

Last substantive update: 2026-08-23 — **Stage 8 complete and verified end to end against the real Team Server repo.**

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
2. **Three cheap Stage 8 follow-ups**, each one integration run (see `MERA-stage8.md` §6):
   - A commit pair touching a **page** settles whether `mx diff` speaks `Forms$Page` or `Pages$Page`; then delete the dead half of `diffTypeToMxcli`.
   - A commit pair touching **javasource** proves `textDiffs` end to end — it has never returned a non-empty result from the real repo.
   - `mxcli.Version` should return `v0.19.0`, not `"mxcli version v0.19.0 (2026-08-21T13:13:26Z)\n"`.

---

## 3. Open items

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
- **Neither validated pair touches a page or `javasource`** — which is exactly why the two follow-ups in §2 are still open.
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