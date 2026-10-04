#!/bin/bash
set -o pipefail
claude "$@" | tee "/Users/amit.bezalel/workspace/pr-triage/experiments/agent-fanout/logs/064458/kubernetes-kubernetes/agent-1-opus"/run-$$-$RANDOM.jsonl
