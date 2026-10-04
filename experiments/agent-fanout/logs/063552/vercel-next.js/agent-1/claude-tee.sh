#!/bin/bash
set -o pipefail
claude "$@" | tee "/Users/amit.bezalel/workspace/pr-triage/experiments/agent-fanout/logs/063552/vercel-next.js/agent-1"/run-$$-$RANDOM.jsonl
