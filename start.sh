#!/bin/bash

set -e

BASE_DIR="$(cd "$(dirname "$0")" && pwd)"

if [ -f "/data/miao/.env" ]; then
    set -a
    source "/data/miao/.env"
    set +a
fi

if [ -f "$BASE_DIR/.env" ]; then
    set -a
    source "$BASE_DIR/.env"
    set +a
fi

export PORT="${PORT:-9096}"
export WORK_DIR="${WORK_DIR:-$BASE_DIR/data}"
export FFMPEG_BIN="${FFMPEG_BIN:-ffmpeg}"

mkdir -p "$WORK_DIR"

nohup "$BASE_DIR/miao_dataService" > "$BASE_DIR/runtime.log" 2>&1 &
echo $! > "$BASE_DIR/miao_dataService.pid"
