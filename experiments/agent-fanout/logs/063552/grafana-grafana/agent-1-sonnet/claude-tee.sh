#!/bin/bash
set -o pipefail
claude "$@" | tee "/Users/amit.bezalel/workspace/pr-triage/experiments/agent-fanout/logs/063552/grafana-grafana/agent-1-sonnet"/run-$$-$RANDOM.jsonl
