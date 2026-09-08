#!/usr/bin/env sh
#
# autodb VM provisioning playbook.
#
# Takes a FRESH Linux VM and brings it to a running autodb front door:
# swap, base packages, mise + a pinned Go toolchain, autodb built and
# installed, then install_frontdoor.sh for the service itself.
#
#   ./provision_vm.sh --user root --host 203.0.113.10        # probe only
#   ./provision_vm.sh --user root --host vm.example.com --apply
#   ./provision_vm.sh root@203.0.113.10 --apply --meta pg-local
#
# --check (the default) changes NOTHING on the VM. It connects, measures,
# prints the plan and the sizing the front door would get, and exits.
#
# WHY THIS ADDS SWAP, measured rather than assumed: compiling autodb peaks
# at ~703 MiB RSS in a SINGLE compile process even fully serialized
# (GOMAXPROCS=1 -p 1; modernc.org/sqlite is the heavy one). A 1 GB VPS with
# no swap -- the DigitalOcean default -- will have the Go compiler
# OOM-killed. Disk is the cheap resource on these boxes, so the playbook
# trades some of it for a build that finishes, and the swap keeps helping
# afterwards because autodb's own memory budgets are accounting rather than
# allocations and nothing in it reads a physical-memory figure.
#
# POSIX sh on the control side. Remote work is uploaded as a script rather
# than inlined through ssh, so nothing depends on nested quoting surviving
# two shells.
#
# STATUS: the remote --apply path is being validated on a disposable VM.
# Until that is recorded, treat --apply as unproven.

set -eu

SSH_USER=""
SSH_HOST=""
SSH_PORT=22
SSH_KEY=""
MODE="check"
GO_VERSION=""
AUTODB_REF="main"
AUTODB_REPO="https://github.com/yongjohnlee80/autodb.git"
SWAP_MIB="auto"
BUILD="vm"           # vm | prebuilt
META="sqlite"
META_DSN=""
BIND="0.0.0.0:5432"
DNS_NAME=""
FD_PORT=""
PREFIX="/usr/local/bin"
ASSUME_YES="no"

# Mirrors install_frontdoor.sh, which mirrors the code.
WATERMARK_MIB=4

HERE="$(cd "$(dirname "$0")" && pwd)"
FD_SCRIPT="$HERE/install_frontdoor.sh"

say()  { printf '%s\n' "$*"; }
info() { printf '  %s\n' "$*"; }
step() { printf '\n=== %s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<'USAGE'
autodb VM provisioning playbook

USAGE:
  provision_vm.sh --user <name> --host <ip|dns> [OPTIONS]
  provision_vm.sh <user>@<host> [OPTIONS]

REQUIRED:
  --user <name>        SSH login user on the VM. Privileged steps use sudo
                       when this is not root.
  --host <ip|dns>      VM address: an IP or a DNS name.

OPTIONS:
  --check              Connect, measure, print the plan. Changes NOTHING.
                       This is the default.
  --apply              Actually provision.
  --port <n>           SSH port. Default: 22
  --key <path>         SSH identity file.
  --go-version <ver>   Go toolchain for mise to install. Default: read from
                       the adjacent autodb checkout's go.mod, else 1.25.3.
  --ref <git-ref>      autodb ref to build. Default: main
  --swap <MiB>|none    Swapfile size. Default: auto -- 2048 MiB when the VM
                       has under 2 GB of RAM, otherwise none.
  --prebuilt           Cross-compile locally and upload the binary instead
                       of building on the VM. Use when the VM is too small
                       to compile even with swap.
  --meta <backend>     sqlite (default) | pg-local | pg-remote
  --meta-dsn <dsn>     DSN, required with --meta pg-remote.
  --bind <addr>        Front-door bind address. Default: 0.0.0.0:5432
  --dns-name <name>    DNS name for the TLS certificate. Passed through to
                       install_frontdoor.sh; omitted means the certificate is
                       issued for an IP address instead.
  --port <n>           Front-door port. Passed through.
  --prefix <dir>       Where to install the binary. Default: /usr/local/bin
  --yes                Do not prompt before provisioning.
  -h, --help           Show this help.

The playbook CALLS install_frontdoor.sh for the service rather than
duplicating it; that script owns config generation, memory sizing and the
systemd unit. This one owns the machine underneath it.
USAGE
}

