#!/usr/bin/env sh
#
# autodb front-door service installer.
#
# Takes a host that ALREADY has the autodb binary (see install.sh) and
# configures the PostgreSQL-wire front door to run as a systemd service:
# a sizing preflight, a config scaffold, optionally a local PostgreSQL
# meta store, and a unit with restart-on-failure and real memory limits.
#
#   ./install_frontdoor.sh --check                    # preflight, changes nothing
#   ./install_frontdoor.sh --check --assume-ram 1024  # size a VPS from your laptop
#   sudo ./install_frontdoor.sh --apply               # interactive on a terminal
#   sudo ./install_frontdoor.sh --apply --non-interactive --bind 0.0.0.0:5432
#
# STATUS -- READ THIS BEFORE --apply.
#
#   --check and --print-config are exercised and safe: they read the host,
#   print numbers, and change nothing.
#
#   --apply IS NOT YET PROVEN ON ANY HOST. Nothing below has been run end to
#   end: the package installs, the PostgreSQL cluster init, the role and
#   database creation, peer auth over the socket, and the systemd unit. Run
#   it against a disposable VM first, never straight at a host you care
#   about.
#
# DISTRO SUPPORT IS WRITTEN, NOT TESTED. The package-manager branches below
# are coded for apt-get, dnf/yum, pacman, apk and zypper. The honest status
# of every one of them is UNTESTED -- they are a best effort at each
# distro's conventions, not a support claim. Each needs one real --apply run
# on that distro before it means anything, and a run on one distro says
# nothing about the other four.
#
#   apt-get (Debian/Ubuntu)  untested
#   dnf / yum (RHEL family)  untested
#   pacman (Arch)            untested
#   apk (Alpine)             untested
#   zypper (SUSE)            untested
#
# Update a row only when a real --apply has run on that distro, and say
# which release it ran on.
#
# SIZING FIGURES ARE PROVISIONAL POLICY, NOT MEASUREMENT.
#
# The reserve fraction, the lane share and the Postgres reserve below are
# CONSERVATIVE GUESSES chosen to fail toward a smaller front door. None is
# derived from a measured resident set. They are deliberately cautious
# because the failure they guard against is silent -- see below -- but they
# are not production sizing guidance and should not be quoted as such.
#
# HOW TO REPLACE THEM WITH MEASUREMENT (do this once there is a real load):
#   1. Run the front door at a known occupancy on the target host.
#   2. Sample RSS at steady state and at peak:
#        systemctl show -p MemoryCurrent autodb-frontdoor
#        grep VmRSS /proc/$(pidof autodb)/status
#   3. Compare peak RSS against general_lane_bytes + the per-session caps
#      that were actually in flight. The gap is what RESERVE_FRACTION and
#      LANE_SHARE are standing in for.
#   4. Update the three constants below, and record the measurement (host,
#      occupancy, peak RSS, date) beside them so the next person can tell a
#      measured number from a guess.
#
# Until step 4 happens, treat every number this script prints as a starting
# point to be checked on the host, not an answer.
#
# WHY THE PREFLIGHT EXISTS. autodb reads no physical-memory figure
# anywhere -- no GOMEMLIMIT, no MemAvailable check, nothing. Its
# front-door budgets are ACCOUNTING, not allocations, so a daemon whose
# budgets exceed the machine starts happily, idles at a fraction of them,
# and then has overload protection that cannot engage before the OS
# out-of-memory killer does. The guard is present, is consulted, and
# observes nothing. Sizing is an install-time decision, and this is where
# it gets made.
#
# POSIX sh. Read it before running it as root.

set -eu

MODE="check"
INTERACTIVE="auto"   # auto | yes | no
PREFIX="${AUTODB_PREFIX:-/usr/local/bin}"
CONFIG_DIR="/etc/autodb"
CONFIG="$CONFIG_DIR/config.toml"
UNIT="/etc/systemd/system/autodb-frontdoor.service"
RUN_USER="autodb"
BIND="0.0.0.0:5432"
STATE_DIR="/var/lib/autodb"
KEY_DIR="/var/lib/autodb-keys"
ASSUME_RAM=""
ASSUME_CPUS=""       # pair with --assume-ram: CPU warnings otherwise describe THIS host

META_BACKEND=""      # sqlite | pg-local | pg-remote  (empty = ask, default sqlite)
META_ENGINE="sqlite" # derived from META_BACKEND
META_DSN=""
PG_DB="autodb"
PG_ROLE=""           # defaults to RUN_USER, so peer auth works with no password
TLS_CERT=""
TLS_KEY=""
TLS_HOSTS=""
TLS_DNS_NAME=""
GEN_CERT="auto"       # auto | yes | no -- run `autodb --create-cert`
RUN_INIT="auto"       # auto | yes | no -- run `autodb --init`
INIT_DONE="no"        # set only once the ceremony actually succeeds
BIND_ADDR="0.0.0.0"
FD_PORT="5432"

# THE RPC ENDPOINT -- the frontends' way in, distinct from the front door.
#
# THE SOCKET IS THE DEFAULT, and a port is an explicit choice. Both halves of
# that matter, so both reasons are written down.
#
# WHY THE SOCKET IS SAFER. It is mode 0600, re-applied on every bind, and the
# file IS the access control -- reaching it proves same-user access, which is
# why a socket peer is exempt from the IP allowlist entirely.
#
# WHY ANYONE WOULD LEAVE IT. A service installed here runs as its own account,
# so that socket is openable only by that account and root. Nobody else on the
# box can run the TUI -- and the TUI is where a developer mints their own PAT
# (auth.token_create authorises any authenticated user, bound to a connection
# they hold a grant on). On a socket, every credential request becomes a root
# operation.
#
# WHY IT IS NOT THE DEFAULT ANYWAY. A first version of this script defaulted
# to the port and justified it by saying the login and rate limits stand in
# the socket's place. THE RATE LIMITS DO NOT EXIST: there is no connection,
# pre-auth or auth-failure throttle anywhere in rpc/ -- the only such throttle
# is the front door's, a different surface -- and core/config says in as many
# words that TCP is M9-gated pending TLS and rate limits. A review found the
# claim before it shipped. So the port is materially weaker than the socket
# against a local attacker, every local account can reach it and attempt a
# login, and it is therefore something an operator ASKS FOR rather than
# inherits.
#
# It binds 127.0.0.1, so nothing is reachable from off the host either way.
RPC_MODE="socket"     # port | socket -- SOCKET IS THE SAFE DEFAULT
RPC_PORT="7419"       # config.DefaultPort
CLIENT_CONFIG=""      # world-readable endpoint-only config, written in port mode
IP_ALLOWLIST='["127.0.0.1/32", "::1/128"]'
START_NOW="no"
CAP_OVERRIDE=""
LANE_OVERRIDE=""

# Matrix figures. These MIRROR THE CODE and must stay in step with it:
#   WATERMARK_MIB      frontdoor.pendingOutputWatermark  (4 MiB)
#   MAX_CAP            config.DefaultMaxSessionsGlobal   (256)
#   MAX_LANE_MIB       config.DefaultGeneralLaneBytes    (1 GiB)
#   MAX_LANE_CEIL_MIB  config.MaxGeneralLaneBytes        (4 GiB)
WATERMARK_MIB=4
MAX_CAP=256
MAX_LANE_MIB=1024
MAX_LANE_CEIL_MIB=4096

