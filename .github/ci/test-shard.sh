#!/usr/bin/env bash
# test-shard.sh SHARD — print the Go packages one CI test shard runs.
#
# ONE PLACE NAMES THE SHARDS, and "rest" is computed from it: every package
# `go list ./...` reports that no named shard claims. A package added later
# therefore lands in "rest" on its own; it can never fall out of CI because
# nobody remembered to list it.
#
# The split follows measured time (go test -race -count=2 on the ubuntu
# runner, 2026-10-07): tui 386 s, frontdoor 303 s, core/exec 250 s,
# cmd/autodb 181 s, internal/gatemutation 173 s, core/auth 143 s,
# internal/scriptguard 117 s, internal/commentguard 94 s; every other package
# under 75 s. One job ran them all in 799 s; split, each shard is bounded by
# its slowest package, and a flake re-runs only its own shard.
#
# `test-shard.sh --check` fails if a named shard lists a package that does not
# exist (a rename would otherwise turn a shard into a no-op that passes).
set -euo pipefail

mod=github.com/yongjohnlee80/autodb

declare -A shards=(
  [tui]="$mod/tui"
  [frontdoor]="$mod/frontdoor"
  [exec]="$mod/core/exec"
  [cmd-auth]="$mod/cmd/autodb $mod/core/auth"
  [guards]="$mod/internal/gatemutation $mod/internal/scriptguard $mod/internal/commentguard"
)

all="$(go list ./...)"

if [[ "${1:-}" == "--check" ]]; then
  bad=0
  for name in "${!shards[@]}"; do
    for p in ${shards[$name]}; do
      grep -qxF "$p" <<<"$all" || { echo "shard $name names $p, which go list does not report" >&2; bad=1; }
    done
  done
  exit "$bad"
fi

shard="${1:?usage: test-shard.sh SHARD | --check}"
if [[ "$shard" == "rest" ]]; then
  claimed="$(printf '%s\n' ${shards[@]})"
  grep -vxF -f <(printf '%s\n' "$claimed") <<<"$all"
  exit 0
fi
[[ -n "${shards[$shard]:-}" ]] || { echo "unknown shard: $shard" >&2; exit 2; }
printf '%s\n' ${shards[$shard]}
