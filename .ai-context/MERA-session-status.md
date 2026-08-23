# MERA — Session Status
 
**Read this first in a new session.** Living status doc, updated at the end of each work session: where we are, what's next, and the environment facts you need to run anything. The other docs are reference material this points into.
 
Last substantive update: 2026-08-23 15:00 UTC+8.
 
## The doc map
 
| Doc | What it's for |
|---|---|
| `MERA-redesign-architecture.md` | The design spine (rev 2). All six decisions, all five agent definitions, the finding schema, the domain model, phasing. **§2 is required reading** before touching Stage 8 — it defines `mx diff` vs `mxcli` and the `ChangeUnit` contract. |
| `MERA-implementation-manual.md` | The build guide. **§1.3** (mx version matrix + Dockerfile), **§1.4** (the frozen `/extract` REST contract), **§1.5** (the 14-step extraction algorithm) are what Stage 8 is built from. Parts 2–9 are the entire, still-unbuilt Mendix side. |
| `MERA-stage8.md` | The active build document — real change detection via `mx diff`. Design, confirmed tool facts, and per-step status. |
| `MERA-extractor-design-notes.md` | Why the extractor's Go code is shaped the way it is. Package split, the concurrency story, mxcli gotchas, open items. No source listings — the repo is the source of truth for code. |
 
---
 
## 1. Where things stand
 
**The extractor sidecar works end to end against a real Mendix Team Server repo.** A Go HTTP service with `GET /health`, `POST /describe`, `POST /clone`, `POST /extract` — clones a Mendix app repo with a user's PAT and renders units to MDL via `mxcli`.
 
**`/extract` still does naive full enumeration** — every unit in the app, or every unit in named modules. That was the deliberate stand-in ("a deliberately dumb version that unblocks parallel work beats a correct version that everything waits on"), and it has done its job. Stage 8 replaces it.
 
**Concurrency is done and confirmed.** A single global semaphore in `internal/mxcli` gates every subprocess call process-wide. Race-tested. Full story and the reasoning behind why the earlier per-request pool was wrong: `MERA-extractor-design-notes.md` §3.
 
**The whole mx binary acquisition / trim / Docker track is CLOSED.** `scripts/fetch-mx.sh add-trimmed-version <version>` downloads, trims and validates a Mendix version in one call, driven per line of `mx-versions.txt` from a dedicated `mx-fetch` Docker build stage. Confirmed with a real `docker build --no-cache` producing real `mx --help` / `mx diff --help` output *inside the running container*: 1.6G tarball → **164M** finalized for 11.13.0. `mx diff` produces correct `unitDifferences[]` against real commit pairs, validated across both a microflow-only change and an image-only change. The script's own `TRIM_CANDIDATES` header comment holds the full list of what was cut, what is confirmed load-bearing and must not be re-attempted (`Mendix.Modeler.WebUI.dll`, the non-Legacy `Mendix.Modeler.Theming.dll`, `CycloneDX.Core.dll`), and how each was validated. **Don't re-derive any of that from scratch — read the script.**
 
**Stage 8 is in progress.** `internal/mx` Steps 1–5 are built and unit-tested (green under `-race`, `resolve.go` at 100% statement coverage). Steps 6–12 remain. See `MERA-stage8.md`.
 
**No work has started on the Mendix side of MERA.** Everything so far is the extractor sidecar. Manual Parts 2 onward is the reference for when that begins — and manual §M2 (the Anthropic SDK smoke test) is a twenty-minute job that de-risks the entire Java side, worth doing in parallel with anything.
 
---
 
## 2. Next: finish Stage 8
 
Full detail in `MERA-stage8.md`. In order:
 
1. Confirm `go build ./...` is clean on the current working branch.
2. **Step 6** — `gitops.CloneBoth`: base+head worktrees from one clone.
3. **Step 7** — rewire `handleExtract` to the real `baseSha`/`headSha` contract, replacing naive enumeration with `mx diff` → `ChangeUnit[]`.
4. **Step 8** — targeted before/after describe.
5. **Steps 9–12** — token estimate, `textDiffs`, integration tests, docs.
---
 
## 3. Open items
 
**Not blocking, but real:**
 
- **`/clone`'s workDir is never reaped.** Sits on disk until the container restarts. Fine for local testing, must be solved before unattended operation — this is what manual §1.8's leased workspaces exist for.
- **`/health` queues behind the mxcli semaphore** as of Stage 7. Arguably correct backpressure, but needs a decision if a deployment platform's liveness probe has a tight timeout.
- **The finalized `.mx-binaries/11.13.0/` copy has not been re-diff-tested with all four trim rounds applied together.** Every real-diff validation happened incrementally against the scratch path as each round landed. Before treating it as production-ready: `add-trimmed-version 11.13.0 --replace`, then re-point the base/head worktrees at the finalized binary and re-run the image-diff and combined-diff tests against *that* exact copy.
- **No real `mx diff` (as opposed to `--help`) has been run inside the container.** The container's binary came from the same pipeline already validated on the host, so this is very likely fine — but it's the one remaining "should work" vs. "confirmed" gap on the mx side.
- **`~/mera/extractor/.mx-binaries/tmp`** (an early manual staging folder, fully superseded) can be deleted whenever convenient.
- **CI egress:** per manual §1.10, confirm whatever runner builds the image has real network access to `cdn.mendix.com` — the `mx-fetch` stage depends on it at build time.
**Open investigation — does the version matrix actually need to be per-version?**
 
