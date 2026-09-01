#!/usr/bin/env bash
# Regression suite for install.sh.
#
# WHY THIS EXISTS: install.sh once reported success while installing nothing.
# `set -e` does NOT fire on a failing command inside an AND-OR list, so
# `cp x y && mv y z` followed by a successful line returned 0 with the copy
# having failed -- and because the final smoke only checked that SOME autodb
# ran, a stale binary already at the prefix stood in as proof of a successful
# install. A user would be told they had v0.3.0 while holding something older.
# Lector caught it in the PR #35 r0 review; these cells keep it caught.
#
# HERMETIC BY CONSTRUCTION: `curl` is shimmed, so nothing here touches the
# network and CI cannot flake on GitHub availability. `tar`, `sha256sum` and
# the shell are real.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/.." && pwd)"
script="$root/install.sh"

VERSION="v9.9.9-test"
pass=0; fail=0
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

ok()  { pass=$((pass+1)); echo "  ok   — $1"; }
bad() { fail=$((fail+1)); echo "  FAIL — $1"; }

case "$(uname -s)" in Linux) goos=linux ;; Darwin) goos=darwin ;; *) goos=linux ;; esac
case "$(uname -m)" in x86_64|amd64) goarch=amd64 ;; aarch64|arm64) goarch=arm64 ;; *) goarch=amd64 ;; esac
asset="autodb-$VERSION-$goos-$goarch"

# --- build a real tarball holding a fake binary that reports $VERSION --------
mkdir -p "$work/release"
printf '#!/bin/sh\necho "autodb %s (testfake)"\n' "$VERSION" > "$work/release/$asset"
chmod +x "$work/release/$asset"
( cd "$work/release" && tar czf "$asset.tar.gz" "$asset" \
  && sha256sum "$asset.tar.gz" > "$asset.tar.gz.sha256" )

# --- shim curl: serve the local release, never the network ------------------
mkdir -p "$work/shim"
cat > "$work/shim/curl" <<SHIM
#!/bin/sh
# Parse the -o <dest> that install.sh uses and serve the local file.
dest=""; url=""
while [ \$# -gt 0 ]; do
  case "\$1" in
    -o) dest="\$2"; shift 2 ;;
    -w) shift 2 ;;
    -fsSL|-fsSLO|-s|-f|-S|-L) shift ;;
    http*) url="\$1"; shift ;;
    *) shift ;;
  esac
done
[ -n "\$dest" ] || exit 0
if [ -n "\${FAKE_CURL_NOWRITE:-}" ]; then exit 0; fi
name="\${url##*/}"
if [ -f "$work/release/\$name" ]; then cat "$work/release/\$name" > "\$dest"; exit 0; fi
exit 22
SHIM
chmod +x "$work/shim/curl"

fakebin() { # $1=name  $2=exit code
  mkdir -p "$work/fake"
  printf '#!/bin/sh\nexit %s\n' "$2" > "$work/fake/$1"; chmod +x "$work/fake/$1"
}
old_binary() { # $1=prefix -- plainly different version
  mkdir -p "$1"
  printf '#!/bin/sh\necho "autodb v0.0.1-OLD"\n' > "$1/autodb"; chmod +x "$1/autodb"
}
stale_superset() { # $1=prefix -- version CONTAINS the requested one
  # The nastier stale binary: "vX-old" contains "vX", so a substring test
  # accepts it. Release-shaped equivalent: a leftover v0.3.0-rc1 satisfying a
  # request for v0.3.0.
  mkdir -p "$1"
  printf '#!/bin/sh\necho "autodb %s-old (stale, built old)"\n' "$VERSION" > "$1/autodb"
  chmod +x "$1/autodb"
}
run() { # $1=prefix, rest=extra PATH dirs -> echoes exit code
  local pfx="$1"; shift
  local p="$work/shim:$PATH"
  [ -d "$work/fake" ] && p="$work/fake:$p"
  PATH="$p" FAKE_CURL_NOWRITE="${FAKE_CURL_NOWRITE:-}" sh "$script" --version "$VERSION" --binary --prefix "$pfx" >"$work/out" 2>&1
  echo $?
}
tmpcount() { ls "$1" 2>/dev/null | grep -c 'autodb\.tmp' || true; }

echo "==> install.sh regression"

# [1] happy path -------------------------------------------------------------
rm -rf "$work/fake"; p="$work/p1"; mkdir -p "$p"
rc=$(run "$p")
if [ "$rc" -eq 0 ] && "$p/autodb" | grep -q "$VERSION"; then
  ok "[1] clean install exits 0 and installs the requested version"
