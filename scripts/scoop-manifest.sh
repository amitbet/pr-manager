#!/usr/bin/env bash
# Writes a Scoop manifest to stdout. bucket/ in this repository is the bucket.
# Usage: scoop-manifest.sh cli VERSION PATH/TO/checksums.txt
#        scoop-manifest.sh desktop VERSION PATH/TO/pr-manager-windows-amd64.exe PATH/TO/checksums.txt
#
# The desktop app also installs the command line from the CLI archive: the
# desktop executable is a GUI program, which Windows gives no console.
set -euo pipefail
kind=$1
version=${2#v}
base="https://github.com/amitbet/pr-manager/releases/download/v$version"

case "$kind" in
  cli)
    checksums=$3
    hash_of() { awk -v f="$1" '$2 == f { print $1 }' "$checksums"; }
    amd64=$(hash_of "pr-manager_${version}_windows_amd64.zip")
    arm64=$(hash_of "pr-manager_${version}_windows_arm64.zip")
    [[ -n $amd64 && -n $arm64 ]] || { echo "windows archives missing from $3" >&2; exit 1; }
    cat <<JSON
{
    "version": "$version",
    "description": "Sort pull request changes by the human review they need",
    "homepage": "https://github.com/amitbet/pr-manager",
    "license": "Apache-2.0",
    "depends": ["git", "gh"],
    "architecture": {
        "64bit": {
            "url": "$base/pr-manager_${version}_windows_amd64.zip",
            "hash": "$amd64"
        },
        "arm64": {
            "url": "$base/pr-manager_${version}_windows_arm64.zip",
            "hash": "$arm64"
        }
    },
    "bin": "pr-manager.exe",
    "notes": "Run 'gh auth login', then 'pr-manager serve'."
}
JSON
    ;;
  desktop)
    sha=$(shasum -a 256 "$3" | cut -d' ' -f1)
    cli=$(awk -v f="pr-manager_${version}_windows_amd64.zip" '$2 == f { print $1 }' "${4:?checksums.txt}")
    [[ -n $cli ]] || { echo "windows amd64 archive missing from $4" >&2; exit 1; }
    cat <<JSON
{
    "version": "$version",
    "description": "Desktop app that sorts pull request changes by the human review they need",
    "homepage": "https://github.com/amitbet/pr-manager",
    "license": "Apache-2.0",
    "depends": ["git", "gh"],
    "architecture": {
        "64bit": {
            "url": [
                "$base/pr-manager-windows-amd64.exe#/pr-manager-desktop.exe",
                "$base/pr-manager_${version}_windows_amd64.zip"
            ],
            "hash": ["$sha", "$cli"]
        }
    },
    "bin": "pr-manager.exe",
    "shortcuts": [["pr-manager-desktop.exe", "PR Manager"]],
    "notes": "Run 'gh auth login' before opening PR Manager. The pr-manager command comes with it."
}
JSON
    ;;
  *)
    echo "usage: scoop-manifest.sh cli|desktop VERSION FILE" >&2
    exit 1
    ;;
esac
