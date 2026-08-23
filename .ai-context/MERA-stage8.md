# Stage 8 — Real change detection with `mx diff`

**COMPLETE.** Steps 1–12 built, unit-tested and verified end to end against the real Mendix Team Server repo on 2026-08-23. `/extract` no longer enumerates the whole app: it clones two commits, diffs them with `mx diff`, resolves each changed unit's qualified name, and renders MDL for only what changed.

This doc is now the durable record of *how Stage 8 was built and what was learned proving it*. Code-level design reasoning lives in `MERA-extractor-design-notes.md` §5–§6; the repo holds the code.

**Read before touching this area:** `MERA-redesign-architecture.md` §2 (the `.mpr` problem and the `ChangeUnit` contract) and `MERA-implementation-manual.md` §1.3–§1.5 (version matrix, the frozen REST contract, the 14-step extraction algorithm).

**Package boundary:** `internal/mx` wraps the official `mx`/`mxbuild` binaries (version selection, `diff`, `dump-mpr`, `analyze-mpr`). `internal/mxcli` wraps the community `mxcli` tool (`describe`, `show`/`list`). Two different binaries, two packages, each named for the binary it fronts. Never fold one into the other.

---

## 1. The proving run

`go test ./... -count=1 -v -run Integration` with the real PAT and the image-add-plus-microflow commit pair:

```
mendixVersion="11.13.0" mxVersion="11.13.0"
2 change units
diff types seen (units / named / with MDL / synthesized name):
    Images$ImageCollection    1 / 1 / 1 / 0
    Microflows$Microflow      1 / 1 / 1 / 0
2/2 units named, 2 rendered MDL
expected unit MxCliExtractor.ACT_Test_newMicroflow: changeKind=Added mdl=231 bytes
0 text diffs
WARNING: a Projects$ProjectConversion unit is present (a record of a past
         Studio Pro upgrade); proceeding
--- PASS: TestExtract_Integration (35.37s)
--- PASS: TestExtract_IntegrationEscapeHatch (13.73s)   19 units from Administration, head worktree
--- PASS: TestCloneBoth_Integration (13.13s)            both worktrees, 151,552-byte .mpr each
```

What that confirms: two commits materialise as worktrees; version detection selects the matching binary; `mx diff` produces real unit differences; `dump-mpr` resolves both GUIDs to qualified names with no synthesis needed; targeted describe renders real MDL for both; the escape hatch still enumerates naively against head; and the `Projects$ProjectConversion` fix lets a healthy app through.

**Two change units, not 1,037.** That is the whole point of the stage.

### Still unproven after this run

- **`Forms$` vs `Pages$` is unresolved.** This commit pair contained no page, snippet, layout or building block, so the one question that discriminates between the two vocabularies went untested. See §4.
- **`textDiffs` returned 0, correctly** — the pair touches no `javasource`, `theme`, `deployment` or `*.json`. Step 10's code is fully unit-tested against a local fixture repo but has never produced a non-empty result from the real repo. Needs a commit pair with a Java change.
- **`mxcliVersion` comes back as `"mxcli version v0.19.0 (2026-08-21T13:13:26Z)\n"`** — prose prefix, build timestamp, trailing newline. Manual §1.4 wants a bare `"v0.19.0"`. Cosmetic, but it is a provenance field that something downstream will parse. Fix belongs in `mxcli.Version`, not at the call site.

---

## 2. Confirmed facts about `mx` / `dump-mpr` / `analyze-mpr`

Durable reference. Every item was confirmed against real output, not inferred from documentation.

**`mx diff` gives you a GUID, not a name.** Output shape is `{"base", "mine", "unitDifferences": [{type, id, change, containerId, containmentName}]}`. `mxcli describe` needs `Module.Name`.

**`mx dump-mpr` resolves it in one step.** Objects carry `$ID` alongside `$QualifiedName`, already in `Module.Name` shape; `Module` is everything before the first `.`.

**`--output-file` is a real flag; `dump-mpr` does NOT take a bare positional output path.** Omitting it dumps the entire JSON to stdout — that's the "huge wall of text, empty file" symptom.

**`--unit-type` and `--module-names` each accept a comma-separated list in one invocation.** One `dump-mpr` call per side, not one per type.

