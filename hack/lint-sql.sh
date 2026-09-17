#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Run squawk against every .up.sql file in the given paths. Treat any
# rule with severity >= warning as a CI failure.
#
# Usage:
#   hack/lint-sql.sh <path> [<path> …]
#
# Requires: squawk on PATH. Install via:
#   curl -L https://github.com/sbdchd/squawk/releases/latest/download/squawk-linux-x86_64.tar.gz | tar xz
#   sudo mv squawk /usr/local/bin/

set -euo pipefail

if ! command -v squawk >/dev/null 2>&1; then
  echo "squawk not found on PATH. Install per the comment at the top of this script." >&2
  exit 2
fi

if [[ "$#" -lt 1 ]]; then
  echo "usage: $0 <path> [<path> …]" >&2
  exit 64
fi

# Excluded rules — accept these by policy. Document each exclusion.
EXCLUDED_RULES=(
  # require-concurrent-index-creation: we don't enforce CONCURRENTLY at
  # this layer because pgroll-expand-contract is the policy for those
  # cases; for raw versioned migrations small-table indices are fine.
  "require-concurrent-index-creation"
)

EXCLUDE_FLAG=""
for r in "${EXCLUDED_RULES[@]}"; do
  EXCLUDE_FLAG="$EXCLUDE_FLAG --exclude=$r"
done

failed=0
while IFS= read -r -d '' f; do
  echo "==> $f"
  if ! squawk $EXCLUDE_FLAG "$f"; then
    failed=1
  fi
done < <(find "$@" -type f -name "*.up.sql" -print0)

if [[ "$failed" -eq 0 ]]; then
  echo "squawk: all clean"
fi
exit "$failed"