# PROVISIONAL POLICY -- unmeasured, conservative, and the numbers to revise
# first once real RSS figures exist. See "SIZING FIGURES ARE PROVISIONAL" in
# the header for how to replace them. These are NOT matrix figures and carry
# none of their authority.
RESERVE_DIVISOR=5            # hold back 1/5 of RAM for OS + runtime + buffers
RESERVE_FLOOR_MIB=256        # ...but never less than this
LANE_DIVISOR=3               # lane gets 1/3 of what remains
PG_RESERVE_POLICY_MIB=384    # a co-hosted Postgres is a second tenant

# Status goes to stdout normally, but to STDERR in --print-config mode, so
# that stdout carries nothing but the config itself and can be piped into a
# validator or a diff without a banner in the middle of it.
MSG_FD=1
say()  { printf '%s\n' "$*" >&"$MSG_FD"; }
# step() prints a phase heading. It exists here because this script CALLS it --
# a runtime "step: not found" killed a real provisioning run after the unit was
# written and before TLS was issued, because the idiom was copied from the
# sibling scripts without the helper. `sh -n` cannot see an undefined function,
# and every cell exited before reaching the apply path, so nothing caught it.
step() { printf '\n=== %s\n' "$*" >&"$MSG_FD"; }
info() { printf '  %s\n' "$*" >&"$MSG_FD"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<'USAGE'
autodb front-door service installer

USAGE:
  install_frontdoor.sh [OPTIONS]

OPTIONS:
  --check              Preflight and print the sizing verdict. Changes
                       nothing. This is the default.
  --print-config       Print the config this host would get, to stdout, and
                       exit. Writes nothing. Useful for review, for diffing
                       against a running config, and for feeding to a
                       validator.
  --print-client-config
                       Print the world-readable CLIENT config (port mode
                       only), to stdout, and exit. Writes nothing. This is
                       what makes its contents assertable -- it must carry
                       the address and nothing sensitive.
  --apply              Write the config and unit, and install Postgres if
                       that is the chosen meta store. Prompts for each
                       setting when run on a terminal.
  --interactive        Force prompting even when stdin is not a terminal.
  --non-interactive    Never prompt; take flags and computed defaults.
  --assume-ram <MiB>   Size for a host of this much RAM instead of the
                       detected figure -- plan a small VPS from a large
                       workstation.
  --assume-cpus <n>    Likewise for the CPU count. Pass it WITH --assume-ram
                       when sizing another machine: without it the CPU
                       warnings describe the host you are standing on, so
                       the single-core argon2 warning silently never fires
                       for the small VM you were sizing for.
  --bind <addr>        Front-door listen address. Default: 0.0.0.0:5432
  --dns-name <name>    DNS name for the TLS certificate. Omitted means no
                       name, and the certificate is issued for an IP address.
  --port <n>           Front-door port. Default: 5432
  --rpc-port <n>       Frontend RPC endpoint on a loopback port. Default:
                       7419. This is what lets developers other than root run
                       the TUI and mint their own PATs.
  --rpc-socket         Use a unix socket for the RPC endpoint instead. Only
                       the account the service runs as (and root) can then
                       reach the TUI.
  --allowlist <toml>   [security] ip_allowlist, as a TOML array. With
                       --rpc-port it MUST admit 127.0.0.1 or the install is
                       refused, because on a port that list gates local
                       callers too.
  --no-cert            Do not issue TLS material. Leaves the door disabled.
  --no-init            Skip the first-run ceremony (autodb --init).
  --user <name>        Service account. Default: autodb
  --prefix <dir>       Where the autodb binary lives. Default: /usr/local/bin
  --config <path>      Config file to write. Default: /etc/autodb/config.toml
  --meta <backend>     Meta store backend, one of:
                         sqlite      one file, no server (default)
                         pg-local    install a NEW local PostgreSQL here
                         pg-remote   use an EXISTING PostgreSQL instance
  --meta-dsn <dsn>     DSN for --meta pg-remote (required with it).
  --sessions <n>       Override the computed max_sessions_global
  --lane <MiB>         Override the computed general lane
  -h, --help           Show this help

The preflight NEVER changes anything, so run it first and read the
numbers. --apply refuses on a host the front door cannot be sized onto.
USAGE
}

while [ $# -gt 0 ]; do
  case "$1" in
    --check)  MODE="check" ;;
    --print-config) MODE="print" ;;
    --print-client-config) MODE="printclient" ;;
    --apply)  MODE="apply" ;;
    --interactive)     INTERACTIVE="yes" ;;
    --non-interactive) INTERACTIVE="no" ;;
    --assume-ram) ASSUME_RAM="${2:?--assume-ram needs a number of MiB}"; shift ;;
    --assume-cpus) ASSUME_CPUS="${2:?--assume-cpus needs a number}"; shift ;;
    --bind)   BIND="${2:?--bind needs an address}"
              # Split so --port and the port prompt stay ONE setting rather
              # than two that can disagree.
              case "$BIND" in *:*) BIND_ADDR="${BIND%:*}"; FD_PORT="${BIND##*:}" ;; esac
              shift ;;
    --dns-name) TLS_DNS_NAME="${2:?--dns-name needs a name}"; shift ;;
    --port)   FD_PORT="${2:?--port needs a number}"; BIND="${BIND_ADDR}:${FD_PORT}"; shift ;;
    --rpc-port) RPC_MODE="port"; RPC_PORT="${2:?--rpc-port needs a number}"; shift ;;
    --rpc-socket) RPC_MODE="socket" ;;
    --allowlist) IP_ALLOWLIST="${2:?--allowlist needs a TOML array}"; shift ;;
    --no-cert) GEN_CERT="no" ;;
    --no-init) RUN_INIT="no" ;;
    --user)   RUN_USER="${2:?--user needs a name}"; shift ;;
    --prefix) PREFIX="${2:?--prefix needs a directory}"; shift ;;
    --config) CONFIG="${2:?--config needs a path}"; CONFIG_DIR="$(dirname "$CONFIG")"; shift ;;
    --meta)   META_BACKEND="${2:?--meta needs sqlite, pg-local or pg-remote}"; shift ;;
    --meta-dsn) META_DSN="${2:?--meta-dsn needs a DSN}"; shift ;;
    --sessions) CAP_OVERRIDE="${2:?--sessions needs a number}"; shift ;;
    --lane)   LANE_OVERRIDE="${2:?--lane needs a number of MiB}"; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown option: $1 (try --help)" ;;
  esac
  shift
done

# `postgres` is accepted as a convenience and resolved by whether a DSN was
# supplied: a DSN means an existing instance, no DSN means install one here.
[ "$META_BACKEND" = "postgres" ] && {
  if [ -n "$META_DSN" ]; then META_BACKEND="pg-remote"; else META_BACKEND="pg-local"; fi
}
case "$MODE" in print|printclient) MSG_FD=2 ;; esac

case "$META_BACKEND" in
  ''|sqlite|pg-local|pg-remote) ;;
  *) die "--meta must be sqlite, pg-local or pg-remote, got: $META_BACKEND" ;;
esac

# apply_backend derives everything downstream from the single choice, so the
# engine name, the locality and the DSN cannot disagree with each other.
apply_backend() {
  case "$META_BACKEND" in
    sqlite)    META_ENGINE="sqlite";   PG_LOCAL="no"  ;;
    pg-local)  META_ENGINE="postgres"; PG_LOCAL="yes" ;;
    pg-remote) META_ENGINE="postgres"; PG_LOCAL="no"  ;;
  esac
}

is_uint() { case "${1:-}" in ''|*[!0-9]*) return 1 ;; *) return 0 ;; esac; }
for pair in "assume-ram:$ASSUME_RAM" "assume-cpus:$ASSUME_CPUS" "sessions:$CAP_OVERRIDE" \
            "lane:$LANE_OVERRIDE" "port:$FD_PORT" "rpc-port:$RPC_PORT"; do
  _n="${pair%%:*}"; _v="${pair#*:}"
  [ -z "$_v" ] && continue
  is_uint "$_v" || die "--$_n needs a non-negative integer, got: $_v"
