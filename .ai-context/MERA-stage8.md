# Stage 8 — Real change detection with `mx diff`

**The active build document.** Replaces the naive full-enumeration `/extract` with real `mx diff`-driven change detection. Merges what were previously `MERA-stage8-plan.md` (design) and `MERA-stage8-checklist.md` (status) — they had started to contradict each other, and one of the plan's stated assumptions turned out to be false against real data (see §2, `$QualifiedName` on nested nodes).

**Progress: Steps 1–6 built and tested. Steps 7–12 remain.** Step 6 is the first part of Stage 8 verified against the real Team Server repo, not just against stubs and captured fixtures; the `mx`-side integration runs in §6 are still outstanding.

**Read before writing code:** `MERA-redesign-architecture.md` §2 (the `.mpr` problem and the `ChangeUnit` contract) and `MERA-implementation-manual.md` §1.3–§1.5 (version matrix, the frozen REST contract, the 14-step extraction algorithm). This doc is orientation, not a substitute.

**Package boundary:** `internal/mx` wraps the official `mx`/`mxbuild` binaries (version selection, `diff`, `dump-mpr`, `analyze-mpr`). `internal/mxcli` wraps the community `mxcli` tool (`describe`, `show`/`list`). Two different binaries, two packages, each named for the binary it fronts. Keep them separate — see `MERA-extractor-design-notes.md` §1.

---

## 1. What Stage 8 actually changes

The current `/extract` renders *every* unit in the app (or in named modules) because there was no way to know what changed. That stand-in did its job — it unblocked the whole agent side of the project — and this stage retires it.