The manual's design assumes one `mx` binary per exact Mendix version. Working hypothesis to test: `mx` may not meaningfully change within a major version, possibly across majors too — which would shrink or eliminate the matrix. The confirmed `-l` / `--loose-version-check` flag on `mx diff` is the direct lever for this. Also relevant: `mx analyze-mpr` is confirmed version-agnostic (an 11.13.0 binary read an 11.10.0-authored file correctly).
 
1. ~~Download 11.13.0~~ — done, and confirmed as the test app's baseline.
2. Download one other 11.x version, chosen meaningfully far from 11.13.0 to stress the hypothesis rather than confirm it cheaply. `add-trimmed-version` makes this one line; a 11.12.0 dry run already succeeded.
3. Run `mx diff` against the same `.mpr` with both binaries, with and without `-l` — note whether the "wrong" binary refuses, silently misreads, or auto-converts.
4. Time permitting, repeat with a 10.x binary against a 10.x-era `.mpr`. The current test app is 11.x, so this half likely needs a second, older test repo — flag that as a possible blocker rather than assuming one is on hand.
5. Record the result here and in manual §1.3 — either confirming the full matrix or replacing it with a coarser scheme.
Empirical by nature; nothing here can be reasoned out from documentation. Doesn't block the Go work if 11.13.0 covers the versions currently in scope.
 
---
 
## 4. Test data
 
- **Repo:** `https://git.api.mendix.com/b12ab91d-b0f7-42fa-b404-a2e86aa7f674.git`
- **Clone for diff work:** `~/mera/mera-repo`, with `git worktree add` checkouts at `/tmp/mera-base` and `/tmp/mera-head`. A bare `.mpr` in a plain directory is **not** sufficient for `mx diff` — `git worktree add` needs a real repository.
- **Validated commit pairs:** one adding a single microflow; one combining an image add (`Images$ImageCollection`) with the microflow add. Both produce correct `unitDifferences[]`. Use these for integration tests.
- **Known-good commit:** `02bb30548e869706ee856a340e26354e5760e0d0`.
- **App shape:** ~608 units across the six default types. `ReviewManagement` (~87 units), `Administration` (~19).
- **Avoid:** any commit capturing a Studio Pro version upgrade — it carries a `Projects$ProjectConversion` unit and no `mx` build parses it. This cost a full investigation once already; see `MERA-stage8.md` §3.
---
 
## 5. Environment notes
 
- **PAT:** never written down here. `export MERA_PAT="..."` or `source ~/.mera-secrets.sh` each session. Clone via the interactive `pat` / `$MERA_PAT` username/password prompt — **never** embedded in the clone URL, which lands it in `.git/config` and shell history.
- **Go** via the official tarball at `/usr/local/go`, `go1.27.0`. `mxcli` at `~/bin/mxcli`. Work happens in `~/mera/` (WSL2 native filesystem).
- **ICU:** Jord's Ubuntu is recent enough that `libicu78` is the only version `apt` offers. The container base (`debian:bookworm-slim`) uses **`libicu72`** — don't assume 78 transfers. `DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1` is a structural dead end for this app, not an environment issue: it hardcodes loading the real `en-us` culture at startup. Real ICU data is the only fix.
- **Docker:** builds and runs locally, not yet pushed or wired to CI. Use `docker buildx build` (or `DOCKER_BUILDKIT=1`) — it also builds the `build` and `mx-fetch` stages in parallel. Pass `-e MERA_MXCLI_CONCURRENCY=<n>` when testing concurrency; it isn't baked into the image.
- **`scripts/fetch-mx.sh`** scratch lives in `~/.mx-fetch-scratch` (override with `$MX_FETCH_SCRATCH`) — outside the repo, safe to blow away with `clean <version>`. Subcommands: `inspect`, `probe`, `trial-trim`, `deep-inspect`, `finalize [--replace]`, `add-trimmed-version <version> [--replace]`, `restore`, `clean`, `list`. Its health check uses `--help` (this CLI's parser rejects `--version`) and judges success by scanning for crash signatures rather than trusting the exit code, which is non-zero even for a valid `--help`.
- **`mx-versions.txt`** at the repo root is the version-matrix source of truth. Currently just `11.13.0` — the only real-diff-validated version.
- **`.mx-binaries/` and `*.tar.gz` are gitignored.**
**WSL2 traps** (carried over from the deleted getting-started doc):
 
- **Never work under `/mnt/c/`** — roughly 10× slower and produces confusing permission behaviour in containers. Stay in `~/`. Use VS Code's WSL extension for Windows-side editing.
- **CRLF line endings** break shell scripts inside containers. `git config --global core.autocrlf input` inside WSL.
- **`localhost` inside a container is the container.** To reach a service on the host, use `host.docker.internal`.
- **`docker build` caches aggressively.** If a change appears to have no effect, `--no-cache`. And always confirm which build a running container's tag points at before treating a missing file as a real bug — a stale tag once made a working `COPY` look empty.
- **Disk grows fast.** `docker system prune -a` when WSL's virtual disk balloons.
---
 
## 6. Doc consolidation, 2026-08-23

**A standing note for future sessions:** these docs accumulate session narration fast. When a track of work closes, fold its conclusions into the reference docs and delete the blow-by-blow rather than appending another "this session, continued further still" section. §1–§3 above should stay a snapshot, not a log.