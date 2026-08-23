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
| 8 | Real change detection via `mx diff` | **landed**, verified end to end against the real repo — see `MERA-stage8.md` |

---

## 1. Package structure, and why

```
mera-extractor/
├── main.go          ← composition root. Config reading + ListenAndServe, nothing else.
└── internal/
    ├── api/         ← HTTP only: decode request, call domain, encode response
    ├── mxcli/       ← wraps the community mxcli binary. Knows nothing about HTTP.
    ├── gitops/      ← wraps git. Knows nothing about HTTP.
    └── mx/          ← wraps the official mx/mxbuild binaries. Knows nothing about HTTP.
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

### `MERA_MX_ROOT` has now caused three incidents

All three produced an error naming a path that looked correct to the person reading it:

1. **`/.mx-binaries`** — a leading slash makes it a directory at the *filesystem root*, not one in the working directory.
2. **A root one level too shallow.** `Resolve` expects `<root>/<version>/modeler/mx`, one deeper than manual §1.3's diagram shows, because the trimmed tree mirrors the tarball's `modeler/`+`runtime/` layout.
3. **`./.mx-binaries` under `go test`.** `go test` runs each package's binary with *that package's source directory* as the working directory — not the module root, and not where you typed the command. The same relative path resolved differently per package.

Two mitigations, both worth keeping:

- **`describeMxRoot` in `internal/api`** appends the resolved absolute path to any mx-root error (`"./.mx-binaries" (resolved to "/home/…/internal/api/.mx-binaries")`), so the message shows where the process actually looked. An already-absolute root prints unadorned.
- **`resolveMxRoot` in the integration test** expands a literal `~/`, resolves a relative path against the *module root* (nearest ancestor with a `go.mod`), stats it, and globs for `*/modeler/mx` — all before the 15-second clone, so a bad root fails in milliseconds with a diagnosis.

Prefer an absolute path in `.env`, and note `$HOME` is not expanded by every `.env` reader — spell it out.

---

## 3. The concurrency story — Stage 6 was wrong, Stage 7 fixed it

Worth keeping in full, because it's the most instructive mistake in this codebase.

**The workload.** 608 units took 2m39.8s sequentially, with `user`/`sys` time near zero — subprocess-wait bound, not CPU bound. Exactly the profile where concurrency pays. (The test app has since grown to 1,037 units; the shape of the measurement stands.)

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

**`Version` returns a sentence, not a version.** Confirmed from a real run: `"mxcli version v0.19.0 (2026-08-21T13:13:26Z)\n"` — prose prefix, build timestamp, trailing newline. Manual §1.4's `mxcliVersion` field wants a bare `v0.19.0`. Cosmetic today, but it is a provenance field something downstream will eventually parse. Fix belongs in `mxcli.Version`, not at the call site.

---

## 5. Git, credentials and worktrees

**The PAT never touches a URL.** It goes into a `0600` credential-helper file, passed to git via `GIT_CONFIG_KEY_0=credential.helper` / `GIT_CONFIG_VALUE_0=store --file=...`, and is removed the moment git no longer needs it. A URL-embedded credential ends up in `.git/config`, in process listings, and in error messages. `GIT_TERMINAL_PROMPT=0` prevents a bad credential from hanging forever on an interactive prompt instead of failing.

**Errors are redacted before they can reach a log or an HTTP response** — git sometimes echoes the remote URL back in its own errors.

**Every caller-supplied SHA is validated against `^[0-9a-fA-F]{7,64}$` before it reaches an argv.** Without this, a value beginning with `-` is read by git as a flag — `--upload-pack=...` on `fetch` is the classic escalation. This was a latent hole in `Clone` from Stage 5, found while writing `CloneBoth` and retrofitted to both. `TextDiffs` validates its revisions the same way.

**The `.mpr` filename problem has two layers.** Globbing for `*.mpr` handles the on-disk name. But the file also carries an *internal self-reference* that may not match: the test app is git-tracked as `MERA.mpr` while internally referring to `App.mpr`, and `mx` refuses to open it under the "wrong" name (`existing MPR contents refer to MPR file 'App.mpr'`). `mx.PrepareMpr` handles this by parsing that error and retrying against a renamed copy.

> Consequence for callers: **take the `.mpr` path from `PrepareMpr`'s return value, never from your own `FindMpr` call.** The two can legitimately differ, because the workaround copies the file to the demanded name. `CloneBoth` calls `FindMpr` on each worktree purely as fast-fail validation — so a missing model file is reported as "base worktree at `<sha>`" rather than surfacing three layers deep inside `mx` — and deliberately does not return the paths it found.

### `CloneBoth` — two commits side by side

`Clone` checks out one SHA into one directory. `mx diff` needs two, and needs both as real checkouts: **a loose `.mpr` sitting in a directory is not enough, because `git worktree add` requires a real repository.** That was confirmed the hard way and is a hard prerequisite, not an optimization.

The layout under the returned `WorkDir`:

```
repo.git/   bare object store — the only thing that talks to the network
base/       worktree, detached at BaseSha
head/       worktree, detached at HeadSha
```

**The repo is bare on purpose.** The Stage 8 plan originally implied worktrees nested under a normal checkout. Bare is better: nothing ever reads the *main* working tree, so having one means an empty directory something could accidentally be pointed at, and it raises the question of whether `git worktree add` behaves against an unborn `HEAD`. A bare repo removes both. `RepoDir` is returned alongside the worktrees because `TextDiffs` runs against it.

**`--detach` is passed explicitly.** Without it, `git worktree add <path> <commit-ish>` can DWIM a branch named after the path's basename. There is never a branch we want here.

**One `fetch`, both SHAs.** `fetch --filter=blob:none --no-tags origin <base> <head>` in a single call. Fetching by raw object name requires the server to allow unadvertised wants — Team Server does, which is what `Clone` has silently depended on since Stage 5. Worth knowing because a stock local git repo does *not*, which matters for testing (below).

**The credential file has to outlive the fetch — this is the non-obvious one.** Under `--filter=blob:none` the repository is a partial clone, and it is the *checkout* that pulls file contents. Both `worktree add` calls therefore hit the network authenticated. The `defer os.Remove(helperPath)` sits at function scope for exactly that reason; tightening it to just around the fetch would break both worktrees with missing-object errors.

Confirmed empirically rather than assumed: after `CloneBoth`, `repo.git/config` shows `repositoryformatversion = 1`, `remote.origin.promisor = true` and `remote.origin.partialclonefilter = blob:none`. So `git fetch --filter` *does* configure the promisor remote on its own — no explicit `git config extensions.partialClone` step is needed. If a real run ever fails with missing objects, printing that config block is the first diagnostic.

**`Cleanup` needed no change, and now has a test saying so.** The worktree administrative entries live in `repo.git/worktrees/{base,head}` — inside `WorkDir` — so the existing recursive `os.RemoveAll` takes the repo, both worktrees and the admin data in one pass. No `git worktree prune`, no `git worktree remove`. `TestCloneBoth_CleanupLeavesNothing` asserts both admin entries exist beforehand and that `workRoot` is empty afterwards, so a future refactor that moves the repo outside `WorkDir` fails loudly instead of leaking.

**Identical base and head is legal, not an error.** Two *detached* worktrees may sit at the same commit; the "already checked out" guard applies only to branches. An empty diff is a valid review input.

### `TextDiffs` — the non-model half of the commit

`git diff --unified=5 <base> <head> -- javasource theme deployment '*.json'`, run against the **bare repo** and split into one entry per file. Two subprocess calls, not N+1: `--name-status -z` for change kinds, one `--unified=5` for content split on the `diff --git ` header.

**No credentials are passed, and that is safe only because both worktrees were checked out** — which materialised every blob in both trees despite the `blob:none` filter. If this ever did need a lazy fetch it would fail, since `CloneBoth` removes the credential file before returning.

Two bugs the awkward cases caught, both worth remembering:

- **Git appends a trailing TAB to `--- a/` and `+++ b/` lines when and only when the path contains a space.** `theme/web/styles with space.css` came back with a tab attached and never matched its `--name-status` entry — silently mis-keying exactly the hand-authored theme files most worth reviewing. `trimDiffPath` handles it.
- **`-M` pairs an unrelated delete and add as a rename** when the two files are similar enough. That is correct git behaviour, not a bug, but it means an add+delete of near-identical files reports as one `Renamed` entry.

A single file's diff is capped at 256KB with an inline truncation marker: one regenerated theme bundle would otherwise land whole in an HTTP response and then in a model's context window.

### Making git code testable without a network

**`runGit` skips the `credential.helper` config when `helperPath` is empty**, and `writeCredentialHelper` returns `("", nil)` when no PAT was supplied. Configuring `store --file=` with an empty path makes git error rather than fall through to anonymous access. `Clone` always passes a real path, so its behaviour is unchanged — but this is what allows a test to point at a `file://` fixture repo with no credentials at all (`hostOf` rejects a `file://` URL by design, since it has no host).

