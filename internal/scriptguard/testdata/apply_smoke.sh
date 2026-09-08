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
      # EVERY file the real --create-cert produces, including the SIGNING
      # KEYS. A stub that omitted ca.key and intermediate.key made the
      # ownership assertions vacuous -- their `[ -e ]` guard skipped, so
      # handing the signing keys to the service group passed the smoke. The
      # stub has to produce what the check is about.
      d="$(dirname "$cfg")/tls"; mkdir -p "$d"
      for f in ca.pem cert.pem key.pem intermediate.pem; do echo "stub-$f" > "$d/$f"; done
      for f in ca.key intermediate.key; do echo "stub-$f" > "$d/$f"; chmod 0600 "$d/$f"; done
      echo "stub: wrote $d/{ca,cert,key,intermediate}.pem and the signing keys"; exit 0 ;;
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
# EVERY invocation is recorded, because "did the installer start the service"
# is a claim about a command that was issued, and the only honest way to assert
# it is to look at what was issued.
echo "systemctl $*" >> /tmp/systemctl.log
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

# --- TLS OWNERSHIP: serve, never issue -------------------------------------
#
# A review found the first version handing ca.key and intermediate.key to the
# service account group. certgen.go says ca.key signs the intermediate and
# never leaves the host, so that let a compromised daemon mint certificates
# every distributed ca.pem would trust. The daemon needs the leaf key and the
# public chain; it never needs a signing key.
for f in key.pem cert.pem intermediate.pem ca.pem; do
  [ -e "/etc/autodb/tls/$f" ] || continue
  su -s /bin/sh autodb -c "test -r /etc/autodb/tls/$f" \
    || fail "the service account cannot read /etc/autodb/tls/$f, which it needs to serve"
done
for f in ca.key intermediate.key; do
  # NOT `|| continue`. An absent signing key must fail this check rather than
  # skip it: a stub or a future change that stops producing them would
  # otherwise make the assertion vacuous, which is exactly what happened once.
  [ -e "/etc/autodb/tls/$f" ] || fail "/etc/autodb/tls/$f does not exist, so the signing-key ownership check asserts nothing"
  if su -s /bin/sh autodb -c "test -r /etc/autodb/tls/$f"; then
    ls -la "/etc/autodb/tls/$f"
    fail "the service account CAN READ /etc/autodb/tls/$f -- a signing key. A compromised daemon could mint trusted certificates."
  fi
done
echo "    tls ownership OK (serves, cannot sign)"

# --- THE PLAYBOOK HANDOFF ROUTE --------------------------------------------
#
# The playbook passes --no-init and runs the ceremony itself, then calls
# --hand-off. A review found the ownership fix living only inside this
# script's own init block, so that route left a root-owned store and the
# daemon crash-looped. This asserts the mode the playbook actually uses.
#
# Simulate what --init leaves behind: root-owned store artifacts.
mkdir -p /var/lib/autodb /var/lib/autodb-keys
: > /var/lib/autodb/meta.db
: > /var/lib/autodb/meta.db.lease-info
: > /var/lib/autodb-keys/service.key
chown root:root /var/lib/autodb/meta.db /var/lib/autodb/meta.db.lease-info /var/lib/autodb-keys/service.key
# 0644 for the store and 0600 for the sidecar/keyfile, which is exactly what
# --init left behind on the real host. The store was READABLE and not
# WRITABLE, and the daemon failed on opening it to take the instance lease --
# so writability is the property to assert, not readability. Getting that
# wrong is how a check passes while the daemon still cannot start.
chmod 0644 /var/lib/autodb/meta.db
chmod 0600 /var/lib/autodb/meta.db.lease-info /var/lib/autodb-keys/service.key

su -s /bin/sh autodb -c "test -w /var/lib/autodb/meta.db" \
  && fail "the simulated root-owned store was already writable; the check proves nothing"
su -s /bin/sh autodb -c "test -r /var/lib/autodb-keys/service.key" \
  && fail "the simulated root-owned keyfile was already readable; the check proves nothing"

sh /opt/install_frontdoor.sh --hand-off > /tmp/handoff.log 2>&1 \
  || { cat /tmp/handoff.log; fail "--hand-off exited non-zero"; }
grep -qiE "not found|unbound variable" /tmp/handoff.log && {
  cat /tmp/handoff.log; fail "--hand-off produced a runtime resolution error"; }

# The store must be WRITABLE (the lease is taken by opening it for write) and
# the keyfile READABLE (the unattended unlock reads it).
su -s /bin/sh autodb -c "test -w /var/lib/autodb/meta.db" \
  || { ls -la /var/lib/autodb/meta.db; fail "the store is still not writable by the service account after --hand-off -- this is the permission-denied crash loop"; }
for f in /var/lib/autodb/meta.db.lease-info /var/lib/autodb-keys/service.key; do
  su -s /bin/sh autodb -c "test -r $f" \
    || { ls -la "$f"; fail "$f is still unreadable by the service account after --hand-off"; }
