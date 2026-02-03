#!/usr/bin/env bash
set -euo pipefail

BASE_PORT=${BASE_PORT:-7000}
NODES=${NODES:-3}
DURATION_SEC=${DURATION_SEC:-20}
DIFFICULTY=${DIFFICULTY:-16}
TX_INTERVAL=${TX_INTERVAL:-1s}

if [[ $NODES -lt 2 ]]; then
  echo "NODES must be >= 2" >&2
  exit 1
fi
if [[ $DURATION_SEC -lt 1 ]]; then
  echo "DURATION_SEC must be >= 1" >&2
  exit 1
fi

ROOT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
RUN_ID=$(date +"%Y%m%d-%H%M%S")
OUT_DIR="$ROOT_DIR/data/tcp-demo/$RUN_ID"
BIN_DIR="$OUT_DIR/bin"
LOG_DIR="$OUT_DIR/logs"

mkdir -p "$BIN_DIR" "$LOG_DIR"

EXE="$BIN_DIR/simchain"
echo "Building $EXE ..."
GO111MODULE=on go build -o "$EXE" ./cmd/simchain

SEED_ADDR="127.0.0.1:$BASE_PORT"
PIDS=()

cleanup() {
  for pid in "${PIDS[@]:-}"; do
    if kill -0 "$pid" >/dev/null 2>&1; then
      kill "$pid" >/dev/null 2>&1 || true
    fi
  done
  wait 2>/dev/null || true
  echo "Done. Logs in: $LOG_DIR"
}
trap cleanup EXIT

for ((i=0; i<NODES; i++)); do
  PORT=$((BASE_PORT + i))
  LISTEN="127.0.0.1:$PORT"
  NODE_DIR="$OUT_DIR/node-$PORT"
  mkdir -p "$NODE_DIR"

  ARGS=(
    "--transport=tcp"
    "--listen=$LISTEN"
    "--seeds=$SEED_ADDR"
    "--data-dir=$NODE_DIR"
    "--difficulty=$DIFFICULTY"
    "--duration=${DURATION_SEC}s"
    "--tx-interval=$TX_INTERVAL"
  )
  if [[ $i -eq 0 ]]; then
    ARGS+=("--tx-interval=0")
  fi

  STDOUT="$LOG_DIR/node-$PORT.out.log"
  STDERR="$LOG_DIR/node-$PORT.err.log"
  echo "Starting node listen=$LISTEN (logs: $STDOUT)"

  "$EXE" "${ARGS[@]}" >"$STDOUT" 2>"$STDERR" &
  PIDS+=("$!")
  sleep 0.15

done

echo "Running for $DURATION_SEC seconds..."
sleep "$DURATION_SEC"
