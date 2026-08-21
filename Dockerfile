# ---------- stage 1: compile the Go program ----------
# This is where Go is used. Full toolchain, thrown away afterwards.
FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/extractor .

# ---------- stage 2: the actual image ----------
FROM debian:bookworm-slim
ARG TARGETARCH=amd64

RUN apt-get update && apt-get install -y --no-install-recommends \
      git ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*

# mxcli: latest by default, pinnable from CI later
RUN curl -fsSL -o /usr/local/bin/mxcli \
      "https://github.com/mendixlabs/mxcli/releases/latest/download/mxcli-linux-${TARGETARCH}" \
    && chmod +x /usr/local/bin/mxcli \
    && { mxcli --version || mxcli version; } > /etc/mxcli.version 2>&1 \
    && cat /etc/mxcli.version

# TODO: the Studio Pro `mx` binaries go here — manual §1.3. Not needed yet.

# The compiled binary from stage 1. No Go toolchain in this image.
COPY --from=build /out/extractor /usr/local/bin/extractor

RUN useradd -m -u 10001 extractor
USER extractor
WORKDIR /work
ENV MERA_WORK_ROOT=/work
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/extractor"]