while [ $# -gt 0 ]; do
  case "$1" in
    --user) SSH_USER="${2:?--user needs a name}"; shift ;;
    --host) SSH_HOST="${2:?--host needs an ip or dns name}"; shift ;;
    --port) SSH_PORT="${2:?--port needs a number}"; shift ;;
    --key)  SSH_KEY="${2:?--key needs a path}"; shift ;;
    --check) MODE="check" ;;
    --apply) MODE="apply" ;;
    --go-version) GO_VERSION="${2:?--go-version needs a version}"; shift ;;
    --ref) AUTODB_REF="${2:?--ref needs a git ref}"; shift ;;
    --swap) SWAP_MIB="${2:?--swap needs MiB or none}"; shift ;;
    --prebuilt) BUILD="prebuilt" ;;
    --meta) META="${2:?--meta needs a backend}"; shift ;;
    --meta-dsn) META_DSN="${2:?--meta-dsn needs a DSN}"; shift ;;
    --bind) BIND="${2:?--bind needs an address}"; shift ;;
    --dns-name) DNS_NAME="${2:?--dns-name needs a name}"; shift ;;
    --port) FD_PORT="${2:?--port needs a number}"; shift ;;
    --prefix) PREFIX="${2:?--prefix needs a directory}"; shift ;;
    --yes) ASSUME_YES="yes" ;;
    -h|--help) usage; exit 0 ;;
    *@*)
      # user@host shorthand, so a copy-pasted ssh target just works.
      SSH_USER="${1%@*}"; SSH_HOST="${1#*@}"
      ;;
    *) die "unknown option: $1 (try --help)" ;;
  esac
  shift
done

[ -n "$SSH_USER" ] || die "--user is required (the SSH login on the VM)"
[ -n "$SSH_HOST" ] || die "--host is required (the VM's IP address or DNS name)"
case "$META" in sqlite|pg-local|pg-remote) ;; *) die "--meta must be sqlite, pg-local or pg-remote" ;; esac
[ "$META" = "pg-remote" ] && [ -z "$META_DSN" ] && die "--meta pg-remote requires --meta-dsn"
[ -r "$FD_SCRIPT" ] || die "install_frontdoor.sh not found beside this script at $FD_SCRIPT"

# Default the toolchain from the checkout beside us -- but take the MINOR
# LINE, not the exact figure.
#
# go.mod's `go 1.25.3` is a MINIMUM LANGUAGE VERSION, not a toolchain pin
# (there is no `toolchain` directive), so installing exactly 1.25.3 builds
# with the OLDEST ACCEPTABLE compiler rather than a current one. That is not
# a cosmetic difference: as of 2026-09-08 the Go 1.25.3 standard library
# carried 30 advisories affecting packages autodb links, including five in
# crypto/tls and five in crypto/x509 -- on a front door that terminates TLS
# and requires sslmode=verify-full for its own meta store. 1.25.13 clears
# every one of them, and they are all patch releases within the same minor
# line, so nothing about the language version changes.
#
# Asking for "1.25" gets the newest patch in that line. Override with
# --go-version when you need an exact build.
if [ -z "$GO_VERSION" ]; then
  for _m in "$HERE/main/go.mod" "$HERE/go.mod"; do
    if [ -r "$_m" ]; then
      # 1.25.3 -> 1.25 ; a bare "1.25" stays "1.25"
      GO_VERSION="$(awk '/^go [0-9]/{split($2,v,"."); print v[1]"."v[2]; exit}' "$_m")"
      break
    fi
  done
  [ -n "$GO_VERSION" ] || GO_VERSION="1.25"
fi

SSH_OPTS="-o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new -p $SSH_PORT"
[ -n "$SSH_KEY" ] && SSH_OPTS="$SSH_OPTS -i $SSH_KEY"
TARGET="$SSH_USER@$SSH_HOST"