done

# Prompting reads /dev/tty rather than stdin, so this still works when the
# script itself arrived through a pipe. Without a tty we never prompt: an
# installer that blocks forever on a missing terminal is worse than one
# that takes its defaults and says so.
TTY_OK=0
if [ -r /dev/tty ] && [ -w /dev/tty ]; then TTY_OK=1; fi
case "$INTERACTIVE" in
  yes) [ "$TTY_OK" -eq 1 ] || die "--interactive given but /dev/tty is unusable" ;;
  no)  TTY_OK=0 ;;
  auto) [ "$MODE" = "apply" ] || TTY_OK=0 ;;
esac

ask() {
  _var="$1"; _prompt="$2"; _def="$3"
  if [ "$TTY_OK" -eq 0 ]; then eval "$_var=\$_def"; return 0; fi
  printf '%s [%s]: ' "$_prompt" "$_def" > /dev/tty
  IFS= read -r _ans < /dev/tty || _ans=""
  [ -z "$_ans" ] && _ans="$_def"
  eval "$_var=\$_ans"
}

ask_opt() { # ask_opt <var> <prompt> <default> <allowed...>
  _var="$1"; _prompt="$2"; _def="$3"; shift 3
  while :; do
    ask _o "$_prompt" "$_def"
    for _c in "$@"; do
      if [ "$_o" = "$_c" ]; then eval "$_var=\$_o"; return 0; fi
    done
    [ "$TTY_OK" -eq 0 ] && die "$_prompt: '$_o' is not one of: $*"
    printf '  choose one of: %s\n' "$*" > /dev/tty
  done
}

ask_yn() {
  _var="$1"; _prompt="$2"; _def="$3"
  if [ "$TTY_OK" -eq 0 ]; then eval "$_var=\$_def"; return 0; fi
  while :; do
    printf '%s [%s]: ' "$_prompt" "$_def" > /dev/tty
    IFS= read -r _ans < /dev/tty || _ans=""
    [ -z "$_ans" ] && _ans="$_def"
    case "$_ans" in
      y|Y|yes|YES|Yes) eval "$_var=yes"; return 0 ;;
      n|N|no|NO|No)    eval "$_var=no";  return 0 ;;
      *) printf '  please answer yes or no\n' > /dev/tty ;;
    esac
  done
}

ask_backend() { # ask_backend <var> <prompt> <default>
  _var="$1"; _prompt="$2"; _def="${3:-sqlite}"
  [ -z "$_def" ] && _def="sqlite"
  while :; do
    ask _b "$_prompt" "$_def"
    case "$_b" in
      1|sqlite)    eval "$_var=sqlite";    return 0 ;;
      2|pg-local)  eval "$_var=pg-local";  return 0 ;;
      3|pg-remote) eval "$_var=pg-remote"; return 0 ;;
    esac
    [ "$TTY_OK" -eq 0 ] && die "$_prompt: '$_b' is not 1, 2, 3, sqlite, pg-local or pg-remote"
    printf '  choose 1, 2 or 3 (sqlite, pg-local, pg-remote)\n' > /dev/tty
  done
}

ask_uint() {
  _var="$1"; _prompt="$2"; _def="$3"
  while :; do
    ask _u "$_prompt" "$_def"
    if is_uint "$_u" && [ "$_u" -gt 0 ]; then eval "$_var=\$_u"; return 0; fi
    [ "$TTY_OK" -eq 0 ] && die "$_prompt: '$_u' is not a positive integer"
    printf '  %s is not a positive integer\n' "$_u" > /dev/tty
  done
}

# ------------------------------------------------------------- host detection

if [ -n "$ASSUME_RAM" ]; then
  MEM_MIB="$ASSUME_RAM"
  [ "$MEM_MIB" -gt 0 ] || die "--assume-ram must be greater than zero"
else
  [ -r /proc/meminfo ] || die "cannot read /proc/meminfo; this targets Linux (or pass --assume-ram)"
  _kib="$(awk '/^MemTotal:/ {print $2}' /proc/meminfo)"
  [ -n "$_kib" ] || die "could not determine MemTotal"
  MEM_MIB=$(( _kib / 1024 ))
fi
if [ -n "$ASSUME_CPUS" ]; then
  NCPU="$ASSUME_CPUS"
  [ "$NCPU" -gt 0 ] || die "--assume-cpus must be greater than zero"
else
  NCPU="$(nproc 2>/dev/null || echo 1)"
fi

# Package manager, for the Postgres path.
PKG=""
for _c in apt-get dnf yum pacman apk zypper; do
  if command -v "$_c" >/dev/null 2>&1; then PKG="$_c"; break; fi
done

# detect_public_ip offers a default for the certificate's IP SAN.
#
# The route lookup is what the host uses to reach the internet, which is the
# closest thing to "the address clients dial" that can be answered locally. It
# is still only a GUESS -- see the prompt -- so it is offered, never assumed,
# and an empty answer is not fatal because the operator may know better.
detect_public_ip() {
  ip route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src"){print $(i+1); exit}}' && return 0
  hostname -I 2>/dev/null | awk '{print $1}'
}

pg_socket_dir() {
  for _d in /var/run/postgresql /run/postgresql; do
    [ -d "$_d" ] && { printf '%s' "$_d"; return 0; }
  done
  # Not created until the cluster initialises; Debian and RHEL both use this.
  printf '/var/run/postgresql'
}

# ------------------------------------------------------------------- sizing
#
# Recomputed after the interview, because whether Postgres shares this host
# changes how much is left for the front door. Sizing before that question
# is answered would hand out a budget the box no longer has.

compute_sizing() {
  # Held back for the kernel, the Go runtime, the meta store, per-connection
  # TLS and bufio buffers (320 connections' worth are charged to NO budget),
  # pgx target pools, and the 64 MiB transient spike each argon2id passphrase
  # verification allocates. 20%, with a 256 MiB floor, because a percentage
  # of a small number will not hold any of that.
  # PROVISIONAL (see the header): a fifth of RAM, floor 256 MiB. Unmeasured.
  RESERVE_MIB=$(( MEM_MIB / RESERVE_DIVISOR ))
  [ "$RESERVE_MIB" -lt "$RESERVE_FLOOR_MIB" ] && RESERVE_MIB="$RESERVE_FLOOR_MIB"

  # A CO-HOSTED POSTGRES IS A SECOND TENANT, so it gets its own reserve
  # rather than quietly eating the front door's. Postgres ships
  # shared_buffers at 128 MB plus per-backend work_mem and the WAL buffers
  # on top; 384 MiB is a small-but-real cluster, not a generous one.
  PG_RESERVE_MIB=0
  if [ "$META_ENGINE" = "postgres" ] && [ "$PG_LOCAL" = "yes" ]; then
    PG_RESERVE_MIB="$PG_RESERVE_POLICY_MIB"
    RESERVE_MIB=$(( RESERVE_MIB + PG_RESERVE_MIB ))
  fi

  AVAIL_MIB=$(( MEM_MIB - RESERVE_MIB ))
  [ "$AVAIL_MIB" -lt 0 ] && AVAIL_MIB=0

  # A THIRD of what is left, not all of it. The lane bounds pending output
  # only; segment input (96 MiB per segment) and retained state (16 MiB per
  # session) are per-session CAPS charged as they occur, so one busy session
  # can legitimately hold ~116 MiB the lane does not account for. A lane
  # sized to the whole remainder leaves nothing for the caps beside it.
  LANE_MIB=$(( AVAIL_MIB / LANE_DIVISOR ))
  [ "$LANE_MIB" -gt "$MAX_LANE_MIB" ] && LANE_MIB="$MAX_LANE_MIB"

  CAP=$(( LANE_MIB / WATERMARK_MIB ))
  [ "$CAP" -gt "$MAX_CAP" ] && CAP="$MAX_CAP"
  [ "$CAP" -lt 0 ] && CAP=0

  # Re-derive the lane FROM the cap so the pair satisfies the floor exactly
  # rather than approximately.
  LANE_MIB=$(( CAP * WATERMARK_MIB ))

  [ -n "$CAP_OVERRIDE" ] && CAP="$CAP_OVERRIDE"
  [ -n "$LANE_OVERRIDE" ] && LANE_MIB="$LANE_OVERRIDE"

  GOMEMLIMIT_MIB=$(( MEM_MIB * 75 / 100 ))
  MEMORYMAX_MIB=$(( MEM_MIB * 90 / 100 ))
}

