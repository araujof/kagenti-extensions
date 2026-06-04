#!/usr/bin/env bash
# Verify that cmd/authbridge-cpex/CPEX_FFI_VERSION agrees with the
# pinned version of the CPEX Go binding in cmd/authbridge-cpex/go.mod.
# The two have to move in lockstep — the .a artifact and the binding
# share an FFI ABI version that is checked at binary load time, so a
# version skew between the linked library and the Go-side wrapper
# would surface as a noisy panic at process start. Catching it
# pre-commit is strictly cheaper.
#
# Run via pre-commit (configured in .pre-commit-config.yaml) and from
# the ci-cpex workflow before the build step.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
VERSION_FILE="${ROOT}/authbridge/cmd/authbridge-cpex/CPEX_FFI_VERSION"
GOMOD_FILE="${ROOT}/authbridge/cmd/authbridge-cpex/go.mod"

if [[ ! -f "${VERSION_FILE}" ]]; then
  echo "check-cpex-version: ${VERSION_FILE} not found" >&2
  exit 1
fi
if [[ ! -f "${GOMOD_FILE}" ]]; then
  echo "check-cpex-version: ${GOMOD_FILE} not found" >&2
  exit 1
fi

pinned="$(tr -d '[:space:]' < "${VERSION_FILE}")"
if [[ -z "${pinned}" ]]; then
  echo "check-cpex-version: ${VERSION_FILE} is empty" >&2
  exit 1
fi

# The CPEX Go binding pin is required to be on the same line as the
# module path. We accept either a `require <path> <version>` line at
# top level or an indented entry inside a `require (...)` block.
binding_path='github.com/contextforge-org/contextforge-plugins-framework/go/cpex'
gomod_version="$(grep -E "^[[:space:]]*${binding_path}[[:space:]]+v" "${GOMOD_FILE}" \
  | head -n1 \
  | awk '{ for (i=1;i<=NF;i++) if ($i ~ /^v[0-9]/) { print $i; exit } }')"

if [[ -z "${gomod_version}" ]]; then
  echo "check-cpex-version: failed to find ${binding_path} pin in ${GOMOD_FILE}" >&2
  echo "  expected a require line for ${binding_path} matching the pin in ${VERSION_FILE}" >&2
  exit 1
fi

if [[ "${pinned}" != "${gomod_version}" ]]; then
  echo "check-cpex-version: version drift between FFI artifact and Go binding pin" >&2
  echo "  CPEX_FFI_VERSION  : ${pinned}" >&2
  echo "  go.mod (${binding_path}): ${gomod_version}" >&2
  echo "  Bump both to the same value in one commit." >&2
  exit 1
fi

echo "check-cpex-version: OK — ${binding_path}=${gomod_version}"
