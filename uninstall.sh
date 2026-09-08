#!/usr/bin/env sh
#
# autodb uninstaller.
#
# Removes what install_frontdoor.sh created -- the service, its config, its
# state and its service account -- and optionally what provision_vm.sh added
# underneath it.
#
#   ./uninstall.sh --check              # list what would go; changes nothing
#   sudo ./uninstall.sh --apply
#   sudo ./uninstall.sh --apply --remove-swap --remove-toolchain
#
# WHAT IS UNRECOVERABLE, said before anything else. The meta store holds every
# user, every grant, the audit log, and the ENCRYPTED CONNECTION SECRETS.
# Deleting it destroys those secrets permanently: the ciphertext is useless
# without the master key, and the master key lives only in that store's keyslot
# envelope. There is no elsewhere to recover from. So a backup is taken by
# DEFAULT and skipping it takes a flag.
#
# THE BACKUP DOES NOT INCLUDE THE KEYFILE, deliberately. An archive holding
# both the meta store and the service keyfile is the two halves of one
# envelope in a single file -- exactly the "one careless tar" hazard the
# config documentation warns about, and it would turn a backup into a
# credential. The store is archived; the keyfile is left where it is and
# reported. Without it the archive still needs a passphrase to open, which is
# the property worth keeping.
#
# POSIX sh. Read it before running it as root.

set -eu

MODE="check"
CONFIG="/etc/autodb/config.toml"
CONFIG_DIR="/etc/autodb"
UNIT="/etc/systemd/system/autodb-frontdoor.service"
PREFIX="/usr/local/bin"
RUN_USER="autodb"
STATE_DIR="/var/lib/autodb"
KEY_DIR="/var/lib/autodb-keys"
BACKUP_DIR=""
DO_BACKUP="yes"
RM_SWAP="no"
RM_TOOLCHAIN="no"
RM_USER="yes"
ASSUME_YES="no"

say()  { printf '%s\n' "$*"; }
info() { printf '  %s\n' "$*"; }
step() { printf '\n=== %s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<'USAGE'
autodb uninstaller

USAGE:
  uninstall.sh [OPTIONS]

OPTIONS:
  --check              List what would be removed. Changes NOTHING.
                       This is the default.
  --apply              Actually remove it.
  --backup-dir <dir>   Where to write the archive. Default: /var/backups
  --no-backup          Do not archive the meta store first. The encrypted
                       connection secrets are then gone for good.
  --remove-swap        Also remove /swapfile and its fstab entry
                       (provision_vm.sh created these).
  --remove-toolchain   Also remove mise and its managed Go toolchains.
  --keep-user          Leave the service account in place.
  --config <path>      Config to read paths from. Default: /etc/autodb/config.toml
  --prefix <dir>       Where the binary lives. Default: /usr/local/bin
  --yes                Do not prompt.
  -h, --help           Show this help.

The archive NEVER contains the service keyfile. Both halves of the envelope
in one file would make the backup a credential; see the header.
USAGE
}

while [ $# -gt 0 ]; do
  case "$1" in
    --check) MODE="check" ;;
    --apply) MODE="apply" ;;
    --backup-dir) BACKUP_DIR="${2:?--backup-dir needs a path}"; shift ;;
    --no-backup) DO_BACKUP="no" ;;
    --remove-swap) RM_SWAP="yes" ;;
    --remove-toolchain) RM_TOOLCHAIN="yes" ;;
    --keep-user) RM_USER="no" ;;
    --config) CONFIG="${2:?--config needs a path}"; CONFIG_DIR="$(dirname "$CONFIG")"; shift ;;
    --prefix) PREFIX="${2:?--prefix needs a directory}"; shift ;;
    --yes) ASSUME_YES="yes" ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown option: $1 (try --help)" ;;
  esac
  shift
done
[ -n "$BACKUP_DIR" ] || BACKUP_DIR="/var/backups"

