#!/bin/sh
# update_frontdoor.sh — move a running autodb front door to the latest release.
#
# It resolves the newest release TAG first, builds THAT, replaces the binary and
# restarts the unit. Nothing else: the config, the meta store, the TLS material
# and the keyslot are untouched, because an update is not a reinstall -- and the
# unit FILE is not rewritten either, so the operator's memory limits and service
# account survive. A release needing a changed unit needs install_frontdoor.sh.
#
#   sudo sh update_frontdoor.sh --check      # what it would do; changes nothing
#   sudo sh update_frontdoor.sh              # do it
#   sudo sh update_frontdoor.sh --ref v0.3.6 # a specific tag, e.g. to go back
#
# WHY IT ROLLS BACK. `systemctl restart` succeeds for a unit that starts and
# then exits immediately, so "the restart worked" is not evidence the daemon is
# running. If the new binary does not reach ActiveState=active, the previous one
# is put back and the service restarted on it — an update that leaves the front
# door down is worse than no update, and the operator finds out at 2am either
# way. The old binary is kept beside the new one for exactly that reason.
#
# Copy-pasteable: this script needs nothing from the repository but itself.
set -eu

say()  { printf '%s\n' "$*"; }
info() { printf '  %s\n' "$*"; }
step() { printf '\n=== %s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }


REPO="${AUTODB_REPO:-https://github.com/yongjohnlee80/autodb.git}"
PREFIX="${AUTODB_PREFIX:-/usr/local/bin}"
UNIT="autodb-frontdoor"
REF="latest"
MODE="apply"
ASSUME_YES="no"
FORCE="no"
KEEP_TMP="no"
GO_VERSION=""
# How many consecutive `active` samples count as "it stayed up".
#
# A CONSTANT, NOT A TUNABLE. The previous comment here said the count "is not
# meant to be tuned down" while reading it from
# ${AUTODB_ACTIVE_STABLE_SAMPLES:-3} -- an intention in prose that nothing
# enforced. With 0 injected, `[ "$_stable" -ge 0 ]` is true at the first sample,
# so a binary that goes active then failed is ACCEPTED, no rollback runs and the
# broken binary stays installed. Measured before this change, not deduced.
ACTIVE_STABLE_SAMPLES=3
# The GAP is adjustable -- it is not the property, and a cell sets it to 0.
# Validated, or a typo makes `sleep` fail mid-poll and takes the run with it.
ACTIVE_SAMPLE_SLEEP="${AUTODB_ACTIVE_SAMPLE_SLEEP:-1}"
# Validated HERE: after the assignment (set -u makes an earlier check an
# unbound-variable abort) and after die() exists (an earlier one exits 127 with
# no message). Both orders were wrong first; a cell caught each.
case "$ACTIVE_SAMPLE_SLEEP" in
  ''|*[!0-9]*) die "AUTODB_ACTIVE_SAMPLE_SLEEP must be a non-negative whole number of
       seconds (got '$ACTIVE_SAMPLE_SLEEP')" ;;
esac

usage() {
  cat <<'USAGE'
update_frontdoor.sh — build the latest autodb release and swap the binary in.

  sudo sh update_frontdoor.sh [options]

  --check              Report the installed and available versions, and what
                       would happen. Changes NOTHING.
  --ref <tag>          Build this ref instead of the newest release tag. Any
                       tag, branch or SHA; use it to roll back to a known one.
  --force              Rebuild and reinstall even when the installed version
                       already matches the target.
  --prefix <dir>       Where the binary lives (default /usr/local/bin).
  --unit <name>        systemd unit to restart (default autodb-frontdoor).
  --go <version>       Go toolchain to build with (default: the repo's go.mod).
  --yes                Do not prompt.
  --keep-tmp           Leave the build directory for inspection.
  -h, --help           This text.

It replaces the BINARY and RESTARTS the unit. It does NOT write the unit file:
your GOMEMLIMIT, MemoryMax, User= and Restart= settings survive an update
untouched -- and a release that needs a CHANGED unit will not get one from here.
install_frontdoor.sh owns that file. The config, the meta store, the TLS
material and the unattended-unlock keyslot are not read and not written.
USAGE
}

while [ $# -gt 0 ]; do
  case "$1" in
    --check) MODE="check" ;;
    --apply) MODE="apply" ;;
    --ref) REF="${2:?--ref needs a tag}"; shift ;;
    --ref=*) REF="${1#*=}" ;;
    --force) FORCE="yes" ;;
    --prefix) PREFIX="${2:?--prefix needs a directory}"; shift ;;
    --unit) UNIT="${2:?--unit needs a name}"; shift ;;
    --go) GO_VERSION="${2:?--go needs a version}"; shift ;;
    --yes|-y) ASSUME_YES="yes" ;;
    --keep-tmp) KEEP_TMP="yes" ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown option: $1 (try --help)" ;;
  esac
  shift
