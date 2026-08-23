# MERA Implementation Manual
 
**A build guide for the multi-agent redesign**
Companion to `MERA-redesign-architecture.md` (rev 2) · Date: 2026-08-21
 
---
 
## How to use this manual
 
You are an expert Mendix developer. So I am going to be almost useless about Mendix and quite thorough about everything else. When I write "make an entity", that's all you get. When I write "here is the tool loop", you get the whole thing with the reasoning behind every decision.
 
The parts I'll spend real time on:
 
- **The extractor sidecar** — a container with `git`, `mx` and `mxcli` on it. Nothing about this is Mendix.
- **The Anthropic Java SDK inside the Mendix classloader** — where the sharp edges are.
- **Agent definitions** — your explicit question, "how do I write the file that creates the agents". The answer is more interesting than "here's a file".
- **`JA_InvokeAgent`** — the tool loop. This is the single piece of code that makes the whole design work.
- **Concurrency and transactions** — where a naive implementation quietly holds a database transaction open for four minutes.
Two disclaimers before we start. The Anthropic Java SDK is at 2.54.0 as I write this and moves fast; treat my code as *shape*, verify method names against the version you pin. And `mxcli` is alpha — pin a version and expect to work around bugs.
 
A note on how to read this: I've marked the genuinely non-obvious bits with **▶ Why**. If you're skimming, read those.
 
---
 
## Part 0 — The shape of the thing
 
Three deployables:
 
```
mera-app          Mendix. UI, data, orchestration, agent runner.
mera-extractor    Docker. git + mx + mxcli. Turns commits into text.
mera-bp-mcp       Your best-practices app, exposing an MCP server.
```
 
And one external dependency you don't deploy: the Anthropic API.
 
Before any code, internalise this split — it's the whole architecture in four lines:
 
| Thing | Lives as | Because |
|---|---|---|
| **Prompts** | Data (Mendix entity rows) | You will change these hourly. A redeploy per prompt tweak kills the tuning loop. |
| **Output schemas** | Code (Java POJOs) | These are a *contract*. If a schema changes, downstream code changes too — you want the compiler to tell you. |
| **Control flow** | Microflow | You want to look at it. You want a junior to look at it. |
| **The agent runner** | One Java action, reused five times | Five copies of a tool loop is five places to fix the same bug. |
 
**▶ Why prompts-as-data but schemas-as-code.** It's tempting to make both data ("full flexibility!"). Resist. A prompt is prose — wrong prompts produce bad findings, which a human filters. A schema is an interface — a wrong schema produces a parse failure at 2am in production. Different volatility, different risk, different home. This asymmetry shows up constantly in agent systems and getting it right early saves a lot of pain.
 
---
 
## Part 1 — The extractor sidecar
 
### 1.1 What it is
 
A stateless HTTP service with two responsibilities:
 
1. `POST /extract` — given `(repoUrl, pat, baseSha, headSha)`, return `ChangeUnit[]` plus the reference graph.
2. `POST /workspaces` … — hold a TTL-bounded checkout that can answer `describe`/`search` queries during a review.
It has no database, no LLM access, no knowledge of what a review is. If you find yourself adding review logic here, stop — it belongs in Mendix.
 
### 1.2 Language choice
 
**Go.** Not because you'll love writing it, but because:
 
- Single static binary, tiny image, no runtime to install alongside the tools you actually need.
- `mxcli` is written in Go and ships a Go library (Part VII of its docs). If you ever want to skip the subprocess boundary and call it in-process, you're already there.
- Concurrency for parallel `mxcli describe` calls across hundreds of units is trivial and safe.
Python is a legitimate second choice if your team writes Python — `subprocess` + FastAPI gets you there in a day. Node is fine too. Don't agonise; the service is ~800 lines whatever you pick. Just don't write it in Java "for consistency" — you'd be shipping a JVM into a container whose whole purpose is running two native binaries.
 
**On what `mx`/`mxbuild` actually is:** a **self-contained .NET application** — `mxbuild.dll`, `mxbuild.deps.json`, `mxbuild.runtimeconfig.json` (`"tfm": "net10.0"`), bundled CoreCLR (`libcoreclr.so`, `libclrjit.so`). Confirmed by real inspection of a downloaded tarball. **No JVM or JRE is needed for it.** Its actual runtime dependency is ICU (`libicu72` on Debian bookworm). The `.jar` files that do exist in the tarball live under `runtime/` — that's the real, Java-based Mendix Runtime, unrelated to `mx`/`mxbuild`, unused by the extractor, and trimmed out entirely (§1.3).
 
### 1.3 The Dockerfile, and the version-matrix problem
 
