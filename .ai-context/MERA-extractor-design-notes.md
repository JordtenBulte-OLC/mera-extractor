# MERA Extractor — Design Notes

**What this is:** the durable design decisions and hard-won gotchas behind the extractor sidecar's Go code. **The repository is the source of truth for the code itself** — this doc deliberately holds no source listings. It exists so a future session (or a new contributor) understands *why* the code is shaped the way it is, and doesn't re-make a mistake that has already been made and fixed.

Supersedes `MERA-extractor-getting-started.md` (Stages 1–4, deleted — a Docker/Go tutorial whose output has been replaced twice) and `MERA-extractor-restructure-and-extract.md` (Stages 5–7, deleted — the full Go source of every file, now redundant with the repo).

Stage history in one line each:

| Stage | What it did | Status |
|---|---|---|
| 1–4 | mxcli by hand → Go service with `/health` + `/describe` → Docker → deployment concepts | superseded |
| 5 | Restructured into `internal/` packages; added `/clone` and `/extract` (naive full enumeration) | landed |
| 6 | Per-request worker pool for `/extract` | **superseded by 7** |
| 7 | Global subprocess concurrency cap in `internal/mxcli` | landed, race-tested |
| 8 | Real change detection via `mx diff` | in progress — see `MERA-stage8.md` |

---

## 1. Package structure, and why

```
mera-extractor/
├── main.go          ← composition root. Config reading + ListenAndServe, nothing else.
└── internal/
    ├── api/         ← HTTP only: decode request, call domain, encode response
    ├── mxcli/       ← wraps the community mxcli binary. Knows nothing about HTTP.
    ├── gitops/      ← wraps git. Knows nothing about HTTP.
    └── mx/          ← wraps the official mx/mxbuild binaries (Stage 8). Knows nothing about HTTP.
```

Three rules hold this together, and Stage 8 follows all three:

**`internal/` is compiler-enforced, not a convention.** Go refuses to let code outside this module import anything under `internal/`. For a deployable service nobody is meant to import as a library, everything except `main.go` belongs there.

**Only `internal/api` imports `net/http`.** The binary-wrapper packages take Go structs and return Go structs or errors. This is what makes them testable without starting a server, and what makes a CLI harness (manual §1.9) nearly free — it imports the wrappers directly and never touches `api`.

**`main.go` is the one place concrete things get wired together.** If you're looking for where something is configured, it's there. Corollary: handlers are methods on a `Server` struct so they can receive config (`WorkRoot`, `MxRoot`) without package-level globals.

Naming: don't stutter the package into its contents — `mxcli.Describe()`, not `mxcli.MxcliDescribe()`.