done
echo "    handoff OK (store and keyfile readable by the service)"

# --- THE UNIT DECIDES WHO GETS THE STORE -----------------------------------
#
# --user can name a different account than the unit actually runs as: the
# playbook passing --service-user, or the interview being answered with a name
# of the operator's own. Handing the store to the wrong account produces the
# very crash loop the handoff exists to prevent, so the handoff reads User=
# out of the unit and uses THAT.
#
# This drives the divergence on purpose: the unit is rewritten to run as a
# second account, and --hand-off is then called with the WRONG name.
id svcacct >/dev/null 2>&1 || useradd --system --shell /usr/sbin/nologin svcacct
[ -r /etc/systemd/system/autodb-frontdoor.service ] \
  || fail "no unit to read User= from; this cell would silently prove nothing"
sed -i 's/^User=.*/User=svcacct/' /etc/systemd/system/autodb-frontdoor.service
grep -qx 'User=svcacct' /etc/systemd/system/autodb-frontdoor.service \
  || fail "could not retarget the unit's User=; the divergence was never created"

chown root:root /var/lib/autodb/meta.db /var/lib/autodb-keys/service.key
chmod 0644 /var/lib/autodb/meta.db
chmod 0600 /var/lib/autodb-keys/service.key
su -s /bin/sh svcacct -c "test -w /var/lib/autodb/meta.db" \
  && fail "the store was already writable by svcacct; the check proves nothing"

sh /opt/install_frontdoor.sh --hand-off --user autodb > /tmp/handoff2.log 2>&1 \
  || { cat /tmp/handoff2.log; fail "--hand-off exited non-zero on the divergent unit"; }

su -s /bin/sh svcacct -c "test -w /var/lib/autodb/meta.db" \
  || { ls -la /var/lib/autodb/meta.db; cat /tmp/handoff2.log
       fail "the store was handed to the account named by --user, not the one the unit RUNS AS -- the daemon would crash-loop on permission denied"; }
echo "    handoff follows the unit's User= (not --user) OK"

# --- A FAILED OWNERSHIP OPERATION MUST FAIL THE HANDOFF --------------------
#
# A review caught the gate above being VACUOUS: every chown and chmod in the
# helpers ended in `2>/dev/null || true`, and hand_off_state closed with an
# info line, so --hand-off printed success and exited 0 no matter what
# happened. The caller's start gate could then only ever observe SSH failing
# to reach the host -- never the handoff failing to do its job -- and the
# daemon was still started into the permission-denied crash loop.
#
# Shadowing chown on PATH is the seam because the real failures here are
# environmental -- a read-only or mis-mounted state directory, a restrictive
# security policy -- and none of them can be arranged inside a container
# without becoming a test of the container instead of the script.
mkdir -p /tmp/shadow
printf '#!/bin/sh\nexit 1\n' > /tmp/shadow/chown
printf '#!/bin/sh\nexit 1\n' > /tmp/shadow/chmod
chmod +x /tmp/shadow/chown /tmp/shadow/chmod

# Re-root the store with the REAL chown first, so the induced failure is
# observable BOTH ways: the operation returns non-zero AND the resulting
# ownership is still wrong. A cell that only catches the exit status would
# pass a helper that checked nothing.
chown root:root /var/lib/autodb/meta.db
su -s /bin/sh svcacct -c "test -w /var/lib/autodb/meta.db" \
  && fail "the store was writable before the induced failure; the check proves nothing"

if PATH=/tmp/shadow:$PATH sh /opt/install_frontdoor.sh --hand-off > /tmp/handoff3.log 2>&1; then
  cat /tmp/handoff3.log
  fail "--hand-off exited ZERO while every chown and chmod failed -- the caller's start gate cannot observe this, so the daemon gets started into the crash loop"
fi
grep -q "handoff FAILED" /tmp/handoff3.log \
  || { cat /tmp/handoff3.log; fail "--hand-off failed without saying the handoff failed"; }
grep -q "is owned by root, not svcacct" /tmp/handoff3.log \
  || { cat /tmp/handoff3.log; fail "--hand-off did not report WHICH ownership is wrong; it exited non-zero for an unstated reason"; }
echo "    a failed ownership operation fails the handoff OK"

# AND THE SECURITY HALF: if the signing keys cannot be locked down, that is a
# failed handoff too, not a cosmetic miss. Leaving them reachable by the
# service account is what lets a compromised daemon mint certificates every
# distributed ca.pem would trust.
: > /etc/autodb/tls/ca.key
chown root:root /etc/autodb/tls/ca.key
chmod 0644 /etc/autodb/tls/ca.key
if PATH=/tmp/shadow:$PATH sh /opt/install_frontdoor.sh --hand-off > /tmp/handoff4.log 2>&1; then
  cat /tmp/handoff4.log
  fail "--hand-off exited zero with a signing key left at mode 644"