**Real top-level shape is `{"units": [...]}`** — not a bare array, not the raw document tree.

**Both `mx diff` and `dump-mpr` write output files with a UTF-8 BOM** (`ef bb bf` before the `{`), confirmed by hexdump. `encoding/json` refuses to parse that. `stripBOM` is applied in every read path — this is a .NET binary, so assume the BOM everywhere.

**Nested objects DO carry `$QualifiedName`.** `DomainModels$Attribute`, `DomainModels$Association`, `Enumerations$EnumerationValue`, `Pages$PageParameter`, `JavaActions$JavaActionParameter`, `Microflows$MicroflowParameterObject`, `Security$ModuleRole` all carry both fields at depth.

**Some top-level units carry `$ID` with NO `$QualifiedName`** — `Projects$Folder`, `DomainModels$DomainModel`, `Projects$ModuleSettings`, `Security$ModuleSecurity`. These are synthesized from the containment chain, not dropped.

**`mx analyze-mpr` prints a size report, not a status report.** Sections: `MPR File Analysis`, `BSON contents`, `Content categories`, `Size by unit type`, `Size by property`, `Size by unit`, `Size by module`. Each is preceded by a blank line and followed by a rule of dashes — the blank line is what a parser uses to close the previous section. `Size by property` rows are shaped identically to `Size by unit type` rows and sit directly beneath them.

**The two exit-code tables are different. Do not share a switch.**

| Code | `mx diff` | `mx dump-mpr` |
|---|---|---|
| 0 | OK | OK |
| 1 | — | wrong project file |
| 2 | conflicts found (**still usable**) | invalid unit type(s) |
| 3 | — | unknown JSON export error |
| 4 | unsupported `.mpr` version | project in a different Mendix version |
| 129 | generic diff error | — |

**Verify exit codes empirically anyway.** This CLI family exits non-zero for a valid `--help`, which already broke the "0 = success" assumption once.

**`mx analyze-mpr` is version-agnostic** — an 11.13.0 binary read an 11.10.0-authored file correctly. That resolves the chicken-and-egg of "need the version to pick a binary, need a binary to read the version."

**Don't parse `.mxunit` files directly.** A diff `id` maps to `mprcontents/<id[0:2]>/<id[2:4]>/<id>.mxunit`, which has plaintext-visible `Name` fragments — but it's BSON, and Mendix's application of it is undocumented and proprietary. `dump-mpr` reads the identical object graph through a supported JSON export.

---

## 3. Three named failure modes

**Version-migration commits — detected on failure, never predicted.** A commit capturing a Studio Pro upgrade in progress fails to parse with `Expected '$ID' as the first property of a storage object, but got 'Associations'.` Detect it by matching `mx.VersionMigrationSignature` against a *failed* diff's stderr.

> **This was implemented wrongly first, and the mistake is instructive.** The original code returned `unsupportedVersionMigrationCommit` whenever `analyze-mpr` mentioned `Projects$ProjectConversion` anywhere in its output. A real run rejected every commit of the test app with a 422. Two errors compounded: a `Projects$ProjectConversion` unit **persists in the model after an upgrade completes** (the test app carries exactly one and diffs perfectly), and manual trap #16 actually says *"if diffing **fails** on a project containing a `Projects$ProjectConversion` unit"* — the condition is the failure, the unit is only the explanation. Dropping the condition and keeping the explanation rejects every app that has ever been upgraded. Presence is now a warning.