**One binary per package.** `mxcli` (community tool: `describe`, `show`/`list`, `search`) and `mx` (Mendix's official CLI: `diff`, `dump-mpr`, `analyze-mpr`) are *different binaries* with confusingly similar names. They get separate packages named after the binary each wraps. Never fold one into the other.

---

## 2. Configuration is injected, never hardcoded

Two bugs this rule prevented, both real:

- **`/work` only exists inside the container** (created by `WORKDIR /work`). Hardcoding it means `/clone` and `/extract` work in Docker and crash under `go run .`. Fix: `MERA_WORK_ROOT` env var, defaulting to `os.TempDir()` locally, set to `/work` in the Dockerfile.
- **`App.mpr` is not a safe filename assumption** — the real test app is `MERA.mpr`. Glob for `*.mpr` in the checkout. (This has a second, nastier dimension — see §5.)

Same reasoning applies to `PORT` (default `8080`) and `MERA_MX_ROOT` (default `/opt/mx`). Container platforms split into two families: some *inject* `PORT` and require you to honour it (Cloud Run), others expect you to declare your listening port as static config (Azure Container Apps, Fargate, plain `docker run -p`). Reading the env var with a sensible default is correct for both.

> **`MERA_MX_ROOT` must be absolute, and it points at the directory *containing* the version directories.** Two mistakes cost real time: `/.mx-binaries` (a leading slash makes it a directory at the filesystem root, not one in the working directory — use `$HOME/mera/extractor/.mx-binaries` locally), and forgetting that `Resolve` expects `<root>/<version>/modeler/mx`, one level deeper than manual §1.3's diagram shows. `ls -d "$MERA_MX_ROOT"/*/modeler/mx` should list one line per installed version. A bad root is a startup *warning*, not a fatal — the service still binds, then fails loudly at request time per the no-fallback rule.

---

## 3. The concurrency story — Stage 6 was wrong, Stage 7 fixed it

Worth keeping in full, because it's the most instructive mistake in this codebase.

**The workload.** 608 units took 2m39.8s sequentially, with `user`/`sys` time near zero — subprocess-wait bound, not CPU bound. Exactly the profile where concurrency pays.

**Stage 6: a per-request worker pool.** Fixed number of goroutines reading job indices off a shared channel; each writes only to its own slot in a pre-sized slice (disjoint slots → no mutex needed); `sync.WaitGroup`'s happens-before guarantee makes reading the slice after `Wait()` safe. Output order matches input order regardless of completion order. All of that reasoning is still correct and still in use.

**What real testing found wrong.** Two things:

1. `nproc` on this machine is 8, and forcing 25 workers performed the same or slightly worse. There is no benefit to running more `mxcli` processes than the machine can schedule.
2. **The cap was scoped to one HTTP request.** Two overlapping `/extract` calls each spun up their own pool of 8 and could together spawn 16 real processes on an 8-core box — the exact opposite of what a concurrency cap is supposed to guarantee.

**Stage 7: move the cap to where the resource actually lives.** Every function in `internal/mxcli` (`Describe`, `ListUnits`, `Version`) funnels through one `run()` helper, so a semaphore gating `run()` bounds every caller at once, across every request and endpoint. Two details worth preserving:

- **Acquire the semaphore slot *before* starting the per-call timeout.** The timeout is an execution budget, not a queue-wait budget. Queue wait is bounded instead by the caller's own `ctx`, via a `select` against `ctx.Done()`.
- **Default to `runtime.NumCPU()`**, overridable via `MERA_MXCLI_CONCURRENCY`. The caveat: inside a container with a CPU quota *below* the host's capacity, `NumCPU()` reports the host's count and overstates what you may use. That's what the env var is for.

**Why this global isn't a regression against the no-globals rule.** `WorkRoot`, `PORT`, and the old `ExtractConcurrency` are *configuration that could vary per instance* — globals hide that badly. The subprocess concurrency limit is a property of the one operating system this one process runs on: genuinely singleton, like a database connection pool. Modeling a real global resource as a global is correct.

**One ordering constraint.** `SetMaxConcurrent` swaps the semaphore channel outright rather than draining it. Call it exactly once, at startup, before `ListenAndServe` — a `run()` call already blocked on the *old* channel would hang forever. This is why it's documented as a startup-time knob, not a live-reloadable one; changing it while serving traffic needs a different data structure (`atomic.Pointer` with careful in-flight handling).

**Consequence for `/extract`.** With the cap enforced centrally, the dispatch loop no longer needs to know a limit exists: fire one goroutine per unit and let each block inside `mxcli.Describe` until a slot frees (or `ctx` cancels, which `run()`'s own `select` already handles). A burst of goroutines blocked on a channel receive costs a few KB of stack each — no need to throttle dispatch too.

**Proven, not assumed.** `internal/mxcli/concurrency_test.go` fires two overlapping 10-item bursts against a limit of 4 and asserts observed max concurrency never exceeds 4 — the exact cross-request property Stage 6 could not provide. Passes under `-race`.

---

## 4. mxcli gotchas

**The startup banner goes to stdout, not stderr.** `WARNING: This is a vibe-coded PoC...` and `Connected to: ...` are prepended to *every* subcommand's stdout, confirmed by surviving a plain pipe. There is no universal suppression flag — `--quiet` exists only on `search`, and the global `--json` flag doesn't suppress it either. This was silently corrupting every `Describe()` result from day one (invisible in MDL text, fatal to JSON parsing). Stripped centrally in `run()` so no caller has to think about it.

> **Fragility warning:** `stripBanner` matches alpha software's literal banner text, not a stable API. If output ever looks like it has stray text at the top again, check this first. If a real suppression flag ever appears in `mxcli --help`, prefer it.

**`describe` takes the singular unit type, `show` takes the plural.** `describe ... microflow` vs `show ... microflows`. Confirmed inconsistent, not a typo. The wrapper takes the singular form everywhere (matching `Describe`'s convention) and translates internally via a `pluralType` map — which returns an explicit error for an unmapped type rather than guessing.

**`show <type> --json` returns `"Qualified Name"` with a literal space** in the key. The struct tag must match exactly. Each unit type also carries type-specific extra fields (`McCabe`, `Params`, `Returns` for microflows); `encoding/json` ignores unmatched fields, so a minimal struct stays correct without modeling every schema.

**Scope enumeration at the CLI, not in Go.** `mxcli show <type> <moduleName>` scopes a listing to one module. Filtering the full result set in Go instead would pay the full listing cost *and* the full `describe` cost downstream for units you never wanted.

**Degrade per unit, never fail the batch.** One bad unit records a warning and the loop continues; the response is `200` with `mdl` for what worked and `warning` for what didn't. This is manual §1.4's core rule and the difference between MERA working on large legacy apps and falling over on exactly the ones that need it most.

---

## 5. Git, credentials and worktrees

**The PAT never touches a URL.** It goes into a `0600` credential-helper file, passed to git via `GIT_CONFIG_KEY_0=credential.helper` / `GIT_CONFIG_VALUE_0=store --file=...`, and is removed the moment git no longer needs it. A URL-embedded credential ends up in `.git/config`, in process listings, and in error messages. `GIT_TERMINAL_PROMPT=0` prevents a bad credential from hanging forever on an interactive prompt instead of failing.

**Errors are redacted before they can reach a log or an HTTP response** — git sometimes echoes the remote URL back in its own errors.

**Every caller-supplied SHA is validated against `^[0-9a-fA-F]{7,64}$` before it reaches an argv.** Without this, a value beginning with `-` is read by git as a flag — `--upload-pack=...` on `fetch` is the classic escalation. This was a latent hole in `Clone` from Stage 5, found while writing `CloneBoth` and retrofitted to both. The 7–64 range covers an abbreviated name through a future SHA-256 object ID.

**The `.mpr` filename problem has two layers.** Globbing for `*.mpr` handles the on-disk name. But the file also carries an *internal self-reference* that may not match: the test app is git-tracked as `MERA.mpr` while internally referring to `App.mpr`, and `mx` refuses to open it under the "wrong" name (`existing MPR contents refer to MPR file 'App.mpr'`). `mx.PrepareMpr` handles this by parsing that error and retrying against a renamed copy.

> Consequence for callers: **take the `.mpr` path from `PrepareMpr`'s return value, never from your own `FindMpr` call.** The two can legitimately differ, because the workaround copies the file to the demanded name. `CloneBoth` calls `FindMpr` on each worktree purely as fast-fail validation — so a missing model file is reported as "base worktree at `<sha>`" rather than surfacing three layers deep inside `mx` — and deliberately does not return the paths it found.

### `CloneBoth` — two commits side by side (Stage 8, Step 6)

`Clone` checks out one SHA into one directory. `mx diff` needs two, and needs both as real checkouts: **a loose `.mpr` sitting in a directory is not enough, because `git worktree add` requires a real repository.** That was confirmed the hard way and is a hard prerequisite, not an optimization.

The layout under the returned `WorkDir`:

```
repo.git/   bare object store — the only thing that talks to the network
base/       worktree, detached at BaseSha
head/       worktree, detached at HeadSha
```

**The repo is bare on purpose.** The Stage 8 plan originally implied worktrees nested under a normal checkout. Bare is better: nothing ever reads the *main* working tree, so having one means an empty directory something could accidentally be pointed at, and it raises the question of whether `git worktree add` behaves against an unborn `HEAD`. A bare repo removes both. The trade is that this diverges from `Clone`'s shape — accepted, because the two have genuinely different jobs.

**`--detach` is passed explicitly.** Without it, `git worktree add <path> <commit-ish>` can DWIM a branch named after the path's basename. There is never a branch we want here.

**One `fetch`, both SHAs.** `fetch --filter=blob:none --no-tags origin <base> <head>` in a single call. Fetching by raw object name requires the server to allow unadvertised wants — Team Server does, which is what `Clone` has silently depended on since Stage 5. Worth knowing because a stock local git repo does *not*, which matters for testing (below).

**The credential file has to outlive the fetch — this is the non-obvious one.** Under `--filter=blob:none` the repository is a partial clone, and it is the *checkout* that pulls file contents. Both `worktree add` calls therefore hit the network authenticated. The `defer os.Remove(helperPath)` sits at function scope for exactly that reason; tightening it to just around the fetch would break both worktrees with missing-object errors.

Confirmed empirically rather than assumed: after `CloneBoth`, `repo.git/config` shows `repositoryformatversion = 1`, `remote.origin.promisor = true` and `remote.origin.partialclonefilter = blob:none`. So `git fetch --filter` *does* configure the promisor remote on its own — no explicit `git config extensions.partialClone` step is needed. If a real run ever fails with missing objects, printing that config block is the first diagnostic.

**`Cleanup` needed no change, and now has a test saying so.** The worktree administrative entries live in `repo.git/worktrees/{base,head}` — inside `WorkDir` — so the existing recursive `os.RemoveAll` takes the repo, both worktrees and the admin data in one pass. No `git worktree prune`, no `git worktree remove`. `TestCloneBoth_CleanupLeavesNothing` asserts both admin entries exist beforehand and that `workRoot` is empty afterwards, so a future refactor that moves the repo outside `WorkDir` fails loudly instead of leaking.

**Identical base and head is legal, not an error.** Two *detached* worktrees may sit at the same commit; the "already checked out" guard applies only to branches. An empty diff is a valid review input, so this is covered by a test rather than rejected.

### Making git code testable without a network

Two small changes unlocked hermetic tests for a package that previously had none.

**`runGit` now skips the `credential.helper` config when `helperPath` is empty**, and `writeCredentialHelper` returns `("", nil)` when no PAT was supplied. Configuring `store --file=` with an empty path makes git error rather than fall through to anonymous access. `Clone` always passes a real path, so its behaviour is unchanged — but this is what allows a test to point at a `file://` fixture repo with no credentials at all (`hostOf` rejects a `file://` URL by design, since it has no host).

**A local fixture repo needs two server-side settings**, and both are easy to miss:

- `uploadpack.allowAnySHA1InWant` — without it, `git fetch origin <sha>` is refused with *"Server does not allow request for unadvertised object"*. Stock git defaults to off.
- `uploadpack.allowFilter` — without it the server silently ignores `--filter=blob:none`, so the test passes while exercising a *full* fetch instead of the partial-clone path actually shipped. A green test that tests the wrong code path is worse than a red one.

Use a `file://` URL, not a bare path: a plain path triggers git's local-transport optimization and bypasses `upload-pack` entirely, which again means testing something other than what runs in production.

**Guarded integration test.** `TestCloneBoth_Integration` skips unless `MERA_IT_REPO`, `MERA_PAT`, `MERA_IT_BASE_SHA` and `MERA_IT_HEAD_SHA` are all set, so `go test ./...` stays hermetic and offline. It asserts more than "no error": each worktree's `.mpr` is non-empty and `mprcontents/` exists, which is where a partial clone that faked the checkout would show. Green against the real Team Server repo. This is the pattern for the remaining Stage 8 integration tests to copy.

> **Running it:** nothing in Go reads a `.env` file — not the standard library, not `go test`. Whatever loads yours (a `godotenv` call in `main.go`, docker compose, the VS Code Go extension) does not apply to `go test` from a terminal. Use `set -a; source .env; set +a` first, and note that `set -a` requires real shell syntax — `KEY = value` with spaces around the `=` is not an assignment and silently gives you nothing. Verify with `env | grep '^MERA_'` before blaming the code. For the editor's test runner, set `"go.testEnvFile": "${workspaceFolder}/.env"`.

---

## 6. Still open

1. **`/clone`'s workDir is never reaped.** Unlike `/extract` (whose whole lifecycle fits in one request, so `defer gitops.Cleanup` is correct), a standalone `/clone` deliberately outlives its request so the caller can use the checkout — and nothing currently cleans it up. It sits on disk until the container restarts. Fine for local testing; must be solved before this runs unattended. This is exactly what the leased-workspace TTL in manual §1.8 exists for. `CloneBoth` inherits the same exposure the moment anything calls it outside a single request.

2. **`/health` now queues behind the semaphore.** As of Stage 7 it calls `mxcli.Version`, which funnels through the same gated `run()`. Under a fully-loaded extraction a health check can queue rather than returning instantly. Arguably correct — it reflects real backpressure — but if a deployment platform's liveness probe has a tight timeout this needs a decision. Options: give `/health` a check that doesn't call `mxcli` at all, or exempt `Version` from the semaphore.

3. **Test coverage is uneven.** `internal/mx` is well covered (see `MERA-stage8.md`), and `internal/gitops` now has hermetic fixture-repo tests plus a guarded integration test. `internal/mxcli` has only the concurrency test — a correctness test for `describeAll`'s ordering and warning behaviour is still worth writing, and it needs its own stub seam now that the function no longer takes a worker count to control.

4. **`CloneBoth`'s 300s timeout is a guess.** It covers one fetch plus two full Mendix checkouts, each paying its own lazy-blob round trip — 2.5× `Clone`'s budget for roughly 2× the work, plus slack. It is a package-level `var` so a test can shrink it. Replace the guess with a measurement once real `/extract` calls are running against apps larger than the 608-unit test app.