fi
grep -q "SIGNING KEY" /tmp/handoff4.log \
  || { cat /tmp/handoff4.log; fail "--hand-off did not report the signing key it could not lock down"; }
echo "    an unlockable signing key fails the handoff OK"

# --- THE UNMASKING, ISOLATED -----------------------------------------------
#
# The two cells above are satisfied by EITHER half of the fix: unmasked
# operations, or verification of the resulting state. Mutating the masking back
# in still left them green, because the verification caught the same outcome --
# so they pin the OUTCOME and not the unmasking, and saying otherwise would
# overclaim what they cover.
#
# This isolates it: an operation that FAILS while the resulting state is
# already CORRECT. Ownership is handed off properly first, then only chmod is
# shadowed -- so every ownership check passes and the sole evidence of failure
# is the chmod's exit status. Nothing but an unmasked operation can observe it.
sh /opt/install_frontdoor.sh --hand-off > /tmp/handoff5.log 2>&1 \
  || { cat /tmp/handoff5.log; fail "could not restore a good handoff before isolating the unmasking"; }
su -s /bin/sh svcacct -c "test -w /var/lib/autodb/meta.db" \
  || fail "ownership was not correct going in, so this cell would not isolate the chmod"

mkdir -p /tmp/shadow-chmod
printf '#!/bin/sh\nexit 1\n' > /tmp/shadow-chmod/chmod
chmod +x /tmp/shadow-chmod/chmod
if PATH=/tmp/shadow-chmod:$PATH sh /opt/install_frontdoor.sh --hand-off > /tmp/handoff6.log 2>&1; then
  cat /tmp/handoff6.log
  fail "--hand-off exited zero while every chmod failed -- ownership was already correct, so a masked chmod is invisible and the mode the daemon depends on is never actually set"
fi
grep -q "cannot chmod" /tmp/handoff6.log \
  || { cat /tmp/handoff6.log; fail "--hand-off did not report the chmod it could not perform"; }
rm -rf /tmp/shadow-chmod
echo "    a failed chmod alone fails the handoff OK"
rm -rf /tmp/shadow

# --- THE INSTALLER'S OWN START IS GATED ON THE STORE -----------------------
#
# A review caught the gap the standalone --hand-off strictness did not close:
# on the installer's OWN --init route a failed hand_off_state only warned, and
# the start at the end of the script consulted "start now?" and TLS alone. So a
# successful --create-cert plus a yes to starting launched the daemon into the
# root-owned store -- the original crash loop, on the route an operator running
# this script by hand actually takes.
#
# chown is shadowed ONLY for its RECURSIVE form, which is the form
# hand_off_state uses. Failing every chown would abort --apply at the earlier
# non-recursive one under set -e, and a run that dies before the start proves
# nothing about the gate -- it would be a cell that passes for the wrong reason.
mkdir -p /tmp/shadow-r
cat > /tmp/shadow-r/chown <<'CHOWN'
#!/bin/sh
[ "${1:-}" = "-R" ] && exit 1
exec /bin/chown "$@"
CHOWN
chmod +x /tmp/shadow-r/chown

: > /tmp/systemctl.log
PATH=/tmp/shadow-r:$PATH sh /opt/install_frontdoor.sh --apply --non-interactive --start \
   --assume-ram 961 --assume-cpus 1 \
   --rpc-port 7419 --dns-name db.example.com --port 5432 \
   > /tmp/install-gated.log 2>&1 || true
grep -q "enable --now autodb-frontdoor" /tmp/systemctl.log && {
  echo "--- installer output:"; cat /tmp/install-gated.log
  fail "the installer STARTED the service after failing to hand the store over -- that is the permission-denied crash loop, issued by the installer itself"; }
grep -q "not starting" /tmp/install-gated.log \
  || { cat /tmp/install-gated.log; fail "the installer withheld the start without saying so"; }
rm -rf /tmp/shadow-r
echo "    a failed handoff withholds the installer's own start OK"

# POSITIVE CONTROL. Without it, an installer that never started the service on
# any path would satisfy the assertion above -- an instrument that cannot
# observe the thing whose absence it is asserting. I have shipped that mistake.
: > /tmp/systemctl.log
sh /opt/install_frontdoor.sh --apply --non-interactive --start \
   --assume-ram 961 --assume-cpus 1 \
   --rpc-port 7419 --dns-name db.example.com --port 5432 \
   > /tmp/install-start.log 2>&1 || { cat /tmp/install-start.log; fail "a clean --apply --start exited non-zero"; }
grep -q "enable --now autodb-frontdoor" /tmp/systemctl.log \
  || { cat /tmp/install-start.log; fail "a clean --apply --start did NOT start the service, so the cell above proves nothing"; }
echo "    a clean --apply --start does start the service OK"
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
