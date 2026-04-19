#!/usr/bin/env bash
#
# fresh-home-test.sh — first-run smoke test for a freshly-built hippo
# binary. Simulates the experience of a new user who has never run
# hippo before: no ~/.hippo, no config, nothing.
#
# What passes the test:
#   1. `hippo init` creates ~/.hippo/config.yaml with mode 0600 and
#      all three providers present and disabled.
#   2. `hippo serve` binds 127.0.0.1 within 5 seconds, serves /config
#      as HTML containing "MCP Servers" plus every provider name
#      (Anthropic, OpenAI, Ollama).
#   3. SIGTERM shuts the server down cleanly within 5 seconds with
#      exit code 0.
#
# Exits 0 on success, non-zero with a clear message on the first
# failure. Uses bash, curl, and POSIX tools only. No jq required.
#
# Run locally:
#     bash scripts/fresh-home-test.sh
#
# Requires Go on PATH so the script can build a hippo binary from
# the current checkout; HIPPO_BIN env var skips the build and uses
# an existing binary instead.

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
tmp_home="$(mktemp -d)"
tmp_bin=""
server_pid=""
server_port="7846"

cleanup() {
    if [[ -n "$server_pid" ]]; then
        # kill -0 returns 0 if the process exists; ignore errors
        # because the happy path already stopped it.
        kill -0 "$server_pid" 2>/dev/null && kill "$server_pid" 2>/dev/null || true
        wait "$server_pid" 2>/dev/null || true
    fi
    rm -rf "$tmp_home" 2>/dev/null || true
    if [[ -n "$tmp_bin" && -f "$tmp_bin" ]]; then
        rm -f "$tmp_bin"
    fi
}
trap cleanup EXIT

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

log() {
    echo "[fresh-home] $*"
}

# --- Build or locate the hippo binary -------------------------------------

if [[ -n "${HIPPO_BIN:-}" ]]; then
    if [[ ! -x "$HIPPO_BIN" ]]; then
        fail "HIPPO_BIN=$HIPPO_BIN is not executable"
    fi
    hippo="$HIPPO_BIN"
    log "using HIPPO_BIN=$hippo"
else
    tmp_bin="$(mktemp -t hippo-fresh.XXXXXX)"
    log "building hippo into $tmp_bin"
    (cd "$repo_root" && CGO_ENABLED=0 go build -o "$tmp_bin" ./cmd/hippo)
    hippo="$tmp_bin"
fi

export HOME="$tmp_home"
log "HOME=$HOME"

# --- 1. hippo init --------------------------------------------------------

log "running: hippo init"
"$hippo" init >/dev/null

cfg="$HOME/.hippo/config.yaml"
[[ -f "$cfg" ]] || fail "expected $cfg to exist after init"

mode="$(stat -f '%Lp' "$cfg" 2>/dev/null || stat -c '%a' "$cfg" 2>/dev/null || echo "")"
[[ "$mode" == "600" ]] || fail "expected config mode 600, got '$mode'"

grep -q 'anthropic:' "$cfg" || fail "config missing anthropic section"
grep -q 'openai:' "$cfg" || fail "config missing openai section"
grep -q 'ollama:' "$cfg" || fail "config missing ollama section"
# All three providers should be disabled on first run. Provider entries
# are double-nested under `providers:` so their `enabled:` key sits at
# 8 leading spaces; `memory.enabled` sits at 4 spaces and should be
# ignored here.
if awk '
    /^providers:/ { inprov=1; next }
    /^[a-z]/ { inprov=0 }
    inprov && /^        enabled: true/ { found=1 }
    END { exit found ? 0 : 1 }
' "$cfg"; then
    fail "first-run config has a provider already enabled"
fi
log "init: ok"

# --- 2. hippo serve binds and responds ------------------------------------

log "starting: hippo serve --addr 127.0.0.1:$server_port"
"$hippo" serve --addr "127.0.0.1:$server_port" >"$tmp_home/server.log" 2>&1 &
server_pid=$!

# Poll the port until the server answers or 5s elapses.
bound=0
for _ in 1 2 3 4 5 6 7 8 9 10; do
    if curl -sf -o /dev/null "http://127.0.0.1:$server_port/config"; then
        bound=1
        break
    fi
    sleep 0.5
done
if [[ "$bound" != "1" ]]; then
    echo "--- server.log ---" >&2
    cat "$tmp_home/server.log" >&2 || true
    fail "server did not bind within 5 seconds"
fi
log "serve: bound"

body="$(curl -sf "http://127.0.0.1:$server_port/config")"
for needle in "MCP Servers" "Anthropic" "OpenAI" "Ollama"; do
    echo "$body" | grep -q "$needle" || fail "/config body missing '$needle'"
done
log "serve: /config content ok"

# --- 3. Graceful SIGTERM shutdown -----------------------------------------

kill -TERM "$server_pid"
shutdown=0
for _ in 1 2 3 4 5 6 7 8 9 10; do
    if ! kill -0 "$server_pid" 2>/dev/null; then
        shutdown=1
        break
    fi
    sleep 0.5
done
if [[ "$shutdown" != "1" ]]; then
    fail "server did not exit within 5 seconds of SIGTERM"
fi

# wait for the exit code.
set +e
wait "$server_pid"
exit_code=$?
set -e
server_pid=""
if [[ "$exit_code" -ne 0 ]]; then
    fail "server exited non-zero: $exit_code"
fi
log "shutdown: clean"

log "PASS"
