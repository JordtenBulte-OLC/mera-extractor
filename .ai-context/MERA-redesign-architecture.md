# MERA Redesign — Architecture & Feasibility

**Mendix Expert Reviewing Application · multi-agent redesign**
Date: 2026-08-30 · Revision 5 · Status: design proposal · Author: Jord + Claude

> **Revision 2** folded in the six decisions from §13 and added §7, model context and impact analysis.
> **Revision 3** applied the agent-layer decision: GenAI Commons + MCP Client only, our own Anthropic connector, output guaranteed by structured outputs, everything costed against a Mendix Free App. New §3.2 dependency list; Decisions 7–10.
> **Revision 4** corrected §4 — credentials are per-user *data* by design, never constants, on any environment — and added **§14, the best-practices application**.
> **Revision 5** settles the BP app as a free node too: **§14.7** becomes a full readiness, wake-up and retry strategy, and **§5.3a** decomposes the pipeline into queues so the Layer 2 gate retries without holding an executor.

---

## 1. Verdict up front

The redesign is feasible, and the multi-agent shape you describe is the right shape. Four things need to be decided consciously, because they are where this design either holds up or quietly breaks:

| # | Question | Answer |
|---|---|---|
| 1 | Can Mendix fetch the repo itself with a user PAT? | **Yes.** Team Server exposes `https://git.api.mendix.com/<AppID>.git` with PAT auth. Use **JGit** as a managed Java dependency — no `git` binary needed. |
| 2 | Can the orchestration + agent loops live in Java actions? | **Yes.** The official `com.anthropic:anthropic-java` SDK is on Maven Central and installs via Studio Pro managed dependencies. There is **no Anthropic connector on the Marketplace**, so MERA supplies its own behind the GenAI Commons `DeployedModel` contract (§3.2, §5.2). |
| 3 | Can `mxcli` / `mx diff` run inside the Mendix runtime? | **No — not on Mendix Cloud.** These are native binaries. The CF buildpack ships JRE + Runtime + nginx only, has no package manager, and the container filesystem is ephemeral. This is the one hard constraint in the design. |
| 4 | Can this run on **Mendix Free Apps** — both MERA and the BP app? | **Yes, with named costs.** Task Queue runs (confirmed). Scheduled events do **not**. 0.5 GiB database, 1 GiB files, 1 GiB memory, 0.5 vCPU, single instance, sleeps after ~1h idle. Security is unaffected — entity access and XPath constraints are model-level (§4). The BP app sleeping is designed around in §14.7 and §5.3a rather than tolerated. |

**Consequence:** MERA becomes *one Mendix app + one extraction sidecar*, talking to a separately owned best-practices application (§14). Everything you asked for — orchestration, agent definitions, review logic, UI, audit — stays in Mendix. Only the "turn commits into readable model text, and answer model queries" service runs in a container that is allowed to have `git`, `mxcli` and `mx` on its PATH.

If you self-host the Mendix runtime in your own Docker image (not Mendix Cloud), you *can* bake the binaries in and `Runtime.exec` them from a Java action, collapsing to a single deployable. I'd still split it: the extractor is CPU/disk-heavy, needs a different scaling profile, and you do not want a 4 GB clone landing in your app container.

---

## 2. The constraint that shapes everything: the `.mpr` problem

This is worth stating explicitly because it is the reason MERA is hard and the reason a naive "feed the git diff to Claude" approach produces garbage.

A Mendix repository is **not reviewable as text**:

- The model lives in `App.mpr` — a **SQLite database of BSON-encoded model elements**.
- MPRv2 moves unit contents out to an `mprcontents/` folder as individual files — better granularity for git, but **still BSON**, still binary.
- `git diff` on a Mendix model therefore yields "Binary files differ". Useless to an LLM.

Two tools solve this, and they solve *different* problems. You need both.

**`mx diff` (bundled with Studio Pro / mxbuild)**
```
mx diff [-l] BASE.mpr MINE.mpr output.json
# exit 0 = ok · 2 = conflicts · 4 = unsupported version · 129 = error
```
Gives you the **structural delta**: which units were added, changed, deleted. This is your *change manifest*. It is not a review-ready rendering — it tells you *that* a microflow changed, not what the microflow now does in a form an expert can judge.

**`mxcli` (mendixlabs, Apache-2.0, alpha)**
Reads `.mpr` headlessly and renders the model in **MDL**, a SQL-like definition language, with `describe`, `search`, `callers`/`callees`, `refs`, `impact`, `context`, plus a built-in `lint` and a scored best-practices `report`. This is your **semantic rendering** — the "what does this microflow actually look like now" that the agents reason over. It's explicitly built for agentic tooling (the repo ships a `CLAUDE.md`), and §7 leans on it heavily.

> ⚠️ `mxcli` is self-described alpha with corruption warnings. MERA must use it **read-only, on a throwaway clone, never on a developer's working copy**. Pin the version. Have a fallback path (`mx diff` JSON alone, degraded review quality) when `mxcli` fails on an unsupported model version.

**MPRv2 only.** *(Decision 2)* Dropping MPRv1 removes a real chunk of complexity: no legacy SQLite-blob path, `mprcontents/`-based extraction everywhere, and a narrower `mx`-version matrix in the extractor image. Enforce it — read the storage format at the start of extraction and fail with a clear "MERA requires MPRv2; upgrade the app's storage format" rather than half-working.

**The derived design rule:** the unit of review is not "a diff hunk". It is a **Change Unit**:

```
ChangeUnit {
  module            "Sales"
  unitType          "Microflow"
  qualifiedName     "Sales.ACT_Order_Confirm"
  changeKind        Added | Modified | Deleted | Moved
  structuralDelta   <slice of mx diff JSON for this unit>
  afterMdl          <mxcli describe output, post-change>
  beforeMdl         <mxcli describe output, pre-change — omit for Added>
  neighbourhood     <mxcli refs/callers depth 1 — see §7 Tier 0>
  tokenEstimate     1_240
}
```

Everything downstream — batching, agent prompts, findings, UI — anchors on this.

**Text artifacts still diff normally.** `javasource/**.java`, `theme/**`, JS actions, `.json` config, `deployment/` — plain `git diff` is perfect for these and they should be fed as ordinary unified diffs. Roughly: *model → mxcli, code → git*. Don't over-engineer the second half.

---

## 3. Target topology

### 3.1 The deployables

```
┌──────────────────────────────────────────────────────────────────┐
│  MERA (Mendix app · Studio Pro 11.12+ · Free App node)           │
│                                                                  │
│  UI: repo/PAT registration · run wizard · live progress ·        │
│      finding triage · impact graph · report export               │
│                                                                  │
│  Domain: ReviewRun, ChangeUnit, ReviewBatch, Finding,            │
│          ModelUnit, ModelReference, ModelQuery,                  │
│          AgentDefinition, AgentInvocation, Credential(encrypted) │
│                                                                  │
│  Marketplace: GenAI Commons · MCP Client · Community Commons ·   │
│               Encryption                            (see §3.2)   │
│                                                                  │
│  Queues (§5.3a): mera-run (1 thread) · mera-batch (2–3 threads)  │
│  Java actions:                                                   │
│    JA_ResolveCommits      (JGit — list/validate commit range)    │
│    JA_RequestExtraction   (REST → Extractor)                     │
│    JA_LeaseWorkspace      (open/close extractor session, §7)     │
│    JA_InvokeAgent         (anthropic-java; IS the GenAI Commons  │
│                            connector microflow — §5.2)           │
│    JA_EstimateTokens                                             │
│  Microflow tools (§7): find_callers · find_references ·          │
│                        impact_radius   [over ModelReference]     │
└───────────────┬──────────────┬───────────────┬───────────────────┘
                │ HTTPS+mTLS   │ HTTPS         │ MCP streamable HTTP
                ▼              ▼               ▼   v2025-03-26
┌──────────────────────────┐ ┌────────────────────┐ ┌────────────────┐
│  MERA Extractor (sidecar)│ │ Anthropic Messages │ │ BP application │
│  Docker: git + mx + mxcli│ │ API                │ │  §14 — its own │
│                          │ │ · structured out   │ │  free app,     │
│  POST /extract           │ │ · prompt caching   │ │  own users     │
│   → clone (PAT, shallow) │ │ · web_search       │ │  MCP Server    │
│   → checkout base & head │ └────────────────────┘ │  module 5.1.0  │
│   → mx diff → manifest   │                        │  ⏰ sleeps —   │
│   → mxcli describe/unit  │ ┌──────────────────┐   │  woken §14.7   │
│   → mxcli refs → graph   │ │ Azure DevOps 7.1 │   └────────────────┘
│   → git diff text files  │ │ (work items, L3) │
│   → ChangeUnit[] +       │ └──────────────────┘
│     ModelUnit/Reference[]│
│                          │
│  POST   /workspaces   ─┐ │  leased session, TTL-bounded,
│  POST   /workspaces/{ }│ │  holds BOTH base and head .mpr
│           /describe    │ │  → mxcli describe / search
│  DELETE /workspaces/{ }─┘ │  → reaper lives HERE ONLY (§5.3)
└──────────────────────────┘
```

**Why the sidecar stays dumb.** It holds no review state, makes no decisions, has no LLM access. Its jobs are: (a) given `(repoUrl, PAT, baseSha, headSha)` return `ChangeUnit[]` plus the reference graph; (b) hold a TTL-bounded workspace that can answer `describe`/`search` against the base and head models. All intelligence stays in Mendix, which is what you wanted.

**Note the MCP arrow direction.** Layer 2's call to the best-practices server is made **from the Mendix runtime**, not from Anthropic. That is a change from revision 2 and it removes the requirement that the BP application be reachable from the public internet. See §6 and §14.

### 3.2 Marketplace and library dependencies — MERA

The BP app's own dependencies are separate; see §14.2.

**Marketplace modules — required**

| Module | Version | Why MERA needs it | Depends on |
|---|---|---|---|
| **GenAI Commons** | 7.1.0 | The vendor-agnostic substrate: `DeployedModel`, `Request`, `Response`, `Message`, `Usage`, `ToolCollection`, `FileCollection`, and `Trace`/`Span` observability. `JA_InvokeAgent` plugs in as the connector microflow behind `ChatCompletions_WithHistory`. Also what makes adopting Agent Commons later additive rather than a rewrite. | Community Commons |
| **MCP Client** | 4.1.0 | Layer 2's access to the best-practices MCP server. Streamable HTTP (protocol v2025-03-26) and HTTP+SSE (v2024-11-05); auth via a `GetCredentialsMicroflow` returning `System.HttpHeader` objects; tool allowlisting. `Request: Add all tools from MCP server` folds MCP tools into the same `ToolCollection` as the §7 microflow tools. | GenAI Commons |
| **Community Commons** | latest | Required by GenAI Commons; also the usual string/file/date utilities. | — |
| **Encryption** | latest | §4's credential-at-rest requirement — PATs and the Anthropic API key. | Community Commons |

**Studio Pro floor: 11.12**, inherited from Agents Kit 2. Confirm MERA's project version before installing anything.

**Marketplace modules — deliberately NOT taken**

| Module | Why not |
|---|---|
| **Agent Commons** 4.1.0 | Its runtime agent store is genuinely good, but it has no per-agent response-schema field, so Decision 9's guarantee would have to be smuggled through a `DeployedModel`-per-agent hack on an entity Mendix reshapes between kit versions. It also drags in Conversational UI, Administration and Nanoflow Commons for a chat UI MERA never opens — expensive on a 0.5 GiB database. Revisit once licensed; §10 stays compatible. |
| **Conversational UI** 7.1.0 | Chat front end. Only reason to have it was as an Agent Commons dependency. |
| **Agent Editor** 2.1.0 | Design-time agent documents. Superseded by §10's runtime `AgentDefinition`. |
| **All connector modules** — Mendix Cloud GenAI 7.1.0, Amazon Bedrock 11.0.0, OpenAI 9.1.0, Gemini 2.0.0, Mistral 2.0.0, Snowflake Cortex | Mendix Cloud GenAI needs paid resource packs on Mendix Cloud (out — Free App). Bedrock needs an AWS account plus AWS Authentication 4.1.1+, and does not carry Opus 5. MERA's own connector replaces all of them. |
| **PgVector Knowledge Base** 7.0.0 | No RAG in MERA — the guideline cache is plain entities. If the *BP app* ever moves to semantic search this appears there, not here (§14.5). |
| **MCP Server** 5.1.0 | MERA consumes MCP; it does not publish it. This module belongs to the BP app (§14.2). |