rsh()  { ssh $SSH_OPTS "$TARGET" "$@"; }
rcp()  { scp -q $( [ -n "$SSH_KEY" ] && printf '%s' "-i $SSH_KEY" ) -P "$SSH_PORT" \
           -o BatchMode=yes -o StrictHostKeyChecking=accept-new "$1" "$TARGET:$2"; }

# ------------------------------------------------------------------- probe

step "Probing $TARGET"
PROBE="$(rsh 'set -eu
  . /etc/os-release 2>/dev/null || true
  printf "os=%s\n" "${ID:-unknown}"
  printf "osver=%s\n" "${VERSION_ID:-unknown}"
  printf "pretty=%s\n" "${PRETTY_NAME:-unknown}"
  printf "cpu=%s\n" "$(nproc 2>/dev/null || echo 1)"
  printf "mem=%s\n" "$(awk "/^MemTotal:/{print int(\$2/1024)}" /proc/meminfo)"
  printf "swap=%s\n" "$(awk "/^SwapTotal:/{print int(\$2/1024)}" /proc/meminfo)"
  printf "diskfree=%s\n" "$(df -Pm / | awk "NR==2{print \$4}")"
  printf "arch=%s\n" "$(uname -m)"
  printf "root=%s\n" "$(id -u)"
  for c in apt-get dnf yum pacman apk zypper; do
    command -v "$c" >/dev/null 2>&1 && { printf "pkg=%s\n" "$c"; break; }
  done
  command -v sudo >/dev/null 2>&1 && printf "sudo=yes\n" || printf "sudo=no\n"
  command -v systemctl >/dev/null 2>&1 && printf "systemd=yes\n" || printf "systemd=no\n"
  command -v mise >/dev/null 2>&1 && printf "mise=%s\n" "$(mise --version 2>/dev/null | head -1)" || printf "mise=absent\n"
  command -v autodb >/dev/null 2>&1 && printf "autodb=%s\n" "$(command -v autodb)" || printf "autodb=absent\n"
' 2>/dev/null)" || die "cannot reach $TARGET over ssh (port $SSH_PORT). Check the address, the login and your key."

get() { printf '%s\n' "$PROBE" | sed -n "s/^$1=//p" | head -1; }
R_OS="$(get os)"; R_PRETTY="$(get pretty)"; R_CPU="$(get cpu)"; R_MEM="$(get mem)"
R_SWAP="$(get swap)"; R_DISK="$(get diskfree)"; R_ARCH="$(get arch)"; R_UID="$(get root)"
R_PKG="$(get pkg)"; R_SUDO="$(get sudo)"; R_SYSTEMD="$(get systemd)"
R_MISE="$(get mise)"; R_AUTODB="$(get autodb)"

[ -n "$R_MEM" ] || die "could not read MemTotal on the VM"
[ -n "$R_PKG" ] || die "no supported package manager found on the VM (looked for apt-get, dnf, yum, pacman, apk, zypper)"
[ "$R_SYSTEMD" = "yes" ] || die "no systemctl on the VM; install_frontdoor.sh installs a systemd unit"
if [ "$R_UID" != "0" ] && [ "$R_SUDO" != "yes" ]; then
  die "$SSH_USER is not root and sudo is absent; privileged steps would fail"
fi
SUDO=""
[ "$R_UID" != "0" ] && SUDO="sudo"

info "host        : $R_PRETTY ($R_OS, $R_ARCH)"
info "login       : $SSH_USER (uid $R_UID)$( [ -n "$SUDO" ] && printf ', via sudo' )"
info "cpu / ram   : $R_CPU vCPU / $R_MEM MiB"
info "swap        : ${R_SWAP} MiB"
info "disk free   : ${R_DISK} MiB on /"
info "pkg manager : $R_PKG"
info "mise        : $R_MISE"
info "autodb      : $R_AUTODB"

# ---------------------------------------------------------------- the plan

