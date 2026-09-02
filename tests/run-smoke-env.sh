#!/usr/bin/env bash
# Regression cells for run-smoke.sh's process environment. The expensive Go
# build and Neovim suite are shimmed: these cells exercise the runner itself.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
runner="$here/run-smoke.sh"
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT

pass=0
fail=0
ok()  { pass=$((pass + 1)); echo "  ok   — $1"; }
bad() { fail=$((fail + 1)); echo "  FAIL — $1"; }

mkdir -p "$work/bin" "$work/outer" "$work/tmp"
printf 'caller-owned\n' > "$work/outer/sentinel"

# run-smoke only needs a successful build before invoking Neovim. The binary is
# irrelevant to this runner-level test, so keep the cell hermetic and fast.
cat > "$work/bin/go" <<'SHIM'
#!/usr/bin/env bash
exit 0
SHIM

# Capture the directory the runner gives Neovim, and dirty it as a daemon would.
# If the runner merely inherits the caller's XDG path, fail before printing the
# sentinel so run-smoke cannot report a false pass.
cat > "$work/bin/nvim" <<'SHIM'
#!/usr/bin/env bash
set -u
[ -n "${XDG_DATA_HOME:-}" ] || exit 30
[ "$XDG_DATA_HOME" != "$OUTER_XDG" ] || exit 31
[ -d "$XDG_DATA_HOME" ] || exit 32
printf '%s\n' "$XDG_DATA_HOME" >> "$CAPTURE"
mkdir -p "$XDG_DATA_HOME/autodb"
printf 'smoke-owned\n' > "$XDG_DATA_HOME/autodb/meta.db"
printf 'SMOKE-COMPLETE OK\n'
SHIM
chmod +x "$work/bin/go" "$work/bin/nvim"

run_one() {
  PATH="$work/bin:$PATH" \
    TMPDIR="$work/tmp" \
    XDG_DATA_HOME="$work/outer" \
    OUTER_XDG="$work/outer" \
    CAPTURE="$work/captured" \
    "$runner" > "$work/out-$1" 2>&1
}

if run_one 1 && run_one 2; then
  ok "the isolated runner completes twice"
else
  bad "the isolated runner failed: $(cat "$work/out-1" "$work/out-2" 2>/dev/null)"
fi

roots=()
if [ -f "$work/captured" ]; then
  mapfile -t roots < "$work/captured"
fi
if [ "${#roots[@]}" -eq 2 ] && [ "${roots[0]}" != "${roots[1]}" ]; then
  ok "each invocation receives a distinct XDG_DATA_HOME"
else
  bad "expected two distinct roots, got: ${roots[*]-}"
fi

if [ -f "$work/outer/sentinel" ] && [ ! -e "$work/outer/autodb" ]; then
  ok "the caller's XDG_DATA_HOME is untouched"
else
  bad "the runner wrote into the caller's XDG_DATA_HOME"
fi

left=0
for root in "${roots[@]}"; do
  [ ! -e "$root" ] || left=$((left + 1))
done
if [ "$left" -eq 0 ]; then
  ok "per-run XDG directories are removed on exit"
else
  bad "$left per-run XDG directories survived runner exit"
fi

echo "smoke-env: $pass passed, $fail failed"
[ "$fail" -eq 0 ] || exit 1
echo "SMOKE-ENV-SUITE OK"
