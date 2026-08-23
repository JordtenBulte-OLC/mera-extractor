# MERA Redesign — Architecture & Feasibility

**Mendix Expert Reviewing Application · multi-agent redesign**
Date: 2026-08-20 · Revision 2 · Status: design proposal · Author: Jord + Claude

> **Revision 2** folds in the six decisions from §13 and adds §7, model context and impact analysis.

---

## 1. Verdict up front

The redesign is feasible, and the multi-agent shape you describe is the right shape. Three things need to be decided consciously, because they are where this design either holds up or quietly breaks:

| # | Question | Answer |
|---|---|---|
| 1 | Can Mendix fetch the repo itself with a user PAT? | **Yes.** Team Server exposes `https://git.api.mendix.com/<AppID>.git` with PAT auth. Use **JGit** as a managed Java dependency — no `git` binary needed. |
| 2 | Can the orchestration + agent loops live in Java actions? | **Yes.** The official `com.anthropic:anthropic-java` SDK is on Maven Central and installs via Studio Pro managed dependencies. Server-side `web_search` and the MCP connector mean Layer 1 and Layer 2 need **no client-side tool plumbing at all**. |
| 3 | Can `mxcli` / `mx diff` run inside the Mendix runtime? | **No — not on Mendix Cloud.** These are native binaries. The CF buildpack ships JRE + Runtime + nginx only, has no package manager, and the container filesystem is ephemeral. This is the one hard constraint in the design. |

**Consequence:** MERA becomes *one Mendix app + one extraction sidecar*. Everything you asked for — orchestration, agent definitions, review logic, UI, audit — stays in Mendix. Only the "turn commits into readable model text, and answer model queries" service runs in a container that is allowed to have `git`, `mxcli` and `mx` on its PATH.

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

```
┌──────────────────────────────────────────────────────────────────┐
│  MERA (Mendix app)                                               │
│                                                                  │
│  UI: repo/PAT registration · run wizard · live progress ·        │
│      finding triage · impact graph · report export               │
│                                                                  │
│  Domain: ReviewRun, ChangeUnit, ReviewBatch, Finding,            │
│          ModelUnit, ModelReference, ModelQuery,                  │
│          AgentInvocation, Credential(encrypted)                  │
│                                                                  │
│  Orchestration: Task Queue → microflow pipeline                  │
│  Java actions:                                                   │
│    JA_ResolveCommits      (JGit — list/validate commit range)    │
│    JA_RequestExtraction   (REST → Extractor)                     │
│    JA_LeaseWorkspace      (open/close extractor session, §7)     │
│    JA_InvokeAgent         (Anthropic SDK, tool loop, retries)    │
│    JA_EstimateTokens                                             │
│  Microflow tools (§7): find_callers · find_references ·          │
│                        impact_radius   [over ModelReference]     │
└───────────────┬──────────────────────────────┬───────────────────┘
                │ HTTPS + mTLS/shared secret   │ HTTPS
                ▼                              ▼
┌──────────────────────────────┐   ┌──────────────────────────────┐
│  MERA Extractor (sidecar)    │   │  Anthropic Messages API      │
│  Docker: git + mx + mxcli    │   │  · server-side web_search    │
│                              │   │  · MCP connector → BP app    │
│  POST /extract               │   │  · prompt caching            │
│   → clone (PAT, shallow)     │   └──────────────────────────────┘
│   → checkout base & head     │
│   → mx diff → manifest       │   ┌──────────────────────────────┐
│   → mxcli describe per unit  │   │  Azure DevOps REST 7.1       │
│   → mxcli refs → ref graph   │   │  (work items — Layer 3)      │
│   → git diff text artifacts  │   └──────────────────────────────┘
│   → return ChangeUnit[] +    │
│     ModelUnit[]/Reference[]  │
│                              │
│  POST   /workspaces      ─┐  │  leased session, TTL-bounded,
│  POST   /workspaces/{id}/ │  │  holds BOTH base and head .mpr
│           describe        │  │  → mxcli describe / search
│  DELETE /workspaces/{id} ─┘  │  → wipe on close or expiry
└──────────────────────────────┘
```

**Why the sidecar stays dumb.** It holds no review state, makes no decisions, has no LLM access. Its jobs are: (a) given `(repoUrl, PAT, baseSha, headSha)` return `ChangeUnit[]` plus the reference graph; (b) hold a TTL-bounded workspace that can answer `describe`/`search` against the base and head models. All intelligence stays in Mendix, which is what you wanted.

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

