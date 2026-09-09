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
TLS_CA=""             # private CA from --create-cert; the chain's trust root
TLS_HOSTS=""
TLS_DNS_NAME=""
GEN_CERT="auto"       # auto | yes | no -- run `autodb --create-cert`
# CLEARTEXT: serve the front door WITHOUT TLS.
#
# There was no way to install this. The config layer accepts it — one exact
# sentence, insecure_disable_tls = "i-accept-that-every-pat-crosses-in-
# cleartext", deliberately a sentence rather than a flag so that turning TLS
# off cannot be done without stating what it costs — but the installer only
# knew how to ship the surface OFF when TLS material was absent. So the one
# supported way to get a cleartext front door was to install, watch it come up
# disabled, and hand-edit the file the generator owns.
#
# It exists for a trusted network and for live testing of the bring-up itself,
# which is what it was added for. It is NOT a convenience: every access token
# crosses in cleartext and works from anywhere it is admitted until revoked.
CLEARTEXT="no"
RUN_INIT="auto"       # auto | yes | no -- run `autodb --init`
INIT_DONE="no"        # set only once the ceremony actually succeeds
KEEP_CONFIG="no"      # a re-run REPLACES the config unless this is set
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
# STATE_OK is the startability of the STORE, tracked separately from START_NOW
# (what the operator asked for) and from TLS_CERT (whether it can serve).
#
# A review caught the reason it has to exist: a failed hand_off_state only
# warned, and the start at the bottom of this script consulted START_NOW and
# TLS_CERT alone -- so a successful --create-cert plus "start now: yes" launched
# the daemon into a store it cannot open. My comment there said "the caller
# decides whether to start", and the caller IS this script.
STATE_OK="yes"
STARTED="no"
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
# have_tty reports whether this process can actually USE a terminal.
#
# `[ -r /dev/tty ]` does not answer that, and every script here asked it. It
# tests the PATH's permission bits, which are satisfied on any Linux box —
# while a process with no controlling terminal (cron, CI, a systemd unit, a
# backgrounded shell) gets ENXIO the moment it opens the device. Measured: in
# `setsid sh -c ...` the test is TRUE and the very next write fails with "No
# such device or address".
#
# So the confirmation prompt guarded by that test was reached in exactly the
# environments it was meant to skip, and the run died at the prompt instead of
# proceeding or refusing cleanly. Opening it is the only test that answers the
# question.
# A SUBSHELL, and that is not style. `:` is a POSIX SPECIAL BUILT-IN, and a
# redirection error on a special built-in makes a non-interactive shell EXIT —
# so `{ : < /dev/tty; }` does not return false when there is no terminal, it
# kills the script. In exactly the case this function exists to detect.
#
# Measured: under `setsid`, the subshell form and a regular built-in (`true`)
# both survive and report false, while the special-built-in form terminated the
# script before the next line ran. My first version of this helper used it, and
# it took the installer down silently on VM43 — exit 1, zero bytes on both
# streams — which is the same failure mode as the guard it replaced, introduced
# by the fix for it.
#
# The subshell is robust whichever built-in is used: an exit inside it is just a
# status to the caller.
have_tty() { ( : < /dev/tty ) 2>/dev/null; }

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
  --hand-off           Give the meta store, the keyfile and the TLS material
                       to the service account, then exit. For a caller that
                       ran `autodb --init` itself: the ceremony runs as root,
                       so without this the daemon cannot open its own store.
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
  --start              Enable and start the service at the end. Answers the
                       interview's "start now?" without a terminal, so
                       --non-interactive can reach it at all. The start is
                       still withheld if TLS is not configured or if the store
                       could not be handed to the service account.
  --no-start           Do not start it (the default).
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
  --keep-config        Do not replace an existing config. Without this a
                       re-run rewrites it, so the flags you pass actually
                       take effect.
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
# AN OPTION PASSED ON THE COMMAND LINE IS AN ANSWER, NOT A DEFAULT.
#
# Every question below took the current value as its DEFAULT and asked anyway,
# so the interview could silently overturn a flag the caller passed
# deliberately. That produced two real failures:
#
#   - The playbook runs this installer over `ssh -t` and then runs
#     `autodb --init` itself, which needs the daemon STOPPED because --init
#     takes the instance lease. The installer asked "enable and start the
#     service now?", an operator said yes, and the ceremony then died on
#     ErrLeaseHeld BEFORE prompting for anything -- which reads exactly like
#     "it never asked me for a root password". No administrator, no keyslot,
#     and a running daemon.
#   - `--user` set the service account, and the interview let a typed answer
#     disagree with the account the playbook then handed the store to. That
#     one was contained by teaching --hand-off to read User= out of the unit;
#     the underlying rule was never fixed.
#
# The rule lives in the ask helpers, keyed by VARIABLE NAME, so every question
# inherits it and a new option cannot forget to honour its own flag.
GIVEN=""
mark()  { GIVEN="$GIVEN $1 "; }
given() { case "$GIVEN" in *" $1 "*) return 0 ;; *) return 1 ;; esac; }