done

# wait_active reports whether $UNIT came up AND STAYED up.
#
# ONE `active` SAMPLE IS NOT A RUNNING DAEMON. The unit is Type=simple, so
# systemd marks it active the moment it forks the process -- before the binary
# has read its config, opened its store or bound anything. A daemon that starts
# and dies is therefore briefly active on its way to failed, and with
# Restart=on-failure it is active again a moment later. A review found both
# polls here accepting that first sample, in the update AND in the rollback --
# so a rollback could report the service restored while it was crash-looping.
#
# Active must hold across consecutive samples with an UNCHANGED MainPID: a
# restart is what a crash loop does and it changes the pid, so the pid is the
# evidence that the process active a second ago is the one active now.
#
# Sets ACTIVE_STATE, ACTIVE_PID and ACTIVE_RESTARTS for the caller's message.
wait_active() {
  _stable=0
  _seen_pid=""
  _tries=0
  ACTIVE_STATE="unknown"; ACTIVE_PID="0"; ACTIVE_RESTARTS="0"
  while [ "$_tries" -lt 30 ]; do
    _show="$(systemctl show -p ActiveState -p MainPID -p NRestarts --value "$UNIT" 2>/dev/null \
             || printf 'unknown\n0\n0\n')"
    ACTIVE_STATE="$(printf '%s\n' "$_show" | sed -n 1p)"
    ACTIVE_PID="$(printf '%s\n' "$_show" | sed -n 2p)"
    ACTIVE_RESTARTS="$(printf '%s\n' "$_show" | sed -n 3p)"
    case "$ACTIVE_STATE" in
      active)
        if [ -n "$_seen_pid" ] && [ "$ACTIVE_PID" != "$_seen_pid" ]; then
          _stable=0            # active, but not the same process
        else
          _stable=$(( _stable + 1 ))
        fi
        _seen_pid="$ACTIVE_PID"
        [ "$_stable" -ge "$ACTIVE_STABLE_SAMPLES" ] && return 0
        ;;
      activating|reloading) _stable=0 ;;
      *) return 1 ;;
    esac
    _tries=$(( _tries + 1 ))
    [ "$ACTIVE_SAMPLE_SLEEP" != "0" ] && sleep "$ACTIVE_SAMPLE_SLEEP"
  done
  return 1
}

need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required and not on PATH"; }
need git

# ---------------------------------------------------------------- the versions
#
# THE TAG IS RESOLVED BEFORE ANYTHING IS FETCHED, so the run reports what it is
# about to build and can compare it with what is installed.
#
# --refs is load-bearing: without it ls-remote also lists peeled entries
# (refs/tags/X^{}), which select only ANNOTATED tags and would silently change
# which set is being sorted. sort -V rather than sort, so v0.3.10 orders after
# v0.3.9 instead of before it.
step "Resolving the newest release"
if [ "$REF" = "latest" ]; then
  TAG="$(git ls-remote --tags --refs "$REPO" 2>/dev/null \
          | awk '{print $2}' | sed 's|refs/tags/||' \
          | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -1)" || TAG=""
  [ -n "$TAG" ] || die "could not resolve a release tag from $REPO"
else
  TAG="$REF"
fi
info "target      : $TAG"

INSTALLED="(none)"
if [ -x "$PREFIX/autodb" ]; then
  # `autodb --version` prints "autodb <ver> (<sha>, built <date>)"; the second
  # field is the version. A binary built without stamping says "dev", which is
  # never equal to a tag, so an unstamped install always updates.
  INSTALLED="$("$PREFIX/autodb" --version 2>/dev/null | awk '{print $2}')"
  [ -n "$INSTALLED" ] || INSTALLED="(unreadable)"
fi
info "installed   : $INSTALLED"
info "binary      : $PREFIX/autodb"
info "unit        : $UNIT"