**`.mpr` internal self-reference mismatch.** The test app is git-tracked as `MERA.mpr` but internally refers to `App.mpr`; `mx` refuses to open it under the "wrong" name with `existing MPR contents refer to MPR file '<X>'`. `mx.PrepareMpr` parses that error and retries against a renamed copy. Treat a persistent mismatch as an explicit failure — the copy is a workaround, not a fix. (Manual trap #15.)

**A test that skips looks exactly like a test that passes.** `go test ./...` prints `ok` in 0.006s for a package whose integration tests all skipped. Nothing in Go reads `.env`, so forgetting `set -a; source .env; set +a` gives a green, instant, hollow run. Both integration files now use `requireIntegrationEnv`: `MERA_IT=1` makes a missing variable a failure, a half-configured environment is always a failure, and only a completely unset one skips.

---

## 4. The type-mapping problem — still one open question

`mx diff` reports `Microflows$Microflow`; `mxcli describe` wants `microflow`. Nothing documents the mapping. Full write-up in `MERA-extractor-design-notes.md` §6; the unresolved part:

**Two vocabularies exist.**

```
mx dump-mpr    →  Pages$Page   Pages$Snippet
mx analyze-mpr →  Forms$Page   Forms$Snippet   Forms$Layout   Forms$BuildingBlock
```

`analyze-mpr` reports storage-level names, `dump-mpr` the current public metamodel names — Mendix evidently renamed the `Forms` namespace to `Pages` without changing what is on disk. **Which one `mx diff` speaks is still unknown.** The proving run's diff contained only `Microflows$Microflow` and `Images$ImageCollection`, which are spelled identically in both and therefore discriminate nothing.

Both spellings are mapped, so the code is correct either way. To settle it: run the integration test against a commit pair that touches a **page**, and read the type table. Then delete the dead half.

> An earlier revision of the design notes asserted the two tools shared one vocabulary, citing exactly those two non-discriminating types as evidence. Recorded here because the error is easy to repeat: overlapping observations are not confirming observations.

---

## 5. What each step landed

| Step | What | Where |
|---|---|---|
| 1 | `Binary`, `Resolve` (exact match, no fallback), `Highest` (numeric compare) | `internal/mx/mx.go`, `version.go` |
| 2 | `Analyze` — Mendix version + unit-type inventory from `analyze-mpr` | `internal/mx/analyze.go` |
| 3 | `PrepareMpr` — find the `.mpr`, analyse it, self-heal the filename mismatch | `internal/mx/prepare.go` |
| 4 | `Diff` — full exit-code switch, BOM stripping, `Raw` per unit | `internal/mx/diff.go` |
| 5 | `ResolveQualifiedNames` — one `dump-mpr` per side, two-phase resolve with containment-chain synthesis | `internal/mx/resolve.go` |
| 6 | `CloneBoth` — bare repo + two detached worktrees from one fetch | `internal/gitops/clone_both.go` |
| 7 | `handleExtract` rewired to `baseSha`/`headSha` → `changeUnits[]`, behind a `Deps` seam | `internal/api/extract.go`, `deps.go` |
| 8 | `describeChangeUnits` — targeted before/after describe, two names per unit | `internal/api/extract.go` |
| 9 | `tokenEstimate` — manual §1.5's `/3.6` placeholder | `internal/api/extract.go` |
| 10 | `TextDiffs` — per-file unified diffs for javasource/theme/deployment/json | `internal/gitops/textdiff.go` |
| 11 | Hermetic tests throughout plus three guarded integration tests | `*_test.go` |
| 12 | This doc, `MERA-extractor-design-notes.md` §5–§6, `MERA-session-status.md` | — |

Coverage: `internal/api` 93.5%, `internal/gitops` 72.1%, `internal/mx` well covered including a real captured `analyze-mpr` report and a real captured `mx diff` payload as fixtures. Everything green under `-race`.

---

## 6. Follow-ups this stage leaves behind

1. **Settle `Forms$` vs `Pages$`** — one integration run against a page-touching commit pair (§4).
2. **Prove `textDiffs` against the real repo** — one run against a `javasource`-touching pair (§1).
3. **Clean up `mxcliVersion`** — `mxcli.Version` should return `v0.19.0`, not a sentence with a newline (§1).
4. **Four `UNVERIFIED` entries and ~18 unmapped mxcli types** remain in `diffTypeToMxcli`. The warning mechanism names them as real commits hit them.
5. **`storageFormat` is emitted empty.** `AnalyzeResult` does not expose it and inventing `"MPRv2"` would fabricate provenance. `mx diff`'s own output has top-level `base`/`mine` fields that `DiffResult` currently discards — worth checking whether they carry it.
6. **The error envelope** — manual §1.4 specifies `{requestId, error, detail, retryable}`; `respondError` still emits `{"error": "..."}`.
7. **Request auth shape** — still flat `username`/`pat` rather than §1.4's `auth: {kind, username, secret}`, with `provider` and `options` absent.
8. **`referenceGraph`** — manual §1.6, deliberately a later phase.