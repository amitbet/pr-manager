#!/usr/bin/env bash
# Parse browser scripts before packaging them into the Go executable.
set -euo pipefail
cd "$(dirname "$0")/.."
for script in ui/js/*.js; do
  # Explicit module mode catches early errors such as duplicate declarations.
  node --input-type=module --check < "$script"
done
echo "PASS: UI JavaScript syntax"