**Already present in a blank app** — Atlas Core / Atlas Web Content, Data Widgets, Nanoflow Commons, and Administration if you use account management. Nothing to install.

**Java managed dependencies (Maven, not Marketplace)**

| Artifact | Used by |
|---|---|
| `com.anthropic:anthropic-java` | `JA_InvokeAgent` — the connector. **Pin the version**; structured outputs are a moving surface (see §8.2). |
| `org.eclipse.jgit:org.eclipse.jgit` | `JA_ResolveCommits` — list and validate the commit range without a `git` binary. |

**Optional, Phase 5**

- A markdown-rendering widget for `SynthesisReport.markdownReport` — pick at build time; several exist and they differ in sanitisation behaviour, which matters because the content is LLM-generated.
- **Charts** for the cost dashboard.
- **Document Generation** for PDF export — verify its free-tier availability before promising PDF; it depends on a rendering service.

**Do not start from the Agents Kit starter app.** It installs the whole kit including Agent Commons and Conversational UI. Add the four required modules to MERA's existing project instead.

---

## 4. Git access via user PAT — mechanics and risk

### Mendix Team Server
- Clone URL: `https://git.api.mendix.com/<AppID>.git`
- Auth: username `pat` (or the Mendix username), password = the PAT
- Scopes needed: `mx:modelrepository:repo:read` (read-only is sufficient for MERA — **do not request write**)

### Non-Mendix remotes
Azure DevOps and GitHub both take PAT-as-password over HTTPS. Same code path. Model `Repository.provider` so credential handling and diff strategy can vary later.

### Two important caveats from the Mendix docs
1. Mendix **does not support** using a CLI-created clone in Studio Pro, and warns that manual modification of `.mpr` storage files corrupts the app. MERA is read-only and disposable-clone, so this is fine — but it must be *enforced*, not merely intended.
2. Studio Pro stamps Mendix-version metadata on commits; third-party commits can break Mendix Cloud deployment. **MERA must never push.** Configure the clone with no push remote and reject any write path in code review.

### Fetching efficiently
Mendix repos get large. Use, in order of impact:
- `--filter=blob:none` partial clone, or shallow fetch of just the two commits (`git fetch --depth=1 <sha>` twice when the server supports it)
- sparse-checkout limited to `*.mpr`, `mprcontents/**`, `javasource/**`, `theme/**`, `deployment/**`
- a warm cache volume keyed by AppID so repeat runs incrementally fetch
- hard cap on clone size + wall-clock, with a clean failure state on the `ReviewRun`

### Where credentials live — data, not constants, on every environment

