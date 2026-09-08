#!/usr/bin/env sh
# Runs install_frontdoor.sh --apply and uninstall.sh --apply to completion
# inside a throwaway container, with stubs for the two things a container has
# no business having: the autodb binary and systemd.
#
# THIS EXISTS BECAUSE NOTHING ELSE EXECUTES THESE PATHS. `sh -n` cannot see an
# undefined function, and every other cell exits before --apply. Two runtime
# defects shipped that way: `step: not found` killed a real provisioning run
# after the unit was written, and rm_file's bare-test return killed a real
# uninstall run on the first absent path. Both would have failed here in a
# second.
set -eu

IMAGE="${SMOKE_IMAGE:-ubuntu:24.04}"
SRC="$(cd "$(dirname "$0")/../../.." && pwd)"

exec docker run --rm -i \
  -v "$SRC/install_frontdoor.sh:/opt/install_frontdoor.sh:ro" \
  -v "$SRC/uninstall.sh:/opt/uninstall.sh:ro" \
  "$IMAGE" sh -s <<'IN'
set -eu

# --- stub autodb: enough surface for the installer to drive it ---
mkdir -p /usr/local/bin
cat > /usr/local/bin/autodb <<'STUB'
#!/bin/sh
cfg=""
while [ $# -gt 0 ]; do
  case "$1" in
    --config) cfg="$2"; shift ;;
    --create-cert)
      d="$(dirname "$cfg")/tls"; mkdir -p "$d"
      for f in ca.pem cert.pem key.pem; do echo "stub-$f" > "$d/$f"; done
      echo "stub: wrote $d/{ca,cert,key}.pem"; exit 0 ;;
    --init) echo "stub: init ran"; exit 0 ;;
    --version) echo "autodb stub"; exit 0 ;;
  esac
  shift
done
exit 0
STUB
chmod +x /usr/local/bin/autodb

# --- stub systemctl: a container has no init ---
cat > /usr/local/bin/systemctl <<'STUB'
#!/bin/sh
case "${1:-}" in
  show) echo inactive ;;
  is-active) exit 3 ;;
  is-enabled) echo disabled; exit 1 ;;
  list-unit-files) ;;
  *) : ;;
esac
exit 0
STUB
chmod +x /usr/local/bin/systemctl

fail() { echo "SMOKE FAIL: $*" >&2; exit 1; }

echo "--- install_frontdoor.sh --apply"
sh /opt/install_frontdoor.sh --apply --non-interactive \
   --assume-ram 961 --assume-cpus 1 \
   --rpc-port 7419 --dns-name db.example.com --port 5432 \
   > /tmp/install.log 2>&1 || { cat /tmp/install.log; fail "install --apply exited non-zero"; }

# The defects this catches are runtime resolution failures, so look for them.
grep -qiE "not found|unbound variable|syntax error" /tmp/install.log && {
  cat /tmp/install.log; fail "install --apply produced a runtime resolution error"; }

for f in /etc/autodb/config.toml /etc/autodb/client.toml \
         /etc/systemd/system/autodb-frontdoor.service \
         /etc/autodb/tls/cert.pem /etc/autodb/tls/ca.pem; do
  [ -r "$f" ] || { cat /tmp/install.log; fail "$f was not created"; }
done

# The phases past the unit must actually have run -- that is where step() died.
grep -q "Issuing TLS material" /tmp/install.log || { cat /tmp/install.log; fail "the TLS phase never ran"; }
grep -q "stub: init ran"       /tmp/install.log || { cat /tmp/install.log; fail "--init was never invoked"; }
grep -q "^enabled = true" /etc/autodb/config.toml || {
  grep -n "^enabled" /etc/autodb/config.toml; fail "the front door was not enabled after certs were issued"; }
grep -q "client_only = true" /etc/autodb/client.toml || fail "client.toml is not client_only"
echo "    install OK"

echo "--- uninstall.sh --apply"
sh /opt/uninstall.sh --apply --yes --remove-swap > /tmp/uninstall.log 2>&1 || {
  cat /tmp/uninstall.log; fail "uninstall --apply exited non-zero"; }
grep -qiE "not found|unbound variable|syntax error" /tmp/uninstall.log && {
  cat /tmp/uninstall.log; fail "uninstall --apply produced a runtime resolution error"; }
# It must REACH THE END, which is what rm_file's return value prevented.
grep -q "^Uninstalled." /tmp/uninstall.log || { cat /tmp/uninstall.log; fail "uninstall did not run to completion"; }
for f in /etc/autodb/config.toml /etc/systemd/system/autodb-frontdoor.service /usr/local/bin/autodb; do
  [ -e "$f" ] && { cat /tmp/uninstall.log; fail "$f survived the uninstall"; }
done
echo "    uninstall OK"

echo "SMOKE PASS"
IN
