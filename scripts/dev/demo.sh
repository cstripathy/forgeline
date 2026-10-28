#!/bin/sh
# Build Forgeline and (re)start the demo console. Usage: scripts/dev/demo.sh [data-dir] [listen]
# The server token lives in <data-dir>/.token (created once, mode 0600), so a restart keeps it.
set -eu
cd "$(dirname "$0")/../.."
DATA=${1:-/tmp/fl-demo}
LISTEN=${2:-127.0.0.1:8080}
mkdir -p "$DATA"
[ -f "$DATA/.token" ] || (umask 077; head -c 24 /dev/urandom | base64 | tr -d '/+=\n' > "$DATA/.token")
go build -o bin/forgeline ./cmd/forgeline
pkill -x forgeline 2>/dev/null || true   # not -f: that would match this script
sleep 1
FORGELINE_TOKEN=$(cat "$DATA/.token") nohup ./bin/forgeline server --listen "$LISTEN" --data-dir "$DATA" >"$DATA/server.log" 2>&1 &
echo "Forgeline at http://$LISTEN (log: $DATA/server.log)"