# THE FLOOR IS NOT NEGOTIABLE, so reconcile rather than emit a config that
# fails start. A lane below cap x watermark is refused by the listener, and
# discovering that from a service that will not boot is worse than being
# told here.
reconcile_floor() {
  _floor=$(( CAP * WATERMARK_MIB ))
  if [ "$LANE_MIB" -lt "$_floor" ]; then
    warn "a ${LANE_MIB} MiB lane is below the floor ${CAP} sessions require (${_floor} MiB)."
    warn "raising the lane to ${_floor} MiB -- lower the session cap if you wanted a smaller lane."
    LANE_MIB="$_floor"
  fi
  [ "$LANE_MIB" -gt "$MAX_LANE_CEIL_MIB" ] &&
    die "a ${LANE_MIB} MiB lane is above the ratified ${MAX_LANE_CEIL_MIB} MiB ceiling"
  LANE_BYTES=$(( LANE_MIB * 1024 * 1024 ))
}

emit_config() {
  # enabled = true REQUIRES both TLS keys -- the loader refuses the
  # combination outright, because a client using sslmode=require
  # authenticates nothing and an active MITM collects access tokens in
  # cleartext. So a config written without TLS material ships with the
  # surface OFF rather than shipping a file the daemon cannot load at all.
  # Flip it to true in the same edit that adds the certificate.
  FD_ENABLED=true
  [ -z "$TLS_CERT" ] && FD_ENABLED=false
    cat <<TOML
# autodb -- front-door production config.
# Generated by install_frontdoor.sh for a ${MEM_MIB} MiB / ${NCPU} CPU host.

[server]
TOML
    if [ "$RPC_MODE" = "port" ]; then
      cat <<TOML
# A LOOPBACK PORT, not the shipped unix-socket default, and deliberately.
#
# The socket is mode 0600 and re-applied on every bind, so it is owned by the
# account this service runs as and NOBODY ELSE ON THIS HOST CAN OPEN IT -- not
# another developer, not your own login, only root by permission bypass. The
# TUI is where a developer mints their own PAT, so a socket would make that a
# root operation for everyone.
#
# On a port, each person logs in with their own autodb credentials and the
# boundary becomes [security] ip_allowlist plus that login, instead of file
# ownership. 127.0.0.1 only: reachable by someone already on the box, and by
# nobody off it.
port = $RPC_PORT
bind = "127.0.0.1"
TOML
    else
      cat <<TOML
# A unix socket: mode 0600, owned by the account this service runs as. ONLY
# THAT ACCOUNT AND ROOT can reach the TUI, so minting a PAT is a root
# operation. Chosen explicitly with --rpc-socket; --rpc-port is the
# multi-user setup.
socket = "$STATE_DIR/autodb.sock"
TOML
    fi
    cat <<TOML

[meta]
engine = "$META_ENGINE"
TOML
  if [ "$META_ENGINE" = "postgres" ]; then
    printf 'dsn = "%s"\n' "$META_DSN"
    if [ "$PG_LOCAL" = "yes" ]; then
      cat <<'TOML'

# Reached over a local unix socket, which sslmode=verify-full cannot describe.
# This key exists for exactly that case, and it is NAMED rather than implied so
# that an insecure transport is visible to whoever reads this file. Do NOT set
# it for a Postgres reached across a network: the meta store holds the audit
# trail, the user records and the ENCRYPTED CONNECTION SECRETS.
allow_insecure_dsn = true
TOML
    fi
  else
    printf 'path = "%s/meta.db"\n' "$STATE_DIR"
  fi
  cat <<TOML

[security]
# Enforced at login. Loopback only is the safe default -- widen DELIBERATELY
# and narrowly to the addresses that must reach this daemon.
ip_allowlist = $IP_ALLOWLIST

# Unattended unlock. Without it a reboot leaves the store LOCKED and every
# front-door client gets 57P03 "the server is not accepting connections"
# until a human logs in by hand.
#
# SET, not commented, because the slot cannot be cut without it: enrolment
# refuses outright when no path is configured. \`autodb --init\` cuts the slot
# against this path; until it has, this key names a file that does not exist
# yet, which is harmless -- the daemon reports the absence loudly and a
# passphrase login still works.
#
# The file is 0600 and lives in its OWN directory, not beside the meta store:
# the store and the key that opens it are two halves of one envelope, and one
# careless tar of a shared directory captures both.
service_keyfile = "$KEY_DIR/service.key"

[exec]
# Sized for this host. The general lane's floor is this number x ${WATERMARK_MIB} MiB, so
# these two move TOGETHER -- raising the cap without raising the lane fails
# start, which is the intended direction.
max_sessions_global = $CAP

[frontdoor]
enabled = $FD_ENABLED
bind = "$BIND"

# Sized for this host: $CAP sessions x ${WATERMARK_MIB} MiB = $LANE_MIB MiB.
#
# ACCOUNTING, not an allocation -- nothing is reserved at start. Set it above
# what this machine can hold and backpressure cannot engage before the OS
# out-of-memory killer does.
general_lane_bytes = $LANE_BYTES
TOML
  # tls_host_names goes in FIRST AND ALWAYS, because `autodb --create-cert`
  # reads it to decide the certificate's SANs. Writing the names before the
  # certificate exists is what lets one command issue material that matches
  # the config, rather than two lists that agree until someone edits one.
  if [ -n "$TLS_HOSTS" ]; then
    _hosts="$(printf '%s' "$TLS_HOSTS" | awk -F, '{for(i=1;i<=NF;i++){gsub(/^ +| +$/,"",$i); if($i!=""){printf "%s\"%s\"", (i>1?", ":""), $i}}}')"
    printf '\ntls_host_names = [%s]\n' "$_hosts"
  fi
  if [ -n "$TLS_CERT" ]; then
    printf 'tls_cert_file = "%s"\ntls_key_file = "%s"\n' "$TLS_CERT" "$TLS_KEY"
  else
    cat <<TOML

# THE FRONT DOOR IS OFF ABOVE because TLS is not configured yet, and
# enabled = true without both keys is refused at load -- the daemon would
# not start at all. Set the three keys below and flip enabled to true in
# the same edit.
#
# TLS is mandatory on this surface: a client using sslmode=require
# authenticates nothing, so an active MITM collects every access token in
# cleartext, and a token works from anywhere it is admitted until revoked.
# tls_cert_file = "$CONFIG_DIR/tls/cert.pem"
# tls_key_file  = "$CONFIG_DIR/tls/key.pem"
TOML
  fi
}