**A local fixture repo needs two server-side settings**, and both are easy to miss:

- `uploadpack.allowAnySHA1InWant` — without it, `git fetch origin <sha>` is refused with *"Server does not allow request for unadvertised object"*. Stock git defaults to off.
- `uploadpack.allowFilter` — without it the server silently ignores `--filter=blob:none`, so the test passes while exercising a *full* fetch instead of the partial-clone path actually shipped. A green test that tests the wrong code path is worse than a red one.

Use a `file://` URL, not a bare path: a plain path triggers git's local-transport optimization and bypasses `upload-pack` entirely, which again means testing something other than what runs in production.

### Integration tests must not be able to skip silently

**`t.Skip` is invisible.** `go test ./...` prints `ok` in 0.006s for a package whose integration tests all skipped — indistinguishable from one where they ran and passed. Nothing in Go reads `.env` either, so forgetting `set -a; source .env; set +a` produces a green, instant, entirely hollow run. That is worse than a red one, and it happened.

`requireIntegrationEnv` (duplicated in `internal/api` and `internal/gitops`, since a test helper cannot cross a package boundary without becoming exported production code) gives three outcomes and no silent fourth:

| State | Behaviour |
|---|---|
| `MERA_IT=1` set | Must run — any missing variable is a **failure** naming it |
| Some variables set, others not | Half-configured — **failure** |
| Nothing set | Skip, printing the command that enables it |