while [ $# -gt 0 ]; do
  case "$1" in
    --check)  MODE="check" ;;
    --print-config) MODE="print" ;;
    --print-client-config) MODE="printclient" ;;
    --hand-off) MODE="handoff" ;;
    --apply)  MODE="apply" ;;
    --interactive)     INTERACTIVE="yes" ;;
    --non-interactive) INTERACTIVE="no" ;;
    # --start makes "start it now" answerable WITHOUT a terminal. It was only
    # ever an interview question, so --non-interactive could never start the
    # service -- and the start gate below could not be exercised by a test
    # without a person typing yes.
    --start) START_NOW="yes"; mark START_NOW ;;
    --no-start) START_NOW="no"; mark START_NOW ;;
    --assume-ram) ASSUME_RAM="${2:?--assume-ram needs a number of MiB}"; shift ;;
    --assume-cpus) ASSUME_CPUS="${2:?--assume-cpus needs a number}"; shift ;;
    --bind)   BIND="${2:?--bind needs an address}"
              # Split so --port and the port prompt stay ONE setting rather
              # than two that can disagree.
              case "$BIND" in *:*) BIND_ADDR="${BIND%:*}"; FD_PORT="${BIND##*:}" ;; esac
              shift; mark BIND; mark FD_PORT ;;
    --dns-name) TLS_DNS_NAME="${2:?--dns-name needs a name}"; shift; mark TLS_DNS_NAME ;;
    --port)   FD_PORT="${2:?--port needs a number}"; BIND="${BIND_ADDR}:${FD_PORT}"; shift; mark FD_PORT ;;
    --rpc-port) RPC_MODE="port"; RPC_PORT="${2:?--rpc-port needs a number}"; shift; mark RPC_PORT; mark RPC_MODE ;;
    --rpc-socket) RPC_MODE="socket"; mark RPC_PORT; mark RPC_MODE ;;
    --allowlist) IP_ALLOWLIST="${2:?--allowlist needs a TOML array}"; shift; mark IP_ALLOWLIST ;;
    --no-cert) GEN_CERT="no" ;;
    --cleartext) CLEARTEXT="yes"; GEN_CERT="no"; mark CLEARTEXT ;;
    --no-init) RUN_INIT="no" ;;
    --keep-config) KEEP_CONFIG="yes" ;;
    --user)   RUN_USER="${2:?--user needs a name}"; shift; mark RUN_USER ;;
    --prefix) PREFIX="${2:?--prefix needs a directory}"; shift ;;
    --config) CONFIG="${2:?--config needs a path}"; CONFIG_DIR="$(dirname "$CONFIG")"; shift; mark CONFIG ;;
    --meta)   META_BACKEND="${2:?--meta needs sqlite, pg-local or pg-remote}"; shift; mark META_BACKEND ;;
    --meta-dsn) META_DSN="${2:?--meta-dsn needs a DSN}"; shift; mark META_DSN ;;
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
if have_tty; then TTY_OK=1; fi
case "$INTERACTIVE" in
  yes) [ "$TTY_OK" -eq 1 ] || die "--interactive given but /dev/tty is unusable" ;;
  no)  TTY_OK=0 ;;
  # auto + apply + NO TERMINAL used to mean "take every default and mutate
  # anyway", because ask() silently substitutes the default when TTY_OK is 0.
  # That is the same fail-open shape review found in the other three scripts:
  # the absence of a terminal authorized an unattended install nobody asked
  # for. --non-interactive is the operator SAYING "take the defaults", and it
  # is now required rather than inferred.
  auto)
    if [ "$MODE" = "apply" ] && [ "$TTY_OK" -eq 0 ]; then
      die "refusing to --apply with no terminal and no explicit mode: there is
       nothing to prompt on, so every answer would be a default this script
       chose. Nothing has been changed. Pass --non-interactive to accept the
       computed defaults deliberately, or run from a terminal to be asked."
    fi
    [ "$MODE" = "apply" ] || TTY_OK=0
    ;;
esac

ask() {
  _var="$1"; _prompt="$2"; _def="$3"
  if given "$_var"; then return 0; fi
  if [ "$TTY_OK" -eq 0 ]; then eval "$_var=\$_def"; return 0; fi
  printf '%s [%s]: ' "$_prompt" "$_def" > /dev/tty
  IFS= read -r _ans < /dev/tty || _ans=""
  [ -z "$_ans" ] && _ans="$_def"
  eval "$_var=\$_ans"
}

