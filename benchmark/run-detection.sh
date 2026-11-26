#!/usr/bin/env bash
# Runs every detection scenario REPS times, PAR at a time. Results land in results/raw/detection/.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
REPS=${REPS:-3}; PAR=${PAR:-3}
SCENARIOS=${SCENARIOS:-"healthy wal-gap backup-wal-missing base-data-deleted heap-corruption logical-loss archiving-stopped"}
mkdir -p "$HERE/results/logs"
for r in $(seq 1 "$REPS"); do for s in $SCENARIOS; do echo "$s $r"; done; done \
  | xargs -P "$PAR" -L 1 sh -c '"$0"/detect.sh "$1" "$2" > "$0/results/logs/detect-${CHECKER:-restorecheck}-$1-$2.log" 2>&1 && echo "ok $1 $2" || echo "FAILED $1 $2"' "$HERE"