# Swap decision. The measured compile peak is the reason, so it is stated.
BUILD_PEAK_MIB=703
if [ "$SWAP_MIB" = "auto" ]; then
  if [ "$R_MEM" -lt 2048 ]; then SWAP_MIB=2048; else SWAP_MIB=0; fi
elif [ "$SWAP_MIB" = "none" ]; then
  SWAP_MIB=0
fi
# SLACK, because mkswap spends a page on its header: a 2048 MiB swapfile
# reports 2047 MiB in SwapTotal, so an exact comparison declares the work
# outstanding forever and a re-run always claims it will create swap it
# already created.
SWAP_SLACK_MIB=16
SWAP_NEEDED=0
if [ "$SWAP_MIB" -gt 0 ] && [ "$R_SWAP" -lt $(( SWAP_MIB - SWAP_SLACK_MIB )) ]; then
  SWAP_NEEDED=1
fi

step "Plan"
if [ "$BUILD" = "vm" ]; then
  info "build       : ON THE VM, with mise-provided Go $GO_VERSION (newest patch in that line)"
  if [ "$R_MEM" -lt 1536 ] && [ "$SWAP_NEEDED" = "0" ] && [ "$R_SWAP" -lt 1024 ]; then
    warn "compiling autodb peaks near ${BUILD_PEAK_MIB} MiB in one process and this VM has"
    warn "${R_MEM} MiB with ${R_SWAP} MiB swap. The Go compiler will likely be OOM-killed."
    warn "Either allow swap (drop --swap none) or use --prebuilt."
  fi
else
  info "build       : LOCALLY (cross-compiled linux/amd64), binary uploaded"
fi
[ "$SWAP_NEEDED" = "1" ] && info "swap        : create ${SWAP_MIB} MiB swapfile (have ${R_SWAP} MiB)" \
                         || info "swap        : unchanged (${R_SWAP} MiB)"
info "autodb ref  : $AUTODB_REF"
info "install to  : $PREFIX/autodb"
info "meta store  : $META"
info "front door  : $BIND"

step "Front-door sizing this VM would get"
# --assume-cpus as well as --assume-ram: without it the sizing report's CPU
# warnings would describe THIS workstation, and the single-core argon2
# warning -- the one that matters for a 1 vCPU droplet -- would silently
# never fire for the host being provisioned.
FD_ARGS="--check --assume-ram $R_MEM --assume-cpus $R_CPU --meta $META"
[ -n "$META_DSN" ] && FD_ARGS="$FD_ARGS --meta-dsn $META_DSN"
# Run the real script locally against the VM's measured RAM, so the numbers
# printed here are the ones it will choose on the box.
sh "$FD_SCRIPT" $FD_ARGS 2>&1 | sed -n '/preflight/,$p' | sed 's/^/  /' || \
  die "install_frontdoor.sh refused this host's size; see above"

if [ "$MODE" = "check" ]; then
  say ""
  say "Probe only -- nothing on the VM was changed. Re-run with --apply to provision."
  exit 0
fi

# ------------------------------------------------------------------- apply

if [ "$ASSUME_YES" != "yes" ] && [ -r /dev/tty ]; then
  say ""
  printf 'Provision %s as described above? [yes/no]: ' "$TARGET" > /dev/tty
  IFS= read -r _a < /dev/tty || _a="no"
  case "$_a" in y|Y|yes|YES|Yes) ;; *) die "aborted; nothing was changed" ;; esac
fi

REMOTE_TMP="/tmp/autodb-provision.$$"
rsh "mkdir -p $REMOTE_TMP"

# The remote half is uploaded as a script. Its parameters arrive as
# arguments, so nothing depends on quoting surviving both shells.
cat > "$REMOTE_TMP.sh" <<'REMOTE'
#!/usr/bin/env sh
# Remote half of the autodb provisioning playbook. Idempotent throughout:
# every step checks for its own result before doing anything.
set -eu

SUDO="$1"; PKG="$2"; SWAP_MIB="$3"; GO_VERSION="$4"
REPO="$5"; REF="$6"; PREFIX="$7"; BUILD="$8"; TMP="$9"

say()  { printf '  %s\n' "$*"; }
step() { printf '\n--- %s\n' "$*"; }