# EACH WRAPPER OWNS ITS TARGET NAME, and that is not style.
#
# These helpers delegate to ask(), which assigns to whatever `_var` holds --
# and `_var` is a GLOBAL, because POSIX sh has no locals. So a wrapper that
# kept its caller's variable in `_var` had it overwritten by the nested call,
# and its own write-back became `_o=$_o`: a no-op. A review reproduced it over
# a pty -- entering 6000 for the front-door port left FD_PORT at 5432, and
# choosing pg-remote left META_BACKEND at sqlite.
#
# Every multi-choice and numeric answer in the interview was silently
# discarded, including the front-door port prompt this installer added for
# exactly that purpose. Distinct names (_ovar/_uvar/_bvar) make the clobber
# impossible rather than merely absent.
ask_opt() { # ask_opt <var> <prompt> <default> <allowed...>
  _ovar="$1"; _oprompt="$2"; _odef="$3"
  if given "$_ovar"; then return 0; fi; shift 3
  while :; do
    ask _o "$_oprompt" "$_odef"
    for _c in "$@"; do
      if [ "$_o" = "$_c" ]; then eval "$_ovar=\$_o"; return 0; fi
    done
    [ "$TTY_OK" -eq 0 ] && die "$_oprompt: '$_o' is not one of: $*"
    printf '  choose one of: %s\n' "$*" > /dev/tty
  done
}

ask_yn() {
  _var="$1"; _prompt="$2"; _def="$3"
  if given "$_var"; then return 0; fi
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
  _bvar="$1"; _bprompt="$2"; _def="${3:-sqlite}"
  if given "$_bvar"; then return 0; fi
  [ -z "$_def" ] && _def="sqlite"
  while :; do
    ask _b "$_bprompt" "$_def"
    case "$_b" in
      1|sqlite)    eval "$_bvar=sqlite";    return 0 ;;
      2|pg-local)  eval "$_bvar=pg-local";  return 0 ;;
      3|pg-remote) eval "$_bvar=pg-remote"; return 0 ;;
    esac
    [ "$TTY_OK" -eq 0 ] && die "$_bprompt: '$_b' is not 1, 2, 3, sqlite, pg-local or pg-remote"
    printf '  choose 1, 2 or 3 (sqlite, pg-local, pg-remote)\n' > /dev/tty
  done
}

ask_uint() {
  _uvar="$1"; _uprompt="$2"; _def="$3"
  if given "$_uvar"; then return 0; fi
  while :; do
    ask _u "$_uprompt" "$_def"
    if is_uint "$_u" && [ "$_u" -gt 0 ]; then eval "$_uvar=\$_u"; return 0; fi
    [ "$TTY_OK" -eq 0 ] && die "$_uprompt: '$_u' is not a positive integer"
    printf '  %s is not a positive integer\n' "$_u" > /dev/tty
  done
}

# cert_failure_note explains a FAILED --create-cert according to WHAT FAILED.
#
# autodb exits 78 (EX_CONFIG) when the failure is the configuration rather than
# the command, and every subcommand loads the config -- so --create-cert can
# fail for a reason that has nothing to do with certificates. That is not
# hypothetical: it stopped a real 1 vCPU provisioning run, and this script
# reported "--create-cert failed", which sent the operator to look at TLS.
#
# A FUNCTION so it can be driven without a host: see the define-only mode
# below. The branch used to be inline in the apply path, reachable only through
# a full provisioning run, so review could not see it execute.
cert_failure_note() {
  if [ "${1:-1}" -eq 78 ]; then
    warn "the CONFIG is invalid -- certificate generation never ran. The message"
    warn "above is autodb's; fix the setting it names in $CONFIG, then re-run:"
    warn "  $0 --apply --config $CONFIG"
  else
    warn "--create-cert failed; leaving the front door disabled. The config and"
    warn "unit are in place, so fix the cause and re-run --apply."
  fi
}