*(Decision 4: individual PATs, matching MERA's existing per-project ownership model. Multiple reviewers each register their own.)*

MERA stores **no credential in a constant, on any environment, licensed or not.** Every credential — the reviewer's Team Server PAT, the Azure DevOps PAT, the Anthropic API key, the BP app's bearer token — is data: a `Credential` record owned by a `User`, encrypted at rest, reachable only through an XPath-constrained access rule on the owner. This is deliberate and it does not change when the app is licensed:

- **Entity access, module roles and XPath constraints are model-level.** They are defined in Studio Pro and enforced by the runtime, and they behave identically on a free node and a licensed one. Production-grade RBAC is not a licensing feature, and nothing in §13 Decision 8 touches it.
- Per-user credentials are the only shape consistent with Decision 4's attribution requirement — a run records *whose* PAT it used, which a single app-level constant could never express.
- The Anthropic API key follows the same pattern rather than becoming an app-level constant, so rotation is an ordinary data edit with an audit trail and an owner.

| Rule | Implementation |
|---|---|
| Never store plaintext | Encrypt at rest with the Encryption module |
| Never in a constant | Credentials are data, owned by a user — see above |
| Never log | Explicit redaction in the extractor's request logging; add a log-scrubber test |
| Never render | No PAT-valued attribute on any page; write-only input, show `••••••••` + last 4 |
| Scope to the owner | XPath-constrained access rule on `Credential.Owner`, inheriting MERA's existing project-level isolation |
| Bounded lifetime | Store `expiresAt`, warn at T-14d, block runs on expired |
| In transit | PAT travels Mendix → Extractor only. mTLS or signed short-lived request token; never as a URL parameter |
| In memory | Extractor holds it for the clone only, writes it to a git credential helper on tmpfs, wipes the workspace in a `finally` |

Because PATs are per-user, a workspace lease (§7) is also per-user — a leased workspace must carry the same access constraint as the `ReviewRun` that opened it, so a second user cannot query a model they have no rights to.

One property worth naming so nobody assumes more than is there: the Encryption module's symmetric key is **app-wide, not per-user**. Encryption at rest defends against database exfiltration and casual inspection; **the per-owner isolation comes from the XPath access rules, not from cryptography.** That is the correct division of labour — just don't let the Consent text describe it as per-user encryption.

**The honest risk statement:** you are asking users to hand a repository credential to a service that pipes derived content to a third-party LLM API. That is legitimate, but it needs to be *said out loud in the UI*: what is sent, to whom, retained for how long. Keep the per-repository `Consent` record with timestamp.

---

## 5. Orchestration inside Mendix

### 5.1 Make the orchestrator code, not a model — with one LLM exception

You described "a Claude orchestrator agent with 3 agents". There are two ways to build that:

**(a) Agentic orchestrator** — one Claude conversation whose tools are `run_layer1`, `run_layer2`, `run_layer3`, each implemented in Java as a nested conversation.
**(b) Deterministic pipeline** — Mendix runs the three layers, then a synthesizer.

**Recommend (b), with an LLM Planner pre-pass.** Reasoning:

- Fan-out is trivially parallel. Independent queued tasks beat a sequential tool loop on latency and cost.
- You lose nothing: the intelligence you actually want from an orchestrator is *scoping* — deciding which changes matter and what each layer should focus on. That is one cheap call, not a control loop.
- Failure handling, retries, partial results and audit are dramatically simpler when the control flow is a microflow you can look at — and the Layer 2 readiness gate in §5.3a is a good example of something an agentic orchestrator would have made unreasonably hard.
- Reproducibility. This mattered less once you confirmed reviews are **advisory** *(Decision 3)*, but a deterministic control flow is still the right default for a tool people run twenty times a day.

So: **Planner (LLM, cheap model) → deterministic fan-out to L1/L2/L3 (parallel, batched) → Synthesizer (LLM) → Report.**

No Marketplace module supplies any of this. Agent Commons, had we taken it, runs *one* agent per call and has no notion of a run, a batch, a budget or fan-out. §5.1 through §5.4 are MERA's own code either way — which is a large part of why taking only the substrate costs so little.

### 5.1a What "advisory" buys you

Decision 3 — a human lead developer owns the review, MERA accelerates it — is more architecturally significant than it looks. It means:

- **Agent-driven exploration becomes acceptable.** A gating tool must produce identical findings on identical input; that argues hard against letting agents wander the model. Advisory output does not carry that burden, which is precisely what makes §7's on-demand impact analysis viable. Take the win.
- **Recall beats precision.** A missed defect costs more than a false positive, because a human is filtering anyway. Tune severity thresholds and Planner filtering toward inclusion, and lean on the confidence field plus UI triage rather than suppressing at generation time.
- **The report's job changes.** It is a briefing for a reviewer, not a verdict. Keep the Synthesizer's `verdict` field — it is a useful summary signal — but the UI should frame it as "MERA suggests" and put the *reasoning* and *citations* front and centre.
- **Design for disagreement.** Every finding needs a one-click "not an issue" that feeds a per-repository suppression list. That list is also your best long-term quality metric.

### 5.2 The generic agent runner — and the connector

`JA_InvokeAgent` is **both** MERA's agent runner and the GenAI Commons connector microflow. One piece of code, two roles: GenAI Commons' `ChatCompletions_WithHistory` derives it from the `DeployedModel` and calls it with a `Request`, and MERA calls it directly with an `AgentInvocation`. Building it this way is what keeps the door open to Agent Commons later without a rewrite.

```
JA_InvokeAgent(
  AgentInvocation invocation   // holds agentKey, model, systemPrompt,
                               // userContent, toolConfigJson, outputSchema,
                               // maxTokens, temperature, cacheBreakpoints,
                               // toolBudgetJson, workspaceId?
) : AgentInvocation             // filled with rawResponse, parsedJson,
                                // stopReason, in/outTokens, cost, latency
```

Inside:

1. Build the Anthropic request with `anthropic-java`.
2. Attach `output_config.format` from the agent's `outputSchema` (§8.2). Attach tools per agent config (§6, §7) with `"strict": true`.
3. Attach `cache_control` breakpoints after the system prompt, the guideline corpus and the requirements document (§5.4).
4. Run the **tool loop** until `stop_reason != tool_use`, capped at N iterations, executing both §7 microflow tools and Layer 2's MCP tools out of the one GenAI Commons `ToolCollection`.
5. Enforce the §7.5 budgets here — this is the natural home for them, because this is where the loop lives.
6. Persist an `AgentInvocation` row **always**, success or failure, associated to the GenAI Commons `Trace`. Audit trail, cost report and debugging surface in one.

**No JSON-repair round-trip.** Revision 2 specified validate-then-one-repair. Structured outputs make malformed JSON a non-failure-mode, so schema validation becomes a cheap assertion that should never fire — log loudly if it ever does, because that means the schema and the model disagree about something.

Retries: exponential backoff on 429/529, jitter, `Retry-After` honoured, max 4. Owning the connector is what makes this policy ours rather than inherited. Idempotency: a `ReviewBatch` carries a UUID so a re-run of a partially failed pipeline doesn't duplicate findings.

**One thing `JA_InvokeAgent` must never do: wait for an external service to wake up.** By the time the tool loop runs, MERA has already paid for the batch's input tokens and is holding an executor thread. External readiness is settled before the call — §5.3a and §14.7.

### 5.3 Async and progress

Reviews take minutes. Do **not** run them in a request handler.

- Kick off with **Mendix Task Queue**; the run microflow is a queued task. **Confirmed working on a Free App node.**
- `ReviewRun.status`: `Draft → Extracting → Planning → Reviewing → Synthesizing → Completed | Failed | PartiallyCompleted`
- `ReviewRun.progress` (0–100) plus a `ReviewBatch` per unit-of-work; the UI polls with a refresh-timer nanoflow every 3–5s. Mendix has no clean server-push; polling is the pragmatic answer. On a free node this polling has a second job: it keeps the node awake for the duration of the run.
- Run batches concurrently but **cap it**. On a free node (1 GiB memory, 0.5 vCPU, single instance) use **2–3** concurrent Anthropic calls per run, not revision 2's 4–6. Make the cap a runtime setting so licensing lifts it without a redeploy.
- `PartiallyCompleted` is a real state: if the BP app cannot be reached, you still ship Layer 1 + 3 findings and mark Layer 2 `Skipped(reason)`. Never silently drop a layer.

**Scheduled events do not run on a Free App.** Revision 2 used them for two things, and both move:

- **Workspace-lease expiry (§7.3).** Revision 2 said "TTL on creation; reaper in the sidecar *and* a Mendix scheduled event; never rely on only one." On a free node you now rely on exactly one — the sidecar's reaper. It is ours and it is hosted outside Mendix, so this is acceptable, but it means the sidecar reaper needs to be correct rather than a backstop. Give it a test.
- **Retention purge (§9).** Becomes an **inline purge at end-of-run**, executed by the run microflow in its finally-equivalent path, plus an opportunistic sweep of orphaned prior runs at the start of each new run. Not as clean as a clock, but it is deterministic and it runs.

### 5.3a Task decomposition and the Layer 2 readiness gate

A review is not one long task. Decomposing it is what lets the queue's own retry machinery handle the BP app's cold start (§14.7) without a sleep-loop holding an executor thread on a 0.5 vCPU node.

**Two queues.** Keep pipeline control separate from agent work, so a task that is retrying and backing off can never head-of-line block a batch:

| Queue | Threads (free node) | Carries |
|---|---|---|
| `mera-run` | 1 | `TQ_ReviewRun`, `TQ_BpWakeUp`, `TQ_Layer2Dispatch`, `TQ_Synthesize`, `TQ_Purge` |
| `mera-batch` | 2–3 | `TQ_ReviewBatch` — one per batch per layer. This is where §5.3's concurrency cap actually lives. |

**The sequence.**

```
TQ_ReviewRun
   ├─► queue TQ_BpWakeUp            ← fires FIRST, before extraction
   ├─ extract (~35s) ─ plan ─ batch
   ├─► queue TQ_ReviewBatch × N      (L1, L3)   → mera-batch
   └─► queue TQ_Layer2Dispatch                  → mera-run
              │
              ├─ bpStatus = Ready       → queue TQ_ReviewBatch × M (L2)
              ├─ bpStatus = Waking      → throw; queue re-runs it with backoff
              └─ bpStatus = Unavailable → ReviewBatch(L2) = Skipped(reason)

TQ_Synthesize   blocked until every batch is terminal (Completed | Failed | Skipped)
TQ_Purge        §9 retention, inline at end of run
```

**Why the wake-up fires before extraction.** Extraction takes ~35 seconds against the real repo, and the Planner adds a few more. That is wall-clock MERA is already spending, during which nothing needs the BP app. Waking it in that window means the node is usually `Ready` before any Layer 2 batch exists — the wait costs nothing. Waking at Layer 2 time instead would spend a minute that was already available for free.

**Three Task Queue mechanics that shape this, and one that will bite you:**

- **Parameters are primitives or *committed* persistent entities.** Pass the `ReviewRun`; it works, because it is committed by then.
- **⚠️ A task only executes after its originating transaction commits.** So `ReviewRun.bpStatus` must be **committed before** the successor task is queued, or the successor reads a stale status and mis-routes. This is the single most likely bug in this section. Write status, commit, *then* queue.
- **You cannot start a task mid-transaction.** Structure each task as: do work → commit state → queue the next task, as the last thing it does.
- **Retries create their own queue entries sharing a sequence number.** That makes the backoff visible in the queue admin page, which is exactly what you want when diagnosing a stuck run — and the row cost against the 0.5 GiB budget is trivial.

**If MERA's own node sleeps mid-run** — the reviewer closed the browser, so §5.3's polling stopped — queued tasks stay in the database and resume when the node next wakes. The run pauses rather than failing. Worth stating so a "stuck" run isn't a mystery; the UI should show `startedAt` and let a reviewer see the run is simply parked.

### 5.4 Batching and token budget

A single sprint commit range can touch hundreds of units. Strategy:

1. **Filter** — Planner marks each ChangeUnit `review | skim | ignore`.
2. **Group** — batch by module, keeping related units together. Target **40–80k input tokens per batch**, leaving headroom for §7 tool results.
3. **Cache** — the layer system prompt, the guideline corpus and the requirements document are stable across batches within a run. Put `cache_control` breakpoints after them. On a 20-batch run this is the single biggest cost lever, and **owning the connector is the only reason it is available at all** — no Marketplace connector exposes it.
4. **Cheap model for the cheap jobs** — Planner on a small model; L1–L3 and Synthesizer on the frontier model.
5. **Hard ceiling** — a `maxSpend` on `ReviewRun`, checked before each batch, stopping cleanly at `PartiallyCompleted`.

Two structured-output interactions worth knowing (§8.2 has the detail): the injected format prompt raises input tokens slightly, and changing `output_config.format` invalidates the prompt cache for that thread. Since each agent's schema is fixed for the life of its `AgentDefinition` version, neither bites in practice — but it does mean **never edit a schema mid-run**.

---

## 6. Agent topology and tool surface

| Agent | Model | Tools | Context it gets |
|---|---|---|---|
| **A0 Planner** | small | none | Change manifest (names + types + change kinds only, no bodies) |
| **A1 Guidelines** | frontier | `web_search` (Mendix domains only) · **model-context tools (§7)** | ChangeUnit batch + curated guideline index |
| **A2 Best Practices** | frontier | **MCP tools from the BP app (§14)** | ChangeUnit batch + BP taxonomy |
| **A3 Functional** | frontier | **model-context tools (§7)** | ChangeUnit batch + work item(s) + requirements doc |
| **A4 Synthesizer** | frontier | none | All findings + change manifest (no raw model bodies) |

### Layer 1 — web search, but constrained
Server-side `web_search` (`web_search_20250305` or later) with `allowed_domains` locked to Mendix-official sources, `max_uses` 5–8 per batch, citations always on. $10/1k searches — meaningful at scale, so:

> **Build a guideline cache.** Most Mendix guidelines are stable. Crawl and store the relevant refguide pages as a `GuidelineDocument` entity in Mendix, refresh weekly, and inject the relevant subset into the prompt. Web search then becomes the *fallback*, not the primary retrieval path. This cuts cost and latency and makes runs reproducible. Version the cache and stamp `ReviewRun.guidelineCacheVersion`.

Two notes carried over from the connector decision. First, `web_search` is available to MERA *because* we own the request — no Marketplace connector exposes server-side tools. Second, **structured outputs are incompatible with Anthropic's Citations feature** (400 error). MERA's `citations[]` is an ordinary schema field, so nothing breaks today — but it permanently forecloses moving the guideline corpus onto the document-citations API. If that ever becomes attractive, it is a trade against Decision 9.

Note also that the weekly cache refresh was going to be a scheduled event. On a free node it isn't one: refresh on demand from the admin UI, and stamp `guidelineCacheVersion` with the refresh date so a stale cache is visible in every report.

### Layer 2 — MCP Client, from our runtime
*(Decision 1: the BP application is self-managed and exposes an MCP server — **its architecture is §14**. Decision 10 settles **who calls it**.)*

Revision 2 used Anthropic's server-side MCP connector, which required the BP server to be reachable by Anthropic over the public internet. The **Mendix MCP Client** makes the call from MERA's own runtime instead, so **the BP application can stay entirely internal**. That is a straight security and infrastructure win, and it is the single best thing the Marketplace stack contributes to this design.

Wiring on MERA's side:

- `Request: Add all tools from MCP server` folds the BP server's tools into the same GenAI Commons `ToolCollection` that the §7 microflow tools live in. `JA_InvokeAgent`'s loop executes both kinds without caring which is which.
- **Pin protocol v2025-03-26 (streamable HTTP) on both ends.** MCP Client supports v2024-11-05 (HTTP+SSE) as well, and *"MCP servers must support the same version as the client."* The Mendix MCP Server module speaks v2025-03-26, so the pin is natural — but assert it during the readiness check (§14.7) rather than discovering a mismatch as an empty tool list mid-batch.
- Auth is a `GetCredentialsMicroflow` returning a list of `System.HttpHeader` — a bearer token for MERA's service identity in the BP app (§14.6). **The microflow cannot take input parameters**, so the token has to be resolved from session or context inside it rather than passed in. Design for that.
- **Allowlist explicitly.** Only `search_best_practices`, `get_best_practice` and `list_categories`. Do this from day one even if the server is read-only today, so a future write tool can never be invoked by a reviewer agent. The BP app should also simply not publish write tools (§14.4) — two independent controls.
- **Never dispatch a Layer 2 batch before `ReviewRun.bpStatus = Ready`** (§5.3a, §14.7). This is the rule that stops a cold start from burning a batch's input tokens.
- MCP tools marked `"strict": true` count against the 20-strict-tools-per-request cap alongside the §7 tools (§8.2). Three is comfortable.

The trade-off to accept: tool round-trips now pass through the Mendix runtime rather than executing at Anthropic, so each hop costs latency and the results are tokens shipped both ways. For Layer 2's two to four lookups per batch this is not material.

### Layer 3 — the functional layer
You proposed feeding requirements statically. Do that for v1, but shape it so it grows:

- `RequirementDocument` entity — versioned, per repository, describing current app functionality. This is the "what the app already does" baseline.
- `WorkItem` entity — populated from **Azure DevOps REST 7.1** (`GET /{org}/{project}/_apis/wit/workitems/{id}?$expand=all&api-version=7.1`, PAT as basic auth). Pull `System.Title`, `System.Description`, `Microsoft.VSTS.Common.AcceptanceCriteria`, `System.State`, parent links.
- **Link commits to work items** by parsing commit messages for `AB#1234` / `#1234` / branch naming. Highest-value automation in Layer 3 — without it, someone types the ID by hand every run. Store the resolution and let the user correct it.
- Layer 3's job is *not* "is this good code" — it is **coverage and fidelity**, and, with §7, **regression risk**.

### A4 Synthesizer — separate agent, not the orchestrator
- Different job: deduplicate across layers, resolve contradictions, rank by severity × confidence, produce the human narrative.
- Different context: findings only, never the raw model. Small, cheap, focused — and it can't re-litigate the review.
- Different discipline: forbidden from inventing findings. Every synthesized item traces to a layer finding ID.

---

## 7. Model context and impact analysis

> **Your question:** should the functional reviewer see the full model and use `mxcli` to find usages of changed code?
> **Answer: yes — but give it a bounded query tool, not the model.** And give the same tool to Layer 1.

### 7.1 Why this is the right call

The case you identified — *existing* code being adjusted rather than a new feature built — is exactly where diff-only review is weakest, and it's the majority of real commits.

A new feature's diff is self-contained: everything the reviewer needs is in the change. A modification's diff is a *fragment of a larger behaviour*. If `Sales.ACT_Order_Confirm` changes its validation, whether that is correct depends entirely on who calls it, from which pages, with what preconditions. Layer 3's own mandate already includes "does this contradict, disable or alter behaviour described in the existing functional description" — that question is **structurally unanswerable from a diff alone**. Right now the A3 prompt tells the agent to answer "Cannot determine" in exactly these cases, which is honest but not very useful. §7 is what converts those into real findings.

Deletions are the sharpest version: if a unit was deleted, every reference to it is now broken, and *none of that appears in the diff for the deleted unit*.

And Decision 3 unlocks it. Agent-driven model exploration introduces run-to-run variance in which queries get made. For a gating tool that would be disqualifying. For an advisory tool whose output a lead developer reads and judges, it is a fair trade for materially better findings.

### 7.2 What not to do

**Do not put the full model in context.** A mid-size Mendix app rendered as MDL runs to millions of tokens. Even if it fit, it would bury the change set in noise and make every batch cost a fortune. The value is in *targeted* lookups.

**Do not let it become "review the whole app".** Guardrails in §7.5.

### 7.3 The three-tier design

Separate the **graph** (small: names, types, edges) from the **bodies** (large: rendered MDL). This split is the whole trick — it lets the cheap, deterministic 90% of impact analysis happen with no sidecar call at all.

---

**Tier 0 — Precomputed neighbourhood.** *(already in `ChangeUnit`)*

At extraction time, for every changed unit, run `mxcli refs` and `mxcli callers` at depth 1 and inline the result into the ChangeUnit. Free, deterministic, always present, no tool call needed. This alone answers "who uses this?" for most findings.

For `Deleted` units, run these against the **base** model — the unit no longer exists in head.

---

**Tier 1 — Reference graph materialised in Mendix.** *(recommended core of this feature)*

The extractor exports the whole-app reference graph once per run and returns it alongside the ChangeUnits:

```
ModelUnit      { qualifiedName, module, unitType, isFromMarketplace, exists(base|head|both) }
ModelReference { fromUnit, toUnit, referenceKind, location }
                 // referenceKind: Calls | Retrieves | Commits | Deletes |
                 //   ShowsPage | UsesEntity | UsesAttribute | UsesAssociation |
                 //   TriggeredBy | Publishes | Consumes | AccessRuleOn
```

Then the three navigation tools are **plain Mendix microflows over indexed data** — no sidecar, no `mxcli`, no latency, no nondeterminism:

| Tool | Implementation | Returns |
|---|---|---|
| `find_callers(qualifiedName, transitive, maxDepth)` | microflow over `ModelReference` | caller units with kind + location, grouped by module |
| `find_references(qualifiedName, referenceKind?)` | microflow | every unit referencing this one, filterable by kind |
| `impact_radius(qualifiedName, maxDepth)` | microflow, BFS | summary: counts by module and unit type, plus the top N most-connected consumers |

Three things this buys you beyond agent tooling:

1. **The human reviewer gets an impact graph in the UI.** This is a genuine product feature independent of the agents — a lead developer looking at "this entity changed" wants to see the blast radius. Same data, no extra work.
2. **Every query is auditable.** Log each as a `ModelQuery` row against the `ReviewRun`. You can see exactly what the agent looked at, and measure empirically whether your budgets are right.
3. **Deterministic and free.** No per-query cost, no external call, no failure mode.

**⚠️ Free App sizing.** For a large app the graph is 5–10k units and 50–150k edges — roughly **50–70 MB with indexes, or 10–15% of a Free App's entire 0.5 GiB database, per retained run.** Two consequences: purge the graph inline at end-of-run (§5.3), and keep at most one or two runs' graphs live. If measurement shows this is still too tight, the fallback is to hold the graph in the sidecar and make the Tier 1 tools REST calls — but try Mendix-side first, because the determinism, the zero per-query cost and the UI impact graph all come from having the data local. This is the first thing to load-test.

---

**Tier 2 — Body retrieval via leased workspace.**

Sometimes the agent genuinely needs to *read* an unchanged unit — "the criterion says notify the customer; `ACT_Order_Confirm` calls `SUB_NotifyCustomer`, which didn't change; does it actually send anything?"

That needs the real `.mpr`, so the extractor gains a short session lifecycle:

```
POST   /workspaces            {repoUrl, pat, baseSha, headSha, ttlSeconds}
                              → {workspaceId, expiresAt}
POST   /workspaces/{id}/describe   {qualifiedName, revision: base|head, detail}
                                   → mxcli describe -p <rev>.mpr <kind> <name>
POST   /workspaces/{id}/search     {query}
                                   → mxcli search -p head.mpr "<q>" --format json
DELETE /workspaces/{id}       → wipe clone, drop credentials
```

- The lease holds **both base and head** checkouts — needed for deleted units and for before/after comparison.
- TTL bounded to the run (say 30 min). **The sidecar's reaper is now the only enforcement** — a Free App runs no scheduled events (§5.3). Test it.
- Opened **lazily** — only if the Planner flagged any unit `impactAnalysisRequired`. Most runs never open one.
- Carries the same access constraint as the `ReviewRun` (§4).

Exposed to agents as:

| Tool | Returns |
|---|---|
| `describe_unit(qualifiedName, revision, detail)` | rendered MDL for one unit; `detail: summary \| full` |
| `search_model(query)` | matching units by name/caption/MDL text |

If the lease can't be opened or `mxcli` fails, the tool returns a structured `{"unavailable": true, reason}` rather than erroring — the agent then works from Tier 0/1 and says so. Graceful degradation over hard failure, every time.

### 7.4 Who gets these tools

| Layer | Tier 0/1 (graph) | Tier 2 (bodies) | Why |
|---|---|---|---|
| **L1 Guidelines** | ✅ | ✅ (smaller budget) | A changed entity's risk is entirely about its consumers: delete behaviour, access rules, removed attributes, published contract changes. L1 needs this as much as L3. |
| **L2 Best Practices** | ❌ | ❌ | Its authority is the BP database. Model traversal would invite it to freelance beyond what the practices cover — precisely the failure mode the A2 prompt is written to prevent. |
| **L3 Functional** | ✅ | ✅ | Regression risk and coverage. The primary consumer of this feature. |

### 7.5 Guardrails

These matter more than the tools themselves. All of them are enforced inside `JA_InvokeAgent` (§5.2), which owns the tool loop.

- **Per-batch budgets.** Suggested starting point: **15 graph calls** (Tier 0/1, cheap) and **6 body retrievals** (Tier 2, expensive) per batch for L3; roughly half that for L1. Tune from the `ModelQuery` data after a few weeks.
- **Budget exhaustion returns a tool result, not an error**: `{"budgetExhausted": true, "remaining": 0}`. The agent then concludes gracefully instead of derailing. This choice now has a second justification: Anthropic's compiled-grammar cache is invalidated by a change to the *set* of tools in a request, so removing an exhausted tool mid-run would throw away the cached grammar and pay the compile again (§8.2).
- **Hard depth cap of 3** in the microflow, regardless of what the agent asks for.
- **Truncate wide results.** Cap caller lists at ~40 with `"and 312 more in 18 modules"`. A unit called from 300 places is itself the signal; the list is noise and a token sink.
- **Findings still anchor to changed units.** An impact finding's `qualifiedName` is the *changed* unit. The affected consumers go in `affectedUnits[]`. This is what stops the review sprawling into untouched code that nobody in this commit touched.
- **Cap total tool tokens per batch** (say 25k) alongside the call count — one `describe_unit` on a monster page can blow the batch budget on its own.
- **Mark every tool `"strict": true`** so tool arguments are grammar-guaranteed too (§8.2). This kills a whole class of malformed-call retries.

### 7.6 Focusing the budget: Planner drives it

The Planner already classifies every unit and knows `changeKind`. Have it emit `impactAnalysisRequired: bool` per unit:

**True for** — `Deleted` anything · `Modified` with a signature change (attribute removed/renamed/retyped, association changed, microflow parameter or return type changed, published REST/OData contract changed, entity access rule changed, delete behaviour changed) · anything already on `highRiskUnits`.

**False for** — `Added` units (nothing depends on them yet) · pure-UI page tweaks · anything marked `skim` or `ignore`.

This is the cheapest possible targeting: one small-model call decides where the expensive budget gets spent, and most runs skip Tier 2 entirely.

### 7.7 The escape hatch if L3's context blows up

If the exploration turns out to consume too much of L3's context — plausible on large change sets — promote it to a **pre-pass**: an `A5 Impact Analyst` that runs once per `impactAnalysisRequired` unit, does the traversal, and emits a compact **Impact Brief** (≤300 words: who consumes this, which of those paths the change plausibly affects, what to verify). Inject that brief into both L1 and L3 as static context.

Trade-off: deterministic and paid once and shared by two layers, but not targeted by the reviewing agent's actual concern. Start with tools on L1/L3; keep the ChangeUnit shape ready for an `impactBrief` field so this is a config change rather than a rewrite.

---

## 8. Finding schema — the contract that holds it all together

Every layer emits the same object. This is what makes synthesis, storage, UI and metrics possible. **As of Decision 9 it is also a compiled grammar**, which changes how it must be written.

### 8.1 The schema

```jsonc
{
  "findingId": "L3-0004",
  "layer": 3,                        // 1 | 2 | 3
  "severity": "Major",               // Blocker | Major | Minor | Info
  "confidence": 0.78,                // 0.0–1.0, enforced in the microflow, not the schema
  "title": "Confirmation flow no longer notifies customer on partial shipment",
  "module": "Sales",
  "unitType": "Microflow",
  "qualifiedName": "Sales.ACT_Order_Confirm",   // the CHANGED unit
  "location": "removed call to 'SUB_NotifyCustomer'",
  "changeAnchor": "Modified",        // Added | Modified | Deleted | Moved
  "affectedUnits": [                 // §7 — impacted, but NOT changed. [] when none
    {"qualifiedName": "Sales.SUB_PartialShipment",
     "referenceKind": "Calls",
     "note": "sole remaining caller; relies on notification side effect"}
  ],
  "evidence": "…verbatim excerpt from the MDL/diff…",
  "rationale": "…why this is a problem, in reviewer voice…",
  "citations": [                     // MANDATORY for layers 1 and 2. [] otherwise
    {"kind":"mendix-doc","title":"Microflow best practices",
     "url":"https://docs.mendix.com/refguide/…","quote":"…"}
  ],
  "recommendation": "…",
  "effort": "S",                     // S | M | L
  "falsePositiveRisk": "Low",        // Low | Medium | High
  "unverified": false                // true = could not be grounded; see A1's hard rules
}
```

**Every property is required.** Absence is expressed as an empty array or an empty string, never as an optional or nullable property. This is not stylistic — see §8.2.

**Three rules that decide whether MERA is trusted or ignored:**

1. **No citation, no finding (L1/L2).** If the agent cannot point to a documented guideline or a BP record, drop it or reclassify as `Info` with `unverified: true`. Nothing kills a review tool faster than confidently invented rules.
2. **Anchor to the change.** Findings reference a ChangeUnit in this range; impacted-but-unchanged units go in `affectedUnits`, pre-existing problems go in `observations` below the fold. Reviewers get furious when a tool flags code they didn't write.
3. **Evidence must be verbatim** — from the batch input or from a §7 tool result. This is cheaply verifiable in post-processing: reject findings whose `evidence` string doesn't appear in either. **Structured outputs do not do this for you** — they guarantee shape, not truth — so this check stays, and it remains the cheapest hallucination catch in the system.

### 8.2 Enforcing it — Anthropic structured outputs

*(Decision 9)* Output shape is guaranteed at the grammar level, not requested in a prompt:

```jsonc
{
  "model": "claude-opus-5",
  "output_config": {
    "format": { "type": "json_schema", "schema": { /* the agent's outputSchema */ } }
  },
  "tools": [ /* §7 tools, each with "strict": true */ ],
  "messages": [ /* ... */ ]
}
```

**GA — no beta header.** Supported on Opus 4.5–5, Sonnet 4.5–5 and Haiku 4.5, so both the small Planner model and the frontier layer models. `output_format` is the legacy spelling; `output_config.format` is current. Structured outputs and strict tool use are separate features that compose — use both.

**Two properties that suit MERA specifically.** Compiled grammars are **cached 24 hours from last use**, so a 20-batch run pays the compile latency once and the next day's run usually still hits the cache. And the cache is invalidated by schema structure and by the *set* of tools — **not** by `name` or `description` text — so prompt iteration is free, and §7.5's tool-result-not-tool-removal choice protects it.

**The caps are hard, and they combine across the response schema and every strict tool schema in the same request:**

| Limit | Value |
|---|---|
| Strict tools per request | 20 |
| **Optional properties, total** | **24** |
| Properties with union types (`anyOf`, type arrays) | 16 |
| Compilation timeout | 180s |

There is also an undocumented internal grammar-size limit: you can satisfy every number above and still get `400 Schema is too complex for compilation`.

**This is why every property is required.** The Finding object plus A3's additions would burn through 24 optionals on its own, before the five §7 tool schemas and Layer 2's MCP tools take their share. Writing the schema all-required drops the count to zero and leaves the whole budget unused. Keep it that way.

**Unsupported keywords, silently dropped:** `minimum`, `maximum`, `minLength`, `maxLength`, `minItems`, `maxItems`, `multipleOf`, `exclusiveMinimum`/`Maximum`; most string `format` values (only `date`, `time`, `date-time`, `email`, `uri`, `uuid` survive); `pattern` is limited. Practically:

- `confidence` cannot be bounded 0–1 by schema. Put the range in the property `description` and clamp in `JA_InvokeAgent`.
- `severity`, `effort`, `falsePositiveRisk`, `changeAnchor`, `layer`, `referenceKind` are all `enum` — **supported**. Lean on enums wherever revision 2 relied on prose.
- The official Python/TS/Ruby/PHP SDKs strip unsupported keywords into descriptions and re-validate afterwards. **Whether `anthropic-java` does this is unconfirmed** — check it, and if it doesn't, do the strip-and-revalidate inside the connector.

**Per-agent envelopes**, all-required, `outputSchema` on the `AgentDefinition`:

| Agent | Envelope |
|---|---|
| A0 | `{verdicts:[{qualifiedName, verdict, reason, impactAnalysisRequired}], focusNotes:{layer1, layer2, layer3}, highRiskUnits:[], estimatedBatches}` |
| A1 | `{findings:[Finding], observations:[], reviewLimitations:[]}` |
| A2 | `{findings:[Finding], uncoveredConcerns:[], toolFailure, toolFailureReason}` |
| A3 | `{findings:[Finding], coverage:[{criterion, verdict, implementingUnit, note}], requirementQuality:{score, issues:[]}, noRequirements, reviewLimitations:[]}` |
| A4 | `{executiveSummary, verdict, items:[{…, sourceFindingIds:[], raisedBy:[]}], reviewerNotes:[], notReviewed:[], markdownReport}` |

**Never edit a schema mid-run** — it invalidates both the compiled grammar and the prompt cache for that thread (§5.4).

---

## 9. Domain model sketch

```
Repository ──< Credential (encrypted, owner, scope, expiresAt)
    │
    ├──< RequirementDocument (version, content, activeFrom)
    ├──< SuppressedFinding (fingerprint, reason, suppressedBy, at)
    └──< ReviewRun
            ├─ baseCommit, headCommit, branch, status, progress,
            │  startedAt, finishedAt, totalInputTokens, totalOutputTokens,
            │  estimatedCost, maxSpend, guidelineCacheVersion, initiatedBy,
            │  workspaceId, workspaceExpiresAt
            ├─ bpStatus, bpUnavailableReason, bpWakeDeadline,
            │  bpFirstContactAt, bpContractVersion              // §14.7
            ├──< WorkItem (adoId, title, description, acceptanceCriteria, state)
            ├──< ChangeUnit (module, unitType, qualifiedName, changeKind,
            │                structuralDelta, beforeMdl, afterMdl,
            │                neighbourhood, tokenEstimate,
            │                plannerVerdict, impactAnalysisRequired)
            ├──< ModelUnit ──< ModelReference          // §7 Tier 1 graph
            ├──< ModelQuery (agentKey, tool, argsJson, resultSize, ms)
            ├──< ReviewBatch (layer, index, status, skipReason,
            │        │         inputTokens, outputTokens, latencyMs)
            │        └──< Finding ──< Citation
            │                    └──< AffectedUnit
            ├──< AgentInvocation (agentKey, agentDefinitionVersion, model,
            │                     requestId, stopReason, cost, latencyMs)
            │        └──1 GenAICommons.Trace ──< Span
            └──1 SynthesisReport (executiveSummary, verdict, markdown, pdf)

AgentDefinition (agentKey, version, model, systemPrompt, toolConfig,
                 outputSchema, isActive)          // §10 — MERA's own
```

**GenAI Commons carries part of the audit trail.** `Usage` (tokens, duration) and `Trace`/`Span` (per-model, per-tool, per-knowledge-base, per-MCP call tree) come free with the module. Associate `AgentInvocation` to the `Trace` rather than duplicating it, and let `ModelQuery` be a thin index over the tool spans. On a 0.5 GiB database that saving is not nothing.

**Retention** *(Decision 5, revised by Decision 8)*:

- **`rawRequest`/`rawResponse` are not persisted as `FileDocument`s on a free node.** 1 GiB of file storage disappears fast at 60–80k tokens per batch. Log them to the sidecar, or persist only on failure, or keep only the last N. Restore full retention when licensed.
- **Purge runs inline at end-of-run** via `TQ_Purge` (§5.3a), not on a scheduled event: `ModelUnit`, `ModelReference`, `ModelQuery` and the Commons `Trace`/`Span` for that run, plus an opportunistic sweep of orphaned prior runs at the start of each new run.
- **`Finding` and `SynthesisReport` are the durable output** and survive the purge. Strip `evidence` at purge time if you want the reports without the model excerpts.
- Retention window stays a runtime constant; 7 days is still the right default when licensed.

---

## 10. Agent definitions

Store these as `AgentDefinition` records in Mendix (agentKey, version, model, systemPrompt, toolConfig, **outputSchema**, isActive) rather than hardcoding them in microflows. Prompt iteration is the main tuning loop of this product — you want to change a prompt without a redeploy, and `ReviewRun` records which prompt *version* produced a report.

`outputSchema` is now load-bearing rather than aspirational: it is the JSON Schema `JA_InvokeAgent` compiles into `output_config.format` (§8.2). Version it with the prompt, because changing it changes the compiled grammar.

**Why this is MERA's own entity rather than Agent Commons.** Agent Commons offers exactly this store with a better UI and a prompt test harness — but it has no per-agent response-schema field, and Decision 9's guarantee is not something to route through an undocumented workaround (§3.2). The store itself is an entity and a page. What is worth rebuilding cheaply is the **test harness**: one page that runs a saved prompt version against a saved `ChangeUnit` fixture and shows the parsed result. Budget for it in Phase 5 — prompt iteration is where this product is won.

---

### A0 — Planner

**Model:** small · **Tools:** none · **Max tokens:** 4k

```
You are the Planner for MERA, a Mendix code-review system. You do not review
code. You decide what deserves review, where deeper analysis is worth paying
for, and how the work should be divided.

You receive a manifest of every model unit and file changed between two commits
in a Mendix application repository. You receive names, types and change kinds
only — never bodies. You also receive the linked work item title, if any.

## 1. Review verdict
For each entry, assign exactly one:
  review  — substantive change to application logic, data, security, UI or
            configuration. Default for anything you are unsure about.
  skim    — mechanical, low-risk change (renames without behavioural change,
            translation entries, layout-only page tweaks).
  ignore  — no review value: marketplace module version bumps, generated
            artifacts, .mpr metadata churn, formatting-only changes.

Bias hard toward `review`. This review is advisory — a human lead developer
filters the output — so a false positive costs a few cents and a moment of
their attention, while a false `ignore` hides a defect completely.

## 2. Impact analysis flag
Set `impactAnalysisRequired: true` where understanding the change requires
knowing what else in the app depends on it:
  - ANY deleted unit — every reference to it is now broken, and none of that
    is visible in the diff.
  - Modified units with a signature change: attribute removed, renamed or
    retyped; association changed; microflow parameter or return type changed;
    published REST/OData contract changed; entity access rule or delete
    behaviour changed.
  - Anything you list in highRiskUnits.
Set it `false` for added units — nothing depends on them yet — and for
UI-only tweaks, skims and ignores. This flag spends a limited budget: be
deliberate, not generous.

## 3. Focus notes
For each of the three review layers, write a short note (max 60 words) telling
that layer what matters most in this change set. Ground it in what you actually
see in the manifest — never invent concerns.
  Layer 1 — Mendix platform guidelines and technical correctness
  Layer 2 — organisational best practices
  Layer 3 — functional fidelity to the work item

## 4. High-risk units
Flag any change touching security rules, entity access, persistent entity
structure, published REST/web services, scheduled events, or before/after-commit
event handlers. These are reviewed first and never skimmed.

Output only JSON matching the supplied schema. No prose.
```

---

### A1 — Mendix Guidelines Reviewer (Layer 1)

**Model:** frontier · **Tools:** `web_search` (Mendix domains) + `find_callers`, `find_references`, `impact_radius`, `describe_unit` · **Max tokens:** 16k

```
You are a Mendix Platform Expert performing the technical guideline layer of a
code review. You have reviewed hundreds of Mendix applications. You are the
person a team calls when they want to know whether something is "the Mendix
way" — and you can always say why, with a source.

## Scope
You review ONLY against official Mendix platform guidance: the Mendix Reference
Guide, official Mendix best-practice documentation, Mendix Expert/Evaluation
guidelines, and official Mendix blog and community guidance. Organisational
conventions are Layer 2's job. Whether the feature is correct is Layer 3's job.
Stay in your lane — overlapping findings are deduplicated later and yours will
lose if it is not grounded in platform guidance.

## What you look for
- Microflow and nanoflow design: retrieves inside loops, unbounded retrieves,
  missing error handling, transaction boundaries, wrong client/server split.
- Domain model: entity type choice, association direction and multiplicity,
  delete behaviour, indexes, attribute types, validation placement.
- Security: entity access rules, XPath constraints, module roles, published
  service authentication, anonymous access, client-side-only validation.
- Pages and UX: widget choice, data source efficiency, conditional visibility
  vs editability, accessibility attributes, listen-to-widget usage.
- Performance: retrieve strategies, XPath vs OQL, caching, indexed attributes,
  batch sizes, image/file handling.
- Integration: REST/OData consumption patterns, error handling, timeouts,
  mapping design, published service contracts.
- Java actions: exception handling, Core API misuse, transaction handling,
  thread safety, resource leaks, logging hygiene.
- Naming and structure only where an official Mendix convention exists.

## Model context tools
Some guideline violations are only visible when you know who consumes a changed
element. Use your tools when the change is a *modification or deletion* of
something other code depends on:
  find_callers / find_references / impact_radius — the reference graph. Cheap.
  describe_unit — read an unchanged unit's actual definition. Expensive; use
                  only when the graph is not enough to settle the question.
The cases that most warrant this: an entity whose attributes, access rules or
delete behaviour changed; a microflow whose signature or error handling changed;
a published service whose contract changed; anything deleted.

Do not go exploring out of curiosity. Every query should be traceable to a
specific concern you are trying to confirm or dismiss. You have a limited
budget; when a tool reports it is exhausted, finish with what you have and note
the limitation in reviewLimitations.

## Method
1. Read the change set. Build a mental model of what changed and why.
2. For each concern you form, decide: is there official Mendix guidance that
   supports this? If confident, cite it from memory with the specific document
   title. If unsure whether current guidance says what you think — or the area
   changed across Mendix versions — use web_search. Search verifies; it does
   not generate ideas.
3. Where the concern depends on consumers of the changed element, query the
   model before deciding.
4. Discard any concern you cannot ground. State it as Info with
   unverified: true, or drop it.

## Hard rules
- Every finding at Minor or above MUST carry at least one citation with a real
  URL and a quoted passage. No citation, no finding.
- `evidence` MUST be verbatim from the material you were given or from a tool
  result. Never paraphrase into evidence. Never invent line references.
- Review only what changed in this commit range. Units you reached through the
  reference graph belong in `affectedUnits` on a finding about the CHANGED
  unit — never as findings of their own. Pre-existing problems in surrounding
  context go in `observations`.
- Do not report style preferences as defects.
- Do not report the same underlying issue twice under different titles.
- If the change set is clean, return an empty findings array. A review with no
  findings is a valid and valuable result. Do not manufacture findings to seem
  thorough.
- Fields you have nothing to say in take an empty string or an empty array.
  Never omit a field.

## Severity calibration
  Blocker — will break in production, loses data, or opens a security hole.
  Major   — significant performance, maintainability or correctness risk.
  Minor   — real but contained; worth fixing, not worth blocking.
  Info    — observation, or a finding you could not fully ground.

Set `confidence` honestly, between 0.0 and 1.0. 0.9+ means you would defend this
in a review meeting. Below 0.6 means you are flagging it for a human to judge.
A human lead developer reads everything you produce and makes the final call, so
a well-reasoned uncertain finding is worth more than silence.
```

---

### A2 — Best Practices Reviewer (Layer 2)

**Model:** frontier · **Tools:** MCP tools from the best-practices application (read tools only — §14.4) · **Max tokens:** 16k

```
You are the guardian of this organisation's Mendix engineering standards. Your
authority comes entirely from the best-practices database you can query — not
from your own opinions about good Mendix development. That is Layer 1's job.

## Scope
You review the change set against the organisation's own best-practice records,
retrieved through your tools. Every finding must map to a specific
best-practice record, by ID.

You deliberately have no access to the wider application model. Your judgements
are about conformance to written standards, which is decidable from the change
set and the standards themselves.

## Method
1. Read the change set and identify which categories of practice are in play
   (naming, layering, module structure, security conventions, logging, error
   handling, reuse, testing, documentation, deployment). Call list_categories
   if you are unsure.
2. Query the database for the practices that apply. Query broadly enough to be
   thorough and narrowly enough to stay relevant. Do not guess at practice
   content — retrieve it.
3. Compare the change set against each retrieved practice.
4. Report only actual deviations, with the practice ID and its stated rule.

## Hard rules
- Every finding MUST reference a best-practice record ID retrieved this session,
  with its version. If you believe something is wrong but no practice covers it,
  do not report it as a violation — add it to `uncoveredConcerns` with a note.
  That list is valuable: it tells the organisation where its standards have gaps.
- Never invent a practice ID. Never paraphrase a practice into something
  stricter than it says.
- If a practice is ambiguous about this case, say so and set confidence below
  0.6 rather than ruling either way.
- `evidence` must be verbatim from the supplied change set.
- Review only what changed in this commit range.
- If your tools fail or return nothing, do not fall back to generic Mendix
  advice. Set `toolFailure: true` with an explanation in toolFailureReason and
  return an empty findings array. A skipped layer is honest; a substituted layer
  is misleading.
- Fields you have nothing to say in take an empty string or an empty array.
  Never omit a field.

## Severity
Inherit severity from the best-practice record where it defines one. Where it
does not, map: mandatory → Major, recommended → Minor, advisory → Info.
Escalate to Blocker only where the practice itself says the violation is
release-blocking.
```

---

### A3 — Functional Reviewer (Layer 3)

**Model:** frontier · **Tools:** `find_callers`, `find_references`, `impact_radius`, `describe_unit`, `search_model` · **Max tokens:** 16k

```
You are a senior functional analyst reviewing whether a Mendix change actually
delivers what was asked for. You do not assess code quality, performance or
conventions — other reviewers cover those. Your single question is:

  "Does this change implement the requested functionality, correctly,
   completely, and without breaking or contradicting anything else?"

## What you are given
- The work item: title, description, acceptance criteria, state.
- The application's current functional description (the baseline of what the
  app already does).
- The change set, rendered as model definitions and code diffs. Each changed
  unit includes its immediate neighbourhood — what references it, at depth 1.
- Tools to explore the rest of the model on demand (below).

## Model context tools — read this carefully
A diff is a fragment of a larger behaviour. When a NEW feature is added, the
change set usually tells you everything. When EXISTING functionality is
adjusted, it usually does not: whether the change is correct depends on who
calls the changed element and what they expect from it.

  find_callers(qualifiedName, transitive, maxDepth)  — what invokes this
  find_references(qualifiedName, referenceKind)      — everything that uses it
  impact_radius(qualifiedName, maxDepth)             — blast-radius summary
  describe_unit(qualifiedName, revision, detail)     — read an unchanged unit
  search_model(query)                                — find units by name/text

Use them when:
  - A unit was DELETED. Find everything that referenced it. This is the single
    highest-value use of these tools — the breakage is invisible in the diff.
  - Existing behaviour was MODIFIED and you need to know which flows depended
    on the old behaviour.
  - An acceptance criterion describes an outcome you cannot find in the change
    set, and it may be satisfied by an unchanged unit the change set calls
    into. Follow the call before concluding "not implemented".
  - You suspect a regression but need to see an unchanged consumer to confirm.

Do not use them to browse. Every query should answer a specific question you
have already formed. Prefer the graph tools (cheap) and reach for
describe_unit only when you must read actual logic. You have a limited budget;
when a tool reports it exhausted, conclude with what you have and say in
`reviewLimitations` what you could not check. If a tool returns
`"unavailable": true`, note that impact analysis was degraded and fall back to
the neighbourhood data you were given.

## What you assess
1. **Coverage** — walk each acceptance criterion. For each: Implemented /
   Partially / Not implemented / Cannot determine. Cite the specific unit that
   implements it. Before answering "Not implemented", check whether an
   unchanged unit reached from the change set satisfies it.
2. **Fidelity** — where implemented, does the behaviour match what was asked,
   including edge cases the criteria imply (empty states, permissions,
   validation, error paths, concurrent access)?
3. **Scope creep** — changes present that no criterion asks for. Not
   automatically wrong, but unreviewed work someone should have decided on.
4. **Regression risk** — does this contradict, disable or alter behaviour
   described in the functional description, or behaviour that consumers of the
   changed units depend on? Use your tools here. Name the affected consumers.
5. **Ambiguity** — where a criterion has more than one reasonable reading and
   the implementation picked one, surface it as a question for the author.

## Hard rules
- Reason only from the artefacts and tool results. If the work item is thin,
  say so in `requirementQuality` — do not fill the gap with assumptions about
  what the business "probably" wanted.
- Do not report technical quality issues. An inefficient microflow that meets
  the requirement is not your finding.
- `evidence` must be verbatim from the change set, work item, or a tool result.
- Every coverage verdict must name the unit that satisfies it, or explain why
  nothing does.
- A finding's `qualifiedName` is always a unit that CHANGED in this commit
  range. Units you discovered through the model go in `affectedUnits`, never as
  findings of their own.
- If no work item was supplied, set `noRequirements: true`, leave coverage
  empty, and report only scope observations describing what the change appears
  to do. Never invent requirements to review against.
- Fields you have nothing to say in take an empty string or an empty array.
  Never omit a field.

## Tone
Write findings as questions where you are inferring intent, and as statements
where the artefacts are unambiguous. You are the reviewer who catches "the
criteria said 'notify the customer' and the only notification path was removed"
— that is worth more than any style comment. A human lead developer reads your
output and decides; give them the reasoning, not just the verdict.
```

---

### A4 — Synthesizer

**Model:** frontier · **Tools:** none · **Max tokens:** 12k

```
You are the lead reviewer's assistant. Three specialists have reviewed a Mendix
change set independently — platform guidelines, organisational best practices,
and functional fidelity. You did not see the code. You see their findings, the
change manifest, and the work item.

Your job is to turn three lists into one briefing that a lead developer can act
on in five minutes. They own the review; you are preparing it for them.

## What you do
1. **Deduplicate.** The same underlying issue often appears in more than one
   layer. Merge into a single finding, keep every citation, keep the highest
   severity, note which layers raised it (`raisedBy: [1,3]`). Corroboration
   across layers raises confidence — say so explicitly.
2. **Resolve contradictions.** Where two layers conflict, do not silently pick
   one. Present the tension and give a reasoned recommendation, naming the
   trade-off. Functional correctness generally outranks convention; security
   outranks everything.
3. **Rank.** Severity, then confidence, then blast radius (a finding with many
   `affectedUnits` outranks an equally severe isolated one). The reader should
   be able to stop after item five and still have handled what matters.
4. **Write the executive summary.** Three to five sentences: what this change
   does, your read on whether it is fit to merge, and the single most important
   thing to look at. Written for a tech lead skimming on a phone.
5. **Give a verdict** — Approve / Approve with comments / Request changes /
   Blocked — and state the criterion you applied. This is a recommendation to a
   human reviewer, not a gate. Phrase it as such.
6. **Note what was not reviewed** in `notReviewed`. Skipped layers, ignored
   units, exhausted tool budgets, low-confidence areas, missing requirements,
   degraded impact analysis. The reader must know the edges of the review to
   trust the middle. If Layer 2 was skipped because the best-practices service
   could not be reached, say so plainly — an unreviewed layer is not a clean one.

## Hard rules
- You may not create findings. Every item traces to at least one input finding
  ID via `sourceFindingIds`. If you think the specialists missed something, say
  so in `reviewerNotes` — never as a finding.
- You may not upgrade severity except when merging (take the max) or when
  functional impact demonstrably makes a technical finding worse — and then
  justify it in the finding.
- Preserve citations exactly. Never rewrite a quoted source.
- Preserve `affectedUnits`. The blast radius is often the most useful thing on
  the page.
- Do not soften findings to make the review pleasant, and do not sharpen them
  to seem rigorous. Report what the layers found.
- If all three layers returned empty, say clearly that the change set was clean
  and briefly what was checked. Do not pad.

## Tone
Direct, specific, collegial. You are briefing a senior colleague who will
disagree with you sometimes and should feel free to. Lead with what matters.
Assume competence.

`markdownReport` carries the full human-readable review.
```

---

## 11. Risks and mitigations

| Risk | Severity | Mitigation |
|---|---|---|
| `mxcli` is alpha; fails or misreads a model version | High | Read-only on disposable clones; pin version; compatibility check before run; degrade to `mx diff`-only with a visible quality warning |
| Mendix version skew between repo and extractor's `mx` | High | Read model version from the manifest first; `mx diff -l`; maintain a Studio Pro version matrix in the image; fail loudly rather than diff wrongly |
| Hallucinated guidelines presented authoritatively | High | No-citation-no-finding; mechanical verbatim-evidence check across batch input *and* tool results; citations rendered clickable in the UI. **Structured outputs do not help here** — they guarantee shape, not truth |
| PAT compromise | High | §4 controls; read-only scope; encrypted at rest; owner-constrained XPath access; never logged; short-lived transport token; per-user ownership carried onto workspace leases |
| `anthropic-java` structured-output surface drifts | Medium | Pin the SDK version; confirm whether it strips unsupported schema keywords (§8.2); one integration test that sends the real §8 schema and asserts a parse |
| §8 schema outgrows the grammar caps | Medium | All-required schema keeps optionals at zero; keep the strict-tool count small; compile the real schema once before Phase 3 |
| Tier 1 graph exceeds the free node's 0.5 GiB database | Medium | §7.3: inline purge at end-of-run, one or two runs live; load-test 150k edges early; sidecar-hosted graph as the fallback |
| Impact analysis balloons cost/context | Medium | §7.5 budgets; Planner-driven targeting; graph tools free and preferred; result truncation; total tool-token cap per batch; §7.7 pre-pass escape hatch |
| Orphaned workspace lease — now single-enforcement | Medium | Sidecar reaper is the only mechanism on a free node (no scheduled events). Give it a real test; TTL on creation; lease opened lazily |
| Review sprawls into untouched code | Medium | `affectedUnits` instead of standalone findings; findings must anchor to a ChangeUnit; enforced in post-processing, not just in the prompt |
| Cost runaway on a large commit range | Medium | Planner filtering; prompt caching (available because we own the connector); `maxSpend` per run; pre-run cost estimate before the user confirms |
| Huge change sets exceed context | Medium | ChangeUnit batching with token estimates; per-batch budget; Synthesizer never sees raw model |
| Layer 2 retrieval misses applicable practices | Medium | §14.5: curated `searchText`, whole records not IDs, capped ranked results. Measure via `uncoveredConcerns` and A2 confidence before reaching for embeddings |
| **BP app asleep when a review starts** | **Medium** | **§14.7: `TQ_BpWakeUp` fires before extraction, in the ~35s the run already spends; retryable/terminal classification; exponential backoff to a 5-minute wall-clock deadline; `TQ_Layer2Dispatch` gates batch dispatch on `bpStatus` so no input tokens are spent on a batch that would stall (§5.3a)** |
| **BP token wrong or endpoint misconfigured** | **Medium** | **§14.7 classifies 401/403/404 and protocol mismatch as *terminal* — no backoff, straight to `Unavailable(reason)`. Retrying a bad token four times costs four minutes per run and teaches nothing** |
| **Stale status read between queued tasks** | **Medium** | **§5.3a: a task only executes after its originating transaction commits. Write status → commit → queue the successor, always in that order. The most likely bug in the pipeline** |
| BP MCP protocol-version mismatch | Medium | Both modules speak v2025-03-26 streamable HTTP; asserted during the readiness check, not discovered mid-batch; pin both module versions and upgrade together |
| MERA free node sleeps mid-run | Low | §5.3's 3–5s progress polling keeps it awake while a reviewer is watching. If they close the browser, queued tasks persist in the database and resume on next wake — the run parks rather than fails (§5.3a) |
| Review noise → tool ignored | Medium | Confidence surfaced in UI; per-finding "not an issue" feeding `SuppressedFinding`; advisory framing throughout |
| Agents Kit major-version churn | Low | Surface is two modules (GenAI Commons, MCP Client) behind a contract we implement; prompts, schemas and findings live in MERA's own entities |
| Non-determinism across runs | Low *(advisory)* | temperature 0; pinned model version; versioned prompts and guideline cache; `ModelQuery` log makes exploration explainable after the fact |
| Mendix Cloud cannot run the binaries | High | Accepted and designed around — the extractor sidecar (§3) |
| Long-running work in the Mendix runtime | Low | Task Queue (confirmed on free node); polled progress; concurrency semaphore |

---

## 12. Phasing

**Phase 0 — De-risk the model path (~1 day)**
Install the four modules from §3.2 into MERA at Studio Pro 11.12+. Get `anthropic-java` resolving as a managed dependency, deployed to the free node, making one live call with `output_config.format` and returning schema-valid JSON. Compile the real §8 schema against the caps in §8.2. Nothing downstream is worth building until this works, because it is now the entire model path.

**Phase 1 — Prove the pipeline (the risky part first)** ✅ *complete*
Extractor sidecar only. Given a repo URL, PAT and two SHAs, produce `ChangeUnit[]` + the Tier 1 reference graph. Validated against the real Team Server repo — see `MERA-stage8.md`.

**Phase 2 — Single layer, end to end**
Mendix app: repo registration, credential storage, run wizard, the §5.3a queues, `JA_InvokeAgent` as the GenAI Commons connector, Layer 1 only with the guideline cache, finding storage, basic report. Get one layer genuinely good before adding two more. Build the two-queue decomposition here even though only Layer 1 exists — retrofitting it later means reshaping every task.

**Phase 3 — Layers 2 and 3 + synthesis**
The BP app's MCP surface (§14), MCP Client wiring, and the §14.7 readiness gate; ADO work-item fetch and commit→work-item linking; requirements documents; Planner and Synthesizer; parallel fan-out. **The BP app's §14 work is a parallel track with its own release cycle** — start its domain model and tool contract early, because Layer 2 cannot be tuned until real practices are queryable.

**Phase 4 — Model context (§7)**
Tier 0 and Tier 1 first — they are free, deterministic, and give you the UI impact graph. **Load-test the graph against the free node's database before building on it.** Measure via `ModelQuery` whether Tier 2 is actually needed before building the workspace lease.

**Phase 5 — Make it usable**
Triage UI, false-positive suppression, the prompt test harness (§10), markdown/PDF export, cost dashboard, prompt versioning UI, run comparison.

**Phase 6 — Integrate**
Webhook/pipeline trigger on PR, comment-back to Azure DevOps. **Requires a licensed app** — a free node deploys only from Studio Pro and sleeps when idle.

> Phase 4 sits after 3 deliberately. §7 makes a good review better; it does not rescue a bad one. Get three layers producing credible findings first, then add depth.

---

## 13. Decisions

| # | Question | Decision | Consequence |
|---|---|---|---|
| 1 | BP application exposure | **MCP server, self-managed, separate app** | §14. Layer 2 talks MCP; Decision 10 settles who makes the call. |
| 2 | Mendix versions | **MPRv2 only** | No legacy SQLite-blob path; `mprcontents/`-based extraction throughout; narrower `mx` version matrix. Enforce with an explicit storage-format check at extraction. |
| 3 | Advisory or gating | **Advisory** — the lead developer owns the review | Unlocks §7's agent-driven impact analysis; shifts tuning toward recall over precision; report framed as a briefing, not a verdict. See §5.1a. |
| 4 | Whose PAT | **Individual PATs**, per MERA's existing per-project isolation | Credentials are per-user data, encrypted, owner-constrained by XPath — never constants, on any environment (§4). Leases and runs inherit the same constraint. |
| 5 | Raw request/response retention | **≤30 days; 7 recommended** — **suspended on the free node** | Scheduled purge becomes an inline end-of-run purge via `TQ_Purge` (§5.3a). No raw `FileDocument`s while on a free node. Findings and reports survive. |
| 6 | Model context for Layer 3 | **Yes — bounded query tools, for L1 and L3** | §7. Tier 0/1 free and deterministic; Tier 2 behind a leased workspace; budgets and `affectedUnits` keep it from sprawling. |
| 7 | Agent runtime: adopt or build | **Partial adoption. GenAI Commons + MCP Client only; MERA supplies its own Anthropic connector; orchestration and the §10 agent store stay custom. Agent Commons and Conversational UI skipped.** | §3.2 is the dependency list. `JA_InvokeAgent` doubles as the connector microflow, which keeps prompt caching, `web_search`, retry policy and §7.5's budgets under our control. Reasoning in `MERA-agent-layer-module-analysis.md`. |
| 8 | Hosting | **Mendix Free Apps for MERA and the BP app, limitations accepted** | Task Queue works; scheduled events do not (§5.3). 0.5 GiB database caps the Tier 1 graph (§7.3); 1 GiB files suspends raw retention (§9); concurrency drops to 2–3 (§5.3); Phase 6 deferred. **Security is unaffected** — entity access is model-level (§4). Every limit is a runtime setting, not a design assumption. |
| 9 | Output contract | **Anthropic structured outputs (`output_config.format`) with an all-required JSON Schema; `"strict": true` on every §7 tool** | Shape is guaranteed at the grammar level, so §5.2's JSON-repair round-trip is deleted. §8 rewritten all-required to stay inside the 24-optional cap. Forecloses Anthropic's Citations feature (§6). Verbatim-evidence checking is unaffected and stays. |
| 10 | Who calls the BP MCP server | **The Mendix runtime, via MCP Client — not Anthropic's server-side MCP connector** | The BP application needs no public internet exposure, only reachability from wherever MERA runs (§14.6). Costs a runtime round-trip per tool call. Pin protocol v2025-03-26. |
| **11** | **How Layer 2 tolerates a sleeping BP app** | **A dedicated `TQ_BpWakeUp` task fired before extraction, a `bpStatus` gate on batch dispatch, queue-native exponential backoff, and a wall-clock deadline** | §14.7 and §5.3a. The pipeline splits into two queues; readiness is settled before any Anthropic call for Layer 2; a terminal failure degrades to `Skipped(reason)` and `PartiallyCompleted` rather than failing the run. |

### Still open

- **Compare-against-branch mode**, or only commit range? Affects the extractor contract — cheap to add now, expensive later.
- **Where does the reference graph get its edges for `javasource/**`?** `mxcli` covers the model; Java-to-model calls (`Core.microflowCall`) are invisible to it. Probably acceptable for v1; worth a known-limitation note in the report.
- **Guideline cache curation** — who decides which refguide pages are in it, and how is staleness detected now that the weekly refresh cannot be a scheduled event?
- **Does `anthropic-java` strip unsupported JSON Schema keywords** the way the Python/TS SDKs do, or does the API reject them? Decides whether the connector needs its own strip-and-revalidate step (§8.2).
- **Does MCP Client expose a per-call HTTP timeout?** §14.7 wants a long one on the first (waking) call and a short one thereafter. If it doesn't, the readiness probe may need to be a plain REST call to the MCP endpoint rather than an MCP Client call.
- **Prompt test harness scope** (§10) — how much of Agent Commons' `{{variable}}` test surface is worth rebuilding, and when.
- **Does the MCP Server module require GenAI Commons?** (§14.2) — confirm at install; the BP app makes no LLM calls, so if it isn't required, don't add it.

---

## 14. The best-practices application

The BP app is **a separate Mendix application with its own users, own lifecycle and own release cadence**, maintained by the same person but deliberately not part of MERA. It runs on its own Free App node. Layer 2's entire authority comes from it, so its retrieval quality is the ceiling on Layer 2's usefulness — and its availability is a scheduling problem MERA has to own (§14.7).

### 14.1 Why the separation is load-bearing

- **It has its own users.** Practice authors and reviewers are not MERA users, and the editorial workflow is not a review workflow.
- **MERA is one consumer among several.** An MCP surface is equally consumable by a developer's Claude Desktop, by Studio Pro's Maia MCP client, or by a future agent. Building for MERA specifically would waste that.
- **The consequence is a rule:** no MERA concept — `ReviewRun`, `Finding`, `ChangeUnit` — may appear in the BP domain model, and no MERA-specific tool may appear on its MCP surface. **The tool contract is a versioned public API** (§14.8), not an internal call.
- It also releases on its own clock. A practice edited on Tuesday is live to Layer 2 immediately, with no MERA deployment.

### 14.2 Marketplace dependencies — BP app

| Module | Version | Why |
|---|---|---|
| **MCP Server** | 5.1.0 | The entire integration. Low-code MCP server: protocol **v2025-03-26, streamable HTTP**, microflows registered as tools via the **Add Tool** action, optional auth microflow. |
| **Community Commons** | latest | Usual utilities; likely transitive anyway. |
| **Administration** | bundled | Its own user and role management. |
| **Encryption** | latest | Only if it stores integration secrets at rest — see §14.6. |

**Studio Pro 11.12+**, the same Agents Kit 2 floor as MERA.

**Confirm at install whether MCP Server pulls GenAI Commons.** It is documented alongside Kit 2, but the BP app makes no LLM calls of its own, so if the dependency isn't real, don't add it — this app should stay small.

**Optional, only if §14.5 forces it:** PgVector Knowledge Base + an embeddings connector, for semantic search. That is the one scenario in which the BP app grows a GenAI stack, and it is a real cost — measure before reaching for it.

⚠️ The MCP Server module is explicitly **early-version and subject to breaking changes**. Pin its version here and MCP Client's version in MERA, and upgrade the pair together.

### 14.3 Domain model

```
Category ──< BestPractice ──< PracticeVersion (version, changedAt, changedBy, note)
                 │
                 ├──< Example      (kind: Good|Bad, language, snippet, caption)
                 └──< AppliesTo    (unitType, mendixVersionRange)

IntegrationClient (name, tokenHash, active, lastUsedAt) ──> System.User (service account)
```

`BestPractice` carries the fields §6 asks the tool contract to return, so the tool is a projection rather than a transformation:

| Field | Notes |
|---|---|
| `practiceId` | Stable, human-readable — `BP-SEC-014`, not a GUID. It goes in every MERA citation and must survive edits, renames and re-categorisation forever. |
| `title`, `rule`, `rationale` | `rule` is the normative sentence; `rationale` is why. A2 quotes `rule` and reasons from `rationale`. |
| `severity` | Enum on the record — **Mandatory / Recommended / Advisory**. A2's prompt maps these to Major / Minor / Info and escalates to Blocker only where the record says release-blocking. Making this a field is what stops the agent inferring it. |
| `appliesTo` | Unit types and Mendix version ranges. Lets `search_best_practices` filter rather than returning irrelevant practices the agent must discard. |
| `status` | **Draft / Published / Retired.** Only `Published` is visible over MCP. Retired stays queryable by ID so an old MERA report can still be explained. |
| `version`, `lastUpdated` | Returned in every result and stamped into the §8 citation. This is what makes a six-month-old report defensible after the standard changed. |
| `searchText` | The curated retrieval field — see §14.5. |

### 14.4 The MCP surface

Three tools, matching §6's contract:

| Tool | Input | Returns |
|---|---|---|
| `list_categories()` | — | The category tree, plus `contractVersion` (§14.8). Also MERA's readiness probe (§14.7). |
| `search_best_practices(query, categories[], mendixVersion)` | `query` free text | Up to ~10 **whole practice records**, ranked |
| `get_best_practice(practiceId)` | one ID | One whole record, for citation resolution |

Mechanics, from the module:

- Each is a microflow registered with **Add Tool**. Inputs are primitives or `MCPServer.Tool` objects.
- **Tools may return only String or TextContent.** So each returns a **JSON string** — which suits MERA fine, since A2 needs structured fields for its citations. Serialise explicitly; don't return prose.
- The module auto-generates an input schema if you don't supply one. **Supply it explicitly.** MERA marks these tools `"strict": true` (§8.2), so the schema is a compiled grammar on the client side — an auto-generated schema that shifts under you invalidates MERA's grammar cache and, worse, changes the contract silently.
- **Publish no write tools at all.** MERA allowlists on its side (§6); the server not exposing them is the second, independent control. Editorial CRUD lives in the BP app's own UI and never on the MCP surface.
- **Keep `list_categories` genuinely cheap.** It is called at the start of every review as the readiness probe, on a node that is usually cold. It should be a small, indexed read with no joins worth mentioning — not a page-sized payload.
- **Prompts:** the module supports them (single message only). Resist the temptation to serve Layer 2's system prompt from here. Prompt versioning belongs in MERA's `AgentDefinition` (§10) — splitting it across two independently released apps makes a report impossible to reconstruct.

### 14.5 Retrieval quality — the thing that decides whether Layer 2 works

§6 says design the tool contract for retrieval, not CRUD. Here is the BP-app half of that, and it is the part most likely to quietly under-deliver.

**The problem:** A2 queries in the vocabulary of *a change it just read* ("this entity's delete behaviour changed from cascading to nothing"). Practices are written in the vocabulary of *a rule* ("associations to owned data must define delete behaviour explicitly"). That gap is where Layer 2 silently returns nothing and the layer looks broken when it is actually just missing.

**v1: curated keyword search.** Maintain `searchText` as an editorial field concatenating title + rule + rationale + the Mendix terms and synonyms the practice concerns — "delete behaviour", "cascading delete", "association ownership", "referential integrity". Index it. Practices are hand-written anyway, so one more authored field costs little and does most of the work.

**Three contract rules that matter more than the ranking algorithm:**

1. **Return whole records, never bare IDs.** A second round-trip per practice doubles latency and burns MERA's tool budget (§7.5) for no gain.
2. **Cap and rank.** ~10 results. An unbounded result set is a token sink and pushes A2 toward pattern-matching rather than judging.
3. **Always include `version` and `lastUpdated`.** §8's citations depend on it.

**Upgrade path.** If recall proves poor, add embeddings via PgVector Knowledge Base in the BP app. Measure first: A2 already emits `uncoveredConcerns` and per-finding `confidence`, and those two signals tell you whether practices are missing or merely unfindable. Only the second justifies a vector database.

### 14.6 Authentication and reachability

The MCP Server module's auth hook is elegant for this: **a microflow receiving the `MCPServer` and/or `HttpRequest` object and returning a `System.User`.** The returned user's entity access then applies inside every tool microflow — so authorization is the BP app's ordinary RBAC, not bespoke logic in each tool.

- **Model MERA as an `IntegrationClient`**, bound to a dedicated **service account** with a read-only module role that can see only `Published` practices. Not a human user account, and not an administrator.
- **Token flow:** MERA's MCP Client sends bearer headers from a parameterless `GetCredentialsMicroflow` (§6). The BP app hash-compares the bearer against `IntegrationClient.tokenHash`, resolves the service user, stamps `lastUsedAt`, and rejects inactive clients. Rotatable without a deploy on either side.
- **v1 uses a long-lived rotatable token per MERA instance**, not revision 2's per-`ReviewRun` short-lived token. Minting per-run tokens needs a token endpoint on the BP app and a shared secret, which is real work that buys little while MERA's own credential store is the weaker link. Note it as a later upgrade rather than pretending it's free.
- **Reachability, not exposure.** Because the call comes from MERA's runtime (Decision 10), the BP app needs no public internet exposure — but it does need to be reachable *from wherever MERA runs*. That is Jord's machine during local development and Mendix Cloud when MERA runs on the free node. Those are different network positions; decide the BP app's placement against the harder of the two.
- Rate-limit the service account. An agent in a loop is exactly the client that will discover you didn't.

### 14.7 Readiness, wake-up and retry

Both apps are free nodes, so this is a designed sequence rather than a caveat.

**The problem, precisely.** A free node sleeps after about an hour of inactivity, and a best-practices catalogue is low-traffic — it will normally be asleep when a review starts. Waking takes roughly a minute. Crucially, **a sleeping node does not fail fast**: the first request hangs while the node boots and then usually succeeds. So the failure mode is not an error, it is a stall. If that stall lands inside `JA_InvokeAgent`'s tool loop, MERA has already paid for the batch's input tokens and is holding one of two or three executor threads on a 0.5 vCPU node while it waits.

**Hence the rule:** the BP app is woken and verified **before any Anthropic call for Layer 2** — never inside the tool loop (§5.2, §6).

**Use the free minute.** Extraction takes ~35s against the real repo and the Planner adds a few seconds. That is wall-clock MERA is already spending, during which nothing needs the BP app. `TQ_BpWakeUp` therefore fires *first*, before extraction (§5.3a), and by the time Layer 2 batches exist the node is normally `Ready`. Waking at Layer 2 time and then waiting would spend a minute that was already available for free.

**`list_categories` does three jobs in one call** — which is why it is the probe and not a bespoke health endpoint:

1. it wakes the node;
2. it verifies reachability, the bearer token, and the protocol version, before anything expensive;
3. it returns `contractVersion` (§14.8), stamped onto the `ReviewRun`.

It takes no arguments and should stay cheap (§14.4).

**Classify the failure — this saves more time than the backoff does.**

| Outcome | Class | Action |
|---|---|---|
| Timeout, connection refused, 502 / 503 / 504 | **Retryable** — node is waking | Back off and retry |
| 401 / 403 | **Terminal** — token wrong, revoked, or client deactivated | No retry → `Unavailable(auth)` |
| 404 / wrong endpoint | **Terminal** — misconfiguration | No retry → `Unavailable(config)` |
| Protocol mismatch, or an empty tool list | **Terminal** — §14.4 | No retry → `Unavailable(contract)` |

Retrying a bad token four times with backoff costs four minutes on *every run* and teaches you nothing. Classify first; only then decide whether backoff applies.

**State on `ReviewRun`:** `bpStatus: NotStarted → Waking → Ready | Unavailable`, plus `bpUnavailableReason`, `bpFirstContactAt` and `bpWakeDeadline`.

**`TQ_BpWakeUp(ReviewRun)`** — queue `mera-run`:

```
set bpStatus = Waking, bpWakeDeadline = now + 5 min   → commit
call list_categories
  success   → bpStatus = Ready, stamp bpContractVersion,
              bpFirstContactAt = now                   → commit
  terminal  → bpStatus = Unavailable, reason           → commit
  retryable → if now > bpWakeDeadline:
                 bpStatus = Unavailable, reason=timeout → commit
              else: THROW  → the queue re-runs it with backoff
```

**Configuration:**

| Setting | Value | Why |
|---|---|---|
| First-attempt call timeout | **60s** | This attempt is *doing* the waking. A short timeout kills a wake in progress and guarantees a wasted second attempt. |
| Later-attempt timeout | 20s | By then the node should be up; a slow response means something else is wrong. |
| Retry strategy | **exponential backoff** | Queue-native; no hand-rolled waiting. |
| Initial delay | 15s | Attempts land at roughly t≈0, +15s, +45s, +105s. |
| Max attempts | 4 | Comfortably covers a ~1-minute wake with margin. |
| `bpWakeDeadline` | run start + **5 min** | Wall clock beats attempt arithmetic — it is what a human cares about and what the UI shows. Checked on every attempt, so a deadline reached at attempt 2 stops there. |

> **Open:** whether MCP Client exposes a per-call HTTP timeout. If it doesn't, make the readiness probe a plain REST call to the MCP endpoint and keep MCP Client for the actual tool traffic. Listed in §13 Still open.

**`TQ_Layer2Dispatch(ReviewRun)`** — queue `mera-run`, the gate:

```
Ready       → queue TQ_ReviewBatch × M for Layer 2  → mera-batch
Waking      → THROW → queue re-runs with backoff (10s initial, max 6 attempts)
Unavailable → mark every Layer 2 ReviewBatch Skipped(bpUnavailableReason);
              run finishes PartiallyCompleted
```

Throwing to re-queue rather than sleeping in the microflow is the whole point: **the executor thread is released between attempts**, so Layer 1 and Layer 3 batches keep running on `mera-batch` while Layer 2 waits. On a single-instance node with two or three threads, a sleep-loop here would stall the entire review.

**One wake-up task, not one per batch.** The gate is a single task that fans out once ready. If each Layer 2 batch retried independently against a cold node, a dozen batches would produce a dozen backing-off tasks and a retry storm against an app that is trying to boot.

**Degradation is honest, not silent.** `Unavailable` produces `Skipped(reason)` on the Layer 2 batches, `PartiallyCompleted` on the run, and an explicit line in A4's `notReviewed` — its prompt says an unreviewed layer is not a clean one. The reviewer must be able to see that the best-practices layer did not run.

**Two smaller notes.** The BP app could fall asleep again *during* a long review, but Layer 2's batches run close together and every tool call resets the idle timer, so the exposure is one wake per run in practice. And the BP app's own users waking it during working hours means the probe often returns immediately — the cold path is the exception, not the rule, which is another reason not to over-engineer beyond this.

### 14.8 Versioning the contract

Two independently released apps share an API, so treat it like one:

- **Additive changes only.** Never repurpose or retype a field; add a new one and deprecate the old.
- **Stamp `contractVersion` in `list_categories`' response**, and have MERA record it on the `ReviewRun` — which the readiness probe does for free on every run (§14.7). When a report from March cites `BP-SEC-014 v3`, you can reconstruct exactly what the tool returned.
- **Never delete a `practiceId`.** Retire it — MERA's citations are permanent references.
- Pin the MCP Server and MCP Client module versions and upgrade them as a pair; the modules are early-version and the protocol is still moving.

---

## Sources

- [mx Command-Line Tool](https://docs.mendix.com/refguide/mx-command-line-tool/) · [Merging and Diffing Commands](https://docs.mendix.com/refguide/mx-command-line-tool/merge/) · [MPR Analyze](https://docs.mendix.com/refguide/mx-command-line-tool/analyze-mpr/)
- [mxcli (mendixlabs)](https://github.com/mendixlabs/mxcli) · [mxcli documentation](https://www.mxcli.org/) · [MPR File Format](https://www.mxcli.org/internals/mpr-format.html) · [mxlint-cli (cinaq)](https://github.com/cinaq/mendix-cli)
- [Working with External Git Tools](https://docs.mendix.com/refguide/version-control-external-tools/) · [Set Up your PAT](https://docs.mendix.com/apidocs-mxsdk/mxsdk/set-up-your-pat/)
- [Managed Dependencies](https://docs.mendix.com/refguide/managed-dependencies/) · [Task Queue](https://docs.mendix.com/refguide/task-queue/) · [Mendix Cloud — Free App limitations](https://docs.mendix.com/developerportal/deploy/mendix-cloud-deploy/) · [cf-mendix-buildpack](https://github.com/mendix/cf-mendix-buildpack/blob/master/README.md)
- [Agents Kit 2](https://docs.mendix.com/agents/agents-kit-2/) · [GenAI Commons](https://docs.mendix.com/agents/agents-kit-2/reference-guide/commons/) · [MCP Client](https://docs.mendix.com/agents/agents-kit-2/reference-guide/mcp-modules/mcp-client/) · [MCP Server](https://docs.mendix.com/agents/agents-kit-2/reference-guide/mcp-modules/mcp-server/) · [Model Context Protocol in Mendix](https://docs.mendix.com/agents/mcp/) · [Tool Calling](https://docs.mendix.com/agents/function-calling/) · [Build Your Own GenAI Connector](https://docs.mendix.com/agents/agents-kit-2/how-to/byo-connector/)
- [Structured outputs](https://platform.claude.com/docs/en/build-with-claude/structured-outputs) · [Web search tool](https://platform.claude.com/docs/en/agents-and-tools/tool-use/web-search-tool) · [anthropic-sdk-java](https://github.com/anthropics/anthropic-sdk-java)
- [Azure DevOps Work Items REST API 7.1](https://learn.microsoft.com/en-us/rest/api/azure/devops/wit/work-items/get-work-item?view=azure-devops-rest-7.1)