step "base packages"
case "$PKG" in
  apt-get)
    if ! dpkg -s git curl ca-certificates >/dev/null 2>&1; then
      $SUDO env DEBIAN_FRONTEND=noninteractive apt-get update -qq
      $SUDO env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq git curl ca-certificates xz-utils
    fi ;;
  dnf|yum)  $SUDO "$PKG" install -y -q git curl ca-certificates xz ;;
  pacman)   $SUDO pacman -Sy --noconfirm --needed git curl ca-certificates xz ;;
  apk)      $SUDO apk add --no-progress git curl ca-certificates xz ;;
  zypper)   $SUDO zypper --non-interactive install git curl ca-certificates xz ;;
esac
say "git $(git --version | awk '{print $3}'), curl present"

step "swap"
if [ "$SWAP_MIB" -gt 0 ]; then
  # Same 16 MiB slack as the control side: mkswap's header means a
  # correctly-sized swapfile never reports its full nominal size.
  if [ "$(awk '/^SwapTotal:/{print int($2/1024)}' /proc/meminfo)" -ge $(( SWAP_MIB - 16 )) ]; then
    say "already have $(awk '/^SwapTotal:/{print int($2/1024)}' /proc/meminfo) MiB; leaving it"
  else
    # /swapfile is the conventional location and the one fstab entries expect.
    if [ ! -e /swapfile ]; then
      $SUDO fallocate -l "${SWAP_MIB}M" /swapfile 2>/dev/null || \
        $SUDO dd if=/dev/zero of=/swapfile bs=1M count="$SWAP_MIB" status=none
      $SUDO chmod 600 /swapfile
      $SUDO mkswap /swapfile >/dev/null
    fi
    $SUDO swapon /swapfile 2>/dev/null || true
    # Persist it, once. A duplicated fstab line makes boots noisy.
    if ! grep -q '^/swapfile ' /etc/fstab 2>/dev/null; then
      printf '/swapfile none swap sw 0 0\n' | $SUDO tee -a /etc/fstab >/dev/null
    fi
    say "swap now $(awk '/^SwapTotal:/{print int($2/1024)}' /proc/meminfo) MiB"
  fi
else
  say "not requested"
fi

if [ "$BUILD" = "vm" ]; then
  step "mise + Go $GO_VERSION"
  if ! command -v mise >/dev/null 2>&1 && [ ! -x "$HOME/.local/bin/mise" ]; then
    curl -fsSL https://mise.run | sh
  fi
  MISE="$(command -v mise 2>/dev/null || printf '%s' "$HOME/.local/bin/mise")"
  [ -x "$MISE" ] || { echo "mise did not install" >&2; exit 1; }
  say "mise $("$MISE" --version 2>/dev/null | head -1)"
  # Resolve a partial spec ("1.25") to the newest matching patch, and say
  # which one it became -- an installer that reports "go@1.25" tells you
  # nothing about which compiler built the binary you are about to run.
  GO_RESOLVED="$("$MISE" latest "go@$GO_VERSION" 2>/dev/null || true)"
  [ -n "$GO_RESOLVED" ] || GO_RESOLVED="$GO_VERSION"
  "$MISE" use -g "go@$GO_RESOLVED" >/dev/null
  say "requested go@$GO_VERSION -> installed $("$MISE" exec -- go version 2>/dev/null | awk '{print $3}')"

  step "build autodb ($REF)"
  SRC="$TMP/src"
  if [ -d "$SRC/.git" ]; then
    git -C "$SRC" fetch --depth 1 origin "$REF" -q
    git -C "$SRC" checkout -q FETCH_HEAD
  else
    git clone --depth 1 --branch "$REF" -q "$REPO" "$SRC" 2>/dev/null || \
      { git clone --depth 1 -q "$REPO" "$SRC"; git -C "$SRC" fetch --depth 1 origin "$REF" -q; git -C "$SRC" checkout -q FETCH_HEAD; }
  fi
  say "at $(git -C "$SRC" rev-parse --short HEAD)"
  # Serialized deliberately: one compile of modernc.org/sqlite peaks near
  # 700 MiB, and parallel compiles on a small VM multiply that into an
  # OOM kill rather than finishing sooner.
  ( cd "$SRC" && GOMAXPROCS=1 "$MISE" exec -- go build -p 1 -o "$TMP/autodb" ./cmd/autodb )
  say "built $(du -m "$TMP/autodb" | awk '{print $1}') MiB"