# dsn_is_local reports whether a DSN names a channel that cannot leave this
# host, which is the ONE case where allow_insecure_dsn is legitimate.
#
# COMPUTED, NOT ASKED. pg-remote used to warn that a DSN without
# sslmode=verify-full would refuse to start and tell the operator they "can add
# allow_insecure_dsn afterwards" -- after the run had written a config and tried
# to start a daemon that could not load it. On a loopback DSN, which the same
# paragraph calls the legitimate case, the script had every fact needed to get
# it right and made the operator find out by failing. Measured on VM43, whose
# Postgres does not speak TLS at all.
#
# LOCAL means a unix socket, 127.0.0.0/8, ::1 or the literal localhost.
# Anything else is a network, and a network without verify-full stays refused:
# this store holds the audit trail, the user records and the ENCRYPTED
# CONNECTION SECRETS, so a predicate that guessed generously would be worse
# than the warning it replaces.
# host_is_local decides ONE host string, by exact match rather than by
# substring.
#
# The substring version of this was a fail-open security defect, found on
# review and reproduced through the real --print-config path:
#
#   postgres://u:p@db.example/m?host=localhost.evil&sslmode=disable
#
# matched `*host=localhost*` and emitted allow_insecure_dsn = true, permitting
# plaintext transport to a REMOTE meta store -- the store holding the audit
# trail, the user records and the encrypted connection secrets. The comment on
# the old predicate said a version that "guessed generously would be worse than
# the warning it replaces". It guessed generously.
host_is_local() {
  case "$1" in
    /*)                  return 0 ;;   # a unix socket directory
    localhost)           return 0 ;;   # EXACT, so localhost.evil is not local
    ::1|0:0:0:0:0:0:0:1) return 0 ;;
    *[!0-9.]*)           return 1 ;;   # not a bare IPv4 literal: not local
  esac
  # Digits and dots only from here. Require EXACTLY four octets, all in range,
  # with 127 first -- so 127.0.0.1.evil.com (rejected above) and 1270.0.0.1
  # and 127.1 are all refused rather than pattern-matched.
  _ifs_save=$IFS
  IFS=.
  # shellcheck disable=SC2086
  set -- $1
  IFS=$_ifs_save
  [ $# -eq 4 ] || return 1
  [ "$1" = "127" ] || return 1
  for _o in "$1" "$2" "$3" "$4"; do
    case "$_o" in ''|*[!0-9]*) return 1 ;; esac
    [ "$_o" -le 255 ] || return 1
  done
  return 0
}

# dsn_host_candidates prints EVERY host the DSN could resolve to, one per line.
#
# Every one, not the first: a URL can carry an authority host and a `host=`
# query parameter at once, and which of them libpq honours is exactly the
# ambiguity the exploit above turned on. Rather than model that precedence --
# and be wrong for some client version -- this collects all of them and
# dsn_is_local requires them all to be local. A DSN whose meaning depends on
# precedence is refused, which is the right answer for a decision about
# plaintext transport.
dsn_host_candidates() {
  # Keyword form and URL query string: every `host=` assignment, whether
  # separated by spaces (keyword DSN) or by ? and & (URL query).
  printf '%s' "$1" | tr '?& ' '\n\n\n' | while IFS= read -r _kv; do
    case "$_kv" in host=*) printf '%s\n' "${_kv#host=}" ;; esac
  done
  # The URL authority, if this is a URL at all.
  case "$1" in
    *://*)
      _rest=${1#*://}
      _auth=${_rest%%/*}      # drop the path
      _auth=${_auth%%\?*}     # and the query, for a URL with no path
      _hostport=${_auth##*@}  # drop any userinfo
      case "$_hostport" in
        # A bracketed IPv6 literal keeps its colons; everything else splits on
        # the port separator.
        \[*\]*) printf '%s\n' "$(printf '%s' "$_hostport" | sed 's/^\[\([^]]*\)\].*/\1/')" ;;
        *)      printf '%s\n' "${_hostport%%:*}" ;;
      esac
      ;;
  esac
}

# dsn_is_local reports whether a DSN names a channel that cannot leave this
# host, which is the ONE case where allow_insecure_dsn is legitimate.
#
# COMPUTED, NOT ASKED. pg-remote used to warn that a DSN without
# sslmode=verify-full would refuse to start and tell the operator they "can add
# allow_insecure_dsn afterwards" -- after the run had written a config and
# tried to start a daemon that could not load it. On a loopback DSN the script
# had every fact needed to get it right and made the operator find out by
# failing. Measured on VM43, whose Postgres does not speak TLS at all.
#
# FAIL CLOSED, in both directions. Every candidate host must be provably local;
# one that is not refuses the whole DSN. A DSN naming NO host is also refused
# even though libpq would default it to a local socket -- refusing costs the
# operator a warning they can answer, and guessing costs plaintext to a store
# full of secrets.
dsn_is_local() {
  _seen=0
  for _h in $(dsn_host_candidates "$1"); do
    _seen=1
    host_is_local "$_h" || return 1
  done
  [ "$_seen" -eq 1 ] || return 1
  return 0
}