if [ "$INSTALLED" = "$TAG" ] && [ "$FORCE" != "yes" ]; then
  say ""
  say "Already on $TAG. Nothing to do (--force rebuilds anyway)."
  exit 0
fi

if [ "$MODE" = "check" ]; then
  say ""
  say "Would build $TAG and replace $PREFIX/autodb, then restart $UNIT."
  say "The config, meta store, TLS material and keyslot are not touched."
  say "Re-run without --check to do it."
  exit 0
fi

# WHAT IT NEEDS, not who it is. Requiring uid 0 was a proxy for "can write
# $PREFIX and manage the unit"; checking the capabilities directly is both
# truer and testable -- a cell can point --prefix at a temp directory with a
# stub systemctl and exercise the whole swap, which is how the rollback path
# gets evidence at all.
[ -w "$PREFIX" ] || die "$PREFIX is not writable by $(id -un); this replaces the binary
       there, so run it with the privilege that allows that (normally sudo)"
need systemctl
[ -x "$PREFIX/autodb" ] || die "no autodb at $PREFIX/autodb — this updates an existing
       install; use provision_vm.sh or install_frontdoor.sh for a new one"

if [ "$ASSUME_YES" != "yes" ] && [ -r /dev/tty ]; then
  say ""
  printf 'Update %s from %s to %s? [yes/no]: ' "$UNIT" "$INSTALLED" "$TAG" > /dev/tty
  IFS= read -r _a < /dev/tty || _a="no"
  case "$_a" in y|Y|yes|YES|Yes) ;; *) die "aborted; nothing was changed" ;; esac
fi

TMP="$(mktemp -d "${TMPDIR:-/tmp}/autodb-update.XXXXXX")"
cleanup() {
  if [ "$KEEP_TMP" = "yes" ]; then
    warn "build directory KEPT at $TMP"
  else
    rm -rf "$TMP"
  fi
}
trap cleanup EXIT

# ------------------------------------------------------------------ the source
step "Fetching $TAG"
# A shallow single-tag fetch rather than a full clone: the history is not wanted
# and a small VM's disk is.
# A SHALLOW SINGLE-TAG FETCH FIRST, then a full clone as the fallback -- and
# the fallback's CHECKOUT MUST SUCCEED.
#
# It used to end in `|| true`, so a ref that does not exist left the default
# branch checked out and the script happily built and installed THAT. An
# operator typing a wrong tag would have been handed main, told it was their
# tag, and had it stamped with the version they asked for.
if ! git -c advice.detachedHead=false clone --quiet --depth 1 --branch "$TAG" \
        "$REPO" "$TMP/src" 2>/dev/null; then
  git -c advice.detachedHead=false clone --quiet "$REPO" "$TMP/src" \
    || die "cannot clone $REPO"
  git -C "$TMP/src" -c advice.detachedHead=false checkout --quiet "$TAG" \
    || die "no such ref in $REPO: $TAG"
fi
SRC="$TMP/src"

# AND VERIFY WHAT IS ACTUALLY CHECKED OUT. The clone succeeding is not the same
# as HEAD being the requested ref: --branch accepts a name, a fallback can be
# left anywhere, and this is the last point before a binary is built from it.
_head="$(git -C "$SRC" rev-parse --verify HEAD 2>/dev/null)" \
  || die "the clone has no HEAD"
_want="$(git -C "$SRC" rev-parse --verify "${TAG}^{commit}" 2>/dev/null)" \
  || die "no such ref in $REPO: $TAG"
[ "$_head" = "$_want" ] || die "the checkout is at $_head but $TAG is $_want; refusing to
       build source that is not the ref you asked for"
info "at $(git -C "$SRC" rev-parse --short HEAD) ($TAG)"

# ------------------------------------------------------------------- the build
step "Building"
GOBIN=""
if command -v mise >/dev/null 2>&1; then
  GOBIN="mise exec --"
  if [ -z "$GO_VERSION" ] && [ -r "$SRC/go.mod" ]; then
    # The MINOR line the source asks for, so a patch release of the toolchain
    # is allowed and a mismatch is not invented.
    GO_VERSION="$(awk '/^go /{print $2}' "$SRC/go.mod" | head -1)"
  fi
  # NOTHING GLOBAL. `mise use -g` writes the operator's global config, which
  # this script has no business touching: its contract is the binary and the
  # unit, and a review caught it changing the default Go for every project on
  # the host as a side effect of an autodb update. `mise exec go@X --` pins the
  # toolchain for THIS build and leaves no trace.
  if [ -n "$GO_VERSION" ]; then
    GOBIN="mise exec go@$GO_VERSION --"
  fi
