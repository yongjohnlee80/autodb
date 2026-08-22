#!/usr/bin/env bash
# The single entry point for autodb's non-Go test suites. Today it runs one
# (the neovim smoke suite); it exists now, with one, so that the Makefile and CI
# route through ONE script — a second suite is added here, not by teaching every
# caller about it. Any suite failing fails the whole run.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

rc=0
run() {
  echo "==> $1"
  if ! "$here/$1"; then
    echo "run-all: $1 FAILED"
    rc=1
  fi
}

run run-smoke.sh

if [ "$rc" -ne 0 ]; then
  echo "run-all: one or more suites failed"
  exit 1
fi
echo "run-all: all suites passed"