# DEFINE-ONLY MODE: stop here with every function defined and nothing done.
#
# A testing seam, and an honest one. The reporting branches in this script were
# reachable only through a full provisioning run, so a cell could assert their
# text but never watch them execute -- which is how the handoff gate came to
# need its own flag. This is the general form of that: a cell sources the
# script, gets the functions, and drives one.
#
# Placed after every definition and BEFORE the first side effect, so sourcing
# it cannot detect a host, write a file or start anything.
if [ -n "${AUTODB_INSTALL_DEFINE_ONLY:-}" ]; then
  return 0 2>/dev/null || exit 0
fi

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

  # Must exceed reserved_headroom (4) with room to serve, and the shipped
  # 2 x CPU default does not on a small host. Eight is the smallest value that
  # leaves a usable lease budget at one core; a bigger box keeps its own.
  POOL_MAX_CONNS=$(( 2 * NCPU ))
  [ "$POOL_MAX_CONNS" -lt 8 ] && POOL_MAX_CONNS=8

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
  # CLEARTEXT enables it WITHOUT TLS material, which is the whole point: there
  # is no identity to prove, so cert/key/host names are not required. The
  # acknowledgement is what the loader checks, and it is written in full so the
  # next reader of this file sees the sentence rather than a flag.
  [ "$CLEARTEXT" = "yes" ] && FD_ENABLED=true
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
    if [ "$PG_LOCAL" = "yes" ] || dsn_is_local "$META_DSN"; then
      cat <<'TOML'

# Reached over a channel that cannot leave this host, which
# sslmode=verify-full cannot describe.
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
# SET EXPLICITLY, because the shipped default is 2 x CPU COUNT and
# reserved_headroom is 4 -- so on a 1 or 2 vCPU host the defaults contradict
# each other and the config will not load at all:
#
#   1 vCPU -> pool_max_conns 2, headroom 4, leaves -2  INVALID
#   2 vCPU -> pool_max_conns 4, headroom 4, leaves  0  INVALID
#
# That is not hypothetical: it stopped a real provisioning run on a 1 vCPU
# droplet, and it stopped it inside --create-cert, which loads the config --
# so the failure surfaced as "certificate generation failed" rather than as
# "your config is invalid".
pool_max_conns = $POOL_MAX_CONNS

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
  if [ "$CLEARTEXT" = "yes" ]; then
    cat <<'TOML'

# TLS IS OFF ON THIS FRONT DOOR, deliberately, and this sentence is the only
# value the loader accepts for saying so. It is a sentence rather than a flag
# because turning TLS off should not be possible without writing down what it
# costs: every access token crosses this wire in CLEARTEXT, and a token works
# from anywhere it is admitted until it is revoked.
#
# Legitimate for a trusted network and for testing the bring-up itself. On
# anything reachable from a network you do not control, this is how tokens are
# harvested. Remove this key and set the three tls_* keys to close it.
insecure_disable_tls = "i-accept-that-every-pat-crosses-in-cleartext"
TOML
  elif [ -n "$TLS_CERT" ]; then
    printf 'tls_cert_file = "%s"\ntls_key_file = "%s"\n' "$TLS_CERT" "$TLS_KEY"
    # THE TRUST ROOT, without which the chain cannot verify. frontdoor builds
    # its roots from tls_root_ca_file and falls back to the SYSTEM pool when it
    # is unset -- which does not contain a private CA that --create-cert just
    # made. The daemon then refuses to serve with "certificate signed by
    # unknown authority", and prints the leaf's DNS names beside it, which
    # reads as a hostname problem and is not one.
    [ -n "$TLS_CA" ] && printf 'tls_root_ca_file = "%s"\n' "$TLS_CA"
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

