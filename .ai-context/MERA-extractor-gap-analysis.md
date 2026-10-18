# MERA Extractor — Gap Analysis

**Subject:** the extractor sidecar as it stands, measured against `MERA-redesign-architecture.md` **revision 5** and `MERA-implementation-manual.md` §1.
Date: 2026-08-30 · Status: analysis, no code changed · Author: Jord + Claude

> **How to read this.** §2–§5 are gaps: the architecture asks for something the extractor does not do. §6 is the other half — things the extractor *does* that no design document describes. Divergence in §6 is not automatically wrong; several items there are better than what the docs specify. But an undocumented good decision decays into an unexplained one, and this project has already paid for that twice (see `MERA-stage8.md` §3).
>
> Every `file:line` was checked against the working tree at commit `6818e2b`. Every § citation was read, not recalled. Findings the docs **already** list as known-deferred are labelled `[known]` so this report is not re-reporting `MERA-stage8.md` §6 as news.

---

## 1. Verdict up front

**The Stage 8 pipeline is sound and was proven properly.** Clone two commits → version-match a binary → `mx diff` → resolve GUIDs → targeted describe → text diffs is the right shape, it works against the real Team Server repo, and the degradation discipline (`§1.4`'s per-unit failure model) is genuinely applied throughout rather than gestured at. Nothing in this report argues for redesigning it.

Three things, though:

| | |
|---|---|
| **§2 — Three guarantees the design believes it has, and does not.** | A concurrency cap that is never taken, no authentication of any kind, and Decision 2 (MPRv2 only) unenforced. The first is a latent production incident, the second is the gap between §4's honest-risk promise and the running service. |
| **§3 — Roughly half of §7 has no extractor-side foundation.** | `neighbourhood` — the Tier 0 data §7.3 describes as "already in ChangeUnit" — does not exist, and `internal/mxcli` has no `refs`/`callers` wrapper to build it from. Workspace leases (§1.8, Tier 2) do not exist, which §5.3 made more serious by removing the Mendix-side reaper. |
| **§6 — The design record has drifted from the code in both directions.** | The extractor's largest piece of real engineering (the two-vocabulary type map) appears in no design document. Two mitigations the architecture names on record (`mx diff -l`, sparse-checkout) were quietly replaced or skipped. |

The single most valuable thing on this list is not the biggest: **finding #4, `neighbourhood`**, is a day's work, needs no new endpoint and no new state, and is what makes deleted units reviewable at all.

---

## 2. Broken guarantees

These three are not "not built yet". They are things a reader of the code or the docs would reasonably believe already work.

### 2.1 The mxcli concurrency cap does not exist ⛔

**`internal/mxcli/mxcli.go:54` — `run()` never acquires `sem`.**

The semaphore is declared (`mxcli.go:28`) and reallocated by `SetMaxConcurrent` (`mxcli.go:51`), and nothing ever sends to it. `git log -S"sem <- struct{}{}"` over that file returns nothing: **the acquire was never written.** Consequences, in order of how much they cost:

- `MERA_MXCLI_CONCURRENCY: "8"` (`docker-compose.yml:41`) is decorative. So is `SetMaxConcurrent` in `main.go`.
- `describeChangeUnits` (`internal/api/extract.go:661`) fires **one goroutine per changed unit, unbounded**, each performing up to two `Describe` calls — and does so *deliberately*, on the written premise that the cap lives downstream: *"both block on internal/mxcli's global semaphore, so no limit is applied here"* (`extract.go:656`). The one place that knew a limit was needed delegated to a limit that isn't there.
- `describeAll` on the legacy path (`extract.go:750`) has the same shape.
- Design notes §3 states two further requirements — acquire the slot **before** starting the per-call timeout, and bound queue wait via `select` on `ctx.Done()` — that also do not exist, being properties of the missing acquire. `run()` starts its 60s timeout immediately, which is the correct behaviour *given* no queueing, and the wrong behaviour the moment queueing is added.
- `MERA-session-status.md` §3's open item 2 ("`/health` queues behind the semaphore") is false today. `/health` cannot queue behind a semaphore nobody takes.

**Why the test did not catch it.** `concurrency_test.go:16`'s `acquireForTest` takes the slot itself:

```go
func acquireForTest(work func()) {
    sem <- struct{}{}          // ← the test does the acquiring
    defer func() { <-sem }()
    work()
}
```

It exercises a *faithful copy* of the intended pattern and proves the channel works. Its own comment then asserts the untested half — *"a single shared semaphore, gated inside `mxcli.run()`"* (`concurrency_test.go:43`). Both statements are true in isolation; the join between them is the bug.

> **▶ Why this matters more than the headline number.** On the proving run this is invisible: two change units, four describes. The scale that matters is manual §1.5's own — *"On a change set of 400 units that's 800 spawns"* — which is a normal sprint commit range, and exactly the workload §5.4's batching exists for. 800 concurrent `mxcli` process spawns on a container sized `cpus: 8` is not slow, it is an OOM or a fork-bomb-shaped stall, and it will first appear on the largest customer app rather than the test app.
>
> ▶ And note the lesson is one this codebase already teaches itself, in `MERA-stage8.md` §3: *"a test that skips looks exactly like a test that passes."* The variant here: **a test that exercises a copy of the mechanism looks exactly like a test that exercises the mechanism.** The `acquireForTest` helper exists precisely because `run()` shells out and is hard to test — which is the moment to be most suspicious, not least.

**What a fix has to satisfy** (all four, or it is not the design that was written down):
1. acquire in `run()` itself, so every entry point — `Describe`, `ListUnits`, `Version` — is covered without each remembering;
2. acquire **before** `context.WithTimeout`, so the 60s budget is execution time, not queue time;
3. `select` the acquire against `ctx.Done()`, so a cancelled request stops queueing instead of waiting for a slot it will never use;
4. a test that calls the **real `run()`** against a stub binary on `PATH` (a shell script that sleeps), not a copy of the acquire pattern.

Point 4 is the load-bearing one. Without it the same class of bug returns silently.

### 2.2 No authentication on any endpoint ⛔

`Routes()` (`internal/api/server.go:26–32`) registers four handlers and no middleware. There is no `Authorization` check, no bearer token, no mTLS, no allowlist — `grep -rn "Authorization\|Bearer\|TLS" --include=*.go .` returns nothing.

What the design specifies, in three separate places:

- **manual §1.4**, in the frozen contract's first line: `Authorization: Bearer <short-lived token minted by Mendix>`
- **architecture §3.1**, on the arrow itself: `HTTPS+mTLS`
- **architecture §4**, credential table, *In transit*: *"PAT travels Mendix → Extractor only. mTLS or signed short-lived request token; never as a URL parameter"*

Two endpoints make an unauthenticated service worse than merely open:

| Endpoint | Primitive it hands an unauthenticated caller |
|---|---|
| `POST /describe` (`internal/api/describe.go:12`) | Takes a caller-supplied **server-side `mprPath`**. Reads any file the process can reach, through `mxcli`. |
| `POST /clone` (`internal/api/clone.go:12`) | Takes a caller-supplied **remote URL and credential**. Fetches from any host the container can reach, and writes the supplied secret to a file on disk. |

`POST /extract` is less sharp — it needs a valid repo and a valid PAT — but it is still an unauthenticated way to make the box clone a multi-gigabyte repository, with no size cap (§5.3 below).

Today this is mitigated only by the deployment being a laptop. Note the asymmetry that shows the awareness exists: `docker-compose.debug.yml:36` binds delve to `127.0.0.1` *because* a delve client can execute arbitrary code — while `docker-compose.yml:25` publishes the app itself as `"8081:8080"`, all interfaces.

**Decision needed: token middleware now, or after the Mendix side exists?**
This report argues **now**, for three reasons. It is small (one `http.Handler` wrapper, a constant-time compare, an env var). It is the cheapest moment — `JA_RequestExtraction` does not exist yet, so there is no caller to migrate. And §4 ends with an explicit promise to users about what happens to their credential; shipping the sidecar without the transport control that promise depends on makes the Consent text inaccurate rather than merely incomplete.

A minimal first step that closes the whole surface: a shared bearer token from `MERA_AUTH_TOKEN`, rejected with `401` when absent or mismatched, exempting only `GET /health`. mTLS is the §3.1 target and can follow at deployment time; a bearer token over TLS terminated at the ingress is a legitimate stopping point and is what §14.6 already settled for the BP app's own service identity.

### 2.3 Decision 2 (MPRv2 only) is not enforced

Architecture §2 is explicit: *"Enforce it — read the storage format at the start of extraction and fail with a clear 'MERA requires MPRv2; upgrade the app's storage format' rather than half-working."* Manual §1.5 step 6 names the failure: `unsupportedStorageFormat`.

`extract.go:415` emits `StorageFormat: ""` and nothing anywhere checks the format.

Two things worth separating, because one was a good call and one was an omission:

- **Emitting `""` rather than fabricating `"MPRv2"` was correct.** The field's entire purpose is provenance; inventing a value in it would be worse than leaving it blank. The code comment says exactly this and it holds. `[known]` — `MERA-stage8.md` §6 item 5.
- **The enforcement is a separate obligation, and it was never discharged.** Not having a source for the field is not a reason not to have the check. Right now an MPRv1 app proceeds into `mx diff` and fails somewhere downstream with a message about something else — the precise "half-working" §2 rules out.

**Why the source went missing** is traceable, and belongs in the record: `PrepareMpr` moved from `analyze-mpr` to `show-version` because `analyze-mpr` aborts the process with a stack overflow on some real models (see §6.2). `show-version` reads only the `.mpr`'s SQLite metadata — no unit inventory, no storage format. The version read survived the substitution; the format read did not, and nobody noticed because nothing consumed it.

**Cheapest honest probe:** MPRv2 stores unit contents in `mprcontents/`; MPRv1 does not. That is a `os.Stat` on the worktree — no tool, no subprocess, no fabrication, and it is literally the thing §2's "read the storage format" is asking for. Two secondary candidates worth a look before settling: `mx diff`'s output has top-level `base`/`mine` objects that `diffFile` (`internal/mx/diff.go:101`) currently discards — check whether either carries a format field; and `mx show-version --help` may expose more than the one line it prints.

> ▶ Note the ordering constraint: the check must run **before** `mx diff`, because the point is a clear refusal rather than a confusing failure. That puts it in `extractDiff` right after `PrepareMpr`, alongside the existing version-mismatch warning.

---

## 3. Missing capability — §7 has no foundation in the sidecar

§7 is the section the architecture spends the most words on, and it is the one Decision 3 (advisory) was taken partly to unlock. Its Tier 0 and Tier 2 both need extractor-side work that has not started.

### 3.1 `neighbourhood` is absent from `ChangeUnit` — the largest functional gap

Three documents describe this as part of extraction, not as a later phase:

- **§2's `ChangeUnit`** lists `neighbourhood <mxcli refs/callers depth 1 — see §7 Tier 0>` as a field of the contract.
- **§7.3, Tier 0** — *"already in `ChangeUnit`"* — *"At extraction time, for every changed unit, run `mxcli refs` and `mxcli callers` at depth 1 and inline the result into the ChangeUnit. Free, deterministic, always present, no tool call needed. This alone answers 'who uses this?' for most findings. For `Deleted` units, run these against the **base** model."*
- **manual §1.5 step 9** spells out the exact calls, including the base-side rule.
- **manual §1.4**'s response example carries `neighbourhood: {callers: [...], references: [...]}`.

The code: `changeUnit` (`extract.go:47–66`) has no such field, and `internal/mxcli` has no `refs` or `callers` wrapper at all — the package is `describe.go`, `list.go`, `version.go` and the shared `run()`.

**Why this is the highest value-per-hour item in the report:**

- §7.4 gives Tier 0 to **both** L1 and L3 — two of the three review layers.
- §7.1's sharpest case is deletions: *"if a unit was deleted, every reference to it is now broken, and none of that appears in the diff for the deleted unit."* Without `neighbourhood`, a deleted microflow arrives at A3 as a `beforeMdl` and nothing else, and A3's prompt instructs it to fall back to "Cannot determine" — the exact outcome §7 exists to eliminate.
- It needs **no new endpoint, no new state, no lease, no graph**. It is two more `mxcli` invocations per unit inside the loop that already exists, with a base/head selection rule of one line.
- It is the cheapest thing that makes the §7 tool budgets (§7.5) meaningful, because Tier 0 is what the agents fall back to when the budget is exhausted.

Two design points to settle before building it, both of which the docs leave open:

1. **Truncation.** §7.5 caps caller lists at ~40 with `"and 312 more in 18 modules"`. That rule is written for the *tool* results, but Tier 0 is inlined into every ChangeUnit and therefore into every batch's input tokens — it needs the cap at least as much. A unit called from 300 places would otherwise blow §5.4's 40–80k per-batch target on its own.
2. **Cost per unit.** This triples the subprocess count per change unit (describe ×2 → describe ×2 + refs + callers). Which makes finding #2.1 a **prerequisite**, not a parallel task: adding Tier 0 on top of an unbounded goroutine fan-out multiplies the existing hazard.

⚠ **Unverified:** whether `mxcli refs` and `mxcli callers` exist with those names and accept `-p <mpr> <qualifiedName>` in v0.19.0, and what their output shape is. §2 of the architecture and manual §1.5 both assert them, but no output has been captured in this project. Confirm against `mxcli --help` before designing the parse — the `describe`-singular/`show`-plural inconsistency (design notes §4) is a standing warning that this tool's surface is not guessable.

### 3.2 Workspace leases (§1.8 / §7 Tier 2) do not exist — and §5.3 raised the stakes

None of `POST /workspaces`, `POST /workspaces/{id}/describe`, `POST /workspaces/{id}/search`, `DELETE /workspaces/{id}` exists.

This was a defensible "later phase" under revision 2. Revision 5 changed the risk profile without changing the code:

> **§5.3:** *"Revision 2 said 'TTL on creation; reaper in the sidecar and a Mendix scheduled event; never rely on only one.' On a free node you now rely on exactly one — the sidecar's reaper. It is ours and it is hosted outside Mendix, so this is acceptable, but it means the sidecar reaper needs to be correct rather than a backstop. **Give it a test.**"*

⚠ **Name collision worth stating loudly: `internal/workspace` is not this.** It is a clone-directory janitor — real, well-designed, tested, and described in §6.1 below. It shares the word "workspace" with §1.8 and nothing else. It has:

| §1.8 / §7 Tier 2 needs | `internal/workspace` has |
|---|---|
| A lease identity (`workspaceId`) returned to a caller | Directory names from `os.MkdirTemp` |
| `expiresAt`, TTL bounded to the run | A TTL, but as an idle-reaper, not a lease deadline |
| Both base and head checkouts held open past the request | Nothing — `/extract` deletes its clone on return |
| Credential wiped at lease creation, queries pure-local afterwards | N/A |
| Per-workspace mxcli bound of 2–4 (§1.8) | N/A — and see finding #2.1 |
| The §4 per-user access constraint the `ReviewRun` carries | Nothing; there is no notion of a caller |

A reader of `MERA-session-status.md` §1 could reasonably conclude §1.8 shipped. It did not; what shipped is the disk-hygiene half of it, which is genuine progress toward it and should be described that way.

**Not urgent** — §7.3 says Tier 2 is opened lazily and *"most runs never open one"*, and Phase 4 sits after Phase 3 deliberately. But two things should happen before Phase 4 rather than during it: rename or namespace `internal/workspace` so the collision stops (`internal/scratch`, or keep the name and be explicit in its package doc that it is not §1.8), and record in the architecture doc that §5.3's single-enforcement ruling now points at a specific mechanism (§6.1).

### 3.3 `referenceGraph` (Tier 1) absent — and its decisive experiment is unrun `[known]`

The `extractResponse` (`extract.go:68`) has no `referenceGraph` field. This is legitimately Phase 4 and is listed as deferred in `MERA-stage8.md` §6 item 8 and design notes §6.

What is *not* on anyone's list: **manual §1.6's experiment has never been run.**

> **§1.6:** *"Approach B — catalog query. […] If the catalog exposes a references/dependencies table, one query gives you the entire edge set. **Try this first.** This is the difference between a 4-second graph build and a 4-minute one. […] Spend an hour with `mxcli` on a real repo before committing to either. […] Everything about the feasibility of §7 in the architecture doc depends on this being cheap, so find out early."*

The whole of §7 Tier 1 — the three microflow tools, the UI impact graph §7.3 calls "a genuine product feature", and §11's Free-App sizing risk row — rests on an answer nobody has looked up. It is an hour against the local clone at `~/mera/mera-repo`, it needs no code, and it can be done today. **This is the cheapest unrun de-risking in the project**, and it is cheaper than the `Forms$`/`Pages$` question already queued in `MERA-session-status.md` §2 because it needs no Team Server commit pair.

---

## 4. Contract divergence from manual §1.4

§1.4 says *"treat it as frozen. Both sides depend on it."* Only one side exists so far, which makes now the free moment to reconcile — every item below gets more expensive once `JA_RequestExtraction` is written.

### 4.1 Error envelope `[known]`

`respondError` (`internal/api/respond.go:33`) emits `{"error": "..."}`; §1.4 specifies `{requestId, error, detail, retryable}`. Listed as deliberately deferred in design notes §6 and `MERA-stage8.md` §6 item 6.

What has changed since that deferral: **`retryable` is now load-bearing.** §14.7 taught the Mendix side to classify failures as retryable-vs-terminal and made the distinction the difference between a 15-second backoff and a wasted four minutes on every run. The extractor is the *other* service MERA calls, and today it offers no such signal — the Mendix side would have to infer retryability from an HTTP status code plus free text. The extractor already knows the answer internally: a 502 from a clone is retryable, a 422 for an unsupported version is not, a 500 from a missing binary is terminal until someone fixes the image.

Related, and worth re-deciding rather than inheriting: `respondError` collapses **every** 5xx to `"internal error"`. That is correct hygiene for an internet-facing service — the reasoning in the code comment is sound about absolute paths and raw subprocess stderr. It is the wrong trade for a two-party private link between two services we own, especially with no `detail` field to carry the truth instead. The §1.4 envelope resolves this properly: a stable machine-readable `error` code, the full text in `detail`, `retryable` as a boolean. Consider gating verbose `detail` on the authenticated caller once §2.2 lands.

### 4.2 `options` absent → no caps exist at all

`extractRequest` (`extract.go:28`) has no `options` object. Each of its three members is a control the architecture asks for by name:

| §1.4 option | Status |
|---|---|
| `maxCloneMb` | **No implementation whatsoever.** §4's "Fetching efficiently" asks for a *"hard cap on clone size + wall-clock, with a clean failure state on the `ReviewRun`"*. A 4 GB Mendix repo is fetched until the disk fills or the timeout fires — and §11's risk table has no row for "the sidecar's disk filled". |
| `timeoutSeconds` | Hardcoded: `cloneBothTimeout = 300 * time.Second` (`clone_both.go:31`), which design notes §7 item 4 itself calls *"a guess"*. `Diff` gets 5 min, each `dump-mpr` 5 min, each describe 60s. |
| `includeReferenceGraph` | Nothing to gate yet — but manual §1.6's fallback plan *is* this flag: *"make `includeReferenceGraph` genuinely optional and only request it when the Planner flagged `impactAnalysisRequired`."* Add it when Tier 1 lands, not after. |

Worth stating plainly because it is easy to miss: **the de-facto wall-clock backstop today is the caller's own HTTP timeout**, and it works — `r.Context()` is threaded through `CloneBoth`, `PrepareMpr`, `Diff`, `ResolveQualifiedNames` and every `Describe`, so a Mendix-side client timeout genuinely cancels the subprocess tree. That is a real property and it should be *documented* rather than left as something a reader has to trace. It is not a substitute for `maxCloneMb`, which has no equivalent.

### 4.3 `auth`, `provider` `[known]`

Flat `username`/`pat` rather than `auth: {kind, username, secret}`; no `provider`. Deferred deliberately (design notes §6) — *"Changing it would break the current caller for no benefit yet."* Still true, with one note: §4 models `Repository.provider` *"so credential handling and diff strategy can vary later"*, and Azure DevOps and GitHub are named as day-one-shaped remotes. Fold `provider` in at the same moment as the auth-shape change, in one contract revision, rather than twice.

### 4.4 `unitType` speaks a different vocabulary than the contract

§1.4's example and §2's `ChangeUnit` both show `"unitType": "Microflow"`. The extractor emits the raw metamodel string: `"unitType": "Microflows$Microflow"` (`extract.go:49` documents this as intentional).

This is not cosmetic. `unitType` travels into:
- the **Planner's** manifest, which is names + types + change kinds only — types are a third of what A0 reasons over;
- **§8's Finding schema**, where `unitType` is a field on every finding a reviewer reads;
- **A2's** category matching and the BP app's `appliesTo` filter (§14.3), which is keyed on *unit types* as an organisation writes them.

An agent shown `Microflows$Microflow` will cope. A best-practice record that says `appliesTo: Microflow` will not match it without a translation somewhere, and "somewhere" is currently nowhere.

The extractor **already computes the normalised form and throws it away**: `changePlan.mxcliType` (`extract.go:584`) holds `microflow` and never reaches the response.

> **Decision needed.** Three options, and the report's recommendation is (b):
> **(a)** Normalise to the contract — title-case `mxcliType` (`microflow` → `Microflow`) and emit that. Loses provenance, and does nothing for the ~unmappable types where `mxcliType` is `""`.
> **(b)** Emit **both**: keep `unitType` as the metamodel string (it is the honest, exact thing `mx` said), and add `unitTypeName` carrying the normalised form, empty when unknown. Costs one field, keeps provenance, gives the Mendix side and the BP taxonomy something stable to key on, and matches how `previousQualifiedName` was already handled additively.
> **(c)** Amend §1.4 and §2 to specify the metamodel string, and make the Mendix side own the mapping.
> The thing not to do is leave the docs saying one thing and the wire saying another.

### 4.5 `Moved` is never produced

§2's `ChangeUnit.changeKind` and §8's `changeAnchor` both enumerate `Added | Modified | Deleted | Moved`. `mx diff` emits three; `UnitDifference.Change` (`internal/mx/diff.go:23`) carries them through verbatim, and nothing ever yields `Moved`.

The extractor *does* derive the underlying signal — `previousQualifiedName` (`extract.go:55`) is set when base and head resolved to different qualified names — and then does not act on it.

> **Decision needed**, and it should be taken **before** Phase 2, because §8.2 makes `changeAnchor` a **compiled grammar**: an enum value that can never occur is not harmful, but a schema edit later invalidates the grammar cache *and* the prompt cache (§5.4's "never edit a schema mid-run"). Either:
> - **derive it**: module prefix changed → `Moved`; name changed within a module → `Modified` with `previousQualifiedName` set. This is the reading that matches how a Mendix developer uses the word.
> - **or drop `Moved`** from §2 and §8 now, while it costs nothing.

### 4.6 `requestId` idempotency `[known, self-documented]`

`extract.go:29` — *"echoed back; idempotency is not implemented yet"*. §1.4 calls it an idempotency key. Extraction is pure, so a duplicate POST is correct-but-wasteful (~35s plus a full refetch). Either implement a short-lived result cache keyed on `(requestId)`, or strike the phrase "idempotency key" from §1.4 so the contract stops promising it. Low priority; listed for completeness.

---

## 5. Operational and hygiene gaps

### 5.1 `dump-mpr`'s output escapes the workspace

`internal/mx/resolve.go:82`:

```go
outPath := filepath.Join(os.TempDir(), fmt.Sprintf("mera-dump-%d.json", time.Now().UnixNano()))
defer os.Remove(outPath)
```

Three problems, in ascending order of seriousness:

1. It lands on the container filesystem, not the `mera-work` volume — different disk, different size budget, invisible to the volume's `docker compose down -v`.
2. It is **outside the janitor's reach entirely**: not under `Manager.Dir()`, and its name does not match `clone-*`, so neither the startup sweep nor the reap loop will ever see it. The `defer` is the *only* cleanup, and the case the whole `internal/workspace` package exists to handle — SIGKILL, OOM-kill, container stop between write and defer — is exactly the case that leaks it.
3. It is a **full JSON dump of the model's requested unit types**, derived from a credentialed clone, sitting outside the directory whose entire design story is "wipe it in a finally" (§4's *In memory* row). For a 1,037-unit app across a handful of types this is not small, and it is called **twice per `/extract`** (base and head).

**The fix is not a one-liner**, which is why it is worth flagging as a design item: `ResolveQualifiedNames` currently has no idea where the request's workspace is. It needs a directory parameter threaded from `extractDiff` (which holds `clone.WorkDir`) through `Deps.ResolveQualifiedNames` — a signature change across the seam, one call site, one fake in the tests. Same reasoning applies to anywhere else a temp file is minted; `Diff` already does the right thing (`filepath.Join(clone.WorkDir, "diff.json")`, `extract.go:365`), which is a good precedent to follow rather than invent.

### 5.2 No sparse-checkout — and a trap in adding one

> **Status 2026-10-18: implemented**, with a model-only sparse set rather than the list below plus a credentialed text-diff prefetch to defuse the trap. See design notes, "Sparse, model-only worktrees". The `MERA_CLONE_TIMEOUT` knob from §4.2 landed with it.

§4's "Fetching efficiently" lists four measures in order of impact. The extractor implements the first and the fourth partially:

| §4 measure | Status |
|---|---|
| `--filter=blob:none` partial clone | ✅ `clone_both.go:95` |
| sparse-checkout limited to `*.mpr`, `mprcontents/**`, `javasource/**`, `theme/**`, `deployment/**` | ❌ not implemented |
| warm cache volume keyed by AppID | ❌ not implemented (§5.3 below) |
| hard cap on clone size + wall-clock | ⚠️ wall-clock only, hardcoded (§4.2 above) |

Without sparse-checkout, the `blob:none` filter buys much less than it looks: `CloneBoth` then runs two `worktree add`s (`clone_both.go:99–100`), and **the checkout is what lazily fetches the blobs back** — a fact the code itself documents at `clone_both.go:77–79`. For a repo with a heavy `theme/`, committed binaries, or a large `resources/` tree, that is the dominant cost on every run, paid twice.

> ⚠ **The trap.** `TextDiffs`' correctness argument depends on the current behaviour: *"No credentials are passed: every object involved is already local, because both worktrees were checked out, which materialised every blob in both trees. […] if this ever did need a lazy fetch it would fail, since `CloneBoth` removes the credential file before returning"* (`textdiff.go:32–38`).
>
> So sparse-checkout and `TextDiffs` are coupled. A sparse set that does not cover `DefaultTextDiffPaths` (`javasource`, `theme`, `deployment`, `*.json`) breaks text diffs — and breaks them in the worst possible way: a lazy-fetch failure *after* the credential file is deleted, surfacing as a git authentication error on a code path that is nowhere near authentication. §4's own sparse list happens to include all four paths, so the correct set is already written down. Whoever implements this must keep the two lists in sync, and a test should assert it.

### 5.3 No warm cache volume

§4, third bullet: *"a warm cache volume keyed by AppID so repeat runs incrementally fetch."* Every `/extract` starts from `git init --bare`. 13s for the test app today; a real customer repo is the case this exists for. The `mera-work` named volume already exists in compose and would be the natural home. Not urgent, but it is an unimplemented §4 recommendation with no recorded decision to skip it — which is the pattern this report is trying to break.

### 5.4 `pushurl` guard absent

Manual §1.7 item 2: *"**Never push.** `git config remote.origin.pushurl /dev/null` after adding the remote. Belt and braces on top of a read-only PAT scope."* Architecture §4 caveat 2 is more emphatic: *"Studio Pro stamps Mendix-version metadata on commits; third-party commits can break Mendix Cloud deployment. **MERA must never push.** Configure the clone with no push remote and reject any write path in code review."*

Not done, in either `Clone` or `CloneBoth`. Live risk is near zero — nothing in the codebase invokes push. But this is a *named* control from a section about a documented way to break someone's production deploy, it is one entry in the existing `steps` slice, and its value is precisely that it survives a future author who does not read §4.

### 5.5 No log-scrubber test

§4's credential table, *Never log* row: *"Explicit redaction in the extractor's request logging; **add a log-scrubber test**."* Manual §1.7 repeats it: *"Write a test for this — it's the kind of thing that regresses silently."*

`gitops.redact` (`clone.go:180`) exists and is correctly applied at both places a PAT could surface — `Clone`'s step loop and `CloneBoth`'s `fail` helper. **No test asserts it.** `grep -rn "redact" internal/gitops/*_test.go` returns nothing.

To keep this proportionate: the actual exposure today is narrow. `mx` and `mxcli` never receive the PAT, so their stderr — which *does* reach `log.Printf` and 500 responses unredacted — cannot carry it. `TextDiffs`' `runGitOutput` (`textdiff.go:230`) does not redact, but it also runs with no credential file and no PAT argument, so there is nothing to leak. The gap is **the test**, not a live hole — and the test's job is to keep it that way when someone later adds a code path that does carry the secret.

A good test is small and adversarial: construct an error whose text embeds a known token, assert the token is absent and `***` is present; and — more valuable — a table over `Clone`/`CloneBoth` failure paths asserting the returned error never contains the PAT, using a deliberately-failing local fixture repo.

### 5.6 `GIT_ASKPASS` not set

Manual §1.7's snippet sets `GIT_ASKPASS=/bin/true` alongside `GIT_TERMINAL_PROMPT=0`; `runGit` (`clone.go:100`) sets only the latter. `GIT_TERMINAL_PROMPT=0` is the load-bearing half and is present, so this is belt-and-braces — noted for completeness because it is a divergence from a snippet the manual presents as the pattern to copy.

### 5.7 `mx diff -l` — a named mitigation, replaced without record

§11's risk table, *Mendix version skew*: *"Read model version from the manifest first; **`mx diff -l`**; maintain a Studio Pro version matrix in the image; fail loudly rather than diff wrongly."*

The `-l` / `--loose-version-check` flag is never passed (`internal/mx/diff.go:74`). The extractor substitutes a different mitigation, and arguably a better one: exact-match binary resolution with no substitution (`Resolve`, `version.go`), head's version selecting the binary, and a warning when base and head disagree (`extract.go:350`). That satisfies "maintain a version matrix" and "fail loudly" — it simply drops the `-l` half.

Two reasons this is worth recording rather than leaving implicit:

1. **The swap is nowhere on record.** §11 still names `-l`; a future reader reconciling doc against code finds a mitigation that isn't there and has no way to know whether that was a decision or an omission. (It reads as an omission — nothing in the design notes mentions the flag.)
2. **The case `-l` exists for is exactly the case the extractor now handles by warning-and-proceeding.** Base authored in 11.12, head in 11.13, both diffed by the 11.13 binary, no loose flag, untested. `MERA-session-status.md` §3's queued investigation — *"Run `mx diff` against the same `.mpr` with both binaries, with and without `-l` — note whether the 'wrong' binary refuses, silently misreads, or auto-converts"* — is precisely the missing evidence. **"Silently misreads" is the outcome that would make the current warning inadequate**, and nothing rules it out today.

### 5.8 Log files have no retention

`internal/logrotate` rotates on the calendar day and at a size cap, naming files `extractor-YYYY-MM-DD.log` with a `.N` iterator. It has no delete, prune, or max-age function — `grep -n "Remove\|prune\|retain\|MaxAge"` over the package returns nothing.

So `MERA_LOG_MAX_BYTES` bounds **one file**, not the directory. The directory is bind-mounted to `./logs` on the host (`docker-compose.yml`), meaning growth accrues to Jord's disk rather than a container that gets recreated. The commit that added this is titled "Bounding log files" — accurate per-file, not per-directory, and worth saying so.

> ▶ **Why this gets sharper soon.** §9's retention decision, revised by Decision 8: *"`rawRequest`/`rawResponse` are not persisted as `FileDocument`s on a free node. […] **Log them to the sidecar**, or persist only on failure, or keep only the last N."* The architecture is planning to make the extractor the place where raw LLM request/response payloads land — at 60–80k tokens per batch, ~20 batches per run — into a log directory with no retention policy, on a host bind mount. That needs a designed answer (max files, max total bytes, or age-based deletion) *before* the Mendix side starts shipping payloads there, not after.

### 5.9 No CI, no registry, all-interfaces bind

Manual §1.10's entire argument — *"you never manually build this for production. A CI pipeline builds it exactly once […] and pushes the result to a container registry"* — is unbuilt. There is no `.github/` directory. Two of its named payoffs are consequently unavailable: a Go/Dockerfile version drift caught as a PR failure rather than a production surprise, and per-commit image traceability ("which exact code produced the extractor that ran last Tuesday's review") — the same provenance discipline `mxcliVersion` exists for, one level up.

`docker-compose.yml:25` publishes `"8081:8080"` on all interfaces while `docker-compose.debug.yml:36` deliberately binds delve to `127.0.0.1`. Combined with §2.2, all-interfaces is the actual exposure today; `"127.0.0.1:8081:8080"` costs nothing and matches the posture the debug file already takes.

---

## 6. Implementation choices the architecture never described

The second half of the ask. These are not gaps against the design — they are places where the code decided something the design never mentions. Each needs one of: **promote** (write it into the design record), **re-decide**, or **leave and label**.

### 6.1 `internal/workspace` in full → **promote**

Per-instance directories keyed to `(hostname, pid)`; an `.owner` marker that gates whether the janitor may descend; heartbeat files refreshed while a request holds a workspace; a startup sweep of this instance's own dir; a background reap loop with a TTL. The package doc explains the three-layer failure model well, and it is properly tested.

None of this is in the architecture or the manual. Manual §1.8 mentions *"a goroutine ticking every 30s that kills expired workspaces"* as a component of leases — this is a broader and different mechanism: it reaps any clone dir, lease or not, and covers crash, OOM-kill, container restart and rolling deploy.

**It must be promoted into the design record**, for a specific reason: §5.3 now rests the *entire* system's workspace-lifetime enforcement on "the sidecar's reaper", says it *"needs to be correct rather than a backstop"*, and says *"give it a test"* — and a reader of §5.3 today has no way to know that a reaper exists, where it lives, or that it is tested. That is a load-bearing claim with an undocumented referent.

Two properties worth writing down at the same time, because they are non-obvious and someone will otherwise rediscover them:

- The `(hostname, pid)` key means **the startup sweep is a no-op after a container *recreate*** — a new container gets a new hostname and therefore a new instance dir, so its own sweep finds nothing. The previous container's leftovers are reclaimed by the TTL janitor instead, one sweep interval later. That is correct by design, but only if you know both layers exist.
- The janitor reaps `clone-*` dirs by mtime idle-time, which means a *lease* (§1.8, when built) cannot simply be a long-lived `clone-*` dir — it will be reaped at TTL regardless of its own `expiresAt` unless it heartbeats or the janitor learns about leases. Design the two together.

### 6.2 `mx show-version` replacing `analyze-mpr` → **promote the consequence**

Forced by a real, uncatchable crash: `analyze-mpr` recurses without bound in `MprStats` on some models and aborts the process (SIGABRT, exit 134). The substitution is correct and well-documented in `showversion.go`'s comment.

What is **not** recorded anywhere in the design docs is the consequence, and it is a real one:

- Manual §1.5 step 6 says *"read Mendix version **and storage format** from head"* as one action. After the substitution the version read survived and **the storage-format read did not** — `show-version` reads only SQLite metadata. That is the direct cause of finding §2.3 (Decision 2 unenforced).
- `AnalyzeResult.UnitTypeCounts` now always comes back empty, and `HasProjectConversion` is always `false` — so the warning branch at `extract.go:342` is dead code. The comment there says so honestly (*"currently inert […] kept wired so it lights up again"*), which is the right call; it just isn't visible from the docs.
- `Analyze` and its parser remain fully implemented and tested but unreachable from any request path.

**Promote:** one paragraph in the manual §1.5 step 6 area, and a note in §11's risk table, saying the version read is `show-version`, the format read is unimplemented, and why.

### 6.3 The `.mpr` self-reference copy is silent → **re-decide**

`PrepareMpr` (`internal/mx/prepare.go:82`) parses mx's *"existing MPR contents refer to MPR file 'X'"* error, copies the `.mpr` to `X`, and retries once. It works, and it is what got the test app (tracked as `MERA.mpr`, self-referring to `App.mpr`) working at all.

It emits **no warning to the response**. A reviewer reading the extraction output has no way to know the model file was substituted before being diffed and described.

Manual §1.5 step 6 anticipates this and asks for more: *"Copying it to match its internal self-reference is a workaround, not a fix; treat a persistent mismatch as an **explicit failure mode** if the copy approach doesn't hold up for other projects."*

Against the provenance discipline applied everywhere else in this codebase — `mxToolsetVersionForResponse` exists purely to flag a toolset/app mismatch; `storageFormat` is left empty rather than fabricated; `qualifiedNameSynthesized` marks an inferred name — a silent file substitution is the outlier. **Recommendation: emit a warning** (`"model file MERA.mpr was copied to App.mpr to satisfy its internal self-reference"`), one line, at the point of the copy. That also gives you the data to answer §1.5's "if the copy approach doesn't hold up" question, which currently nobody can answer because nobody can see when it fires.

### 6.4 The two-vocabulary type map → **promote; it sets a review-quality ceiling**

`diffTypeToMxcli` and `knownNotDescribable` (`extract.go:155–239`), the three-tier classification, and the one-warning-per-unknown-type self-correction mechanism, are collectively **the largest piece of genuine engineering in the extractor** — and §2 of the architecture covers the ground they occupy in a single clause ("`mxcli describe`/unit"). Design notes §6 documents it thoroughly; no *design* doc knows it exists.

Promote it, because it is not an implementation detail — **it determines which kinds of change can ever produce MDL**, which is the ceiling on review quality per change type, which is exactly the kind of fact §11's risk table is for.

⚠ **One consequence in particular needs to reach the agent layer.** `Projects$Module` is deliberately unmapped, and correctly so — `mxcli describe module` renders the module's entire contents and would reintroduce the full-app enumeration Stage 8 exists to eliminate. The consequence:

> **A module-level change produces a `changeUnit` with no MDL at all, and no warning** (it is in `knownNotDescribable`, so it is silent by design).

That is a real, designed review blind spot. It is the right engineering decision and the wrong thing to keep private: A4's `notReviewed` is specified to carry exactly this kind of edge (*"The reader must know the edges of the review to trust the middle"*), and it cannot report what it is never told. Whichever field carries this — a structured warning (§6.7), or a flag on the change unit — the decision should be made now while `warnings` is still malleable.

The same applies, more mildly, to the ~18 unmapped mxcli types and the four `UNVERIFIED` guesses: those *do* warn, so they self-correct as real commits hit them, which is the mechanism working as intended.

### 6.5 `previousQualifiedName` and `qualifiedNameSynthesized` → **promote into §1.4**

Both are additive fields not in the frozen contract, both `omitempty`, both documented in design notes §6 as deliberate. Both are more valuable than "additive extra" suggests:

- **`previousQualifiedName`** is the only rename signal in the system, and it is the input to the `Moved` decision (§4.5). It is also what the Planner needs for its own *"renames without behavioural change → skim"* rule.
- **`qualifiedNameSynthesized`** deserves more attention than it has had. A synthesized name is built by walking the containment chain (`resolve.go`), and it is generally **not a name `mxcli describe` will accept** — so the flag effectively *predicts* a describe failure for that unit. Nothing downstream is told to read it, and no consumer would know to. Either document it in §1.4 with that meaning attached, or have the extractor act on it (skip the describe and warn once, rather than warning per unit after a failed subprocess).

### 6.6 Head's version selects the binary for both sides → **promote**

`extract.go:347–354`: head's Mendix version resolves the binary; a base/head mismatch is a warning, not an error, because *"head is what's under review"*. Design notes §6 argues this well, including why comparing base and head versions is *"neither necessary nor sufficient"* as a migration gate.

Nowhere in the design docs. It interacts directly with §5.7 (`-l`) and belongs in the same paragraph.

### 6.7 `warnings` is a flat `[]string` → **re-decide, now, before the Mendix side exists**

This matches §1.4, so it is not a divergence — it is a place where the frozen contract is out of step with what revisions 3–5 made the rest of the system look like. §8 is built entirely on enums and required fields, precisely because prose fields cannot be reasoned over reliably.

Today the Mendix side would have to string-match to distinguish:

```
"unrecognised change unit type \"Foo$Bar\" — reported without MDL; add it to …"
"describe base Sales.Foo: mxcli [describe …]: <stderr>"
"base is Mendix 11.12.0 but head is 11.13.0; using the head binary for both"
"mx diff reported conflicts (exit 2); results are still usable"
"could not resolve base-side names: …"
```

Those are five different *kinds* of event with different consequences: one is a review blind spot A4 must report, one is a per-unit degradation, one is provenance, one is advisory, one is a whole-batch degradation. A4's `notReviewed` needs to tell them apart; so does any UI that wants to show "3 units could not be rendered" rather than a wall of strings.

**Recommendation:** structured warnings — `{code, unit?, unitType?, detail}` with `code` an enum — while keeping a rendered `detail` string so nothing gets less readable. This is a §1.4 revision and should ride along with the error-envelope change (§4.1), in one contract bump, **before** `JA_RequestExtraction` is written against `[]string`.

### 6.8 The escape hatch returns a second response *shape* → **leave, label, and plan its retirement**

`units`/`modules` bypass the diff path and return `legacyExtractResponse` (`extract.go:113`) — a structurally different body from the same endpoint. The reasoning in design notes §6 is good: a separate type means "the old response is unchanged" is guaranteed by the type system rather than by careful `omitempty` tagging.

The cost is that `POST /extract` is polymorphic: a consumer cannot know which shape it will receive without inspecting the body. That is a mild smell in general and a slightly worse one next to §8.2's grammar discipline on the Mendix side.

No action needed now — nothing in the architecture calls this path. But note that **§7 Tier 2's `describe_unit` is this feature done properly**: scoped to a lease, one unit at a time, with a `detail: summary|full` knob. When leases land, the escape hatch is a retirement candidate, and moving it to its own path (`POST /extract/units`) is the cheap interim if it earns its keep.

### 6.9 `godotenv` loads `.env` in the production image → **re-decide (small)**

`main.go:23` calls `godotenv.Load()` unconditionally. In the container there is no `.env`, so it logs and continues — harmless today. But `WORKDIR` is `/work`, and `/work` is a **mounted volume** (`mera-work`), so a `.env` appearing there would be read at startup and would silently override nothing-in-particular today but could override anything tomorrow. Design notes §2's *"configuration is injected, never hardcoded"* does not mention `.env` at all.

Cheapest fix: load `.env` only when a build tag or an env var says this is local development, or point `Load` at an explicit path outside the volume. Genuinely minor; listed because it is a config-injection surface on a writable mount, and those age badly.

### 6.10 The compose file misdescribes the service → **fix when §2.1 is fixed**

`docker-compose.yml` is the nearest thing this project has to a deployment specification, and two of its statements are not true:

- `mem_limit: 4g` sits directly under a comment reasoning that *"2G is comfortable for a ~1,000-unit app"*. One of the two is stale; the number and its justification should agree.
- `MERA_MXCLI_CONCURRENCY: "8"` carries a careful comment about `runtime.NumCPU()` overstating a container's quota — and is inert (§2.1). Once the semaphore is actually acquired this becomes true again; until then it reads as a control that exists.

---

## 7. Recommended order

Not a schedule — a dependency argument.

| | Item | Why here |
|---|---|---|
| 1 | **§2.1 semaphore** (+ a test against the real `run()`) | Prerequisite for §3.1, which triples per-unit subprocess count. Latent incident, not a feature. |
| 2 | **§2.2 bearer-token middleware** | Cheapest now — no caller exists to migrate. Gates anything running unattended. Pair with the `127.0.0.1` bind (§5.9). |
| 3 | **§3.3 the `mxcli` catalog-query hour** | Pure investigation, no code, needs no Team Server pair. Its answer shapes Tier 1's design; running it late means designing Tier 1 twice. |
| 4 | **§3.1 `neighbourhood` (Tier 0)** | Highest value per hour in the report. Makes deletions reviewable; prerequisite for tuning L1/L3 prompts against realistic input. Depends on (1). |
| 5 | **One "extraction hygiene" pass:** §2.3 MPRv2 check · §5.1 dump file into the workspace · §5.2 sparse-checkout (with the `TextDiffs` pathspec guard) · §5.4 `pushurl` · §5.5 the scrubber test | All small, all in the same two files, all cheaper together than separately. |
| 6 | **One contract revision:** §4.1 error envelope + §6.7 structured warnings + §4.2 `options` + §4.3 `auth`/`provider` + §4.4 `unitType` decision + §4.5 `Moved` decision | Bump §1.4 **once**. Every item here is nearly free before `JA_RequestExtraction` exists and expensive after. This is the deadline that actually matters. |
| 7 | **Fold §6 into the design docs** | `internal/workspace` into §5.3's single-enforcement claim; the type map and its `Projects$Module` blind spot into §2 and §11; the `show-version` consequence into §1.5 step 6; the `-l` substitution into §11. Do it in the same session as (6) so the docs and the wire change together. |
| 8 | §3.2 leases · §5.3 warm cache · §5.8 log retention · §5.9 CI | Phase 4 and deployment work. §5.8 needs an answer before §9's raw-payload logging starts, not before. |

---

## 8. What this report did not check

Stated so the edges are visible, per the house rule:

- **No code was run.** No `go test`, no `docker build`, no integration run. Every finding is from reading the working tree at `6818e2b` plus `git log -S`.
- **`mxcli refs` / `mxcli callers`** (§3.1) are asserted by the architecture and the manual; **no output has been captured in this project** and their existence in v0.19.0 is unverified here.
- **`mprcontents/` as an MPRv2 discriminator** (§2.3) is inferred from architecture §2's own description of the format, not confirmed against an MPRv1 app — this project has none on hand.
- **The 800-spawn figure** (§2.1) is manual §1.5's own arithmetic for a 400-unit change set, not a measurement. The unbounded fan-out is confirmed by reading; its behaviour at scale is not.
- **Test coverage claims** in `MERA-stage8.md` §5 (93.5% / 72.1%) were not re-measured.
- The **Mendix side** is entirely out of scope: this report measures the sidecar only.
