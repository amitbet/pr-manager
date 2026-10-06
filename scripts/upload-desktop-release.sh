#!/usr/bin/env bash
# Uploads a desktop package to the release CI is publishing. The Release job
# creates the release after the CLI tests pass: wait for it, and stop waiting
# if that job ends without creating one.
# Usage: upload-desktop-release.sh TAG PATH
set -euo pipefail
tag=$1
jobs_url="repos/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID/attempts/$GITHUB_RUN_ATTEMPT/jobs"
until gh release view "$tag" >/dev/null 2>&1; do
  conclusion=$(gh api "$jobs_url" --jq '.jobs[] | select(.name == "Release") | .conclusion // ""')
  if [[ -n "$conclusion" && "$conclusion" != "success" ]]; then
    echo "release job ended with $conclusion" >&2
    exit 1
  fi
  if (( SECONDS > 1800 )); then
    echo "timed out waiting for release $tag" >&2
    exit 1
  fi
  sleep 15
done
gh release upload "$tag" "$2" --clobber