It also logs `INTEGRATION: <repo>  <base>..<head>` on entry, so `-v` shows which commits were exercised — a stale `.env` pointing at the wrong pair is otherwise invisible.

> `MERA_IT=1` belongs in the shell, not in `.env`. Its job is to assert *this particular run* is meant to hit the network, which is exactly what `.env` cannot know. And `set -a` requires real shell syntax: `KEY = value` with spaces around the `=` is not an assignment and silently gives you nothing. Verify with `env | grep '^MERA_'` before blaming the code. For the editor's test runner, set `"go.testEnvFile": "${workspaceFolder}/.env"`.

---

## 6. The `/extract` pipeline

`handleExtract` runs: `CloneBoth` → `PrepareMpr` per side → version-matched binary → `Diff` → `ResolveQualifiedNames` per side → plan → targeted describe → `TextDiffs` → respond. Files: `internal/api/extract.go` and `internal/api/deps.go`, ~35 tests at 93.5% statement coverage, green under `-race`, and verified end to end against the real Team Server repo (see `MERA-stage8.md` §1).

### The `Deps` seam

Everything that shells out or touches the network reaches the handler through a `Deps` struct of **function fields**, held on `Server`.

Function fields rather than an interface, because a test almost never wants to fake all twelve — it overrides the two it cares about and the rest stay real. An interface would force every fake to implement the whole surface. `withDefaults()` fills each nil field with the real implementation, so **the zero value means "use the real tools"** and adding the seam cost exactly one new field on `Server`; `NewServer` and every existing construction site were untouched.

`withDefaults()` runs per request rather than in `NewServer`, so a `Server` built as a struct literal — which every test does — behaves identically to one from the constructor. `TestDepsWithDefaults_FillsEveryNil` walks the struct reflectively, so adding a field and forgetting its `withDefaults` line fails a test instead of nil-panicking in production.

This is not a regression against §1's no-globals rule: the seam is per-`Server` state, not a package-level variable, and it carries behaviour rather than configuration.

### The type-mapping problem — the crux of the stage

