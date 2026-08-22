#!/usr/bin/env bash
set -euo pipefail

# fetch-mx.sh — download, probe, and (empirically) trim an mxbuild release,
# as a step towards populating MERA's .mx-binaries/ with a lean copy.
#
# Usage:
#   ./fetch-mx.sh inspect      <mxversion>   Download + extract to scratch, report layout. Writes NOTHING to .mx-binaries/.
#   ./fetch-mx.sh probe        <mxversion>   Try to actually run mx --help / mx diff --help, working around the ICU issue.
#   ./fetch-mx.sh trial-trim   <mxversion>   Move aside every confirmed-safe candidate (merged, see TRIM_CANDIDATES),
#                                             sweep every scattered *.pdb, then retest mx. Reversible via 'restore'.
#   ./fetch-mx.sh deep-inspect <mxversion>   Break down what's LEFT after trial-trim, to look for further candidates.
#   ./fetch-mx.sh add-trimmed-version <mxversion> [--replace]
#                                             Convenience: inspect + trial-trim + finalize in one run, for a
#                                             version you already trust the flow for.
#   ./fetch-mx.sh restore      <mxversion>   Undo trial-trim — move everything back.
#   ./fetch-mx.sh clean        <mxversion>   Remove the scratch download + extraction for a version.
#   ./fetch-mx.sh list                       List versions currently present in .mx-binaries/.
#
# (trial-trim-2 used to be a separate low-risk second pass; it's now merged
# into trial-trim — one candidate list, one pass, see the TRIM_CANDIDATES
# comment below for what's in it and why.)
#
# Typical flow, first time through (or any time you want to watch each step
# and decide whether to probe/restore in between):
#   ./fetch-mx.sh inspect 11.13.0        # first run: download + look
#   ./fetch-mx.sh probe 11.13.0          # confirm mx actually runs (see ICU note below)
#   ./fetch-mx.sh trial-trim 11.13.0     # move aside the big suspect folders, retest
#   ./fetch-mx.sh restore 11.13.0        # if trial-trim broke it, put everything back
#   ./fetch-mx.sh finalize 11.13.0       # once happy, copy the trimmed tree into .mx-binaries/
#
# Once you trust that flow for a version (e.g. adding a second Mendix
# version after 11.13.0 is already validated), 'add-trimmed-version' runs
# inspect + trial-trim + finalize back to back in one call — same
# functions, same safety gates, just chained.
#
# --- Corrected understanding, from a real 11.13.0 download ------------------
#
# mxbuild/mx is a self-contained **.NET** application, not Java. The manual's
# original "mxbuild is JVM-based, needs a JRE" was wrong — corrected here from
# actual evidence: modeler/mx and modeler/mxbuild ship next to mxbuild.dll,
# mxbuild.deps.json, mxbuild.runtimeconfig.json, and a bundled CoreCLR
# (libcoreclr.so, libclrjit.so, System.*.dll). The crash this script first hit
# ("Couldn't find a valid ICU package... Mendix.MxToolset.Program.Main") is a
# .NET globalization stack trace, not a Java one. The .jar files that DO exist
# in the tarball live under runtime/ — that's the actual Mendix Runtime
# (Java-based, used to run/deploy an app), unrelated to mx/mxbuild itself.
#
# Consequence for the Dockerfile: no JRE needed for mx/mxbuild — install
# libicu instead (the exact package name is host-specific; Jord's WSL2 Ubuntu
# needed libicu78 specifically, confirm the right one for whatever base image
# the container actually uses). DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1 was
# tried as a cheaper alternative and is CONFIRMED NOT TO WORK for this build,
# on any machine: Mendix.Modeler.Internationalization.LocalizationProvider
# hardcodes loading the real 'en-us' culture at startup, and invariant mode
# only supports the truly-invariant culture — structurally incompatible,
# not a config problem. libicu is the only real fix. `probe` still checks
# invariant mode as a diagnostic (useful for OTHER mx versions this hasn't
# been tested against), but don't expect it to save you here.
#
# --- Trimming: confirmed empirically, not guessed ---------------------------
#
# A real 11.13.0 extraction came back at 1.6G, dominated by:
#   modeler/tools        876M   (dev-tooling of some kind — still unidentified)
#   runtime/              410M   (the Java-based Mendix Runtime — not mx/mxbuild)
#   modeler/ide-client    112M   (Studio Pro's web IDE: js/ts/svg/html/css)
# trial-trim moved all three aside (not deleted) and re-ran mx --help and
# mx diff --help against what was left: both came back clean — full command
# list, full diff usage text (which is also how we learned '-l /
# --loose-version-check' is a real flag, confirming a guess the architecture
# doc had flagged as unverified). Result: 1.6G -> 230M, confirmed 2026-08-22.
# CAVEAT: --help proves mx starts and parses args without these folders; it
# does NOT prove the diff algorithm itself runs clean end-to-end (a real
# `mx diff a.mpr b.mpr out.json` might still reach for something under
# tools/ that --help never touches). Run a real diff against two real .mpr
# snapshots before fully trusting this for production.