emit_client_config() {
    # STATIC PROSE GOES IN A QUOTED HEREDOC, and the variable-bearing lines are
    # printf'd separately. That split is not style.
    #
    # A review found this function rendering with an UNQUOTED heredoc while its
    # own comment contained backticks around a command name. Shell performed
    # command substitution inside the heredoc, so merely RENDERING this config
    # executed `autodb --serve` and pasted its diagnostic into the generated
    # TOML. On a host with no service running, writing a client config would
    # have started a server -- the exact hazard client_only exists to prevent,
    # reintroduced by the comment explaining it.
    #
    # A quoted delimiter disables substitution entirely, which is why every
    # block below that contains prose uses one.
    cat <<'TOML'
# autodb -- CLIENT config. Safe to read; safe to share on this host.
#
# It carries the daemon's address and nothing else. Use it to run the TUI as
# your own user:
TOML
    printf '#\n#   autodb --ui --config %s/client.toml\n#\n' "$CONFIG_DIR"
    cat <<'TOML'
# and from there log in with your own autodb credentials and mint a PAT bound
# to a connection you have been granted.
#
# The SERVER config beside this one is 0640 on purpose: it can name a
# PostgreSQL DSN with a password in it, which a client has no need for.

[server]
TOML
    printf 'port = %s\nbind = "127.0.0.1"\n' "$RPC_PORT"
    cat <<'TOML'

# THIS CONFIG MAY NOT START A DAEMON.
#
# The TUI spawns `autodb --serve` when it cannot dial, which is right on a
# single-user machine and a hazard here: if the service were down, this file
# would start a daemon as YOU, against your own empty meta store, on the port
# the real service binds -- handing you a store you could bootstrap as
# administrator of, while the real service could no longer rebind. With this
# set, a failed dial reports that nothing is listening instead.
client_only = true
TOML
}

# Resolve the backend before the first sizing pass, so --check reports the
# same numbers --apply would use for the same flags. An unspecified backend
# sizes as sqlite, which is what the interview will offer as its default.
PG_LOCAL="no"
[ -z "$META_BACKEND" ] && META_BACKEND="sqlite"
apply_backend

compute_sizing
reconcile_floor

# ----------------------------------------------------------------- preflight

# Checked here so it applies to --print-config and --non-interactive as well
# as the interview: a refusal that only fires when somebody is watching is not
# a guard.
check_loopback_admission() {
# LOOPBACK MUST BE ADMITTED WHEN THE ENDPOINT IS A PORT -- CHECKED, NOT REPAIRED.
#
# On a socket the allowlist is bypassed entirely: reaching the file proves
# same-user access. On a port it becomes the gate, including for a TUI running
# on this very machine, so an allowlist omitting loopback locks every local
# account out -- the operator included -- and the symptom is a login refusal
# with nothing visibly misconfigured.
#
# A first version ADDED loopback for the operator, and a review rejected that
# twice over. Editing somebody's security policy on their behalf is the wrong
# instinct whatever the motive. And the test it used, a substring search for
# "127.0.0.1", matched an entry like 127.0.0.10/32 -- so a list that did NOT
# admit loopback was read as one that did, nothing was added, and the lockout
# happened anyway. The repair had the failure it was written to prevent.
#
# So: an EXACT check against the entries, and a refusal that says what to add.
if [ "$RPC_MODE" = "port" ]; then
  # Entry-wise, not substring: strip brackets and quotes, split on commas, and
  # compare each entry whole. 127.0.0.10/32 is a different entry from
  # 127.0.0.1/32 and must not be mistaken for it.
  _has_loopback=no
  _entries="$(printf '%s' "$IP_ALLOWLIST" | tr -d '[]"' | tr ',' '\n')"
  for _e in $_entries; do
    case "$_e" in
      127.0.0.1|127.0.0.1/32|0.0.0.0/0) _has_loopback=yes ;;
      # A prefix shorter than /32 that contains 127.0.0.1. Only the forms an
      # operator plausibly writes are accepted; anything else is refused
      # rather than guessed at.
      127.0.0.0/8|127.0.0.0/16|127.0.0.0/24|127.0.0.1/8|127.0.0.1/16|127.0.0.1/24) _has_loopback=yes ;;
    esac
  done
  if [ "$_has_loopback" != "yes" ]; then
    die "the RPC endpoint is a loopback port but [security] ip_allowlist does not
       admit 127.0.0.1:
         $IP_ALLOWLIST
       On a port that list is the gate for LOCAL callers too, so nobody on this
       host could log in -- you included. Nothing has been written. Add
       \"127.0.0.1/32\" to the list and run this again, or use the default
       unix socket endpoint, where the allowlist does not apply."
  fi
fi
}

check_loopback_admission

say ""
say "autodb front-door sizing preflight"
say "-----------------------------------"
[ -n "$ASSUME_RAM" ] && info "RAM (assumed)       : ${MEM_MIB} MiB"
[ -z "$ASSUME_RAM" ] && info "detected RAM        : ${MEM_MIB} MiB"
[ -n "$ASSUME_CPUS" ] && info "CPUs (assumed)      : ${NCPU}"
[ -z "$ASSUME_CPUS" ] && info "detected CPUs       : ${NCPU}"
info "meta store          : ${META_ENGINE}$( [ "$PG_LOCAL" = yes ] && printf ' (installed locally)' )"
info "held back for OS etc: ${RESERVE_MIB} MiB"
[ "$PG_RESERVE_MIB" -gt 0 ] && info "  of which postgres : ${PG_RESERVE_MIB} MiB"
info "general lane        : ${LANE_MIB} MiB  (shipped default ${MAX_LANE_MIB} MiB)"
info "max_sessions_global : ${CAP}  (shipped default ${MAX_CAP})"
info "GOMEMLIMIT          : ${GOMEMLIMIT_MIB} MiB"
info "MemoryMax           : ${MEMORYMAX_MIB} MiB"
say ""
say "The reserve, lane share and postgres allowance behind these numbers are"
say "PROVISIONAL policy, not measured figures -- deliberately conservative"
say "guesses. Check them against real RSS on this host before treating any of"
say "this as production sizing. The script header says how."
say ""

if [ "$CAP" -lt 1 ]; then
  warn "this host cannot serve even ONE front-door session after the reserve."
  [ "$PG_RESERVE_MIB" -gt 0 ] &&
    warn "a co-hosted Postgres is taking ${PG_RESERVE_MIB} MiB of it; a managed or separate-host database would free that."
  die "refusing to size the front door onto this host"
fi

if [ "$MEM_MIB" -lt 2048 ]; then
  warn "under 2 GB. The front door will run at a REDUCED session cap (${CAP} rather"
  warn "than ${MAX_CAP}). That is a real reduction in what this host serves, not a"
  warn "tuning nicety: each session may hold up to ~116 MiB across the per-session"
  warn "caps, which no budget on this surface accounts for."
  if [ "$PG_RESERVE_MIB" -gt 0 ]; then
    warn "AND Postgres is sharing this box. On a host this size, give the database"
    warn "its own instance or use sqlite -- co-hosting both is the shape most"
    warn "likely to end in an out-of-memory kill."
  fi
fi

if [ "$NCPU" -lt 2 ]; then
  warn "single CPU. Front-door PAT auth is SHA-256 and cheap, but argon2id"
  warn "passphrase login is configured m=64 MiB, p=4 -- four parallel lanes"
  warn "serialized onto one core, so interactive logins are ~4x slower than the"
  warn "profile assumes, and TLS handshakes land on that same core."
fi

if [ "$MODE" = "print" ]; then
  emit_config
  exit 0