`mx diff` reports `Microflows$Microflow`. `mxcli describe` wants `microflow`. Nothing in Mendix's documentation maps between them, and **this translation is most of what makes targeted describe work at all.**

#### There are two vocabularies

```
mx dump-mpr    →  Pages$Page   Pages$Snippet
mx analyze-mpr →  Forms$Page   Forms$Snippet   Forms$Layout   Forms$BuildingBlock
```

`analyze-mpr` reports storage-level names, `dump-mpr` the current public metamodel names — Mendix evidently renamed the `Forms` namespace to `Pages` without changing what is on disk. Most types are spelled identically in both (`Microflows$Microflow`, `Images$ImageCollection`, `JavaActions$JavaAction`, `DomainModels$DomainModel`, `Projects$Folder`…).

**Which one `mx diff` speaks is still unknown.** Both spellings are mapped, so the code is correct either way; one half is dead weight to be deleted once a page-touching commit settles it.

> **An earlier revision of this section claimed the two tools share one vocabulary**, citing `Microflows$Microflow` and `Images$ImageCollection` appearing in both a real diff and a real dump. That evidence was worthless: those are exactly the types where the vocabularies agree, so they cannot discriminate. **Overlapping observations are not confirming observations** — the lesson generalises well beyond this map.

#### Three tiers, and the tiering is the design

**1. `diffTypeToMxcli` — confirmed.** Read off real `dump-mpr` and `analyze-mpr` output, matched against `mxcli describe --help` (v0.19.0): `Microflows$Microflow`→`microflow`, `Microflows$Nanoflow`→`nanoflow`, `Pages$Page`/`Forms$Page`→`page`, `Pages$Snippet`/`Forms$Snippet`→`snippet`, `Forms$Layout`→`layout`, `Forms$BuildingBlock`→`buildingblock`, `Constants$Constant`→`constant`, `Enumerations$Enumeration`→`enumeration`, `JavaActions$JavaAction`→`javaaction`, `JsonStructures$JsonStructure`→`jsonstructure`, `ImportMappings$ImportMapping`→`importmapping`, `ExportMappings$ExportMapping`→`exportmapping`, `Images$ImageCollection`→`imagecollection`.

Plus four confirmed type strings that appear *nested* in dump-mpr but carry a real `$QualifiedName` and have an mxcli type — `DomainModels$Entity`, `DomainModels$Association`, `DomainModels$CrossAssociation`, `Security$ModuleRole`. Whether `mx diff` ever reports at member granularity is unknown. Harmless if never hit.

> An earlier revision asserted `Images$ImageCollection` had **no** mxcli equivalent. It does — `imagecollection` — and the real run rendered MDL for one. That error came from reasoning about the fixture instead of reading the tool's own help output.

**2. `knownNotDescribable` — expected to produce no MDL, with a reason.** `Projects$Folder`, `Projects$ModuleSettings`, `DomainModels$DomainModel`, `Security$ModuleSecurity`, `Projects$ProjectConversion`, `Forms$PageTemplate`, `JavaScriptActions$JavaScriptAction`, `CustomIcons$CustomIconCollection`, `Texts$SystemTextCollection`. And deliberately:

> **`Projects$Module` is not mapped, even though `mxcli describe module` exists.** That command renders the module's *entire contents* — a module-level change would drag in every unit of the module and reintroduce exactly the full-app enumeration this stage exists to eliminate. Its individual changed units are reported separately anyway. This is a decision, not an omission.

**3. Everything else — one warning per distinct type**, naming the exact string `mx` emitted and saying which table to add it to.

Separating tier 2 from tier 3 is what keeps the warning list actionable. If folders and domain models warned on every commit that touched one, the list would become noise and get ignored — and the *real* signal, an unrecognised type, would be lost in it.

**Completeness was never the goal; self-correction was.** An unclassified type still produces a `changeUnit` carrying type, changeKind, `structuralDelta` and a resolved name — just no MDL — and tells you exactly what to add. That is why a handful of `UNVERIFIED` pattern guesses are acceptable: a wrong key never matches, and a wrong value degrades to a per-unit describe warning. `Microflows$Nanoflow` was one such guess and turned out right.