MX_CDN_BASE="https://cdn.mendix.com/runtime"
SCRATCH_ROOT="${MX_FETCH_SCRATCH:-$HOME/.mx-fetch-scratch}"
REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
DEST_ROOT="${MX_BINARIES_DIR:-$REPO_ROOT/.mx-binaries}"

# --- Trim candidates, merged, 2026-08-22 -------------------------------------
#
# Originally built up in stages (a first round cutting whole separate apps,
# then a low-risk second round inside mx's own assembly graph, then a set of
# manually-tested, higher-risk single files) and now merged into one list —
# every entry below has been empirically confirmed, not guessed:
#   - stage 1: whole separate co-located apps, not part of mx's own .NET
#     assembly graph at all — modeler/ide-client (Studio Pro's web IDE),
#     modeler/tools, runtime (the Java-based Mendix Runtime). Confirmed via
#     mx --help / mx diff --help.
#   - stage 2: low-risk, standard omissions inside mx's own graph — *.pdb
#     debug symbols (handled separately below, they're scattered, not one
#     folder), satellite culture folders (ru/ja/ko/fr/de/it/es/zh-Hant/
#     zh-Hans/pl/pt-BR/cs/tr — localized .resources.dll strings; this is a
#     headless CLI that only ever runs en-us), Translations/*.po (IDE-side
#     UI text catalogs), and .NET's own optional diagnostics/debugger-attach
#     libs (libmscordaccore.so, libmscordbi.so, libcoreclrtraceptprovider.so).
#     Confirmed via mx --help / mx diff --help.
#   - stage 3: bigger, riskier files, confirmed SAFE by real end-to-end
#     testing this time, not just --help — Jord ran a real `mx diff` across
#     an image-only change (Images$ImageCollection, an image literally
#     Added) AND a second real diff spanning two actual commits (an image
#     change + a microflow addition together), both producing correct,
#     complete unitDifferences[] output with these removed:
#     libonnxruntime.so (22M, ML inference — turned out unused for diff),
#     libSkiaSharp.so (8.7M, image rendering — turned out unused even for an
#     image-touching diff, which was the specific risk this was held back
#     over), extensions/ (4.9M, IDE wizard modules), Licenses/ (license
#     text, no functional role).
#   - stage 4: the remaining "still untested" list from stage 3, actually
#     tested by Jord with a real diff — split into safe/not-safe below.
#     Mendix.Modeler.Theming.Legacy.dll (+ its .pdb, redundant with the
#     generic *.pdb sweep below so not separately listed), the NPOI.*
#     assembly family (NPOI.Core.dll, NPOI.OpenXmlFormats.dll,
#     NPOI.OOXML.dll — matched by glob, see move_candidate()), and
#     HotChocolate.Types.dll all turned out SAFE. Mendix.Modeler.Theming.dll
#     (the non-Legacy one — interesting split: Legacy is dead weight, the
#     mainline assembly is not) and CycloneDX.Core.dll turned out NOT safe —
#     see the confirmed-not-safe block below.
#
# CONFIRMED NOT SAFE, do not add — all tried for real, mx failed to start
# without them:
#   - Mendix.Modeler.WebUI.dll
#   - Mendix.Modeler.Theming.dll (NOT the same as Theming.Legacy.dll, which
#     IS safe — don't let the similar name cause a mix-up)
#   - CycloneDX.Core.dll
#
# CAVEAT that still stands: all real-diff validation so far has exercised
# the `diff` verb (plus a plain-file `dump-mpr`/`analyze-mpr` smoke test
# earlier in this project). If `build` or `create-project` verbs are ever
# needed for MERA, re-validate against those specifically before trusting
# this list for them — extensions/ in particular sounds like exactly the
# kind of thing 'create-project' templates might reach for.
#
# Paths are relative to the extraction root and may be an exact path OR a
# glob pattern (e.g. "modeler/NPOI.*") — move_candidate() below handles
# both. Re-validate against any other Mendix version before assuming this
# list transfers.
TRIM_CANDIDATES=(
  "modeler/ide-client"
  "modeler/tools"
  "runtime"
  "modeler/ru"
  "modeler/ja"
  "modeler/ko"
  "modeler/fr"
  "modeler/de"
  "modeler/it"
  "modeler/es"
  "modeler/zh-Hant"
  "modeler/zh-Hans"
  "modeler/pl"
  "modeler/pt-BR"
  "modeler/cs"
  "modeler/tr"
  "modeler/Translations"
  "modeler/libmscordaccore.so"
  "modeler/libmscordbi.so"
  "modeler/libcoreclrtraceptprovider.so"
  "modeler/libonnxruntime.so"
  "modeler/libSkiaSharp.so"
  "modeler/extensions"
  "modeler/Licenses"
  "modeler/Mendix.Modeler.Theming.Legacy.dll"
  "modeler/NPOI.*"
  "modeler/HotChocolate.Types.dll"
)