# grant_tls_read gives the service account exactly what it needs to SERVE, and
# nothing that lets it ISSUE.
#
# A first version chgrp'd the whole directory and chmod 0640'd every file in
# it, which handed ca.key and intermediate.key to the service account's group.
# certgen.go says of ca.key that it signs the intermediate and never leaves
# this host -- so that made a compromised daemon able to mint certificates
# every distributed ca.pem would trust, for any name it liked. A review caught
# it. Serving needs the leaf key and the public chain; it never needs a
# signing key.
# NOTHING HERE IS MASKED, and each check is on the RESULTING STATE rather than
# on the command's exit status.
#
# A review caught the version below this one: every chown and chmod ended in
# `2>/dev/null || true`, so a real ownership failure -- a read-only or
# mis-mounted state directory, a restrictive policy, a chmod refused -- still
# printed "Handed off" and exited 0. That made the caller's start gate VACUOUS:
# it could only ever observe ssh failing to reach the host, never the handoff
# failing to do its job, so the daemon was still started into the
# permission-denied crash loop by a different path.
#
# Verifying the state, not the call, is deliberate: a chown can succeed and
# leave the wrong owner (a symlink, a racing remount), and the property the
# daemon needs is who owns the file, not whether a command returned 0.
grant_tls_read() {
  _d="$CONFIG_DIR/tls"
  # NO TLS MATERIAL YET IS LEGITIMATE -- the installer writes the front door
  # disabled precisely because certificates may not exist. Absent is optional;
  # present-and-unfixable is a failure.
  [ -d "$_d" ] || return 0
  _rc=0

  # Traversal only: group-executable so the daemon can reach the files named
  # below, NOT group-readable, so it cannot enumerate what else is there.
  chgrp "$RUN_USER" "$_d" || { warn "cannot chgrp $_d to $RUN_USER"; _rc=1; }
  chmod 0710 "$_d" || { warn "cannot chmod 0710 $_d"; _rc=1; }

  for _f in key.pem cert.pem intermediate.pem ca.pem; do
    [ -e "$_d/$_f" ] || continue
    chgrp "$RUN_USER" "$_d/$_f" || { warn "cannot chgrp $_d/$_f"; _rc=1; }
    chmod 0640 "$_d/$_f" || { warn "cannot chmod 0640 $_d/$_f"; _rc=1; }
    _g="$(stat -c '%G' "$_d/$_f" 2>/dev/null || echo '?')"
    [ "$_g" = "$RUN_USER" ] || {
      warn "$_d/$_f is group $_g, not $RUN_USER -- the daemon cannot read it"; _rc=1; }
  done

  # SIGNING KEYS STAY root:root 0600, restated rather than left at whatever
  # --create-cert produced, so this function is the whole policy -- and
  # VERIFIED, because failing to lock them down is not a cosmetic miss: it
  # leaves a compromised daemon able to mint certificates every distributed
  # ca.pem would trust, for any name it likes.
  for _f in ca.key intermediate.key; do
    [ -e "$_d/$_f" ] || continue
    chown root:root "$_d/$_f" || { warn "cannot chown $_d/$_f to root:root"; _rc=1; }
    chmod 0600 "$_d/$_f" || { warn "cannot chmod 0600 $_d/$_f"; _rc=1; }
    _m="$(stat -c '%U:%G %a' "$_d/$_f" 2>/dev/null || echo '?')"
    [ "$_m" = "root:root 600" ] || {
      warn "SIGNING KEY $_d/$_f is $_m, not root:root 600 -- refusing to call this"
      warn "a successful handoff: the service account must never be able to sign"
      _rc=1; }
  done
  return "$_rc"
}