elif command -v go >/dev/null 2>&1; then
  GOBIN=""
else
  die "no Go toolchain: install mise or go, then re-run"
fi

# STAMPED, or the new binary reports "autodb dev" and the next update cannot
# tell what is installed -- which is the comparison this script opens with.
_ver="$(git -C "$SRC" describe --tags --always 2>/dev/null || echo "$TAG")"
_sha="$(git -C "$SRC" rev-parse --short HEAD 2>/dev/null || echo unknown)"
_now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
info "stamping version=$_ver commit=$_sha"
# Serialized deliberately: one compile of modernc.org/sqlite peaks near 700 MiB,
# and parallel compiles on a small VM multiply that into an OOM kill rather than
# finishing sooner.
( cd "$SRC" && GOMAXPROCS=1 CGO_ENABLED=0 $GOBIN go build -p 1 \
    -ldflags "-X main.version=$_ver -X main.commit=$_sha -X main.buildDate=$_now" \
    -o "$TMP/autodb" ./cmd/autodb )
[ -x "$TMP/autodb" ] || die "the build produced no binary"
info "built $(du -m "$TMP/autodb" | awk '{print $1}') MiB"
# Prove the artefact runs BEFORE the service is stopped. A binary that cannot
# even print its version is one nobody should take a daemon down for.
info "reports: $("$TMP/autodb" --version)"

# ------------------------------------------------------------------- the swap
step "Swapping the binary"
BACKUP="$PREFIX/autodb.previous"
cp -p "$PREFIX/autodb" "$BACKUP"
info "kept the previous binary at $BACKUP"

WAS_ACTIVE="no"
[ "$(systemctl show -p ActiveState --value "$UNIT" 2>/dev/null)" = "active" ] && WAS_ACTIVE="yes"

systemctl stop "$UNIT" 2>/dev/null || true
# install(1) rather than cp: it replaces the file rather than writing through an
# open one, and sets the mode in the same step.
install -m 0755 "$TMP/autodb" "$PREFIX/autodb"
info "installed $("$PREFIX/autodb" --version)"

# ------------------------------------------------------------------ the restart
#
# RESTART SUCCESS IS NOT RUNNING. `systemctl restart` returns 0 for a unit that
# starts and exits at once, so ActiveState is polled -- and a unit that
# legitimately reads "activating" for a moment is not charged as a failure.
step "Restarting $UNIT"
ROLLED_BACK="no"
if [ "$WAS_ACTIVE" = "no" ]; then
  info "the unit was not running before this update; leaving it stopped"
  info "start it with: systemctl start $UNIT"
else
  systemctl start "$UNIT" || true
  if wait_active; then
    info "service is ACTIVE and stayed up on $_ver (pid $ACTIVE_PID)"
  else
    # ROLL BACK. The daemon was running before this script touched it, and it
    # is not running now, so the change is undone rather than reported.
    warn "the new binary did not stay active (ActiveState=$ACTIVE_STATE, MainPID=$ACTIVE_PID, NRestarts=$ACTIVE_RESTARTS)"
    warn "rolling back to the previous binary"
    install -m 0755 "$BACKUP" "$PREFIX/autodb"
    systemctl start "$UNIT" || true
    ROLLED_BACK="yes"
    if wait_active; then
      warn "restored $INSTALLED and the service is active again (pid $ACTIVE_PID)"
    else
      warn "THE SERVICE IS DOWN and the rollback did not bring it up either."
      warn "  systemctl status $UNIT"
      warn "  journalctl -u $UNIT -b --no-pager"
    fi
  fi
fi

step "Result"
info "binary   : $("$PREFIX/autodb" --version 2>/dev/null || echo unknown)"
info "service  : $(systemctl show -p ActiveState --value "$UNIT" 2>/dev/null || echo unknown)"
info "previous : $BACKUP"
say ""
if [ "$ROLLED_BACK" = "yes" ]; then
  die "the update was ROLLED BACK: $TAG did not come up. See the journal above."
fi
say "Updated to $_ver. The config, meta store, TLS material and keyslot were"
say "not touched. The previous binary is at $BACKUP if you want it back:"
say "  install -m 0755 $BACKUP $PREFIX/autodb && systemctl restart $UNIT"
