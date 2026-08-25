# ---------- stage 1: compile the Go program ----------
# This is where Go is used. Full toolchain, thrown away afterwards.
FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/extractor .

# ---------- stage 2: mx/mxbuild acquisition, trimmed before it ever
# reaches the final image ----------
#
# Runs fetch-mx.sh's inspect + trial-trim + finalize chain
# (add-trimmed-version) once per line of mx-versions.txt, then discards
# each version's scratch download/extraction/trimmed-aside leftovers
# immediately with 'clean' — keeps peak disk use during this stage bounded
# to roughly one version's ~1.6G untrimmed extraction at a time, not N.
#
# Uses the SAME base image as the final stage on purpose: trial-trim's own
# baseline check actually runs mx to confirm it starts (see fetch-mx.sh),
# so validating it here means validating it against the real target
# environment — not a different OS/libc combination that happens to also
# have libicu.
FROM debian:bookworm-slim AS mx-fetch

# libicu72 confirmed correct for bookworm (packages.debian.org/bookworm/libicu72).
# mx/mxbuild needs real ICU data, not the invariant-globalization workaround —
# this build hardcodes loading the 'en-us' culture at startup (see
# fetch-mx.sh's header comment for how that was confirmed).
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

RUN --mount=type=cache,target=/mxcache,sharing=locked \
    set -eux; \
    chmod +x fetch-mx.sh; \
    while IFS= read -r v <&3 || [ -n "$v" ]; do \
      v="$(echo "$v" | sed 's/#.*//' | xargs)"; \
      [ -z "$v" ] && continue; \
      if [ -x "/mxcache/$v/modeler/mx" ]; then \
        echo "== cache hit: $v =="; \
      else \
        echo "== building: $v =="; \
        ./fetch-mx.sh add-trimmed-version "$v" </dev/null; \
        ./fetch-mx.sh clean "$v" </dev/null; \
        mkdir -p "/mxcache/$v"; \
        cp -a "/build/.mx-binaries/$v/." "/mxcache/$v/"; \
      fi; \
      mkdir -p "/build/.mx-binaries/$v"; \
      cp -a "/mxcache/$v/." "/build/.mx-binaries/$v/"; \
    done 3< mx-versions.txt; \
    [ -n "$(ls -A /build/.mx-binaries 2>/dev/null)" ] \
      || { echo "mx-versions.txt produced no versions"; exit 1; }; \
    du -sh /build/.mx-binaries/*

# ---------- stage 3: the actual image ----------
FROM debian:bookworm-slim AS runtime
ARG TARGETARCH=amd64

RUN apt-get update && apt-get install -y --no-install-recommends \
      git ca-certificates curl \
      libicu72 \
    && rm -rf /var/lib/apt/lists/*

# --- mxcli: latest by default, pinnable from CI. See manual §1.3a ---
ARG MXCLI_VERSION=latest
RUN set -eux; \
    if [ "$MXCLI_VERSION" = "latest" ]; then \
      url="https://github.com/mendixlabs/mxcli/releases/latest/download/mxcli-linux-${TARGETARCH}"; \
    else \
      url="https://github.com/mendixlabs/mxcli/releases/download/${MXCLI_VERSION}/mxcli-linux-${TARGETARCH}"; \
    fi; \
    curl -fsSL --retry 3 --retry-delay 2 -o /usr/local/bin/mxcli "$url"; \
    chmod +x /usr/local/bin/mxcli; \
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

## Debug configuration

FROM golang:1.27-bookworm AS delve
RUN go install github.com/go-delve/delve/cmd/dlv@latest

FROM build AS build-debug
RUN CGO_ENABLED=0 go build -gcflags="all=-N -l" -o /out/extractor-debug .

FROM runtime AS debug
USER root
COPY --from=delve /go/bin/dlv /usr/local/bin/dlv
COPY --from=build-debug /out/extractor-debug /usr/local/bin/extractor
USER extractor
ENTRYPOINT ["dlv", "exec", "/usr/local/bin/extractor", \
  "--headless", "--listen=:2345", "--api-version=2", "--accept-multiclient", "--"]