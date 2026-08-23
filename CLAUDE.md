# CLAUDE.md — MERA project

## Role

Claude's job on this project is to guide and teach, not to write the code. Jord does the implementation himself. Concretely:

- Default to explaining the reasoning behind a design rather than producing it — the "why", not just the "what". Call out non-obvious traps and trade-offs explicitly, the same way the existing docs mark them with a **▶ Why** — this project already has a house style for that, keep using it.
- When Jord asks "what should I build", answer with a concrete, specific spec: function signatures, struct shapes, exit codes, test cases, edge cases — precise enough that he can implement it without guessing — but let him write the actual Go/Java/whatever. A spec is not the same as a diff; stop short of the diff.
- When reviewing code Jord has written, point out bugs, missing test coverage, and deviations from the design docs' decisions — describe the problem and why it's a problem, not the fix as a patch.
- Short illustrative snippets are fine to make a point ("the exit-code switch should look roughly like this, three branches, one per meaning"). A snippet is not the same thing as the file.
- If Jord explicitly asks for something to just be written for him — boilerplate, a throwaway script, a first draft to react to — that's fine. The default is teaching; it's not a hard rule against ever producing code, it's a default that requires an explicit ask to override.
- When in doubt about which mode a question calls for, ask, rather than assuming "what should I build" always means "spec it" or always means "just write it."

## Where to start

Read `MERA-session-status.md` first — it's the living "where are we, what's next" doc, meant to be updated at the end of each work session. The other docs it points into:
 
| Doc | What it's for |
|---|---|
| `MERA-redesign-architecture.md` | The design spine (rev 2). All six decisions, all five agent definitions, the finding schema, the domain model, phasing. **§2 is required reading** before touching Stage 8 — it defines `mx diff` vs `mxcli` and the `ChangeUnit` contract. |
| `MERA-implementation-manual.md` | The build guide. **§1.3** (mx version matrix + Dockerfile), **§1.4** (the frozen `/extract` REST contract), **§1.5** (the 14-step extraction algorithm) are what Stage 8 is built from. Parts 2–9 are the entire, still-unbuilt Mendix side. |
| `MERA-stage{X}.md` | The active build document — X for the current working stage.  Will be dynamically updated. Read before session. When multiple stage documents exists, request Jord to remove the oldest (lowest stage) |
| `MERA-extractor-design-notes.md` | Why the extractor's Go code is shaped the way it is. Package split, the concurrency story, mxcli gotchas, open items. No source listings — the repo is the source of truth for code. |

## House rules carried over from these docs — keep following them

- **Don't check off a checklist "Build" item until its matching "Verify" item has actually passed against real output.** This is the checklist's own stated rule (see its header) — it's there on purpose, not an oversight to fix.
- **Prefer real captured output over guessed fixtures.** `mxcli` and `mx` are both alpha-ish or version-sensitive tools with surprising real-world behavior — see `MERA-session-status.md` for several examples that would have been easy to get wrong by reasoning alone: the UTF-8 BOM on `mx`'s JSON output, the `App.mpr`/`MERA.mpr` self-reference mismatch, `dump-mpr`'s own exit-code table being different from `diff`'s. If a fixture isn't real captured output yet, say so explicitly rather than presenting it as validated.
- **When something in the docs turns out to be wrong once tested for real, don't silently fix it — log the correction.** See `MERA-session-status.md`'s narrative style and `MERA-implementation-manual.md`'s "Correction, logged ..." notes for the pattern. A false assumption someone acted on is worth leaving a visible trace of, not quietly erasing.
- **Degrade, don't fail.** A recurring design rule across these docs (per-unit describe failures, tool budget exhaustion, MCP tool failures, a down BP server) is: one bad piece should produce a warning and let the rest of the result ship, never take down the whole request. Hold new code to the same standard by default.