fi

if [ "$MODE" = "printclient" ]; then
  if [ "$RPC_MODE" != "port" ]; then
    die "there is no client config in socket mode: the socket is openable only
       by the service account, so there is nothing a client config could hand
       to anyone else. Pass --rpc-port to see it."
  fi
  CLIENT_CONFIG="$CONFIG_DIR/client.toml"
  emit_client_config
  exit 0
fi

if [ "$MODE" = "check" ]; then
  say ""
  say "Preflight only; nothing was changed. Re-run with --apply to install."
  info "would write config : $CONFIG"
  info "would write unit   : $UNIT"
  [ "$PG_LOCAL" = "yes" ] && info "would install      : postgresql (via ${PKG:-<no package manager found>})"
  say ""
  exit 0
fi

# ------------------------------------------------------------------ interview
#
# Every prompt is pre-filled with the value the preflight computed or a flag
# supplied, so pressing return through the whole interview yields exactly the
# non-interactive result. The two paths must not diverge, or what an operator
# reviews on screen stops being what gets written.

warn "--apply has not been proved end to end on ANY host, and the"
warn "package-manager branch for this distro is UNTESTED. It installs"
warn "packages, creates a system user and writes a systemd unit. Use a"
warn "disposable VM until you have seen it work."

[ "$(id -u)" -eq 0 ] || die "--apply needs root (writes $UNIT)"
[ -x "$PREFIX/autodb" ] || die "no autodb binary at $PREFIX/autodb; run install.sh first"

if [ "$TTY_OK" -eq 1 ]; then
  say "Configuration -- press return to accept each default."
  say ""
fi

ask RUN_USER  "service account" "$RUN_USER"
ask BIND      "front-door bind address" "$BIND"
ask CONFIG    "config file path" "$CONFIG"
CONFIG_DIR="$(dirname "$CONFIG")"
ask STATE_DIR "state directory (sqlite meta store lives here)" "$STATE_DIR"
ask KEY_DIR   "keyfile directory (MUST NOT be the state directory)" "$KEY_DIR"
[ "$KEY_DIR" = "$STATE_DIR" ] && die "the keyfile directory must not be the state directory: the store and the key that opens it are two halves of one envelope, and one careless tar of that directory captures both"

say ""
say "Meta store -- autodb's OWN database (users, grants, encrypted connection"
say "secrets, the audit log). NOT a database you connect TO."
say ""
say "  1) sqlite      one file, no server. Right when the clients are people"
say "                 at a terminal, and the lighter choice on a small VPS."
say "  2) pg-local    install a NEW PostgreSQL on this machine and create the"
say "                 database and role. Connects over the unix socket."
say "  3) pg-remote   use an EXISTING PostgreSQL instance you already run."
say ""
say "Postgres suits many users and long script retention. Migration from"
say "sqlite is supported and ONE-WAY, so choose before there is real data."
ask_backend META_BACKEND "  meta store (1|2|3, or a name)" "$META_BACKEND"
apply_backend

case "$META_BACKEND" in
  pg-local)
    [ -n "$PKG" ] || die "no supported package manager found; choose pg-remote and point --meta-dsn at an instance you install yourself"
    say ""
    ask PG_DB   "  database name" "$PG_DB"
    ask PG_ROLE "  database role (matching the service account enables peer auth)" "${PG_ROLE:-$RUN_USER}"
    say ""
    say "  It will connect over the local unix socket, so [meta] allow_insecure_dsn"
    say "  is set. That key exists precisely for this case -- a trusted local"
    say "  channel -- and it is NAMED so the choice stays visible to whoever reads"
    say "  the config. sslmode=verify-full cannot describe a unix socket."
    ;;
  pg-remote)
    say ""
    say "  Existing Postgres. Its transport is CHECKED AT STARTUP: the DSN needs"
    say "  sslmode=verify-full with an explicit sslrootcert, or autodb refuses to"
    say "  start. 'require' encrypts but authenticates NOTHING. An absent sslmode"
    say "  is not unspecified -- libpq defaults to 'prefer', which silently falls"
    say "  back to plaintext."
    say "  If it is reached over a unix socket or same-host loopback, that is the"
    say "  one case for allow_insecure_dsn, which you can add afterwards."
    ask META_DSN "  dsn" "$META_DSN"
    [ -n "$META_DSN" ] || die "pg-remote requires a dsn"
    case "$META_DSN" in
      *sslmode=verify-full*) ;;
      *) warn "the DSN does not request sslmode=verify-full; autodb will refuse to start unless allow_insecure_dsn is set, and this store holds your encrypted connection secrets" ;;
    esac
    ;;
esac

say ""
say "TLS. The front door will not serve without it, and sslmode=verify-full"
say "verifies the NAME a client dialled -- so the certificate has to carry"
say "whatever your clients will actually type."
say ""
say "  Give a DNS name if this host has one. Leave it EMPTY to use the IP"
say "  address instead, and the certificate will be issued for the IP."
ask TLS_DNS_NAME "  DNS name (empty = use the IP address)" "$TLS_DNS_NAME"

if [ -n "$TLS_DNS_NAME" ]; then
  TLS_HOSTS="$TLS_DNS_NAME"
else
  # No name, so the certificate is issued for an ADDRESS. Offered rather than
  # imposed: the address a client dials is not always the one this host sees
  # itself as -- a NAT, a floating IP or a provider's public address all break
  # that assumption, and sslmode=verify-full checks what the client typed.
  _guess="$(detect_public_ip)"
  say ""
  say "  No DNS name. The certificate will be issued for an IP address, which"
  say "  must be the address CLIENTS DIAL -- not necessarily the one this host"
  say "  sees on its own interface. Behind NAT or a floating address those"
  say "  differ, and verify-full checks the one the client typed."
  ask TLS_HOSTS "  IP address clients will dial" "$_guess"
  [ -n "$TLS_HOSTS" ] || die "no DNS name and no IP address: nothing to issue a certificate for"
fi

# THE PORT, asked here because it belongs with the address a client dials --
# the two together are what somebody types into a client, and splitting them
# across the interview is how one gets set and the other forgotten.
say ""
say "  The port the front door listens on. 5432 is what every PostgreSQL"
say "  client tries first, so changing it means every client must be told."
ask_uint FD_PORT "  front-door port" "$FD_PORT"
[ "$FD_PORT" -le 65535 ] || die "port $FD_PORT is out of range (1..65535)"
BIND="${BIND_ADDR}:${FD_PORT}"

say ""
say "Frontend RPC endpoint -- how the TUI and editor integration reach the"
say "daemon. This is NOT the front door; SQL clients never touch it."
say ""
say "  socket  (default) a unix socket at mode 0600. The file IS the access"
say "          control, so only the service account and root can reach the"
say "          TUI -- which means every PAT request is a root operation."
say "  port    a loopback TCP port. Every account on this host can run the"
say "          TUI with their own autodb login, which is where a developer"
say "          MINTS THEIR OWN PAT."
say ""
say "  CHOOSING port IS A DELIBERATE WEAKENING. It replaces \"same OS user\""
say "  with \"allowlist plus login\", and there is NO rate limiting on that"
say "  surface -- no connection, pre-auth or auth-failure throttle exists in"
say "  the RPC layer, and autodb'"'"'s own config calls TCP M9-gated pending TLS"
say "  and rate limits. Every local account could then reach it and attempt"
say "  logins. Choose it when developer self-service is worth that, and"
say "  keep the host'"'"'s own accounts trusted."
ask_opt RPC_MODE "  rpc endpoint (socket|port)" "$RPC_MODE" socket port
if [ "$RPC_MODE" = "port" ]; then
  ask_uint RPC_PORT "  rpc port" "$RPC_PORT"
  [ "$RPC_PORT" -le 65535 ] || die "rpc port $RPC_PORT is out of range (1..65535)"