# hand_off_state gives the meta store and the keyfile to the service account.
#
# ONE implementation, because there are two routes to needing it: this script
# running --init itself, and the playbook running --init separately over a pty
# and then starting the service. The first version only covered the first
# route, so the normal playbook path still produced a root-owned store and the
# daemon crash-looped on "permission denied" -- the very failure the handoff
# was added to fix.
hand_off_state() {
  _rc=0
  # THESE DIRECTORIES ARE EXPECTED, not optional: --apply creates both. Their
  # absence here means the handoff is being run against a box that was never
  # installed, and calling that a success is how a caller comes to start a
  # daemon with no store to open.
  for _p in "$STATE_DIR" "$KEY_DIR"; do
    [ -d "$_p" ] || { warn "$_p does not exist; was the installer ever applied?"; _rc=1; }
  done
  [ "$_rc" -eq 0 ] || return 1

  chown -R "$RUN_USER":"$RUN_USER" "$STATE_DIR" "$KEY_DIR" \
    || { warn "cannot chown $STATE_DIR / $KEY_DIR to $RUN_USER"; _rc=1; }
  # if/then, NOT `[ -e ] && chmod || true`: that shape returns the test's
  # status and masks the chmod's, which is the trap this whole function was
  # rewritten to remove.
  if [ -e "$KEY_DIR/service.key" ]; then
    chmod 0600 "$KEY_DIR/service.key" || { warn "cannot chmod the keyfile"; _rc=1; }
  fi
  chmod 0700 "$KEY_DIR" || { warn "cannot chmod 0700 $KEY_DIR"; _rc=1; }

  # VERIFY WHAT THE DAEMON ACTUALLY NEEDS. The failure this exists to prevent
  # is the daemon taking "permission denied" on the instance lease, which is a
  # question about the OWNER of the store -- so that is what is asserted, on
  # the directories and on each artifact the ceremony leaves behind.
  for _p in "$STATE_DIR" "$KEY_DIR" "$STATE_DIR/meta.db" "$KEY_DIR/service.key"; do
    [ -e "$_p" ] || continue
    _u="$(stat -c '%U' "$_p" 2>/dev/null || echo '?')"
    [ "$_u" = "$RUN_USER" ] || {
      warn "$_p is owned by $_u, not $RUN_USER -- the daemon would fail to open it"
      _rc=1; }
  done

  if [ "$_rc" -eq 0 ]; then
    info "handed $STATE_DIR and $KEY_DIR to $RUN_USER"
  else
    # The old version printed this line unconditionally, so the output claimed
    # a handoff that had not happened.
    warn "the handoff did NOT complete; $STATE_DIR is not usable by $RUN_USER"
  fi
  return "$_rc"
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

if [ "$MODE" = "handoff" ]; then
  [ "$(id -u)" -eq 0 ] || die "--hand-off needs root (it chowns the store and the keyfile)"
  # THE UNIT IS THE SOURCE OF TRUTH FOR THE ACCOUNT, not --user.
  #
  # Handing the store to the wrong account produces exactly the failure this
  # mode exists to prevent -- the daemon gets "permission denied" opening
  # meta.db and crash-loops -- and --user can name a different account than the
  # unit runs as by more than one route: the playbook passing --service-user,
  # or the interview at install time being answered with a name of the
  # operator's own. So when the unit exists, READ User= out of it. The value
  # that decides who the daemon runs as is the value the handoff must use.
  if [ -r "$UNIT" ]; then
    _unit_user="$(sed -n 's/^User=[[:space:]]*//p' "$UNIT" | head -1)"
    if [ -n "$_unit_user" ] && [ "$_unit_user" != "$RUN_USER" ]; then
      warn "the unit runs as $_unit_user, not $RUN_USER -- handing the store to"
      warn "$_unit_user, because that is the account that has to open it"
      RUN_USER="$_unit_user"
    fi
  fi
  id "$RUN_USER" >/dev/null 2>&1 || die "no such service account: $RUN_USER"
  # BOTH helpers run before the verdict, so one failure does not hide the
  # other's diagnosis -- an operator repairing this wants every reason at once.
  _handoff_rc=0
  hand_off_state || _handoff_rc=1
  grant_tls_read || _handoff_rc=1
  [ "$_handoff_rc" -eq 0 ] || die "the handoff FAILED (see the warnings above). The service
       account cannot open its store, so DO NOT start the front door: it would
       crash-loop on \"permission denied\" and bury the cause under the restarts."
  say ""
  say "Handed off. The service may now open its own store."
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
      *)
        if dsn_is_local "$META_DSN"; then
          say "  This DSN is loopback or a unix socket, so allow_insecure_dsn is set for"
          say "  you and NAMED in the config: sslmode=verify-full cannot describe a"
          say "  channel that never leaves the host."
        else
          warn "the DSN does not request sslmode=verify-full; autodb will refuse to start unless allow_insecure_dsn is set, and this store holds your encrypted connection secrets"
        fi ;;
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
say "  the RPC layer, and autodb's own config calls TCP M9-gated pending TLS"
say "  and rate limits. Every local account could then reach it and attempt"
say "  logins. Choose it when developer self-service is worth that, and"
say "  keep the host's own accounts trusted."
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

if [ -e "$CONFIG" ] && [ "$KEEP_CONFIG" = "yes" ]; then
  warn "$CONFIG exists and --keep-config was given; leaving it alone."