The same reasoning is why roughly eighteen further mxcli types — `restclient`, `odataservice`, `queue`, `scheduledevent`, `navigation`, `settings`, `agent`, `aimodel` and the rest — are **left out on purpose**. Their mxcli side is known but no metamodel string is confirmed for any of them, and inventing eighteen namespaces would put fiction into a lookup table that reads as authoritative.

`TestTypeTablesAreCoherent` guards all of it: every mapped value must exist in the v0.19.0 type list, no key may appear in both tables, keys must look like `Namespace$Type`, and every type observed in real output must be classified somewhere.

### `analyze-mpr` is a size report, and parsing it needs section tracking

Sections: `MPR File Analysis`, `BSON contents`, `Content categories`, `Size by unit type`, `Size by property`, `Size by unit`, `Size by module`. Each is preceded by a blank line and followed by a rule of dashes — **the blank line is what closes the previous section**, and the parser depends on it.

`Size by property` rows are shaped identically to `Size by unit type` rows and sit directly beneath them, so without section tracking `Forms$PageTemplate.ImageData` lands in the unit-type inventory. A dot in the name is the second guard.

`AnalyzeResult.UnitTypeCounts` is the app's real unit-type inventory, which is how the vocabulary question above got asked at all. Counts arrive comma-formatted over 999.

### Detect an unreadable snapshot on failure, never predict it

The single most instructive bug of this stage.

`handleExtract` used to return `422 unsupportedVersionMigrationCommit` whenever `analyze-mpr` mentioned `Projects$ProjectConversion` anywhere in its output. The first real integration run rejected **every commit of the test app**.

Two errors compounded:

1. **A `Projects$ProjectConversion` unit persists in the model after an upgrade completes.** It is a record of the conversion, not a conversion in progress. The test app carries exactly one and diffs perfectly.
2. **Manual trap #16 actually says *"if diffing **fails** on a project containing a `Projects$ProjectConversion` unit"*.** The condition is the failure; the unit is only the explanation. Dropping the condition and keeping the explanation rejects every app that has ever been upgraded.

Now: presence is a warning, and the `422` fires when `mx diff` actually fails with `mx.VersionMigrationSignature` in its stderr — matched against both `ErrDiffFailed` and `ErrUnexpectedExitCode`, since this CLI family's exit codes have already proven unreliable once. The signature deliberately omits the trailing `but got 'Associations'`, because the property name will not be stable.

> **Why not compare base and head Mendix versions instead?** It is neither necessary nor sufficient. A *completed* upgrade commit pair has different versions on each side and parses fine — gating on it would reject exactly the reviews most worth having. And a genuinely mid-migration snapshot can report the same version on both sides, since the conversion is in flight within one commit. The comparison stays a warning.

The old tests encoded the old assumption, and both fed the parser a `Projects$ProjectConversion: 1` line the tool has never emitted. A test built on an imagined output format validated a parser that didn't care about format either — two guesses cancelling out. The rewritten tests build a report in the real layout via a helper, so no test in that file can invent output shape again.

### Two names, one response field

A unit renamed inside one commit has a different qualified name in base and in head. The frozen contract has a single `qualifiedName`, so the internal `changePlan` carries **both** all the way to the describe stage: `beforeMdl` is fetched from `base.mpr` under the old name, `afterMdl` from `head.mpr` under the new one. Collapsing to one name before describe runs would silently produce a wrong or empty `beforeMdl`.

The response reports the head-side name, falling back to base for a deleted unit, and adds `previousQualifiedName` (omitempty) when the two differ. That field is **not** in manual §1.4 — it is additive, so no existing consumer sees a change, and dropping it would lose information the reviewer needs.

### What degrades and what is fatal

Manual §1.4's partial-success rule was applied case by case, not blanket-applied:

**Warnings, request still `200`:** a describe failure on one unit; a `Projects$ProjectConversion` unit present; a base/head Mendix version mismatch (head's version selects the binary, since head is what's under review); `mx diff` exiting 2 with conflicts found, which the manual confirms is still usable output; a `TextDiffs` failure, since the text half is independent of the model half; a failure to read the mxcli version, which is provenance rather than payload.

