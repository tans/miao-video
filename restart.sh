#!/bin/bash

set -e

BASE_DIR="$(cd "$(dirname "$0")" && pwd)"
PID_FILE="$BASE_DIR/miao_dataService.pid"

if [ -f "$PID_FILE" ]; then
    PID=$(cat "$PID_FILE")
    if ps -p "$PID" > /dev/null 2>&1; then
        kill -TERM "$PID" || true
        sleep 1
    fi
    rm -f "$PID_FILE"
fi

if pgrep -f "$BASE_DIR/miao_dataService" > /dev/null 2>&1; then
    pkill -f "$BASE_DIR/miao_dataService" || true
    sleep 1
fi

"$BASE_DIR/start.sh"