usage() {
  cat <<'EOF'
Usage:
  fetch-mx.sh inspect      <mxversion>   Download + extract to scratch, report on-disk layout.
  fetch-mx.sh probe        <mxversion>   Try to run mx --help / mx diff --help (works around the ICU issue).
  fetch-mx.sh trial-trim   <mxversion>   Move aside ALL confirmed-safe candidates (merged stages 1-3, see
                                          the TRIM_CANDIDATES comment) + sweep every scattered *.pdb.
                                          Retests mx. Reversible via 'restore'.
  fetch-mx.sh deep-inspect <mxversion>   Break down what's left after trial-trim, to look for further candidates.
  fetch-mx.sh finalize     <mxversion> [--replace]
                                        Copy the current (trimmed, if trial-trim has run) tree into
                                        .mx-binaries/<version>/. Refuses to overwrite an existing copy unless
                                        --replace is passed (e.g. TRIM_CANDIDATES grew since the last finalize).
  fetch-mx.sh add-trimmed-version <mxversion> [--replace]
                                        Convenience: inspect + trial-trim + finalize, back to back, in one run.
                                        For a version you already trust the flow for. --replace forwards to
                                        finalize (and is checked up front, before downloading anything).
  fetch-mx.sh restore      <mxversion>   Undo trial-trim.
  fetch-mx.sh clean        <mxversion>   Remove the scratch extraction + downloaded tar.gz for a version.
  fetch-mx.sh list                       List versions currently present in .mx-binaries/.

Note: 'trial-trim-2' has been merged into 'trial-trim' — everything that was
staged across trial-trim/trial-trim-2/manual testing is now one candidate
list, cut in a single pass. Just run 'trial-trim'.

Examples:
  ./fetch-mx.sh inspect 11.13.0
  ./fetch-mx.sh probe 11.13.0
  ./fetch-mx.sh trial-trim 11.13.0
  ./fetch-mx.sh deep-inspect 11.13.0
  ./fetch-mx.sh finalize 11.13.0
  ./fetch-mx.sh add-trimmed-version 11.12.0        # inspect + trial-trim + finalize in one go
  ./fetch-mx.sh add-trimmed-version 11.12.0 --replace
EOF
}

require() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing required tool: $1" >&2; exit 1; }
}

extract_dir_for() { echo "$SCRATCH_ROOT/mxbuild-${1}"; }
aside_dir_for()   { echo "$SCRATCH_ROOT/mxbuild-${1}.trimmed-aside"; }

mx_bin_for() {
  local extract_dir="$1"
  local mx_bin="$extract_dir/modeler/mx"
  [[ -e "$mx_bin" ]] || { echo ""; return; }
  chmod +x "$mx_bin" 2>/dev/null || true
  echo "$mx_bin"
}