- **REST contract** moves from a single `sha` to `baseSha`/`headSha`, and from a flat `unitResult[]` to real `ChangeUnit[]` (manual §1.4's frozen shape). This is most of what Stage 8 *is*, code-wise.
- **`gitops.Clone` checks out one SHA**; diffing needs both. One clone, two `git worktree add` calls — a real change to `gitops`'s contract, not just the API layer.
- **Only changed units get described**, before and after, instead of all 608. That's the performance and cost win on top of the correctness win.
- **Version detection has to happen before `mx diff` runs**, so the right `/opt/mx/<version>/modeler/mx` is selected — and fail loudly on no match rather than falling back to the newest and hoping.
- **Stage 7's global concurrency cap already covers the fan-out.** No new concurrency design needed; just fewer, more targeted units flowing through the same pattern.

---

## 2. Confirmed facts about `mx` / `dump-mpr`

Durable reference. Every item below was confirmed against real output or real `--help`, not inferred from docs.

**`mx diff` gives you a GUID, not a name.** Its output shape is `{"base", "mine", "unitDifferences": [{type, id, change, containerId, containmentName}]}` — no qualified name anywhere. `mxcli describe` needs `Module.Name`.

**`mx dump-mpr` resolves it in one step.** Objects carry `$ID` alongside `$QualifiedName`, already in `Module.Name` shape; `Module` is everything before the first `.`. No cross-referencing against `mxcli`'s own listing is needed — both sides come from the same official tool family.

**`--output-file` is a real flag; `dump-mpr` does NOT take a bare positional output path.** Omitting it dumps the entire JSON to stdout — that's the "huge wall of text, empty file" symptom.

**`--unit-type` and `--module-names` each accept a comma-separated list in one invocation.** One `dump-mpr` call per side, not one per type.

**Real top-level shape is `{"units": [...]}`** — not a bare array, not the raw document tree.

**Both `mx diff` and `dump-mpr` write output files with a UTF-8 BOM** (`ef bb bf` before the `{`), confirmed by hexdump. `encoding/json` refuses to parse that. `stripBOM` is applied in every read path — this is a .NET binary, so assume the BOM everywhere.

**Nested objects DO carry `$QualifiedName`** — the original plan claimed only top-level document objects did, and that was wrong. `DomainModels$Attribute`, `DomainModels$Association`, `Enumerations$EnumerationValue`, `Pages$PageParameter`, `JavaActions$JavaActionParameter`, `Microflows$MicroflowParameterObject`, `Security$ModuleRole` all carry both fields at depth. The code was always right (the recursion never stopped at depth 1); only the explanatory comment was misleading.

**Some top-level units carry `$ID` with NO `$QualifiedName` at all** — `Projects$Folder`, `DomainModels$DomainModel`, `Projects$ModuleSettings`, `Security$ModuleSecurity`. These must be synthesized, not dropped (see §4, Step 5).

**The two exit-code tables are different. Do not share a switch.**

| Code | `mx diff` | `mx dump-mpr` |
|---|---|---|
| 0 | OK | OK |
| 1 | — | wrong project file |
| 2 | conflicts found (**still usable**) | invalid unit type(s) |
| 3 | — | unknown JSON export error |
| 4 | unsupported `.mpr` version | project in a different Mendix version |
| 129 | generic diff error | — |

**Verify exit codes empirically anyway.** This CLI family exits non-zero for a valid `--help`, which already broke the "0 = success" assumption once in this project (see `scripts/fetch-mx.sh`'s own workarounds).

**`mx analyze-mpr` is version-agnostic** — it read an 11.10.0-authored file correctly using an 11.13.0 binary. That's what resolves the chicken-and-egg of "need the version to pick a binary, need a binary to read the version."

**Don't parse `.mxunit` files directly.** A diff `id` also maps to `mprcontents/<id[0:2]>/<id[2:4]>/<id>.mxunit`, and that file has plaintext-visible `Name` fragments — but it's BSON, and Mendix's specific application of it is undocumented and proprietary. `dump-mpr` reads the identical object graph through a supported JSON export. Same instinct that drove inspecting the `mxbuild` tarball before writing trim rules rather than guessing.

---

## 3. Two named failure modes to handle explicitly

Both were hit for real, and both surface as confusing parser exceptions if you don't catch them first.

**Version-migration commits.** A commit that captures a Studio Pro version upgrade in progress contains a `Projects$ProjectConversion` unit and fails to parse with `Expected '$ID' as the first property of a storage object, but got 'Associations'.` This looks exactly like an `mx` bug and isn't. Detect via `analyze-mpr` and return `unsupportedVersionMigrationCommit` up front. (Manual trap #16.)

**`.mpr` internal self-reference mismatch.** The test app is git-tracked as `MERA.mpr` but internally refers to `App.mpr`; `mx` refuses to open it under the "wrong" name with `existing MPR contents refer to MPR file '<X>'`. Glob for the real filename, then parse that error and retry against a renamed copy. Treat a persistent mismatch as an explicit failure mode — the copy is a workaround, not a fix. (Manual trap #15.)

---

## 4. Steps 1–6 — built

Steps 1–5 are in `internal/mx`, Step 6 in `internal/gitops`. All green under `go test ./...` and `go test -race ./...`, `gofmt`/`go vet` clean.

**Step 1 — `mx.go` + `version.go`.** Shared subprocess runner mirroring `internal/mxcli`'s `run()`, but deliberately divergent in two ways: it takes a `Binary` and execs `bin.Path` (never a bare `mx` on `PATH`), and it returns the raw exit code rather than folding non-zero into an error, because each subcommand's codes mean different things. No semaphore — `mx` is called a handful of times per request, not once per unit. `Binary{Version, Path}`, `Resolve` (exact match only, fail loudly per manual §1.3), `Highest` (numeric `a.b.c.d` comparison, not a string sort). `MERA_MX_ROOT` defaults to `/opt/mx`, threaded through `api.NewServer(workRoot, mxRoot)` — plumbed but not yet consumed, that's Step 7's job.

> `Binary.Path` is `<version>/modeler/mx`, **not** `<version>/mx`. The trimmed tree mirrors the tarball's `modeler/`+`runtime/` layout, so the binary sits one level deeper than manual §1.3's simplified diagram shows. Confirmed against the real Docker image. Locally, `MERA_MX_ROOT` must be an *absolute* path to the directory containing the version directories — `ls -d "$MERA_MX_ROOT"/*/modeler/mx` is the one-line check.

Covered by `TestResolve_*`, `TestHighest_*`, `TestCompareVersions`, `TestParseVersion*`, and `FuzzCompareVersions` (30s, ~2.9M execs, 0 failures). Version ordering rule agreed with Jord: major/minor/patch numeric, the fourth `d` field a plain string, absent `d` ranking below a present `d` at the same `a.b.c`.

**Step 2 — `analyze.go`.** `AnalyzeResult{MendixVersion, HasProjectConversion, Raw}`, run against `Highest()`'s binary before the version-matched one is known. Parses `"Mendix version: X"` and scans for `Projects$ProjectConversion`. `ErrUnsupportedVersionMigrationCommit` exists and is ready for Step 7 to return.

**Step 3 — `prepare.go`.** `PrepareMpr(ctx, bin, dir)` calls `gitops.FindMpr` (already exported), then `Analyze`; on a self-reference mismatch it copies the `.mpr` to the demanded name and retries once. `parseSelfReferenceMismatch` matches the confirmed real error text. Run once per worktree — version detection, migration detection, and the filename fix all fall out of one call per side.

**Step 4 — `diff.go`.** `UnitDifference` grew one field beyond the plan: `Raw json.RawMessage`, capturing the complete per-unit object as produced (including `changeDetails`, a real field on `Modified` units). That's what feeds `ChangeUnit.StructuralDelta`. Full exit-code switch per §2's table. `stripBOM` was discovered here.

Real captured output is committed as `internal/mx/testdata/diff-image-and-microflow.json` and round-tripped through `Diff()` by `TestDiff_RealCapturedFixture`, checking both units (the `Images$ImageCollection` Modified unit and the `Microflows$Microflow` Added unit). The parsing contract — the actual open risk — is covered; a live `Diff()` call against live worktrees in Docker is not yet done.

**Step 5 — `resolve.go`.** `ResolveQualifiedNames` makes one `dump-mpr` call with comma-joined `--unit-type`, plus `Projects$Module` auto-appended by `ensureModuleType` to guarantee a base case for the fallback below. Decodes generically into `any` and walks recursively rather than modeling `dump-mpr`'s undocumented nesting schema.

The nameless-unit gap from §2 is handled by a **two-phase resolve**: `indexByID` builds an id→node index over the whole tree, then `resolveFromTree` resolves each wanted id either natively or via `synthesizeQualifiedName`, which walks the `$ContainerID` chain up to the nearest ancestor that *does* have a `$QualifiedName` (normally the module) and composes a segment per hop from `name`, else `$ContainerProperty` (set on essentially every node — this is what made `ReviewManagement.domainModel`, `.moduleSettings`, `.moduleSecurity` resolvable), else `$Type` as a last resort. Synthesized results carry `QualifiedNameSynthesized: true` so a caller can render them as inferred. Depth/cycle-guarded at 32 hops — a synthetic cyclic-chain test confirms it degrades by omitting the id rather than hanging.

Call it once per side: `head.mpr` for `change != "Deleted"`, `base.mpr` for `change != "Added"`. This handles a same-commit rename for free — the unit resolves to its old name in base and its new one in head, each valid in its own snapshot.

`resolve.go` is at 100% statement coverage. Fixture `internal/mx/testdata/dump-reviewmanagement-trim.json` (real, trimmed to one item per type) drives `TestResolveFromTree_RealCapturedFixture`, asserting 13 real IDs — 6 requiring synthesis, 7 resolving natively — plus 11 synthetic-tree tests covering fallback ordering, multi-level and cyclic chains, missing ancestors, and non-string `$ID`/`$QualifiedName` values.

> The fixture is **not exhaustive of every Mendix unit type** — no published REST service is present in it, among others. Treat gaps as untested, not as proven-absent.

**Step 6 — `gitops.CloneBoth`.**

- [x] `CloneBothRequest{RepoURL, Username, Pat, BaseSha, HeadSha}`, `CloneBothResult{WorkDir, BaseDir, HeadDir}`.
- [x] `CloneBoth` — same remote-add / credential-helper dance as `Clone`, then `fetch --filter=blob:none --no-tags origin <baseSha> <headSha>` in **one** call.
- [x] `git worktree add --detach <workDir>/base <baseSha>` and `.../head <headSha>`.
- [x] Confirmed `Cleanup(workDir)` needs no change, and added a test that says so.
- [x] Integration run against the real repo — both worktrees check out cleanly and each holds a non-empty `.mpr` plus `mprcontents/`.

Landed in `internal/gitops/clone_both.go`, with `clone_both_test.go` (hermetic, `file://` fixture repo) and `clone_both_integration_test.go` (env-guarded, real Team Server). Design reasoning and the gotchas live in `MERA-extractor-design-notes.md` §5; the four that matter most here:

1. **The repo is bare** — `workDir/repo.git` plus `base/` and `head/` worktrees. The plan implied worktrees nested under a normal checkout; bare removes an empty main working tree nothing should ever read, and sidesteps `worktree add` against an unborn `HEAD`.
2. **The credential file must outlive the fetch.** Under `--filter=blob:none` it is the *checkout* that pulls file contents, so both `worktree add` calls hit the network authenticated. Confirmed empirically that `git fetch --filter` configures the promisor remote by itself (`remote.origin.promisor = true`, `partialclonefilter = blob:none`) — no explicit `extensions.partialClone` step needed.
3. **A local fixture repo needs `uploadpack.allowAnySHA1InWant` and `uploadpack.allowFilter`.** Without the first, fetching by raw SHA is refused; without the second, `--filter` is silently ignored and the test greens while exercising a full fetch. Team Server allows both, which is what `Clone` has depended on since Stage 5.
4. **SHA validation was retrofitted to `Clone` too.** An unvalidated SHA beginning with `-` is read by git as a flag (`--upload-pack=...`); that hole existed from Stage 5 and is now closed in both functions.

> Also note: `git worktree add` needs a real git repository. A loose `.mpr` sitting in a directory is not enough — a real clone is a hard prerequisite, not an optimization.

---

## 5. Steps 7–12 — remaining

### Step 7 — rewire the REST contract in `internal/api/extract.go`

- [ ] Replace `Sha` with `BaseSha`/`HeadSha`.
- [ ] Keep `Units`/`Modules` as an explicit escape hatch — when set, skip the diff path and keep naive enumeration unchanged.
- [ ] Wire the default path:
  ```
  gitops.CloneBoth
    → mx.Highest + mx.PrepareMpr (head, and separately base)
    → mx.Resolve(detected mendixVersion)
    → mx.Diff(base.mpr, head.mpr)
    → mx.ResolveQualifiedNames (head for non-Deleted, base for non-Added)
    → assemble ChangeUnit{Module, UnitType, QualifiedName, ChangeKind, StructuralDelta}
    → targeted describe (Step 8)
  ```
- [ ] Return `ErrUnsupportedVersionMigrationCommit` immediately when `PrepareMpr` reports `HasProjectConversion` — closes the Step 2 bullet that was blocked on this step.
- [ ] Add response fields: `mendixVersion`, `storageFormat`, `mxcliVersion`, `mxVersion`, `changeUnits[]`, `textDiffs[]`, `warnings[]`.
- [ ] Leave `referenceGraph` out deliberately (manual §1.6 has it as a later phase) — don't let it block this stage.

> **Take the `.mpr` path from `PrepareMpr`'s return, not from your own `FindMpr` call.** The two can legitimately differ, because the self-reference workaround copies the file to the name `mx` demands. `CloneBoth` deliberately returns only directories for this reason — its own `FindMpr` calls are fast-fail validation, nothing more.

**Verify:** `POST /extract` with `BaseSha`/`HeadSha` against the real repo runs end to end; the `Units`/`Modules` escape hatch still returns the old response unchanged; response matches the frozen contract's field list.

### Step 8 — targeted describe, before/after

- [ ] `describeChangeUnits`, sibling to `describeAll` — same one-goroutine-per-unit pattern on `internal/mxcli`'s existing global semaphore, but up to two `Describe` calls per unit.
- [ ] `changeKind != "Added"` → describe from `base.mpr` using the **base-side** qualified name → `BeforeMdl`.
- [ ] `changeKind != "Deleted"` → describe from `head.mpr` using the **head-side** qualified name → `AfterMdl`.
- [ ] Describe failures degrade to a warning, never a request failure.

**Verify:** unit test that a forced failure yields a warning and the request still completes; integration test covering all four cases — Added, Modified, Deleted, and a same-commit rename where base and head names correctly differ.

### Step 9 — token estimate

- [ ] `tokenEstimate = len(beforeMdl + afterMdl + structuralDeltaJSON) / 3.6`.

Explicitly manual §1.5's placeholder constant — "measure on your own corpus later." It only needs to be right enough for batching decisions; the API returns exact counts anyway.

**Verify:** unit-test the arithmetic; order-of-magnitude sanity check against a couple of real payloads.

### Step 10 — `textDiffs` (optional — don't let it block 7–9)

- [ ] `git diff --unified=5 base head -- javasource theme deployment '*.json'` against the two worktrees, parsed into the `textDiffs[]` shape.

Cleanly separable from the `mx diff` path. Land it after 7–9 work end to end.

**Verify:** integration test against a commit pair with `javasource`/`theme` changes, compared directly to `git diff` output.

### Step 11 — testing

- [ ] Extend the stub-`mx` harness (`writeStubMx` / `writeDiffStubMx` / `writeDumpMprStubMx` already exist in `internal/mx`'s tests) to cover the Step 7–8 paths.
- [ ] **Guarded integration test** against the real repo, using the two already-validated commit pairs: microflow-only add, and image-add-plus-microflow.

> The guard pattern is established — copy `internal/gitops/clone_both_integration_test.go`: skip unless the `MERA_IT_*` env vars and `MERA_PAT` are all set, so `go test ./...` stays hermetic and offline. Nothing in Go reads `.env`; use `set -a; source .env; set +a` (and `KEY=value` with no spaces, or the assignment silently does nothing).

**Verify:** the integration `/extract` response shows real `changeUnits[]` with correctly resolved `qualifiedName`s and real `beforeMdl`/`afterMdl` — not naive full-app enumeration — for both validated pairs.

### Step 12 — document it

- [ ] Fold the remaining implementation notes into `MERA-extractor-design-notes.md` once Steps 7–11 land — code-level *decisions and gotchas*, not source listings (the repo holds the code). Step 6 is already written up there, in §5.
- [ ] Update `MERA-session-status.md` to reflect the new state.
- [ ] Keep the Step 0 id-correlation story as a callout, in the same "what we thought, what testing found wrong, what changed" style as the Stage 6→7 concurrency write-up.

---

## 6. Outstanding integration runs

Step 6's integration test is green against the real repo. These `mx`-side runs still need the real repo and a real Team Server connection, and none have been done:

- `PrepareMpr` against the commit known to hit the `MERA.mpr`/`App.mpr` mismatch — confirm it self-heals.
- `ResolveQualifiedNames` against Step 4's real diff output — confirm names match what was seen by hand (`MxCliExtractor.ACT_Test_newMicroflow`).
- A live `Diff()` call against live base/head worktrees inside the container, rather than the committed fixture.

All three are now cheaply reachable: `CloneBoth` produces exactly the base/head worktree pair they need, so each can be written as a guarded integration test that calls it first.