**Confirmed working end to end via a real `docker build --no-cache`:** the 3-stage shape below produces a container where `/opt/mx/11.13.0/modeler/mx --help` and `mx diff --help` both return real command output. Two bugs found during that build are already corrected below — the Go build path (this repo's `main` package is at the module root, plain `.`, not `./cmd/extractor`) and `libicu72` in the final stage (without it `mx` fails to start with a missing-ICU error).
 
```dockerfile
# ---------- stage 1: compile the Go program ----------
FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/extractor .
 
# ---------- stage 2: mx/mxbuild acquisition, trimmed before it ever
# reaches the final image ----------
#
# Runs scripts/fetch-mx.sh's inspect + trial-trim + finalize chain
# (add-trimmed-version) once per line of mx-versions.txt, then discards
# each version's scratch download/extraction/trimmed-aside leftovers
# immediately with 'clean' — keeps peak disk use during this stage bounded
# to roughly one version's ~1.6G untrimmed extraction at a time, not N.
#
# Uses the SAME base image as the final stage on purpose: fetch-mx.sh's
# trial-trim actually runs mx to confirm it starts (see its
# check_mx_mode()), so validating it here means validating it against the
# real target environment, not a different OS/libc combination that
# happens to also have libicu.
FROM debian:bookworm-slim AS mx-fetch
 
# libicu72 confirmed correct for bookworm (packages.debian.org/bookworm/libicu72).
# mx/mxbuild needs real ICU data, not the invariant-globalization workaround —
# this build hardcodes loading the 'en-us' culture at startup (confirmed via
# a real CultureNotFoundException, not assumed — see fetch-mx.sh's header
# comment and MERA-session-status.md).
RUN apt-get update && apt-get install -y --no-install-recommends \
      curl \
      ca-certificates \
      tar \
      libicu72 \
    && rm -rf /var/lib/apt/lists/*
 
WORKDIR /build
COPY scripts/fetch-mx.sh mx-versions.txt ./
 
# Explicit, not the git-toplevel fallback fetch-mx.sh uses by default —
# there's no .git in this build context (and there shouldn't be).
ENV MX_FETCH_SCRATCH=/build/.mx-fetch-scratch
ENV MX_BINARIES_DIR=/build/.mx-binaries
 
RUN chmod +x fetch-mx.sh && set -eux; \
    while IFS= read -r v; do \
      v="$(echo "$v" | sed 's/#.*//' | xargs)"; \
      [ -z "$v" ] && continue; \
      ./fetch-mx.sh add-trimmed-version "$v"; \
      ./fetch-mx.sh clean "$v"; \
    done < mx-versions.txt
 
# ---------- stage 3: the actual image ----------
FROM debian:bookworm-slim
ARG TARGETARCH=amd64
 
RUN apt-get update && apt-get install -y --no-install-recommends \
      git ca-certificates curl \
      libicu72 \
    && rm -rf /var/lib/apt/lists/*
 
# --- mxcli: latest by default, pinnable from CI. See §1.3a ---
ARG MXCLI_VERSION=latest
RUN set -eux; \
    if [ "$MXCLI_VERSION" = "latest" ]; then \
      url="https://github.com/mendixlabs/mxcli/releases/latest/download/mxcli-linux-${TARGETARCH}"; \
    else \
      url="https://github.com/mendixlabs/mxcli/releases/download/${MXCLI_VERSION}/mxcli-linux-${TARGETARCH}"; \
    fi; \
    curl -fsSL --retry 3 --retry-delay 2 -o /usr/local/bin/mxcli "$url"; \
    chmod +x /usr/local/bin/mxcli; \
    # Record what actually landed — see §1.3a on provenance.
    { mxcli --version || mxcli version; } > /etc/mxcli.version 2>&1; \
    cat /etc/mxcli.version
 
# --- mx/mxbuild: one trimmed binary per Mendix version, copied from the
# mx-fetch stage above. ONLY the finalized .mx-binaries/<version>/ trees
# land here — the download, the untrimmed ~1.6G extraction, and the
# trimmed-aside leftovers from stage 2 never reach this image, because
# that entire stage is discarded once the build finishes. See
# scripts/fetch-mx.sh's TRIM_CANDIDATES comment for exactly what got cut
# from each version and how each cut was validated.
COPY --from=mx-fetch /build/.mx-binaries /opt/mx
 
# The compiled binary from stage 1. No Go toolchain in this image.
COPY --from=build /out/extractor /usr/local/bin/extractor
 
RUN useradd -m -u 10001 extractor
USER extractor
WORKDIR /work
ENV MERA_WORK_ROOT=/work
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/extractor"]
```
 
**`mx-versions.txt`** (repo root, alongside `scripts/fetch-mx.sh`) is the actual source of truth for the version matrix below — one Mendix version per line, `#`-comments and blank lines ignored:
 
```
# Mendix versions to bundle into the extractor image.
11.13.0
```
 
Add a line, rebuild, done — no other file needs to change for a new version to become available. (Only 11.13.0 is real-diff-validated as of this writing; see the version-matrix caveat later in this section before adding others.)
 
### 1.3a Tracking `mxcli` latest
 
The release assets are **raw binaries, not archives** — `mxcli-linux-amd64`, `mxcli-linux-arm64`, and the darwin/windows equivalents. No tarball, no `checksums.txt`. So there's nothing to extract, and nothing to verify. For an internal alpha tool that's acceptable; just know that you're trusting the transport.
 
GitHub gives you a stable redirect to the newest release's asset:
 
```
https://github.com/mendixlabs/mxcli/releases/latest/download/mxcli-linux-amd64
```
 
No API call, no token, no `jq`. That's the default in the Dockerfile above.
 
**▶ But Docker layer caching will silently defeat you.** `RUN curl .../latest/download/...` produces a cache key from the *command string*, which never changes. So you get "latest as of the first time this layer was built" — possibly months old — and no signal that anything is stale. This is the failure mode where you think you're tracking latest and you're pinned to something you can't name.
 
The fix is to make CI resolve the tag and pass it in, so the ARG value changes exactly when a new release exists:
 
```bash
MXCLI_VERSION=$(curl -fsSL \
  -H "Authorization: Bearer $GITHUB_TOKEN" \
  https://api.github.com/repos/mendixlabs/mxcli/releases/latest | jq -r .tag_name)
 
docker build \
  --build-arg MXCLI_VERSION="$MXCLI_VERSION" \
  --label org.opencontainers.image.version="mxcli=$MXCLI_VERSION" \
  -t mera-extractor:latest .
```
 
Cache correctness and provenance fall out of the same mechanism: a new release changes the build arg, which invalidates the layer, and the tag ends up in an image label you can read back. Local `docker build .` with no args still just grabs latest, which is what you want on a dev machine.
 
Pass `GITHUB_TOKEN` on that API call. Unauthenticated requests are limited to 60/hour **per IP**, and CI runners share egress addresses — this fails intermittently and confusingly at exactly the wrong moment.
 
**▶ Provenance matters more when you're deliberately unpinned.** "Always latest, breaking is fine" is a reasonable stance for an alpha project, but it means a review that behaved oddly last Tuesday cannot be reproduced unless you recorded which build produced it. So:
 
1. The extractor reads `/etc/mxcli.version` at startup.
2. It returns `mxcliVersion` in every `/extract` response.
3. MERA stamps it on `ReviewRun`, next to `mendixVersion`.
Three lines of work, and it turns "the reviews got weird last week" from a mystery into a diff between two version strings. Do the same for the `mx` binary you selected — that one already varies per repository, so you want it recorded regardless.
 
If you want to go further: have CI run your fixture suite (§1.9) against the new `mxcli` before promoting the image. Since the fixtures are committed and the extraction is deterministic, a diff in the output tells you precisely what a new `mxcli` release changed — which on alpha software is worth knowing before it reaches a review.
 
**▶ The version matrix is the thing that will hurt you — though acquiring and trimming the binaries is now fully automated.** `mx diff` is bundled with `mxbuild` and is version-sensitive: an `mx` from a Studio Pro 10.12-era release will refuse (exit code 4) or misread a model authored in 11.3. You are reviewing repositories authored by different teams on different Mendix versions.
 
Mendix publishes `mxbuild` (which bundles `mx`) as a direct, public download per version:
 
```
https://cdn.mendix.com/runtime/mxbuild-{mxversion}.tar.gz       # Linux
https://cdn.mendix.com/runtime/win-mxbuild-{mxversion}.tar.gz   # Windows
```
 
(macOS: bundled inside `StudioPro.app`, symlinked at `/usr/local/bin` — not needed for this container, noted for completeness.) No login or license-gated artifact store showed up in the documented download flow — flagging that as **empirically unverified rather than asserted fact**; confirm it holds for your org's Mendix account before relying on it in CI.
 
**The raw tarball is large — 1.6G extracted for 11.13.0 — and most of it isn't needed.** It bundles Studio Pro's entire web-based IDE client, the Java-based Mendix Runtime, and a large `tools/` folder, none of which matter for `mx diff`/`dump-mpr`/`analyze-mpr`. `scripts/fetch-mx.sh` inspects the real layout empirically rather than guessing what's safe to delete, and its `add-trimmed-version` command downloads, trims and validates a version in one step: **1.6G → 164M for 11.13.0**, confirmed via a real from-scratch `docker build`, and validated with a real `mx diff` (not just `--help`) across both a text-only change and an image-only change — the latter mattering because this project's content includes `Images$ImageCollection` data. See the script's own `TRIM_CANDIDATES` comment for the full cut list, what is confirmed load-bearing and must be left alone (`Mendix.Modeler.WebUI.dll`, the non-Legacy `Mendix.Modeler.Theming.dll`, `CycloneDX.Core.dll`), and how each was validated.
 
This opens a question worth settling empirically before committing to a large version matrix: whether a single `mx`/`mxbuild` binary can handle model files from more than one Mendix version (within a major, or even across 10.x/11.x), which would simplify — or eliminate — keeping one binary per exact version. That investigation is tracked in `MERA-session-status.md` and isn't resolved yet; treat the matrix design below as the safe default until it is.
 
Handle the matrix explicitly, until/unless that investigation says otherwise — `mx-versions.txt` above is the literal source of truth for this list, this is just illustrating the shape it produces on disk:
 
```
/opt/mx/
  10.12/modeler/mx
  10.18/modeler/mx
  11.0/modeler/mx
  11.6/modeler/mx
  11.13.0/modeler/mx
```
 
(Note the `modeler/` subfolder — confirmed via the real Docker build. The trimmed tree mirrors the original tarball's `modeler/`+`runtime/` top-level layout, so the binary lands one level deeper than a naive `/opt/mx/<version>/mx` would suggest.)
 
At extraction time:
 
1. Read the Mendix version from the checkout **before** running anything. In MPRv2 it's in the `Metadata`/`ProjectSettings` unit; `mxcli describe` will surface it, or read `.mpr`'s metadata table directly. (`mx analyze-mpr` also surfaces it plainly, and is confirmed version-agnostic — which is what makes it usable *before* you know which binary to select.)
2. Select the closest `mx` at or above that version.
3. If no compatible binary exists, **fail loudly** with `unsupportedMendixVersion` and the version you found. Do not fall back to the newest and hope.
Getting the binaries into the image is `./fetch-mx.sh add-trimmed-version <version>` per line of `mx-versions.txt`, run inside the Dockerfile's dedicated `mx-fetch` stage. Budget your remaining time for confirming exactly which versions you need in the matrix, not for the acquisition or trimming mechanics — those are done.
 
### 1.4 The REST contract
 
Write this down before you write code, and treat it as frozen. Both sides depend on it.
 
```
POST /extract
Authorization: Bearer <short-lived token minted by Mendix>
Content-Type: application/json
 
{
  "requestId":  "uuid",              // idempotency key
  "repoUrl":    "https://git.api.mendix.com/abc-123.git",
  "provider":   "mendix|azuredevops|github",
  "auth":       {"kind":"pat","username":"pat","secret":"..."},
  "baseSha":    "a1b2c3...",
  "headSha":    "d4e5f6...",
  "options": {
    "includeReferenceGraph": true,
    "maxCloneMb":            4096,
    "timeoutSeconds":        900
  }
}
 
200 →
{
  "requestId": "uuid",
  "mendixVersion": "11.3.0",
  "storageFormat": "MPRv2",
  "mxcliVersion": "v0.13.0",         // provenance — see §1.3a
  "mxVersion": "11.0",               // which /opt/mx binary was selected
  "changeUnits": [
    {
      "module": "Sales",
      "unitType": "Microflow",
      "qualifiedName": "Sales.ACT_Order_Confirm",
      "changeKind": "Modified",
      "structuralDelta": { ... },     // slice of mx diff JSON
      "beforeMdl": "...",
      "afterMdl":  "...",
      "neighbourhood": {
        "callers":    [{"qualifiedName":"...","referenceKind":"Calls"}],
        "references": [ ... ]
      },
      "tokenEstimate": 1240
    }
  ],
  "textDiffs": [
    {"path":"javasource/sales/actions/Foo.java","changeKind":"Modified","unifiedDiff":"@@ ..."}
  ],
  "referenceGraph": {
    "units": [{"qualifiedName":"...","module":"...","unitType":"...","existsIn":"both"}],
    "edges": [{"from":"...","to":"...","referenceKind":"Calls","location":"..."}]
  },
  "warnings": ["mxcli could not render Sales.PAGE_Legacy: unsupported widget"]
}
 
4xx/5xx →
{"requestId":"uuid","error":"unsupportedMendixVersion","detail":"...","retryable":false}
```
 
**▶ Note `warnings` and the per-unit failure model.** `mxcli` is alpha. It *will* fail on individual units. If one bad page fails the whole extraction, MERA becomes useless on exactly the large legacy apps that need it most. Degrade per unit: emit the unit with `afterMdl: null` and a warning, and let the agents review what rendered. Design for partial success from line one — retrofitting it is miserable.
 
### 1.5 The extraction algorithm
 
```
1.  mkdir /work/<requestId>            (0700, on tmpfs if you can afford the RAM)
2.  write git credential helper        (§1.7)
3.  git init; git remote add origin <repoUrl>
    git config remote.origin.promisor true
    git config remote.origin.partialclonefilter blob:none
4.  git fetch --filter=blob:none --no-tags origin <baseSha> <headSha>
    - if server rejects SHA fetch → fall back to a shallow branch fetch
    - enforce maxCloneMb; abort with cloneTooLarge
5.  git worktree add /work/<id>/base <baseSha>
    git worktree add /work/<id>/head <headSha>
6.  read Mendix version + storage format from head
    - if MPRv1 → fail: unsupportedStorageFormat
    - select /opt/mx/<version>/modeler/mx
    - discover the checkout's actual .mpr filename (glob for *.mpr — don't
      hardcode App.mpr; a real test app was git-tracked as MERA.mpr while
      internally self-referencing App.mpr, and mx refuses to open a file
      under the "wrong" name). Copying it to match its internal
      self-reference is a workaround, not a fix; treat a persistent
      mismatch as an explicit failure mode if the copy approach doesn't
      hold up for other projects.
7.  mx diff base/<found>.mpr head/<found>.mpr /work/<id>/diff.json
    - exit 0 → ok; 2 → conflicts (still usable); 4 → unsupported; 129 →
      generic error; else unexpected — this CLI's exit-code behavior for
      *usage* errors (e.g. --help) is unreliable, so don't treat "non-zero"
      alone as failure without checking the actual output/exit code table.
      Note `mx dump-mpr` (step 7a) has its OWN, DIFFERENT exit-code table —
      0 OK, 1 wrong project file, 2 invalid unit type(s), 3 unknown JSON
      export error, 4 different Mendix version — don't reuse this table
      for it.
    - if diffing fails on a project containing a Projects$ProjectConversion
      unit (visible via `mx analyze-mpr`), treat that as a known-unsupported
      case up front rather than a generic error — that unit means the
      commit captures a Studio Pro version-upgrade migration in progress,
      which no mx build parses cleanly. The "$ID"/"Associations" parse
      exception this project once chased for a session was exactly this,
      not an mx bug.
7a. mx diff's unitDifferences[] gives id/type/change/containerId/
    containmentName — no qualifiedName. Resolve it via:
      mx dump-mpr base/<found>.mpr --unit-type <comma-joined distinct types>
        --output-file base-dump.json   (only for change != Added)
      mx dump-mpr head/<found>.mpr --unit-type <comma-joined distinct types>
        --output-file head-dump.json   (only for change != Deleted)
    then find each id in the relevant dump: objects carry "$ID" alongside
    "$QualifiedName". --output-file is a real flag — dump-mpr does NOT take
    a bare second positional output path; omitting it dumps the full JSON
    to stdout instead. --unit-type and --module-names both accept a
    comma-separated list in one call — no need to invoke dump-mpr once per
    type. Module is just $QualifiedName's prefix before the first ".".
    Note some units carry $ID with no $QualifiedName at all and must be
    synthesized from the container chain — see MERA-stage8.md §2.
8.  parse diff.json + the dump-mpr resolution → list of (qualifiedName,
    module, unitType, changeKind)
9.  for each changed unit, in a worker pool of ~8:
      changeKind != Added   → mxcli describe -p base/<found>.mpr <kind> <qn>  → beforeMdl
      changeKind != Deleted → mxcli describe -p head/<found>.mpr <kind> <qn>  → afterMdl
      neighbourhood:
        Deleted → mxcli refs    -p base/<found>.mpr <qn>
        else    → mxcli refs    -p head/<found>.mpr <qn>
                  mxcli callers -p head/<found>.mpr <qn>
      on failure → record warning, continue
10. if includeReferenceGraph:
      build the whole-app graph from head (and base, for deleted units) —
      see §1.6 below on how
11. git diff --unified=5 <base> <head> -- javasource theme deployment *.json
    → textDiffs
12. tokenEstimate per unit: len(beforeMdl + afterMdl + delta) / 3.6
13. respond
14. finally: shred the credential file, rm -rf /work/<requestId>
```
 
**Step 7a.** `mx diff` alone doesn't give you enough to call `mxcli describe` — its `id` field is an internal GUID, not a qualified name. `mx dump-mpr` resolves it in one step, using the same official tool family rather than any workaround (an earlier investigation found `id` also maps to a raw `.mxunit` BSON file at `mprcontents/<id[0:2]>/<id[2:4]>/<id>.mxunit` — deliberately NOT used: it's Mendix's undocumented proprietary application of BSON, and `dump-mpr`'s supported JSON export reads the identical data). Full design and the Go package shape this maps to (`internal/mx`) is in `MERA-stage8.md`.
 
**Step 12, the token estimate.** MDL is dense, structured, and low-entropy — roughly 3.5–3.8 characters per token, versus ~4 for English prose. Measure it on your own corpus once and hardcode the constant. It only needs to be right enough for batching decisions; Mendix will get exact counts back from the API anyway.
 
**▶ Step 9's worker pool.** `mxcli describe` is a process spawn each time. On a change set of 400 units that's 800 spawns. Eight workers keeps a 4-core container busy without thrashing. If this becomes your bottleneck, that's the moment to look at mxcli's Go library and skip the subprocess boundary — but measure first, don't pre-optimise.
 
*(Implementation note: the extractor's actual Go code moved this worker-pool cap to a single global semaphore in `internal/mxcli`, gating every mxcli subprocess call process-wide rather than per request — see `MERA-extractor-design-notes.md` §3. The reasoning above about "~8 keeps a 4-core container busy" still holds; it's just enforced globally now rather than per `/extract` call.)*
 
### 1.6 Building the reference graph
 
The architecture doc treats this as one line. It isn't. There are two approaches:
 
**Approach A — enumerate and query.** List every unit (`mxcli` catalog query), then run `mxcli refs` per unit. Simple, but that's thousands of process spawns and it will take minutes.
 
**Approach B — catalog query.** mxcli's docs, Part V, cover "catalog queries" — the parsed model exposed as queryable tables. If the catalog exposes a references/dependencies table, one query gives you the entire edge set. **Try this first.** This is the difference between a 4-second graph build and a 4-minute one.
 
Spend an hour with `mxcli` on a real repo before committing to either. Specifically: run the catalog query commands against a mid-size app and see whether references come back as a relation. Everything about the feasibility of §7 in the architecture doc depends on this being cheap, so find out early.
 
**Fallback if the graph is expensive:** make `includeReferenceGraph` genuinely optional and only request it when the Planner flagged `impactAnalysisRequired` on at least one unit. You already have that flag; use it as a gate on extraction too, not just on tool budgets.
 
### 1.7 PAT handling — the credential helper trick
 
Never put the PAT in the URL. `https://pat:TOKEN@git...` lands in `.git/config`, in process listings, in error messages, in your logs. Instead:
 
```go
// Write a one-shot credential helper to a 0600 file on tmpfs.
helper := filepath.Join(work, ".git-credentials")
os.WriteFile(helper,
    []byte(fmt.Sprintf("https://%s:%s@%s\n", url.QueryEscape(user),
        url.QueryEscape(pat), host)),
    0600)
 
cmd.Env = append(os.Environ(),
    "GIT_TERMINAL_PROMPT=0",                       // never hang on a prompt
    "GIT_CONFIG_COUNT=1",
    "GIT_CONFIG_KEY_0=credential.helper",
    "GIT_CONFIG_VALUE_0=store --file="+helper,
    "GIT_ASKPASS=/bin/true",
)
 
defer func() {
    // Overwrite before unlinking. Paranoid, cheap, correct.
    if f, err := os.OpenFile(helper, os.O_WRONLY, 0600); err == nil {
        st, _ := f.Stat()
        f.Write(make([]byte, st.Size()))
        f.Sync(); f.Close()
    }
    os.Remove(helper)
}()
```
 
Then, three more things:
 
1. **Scrub your logs.** A `git` stderr line can contain the remote URL. Run every subprocess's stdout/stderr through a redactor that replaces the PAT string with `***` before it reaches your logger. Write a test for this — it's the kind of thing that regresses silently.
2. **Never push.** `git config remote.origin.pushurl /dev/null` after adding the remote. Belt and braces on top of a read-only PAT scope.
3. **`GIT_TERMINAL_PROMPT=0`** — without it, a bad credential makes git block on an interactive prompt forever, and your request times out with no useful error.
(The same discipline applies to the Mendix Team Server PAT used from a dev shell, not just from Go code: never embed it in a clone URL — `git clone https://pat:$MERA_PAT@...` ends up in `.git/config` and shell history — use the interactive `pat`/`$MERA_PAT` username/password prompt instead. This was confirmed the hard way to matter in practice, not just in theory, during this project's own `mx diff` testing.)
 
### 1.8 Workspace leases
 
```
POST /workspaces  {repoUrl, auth, baseSha, headSha, ttlSeconds}
                  → {workspaceId, expiresAt}
POST /workspaces/{id}/describe {qualifiedName, revision, detail}
POST /workspaces/{id}/search   {query}
DELETE /workspaces/{id}
```
 
Implementation notes that matter:
 
- **Reap independently of Mendix.** A goroutine ticking every 30s that kills expired workspaces. Mendix also runs a scheduled event to close leases. Two reapers, because the failure mode of zero reapers is a disk full of clones containing credential-derived data.
- **The lease holds the checkout, not the PAT.** Clone during `POST /workspaces`, then wipe the credential immediately. Subsequent queries are pure local `mxcli` calls. If the lease needed to re-fetch, you'd have to hold the secret for 30 minutes — don't design yourself into that.
- **Bound concurrency per workspace.** An agent can fire queries fast; a semaphore of 2–4 `mxcli` processes per workspace stops one review starving the box.
- **`detail: summary|full`.** `summary` returns signature and structure; `full` returns the whole MDL body. Give the agent the cheaper option and describe it honestly in the tool description — models do respond to "this is expensive, prefer X".
### 1.9 Developing it without Mendix
 
Build a CLI harness on day one:
 
```bash
extractor-cli extract \
  --repo https://git.api.mendix.com/abc.git \
  --pat  "$MERA_PAT" \
  --base a1b2c3 --head d4e5f6 \
  --out  ./fixture.json
```
 
Then commit a handful of `fixture.json` files from real repos into the Mendix side as test data. **You want to build and tune the entire agent layer against fixtures, with no network and no extractor running.** This one decision will save you days: prompt iteration is a tight loop, and you do not want a 90-second clone in it.
 
### 1.10 From your laptop to production — build once, run anywhere
 
**▶ This is worth being explicit about, because it's easy to assume "it needs to work on my machine" is a permanent requirement. It isn't.**
 
Everything in §1.3–§1.9 — the Go/mxcli version argument you'll hit locally, `mx`'s version matrix, `docker build` succeeding on your laptop at all — is a **build-time** concern. It only exists at the moment an image is being assembled. Once `docker build` succeeds, the result is a sealed artifact: your own copy of Go, `mxcli`, `git`, and your compiled binary, frozen inside it. That image no longer depends on anything installed on the machine that built it, or on any machine that later runs it. That's what a container *is* — the whole reason to use one instead of "here's a script, please have the right tools installed."
 
So the deployment answer is: **you never manually build this for production. A CI pipeline builds it exactly once, in one controlled environment, and pushes the result to a container registry.** From then on, no laptop is involved — not yours, not a teammate's, not whatever eventually runs it. They all pull and run the same already-built image.
 
A minimal GitHub Actions workflow does this (adjust the registry if you're not on GitHub):
 
```yaml
# .github/workflows/build-extractor.yml
on:
  push:
    branches: [main]
 
jobs:
  build-and-push:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: docker/login-action@v3
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - run: |
          MXCLI_VERSION=$(curl -fsSL -H "Authorization: Bearer ${{ secrets.GITHUB_TOKEN }}" \
            https://api.github.com/repos/mendixlabs/mxcli/releases/latest | jq -r .tag_name)
          docker build \
            --build-arg MXCLI_VERSION="$MXCLI_VERSION" \
            -t ghcr.io/<your-org>/mera-extractor:latest \
            -t ghcr.io/<your-org>/mera-extractor:${{ github.sha }} \
            .
          docker push --all-tags ghcr.io/<your-org>/mera-extractor
```
 
Two things this buys you beyond "it's automated":
 
1. **A local Go/mxcli version drift becomes a CI-caught PR failure, not a production incident.** If your `go.mod` ever requires a newer Go than the Dockerfile's `FROM golang:X` line provides — exactly the error you'll hit locally when the two disagree — CI fails the build on the pull request. Far better than discovering it when someone else's laptop, with a different local Go, happens to try building it.
2. **Every image is traceable.** Tagging with both `latest` and the commit SHA means you can always answer "which exact code produced the extractor that ran last Tuesday's review" — the same provenance discipline as `mxcliVersion` in §1.3a, one level up.
**Where does the built image actually run?** Not Mendix Cloud — §1 already established that its buildpack can't run native binaries at all, which is the entire reason the extractor is a separate deployable. It needs its own host that can pull from the registry and expose HTTPS: Azure Container Apps or AWS Fargate (point at the image, minimal ops), a small dedicated VM running `docker run` (simplest to reason about), or Kubernetes if your org already runs one. Whichever you pick, `JA_RequestExtraction` (Part 5) just does an HTTPS POST to wherever that lands — `https://mera-extractor.yourcompany.com/extract`. Mendix doesn't know or care that the other side is a Go binary in a container; it only sees a REST endpoint.
 
**▶ The boundary this creates is worth internalising, not just working around.** The JVM running MERA and the Go binary running the extractor never share a machine, a filesystem, or a toolchain — they only ever talk over HTTP. That's not incidental; it's the reason the two-deployable design exists at all (architecture doc, §1). Once that boundary is real, "does Go run on the machine hosting Mendix" stops being a question, because it was never going to.
 
**Ongoing discipline this leaves you with:** `go.mod`'s `go` directive and the Dockerfile's `FROM golang:X-bookworm` tag need to move together deliberately — same relationship as the `mxcli` version pin. When you upgrade Go locally, bump the Dockerfile in the same commit, and let CI catch it if you forget. The same applies to `mx-versions.txt`: a version added there only takes effect on the next image build, and `add-trimmed-version` needs real network access to `cdn.mendix.com` at build time — confirm your CI runner has that egress before wiring this into a pipeline that currently doesn't need it.
 
---
 
## Part 2 — Wiring the Anthropic SDK into Mendix
 
### 2.1 Declaring the dependency
 
Module settings → **Java Dependencies** tab → add:
 
| Group ID | Artifact ID | Version |
|---|---|---|
| `com.anthropic` | `anthropic-java` | `2.54.0` (pin exactly) |
| `org.eclipse.jgit` | `org.eclipse.jgit` | pin exactly |
 
Studio Pro resolves via Gradle into `vendorlib`.
 
**Pin exact versions.** The docs warn that unpinned versions cause frequent automatic updates and a high commit volume — but the real reason is that an LLM SDK moving under you between deploys is a debugging nightmare you don't need.
 
### 2.2 The classloader trap
 
**▶ This is the one that will cost you an afternoon if you don't expect it.** The Anthropic Java SDK depends on Jackson and OkHttp. The Mendix runtime also ships Jackson. Managed dependencies resolve version conflicts by **taking the newest**, across all modules. So:
 
- Another module (or the Mendix runtime) pulls an older Jackson → the SDK gets a Jackson missing methods it needs → `NoSuchMethodError` at runtime, not compile time.
- Or the SDK pulls a newer Jackson → some other module that was fine yesterday breaks.
What to do:
 
1. After adding the dependency, **inspect `vendorlib`** and note the Jackson and OkHttp versions that landed.
2. Check for duplicates between `vendorlib` and `userlib`. If a stray Jackson jar is in `userlib` from some older integration, it will fight. Remove it.
3. If you hit a conflict, use per-dependency **exclusions** on the Java Dependencies tab and add the transitive dependency explicitly at a version that works for everyone.
4. Write the hello-world action in §2.3 **before** writing any real code, so you find this on day one rather than day twenty.
If the conflict proves intractable — for instance, the Mendix runtime hard-requires a Jackson the SDK can't work with — the escape hatch is to skip the SDK entirely and call the Messages API over plain HTTP with your own JSON handling. It's a REST API; the SDK is a convenience, not a requirement. Keep that in your back pocket. It also means: don't let SDK types leak into your Mendix domain layer, so swapping is contained.
 
### 2.3 Prove it works
 
```java
// JA_AnthropicSmokeTest — returns String, no parameters.
AnthropicClient client = AnthropicOkHttpClient.builder()
        .apiKey(Core.getConfiguration().getConstantValue("MERA.AnthropicApiKey").toString())
        .maxRetries(2)
        .timeout(Duration.ofSeconds(60))
        .build();
 
Message m = client.messages().create(MessageCreateParams.builder()
        .model(Model.CLAUDE_OPUS_5)
        .maxTokens(64L)
        .addUserMessage("Reply with exactly: MERA online")
        .build());
 
return m.content().stream()
        .flatMap(b -> b.text().stream())
        .map(TextBlock::text)
        .collect(Collectors.joining());
```
 
Run it. If you get `MERA online`, your classloader is clean and you can proceed with confidence. If you get a `NoSuchMethodError`, go back to §2.2 — and be glad you found it now.
 
### 2.4 The API key
 
Runtime **constant**, value set per environment in Mendix Cloud (or a cloud secret injected as an environment variable). Never in the model, never in a `.mpr` you commit. Same discipline as the PAT, minus the per-user part — the Anthropic key is MERA's own credential, not the user's.
 
### 2.5 Client lifecycle — build once
 
**▶ Do not construct `AnthropicOkHttpClient` per Java action call.** Each one builds an OkHttp connection pool and thread pool. Under a queue running six concurrent batches you'd be churning pools constantly, leaking threads, and paying TLS handshakes you don't need.
 
Hold a singleton:
 
```java
public final class AnthropicClientHolder {
    private static volatile AnthropicClient instance;
 
    public static AnthropicClient get() {
        AnthropicClient c = instance;
        if (c == null) {
            synchronized (AnthropicClientHolder.class) {
                if (instance == null) {
                    instance = AnthropicOkHttpClient.builder()
                        .apiKey(Core.getConfiguration()
                                    .getConstantValue("MERA.AnthropicApiKey").toString())
                        .maxRetries(0)   // we retry ourselves — see §4.7
                        .timeout(Duration.ofMinutes(10))
                        .build();
                }
                c = instance;
            }
        }
        return c;
    }
}
```
 
Put this in a plain class under `javasource/mera/`, not in a Java action. Java actions are generated wrappers; helper classes are yours.
 
---
 
## Part 3 — "The file that creates the agents"
 
You asked how to write the file that creates and orchestrates the planner and agents. Let me answer directly, because the answer shapes a lot of what follows.
 
### 3.1 There isn't one — and that's deliberate
 
If you've seen Claude Code's subagents, you've seen `.claude/agents/*.md` — a markdown file with frontmatter per agent, discovered from disk. That works because Claude Code is a CLI reading a project directory at startup.
 
MERA is a deployed Mendix application. A file-on-disk approach there means: prompts live in `resources/`, changing one requires a redeploy, and you cannot tell which prompt version produced last Tuesday's report. For a system whose *primary tuning loop is prompt iteration*, that's the wrong trade.
 
So: **agent definitions are rows in an entity, and the orchestration is a microflow.** No file at runtime.
 
But you still want a file at *design* time — something reviewable in git, diffable, and importable. So the pattern is:
 
```
agents.yaml   (in your repo, reviewed like code)
     │
     │  imported by ACT_Admin_ImportAgentDefinitions
     ▼
AgentDefinition rows   (in Mendix, versioned, referenced by every run)
```
 
### 3.2 The entity
 
```
AgentDefinition
  agentKey          String   "planner" | "layer1" | "layer2" | "layer3" | "synthesizer"
  version           Integer  monotonic per agentKey
  isActive          Boolean  exactly one active per agentKey
  model             String   "claude-opus-5"
  maxTokens         Integer
  temperature       Decimal
  systemPrompt      String (unlimited)
  toolConfigJson    String (unlimited)
  outputSchemaKey   String   names the Java POJO — see §3.4
  notes             String   why this version exists
  createdBy/At
```
 
**▶ `isActive` plus monotonic `version`, never in-place edits.** When a reviewer disputes a finding three weeks later, you need to answer "what prompt produced this?" `ReviewRun` stores the `AgentDefinition` ID it used, and because rows are immutable, that answer stays true forever. Editing prompts in place destroys your ability to explain your own output — and for an advisory tool, explicability is the product.
 
### 3.3 The seed file
 
```yaml
# agents.yaml — reviewed in git, imported into MERA.
# Prompts here are abbreviated; the full text lives in the architecture doc §10.
version: 3
agents:
 
  - agentKey: planner
    model: claude-haiku-4-5
    maxTokens: 4096
    temperature: 0
    outputSchemaKey: PlannerOutput
    notes: "v3: added impactAnalysisRequired flag"
    tools: []
    systemPrompt: |
      You are the Planner for MERA, a Mendix code-review system. You do not
      review code. You decide what deserves review...
 
  - agentKey: layer1
    model: claude-opus-5
    maxTokens: 16384
    temperature: 0
    outputSchemaKey: LayerFindings
    notes: "v3: added model-context tools"
    tools:
      - type: server_web_search
        allowedDomains: ["docs.mendix.com", "mendix.com", "community.mendix.com"]
        maxUses: 6
      - type: local
        name: find_callers
        budgetClass: graph
      - type: local
        name: find_references
        budgetClass: graph
      - type: local
        name: impact_radius
        budgetClass: graph
      - type: local
        name: describe_unit
        budgetClass: body
    budgets:
      graph: 8
      body: 3
      toolTokens: 20000
    systemPrompt: |
      You are a Mendix Platform Expert performing the technical guideline
      layer of a code review...
 
  - agentKey: layer2
    model: claude-opus-5
    maxTokens: 16384
    temperature: 0
    outputSchemaKey: LayerFindings
    tools:
      - type: mcp
        serverName: mera-bestpractices
        urlConstant: MERA.BpMcpUrl
        allowedTools: [search_best_practices, get_best_practice, list_categories]
    systemPrompt: |
      You are the guardian of this organisation's Mendix engineering standards...
 
  - agentKey: layer3
    model: claude-opus-5
    maxTokens: 16384
    temperature: 0
    outputSchemaKey: FunctionalFindings
    tools:
      - {type: local, name: find_callers,     budgetClass: graph}
      - {type: local, name: find_references,  budgetClass: graph}
      - {type: local, name: impact_radius,    budgetClass: graph}
      - {type: local, name: describe_unit,    budgetClass: body}
      - {type: local, name: search_model,     budgetClass: body}
    budgets:
      graph: 15
      body: 6
      toolTokens: 25000
    systemPrompt: |
      You are a senior functional analyst reviewing whether a Mendix change
      actually delivers what was asked for...
 
  - agentKey: synthesizer
    model: claude-opus-5
    maxTokens: 12288
    temperature: 0
    outputSchemaKey: SynthesisOutput
    tools: []
    systemPrompt: |
      You are the lead reviewer's assistant...
```
 
### 3.4 The import action
 
`ACT_Admin_ImportAgentDefinitions(FileDocument yaml)`:
 
1. Parse YAML in a Java action (SnakeYAML, or convert to JSON and use Jackson — which is already on the classpath).
2. For each agent: compare `systemPrompt` + `toolConfigJson` + model settings against the current active row. **If nothing changed, skip.** Otherwise deactivate the current row and insert a new one at `version + 1`.
3. Validate `outputSchemaKey` resolves to a registered POJO — fail the import if not. Better to reject a bad YAML at import than to discover it mid-run.
4. Log a summary: which agents changed, old → new version.
**▶ The "skip if unchanged" step matters more than it looks.** Without it, every import bumps every version, and `ReviewRun.agentDefinitionVersion` stops meaning anything. Versions should only move when something actually moved.
 
### 3.5 Schemas as code
 
`outputSchemaKey` names a Java class. The SDK derives the JSON schema from the POJO and enforces the response shape via `.outputConfig()`:
 
```java
// javasource/mera/schema/LayerFindings.java
@JsonClassDescription("The complete set of findings from one review layer.")
public class LayerFindings {
    @JsonPropertyDescription("Findings, most severe first. Empty is a valid result.")
    public List<Finding> findings;
 
    @JsonPropertyDescription("Concerns you could not ground in a source. May be empty.")
    public List<String> uncoveredConcerns;
 
    @JsonPropertyDescription("Set true if your tools failed and you could not review.")
    public boolean toolFailure;
}
 
@JsonClassDescription("A single review finding.")
public class Finding {
    @JsonPropertyDescription("Stable ID within this batch, e.g. L1-0003")
    public String findingId;
 
    @JsonPropertyDescription("Blocker | Major | Minor | Info")
    public String severity;
 
    @JsonPropertyDescription("0.0–1.0. Be honest; low confidence is useful, silence is not.")
    public double confidence;
 
    @JsonPropertyDescription("Qualified name of the unit that CHANGED in this commit range.")
    public String qualifiedName;
 
    @JsonPropertyDescription("Units impacted but NOT changed, discovered via model tools.")
    public List<AffectedUnit> affectedUnits;
 
    @JsonPropertyDescription("Verbatim excerpt from the supplied material or a tool result.")
    public String evidence;
 
    // ... rationale, citations, recommendation, effort, falsePositiveRisk
}
```
 
**▶ Write the `@JsonPropertyDescription` text as if it were prompt text, because it is.** The model reads these descriptions. `"0.0–1.0. Be honest; low confidence is useful, silence is not."` measurably outperforms `"confidence score"`. This is the highest-leverage, least-obvious prompt surface in the whole system — most people fill these in like Javadoc and leave value on the table.
 
### 3.6 Where the orchestration actually lives
 
Not in a file. In `TQ_ReviewRun_Execute` and its sub-microflows — Part 6. The "orchestrator" you imagined as an agent is, in this design, a microflow plus one small LLM call (the Planner). That's the trade the architecture doc argued for in §5.1: you keep the intelligence where it adds value (scoping) and keep the control flow where you can see it.
 
---
 
## Part 4 — `JA_InvokeAgent`
 
The heart of the system. One Java action, five agents, one place to fix bugs.
 
### 4.1 Signature
 
```
JA_InvokeAgent(AgentInvocation invocation) : AgentInvocation
```
 
Everything in and out through one entity. Mendix Java action parameters are painful for anything structured, and this way the audit row and the call parameters are the same object — you physically cannot invoke an agent without leaving a trace.
 
`AgentInvocation` fields, in:
 
```
agentDefinition (assoc)   the immutable prompt row
reviewBatch     (assoc)   nullable — Planner and Synthesizer have no batch
userContentJson           the actual payload for this call
cachePrefixJson           stable content to place before the cache breakpoint
toolContextJson           {workspaceId, reviewRunId} — for local tool dispatch
```
 
out:
 
```
stopReason, parsedOutputJson, rawRequest/rawResponse (FileDocument),
inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens,
toolCallsGraph, toolCallsBody, toolTokensUsed,
costEstimate, latencyMs, status, errorDetail
```
 
### 4.2 The skeleton
 
```java
public class JA_InvokeAgent extends CustomJavaAction<IMendixObject> {
 
    @Override
    public IMendixObject executeAction() throws Exception {
        AgentInvocation inv = AgentInvocation.initialize(getContext(), __invocation);
        AgentDefinition def = inv.getAgentInvocation_AgentDefinition();
        AgentConfig cfg = AgentConfig.parse(def.getToolConfigJson());
 
        long start = System.nanoTime();
        try {
            MessageCreateParams.Builder b = MessageCreateParams.builder()
                .model(def.getModel())
                .maxTokens(def.getMaxTokens())
                .temperature(def.getTemperature());
 
            applySystem(b, def, inv);      // §4.3
            applyTools(b, cfg, def);       // §4.4
            b.addUserMessage(inv.getUserContentJson());
 
            // Structured output: the SDK derives the schema from the POJO
            // and enforces it, so no defensive parsing downstream.
            b.outputConfig(SchemaRegistry.resolve(def.getOutputSchemaKey()));
 
            Message finalMessage = runToolLoop(b, cfg, inv);   // §4.5
 
            record(inv, finalMessage);
            inv.setStatus(Status.Succeeded);
 
        } catch (BudgetExceededException e) {
            inv.setStatus(Status.BudgetExceeded);
            inv.setErrorDetail(e.getMessage());
        } catch (AnthropicServiceException e) {
            inv.setStatus(Status.Failed);
            inv.setErrorDetail(e.statusCode() + ": " + e.getMessage());
        } finally {
            inv.setLatencyMs((System.nanoTime() - start) / 1_000_000);
            inv.commit();   // ALWAYS. Success or failure.
        }
        return inv.getMendixObject();
    }
}
```
 
**▶ `inv.commit()` in `finally` is not defensive coding, it's the product.** Your `AgentInvocation` table is simultaneously your audit trail, your cost report, your debugging surface, and your prompt-regression dataset. A failed invocation is *more* interesting than a successful one. Commit unconditionally.
 
### 4.3 System prompt and cache breakpoints
 
```java
private void applySystem(MessageCreateParams.Builder b,
                         AgentDefinition def, AgentInvocation inv) {
    List<TextBlockParam> system = new ArrayList<>();
 
    // 1. The agent's system prompt — stable across every batch in the run.
    system.add(TextBlockParam.builder()
        .text(def.getSystemPrompt())
        .build());
 
    // 2. Stable per-run context: guideline corpus, requirements doc, BP taxonomy.
    //    Whatever is identical for every batch of this layer in this run.
    String prefix = inv.getCachePrefixJson();
    if (prefix != null && !prefix.isBlank()) {
        system.add(TextBlockParam.builder()
            .text(prefix)
            // The breakpoint goes on the LAST stable block.
            .cacheControl(CacheControlEphemeral.builder().build())
            .build());
    }
 
    b.systemOfTextBlockParams(system);
}
```
 
**▶ Use `systemOfTextBlockParams`, not `system(String)`.** The plain string overload gives you nowhere to hang `cache_control`. This is a small API detail with a large cost consequence.
 
**▶ Where the breakpoint goes.** Everything *before* a breakpoint is cached; everything after is fresh. So: system prompt → stable run context → **breakpoint** → per-batch change units. On a 20-batch run, layers 1–3 re-send the same multi-thousand-token guideline corpus twenty times; cached, you pay ~10% for reads after the first. This is the single largest cost lever in the design and it's about ten lines of code.
 
Order your content stable-to-volatile. Always. If you find yourself putting something volatile early because it "reads better", you're paying real money for aesthetics.
 
### 4.4 Assembling tools
 
Three kinds, and the distinction drives everything about the loop:
 
| Kind | Executed by | Appears in loop? |
|---|---|---|
| `server_web_search` | Anthropic, server-side | No |
| `mcp` | Anthropic, server-side, calling your MCP server | No |
| `local` | Your Java action, calling a microflow | **Yes** |
 
```java
private void applyTools(MessageCreateParams.Builder b,
                        AgentConfig cfg, AgentDefinition def) {
 
    for (ToolSpec t : cfg.tools()) {
        switch (t.type()) {
 
            case SERVER_WEB_SEARCH -> b.addTool(
                WebSearchTool20260209.builder()
                    .maxUses(t.maxUses())
                    .allowedDomains(t.allowedDomains())
                    .build());
 
            case MCP -> {
                // The MCP connector is configured on the request body, not as
                // a tool. Check whether your SDK version has a first-class
                // builder; if not, this is what putAdditionalBodyProperty
                // exists for.
                b.putAdditionalBodyProperty("mcp_servers", JsonValue.from(List.of(
                    Map.of("type", "url",
                           "name", t.serverName(),
                           "url", constant(t.urlConstant()),
                           "authorization_token", mintBpToken()))));
                b.addBeta(AnthropicBeta.of("mcp-client-2025-11-20"));
                b.addTool(mcpToolset(t));   // allowlist, default disabled
            }
 
            case LOCAL -> b.addTool(Tool.builder()
                .name(t.name())
                .description(LocalToolCatalog.description(t.name()))
                .inputSchema(LocalToolCatalog.schema(t.name()))
                .build());
        }
    }
}
```
 
For local tools, build the schema explicitly rather than from a POJO — the tool set is data-driven, so you can't reference compile-time classes:
 
```java
// LocalToolCatalog.schema("find_callers")
Tool.InputSchema.builder()
    .properties(Tool.InputSchema.Properties.builder()
        .putAdditionalProperty("qualifiedName", JsonValue.from(Map.of(
            "type", "string",
            "description", "Fully qualified name, e.g. Sales.ACT_Order_Confirm")))
        .putAdditionalProperty("transitive", JsonValue.from(Map.of(
            "type", "boolean",
            "description", "Follow callers of callers. Default false.")))
        .putAdditionalProperty("maxDepth", JsonValue.from(Map.of(
            "type", "integer",
            "description", "1–3. Ignored unless transitive. Hard-capped at 3.")))
        .build())
    .putAdditionalProperty("required", JsonValue.from(List.of("qualifiedName")))
    .build();
```
 
**▶ Tool descriptions are prompt engineering, not documentation.** `"Read an unchanged unit's full definition. Expensive — prefer the graph tools unless you must see actual logic."` is a better description than `"Returns the MDL for a unit."` The model uses this text to decide *whether* to call, and cost discipline you express here is cheaper than cost discipline you enforce with budgets.
 
### 4.5 The tool loop
 
```java
private Message runToolLoop(MessageCreateParams.Builder b,
                            AgentConfig cfg, AgentInvocation inv) {
 
    Budget budget = Budget.from(cfg);          // graph, body, toolTokens
    int iterations = 0;
    final int MAX_ITERATIONS = 20;             // hard stop, always
 
    while (true) {
        if (++iterations > MAX_ITERATIONS) {
            throw new BudgetExceededException("iteration cap reached");
        }
 
        Message msg = callWithRetry(b.build());   // §4.7
        accumulateUsage(inv, msg);
 
        if (!"tool_use".equals(msg.stopReason().map(Object::toString).orElse(""))) {
            return msg;                            // done
        }
 
        // Echo the assistant turn back verbatim — including any text or
        // thinking blocks alongside the tool_use. Dropping them corrupts
        // the conversation and degrades subsequent reasoning.
        b.addAssistantMessageOfContentBlockParams(toParams(msg.content()));
 
        List<ContentBlockParam> results = new ArrayList<>();
        for (ToolUseBlock use : toolUses(msg)) {
            results.add(ToolResultBlockParam.builder()
                .toolUseId(use.id())
                .contentAsJson(dispatch(use, budget, inv))
                .build());
        }
        b.addUserMessageOfContentBlockParams(results);
    }
}
```
 
And the dispatcher, where the budget lives:
 
```java
private Object dispatch(ToolUseBlock use, Budget budget, AgentInvocation inv) {
    BudgetClass klass = LocalToolCatalog.budgetClass(use.name());
 
    if (!budget.tryConsume(klass)) {
        // A tool RESULT, not an exception. This is the important bit.
        return Map.of(
            "budgetExhausted", true,
            "budgetClass", klass.name().toLowerCase(),
            "message", "No further " + klass + " queries available. "
                     + "Conclude with what you have and note the limitation.");
    }
 
    long t0 = System.nanoTime();
    Object result;
    try {
        result = switch (use.name()) {
            case "find_callers", "find_references", "impact_radius" ->
                callMicroflow(use, inv);                       // Part 7
            case "describe_unit", "search_model" ->
                callExtractorWorkspace(use, inv);              // §1.8
            default ->
                Map.of("error", "unknown tool: " + use.name());
        };
    } catch (Exception e) {
        // Degrade, never explode. The agent can work around a dead tool.
        result = Map.of("unavailable", true, "reason", e.getMessage());
    }
 
    String json = Json.write(result);
    budget.consumeTokens(json.length() / 4);
    logModelQuery(inv, use, json.length(), (System.nanoTime() - t0) / 1_000_000);
    return truncateIfWide(result);      // cap caller lists at ~40 — §7.5
}
```
 
Four things in that dispatcher earn their place:
 
1. **Budget exhaustion returns a tool result.** Throwing gives the model nothing to work with and wastes the entire conversation. Returning a structured "you're out" lets it write a good finding and note the limitation. Test this path deliberately by setting the budget to 1.
2. **Failures degrade to `{"unavailable": true, reason}`.** Same reasoning. Your extractor will be down sometimes; the review should still ship.
3. **Every call logs a `ModelQuery` row.** After two weeks you'll know empirically whether 15/6 is the right budget, which tools actually get used, and which agent wanders. You cannot tune what you don't measure, and this table is nearly free.
4. **`truncateIfWide`.** A unit called from 300 places will otherwise dump 300 entries into context. Cap at ~40 and append `"and 260 more across 18 modules"` — the count *is* the signal.
**On `BetaToolRunner`:** the SDK ships an automatic tool-execution loop. Don't use it here. You need budget enforcement, per-call logging, graceful degradation and truncation *inside* the loop. Hand-rolling is ~60 lines and gives you all four.
 
### 4.6 Getting structured output back
 
With `.outputConfig(POJO.class)`, the SDK enforces the schema and gives you a typed object. That removes the whole category of "the model returned prose around the JSON" bugs.
 
Then run your own verification pass before persisting — this is where the architecture doc's mechanical hallucination check lives:
 
```java
// Every finding's evidence must appear verbatim in the material the model
// was given, or in something a tool returned to it.
for (Finding f : output.findings) {
    if (!haystack.contains(normalise(f.evidence))) {
        f.severity = "Info";
        f.falsePositiveRisk = "High";
        f.unverifiedEvidence = true;
        counters.evidenceMismatch++;
    }
}
```
 
**▶ This one check does more for trust than any amount of prompt tuning.** It's a substring search. Build it in Phase 2, not Phase 5 — and track `evidenceMismatch` as a metric, because a spike after a prompt change tells you instantly that you broke something.
 
Also enforce here: findings whose `qualifiedName` isn't a ChangeUnit in this run get demoted or dropped, and L1/L2 findings at Minor+ without a citation get demoted to Info. Prompts *ask* for these rules; code *enforces* them. Never rely on the prompt alone for an invariant you actually depend on.
 
### 4.7 Retries
 
Set `maxRetries(0)` on the client and own it, because you want to record attempts:
 
```java
private Message callWithRetry(MessageCreateParams p) {
    int attempt = 0;
    while (true) {
        try {
            return AnthropicClientHolder.get().messages().create(p);
        } catch (RateLimitException e) {
            if (++attempt > 4) throw e;
            sleep(retryAfterOr(e, backoff(attempt)));
        } catch (InternalServerException | AnthropicIoException e) {
            if (++attempt > 4) throw e;
            sleep(backoff(attempt));
        }
        // BadRequest / Unauthorized: do not retry. Fail loudly.
    }
}
 
private Duration backoff(int attempt) {
    long base = Math.min(30_000, (long) (1000 * Math.pow(2, attempt)));
    return Duration.ofMillis(base + ThreadLocalRandom.current().nextInt(1000));
}
```
 
Honour `Retry-After` when present. Jitter always — six batches launching simultaneously and retrying in lockstep is a self-inflicted thundering herd.
 
---
 
## Part 5 — The Java action inventory
 
| Action | Signature | What's non-obvious |
|---|---|---|
| `JA_ResolveCommits` | `(Repository, String branch, String baseSha, String headSha) : ResolveResult` | JGit `LsRemoteCommand` — **no clone needed**. Pure Java, works on Mendix Cloud. Validates SHAs exist and are ancestrally ordered before you pay for extraction. |
| `JA_RequestExtraction` | `(ReviewRun) : Boolean` | HTTP POST to the extractor with a 15-min timeout. Mints a short-lived transport token. Decrypts the PAT at the last possible moment; never assigns it to a Mendix attribute. |
| `JA_ImportExtractionResult` | `(ReviewRun, FileDocument json) : Boolean` | Streams the JSON. A 400-unit change set is large; don't `String`-ify it. |
| `JA_OpenWorkspace` | `(ReviewRun) : String workspaceId` | Lazy — called only if any ChangeUnit has `impactAnalysisRequired`. Stores `workspaceExpiresAt`. |
| `JA_CloseWorkspace` | `(ReviewRun) : Boolean` | Idempotent, and called from a `finally`-equivalent error handler. |
| `JA_InvokeAgent` | `(AgentInvocation) : AgentInvocation` | Part 4. |
| `JA_EstimateTokens` | `(String) : Long` | `length / 3.6` for MDL. Don't call the count-tokens API per unit — hundreds of round-trips for a batching heuristic isn't worth it. |
| `JA_ParseYamlAgents` | `(FileDocument) : String json` | YAML → JSON so microflows can iterate it. |
| `JA_MintBpToken` | `(ReviewRun) : String` | Short-lived bearer for the BP MCP server, scoped to the run. |
 
**▶ On `JA_ResolveCommits` and JGit.** This is the one place JGit belongs on the Mendix side. `ls-remote` is a single lightweight network call — no working copy, no `.mpr`, no native binary. It lets you validate the PAT and the commit range *before* dispatching a 90-second extraction, so bad input fails in two seconds with a clear message. Cheap validation early is worth a lot in a system where the next step is expensive.
 
---
 
## Part 6 — Where everything sits in the process
 
### 6.1 The map
 
```
ACT_ReviewRun_Start  (user clicks Run)
  ├─ validate repo + credential not expired
  ├─ JA_ResolveCommits                        → fail fast on bad input
  ├─ create ReviewRun (status = Extracting)
  └─ queue TQ_ReviewRun_Prepare               ← returns to the user immediately
 
TQ_ReviewRun_Prepare                          [Task Queue, own transaction]
  ├─ JA_RequestExtraction  →  JA_ImportExtractionResult
  ├─ status = Planning
  ├─ SUB_RunPlanner
  │    └─ JA_InvokeAgent(planner)
  ├─ apply verdicts to ChangeUnits; set impactAnalysisRequired
  ├─ SUB_BuildBatches                         → ReviewBatch rows, status Pending
  ├─ if any impactAnalysisRequired → JA_OpenWorkspace
  ├─ status = Reviewing
  └─ for each ReviewBatch: queue TQ_ReviewBatch_Execute     ← the fan-out
 
TQ_ReviewBatch_Execute(ReviewBatch)           [Task Queue, own transaction, xN]
  ├─ build userContentJson + cachePrefixJson
  ├─ JA_InvokeAgent(layerN)
  ├─ SUB_VerifyAndPersistFindings             → §4.6 checks
  ├─ batch.status = Completed | Failed | Skipped
  └─ SUB_TryAdvanceRun(reviewRun)             ← the join
 
SUB_TryAdvanceRun                             [the tricky bit — §6.3]
  ├─ lock the ReviewRun row
  ├─ if any batch still Pending/Running → return
  ├─ JA_CloseWorkspace
  ├─ status = Synthesizing
  └─ queue TQ_ReviewRun_Synthesize
 
TQ_ReviewRun_Synthesize
  ├─ JA_InvokeAgent(synthesizer)
  ├─ create SynthesisReport
  └─ status = Completed | PartiallyCompleted
```
 
### 6.2 Fan-out: use the Task Queue, not threads
 
You need three layers × N batches running concurrently. The obvious move is `CompletableFuture` inside a Java action. **Don't.**
 
**▶ Why not threads.** A Mendix `IContext` is not thread-safe and carries a transaction. Spawning threads that touch Mendix objects gives you either exceptions or, worse, silent corruption. You *can* create a system context per thread, but then each thread has its own transaction and your careful commit semantics evaporate.
 
The Task Queue gives you exactly what you need:
 
- **Concurrency** — set the queue's thread count to your desired cap (4–6). That single setting is your semaphore.
- **Transaction scoping** — each task runs in its own transaction. Which solves the next problem for free.
- **Retry and visibility** — built in, with a monitoring page you didn't write.
**▶ The transaction problem it solves.** A `JA_InvokeAgent` call takes 30–180 seconds. If it runs inside `TQ_ReviewRun_Prepare`'s transaction along with everything else, you hold a database transaction open for the entire run — minutes, across many LLM calls. On a busy database that's lock contention and a connection-pool exhaustion incident waiting for a quiet Tuesday. One task per batch means one transaction per batch: opened, LLM call, findings written, committed, released.
 
This is the most important structural decision in Part 6, and it's invisible until it bites in production.
 
### 6.3 The join, and the race in it
 
Every batch finishing runs `SUB_TryAdvanceRun`. Two batches finishing simultaneously both see "all others complete" and both queue synthesis. Now you have two reports and double the cost.
 
Fix it with a database-level lock on the `ReviewRun` row:
 
1. Retrieve the `ReviewRun` with a locking read — in Mendix that's an OQL/`SELECT ... FOR UPDATE` via a Java action, or a dedicated `RunLock` entity you insert into with a uniqueness constraint on `(reviewRunId, phase)`.
2. Re-check batch statuses **inside** the lock.
3. Transition status and queue synthesis.
4. Commit, releasing the lock.
The uniqueness-constraint approach is the simplest correct one: insert a `RunPhaseTransition{reviewRunId, phase}` row with a unique index. Whoever inserts successfully advances the run; whoever gets a constraint violation catches it and returns. No explicit locking, no deadlock risk, and it's obvious to the next person reading it.
 
**▶ Test this deliberately.** Set the queue to 8 threads and run a 20-batch review. If you get one report, you're fine. Races that only appear under load are the worst kind to find in production, and this one is trivially reproducible in a test.
 
### 6.4 Progress
 
`ReviewRun.progress` = completed batches / total, weighted a little (extraction ~15%, planning ~5%, review ~65%, synthesis ~15%). The UI polls with a refresh-timer nanoflow every 3–5s. Mendix has no clean server push; polling is the honest answer and at this cadence it costs nothing.
 
---
 
## Part 7 — The local tool bridge
 
When Layer 3 calls `find_callers`, this happens:
 
```
Claude → tool_use block
       → JA_InvokeAgent.dispatch()
       → Core.microflowCall("MERA.SUB_Tool_FindCallers")
       → microflow: XPath over ModelReference
       → returns a JSON string
       → ToolResultBlockParam
       → back to Claude
```
 
The Java side:
 
```java
private Object callMicroflow(ToolUseBlock use, AgentInvocation inv) throws Exception {
    Map<String, Object> args = use._input().convert(Map.class);
 
    IMendixObject req = Core.instantiate(getContext(), "MERA.ToolRequest");
    req.setValue(getContext(), "ToolName", use.name());
    req.setValue(getContext(), "ArgsJson", Json.write(args));
    req.setValue(getContext(), "ReviewRunId", inv.getReviewRunId());
 
    return Core.microflowCall("MERA.SUB_Tool_Dispatch")
               .withParam("Request", req)
               .execute(getContext());        // returns String (JSON)
}
```
 
Three things to get right:
 
**Use `getContext()`, not a fresh system context.** The tool query must run under the same user whose PAT and project access authorised this run. Creating a system context here would silently bypass the XPath access rules that enforce your per-project isolation — a quiet privilege escalation inside a feature designed to read arbitrary application internals. Use the caller's context and let Mendix security do its job.
 
**Enforce caps in the microflow, not the schema.** The schema says `maxDepth: 1–3`. The model can send 9. Clamp server-side. Never trust a tool argument you didn't compute.
 
**Return JSON strings, not Mendix objects.** The tool result goes back to the API as JSON anyway. Building object trees and serialising them is work for nothing; have the microflow produce the string directly.
 
### 7.1 What the microflows do
 
- `SUB_Tool_FindCallers` — XPath over `ModelReference` where `toUnit = X`. If `transitive`, loop to clamped depth with a visited set. **Index `ModelReference.toUnit` and `fromUnit`** or this crawls on a 100k-edge graph.
- `SUB_Tool_FindReferences` — same, filtered by `referenceKind`.
- `SUB_Tool_ImpactRadius` — BFS to depth, return counts grouped by module and unit type plus the top N most-connected consumers. Not the full list — the *shape* of the blast radius is what's useful.
`describe_unit` and `search_model` don't go to microflows at all — they're HTTP calls to the leased workspace (§1.8), because only the sidecar has the `.mpr`.
 
---
 
## Part 8 — Build order
 
Each milestone has a "you're done when" that you can actually check.
 
**M1 — Extractor, standalone.** *Done when:* `extractor-cli extract` produces a valid `fixture.json` from three real MPRv2 repos on different Mendix versions, and one of them has a unit that fails to render but doesn't fail the run.
 
**M2 — SDK smoke test.** *Done when:* `JA_AnthropicSmokeTest` returns `MERA online` from a deployed environment. (§2.3. Do this on day one.)
 
**M3 — Fixture-driven single agent.** Import `fixture.json` as test data. Build `AgentDefinition`, `JA_InvokeAgent` without the tool loop, Layer 1 only, no web search. *Done when:* you get structured findings out of a real change set and the evidence-verification check passes.
 
**M4 — Prompt loop.** `agents.yaml` + import action. *Done when:* you can change a prompt and re-run against a fixture in under a minute, without a deploy. **This is the moment the project starts moving fast** — everything before it is scaffolding for this.
 
**M5 — Real pipeline.** Wire the extractor to Mendix, Task Queue, batching, progress. *Done when:* a review runs end to end from the UI against a live repo.
 
**M6 — Three layers + synthesis.** Add L2's MCP wiring, L3's ADO fetch, the Planner, the Synthesizer, parallel fan-out. *Done when:* the join race test (§6.3) passes with 8 queue threads.
 
**M7 — Model context.** Tier 0 and Tier 1 first. *Done when:* the `ModelQuery` table has two weeks of data telling you whether Tier 2 is worth building.
 
**M8 — Usability.** Triage, suppression, export, cost dashboard.
 
**▶ On M4.** Resist the urge to skip straight to M5 because the pipeline feels like "the real work". The prompt loop is the thing you'll use hundreds of times; the pipeline you'll build once. Optimising the loop you run most is almost always right.
 
---
 
## Part 9 — The traps
 
Ordered roughly by how much time they'll cost you if you don't see them coming.
 
1. **The `mx` version matrix.** §1.3. Acquiring and trimming the binaries is fully automated (`fetch-mx.sh add-trimmed-version`, one call per line of `mx-versions.txt`). The real remaining work is confirming which versions the matrix actually needs — see the compatibility investigation in `MERA-session-status.md`. Note the runtime dependency is `libicu72`, not a JRE.
2. **Jackson/OkHttp classloader conflicts.** §2.2. Find it on day one with the smoke test, not on day twenty with a `NoSuchMethodError` in production.
3. **A transaction held open across LLM calls.** §6.2. Invisible until it's a production incident. The Task Queue solves it — use it from the start rather than refactoring in.
4. **The synthesis join race.** §6.3. Two reports, double cost, reproducible in a test you can write in ten minutes.
5. **Cache breakpoints in the wrong place.** §4.3. Silently expensive. Watch `cacheReadTokens` on `AgentInvocation` — if it's near zero after the first batch of a run, your ordering is wrong.
6. **Constructing the Anthropic client per call.** §2.5. Thread and connection leak under load.
7. **`mxcli` failing a single unit and taking down the extraction.** §1.4. Per-unit degradation from the start.
8. **PAT in the clone URL.** §1.7. Ends up in `.git/config` and your logs. Credential helper on tmpfs, and a log-redaction test.
9. **Trusting the prompt to enforce invariants.** §4.6. "Every finding must cite a source" is a hope until it's a code check.
10. **Throwing on budget exhaustion.** §4.5. Wastes the whole conversation. Return a tool result.
11. **A system context in the tool bridge.** §7. Quiet bypass of your per-project access rules.
12. **Unindexed `ModelReference`.** §7.1. Fine on your test app, unusable on a real one.
13. **Editing prompts in place.** §3.2. Destroys your ability to explain a three-week-old finding.
14. **`GIT_TERMINAL_PROMPT` unset.** §1.7. A bad credential hangs instead of failing.
15. **Assuming the on-disk `.mpr` filename matches its internal self-reference.** §1.5. A real repo hit this: git-tracked as `MERA.mpr`, internally self-referencing `App.mpr`; `mx diff` refuses to open it under the "wrong" name. Glob for the real filename, don't hardcode.
16. **A commit that captures a Studio Pro version-upgrade migration, fed straight into `mx diff`/`dump-mpr`.** §1.5. Produces a genuinely confusing parser exception (`Expected '$ID' as the first property of a storage object...`) that looks like a tooling bug but isn't — detect via `Projects$ProjectConversion` in `mx analyze-mpr`'s output and treat as an explicit unsupported case.
17. **Assuming `mx diff`'s `id` is directly usable as a qualified name.** §1.5 step 7a. It's an internal GUID; `mxcli describe` needs `Module.Name`. Resolve via `mx dump-mpr --unit-type <types> --output-file <path>` (a real flag — not a bare positional output path) and match on `$ID` to get `$QualifiedName`, rather than parsing the raw `.mxunit` BSON file the GUID also happens to name on disk.
---
 
## Where to start tomorrow
 
Two things, in parallel, both cheap:
 
1. **The smoke test (M2).** Twenty minutes, and it de-risks the entire Java side.
2. **An hour with `mxcli` on a real repo.** Specifically: does a catalog query give you the reference graph in one shot (§1.6)? That answer determines whether §7 of the architecture doc is a cheap feature or an expensive one, and it's better to know before you plan around it.
Then M1. The extractor is where this project succeeds or fails — everything downstream is conventional application development you already know how to do.
 
---
 
## Sources
 
- [Managed Dependencies](https://docs.mendix.com/refguide/managed-dependencies/) · [mx Command-Line Tool](https://docs.mendix.com/refguide/mx-command-line-tool/) · [Merging and Diffing Commands](https://docs.mendix.com/refguide/mx-command-line-tool/merge/) · [MPR Dump](https://docs.mendix.com/refguide/mx-command-line-tool/dump-mpr/) · [MPR Analyze](https://docs.mendix.com/refguide/mx-command-line-tool/analyze-mpr/) · [Working with External Git Tools](https://docs.mendix.com/refguide/version-control-external-tools/) · [Installing Mendix Studio Pro without Admin Rights](https://docs.mendix.com/refguide/install/) · [mxbuild reference](https://docs.mendix.com/refguide/mxbuild/) · [Debian bookworm libicu72](https://packages.debian.org/bookworm/libicu72)
- [mxcli](https://github.com/mendixlabs/mxcli) · [mxcli documentation](https://www.mxcli.org/)
- [anthropic-sdk-java](https://github.com/anthropics/anthropic-sdk-java) · [Java SDK docs](https://platform.claude.com/docs/en/api/sdks/java) · [Claude API Java reference](https://github.com/anthropics/skills/blob/main/skills/claude-api/java/claude-api.md)
- [MCP connector](https://platform.claude.com/docs/en/agents-and-tools/mcp-connector) · [Web search tool](https://platform.claude.com/docs/en/agents-and-tools/tool-use/web-search-tool)