### PAT handling — non-negotiables
*(Decision 4: individual PATs, matching MERA's existing per-project ownership model. This is the right call — it keeps least privilege, gives correct attribution on every run, and requires no change to your existing access model.)*

| Rule | Implementation |
|---|---|
| Never store plaintext | Encrypt at rest with the Encryption module; key from a runtime constant / cloud secret, **not** in the model |
| Never log | Explicit redaction in the extractor's request logging; add a log-scrubber test |
| Never render | No PAT-valued attribute on any page; write-only input, show `••••••••` + last 4 |
| Scope to the user | `Credential` owned by a `User`, XPath-constrained access rule inheriting MERA's existing project-level isolation; a run records *whose* PAT it used |
| Bounded lifetime | Store `expiresAt`, warn at T-14d, block runs on expired |
| In transit | PAT travels Mendix → Extractor only. mTLS or signed short-lived request token; never as a URL parameter |
| In memory | Extractor holds it for the clone only, writes it to a git credential helper on tmpfs, wipes the workspace in a `finally` |

Because PATs are per-user, a workspace lease (§7) is also per-user — a leased workspace must carry the same access constraint as the `ReviewRun` that opened it, so a second user cannot query a model they have no rights to.

**The honest risk statement:** you are asking users to hand a repository credential to a service that pipes derived content to a third-party LLM API. That is legitimate, but it needs to be *said out loud in the UI*: what is sent, to whom, retained for how long. Add a per-repository `Consent` record with timestamp.

---

## 5. Orchestration inside Mendix

### 5.1 Make the orchestrator code, not a model — with one LLM exception

You described "a Claude orchestrator agent with 3 agents". There are two ways to build that:

**(a) Agentic orchestrator** — one Claude conversation whose tools are `run_layer1`, `run_layer2`, `run_layer3`, each implemented in Java as a nested conversation.
**(b) Deterministic pipeline** — Mendix runs the three layers, then a synthesizer.

**Recommend (b), with an LLM Planner pre-pass.** Reasoning:

- Fan-out is trivially parallel. Java `CompletableFuture` over three independent calls beats a sequential tool loop on latency and cost.
- You lose nothing: the intelligence you actually want from an orchestrator is *scoping* — deciding which changes matter and what each layer should focus on. That is one cheap call, not a control loop.
- Failure handling, retries, partial results and audit are dramatically simpler when the control flow is a microflow you can look at.
- Reproducibility. This mattered less once you confirmed reviews are **advisory** *(Decision 3)* — see below — but a deterministic control flow is still the right default for a tool people run twenty times a day.

So: **Planner (LLM, cheap model) → deterministic fan-out to L1/L2/L3 (parallel, batched) → Synthesizer (LLM) → Report.**

### 5.1a What "advisory" buys you

Decision 3 — a human lead developer owns the review, MERA accelerates it — is more architecturally significant than it looks. It means:

- **Agent-driven exploration becomes acceptable.** A gating tool must produce identical findings on identical input; that argues hard against letting agents wander the model. Advisory output does not carry that burden, which is precisely what makes §7's on-demand impact analysis viable. Take the win.
- **Recall beats precision.** A missed defect costs more than a false positive, because a human is filtering anyway. Tune severity thresholds and Planner filtering toward inclusion, and lean on the confidence field plus UI triage rather than suppressing at generation time.
- **The report's job changes.** It is a briefing for a reviewer, not a verdict. Keep the Synthesizer's `verdict` field — it is a useful summary signal — but the UI should frame it as "MERA suggests" and put the *reasoning* and *citations* front and centre, since the human needs to judge them, not obey them.
- **Design for disagreement.** Every finding needs a one-click "not an issue" that feeds a per-repository suppression list. That list is also your best long-term quality metric.

### 5.2 The generic agent runner

Build **one** Java action, `JA_InvokeAgent`, and reuse it for all five agents. Signature:

```
JA_InvokeAgent(
  AgentInvocation invocation   // holds agentKey, model, systemPrompt,
                               // userContent, toolConfigJson, maxTokens,
                               // temperature, cacheBreakpoints,
                               // toolBudgetJson, workspaceId?
) : AgentInvocation             // filled with rawResponse, parsedJson,
                                // stopReason, in/outTokens, cost, latency
```

Inside:
1. Build the request with the Anthropic Java SDK.
2. Attach tools per agent config (see §6, §7).
3. Run the **tool loop** until `stop_reason != tool_use`, capped at N iterations. With server-side `web_search` and the MCP connector, L2 needs no loop at all — Anthropic executes those tools server-side. The loop exists for the **Mendix-local model-context tools** in §7, which L1 and L3 use.
4. Validate the response against the agent's JSON schema. On failure, one repair round-trip, then fail the batch.
5. Persist an `AgentInvocation` row **always**, success or failure. Audit trail, cost report and debugging surface in one.

Retries: exponential backoff on 429/529, jitter, `Retry-After` honoured, max 4. Idempotency: a `ReviewBatch` carries a UUID so a re-run of a partially failed pipeline doesn't duplicate findings.

### 5.3 Async and progress

Reviews take minutes. Do **not** run them in a request handler.

- Kick off with **Mendix Task Queue**; the run microflow is a queued task.
- `ReviewRun.status`: `Draft → Extracting → Planning → Reviewing → Synthesizing → Completed | Failed | PartiallyCompleted`
- `ReviewRun.progress` (0–100) plus a `ReviewBatch` per unit-of-work; the UI polls with a refresh-timer nanoflow every 3–5s. Mendix has no clean server-push; polling is the pragmatic answer.
- Run batches concurrently but **cap it** — a semaphore of 4–6 concurrent Anthropic calls per run, and a global cap.
- `PartiallyCompleted` is a real state: if Layer 2's MCP server is down, you still ship Layer 1 + 3 findings and mark Layer 2 `Skipped(reason)`. Never silently drop a layer.
- A scheduled event closes expired workspace leases (§7) — belt and braces against orphaned clones holding PAT-derived data.

### 5.4 Batching and token budget

A single sprint commit range can touch hundreds of units. Strategy:

1. **Filter** — Planner marks each ChangeUnit `review | skim | ignore`.
2. **Group** — batch by module, keeping related units together. Target **40–80k input tokens per batch**, leaving headroom for §7 tool results.
3. **Cache** — the layer system prompt, the guideline corpus and the requirements document are stable across batches within a run. Put cache breakpoints after them. On a 20-batch run this is the single biggest cost lever.
4. **Cheap model for the cheap jobs** — Planner on a small model; L1–L3 and Synthesizer on the frontier model.
5. **Hard ceiling** — a `maxSpend` on `ReviewRun`, checked before each batch, stopping cleanly at `PartiallyCompleted`.

---

## 6. Agent topology and tool surface

| Agent | Model | Tools | Context it gets |
|---|---|---|---|
| **A0 Planner** | small | none | Change manifest (names + types + change kinds only, no bodies) |
| **A1 Guidelines** | frontier | `web_search` (Mendix domains only) · **model-context tools (§7)** | ChangeUnit batch + curated guideline index |
| **A2 Best Practices** | frontier | MCP connector → your BP application | ChangeUnit batch + BP taxonomy |
| **A3 Functional** | frontier | **model-context tools (§7)** | ChangeUnit batch + work item(s) + requirements doc |
| **A4 Synthesizer** | frontier | none | All findings + change manifest (no raw model bodies) |

### Layer 1 — web search, but constrained
Server-side `web_search` (`web_search_20250305` or later) with `allowed_domains` locked to Mendix-official sources, `max_uses` 5–8 per batch, citations always on. $10/1k searches — meaningful at scale, so:

> **Build a guideline cache.** Most Mendix guidelines are stable. Crawl and store the relevant refguide pages as a `GuidelineDocument` entity in Mendix, refresh weekly, and inject the relevant subset into the prompt. Web search then becomes the *fallback*, not the primary retrieval path. This cuts cost and latency and makes runs reproducible. Version the cache and stamp `ReviewRun.guidelineCacheVersion`.

### Layer 2 — MCP connector, confirmed
*(Decision 1: the BP application is self-managed and will expose an MCP server.)* This is the clean path — no client-side tool plumbing, no REST fallback needed.

```json
"mcp_servers": [{
  "type": "url",
  "url": "https://bp.internal.example.com/mcp",
  "name": "mera-bestpractices",
  "authorization_token": "<short-lived token>"
}],
"tools": [{
  "type": "mcp_toolset",
  "mcp_server_name": "mera-bestpractices",
  "default_config": {"enabled": false},
  "configs": {
    "search_best_practices": {"enabled": true},
    "get_best_practice":     {"enabled": true},
    "list_categories":       {"enabled": true}
  }
}]
```
Beta header `mcp-client-2025-11-20`. Design constraints to build the BP MCP server against:

- **Streamable HTTP or SSE, HTTPS, publicly reachable by Anthropic.** No stdio. Since you own the server, put it behind an IP allowlist plus short-lived bearer tokens minted per `ReviewRun` — the `authorization_token` field takes them directly.
- **Tools only.** MCP prompts and resources are not supported through the connector. Anything you were tempted to model as a resource (the practice catalogue, category tree) must be a *tool* that returns it.
- **Allowlist explicitly** with `default_config.enabled: false`, so a future write tool on that server can never be invoked by a reviewer agent. Do this from day one even if the server is read-only today.
- **Design the tool contract for retrieval quality, not CRUD.** `search_best_practices(query, categories[], mendixVersion?)` should do semantic or keyword search over practice *text* and return `{id, title, rule, rationale, severity, appliesTo, examples}`. Returning a bare list of IDs forces a second round-trip per practice and doubles latency.
- **Return severity from the record.** The A2 prompt inherits severity from the practice; make that a real field rather than something the agent has to infer.
- **Include a `version` and `lastUpdated` per practice** and stamp them into the citation, so an old report can be explained after the standards change.

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

For a large app the graph is maybe 5–10k units and 50–150k edges. Trivial for Mendix to store and index. Retain it only as long as the run's raw artifacts (Decision 5).

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
- TTL bounded to the run (say 30 min), hard-expired by a scheduled event in Mendix *and* a reaper in the sidecar. Never rely on only one.
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

These matter more than the tools themselves.

- **Per-batch budgets, enforced in `JA_InvokeAgent`.** Suggested starting point: **15 graph calls** (Tier 0/1, cheap) and **6 body retrievals** (Tier 2, expensive) per batch for L3; roughly half that for L1. Tune from the `ModelQuery` data after a few weeks.
- **Budget exhaustion returns a tool result, not an error**: `{"budgetExhausted": true, "remaining": 0}`. The agent then concludes gracefully instead of derailing.
- **Hard depth cap of 3** in the microflow, regardless of what the agent asks for.
- **Truncate wide results.** Cap caller lists at ~40 with `"and 312 more in 18 modules"`. A unit called from 300 places is itself the signal; the list is noise and a token sink.
- **Findings still anchor to changed units.** An impact finding's `qualifiedName` is the *changed* unit. The affected consumers go in a new `affectedUnits[]` field. This is what stops the review sprawling into untouched code that nobody in this commit touched.
- **Cap total tool tokens per batch** (say 25k) alongside the call count — one `describe_unit` on a monster page can blow the batch budget on its own.

### 7.6 Focusing the budget: Planner drives it

The Planner already classifies every unit and knows `changeKind`. Have it emit `impactAnalysisRequired: bool` per unit:

**True for** — `Deleted` anything · `Modified` with a signature change (attribute removed/renamed/retyped, association changed, microflow parameter or return type changed, published REST/OData contract changed, entity access rule changed, delete behaviour changed) · anything already on `highRiskUnits`.

**False for** — `Added` units (nothing depends on them yet) · pure-UI page tweaks · anything marked `skim` or `ignore`.

This is the cheapest possible targeting: one small-model call decides where the expensive budget gets spent, and most runs skip Tier 2 entirely.

### 7.7 The escape hatch if L3's context blows up

If the exploration turns out to consume too much of L3's context — plausible on large change sets — promote it to a **pre-pass**: an `A5 Impact Analyst` that runs once per `impactAnalysisRequired` unit, does the traversal, and emits a compact **Impact Brief** (≤300 words: who consumes this, which of those paths the change plausibly affects, what to verify). Inject that brief into both L1 and L3 as static context.

Trade-off: deterministic and paid once and shared by two layers, but not targeted by the reviewing agent's actual concern. Start with tools on L1/L3; keep the ChangeUnit shape ready for a `impactBrief` field so this is a config change rather than a rewrite.

---

## 8. Finding schema — the contract that holds it all together

Every layer emits the same object. This is what makes synthesis, storage, UI and metrics possible.

```jsonc
{
  "findingId": "L3-0004",
  "layer": 3,                        // 1 | 2 | 3
  "severity": "Major",               // Blocker | Major | Minor | Info
  "confidence": 0.78,
  "title": "Confirmation flow no longer notifies customer on partial shipment",
  "module": "Sales",
  "unitType": "Microflow",
  "qualifiedName": "Sales.ACT_Order_Confirm",   // the CHANGED unit
  "location": "removed call to 'SUB_NotifyCustomer'",
  "changeAnchor": "Modified",
  "affectedUnits": [                 // §7 — impacted, but NOT changed
    {"qualifiedName": "Sales.SUB_PartialShipment",
     "referenceKind": "Calls",
     "note": "sole remaining caller; relies on notification side effect"}
  ],
  "evidence": "…verbatim excerpt from the MDL/diff…",
  "rationale": "…why this is a problem, in reviewer voice…",
  "citations": [                     // MANDATORY for layers 1 and 2
    {"kind":"mendix-doc","title":"Microflow best practices",
     "url":"https://docs.mendix.com/refguide/…","quote":"…"}
  ],
  "recommendation": "…",
  "effort": "S",                     // S | M | L
  "falsePositiveRisk": "Low"
}
```

**Three rules that decide whether MERA is trusted or ignored:**

1. **No citation, no finding (L1/L2).** If the agent cannot point to a documented guideline or a BP record, drop it or reclassify as `Info` with an explicit `"unverified"` flag. Nothing kills a review tool faster than confidently invented rules.
2. **Anchor to the change.** Findings reference a ChangeUnit in this range; impacted-but-unchanged units go in `affectedUnits`, pre-existing problems go in `observations` below the fold. Reviewers get furious when a tool flags code they didn't write.
3. **Evidence must be verbatim** — from the batch input or from a §7 tool result. This is cheaply verifiable in post-processing: reject findings whose `evidence` string doesn't appear in either. That one mechanical check catches most hallucinations.

---

## 9. Domain model sketch

```
Repository ──< Credential (encrypted PAT, owner, scope, expiresAt)
    │
    ├──< RequirementDocument (version, content, activeFrom)
    ├──< SuppressedFinding (fingerprint, reason, suppressedBy, at)
    └──< ReviewRun
            ├─ baseCommit, headCommit, branch, status, progress,
            │  startedAt, finishedAt, totalInputTokens, totalOutputTokens,
            │  estimatedCost, maxSpend, guidelineCacheVersion, initiatedBy,
            │  workspaceId, workspaceExpiresAt
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
            │                     requestId, stopReason, rawRequest,
            │                     rawResponse, cost, latencyMs)
            └──1 SynthesisReport (executiveSummary, verdict, markdown, pdf)
```

**Retention** *(Decision 5)*: `rawRequest`/`rawResponse` as `FileDocument`, purged by a scheduled event. 30 days is the ceiling; **7 days is a better default** — you debug from recent runs, not last month's, and less retained data is less liability. Make it a runtime constant so you can dial it without a deploy. Purge `ModelUnit`/`ModelReference`/`ModelQuery` on the same clock. `Finding` and `SynthesisReport` are the durable output and survive the purge; strip `evidence` at purge time if you want the reports without the model excerpts.

---

## 10. Agent definitions

Store these as `AgentDefinition` records in Mendix (agentKey, version, model, systemPrompt, toolConfig, outputSchema, isActive) rather than hardcoding them in microflows. Prompt iteration is the main tuning loop of this product — you want to change a prompt without a redeploy, and `ReviewRun` records which prompt *version* produced a report.

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

**Output schema:** `{ verdicts: [{qualifiedName, verdict, reason, impactAnalysisRequired}], focusNotes: {layer1, layer2, layer3}, highRiskUnits: [string], estimatedBatches: int }`

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
the limitation in the finding's rationale.

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
   "unverified": true, or drop it.

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

## Severity calibration
  Blocker — will break in production, loses data, or opens a security hole.
  Major   — significant performance, maintainability or correctness risk.
  Minor   — real but contained; worth fixing, not worth blocking.
  Info    — observation, or a finding you could not fully ground.

Set `confidence` honestly. 0.9+ means you would defend this in a review meeting.
Below 0.6 means you are flagging it for a human to judge. A human lead developer
reads everything you produce and makes the final call, so a well-reasoned
uncertain finding is worth more than silence.

Output only JSON matching the supplied schema.
```

---

### A2 — Best Practices Reviewer (Layer 2)

**Model:** frontier · **Tools:** MCP toolset → best-practices application (read tools only) · **Max tokens:** 16k

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
  advice. Return `"toolFailure": true` with an explanation and an empty findings
  array. A skipped layer is honest; a substituted layer is misleading.

## Severity
Inherit severity from the best-practice record where it defines one. Where it
does not, map: mandatory → Major, recommended → Minor, advisory → Info.
Escalate to Blocker only where the practice itself says the violation is
release-blocking.

Output only JSON matching the supplied schema.
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
  find_references(qualifiedName, referenceKind?)     — everything that uses it
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
- If no work item was supplied, return `"noRequirements": true`, leave coverage
  empty, and report only scope observations describing what the change appears
  to do. Never invent requirements to review against.

## Tone
Write findings as questions where you are inferring intent, and as statements
where the artefacts are unambiguous. You are the reviewer who catches "the
criteria said 'notify the customer' and the only notification path was removed"
— that is worth more than any style comment. A human lead developer reads your
output and decides; give them the reasoning, not just the verdict.

Output only JSON matching the supplied schema.
```

**Additional output fields:** `coverage: [{criterion, verdict, implementingUnit, note}]`, `requirementQuality: {score, issues[]}`, `noRequirements: bool`, `reviewLimitations: string[]`

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
6. **Note what was not reviewed.** Skipped layers, ignored units, exhausted
   tool budgets, low-confidence areas, missing requirements, degraded impact
   analysis. The reader must know the edges of the review to trust the middle.

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

Output JSON matching the supplied schema, with `markdownReport` containing the
full human-readable review.
```

---

## 11. Risks and mitigations

| Risk | Severity | Mitigation |
|---|---|---|
| `mxcli` is alpha; fails or misreads a model version | High | Read-only on disposable clones; pin version; compatibility check before run; degrade to `mx diff`-only with a visible quality warning |
| Mendix version skew between repo and extractor's `mx` | High | Read model version from the manifest first; `mx diff -l`; maintain a Studio Pro version matrix in the image; fail loudly rather than diff wrongly |
| Hallucinated guidelines presented authoritatively | High | No-citation-no-finding; mechanical verbatim-evidence check across batch input *and* tool results; citations rendered clickable in the UI |
| PAT compromise | High | §4 controls; read-only scope; encrypted at rest; never logged; short-lived transport token; per-user ownership carried onto workspace leases |
| **Impact analysis balloons cost/context** | **Medium** | **§7.5 budgets; Planner-driven targeting; graph tools free and preferred; result truncation; total tool-token cap per batch; §7.7 pre-pass escape hatch** |
| **Orphaned workspace lease holds a clone + credentials** | **Medium** | **TTL on creation; reaper in the sidecar *and* a Mendix scheduled event; wipe on close; lease opened lazily and only when needed** |
| **Review sprawls into untouched code** | **Medium** | **`affectedUnits` instead of standalone findings; findings must anchor to a ChangeUnit; enforced in post-processing, not just in the prompt** |
| Cost runaway on a large commit range | Medium | Planner filtering; prompt caching; `maxSpend` per run; pre-run cost estimate before the user confirms |
| Huge change sets exceed context | Medium | ChangeUnit batching with token estimates; per-batch budget; Synthesizer never sees raw model |
| BP MCP endpoint must be reachable by Anthropic | Medium | IP allowlist + per-run short-lived bearer tokens; read-only tool allowlist with `default_config.enabled: false` |
| Review noise → tool ignored | Medium | Confidence surfaced in UI; per-finding "not an issue" feeding `SuppressedFinding`; advisory framing throughout |
| Non-determinism across runs | Low *(advisory)* | temperature 0; pinned model version; versioned prompts and guideline cache; `ModelQuery` log makes exploration explainable after the fact |
| Mendix Cloud cannot run the binaries | High | Accepted and designed around — the extractor sidecar (§3) |
| Long-running work in the Mendix runtime | Low | Task Queue; polled progress; concurrency semaphore |

---

## 12. Phasing

**Phase 1 — Prove the pipeline (the risky part first)**
Extractor sidecar only. Given a repo URL, PAT and two SHAs, produce `ChangeUnit[]` + the Tier 1 reference graph. Validate against three real MPRv2 repos of different Mendix versions. **Do not write a line of agent code until this is solid** — everything else is conventional work; this is where the project actually fails.

**Phase 2 — Single layer, end to end**
Mendix app: repo registration, credential storage, run wizard, Task Queue, `JA_InvokeAgent`, Layer 1 only with the guideline cache, finding storage, basic report. Get one layer genuinely good before adding two more.

**Phase 3 — Layers 2 and 3 + synthesis**
BP MCP server and connector wiring; ADO work-item fetch and commit→work-item linking; requirements documents; Planner and Synthesizer; parallel fan-out.

**Phase 4 — Model context (§7)**
Tier 0 and Tier 1 first — they are free, deterministic, and give you the UI impact graph. Measure via `ModelQuery` whether Tier 2 is actually needed before building the workspace lease. It may turn out the graph alone answers most questions, in which case you have saved yourself a session lifecycle.

**Phase 5 — Make it usable**
Triage UI, false-positive suppression, PDF/markdown export, cost dashboard, prompt versioning UI, run comparison.

**Phase 6 — Integrate**
Webhook/pipeline trigger on PR, comment-back to Azure DevOps.

> Phase 4 sits after 3 deliberately. §7 makes a good review better; it does not rescue a bad one. Get three layers producing credible findings first, then add depth.

---

## 13. Decisions

| # | Question | Decision | Consequence |
|---|---|---|---|
| 1 | BP application exposure | **MCP server, self-managed** | Layer 2 uses the MCP connector; no client-side REST fallback needed. Build the BP MCP server to the constraints in §6 — HTTPS/Streamable-HTTP, tools-only, allowlisted, severity and version on every record. |
| 2 | Mendix versions | **MPRv2 only** | No legacy SQLite-blob path; `mprcontents/`-based extraction throughout; narrower `mx` version matrix. Enforce with an explicit storage-format check at extraction. |
| 3 | Advisory or gating | **Advisory** — the lead developer owns the review | Unlocks §7's agent-driven impact analysis; shifts tuning toward recall over precision; report framed as a briefing, not a verdict. See §5.1a. |
| 4 | Whose PAT | **Individual PATs**, per MERA's existing per-project isolation | No change to the existing access model. Leases and runs inherit the same constraint (§4). |
| 5 | Raw request/response retention | **≤30 days; 7 recommended** | Scheduled purge, retention as a runtime constant. Findings and reports survive; strip `evidence` at purge if you want reports without model excerpts. |
| 6 | Model context for Layer 3 | **Yes — bounded query tools, for L1 and L3** | New §7. Tier 0/1 free and deterministic; Tier 2 behind a leased workspace; budgets and `affectedUnits` keep it from sprawling. |

### Still open

- **Compare-against-branch mode**, or only commit range? Affects the extractor contract — cheap to add now, expensive later.
- **Where does the reference graph get its edges for `javasource/**`?** `mxcli` covers the model; Java-to-model calls (`Core.microflowCall`) are invisible to it. Probably acceptable for v1; worth a known-limitation note in the report.
- **Guideline cache curation** — who decides which refguide pages are in it, and how is staleness detected beyond a weekly refresh?

---

## Sources

- [mx Command-Line Tool](https://docs.mendix.com/refguide/mx-command-line-tool/) · [Merging and Diffing Commands](https://docs.mendix.com/refguide/mx-command-line-tool/merge/) · [MPR Analyze](https://docs.mendix.com/refguide/mx-command-line-tool/analyze-mpr/)
- [mxcli (mendixlabs)](https://github.com/mendixlabs/mxcli) · [mxcli documentation](https://www.mxcli.org/) · [MPR File Format](https://www.mxcli.org/internals/mpr-format.html) · [mxlint-cli (cinaq)](https://github.com/cinaq/mendix-cli)
- [Working with External Git Tools](https://docs.mendix.com/refguide/version-control-external-tools/) · [Set Up your PAT](https://docs.mendix.com/apidocs-mxsdk/mxsdk/set-up-your-pat/)
- [Managed Dependencies](https://docs.mendix.com/refguide/managed-dependencies/) · [cf-mendix-buildpack](https://github.com/mendix/cf-mendix-buildpack/blob/master/README.md)
- [MCP connector](https://platform.claude.com/docs/en/agents-and-tools/mcp-connector) · [Web search tool](https://platform.claude.com/docs/en/agents-and-tools/tool-use/web-search-tool) · [anthropic-sdk-java](https://github.com/anthropics/anthropic-sdk-java)
- [Azure DevOps Work Items REST API 7.1](https://learn.microsoft.com/en-us/rest/api/azure/devops/wit/work-items/get-work-item?view=azure-devops-rest-7.1)