**Also a warning — and this one is debatable:** a wholesale `ResolveQualifiedNames` failure. Failing the request would throw away a successful clone and diff, and the units still carry type, changeKind and `structuralDelta`, which is a diminished but real review payload. If experience says a nameless response is worse than no response, it is a three-line change.

**Fatal:** `mx diff` failing with the migration signature (`422`); no binary matching the detected version (`422`, per manual §1.3's fail-loudly-never-substitute rule); a clone failure (`502`); no binaries at all under `MxRoot` (`500` — our misconfiguration, not the caller's). `ErrUnsupportedMendixVersion` from diff is `422` because the caller chose the commits; every other diff failure is `500`.

### The escape hatch

`Units`/`Modules` skip the diff path entirely and run the old naive enumeration. Two decisions the checklist did not record:

- **It enumerates against the HEAD worktree.** The request no longer carries a single `sha`, and head is the only defensible reading of "the app" when two commits are in play.
- **It returns a separate `legacyExtractResponse` type**, not the new struct with fields omitted. That way "the old response is unchanged" is guaranteed by the type system rather than by careful `omitempty` tagging, and a test asserts the new field names never appear in that body.

### Deliberately not done

Recording these so they read as decisions rather than oversights:

- **The error envelope.** Manual §1.4 specifies `{requestId, error, detail, retryable}`; `respondError` still emits `{"error": "..."}`.
- **Request auth shape.** Still flat `username`/`pat` rather than §1.4's `auth: {kind, username, secret}`, with `provider` and `options` absent. Changing it would break the current caller for no benefit yet.
- **`storageFormat` is emitted empty.** `AnalyzeResult` does not expose it, and emitting `"MPRv2"` would fabricate a value in a field whose entire purpose is provenance.
- **`referenceGraph`** — manual §1.6, a later phase.

---

## 7. Still open

1. **`/clone`'s workDir is never reaped.** Unlike `/extract` (whose whole lifecycle fits in one request, so `defer gitops.Cleanup` is correct), a standalone `/clone` deliberately outlives its request so the caller can use the checkout — and nothing currently cleans it up. It sits on disk until the container restarts. Fine for local testing; must be solved before this runs unattended. This is exactly what the leased-workspace TTL in manual §1.8 exists for. `CloneBoth` inherits the same exposure the moment anything calls it outside a single request.

2. **`/health` queues behind the semaphore.** It calls `mxcli.Version`, which funnels through the same gated `run()`, so under a fully-loaded extraction a health check can queue rather than returning instantly. Arguably correct backpressure — but if a deployment platform's liveness probe has a tight timeout this needs a decision. Options: a check that doesn't call `mxcli` at all, or exempt `Version` from the semaphore.

3. **`internal/mxcli` has only the concurrency test.** A correctness test for its `run()` banner-stripping and error behaviour is still worth writing. Everything else is well covered: `internal/api` 93.5%, `internal/gitops` 72.1%, `internal/mx` including real captured `analyze-mpr` and `mx diff` output as fixtures.

4. **`CloneBoth`'s 300s timeout is a guess** — 2.5× `Clone`'s budget for roughly 2× the work, plus slack. A package-level `var` so a test can shrink it. Replace with a measurement once real `/extract` calls run against apps larger than the test app's 1,037 units. Real numbers so far: `/extract` 35s, `CloneBoth` alone 13s.

5. **`Forms$` vs `Pages$` is unresolved** (§6). One integration run against a page-touching commit pair settles it; then delete the dead half of the map.

6. **`textDiffs` has never returned a non-empty result from the real repo.** Fully unit-tested against a local fixture, but the proving commit pair touched no `javasource`. Needs one run against a Java-touching pair.

7. **`mxcli.Version` returns a sentence** (§4). Should return `v0.19.0`.

8. **The `mx` calls in `/extract` run sequentially** — two `PrepareMpr`, then `Diff`, then two `ResolveQualifiedNames`: five .NET process startups back to back, and the measured 35s is mostly them. Both sides of `PrepareMpr` and of `ResolveQualifiedNames` are independent and could run concurrently, roughly halving the `mx` portion. Left sequential until a timing measurement says it matters; `internal/mx` has no semaphore, so this is free to parallelise when it does.