fi

step "install binary"
$SUDO install -m 0755 "$TMP/autodb" "$PREFIX/autodb"
say "$("$PREFIX/autodb" --version 2>/dev/null || echo "$PREFIX/autodb installed")"
REMOTE

step "Uploading remote steps"
rcp "$REMOTE_TMP.sh" "$REMOTE_TMP/remote.sh"
rm -f "$REMOTE_TMP.sh"
rcp "$FD_SCRIPT" "$REMOTE_TMP/install_frontdoor.sh"
rsh "chmod +x $REMOTE_TMP/remote.sh $REMOTE_TMP/install_frontdoor.sh"
info "uploaded remote.sh and install_frontdoor.sh to $REMOTE_TMP"

if [ "$BUILD" = "prebuilt" ]; then
  step "Cross-compiling locally for linux/amd64"
  _src=""
  for _c in "$HERE/main" "$HERE"; do [ -r "$_c/go.mod" ] && { _src="$_c"; break; }; done
  [ -n "$_src" ] || die "--prebuilt needs an autodb checkout beside this script"
  ( cd "$_src" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "/tmp/autodb-linux-amd64" ./cmd/autodb )
  info "built $(du -m /tmp/autodb-linux-amd64 | awk '{print $1}') MiB from $_src"
  rcp /tmp/autodb-linux-amd64 "$REMOTE_TMP/autodb"
  rsh "chmod +x $REMOTE_TMP/autodb"
  info "uploaded"
fi

step "Running remote provisioning"
rsh "$REMOTE_TMP/remote.sh '$SUDO' '$R_PKG' '$SWAP_MIB' '$GO_VERSION' '$AUTODB_REPO' '$AUTODB_REF' '$PREFIX' '$BUILD' '$REMOTE_TMP'"

step "Configuring the front door"
FD_APPLY="--apply --non-interactive --bind $BIND --prefix $PREFIX --meta $META"
[ -n "$META_DSN" ] && FD_APPLY="$FD_APPLY --meta-dsn $META_DSN"
[ -n "$DNS_NAME" ] && FD_APPLY="$FD_APPLY --dns-name $DNS_NAME"
[ -n "$FD_PORT" ]  && FD_APPLY="$FD_APPLY --port $FD_PORT"
# --init prompts for a passphrase on a terminal, and the remote side of this
# playbook has none. So the ceremony is left for the operator rather than
# half-run: install_frontdoor.sh reports the exact command at the end.
FD_APPLY="$FD_APPLY --no-init"
rsh "$SUDO $REMOTE_TMP/install_frontdoor.sh $FD_APPLY"

step "Result"
rsh "set -eu
  printf '  binary   : %s\n' \"\$($PREFIX/autodb --version 2>/dev/null || echo present)\"
  printf '  config   : %s\n' \"\$( [ -r /etc/autodb/config.toml ] && echo /etc/autodb/config.toml || echo MISSING )\"
  printf '  unit     : %s\n' \"\$( [ -r /etc/systemd/system/autodb-frontdoor.service ] && echo installed || echo MISSING )\"
  printf '  swap     : %s MiB\n' \"\$(awk '/^SwapTotal:/{print int(\$2/1024)}' /proc/meminfo)\"
  printf '  service  : %s\n' \"\$(systemctl is-enabled autodb-frontdoor 2>/dev/null || echo not-enabled)\"
"

say ""
say "Provisioned. The front door is NOT serving yet -- install_frontdoor.sh"
say "writes it disabled until TLS material exists, because enabled without"
say "TLS is refused at config load. Its closing notes list what remains."
say ""
info "leftover working directory on the VM: $REMOTE_TMP"