# Read the real paths out of the config when it is there, so an install that
# moved its store is not left behind by an uninstaller using defaults.
tomlstr() { # tomlstr <key>
  [ -r "$CONFIG" ] || return 0
  sed -n "s/^[[:space:]]*$1[[:space:]]*=[[:space:]]*\"\([^\"]*\)\".*/\1/p" "$CONFIG" | head -1
}
_p="$(tomlstr path)";            [ -n "$_p" ] && STATE_DIR="$(dirname "$_p")"
_k="$(tomlstr service_keyfile)"; [ -n "$_k" ] && KEY_DIR="$(dirname "$_k")"
META_ENGINE="$(tomlstr engine)"; [ -n "$META_ENGINE" ] || META_ENGINE="sqlite"
STORE_FILE="$_p"

# ------------------------------------------------------------------ inventory

step "What is here"
present() { [ -e "$1" ] && printf '  PRESENT  %s\n' "$1" || printf '  absent   %s\n' "$1"; }
present "$UNIT"
present "$CONFIG_DIR"
present "$STATE_DIR"
present "$KEY_DIR"
present "$PREFIX/autodb"
[ "$RM_SWAP" = "yes" ] && present /swapfile
[ "$RM_TOOLCHAIN" = "yes" ] && { present "$HOME/.local/share/mise"; present "$HOME/.local/bin/mise"; }
printf '  %s  service account %s\n' "$(id "$RUN_USER" >/dev/null 2>&1 && echo PRESENT || echo 'absent ')" "$RUN_USER"

if [ -n "$STORE_FILE" ] && [ -r "$STORE_FILE" ]; then
  info ""
  info "meta store: $STORE_FILE ($(du -h "$STORE_FILE" 2>/dev/null | awk '{print $1}'))"
  info "  it holds users, grants, the audit log and the ENCRYPTED CONNECTION"
  info "  SECRETS. Deleting it destroys those secrets permanently."
elif [ "$META_ENGINE" = "postgres" ]; then
  info ""
  info "meta store: POSTGRES, and this script does NOT touch it."
  info "  Dropping a database is not something an uninstaller should decide."
  info "  Remove it yourself when you are certain."
fi

if [ "$DO_BACKUP" = "yes" ]; then
  info ""
  info "backup    : $BACKUP_DIR/autodb-<timestamp>.tar.gz"
  info "  includes the meta store and the config; EXCLUDES the service keyfile,"
  info "  because both halves of the envelope in one archive would make the"
  info "  backup itself a credential."
else
  info ""
  info "backup    : SKIPPED (--no-backup)"
fi

if [ "$MODE" = "check" ]; then
  say ""
  say "Nothing was changed. Re-run with --apply to remove."
  exit 0
fi

# -------------------------------------------------------------------- confirm

[ "$(id -u)" -eq 0 ] || die "--apply needs root"

if [ "$ASSUME_YES" != "yes" ] && [ -r /dev/tty ]; then
  say ""
  if [ "$DO_BACKUP" = "no" ]; then
    printf 'Remove all of the above WITHOUT a backup? Encrypted connection secrets will be unrecoverable. [yes/no]: ' > /dev/tty
  else
    printf 'Remove all of the above? [yes/no]: ' > /dev/tty
  fi
  IFS= read -r _a < /dev/tty || _a="no"
  case "$_a" in y|Y|yes|YES|Yes) ;; *) die "aborted; nothing was changed" ;; esac
fi

# --------------------------------------------------------------------- backup

