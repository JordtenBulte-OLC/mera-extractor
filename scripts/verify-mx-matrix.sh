#!/usr/bin/env bash
#
# verify-mx-matrix.sh — assert the image actually contains the versions
# mx-versions.txt declares.
#
# Why this exists: mx-versions.txt is a BUILD INPUT, not a description of the
# result. The two can drift, and when they do nothing notices until /extract
# returns unsupportedMendixVersion for a version the manifest promised. 
#
# The oracle is /health, not `ls /opt/mx`, deliberately. /health reports
# mx.ListVersions, which is the same code path Resolve and Highest use, so a
# pass here means the service will genuinely accept those versions. A bare
# directory listing would happily count a version directory whose modeler/mx
# never got extracted.
#
# Usage:
#   scripts/verify-mx-matrix.sh                     # start MERA_IMAGE, probe, stop
#   MERA_HEALTH_URL=http://localhost:8081/health \
#     scripts/verify-mx-matrix.sh                   # probe something already running
#
# Environment:
#   MERA_IMAGE           image to run          (default: mera-extractor:local)
#   MERA_VERSIONS_FILE   manifest path         (default: mx-versions.txt)
#   MERA_HEALTH_URL      skip starting a container and probe this URL instead
#   MERA_HEALTH_TIMEOUT  seconds to wait for readiness (default: 60)
#
# Exit codes: 0 match · 1 mismatch · 2 could not run the check at all.

set -euo pipefail

IMAGE="${MERA_IMAGE:-mera-extractor:local}"
VERSIONS_FILE="${MERA_VERSIONS_FILE:-mx-versions.txt}"
HEALTH_URL="${MERA_HEALTH_URL:-}"
HEALTH_TIMEOUT="${MERA_HEALTH_TIMEOUT:-60}"

CONTAINER=""
cleanup() {
    if [ -n "$CONTAINER" ]; then
        docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
    fi
}
trap cleanup EXIT

die() { printf 'verify-mx-matrix: %s\n' "$*" >&2; exit 2; }

# --------------------------------------------------------------------------
# Declared: parse mx-versions.txt
# --------------------------------------------------------------------------

# The `|| [ -n "$line" ]` guard is the entire reason this script exists.
# `read` returns non-zero at EOF without a delimiter — it still fills the
# variable, but the loop condition is already false, so a final line with no
# trailing newline is silently dropped. That is the bug being guarded against;
# this script must not contain it.
read_declared() {
    [ -f "$VERSIONS_FILE" ] || die "no manifest at $VERSIONS_FILE (run from the repo root?)"
    local line
    while IFS= read -r line || [ -n "$line" ]; do
        line="${line%$'\r'}"                                   # CRLF from a Windows editor
        line="$(printf '%s' "$line" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')"
        case "$line" in ''|\#*) continue ;; esac
        printf '%s\n' "$line"
    done < "$VERSIONS_FILE"
}

# --------------------------------------------------------------------------
# Actual: ask the running service
# --------------------------------------------------------------------------

start_container() {
    command -v docker >/dev/null 2>&1 || die "docker not found and MERA_HEALTH_URL not set"
    docker image inspect "$IMAGE" >/dev/null 2>&1 \
        || die "image $IMAGE not found — build it first (docker compose build)"

    # Port 0 lets the kernel pick, so this never collides with a running
    # compose stack, Mendix Studio Pro's 8080/8090, or a parallel CI job.
    CONTAINER="$(docker run -d --rm -p 127.0.0.1:0:8080 "$IMAGE")" \
        || die "could not start $IMAGE"

    local hostport
    hostport="$(docker port "$CONTAINER" 8080/tcp | head -n1)" \
        || die "container started but published no port"
    HEALTH_URL="http://${hostport}/health"
    printf 'probing %s (container %s)\n' "$HEALTH_URL" "${CONTAINER:0:12}"
}

fetch_health() {
    local waited=0 body
    while :; do
        if body="$(curl -fsS --max-time 10 "$HEALTH_URL" 2>/dev/null)"; then
            printf '%s' "$body"
            return 0
        fi
        waited=$((waited + 1))
        [ "$waited" -lt "$HEALTH_TIMEOUT" ] || {
            if [ -n "$CONTAINER" ]; then
                printf -- '--- container logs ---\n' >&2
                docker logs "$CONTAINER" 2>&1 | tail -n 30 >&2
            fi
            die "no answer from $HEALTH_URL after ${HEALTH_TIMEOUT}s"
        }
        sleep 1
    done
}

# Tied to the known /health shape rather than requiring jq, which is not
# installed on every runner. If healthResponse ever changes, this is the line
# that breaks — loudly, because the comparison then finds nothing installed.
extract_versions() {
    sed -n 's/.*"mxVersions":\[\([^]]*\)\].*/\1/p' | tr -d '" ' | tr ',' '\n' | sed '/^$/d'
}

extract_field() {
    sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p"
}

# --------------------------------------------------------------------------

main() {
    [ -n "$HEALTH_URL" ] || start_container

    local body declared installed status
    body="$(fetch_health)"

    status="$(printf '%s' "$body" | extract_field status)"
    printf 'status: %s\n' "${status:-<unparsed>}"

    declared="$(read_declared | sort -u)"
    installed="$(printf '%s' "$body" | extract_versions | sort -u)"

    printf 'declared in %s: %s\n' "$VERSIONS_FILE" "$(printf '%s' "$declared" | tr '\n' ' ')"
    printf 'installed in image:  %s\n' "$(printf '%s' "$installed" | tr '\n' ' ')"

    if [ -z "$declared" ]; then
        die "manifest lists no versions — that is almost certainly a parse failure, not intent"
    fi

    local missing extra
    missing="$(comm -23 <(printf '%s\n' "$declared") <(printf '%s\n' "$installed"))"
    extra="$(comm -13 <(printf '%s\n' "$declared") <(printf '%s\n' "$installed"))"

    local failed=0
    if [ -n "$missing" ]; then
        failed=1
        printf '\nDECLARED BUT NOT INSTALLED:\n' >&2
        printf '%s\n' "$missing" | sed 's/^/  /' >&2
        printf '  → the build did not produce these. Check the mx-fetch stage: a\n' >&2
        printf '    manifest with no trailing newline drops its last line, and a\n' >&2
        printf '    failed download or trim is easy to miss in build output.\n' >&2
    fi
    if [ -n "$extra" ]; then
        failed=1
        printf '\nINSTALLED BUT NOT DECLARED:\n' >&2
        printf '%s\n' "$extra" | sed 's/^/  /' >&2
        printf '  → probably a stale image, or a leftover directory under the mx\n' >&2
        printf '    root. Rebuild with --no-cache before believing this.\n' >&2
    fi

    if [ "$failed" -ne 0 ]; then
        printf '\nverify-mx-matrix: FAIL — the manifest and the image disagree\n' >&2
        exit 1
    fi

    # Reported after the set comparison: a degraded status with the right
    # versions is a different (and lesser) problem than a version mismatch.
    if [ "$status" != "ok" ]; then
        printf '\nverify-mx-matrix: versions match, but /health reports %s\n' "$status" >&2
        printf '%s\n' "$body" >&2
        exit 1
    fi

    printf '\nverify-mx-matrix: OK — %s version(s) declared and installed\n' \
        "$(printf '%s\n' "$declared" | wc -l | tr -d ' ')"
}

main "$@"