else
  if [ -e "$CONFIG" ]; then
    # A RE-RUN MUST REWRITE IT. Leaving an existing config alone meant every
    # flag on a second --apply silently did nothing: --rpc-port, --port,
    # --dns-name and the sizing were all computed, printed in the summary, and
    # then discarded. An operator re-running to CHANGE something got the old
    # config and no indication why.
    cp -p "$CONFIG" "$CONFIG.bak" 2>/dev/null || true
    warn "$CONFIG exists; REPLACING it (previous kept as $CONFIG.bak)."
    warn "  pass --keep-config to preserve it instead."
  fi
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
  if [ -e "$CLIENT_CONFIG" ] && [ "$KEEP_CONFIG" = "yes" ]; then
    warn "$CLIENT_CONFIG exists and --keep-config was given; leaving it alone"
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
  # THE EXIT CODE DECIDES WHAT THIS FAILURE WAS ABOUT. autodb exits 78
  # (EX_CONFIG) when the failure is the CONFIGURATION rather than the command,
  # and every subcommand loads the config -- so --create-cert can fail for a
  # reason that has nothing to do with certificates. That is not hypothetical:
  # it stopped a real 1 vCPU provisioning run, and this script reported
  # "--create-cert failed", which sent the operator to look at TLS.
  #
  # Branching on the CODE, not on the message: a script that grepped the
  # daemon's prose would break the first time the wording improved.
  # `if ...; then` rather than a bare call plus $?: set -e is in force, and a
  # failing command outside a condition aborts the script -- which would take
  # the run down at exactly the failure this branch exists to explain.
  if "$PREFIX/autodb" --config "$CONFIG" --create-cert; then CERT_RC=0; else CERT_RC=$?; fi
  if [ "$CERT_RC" -eq 0 ]; then
    TLS_CERT="$CONFIG_DIR/tls/cert.pem"
    TLS_KEY="$CONFIG_DIR/tls/key.pem"
    TLS_CA="$CONFIG_DIR/tls/ca.pem"
    if [ -r "$TLS_CERT" ] && [ -r "$TLS_KEY" ]; then
      # Re-emit rather than sed the file: emit_config is the ONE place that
      # knows this config's shape, and patching it from outside is how the
      # generated file and the generator drift.
      emit_config > "$CONFIG.new" && mv "$CONFIG.new" "$CONFIG"
      chown root:"$RUN_USER" "$CONFIG"; chmod 0640 "$CONFIG"
      # THE WHOLE DIRECTORY, not just the key. --create-cert runs as root, so
      # everything it writes is root-owned and tls/ is 0700 -- and the daemon
      # runs as the service account, which then cannot read cert.pem either.
      # Fixing only the key left the next failure one line further on, which
      # is exactly how it played out on a real host.
      # A FAILURE HERE DISABLES THE FRONT DOOR rather than killing the run.
      #
      # The helpers now return non-zero, and this is the middle of --apply: the
      # config and the unit are already written, so aborting would leave a
      # half-configured box -- the exact outcome an earlier defect in this
      # script produced. Enabling a front door whose key the daemon cannot read
      # is worse than leaving it disabled, so the failure demotes the outcome
      # and says what to fix.
      if grant_tls_read; then
        info "front door ENABLED in $CONFIG"
        info "give clients $CONFIG_DIR/tls/ca.pem and sslmode=verify-full"
      else
        warn "the TLS material could not be made readable by $RUN_USER, so the"
        warn "front door is being left DISABLED: enabling it would only produce a"
        warn "daemon that cannot read its own key. Fix the warnings above, then:"
        warn "  $0 --hand-off --config $CONFIG --user $RUN_USER"
        TLS_CERT=""; TLS_KEY=""
      fi
    else
      warn "--create-cert reported success but $TLS_CERT is not readable;"
      warn "leaving the front door disabled."
      TLS_CERT=""; TLS_KEY=""
    fi
  else
    cert_failure_note "$CERT_RC"
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
      # THE CEREMONY RUNS AS ROOT, so meta.db, meta.db.lease-info and the
      # keyfile are root-owned when it finishes -- and the daemon runs as the
      # service account. Without this it crash-loops on "opening the meta
      # store to lease it: permission denied", which reads like a corrupt
      # store rather than an ownership mistake.
      # Same reasoning as the TLS handoff above: report it, do not abort a run
      # that has already written the config and the unit. The caller decides
      # whether to start the service, and this is the sentence it decides on.
      hand_off_state || {
        # NOT A WARNING. This clears startability, because the start below is in
        # this same script and a warning does not reach it.
        STATE_OK="no"
        warn "the store could not be handed to $RUN_USER, so the front door will"
        warn "NOT be started: it would crash-loop on \"permission denied\" opening"
        warn "its store. After fixing the warnings above:"
        warn "  $0 --hand-off --config $CONFIG --user $RUN_USER"
        warn "  systemctl enable --now autodb-frontdoor"
      }
    else
      # An unfinished ceremony means no administrator and no enrolled keyslot,
      # so starting would serve a store nothing can unlock.
      STATE_OK="no"
      warn "--init did not complete. The config and unit are in place; run"
      warn "  $PREFIX/autodb --config $CONFIG --init"
      warn "again before starting the service, or a restart will leave the"
      warn "store locked."
    fi
  fi
fi

say ""
if [ "$START_NOW" = "yes" ] && [ -n "$TLS_CERT" ] && [ "$STATE_OK" = "yes" ]; then
  say "starting autodb-frontdoor"
  systemctl enable --now autodb-frontdoor
  systemctl --no-pager --lines=0 status autodb-frontdoor || true
  STARTED="yes"
elif [ "$START_NOW" = "yes" ] && [ "$STATE_OK" != "yes" ]; then
  warn "not starting: the store is not usable by $RUN_USER (see the warnings"
  warn "above). Starting would only crash-loop on \"permission denied\" and bury"
  warn "the cause under systemd's restarts. Repair, then start it by hand."
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
# STARTED, not START_NOW: asking for a start that was then withheld must still
# leave the operator with the command to run.
if [ "$STARTED" != "yes" ]; then
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