fi

say ""
say "Client addresses allowed to reach this daemon, enforced at login."
say "Loopback only is the safe default; widen it deliberately and narrowly."
ask IP_ALLOWLIST "  ip_allowlist (TOML array)" "$IP_ALLOWLIST"


# The meta answer may have changed what this host has to spare.
compute_sizing
reconcile_floor

say ""
say "Memory sizing. These two move TOGETHER: the general lane's floor is"
say "max_sessions_global x ${WATERMARK_MIB} MiB, so raising the cap without raising the"
say "lane fails start. Lowering the CAP is how a small host asks for a"
say "smaller lane."
ask_uint CAP      "  max_sessions_global" "$CAP"
ask_uint LANE_MIB "  general lane (MiB)" "$LANE_MIB"
reconcile_floor

say ""
ask_yn START_NOW "enable and start the service now?" "$START_NOW"

# -------------------------------------------------------------------- summary

say ""
say "About to write"
say "--------------"
info "config             : $CONFIG"
info "unit               : $UNIT"
info "service account    : $RUN_USER"
info "bind               : $BIND"
info "meta engine        : $META_ENGINE$( [ "$PG_LOCAL" = yes ] && printf ' (local install)' )"
[ "$PG_LOCAL" = "yes" ] && info "  database/role    : $PG_DB / ${PG_ROLE:-$RUN_USER}"
[ -n "$META_DSN" ] && [ "$PG_LOCAL" != "yes" ] && info "  dsn              : $META_DSN"
info "state dir          : $STATE_DIR"
info "keyfile dir        : $KEY_DIR"
info "max_sessions_global: $CAP"
info "general lane       : ${LANE_MIB} MiB (${LANE_BYTES} bytes)"
info "GOMEMLIMIT         : ${GOMEMLIMIT_MIB} MiB"
info "MemoryMax          : ${MEMORYMAX_MIB} MiB"
if [ -n "$TLS_CERT" ]; then
  info "tls                : $TLS_CERT (supplied)"
elif [ -n "$TLS_HOSTS" ]; then
  info "tls                : will be ISSUED for $TLS_HOSTS"
else
  info "tls                : NOT configured (keys written commented out)"
fi
info "start now          : $START_NOW"
say ""

ask_yn CONFIRM "proceed?" "yes"
[ "$CONFIRM" = "yes" ] || die "aborted; nothing was written"
say ""

# ---------------------------------------------------------------------- apply

id "$RUN_USER" >/dev/null 2>&1 || {
  say "creating service account $RUN_USER"
  useradd --system --home-dir "$STATE_DIR" --shell /usr/sbin/nologin "$RUN_USER"
}

mkdir -p "$CONFIG_DIR" "$STATE_DIR" "$KEY_DIR"
chown "$RUN_USER:$RUN_USER" "$STATE_DIR" "$KEY_DIR"
# The keyfile directory is deliberately NOT the one holding the meta store.
chmod 0700 "$KEY_DIR"

install_postgres() {
  say "installing PostgreSQL via $PKG"
  case "$PKG" in
    apt-get)
      DEBIAN_FRONTEND=noninteractive apt-get update -qq
      DEBIAN_FRONTEND=noninteractive apt-get install -y -qq postgresql
      ;;
    dnf|yum)
      "$PKG" install -y postgresql-server
      # RHEL-family ships an uninitialised cluster.
      if [ ! -s /var/lib/pgsql/data/PG_VERSION ]; then
        postgresql-setup --initdb
      fi
      ;;
    pacman)
      pacman -Sy --noconfirm postgresql
      if [ ! -s /var/lib/postgres/data/PG_VERSION ]; then
        su - postgres -c "initdb --locale=C.UTF-8 -E UTF8 -D /var/lib/postgres/data"
      fi
      ;;
    apk)
      apk add --no-progress postgresql
      if [ ! -s /var/lib/postgresql/data/PG_VERSION ]; then
        su - postgres -c "initdb -D /var/lib/postgresql/data"
      fi
      ;;
    zypper)
      zypper --non-interactive install postgresql-server
      ;;
    *) die "no supported package manager for a Postgres install" ;;
  esac

  systemctl enable --now postgresql
  # The cluster needs to be accepting connections before roles can be made.
  _tries=0
  until su - postgres -c "psql -tAc 'SELECT 1'" >/dev/null 2>&1; do
    _tries=$(( _tries + 1 ))
    [ "$_tries" -gt 30 ] && die "postgres did not become ready after 30s"
    sleep 1
  done

  _role="${PG_ROLE:-$RUN_USER}"
  # Idempotent: re-running the installer must not fail on an existing role.
  su - postgres -c "psql -tAc \"SELECT 1 FROM pg_roles WHERE rolname='$_role'\"" \
    | grep -q 1 || su - postgres -c "psql -c \"CREATE ROLE \\\"$_role\\\" LOGIN\""
  su - postgres -c "psql -tAc \"SELECT 1 FROM pg_database WHERE datname='$PG_DB'\"" \
    | grep -q 1 || su - postgres -c "createdb -O \"$_role\" \"$PG_DB\""

  # Peer auth over the unix socket: the OS user the service runs as maps to
  # the like-named Postgres role, so there is no password to store anywhere.
  META_DSN="postgres:///${PG_DB}?host=$(pg_socket_dir)"
  say "postgres ready; meta dsn is $META_DSN"
}

[ "$PG_LOCAL" = "yes" ] && install_postgres

if [ -e "$CONFIG" ]; then
  warn "$CONFIG exists; leaving it alone. Compare it against the numbers above."
else
  say "writing $CONFIG"
  emit_config > "$CONFIG"
  chown root:"$RUN_USER" "$CONFIG"
  chmod 0640 "$CONFIG"
fi

# ---------------------------------------------------- the developers' config
#
# A SECOND, ENDPOINT-ONLY CONFIG, WORLD-READABLE.
#
# The server config stays 0640 root:<service> because it can name a PostgreSQL
# DSN with a password in it. But `autodb --ui` calls config.Load, so a
# developer who cannot read a config cannot start the TUI -- and the TUI is
# where they mint their own PAT. Making the server config world-readable to fix
# that would publish the DSN; the fix is to give the client only what a client
# needs, which is the address.
#
# Everything else resolves to that user's own defaults: notes land under their
# home directory, and nothing here points at the meta store, because the TUI
# never touches it. It talks to the daemon.
if [ "$RPC_MODE" = "port" ]; then
  CLIENT_CONFIG="$CONFIG_DIR/client.toml"
  if [ -e "$CLIENT_CONFIG" ]; then
    warn "$CLIENT_CONFIG exists; leaving it alone"
  else
    say "writing $CLIENT_CONFIG"
    emit_client_config > "$CLIENT_CONFIG"

    chmod 0644 "$CLIENT_CONFIG"
    info "any account on this host can run: autodb --ui --config $CLIENT_CONFIG"
  fi
fi

say "writing $UNIT"
_after="network-online.target"
_wants="network-online.target"
if [ "$PG_LOCAL" = "yes" ]; then
  _after="$_after postgresql.service"
  _wants="$_wants postgresql.service"
fi
cat > "$UNIT" <<UNITFILE
[Unit]
Description=autodb PostgreSQL-wire front door
Documentation=https://github.com/yongjohnlee80/autodb
After=$_after
Wants=$_wants

