#!/bin/bash
# run.sh NAME MODEL EFFORT: one eval of both fixtures with that reviewer.
set -e
cd "$(dirname "$0")"
name=$1 model=$2 effort=$3
export REVIEWER_LOGS=$PWD/logs/$name-$(date +%H%M%S)
mkdir -p "$REVIEWER_LOGS"
start=$(date +%s)
PATH=$PWD/shim:$PATH ../../pr-manager-poc eval -fixtures fixtures -codemap off \
  -summarizer claude-code -summary-model "$model" -review-effort "$effort" > "$REVIEWER_LOGS/eval.txt" 2> "$REVIEWER_LOGS/stderr.txt"
echo "$(( $(date +%s) - start ))" > "$REVIEWER_LOGS/wall_seconds"
echo "$name done in $(cat $REVIEWER_LOGS/wall_seconds)s -> $REVIEWER_LOGS"
