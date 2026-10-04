#!/bin/bash
set -o pipefail
claude "$@" | tee "/Users/amit.bezalel/workspace/pr-triage/experiments/agent-fanout/logs/064743/prometheus-prometheus/baseline"/run-$$-$RANDOM.jsonl
