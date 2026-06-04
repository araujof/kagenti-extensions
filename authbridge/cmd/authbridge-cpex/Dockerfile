# AuthBridge proxy-sidecar combined image — authbridge-cpex (HTTP
# forward + reverse proxies, no Envoy / no gRPC, full plugin set
# including the CPEX runtime) in a single container.
#
# Differences from the default authbridge image:
#   - Pulls the signed libcpex_ffi.a release artifact at build time
#     (no Rust toolchain in this repo).
#   - Builds with CGO_ENABLED=1 + `-tags cpex` so the linker pulls in
#     libcpex_ffi.a and main.go pulls in the cpex plugin import +
#     manager factory wiring.
#   - Uses Go 1.25 toolchain because the CPEX Go binding declares
#     `go 1.25.4` in its go.mod; the binary's own go.mod also pins
#     1.25.4. The other authbridge-* binaries continue to build on
#     Go 1.24.
#
# Build context: ./authbridge (needs access to authlib/ and
# cmd/authbridge-cpex/).

# Single source of truth for the CPEX FFI artifact version.
# Mirrored into the binary's go.mod via the cpex Go binding pin; a
# pre-commit hook (scripts/check-cpex-version.sh) enforces agreement
# so bumping is one file edit, one PR, one invariant.
ARG CPEX_FFI_VERSION=v0.2.0-ffi.test.3

# Stage 1: download + verify the libcpex_ffi.a artifact for the
# current TARGETOS/TARGETARCH. Uses the upstream
# scripts/download-ffi-artifact.sh (cosign verify-blob + sha256). The
# script is fetched from the same release tag as the .a so an
# operator who reviews this Dockerfile can pin both via the single
# CPEX_FFI_VERSION ARG.
FROM debian:12-slim AS ffi-fetch
ARG CPEX_FFI_VERSION
ARG TARGETOS
ARG TARGETARCH
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl jq tar gzip cosign && \
    rm -rf /var/lib/apt/lists/*
WORKDIR /work
RUN curl -fsSL \
      "https://raw.githubusercontent.com/contextforge-org/cpex/${CPEX_FFI_VERSION}/scripts/download-ffi-artifact.sh" \
      -o download-ffi-artifact.sh && \
    chmod +x download-ffi-artifact.sh && \
    CPEX_VERSION="${CPEX_FFI_VERSION}" \
    OS="${TARGETOS}" ARCH="${TARGETARCH}" \
    DEST=/work/ffi \
    ./download-ffi-artifact.sh

# Stage 2: build authbridge-cpex with CGO=1 + -tags cpex.
# `-ldflags="-s -w"` drops the symbol table and DWARF debug info to
# shave ~30% off the binary; pclntab still carries function names for
# panic stack traces.
FROM golang:1.25-bookworm AS builder
ARG CPEX_FFI_VERSION
ENV CPEX_FFI_VERSION=${CPEX_FFI_VERSION}

# CGO + libcpex_ffi.a needs a working C linker; bookworm ships gcc.
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential ca-certificates && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY authlib/ authlib/
COPY cmd/authbridge-cpex/ cmd/authbridge-cpex/
COPY --from=ffi-fetch /work/ffi /opt/cpex-ffi

ENV GOWORK=off
# CGO_LDFLAGS points at the verified .a; the cpex Go binding's cgo
# preamble references libcpex_ffi via -lcpex_ffi which the linker
# resolves through this -L path.
ENV CGO_ENABLED=1
ENV CGO_LDFLAGS="-L/opt/cpex-ffi/lib -lcpex_ffi"
RUN cd cmd/authbridge-cpex && \
    go build -tags cpex -ldflags="-s -w" -o /authbridge-cpex .

# Stage 3: runtime — debian-slim because libcpex_ffi.a brings glibc
# symbols at link time; alpine's musl would mismatch unless we built
# the musl tuple of the .a, which is a future variant.
FROM debian:12-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash ca-certificates && \
    rm -rf /var/lib/apt/lists/* && \
    useradd --uid 1001 --create-home --shell /bin/bash authbridge

COPY --from=builder --chmod=755 /authbridge-cpex /usr/local/bin/authbridge-cpex
COPY --chmod=755 cmd/authbridge-cpex/entrypoint.sh /usr/local/bin/entrypoint.sh

USER 1001

EXPOSE 8080 8081 9091 9093 9094

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
