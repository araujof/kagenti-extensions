#!/bin/bash
set -eu

# AuthBridge cpex combined entrypoint with process supervision.
# Manages: authbridge-cpex.
#
# Startup order:
#   1. authbridge-cpex (background) — proxy-sidecar listeners + CPEX
#      runtime (Rust core via CGO + bundled APL governance layer)
#
# Process management: PID 1 (this shell) supervises every long-running
# critical process. If any critical process exits, the others are killed
# and the container exits non-zero so Kubernetes restarts it. SIGTERM /
# SIGINT are forwarded for graceful shutdown.

CRITICAL_PIDS=""

cleanup() {
  echo "[entrypoint] Received signal, shutting down..."
  # shellcheck disable=SC2086
  kill $CRITICAL_PIDS 2>/dev/null || true
  wait
  exit 0
}
trap cleanup TERM INT

# --- Phase 1: authbridge-cpex (proxy-sidecar listeners + CPEX) ---
echo "[entrypoint] Starting authbridge-cpex..."
/usr/local/bin/authbridge-cpex "$@" &
CRITICAL_PIDS="$CRITICAL_PIDS $!"

# Block until any critical process exits, then terminate the container
# so Kubernetes restarts the pod.
# shellcheck disable=SC2086
wait -n $CRITICAL_PIDS
EXIT_CODE=$?
echo "[entrypoint] A critical process exited unexpectedly (exit code $EXIT_CODE), terminating container"
# shellcheck disable=SC2086
kill $CRITICAL_PIDS 2>/dev/null || true
wait
exit 1