else
  bad "[1] clean install (rc=$rc): $(cat "$work/out")"
fi

# [2] the original defect: cp fails, stale binary already present ------------
# Mutation that turns this red: restore `cp a b && mv b c` + unconditional
# `info`, which returns 0 with the old binary still in place.
p="$work/p2"; old_binary "$p"; fakebin cp 1
rc=$(run "$p")
if [ "$rc" -ne 0 ] && [ "$(tmpcount "$p")" -eq 0 ] \
   && grep -q 'could not copy' "$work/out" \
   && ! grep -q 'installed' "$work/out" \
   && "$p/autodb" | grep -q 'v0.0.1-OLD'; then
  ok "[2] failed copy is fatal, leaves no temp file, does not claim success"
else
  bad "[2] failed copy masked (rc=$rc, tmp=$(tmpcount "$p")): $(cat "$work/out")"
fi

# [3] rename failure ---------------------------------------------------------
rm -rf "$work/fake"; p="$work/p3"; old_binary "$p"; fakebin mv 1
rc=$(run "$p")
if [ "$rc" -ne 0 ] && [ "$(tmpcount "$p")" -eq 0 ]; then
  ok "[3] failed rename is fatal and cleans up its temp file"
else
  bad "[3] failed rename (rc=$rc, tmp=$(tmpcount "$p")): $(cat "$work/out")"
fi

# [4] the stale-binary trap: copy and rename both "succeed" but change nothing
# Mutation that turns this red: drop the version check, OR weaken it back to
# a substring test (case *"$VERSION"*), which accepts VERSION-old.
rm -rf "$work/fake"; p="$work/p4"; stale_superset "$p"; fakebin cp 0; fakebin mv 0
rc=$(run "$p")
if [ "$rc" -ne 0 ] && grep -q 'not the requested' "$work/out"; then
  ok "[4] a stale VERSION-old binary is refused (exact token, not substring)"
else
  bad "[4] stale VERSION-old binary accepted (rc=$rc): $(cat "$work/out")"
fi

# [5] checksum mismatch fails closed ----------------------------------------
rm -rf "$work/fake"; p="$work/p5"; mkdir -p "$p"
echo "0000000000000000000000000000000000000000000000000000000000000000  $asset.tar.gz" \
  > "$work/release/$asset.tar.gz.sha256"
rc=$(run "$p")
if [ "$rc" -ne 0 ] && grep -q 'checksum mismatch' "$work/out"; then
  ok "[5] checksum mismatch is fatal"
else
  bad "[5] checksum mismatch not caught (rc=$rc): $(cat "$work/out")"
fi
( cd "$work/release" && sha256sum "$asset.tar.gz" > "$asset.tar.gz.sha256" )

# [7] an empty/unreadable download must never verify --------------------------
# The checksum compared the two sums directly, so a missing file made both
# strings empty and "" = "" reported "checksum verified" -- the security
# control approving a file it never read. Mutation that turns this red: drop
# the `[ -s "$1" ]` / `[ -n "$got" ]` guards from verify_sha256.
rm -rf "$work/fake"; p="$work/p7"; mkdir -p "$p"
rc=$(FAKE_CURL_NOWRITE=1 run "$p")
if [ "$rc" -ne 0 ] && ! grep -q 'checksum verified' "$work/out"; then
  ok "[7] an absent download fails closed and never reports 'checksum verified'"
else
  bad "[7] absent download passed verification (rc=$rc): $(cat "$work/out")"
fi
( cd "$work/release" && sha256sum "$asset.tar.gz" > "$asset.tar.gz.sha256" )

# [6] argument handling ------------------------------------------------------
rm -rf "$work/fake"
sh "$script" --help >/dev/null 2>&1 && h=0 || h=1
sh "$script" --bogus  >/dev/null 2>&1 && b=0 || b=1
sh "$script" --version >/dev/null 2>&1 && v=0 || v=1
if [ "$h" -eq 0 ] && [ "$b" -ne 0 ] && [ "$v" -ne 0 ]; then
  ok "[6] --help exits 0; bad flag and missing value exit non-zero"
else
  bad "[6] argument handling (help=$h bogus=$b missing=$v)"
fi

echo "install: $pass passed, $fail failed"
[ "$fail" -eq 0 ] || exit 1
echo "INSTALL-SUITE OK"