# Moves one TRIM_CANDIDATES entry from extract_dir into aside_dir, preserving
# its relative path. Handles both an exact path ("modeler/extensions") and a
# glob pattern ("modeler/NPOI.*") — a plain `[[ -e ... ]]` check on a quoted
# variable never matches a glob (bash doesn't expand it there), so a glob
# entry needs its own expansion path or it silently no-ops every time.
move_candidate() {
  local extract_dir="$1" aside_dir="$2" c="$3"
  if [[ "$c" == *[\*\?\[]* ]]; then
    local matched=0 f rel
    shopt -s nullglob
    for f in "$extract_dir"/$c; do
      matched=1
      rel="${f#"$extract_dir"/}"
      mkdir -p "$aside_dir/$(dirname "$rel")"
      mv "$f" "$aside_dir/$rel"
      echo "moved aside: $rel"
    done
    shopt -u nullglob
    [[ "$matched" -eq 1 ]] || echo "(already absent, skipping: $c)"
  else
    if [[ -e "$extract_dir/$c" ]]; then
      mkdir -p "$aside_dir/$(dirname "$c")"
      mv "$extract_dir/$c" "$aside_dir/$c"
      echo "moved aside: $c"
    else
      echo "(already absent, skipping: $c)"
    fi
  fi
}

# Whether $1 (captured stdout+stderr from an mx invocation) shows mx actually
# started the .NET runtime successfully. Deliberately NOT based on exit code:
# this CLI's argument parser (CommandLineParser-style — "ERROR(S): Verb ...
# is not recognized") appears to exit non-zero even for a plain --help, so
# exit code alone can't distinguish "wrong flag, runtime is fine" from "the
# runtime never started". Only the two crash signatures actually seen so far
# count as "not started"; anything else (including a usage/verb error) means
# mx got past startup and is a live process.
mx_started_ok() {
  local out="$1"
  echo "$out" | grep -q "Unhandled exception" && return 1
  echo "$out" | grep -q "Couldn't find a valid ICU package" && return 1
  return 0
}

# Sets MX_WORKING_MODE to "plain" or "invariant" and returns 0 if mx starts
# successfully (see mx_started_ok) in either mode; returns 1
# (MX_WORKING_MODE="") if neither does. Silent — callers report their own
# diagnostics.
check_mx_mode() {
  local mx_bin="$1"
  local out
  out="$("$mx_bin" --help 2>&1)" || true
  if mx_started_ok "$out"; then
    MX_WORKING_MODE="plain"
    return 0
  fi
  out="$(DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1 "$mx_bin" --help 2>&1)" || true
  if mx_started_ok "$out"; then
    MX_WORKING_MODE="invariant"
    return 0
  fi
  MX_WORKING_MODE=""
  return 1
}

# Runs mx with whatever env MX_WORKING_MODE calls for.
mx_run() {
  local mx_bin="$1"; shift
  if [[ "$MX_WORKING_MODE" == "invariant" ]]; then
    DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1 "$mx_bin" "$@"
  else
    "$mx_bin" "$@"
  fi
}

inspect() {
  local version="$1"
  require curl
  require tar
  require du
  require find

  local tarball="$SCRATCH_ROOT/mxbuild-${version}.tar.gz"
  local extract_dir
  extract_dir="$(extract_dir_for "$version")"

  mkdir -p "$SCRATCH_ROOT"

  if [[ -f "$tarball" ]]; then
    echo "== reusing already-downloaded $tarball =="
  else
    local url="${MX_CDN_BASE}/mxbuild-${version}.tar.gz"
    echo "== downloading $url =="
    curl -fL --retry 3 --retry-delay 2 --progress-bar -o "$tarball" "$url"
  fi

  echo
  echo "== tarball size =="
  du -h "$tarball"

  rm -rf "$extract_dir"
  mkdir -p "$extract_dir"
  echo
  echo "== extracting =="
  tar -xzf "$tarball" -C "$extract_dir"

  echo
  echo "== extracted size (total) =="
  du -sh "$extract_dir"

  echo
  echo "== layout, depth 2, sorted by size (biggest first) =="
  du -ah --max-depth=2 "$extract_dir" 2>/dev/null | sort -rh | head -60

  echo
  echo "== file count by extension (top 20) =="
  find "$extract_dir" -type f | sed -n 's/.*\.\([a-zA-Z0-9]*\)$/\1/p' | sort | uniq -c | sort -rn | head -20

  echo
  echo "== candidate mx / mxbuild executables =="
  find "$extract_dir" -type f \( -name 'mx' -o -name 'mx.exe' -o -iname 'mxbuild*' \) -print

  echo
  echo "======================================================================"
  echo "Nothing was written to .mx-binaries/. Scratch extraction is at:"
  echo "  $extract_dir"
  echo "Next: ./fetch-mx.sh probe $version"
  echo "======================================================================"
}

probe() {
  local version="$1"
  local extract_dir
  extract_dir="$(extract_dir_for "$version")"
  [[ -d "$extract_dir" ]] || { echo "no extraction found for $version — run 'inspect' first" >&2; exit 1; }

  local mx_bin
  mx_bin="$(mx_bin_for "$extract_dir")"
  if [[ -z "$mx_bin" ]]; then
    echo "no modeler/mx found under $extract_dir" >&2
    exit 1
  fi
  echo "using: $mx_bin"

  echo
  echo "== mxbuild.runtimeconfig.json (what runtime it expects) =="
  cat "$extract_dir/modeler/mxbuild.runtimeconfig.json" 2>/dev/null || echo "(not found)"

  echo
  echo "== attempt 1: mx --help, as-is =="
  local out1
  out1="$("$mx_bin" --help 2>&1)" || true
  echo "$out1"
  if mx_started_ok "$out1"; then
    echo "OK — mx started fine (exit code from --help is not meaningful for this"
    echo "CLI's parser, only checked here for a real crash signature). No"
    echo "workaround needed."
    MX_WORKING_MODE="plain"
    echo
    echo "== mx diff --help (exit-code/usage sanity check) =="
    "$mx_bin" diff --help 2>&1 || echo "(diff --help returned non-zero — check the output above for an actual crash vs. just a usage message)"
    return 0
  fi

  echo
  echo "(mx did not start — trying with DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1)"
  echo "== attempt 2: mx --help, DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1 =="
  local out2
  out2="$(DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1 "$mx_bin" --help 2>&1)" || true
  echo "$out2"
  if mx_started_ok "$out2"; then
    MX_WORKING_MODE="invariant"
    echo "OK — mx starts fine with globalization invariant mode."
  fi

  if [[ "${MX_WORKING_MODE:-}" == "invariant" ]]; then
    echo
    echo "No libicu install needed; just set DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1"
    echo "wherever mx runs (the Dockerfile's ENV, and your local shell for testing)."
  elif echo "$out2" | grep -q "CultureNotFoundException"; then
    echo
    echo "This is a DEAD END for this build, not a config mistake: it's throwing"
    echo "CultureNotFoundException on 'en-us' specifically, which means the code"
    echo "hardcodes loading a real named culture at startup (see"
    echo "Mendix.Modeler.Internationalization.LocalizationProvider in the trace)."
    echo "Invariant globalization mode only supports the truly-invariant culture,"
    echo "so it structurally cannot satisfy that request — no amount of retrying"
    echo "or flag-tweaking will fix this path. The real fix is installing actual"
    echo "ICU data:"
    echo "    sudo apt-get update && sudo apt-get install -y libicu-dev"
    echo "(or find the exact runtime package for your distro with"
    echo "'apt-cache search libicu | grep -E \"^libicu[0-9]\"' and install that)."
    echo "Re-run './fetch-mx.sh probe $version' after installing to confirm."
  else
    echo
    echo "Still failing, and not the known CultureNotFoundException dead end above —"
    echo "capture this output, it's worth a closer look before assuming libicu alone"
    echo "fixes it."
  fi

  echo
  if [[ "${MX_WORKING_MODE:-}" != "" ]]; then
    echo "== mx diff --help (exit-code/usage sanity check) =="
    if [[ "$MX_WORKING_MODE" == "invariant" ]]; then
      DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1 "$mx_bin" diff --help 2>&1 || echo "(diff --help did not succeed — capture this output, worth a look)"
    else
      "$mx_bin" diff --help 2>&1 || echo "(diff --help did not succeed — capture this output, worth a look)"
    fi
  else
    echo "Skipping 'mx diff --help' — mx doesn't run successfully in any mode yet,"
    echo "so that check would just fail the same way. Fix the ICU issue above first."
  fi
}

trial_trim() {
  local version="$1"
  local extract_dir aside_dir
  extract_dir="$(extract_dir_for "$version")"
  aside_dir="$(aside_dir_for "$version")"
  [[ -d "$extract_dir" ]] || { echo "no extraction found for $version — run 'inspect' first" >&2; exit 1; }

  local mx_bin
  mx_bin="$(mx_bin_for "$extract_dir")"
  [[ -n "$mx_bin" ]] || { echo "no modeler/mx found under $extract_dir" >&2; exit 1; }

  echo "== baseline check: does mx run at all, untrimmed? =="
  if ! check_mx_mode "$mx_bin"; then
    echo
    echo "mx does not run successfully in ANY mode yet (plain or"
    echo "DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1) — trimming now would compare"
    echo "two equally-broken states and tell us nothing about whether the trim"
    echo "itself is safe. Fix that first:"
    echo "  ./fetch-mx.sh probe $version"
    echo "and follow its guidance. (Known issue as of 11.13.0: this build"
    echo "hardcodes loading the 'en-us' culture, so the invariant-mode flag is a"
    echo "dead end here — you need libicu actually installed, e.g."
    echo "'sudo apt-get install -y libicu-dev'.)"
    exit 1
  fi
  echo "baseline OK — mx runs in '$MX_WORKING_MODE' mode."

  mkdir -p "$aside_dir"

  echo
  echo "== before: extracted size =="
  du -sh "$extract_dir"

  for c in "${TRIM_CANDIDATES[@]}"; do
    move_candidate "$extract_dir" "$aside_dir" "$c"
  done

  echo
  echo "== moving aside scattered *.pdb files (debug symbols, never loaded at runtime) =="
  local pdb_moved=0
  while IFS= read -r -d '' pdb; do
    local rel="${pdb#"$extract_dir"/}"
    mkdir -p "$aside_dir/$(dirname "$rel")"
    mv "$pdb" "$aside_dir/$rel"
    pdb_moved=$((pdb_moved + 1))
  done < <(find "$extract_dir" -type f -name '*.pdb' -print0)
  echo "moved aside: $pdb_moved .pdb file(s)"

  echo
  echo "== after: extracted size =="
  du -sh "$extract_dir"

  echo
  echo "== retesting: mx --help ($MX_WORKING_MODE mode) =="
  local retest_out
  retest_out="$(mx_run "$mx_bin" --help 2>&1)" || true
  echo "$retest_out"
  if mx_started_ok "$retest_out"; then
    echo "OK — mx still starts fine (exit code ignored, same as the baseline check)."
  else
    echo "STILL FAILS after trim — this is a real crash signature, not just a"
    echo "usage/verb message. Something in the moved-aside folders was load-bearing."
  fi

  echo
  echo "== retesting: mx diff --help ($MX_WORKING_MODE mode) =="
  retest_out="$(mx_run "$mx_bin" diff --help 2>&1)" || true
  echo "$retest_out"
  if mx_started_ok "$retest_out"; then
    echo "OK — 'diff' verb still starts fine."
  else
    echo "diff --help FAILED after trim — a real crash, worth investigating before"
    echo "trusting any trim result."
  fi

  echo
  echo "======================================================================"
  echo "Moved-aside content is at: $aside_dir (nothing was deleted)."
  echo "If mx/diff still worked above: these folders are real trim candidates —"
  echo "  ${TRIM_CANDIDATES[*]}"
  echo "If something broke: run './fetch-mx.sh restore $version' and report"
  echo "which check failed so we narrow TRIM_CANDIDATES instead of cutting"
  echo "all three at once."
  echo "======================================================================"
}

# Breaks down what's left in extract_dir AFTER the first-round trim (or the
# full tree, if trial-trim hasn't run yet — it says so explicitly either way).
# Writes nothing, moves nothing — pure reporting, same "look before cutting"
# discipline as inspect(). Use this to find real second-round trim candidates
# instead of guessing (satellite culture resources, .pdb symbols, duplicate
# RID native libs, and any single oversized files are the usual suspects in a
# self-contained .NET publish, but don't assume any of them are actually
# present here without checking).
deep_inspect() {
  local version="$1"
  require du
  require find

  local extract_dir aside_dir
  extract_dir="$(extract_dir_for "$version")"
  aside_dir="$(aside_dir_for "$version")"
  [[ -d "$extract_dir" ]] || { echo "no extraction found for $version — run 'inspect' first" >&2; exit 1; }

  if [[ -d "$aside_dir" ]]; then
    echo "== current state: trimmed (trial-trim has run) — analyzing what's LEFT =="
  else
    echo "== current state: NOT trimmed — this is the full tree, run trial-trim first"
    echo "   for a meaningful second-pass breakdown (otherwise you're just re-seeing"
    echo "   modeler/tools, modeler/ide-client, runtime/ again)."
  fi

  echo
  echo "== total size of what remains =="
  du -sh "$extract_dir"

  echo
  echo "== layout under modeler/, depth 3, sorted by size (biggest first) =="
  du -ah --max-depth=3 "$extract_dir/modeler" 2>/dev/null | sort -rh | head -60

  echo
  echo "== file count + total size by extension, within what remains =="
  echo "   (files with no extension are excluded here, not lost — see 'largest"
  echo "   individual files' below)"
  find "$extract_dir" -type f -printf '%s %f\n' 2>/dev/null \
    | sed -nE 's/^([0-9]+) .*\.([a-zA-Z0-9]+)$/\1 \2/p' \
    | awk '{ sizes[$2]+=$1; counts[$2]+=1 } END { for (ext in sizes) printf "%12d bytes  %6d files  .%s\n", sizes[ext], counts[ext], ext }' \
    | sort -rn | head -25

  echo
  echo "== largest individual files (top 25) =="
  find "$extract_dir" -type f -printf '%s\t%p\n' 2>/dev/null | sort -rn | head -25 | awk -F'\t' '{ printf "%12d  %s\n", $1, $2 }'

  echo
  echo "== .pdb debug symbols (safe to cut in every case seen so far — not needed at runtime) =="
  local pdb_count pdb_size
  pdb_count="$(find "$extract_dir" -type f -name '*.pdb' | wc -l)"
  if [[ "$pdb_count" -gt 0 ]]; then
    pdb_size="$(find "$extract_dir" -type f -name '*.pdb' -printf '%s\n' | awk '{s+=$1} END {printf "%.0f", s}')"
    echo "$pdb_count files, $((pdb_size / 1024 / 1024)) MB total"
  else
    echo "none found"
  fi

  echo
  echo "== satellite culture-resource folders (localized strings — likely safe, this CLI only needs en-us) =="
  echo "   (found by content signature — any dir containing a *.resources.dll —"
  echo "   not by guessing culture-code folder names, so this won't miss unusual"
  echo "   naming or false-positive on an unrelated short folder name)"
  find "$extract_dir" -type f -name '*.resources.dll' 2>/dev/null \
    | sed -E 's#/[^/]+\.resources\.dll$##' | sort -u \
    | while read -r d; do du -sh "$d" 2>/dev/null; done | sort -rh | head -30

  echo
  echo "== runtimes/ subfolder (bundled per-RID native libs — flags if more than one RID is present) =="
  if [[ -d "$extract_dir/modeler" ]]; then
    find "$extract_dir/modeler" -type d -path '*/runtimes/*' -mindepth 0 -maxdepth 6 2>/dev/null | \
      sed -E 's#.*/runtimes/([^/]+).*#\1#' | sort -u | while read -r rid; do
        echo "  RID present: $rid"
      done
  fi

  echo
  echo "======================================================================"
  echo "This is reporting only — nothing moved. TRIM_CANDIDATES already covers"
  echo "the standard stuff (.pdb, satellite cultures, Translations, diagnostic"
  echo ".so's, plus onnxruntime/SkiaSharp/extensions/Licenses — all confirmed"
  echo "via a real mx diff, not just --help). Use this output to spot anything"
  echo "NEW and still large — extend TRIM_CANDIDATES with real paths, and for"
  echo "anything inside mx's own assembly graph (basically everything left"
  echo "here), validate one file/folder at a time with a REAL mx diff before"
  echo "trusting it — --help passing is not enough on its own."
  echo "======================================================================"
}

restore() {
  local version="$1"
  local extract_dir aside_dir
  extract_dir="$(extract_dir_for "$version")"
  aside_dir="$(aside_dir_for "$version")"
  if [[ ! -d "$aside_dir" ]]; then
    echo "nothing to restore for $version"
    exit 0
  fi
  cp -a "$aside_dir/." "$extract_dir/"
  rm -rf "$aside_dir"
  echo "restored $version from $aside_dir"
}

# Copies the CURRENT scratch extraction (as-is — trimmed if trial-trim has
# been run and not restored since) into .mx-binaries/<version>/. Nothing
# lands in .mx-binaries/ until this is run; inspect/probe/trial-trim only
# ever touch the scratch dir.
finalize() {
  local version="$1"
  local replace="${2:-}"
  local extract_dir dest_dir
  extract_dir="$(extract_dir_for "$version")"
  dest_dir="$DEST_ROOT/$version"
  [[ -d "$extract_dir" ]] || { echo "no extraction found for $version — run 'inspect' first" >&2; exit 1; }

  if [[ -d "$(aside_dir_for "$version")" ]]; then
    echo "== current state: trimmed (trial-trim has run, not restored) =="
  else
    echo "== current state: NOT trimmed — this will finalize the FULL untrimmed tree =="
    echo "   (run trial-trim first if you meant to ship the lean version)"
  fi

  local mx_bin
  mx_bin="$(mx_bin_for "$extract_dir")"
  [[ -n "$mx_bin" ]] || { echo "no modeler/mx found under $extract_dir" >&2; exit 1; }

  echo
  echo "== sanity check before finalizing =="
  if ! check_mx_mode "$mx_bin"; then
    echo "mx does not start from $extract_dir — refusing to finalize a broken tree." >&2
    echo "Run './fetch-mx.sh probe $version' to diagnose." >&2
    exit 1
  fi
  echo "OK — mx starts in '$MX_WORKING_MODE' mode from the current tree."
  echo "(This checks mx starts, same as before — it does NOT re-run a real"
  echo "mx diff. If TRIM_CANDIDATES changed since the last time this tree was"
  echo "diff-tested for real, re-validate with an actual mx diff against real"
  echo "worktrees before trusting the finalized copy, not just this --help check.)"

  if [[ -e "$dest_dir" ]]; then
    if [[ "$replace" == "--replace" ]]; then
      echo
      echo "$dest_dir already exists — removing it because --replace was passed."
      rm -rf "$dest_dir"
    else
      echo
      echo "$dest_dir already exists — refusing to overwrite it. Re-run with" >&2
      echo "'--replace' if you deliberately want to replace it (e.g. TRIM_CANDIDATES" >&2
      echo "grew since the last finalize) — this script otherwise never deletes" >&2
      echo "from .mx-binaries/ on its own; that's the one place in this flow" >&2
      echo "that's supposed to be durable by default." >&2
      exit 1
    fi
  fi

  mkdir -p "$DEST_ROOT"
  cp -a "$extract_dir" "$dest_dir"

  echo
  echo "== finalized =="
  du -sh "$dest_dir"
  echo "Wrote $dest_dir"
  echo
  echo "Runtime requirements wherever this actually runs (container, CI, etc.):"
  echo "  - libicu (host-specific package name; NOT necessarily libicu78 — that"
  echo "    was this WSL2 box's Ubuntu version. Confirm the right package for"
  echo "    whatever base image/OS actually runs this.)"
  if [[ "$MX_WORKING_MODE" == "invariant" ]]; then
    echo "  - DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1 (this finalize ran in invariant"
    echo "    mode — unexpected for 11.13.0, which is confirmed to need real libicu;"
    echo "    double check this wasn't a fluke before relying on it)."
  fi
  echo
  echo "Reminder: do NOT remove these — all confirmed load-bearing, tested and"
  echo "rejected, not just untested:"
  echo "  - Mendix.Modeler.WebUI.dll"
  echo "  - Mendix.Modeler.Theming.dll (Theming.Legacy.dll IS safe — don't confuse them)"
  echo "  - CycloneDX.Core.dll"
}

# Convenience wrapper: inspect -> trial-trim -> finalize, back to back, for
# a version you already trust the flow for (e.g. bringing up a second
# Mendix version once 11.13.0 is already validated). Each step is the exact
# same function the standalone commands call — no shortcuts, no different
# behavior — this just chains them so you don't have to run three commands
# and wait between them. It does NOT run 'probe' separately, but trial-trim
# already gates on the same mx-starts check internally (see its "baseline
# check" step) and refuses to trim if mx doesn't run at all, and finalize
# refuses to finalize a broken tree too — so a version that can't run mx at
# all still fails loudly here rather than silently producing a broken
# .mx-binaries/<version>/. Run 'probe' by hand afterward (or instead) if you
# want the fuller ICU-specific diagnostics on a failure.
#
# Accepts an optional trailing --replace, forwarded to finalize. Checked
# up front (before downloading/extracting/trimming anything) so a version
# that's already finalized fails fast instead of wasting a full inspect +
# trial-trim run only to have finalize refuse at the very end.
add_trimmed_version() {
  local version="$1"
  local replace="${2:-}"
  local dest_dir="$DEST_ROOT/$version"

  if [[ -e "$dest_dir" && "$replace" != "--replace" ]]; then
    echo "$dest_dir already exists — refusing to start. Re-run with '--replace'" >&2
    echo "if you deliberately want to replace it, e.g.:" >&2
    echo "  ./fetch-mx.sh add-trimmed-version $version --replace" >&2
    exit 1
  fi

  echo "======================================================================"
  echo "add-trimmed-version $version — step 1/3: inspect"
  echo "======================================================================"
  inspect "$version"

  echo
  echo "======================================================================"
  echo "add-trimmed-version $version — step 2/3: trial-trim"
  echo "======================================================================"
  trial_trim "$version"

  echo
  echo "======================================================================"
  echo "add-trimmed-version $version — step 3/3: finalize"
  echo "======================================================================"
  finalize "$version" "$replace"

  echo
  echo "======================================================================"
  echo "add-trimmed-version $version — done."
  echo "Reminder: TRIM_CANDIDATES was built and validated against 11.13.0. For"
  echo "any other version, trial-trim's own output above is the source of"
  echo "truth — check for entries that were 'already absent' for a real"
  echo "reason (this version's layout differs) vs. mx actually failing to"
  echo "start after the trim (a real problem). --help passing is also not"
  echo "the same as a real diff working — validate with an actual mx diff"
  echo "against real .mpr files before trusting this version's binary in"
  echo "production, the same way 11.13.0 was validated."
  echo "======================================================================"
}

clean() {
  local version="$1"
  rm -rf "$(extract_dir_for "$version")" "$(aside_dir_for "$version")" "$SCRATCH_ROOT/mxbuild-${version}.tar.gz"
  echo "removed scratch data for $version"
}

list() {
  if [[ ! -d "$DEST_ROOT" ]]; then
    echo "$DEST_ROOT does not exist yet"
    exit 0
  fi
  find "$DEST_ROOT" -mindepth 1 -maxdepth 1 -type d -exec basename {} \;
}

cmd="${1:-}"
case "$cmd" in
  inspect)    shift; [[ $# -ge 1 ]] || { usage; exit 1; }; inspect "$1" ;;
  probe)      shift; [[ $# -ge 1 ]] || { usage; exit 1; }; probe "$1" ;;
  trial-trim) shift; [[ $# -ge 1 ]] || { usage; exit 1; }; trial_trim "$1" ;;
  deep-inspect) shift; [[ $# -ge 1 ]] || { usage; exit 1; }; deep_inspect "$1" ;;
  trial-trim-2) echo "trial-trim-2 has been merged into trial-trim — just run:" >&2
                echo "  ./fetch-mx.sh trial-trim ${2:-<mxversion>}" >&2
                exit 1 ;;
  finalize)   shift; [[ $# -ge 1 ]] || { usage; exit 1; }; finalize "$1" "${2:-}" ;;
  add-trimmed-version) shift; [[ $# -ge 1 ]] || { usage; exit 1; }; add_trimmed_version "$1" "${2:-}" ;;
  restore)    shift; [[ $# -ge 1 ]] || { usage; exit 1; }; restore "$1" ;;
  clean)      shift; [[ $# -ge 1 ]] || { usage; exit 1; }; clean "$1" ;;
  list)       list ;;
  *) usage; exit 1 ;;
esac