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

# The daemon's default sqlite store lives under $XDG_DATA_HOME/autodb. Running
# against the caller's real data directory means a killed smoke run can poison
# every later run on the same host. Give each invocation its own root; even a
# SIGKILL can then leave only an unreferenced temp directory, never shared state.
smoke_xdg="$(mktemp -d "${TMPDIR:-/tmp}/autodb-smoke-xdg.XXXXXXXX")" || {
  echo "run-smoke: could not create an isolated XDG_DATA_HOME"
  exit 1
}
trap 'rm -rf -- "$smoke_xdg"' EXIT
export XDG_DATA_HOME="$smoke_xdg"

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
# EXACT full-line match, exactly once. A substring match (grep -q) could be
# satisfied by the token appearing inside some other line, and more than one would
# mean the driver printed it more than once — neither is the single end-of-chunk
# sentinel we require. -Fxq is fixed-string, whole-line; the count enforces "once".
sentinels="$(grep -Fxc 'SMOKE-COMPLETE OK' <<<"$out")"
if [ "$sentinels" -ne 1 ]; then
  echo "run-smoke: FAIL — expected exactly one 'SMOKE-COMPLETE OK' full-line"
  echo "          sentinel, found $sentinels. Zero means the driver aborted before"
  echo "          finishing (nvim exits 0 on an uncaught error), so a green exit"
  echo "          code cannot be trusted. Treating as failure."
  exit 1
fi
echo "run-smoke: OK"
