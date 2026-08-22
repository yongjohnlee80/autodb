#!/usr/bin/env bash
# Runs the Lua smoke suite with a real bin/autodb, and enforces the contract that
# the suite's own exit code cannot: nvim exits 0 on an uncaught error, so an abort
# in the driver would otherwise look like a pass (finding 3). The suite prints a
# single SMOKE-COMPLETE sentinel at the natural end of its main chunk; this script
# fails unless it sees "SMOKE-COMPLETE OK", which means the chunk both reached the
# end AND had zero failures and zero missing preconditions.
#
# Usage: tests/run-smoke.sh    (run from the repo root, or anywhere — it cd's)
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$here"

command -v nvim >/dev/null 2>&1 || { echo "run-smoke: nvim not found on PATH"; exit 127; }

# The five end-to-end sections need bin/autodb, which is gitignored — build it so
# the suite's preconditions are met rather than skipped-into-failure.
echo "run-smoke: building bin/autodb"
if ! CGO_ENABLED=0 GOFLAGS=-buildvcs=false go build -o bin/autodb ./cmd/autodb; then
  echo "run-smoke: go build failed"
  exit 1
fi

echo "run-smoke: running tests/smoke.lua"
out="$(nvim --headless -u tests/smoke.lua -c 'qa!' 2>&1)"
code=$?
echo "$out"

# Two independent gates. Either failing means the suite did not cleanly pass:
#   - a nonzero exit is the suite's own cq! (failures / missing preconditions);
#   - a missing "SMOKE-COMPLETE OK" is an ABORT (exit 0, sentinel never printed)
#     or an explicit FAIL sentinel.
if [ "$code" -ne 0 ]; then
  echo "run-smoke: FAIL — smoke suite exited $code"
  exit "$code"
fi
if ! grep -q "SMOKE-COMPLETE OK" <<<"$out"; then
  echo "run-smoke: FAIL — no SMOKE-COMPLETE OK sentinel; the driver aborted before"
  echo "          finishing (nvim exits 0 on an uncaught error), so a green exit"
  echo "          code cannot be trusted. Treating as failure."
  exit 1
fi
echo "run-smoke: OK"