[Service]
Type=simple
User=$RUN_USER
Group=$RUN_USER
ExecStart=$PREFIX/autodb --serve --config $CONFIG
Restart=on-failure
RestartSec=5s

# autodb reads no physical-memory figure of its own, so the ceiling is set
# here. GOMEMLIMIT makes Go's collector aggressive BEFORE the kernel gets
# violent; MemoryMax turns an overrun into a predictable cgroup kill of this
# service rather than the OOM killer choosing a victim elsewhere on the box.
Environment=GOMEMLIMIT=${GOMEMLIMIT_MIB}MiB
MemoryMax=${MEMORYMAX_MIB}M

NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=$STATE_DIR $KEY_DIR
ProtectKernelTunables=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes

[Install]
WantedBy=multi-user.target
UNITFILE

systemctl daemon-reload

# ------------------------------------------------------------ TLS material
#
# AFTER the config, because `autodb --create-cert` reads tls_host_names from
# it. That ordering is why the names were written first and the cert paths
# were not: a config naming files that do not exist yet fails to load, and
# --create-cert loads the config.

if [ "$GEN_CERT" != "no" ] && [ -z "$TLS_CERT" ] && [ -n "$TLS_HOSTS" ]; then
  step "Issuing TLS material for: $TLS_HOSTS"
  if "$PREFIX/autodb" --config "$CONFIG" --create-cert; then
    TLS_CERT="$CONFIG_DIR/tls/cert.pem"
    TLS_KEY="$CONFIG_DIR/tls/key.pem"
    if [ -r "$TLS_CERT" ] && [ -r "$TLS_KEY" ]; then
      # Re-emit rather than sed the file: emit_config is the ONE place that
      # knows this config's shape, and patching it from outside is how the
      # generated file and the generator drift.
      emit_config > "$CONFIG.new" && mv "$CONFIG.new" "$CONFIG"
      chown root:"$RUN_USER" "$CONFIG"; chmod 0640 "$CONFIG"
      # The private key is read by the service account, not the world.
      chown root:"$RUN_USER" "$TLS_KEY" 2>/dev/null || true
      chmod 0640 "$TLS_KEY" 2>/dev/null || true
      info "front door ENABLED in $CONFIG"
      info "give clients $CONFIG_DIR/tls/ca.pem and sslmode=verify-full"
    else
      warn "--create-cert reported success but $TLS_CERT is not readable;"
      warn "leaving the front door disabled."
      TLS_CERT=""; TLS_KEY=""
    fi
  else
    warn "--create-cert failed; leaving the front door disabled. The config and"
    warn "unit are in place, so fix the cause and re-run --apply."
    TLS_CERT=""; TLS_KEY=""
  fi
fi

# -------------------------------------------------------- first-run ceremony
#
# `autodb --init` creates the first administrator and cuts the
# unattended-unlock slot. It has to happen with the service STOPPED, because
# it takes the instance lease on the meta store -- which is also why it runs
# here, after the unit exists but before anything starts it.
#
# Not something this script can do itself: enrolling the slot needs an
# authenticated admin against an unlocked store, and only the process that
# authenticated holds the master key.

if [ "$RUN_INIT" != "no" ]; then
  say ""
  ask_yn _doinit "create the first administrator and enable unattended unlock now?" "yes"
  if [ "$_doinit" = "yes" ]; then
    step "First-run ceremony"
    if "$PREFIX/autodb" --config "$CONFIG" --init; then
      INIT_DONE="yes"
    else
      warn "--init did not complete. The config and unit are in place; run"
      warn "  $PREFIX/autodb --config $CONFIG --init"
      warn "again before starting the service, or a restart will leave the"
      warn "store locked."
    fi
  fi
fi

say ""
if [ "$START_NOW" = "yes" ] && [ -n "$TLS_CERT" ]; then
  say "starting autodb-frontdoor"
  systemctl enable --now autodb-frontdoor
  systemctl --no-pager --lines=0 status autodb-frontdoor || true
elif [ "$START_NOW" = "yes" ]; then
  warn "not starting: TLS is not configured, and a front door without it would"
  warn "either refuse to bind or serve every token in cleartext. Add the tls_*"
  warn "keys to $CONFIG first, then: systemctl enable --now autodb-frontdoor"
fi

say ""
say "Installed. Remaining steps:"
_n=1
if [ -z "$TLS_CERT" ]; then
  info "$_n. Put TLS material in place, uncomment the tls_* keys in $CONFIG,"
  info "   and set [frontdoor] enabled = true -- it is false until TLS exists,"
  info "   because enabled without TLS is refused at load."
  _n=$(( _n + 1 ))
fi
case "$IP_ALLOWLIST" in
  *127.0.0.1*)
    info "$_n. Widen [security] ip_allowlist -- it is loopback-only, so nothing"
    info "   remote can log in yet."
    _n=$(( _n + 1 )) ;;
esac
if [ "$START_NOW" != "yes" ]; then
  info "$_n. Start it:  systemctl enable --now autodb-frontdoor"
  _n=$(( _n + 1 ))
fi
# WHAT --init ACTUALLY DID, rather than a recipe that may already be done. The
# previous version told every operator to uncomment service_keyfile and go to
# the TUI -- including the ones for whom this script had just written the key
# and run the ceremony, so the instructions contradicted the install they were
# printed for.
if [ "$INIT_DONE" = "yes" ]; then
  info "$_n. DONE already: the first administrator was created and the"
  info "   unattended unlock enrolled, so a restart will not lock the store."
else
  info "$_n. Create the first administrator and enrol the unattended unlock:"
  info "     $PREFIX/autodb --config $CONFIG --init"
  info "   Needed before the service is useful, and before a restart can be"
  info "   survived. It prompts for a passphrase, so it needs a terminal, and"
  info "   it takes the instance lease -- run it with the service stopped."
fi
_n=$(( _n + 1 ))
info "$_n. Then the operating sequence, which splits by who does what:"
info ""
info "   AS THE ADMIN (you), in the TUI:"
info "     a. add the target database as a CONNECTION"
info "     b. set that connection to the session profile, or the front door"
info "        will not serve it"
info "     c. create a user for each developer"
info "     d. GRANT each developer on that connection -- this is the"
info "        association that lets them mint against it, and a mint"
info "        without it is refused"
info ""
if [ "$RPC_MODE" = "port" ]; then
  info "   AS EACH DEVELOPER, on this host, needing neither root nor the"
  info "   server config:"
  info "     autodb --ui --config ${CLIENT_CONFIG:-$CONFIG_DIR/client.toml}"
  info "     then log in as themselves and mint a PAT BOUND TO that one"
  info "     connection. The token reaches that connection and nothing else,"
  info "     so a leaked one does not open the estate."
  info ""
  info "   THEIR CLIENT then dials the front door with that token:"
  if [ -n "$TLS_HOSTS" ]; then
    info "     postgres://<user>:<token>@$TLS_HOSTS:$FD_PORT/<database>"
    info "       ?sslmode=verify-full&sslrootcert=<their copy of ca.pem>"
  else
    info "     postgres://<user>:<token>@<host>:$FD_PORT/<database>"
    info "       ?sslmode=verify-full&sslrootcert=<their copy of ca.pem>"
  fi
  info "     ca.pem is the ONE file they each need; export it any time with"
  info "       $PREFIX/autodb --config $CONFIG --export-ca"
else
  info "   AS EACH DEVELOPER: not possible without root. The RPC endpoint is"
  info "   a unix socket, so only the service account and root can reach the"
  info "   TUI, and minting is where a developer would get their own token."
  info "   Re-run with --rpc-port to give them self-service."
fi
say ""