if [ "$DO_BACKUP" = "yes" ]; then
  step "Backup"
  if [ -n "$STORE_FILE" ] && [ -r "$STORE_FILE" ]; then
    mkdir -p "$BACKUP_DIR"; chmod 0700 "$BACKUP_DIR"
    _ts="$(date -u +%Y%m%dT%H%M%SZ)"
    _archive="$BACKUP_DIR/autodb-$_ts.tar.gz"
    _stage="$(mktemp -d)"
    mkdir -p "$_stage/autodb-$_ts"

    # ALL THREE sqlite files, or none of them is useful: the -wal holds
    # committed pages the main file does not have yet, so a lone meta.db can
    # be an older database than the one that was running.
    for f in "$STORE_FILE" "$STORE_FILE-wal" "$STORE_FILE-shm"; do
      [ -e "$f" ] && cp -p "$f" "$_stage/autodb-$_ts/" 2>/dev/null || true
    done
    [ -r "$CONFIG" ] && cp -p "$CONFIG" "$_stage/autodb-$_ts/" 2>/dev/null || true
    # Certificates are reissuable, but keeping them saves redistributing ca.pem.
    [ -d "$CONFIG_DIR/tls" ] && cp -rp "$CONFIG_DIR/tls" "$_stage/autodb-$_ts/" 2>/dev/null || true

    # The keyfile is NOT copied. Stated in the archive itself so whoever finds
    # it later knows what it can and cannot open.
    cat > "$_stage/autodb-$_ts/README" <<README
autodb backup taken $_ts by uninstall.sh

Contains: the meta store (and its -wal/-shm, which are part of the database),
the config, and the front door's TLS material if it existed.

DOES NOT CONTAIN the service keyfile, deliberately. That file and this store
are the two halves of one envelope; putting both in one archive would make
this file a credential rather than a backup. Opening this store needs a user
passphrase, which is the property that choice preserves.

The connection secrets in here are ENCRYPTED. Without a passphrase that
unwraps the master key, they cannot be read -- and there is no other copy of
that key.
README

    tar -czf "$_archive" -C "$_stage" "autodb-$_ts"
    chmod 0600 "$_archive"
    rm -rf "$_stage"
    info "wrote $_archive ($(du -h "$_archive" | awk '{print $1}'), mode 0600)"
    [ -n "$_k" ] && [ -e "$_k" ] && info "keyfile left in place at $_k (NOT in the archive)"
  else
    info "no meta store to archive"
  fi
fi

# --------------------------------------------------------------------- remove

step "Service"
if [ -e "$UNIT" ]; then
  systemctl disable --now autodb-frontdoor 2>/dev/null || true
  rm -f "$UNIT"; systemctl daemon-reload
  systemctl reset-failed autodb-frontdoor 2>/dev/null || true
  info "unit removed"
else
  info "no unit"
fi

step "Files"
for d in "$CONFIG_DIR" "$STATE_DIR" "$KEY_DIR"; do
  if [ -e "$d" ]; then rm -rf "$d"; info "removed $d"; fi
done
[ -e "$PREFIX/autodb" ] && { rm -f "$PREFIX/autodb"; info "removed $PREFIX/autodb"; }

if [ "$RM_USER" = "yes" ]; then
  step "Service account"
  if id "$RUN_USER" >/dev/null 2>&1; then
    userdel "$RUN_USER" 2>/dev/null && info "deleted user $RUN_USER" || warn "could not delete $RUN_USER"
    groupdel "$RUN_USER" 2>/dev/null || true
  else
    info "no such user"
  fi
fi

if [ "$RM_SWAP" = "yes" ]; then
  step "Swap"
  if [ -e /swapfile ]; then
    swapoff /swapfile 2>/dev/null || true
    rm -f /swapfile
    sed -i '\#^/swapfile #d' /etc/fstab 2>/dev/null || true
    info "removed /swapfile and its fstab entry"
  else
    info "no /swapfile"
  fi
fi

if [ "$RM_TOOLCHAIN" = "yes" ]; then
  step "Toolchain"
  rm -rf "$HOME/.local/share/mise" "$HOME/.local/bin/mise" "$HOME/.config/mise"
  info "removed mise and its managed toolchains"
fi

step "Left alone"
if [ "$META_ENGINE" = "postgres" ]; then
  info "the PostgreSQL meta store and its server: dropping a database is not"
  info "an uninstaller's decision"
fi
[ "$DO_BACKUP" = "yes" ] && info "the backup archive in $BACKUP_DIR"
[ -n "$_k" ] && info "nothing else references the keyfile path now"
info "git, curl and other base packages"

say ""
say "Uninstalled."
