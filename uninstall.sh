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
# credential.
#
# The keyfile is EXCLUDED FROM THE ARCHIVE and then DELETED with everything
# else, which is worth stating precisely because an earlier version of this
# comment said it was "left in place" and that was simply wrong. Losing it
# costs nothing recoverable: it only ever unwrapped the master key for
# unattended start, and the archive still carries every USER's
# passphrase-wrapped slot -- so an operator who knows a user passphrase can
# restore and open the store, while nobody who merely holds the archive can.
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
  --print-targets      Print the resolved deletion set, one path per line,
                       and exit. Changes nothing. This is what makes the
                       destructive surface testable rather than a matter of
                       reading the script.
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
    --print-targets) MODE="targets" ;;
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

# Read the real paths out of the config, so an install that moved its store is
# not left behind by an uninstaller using defaults.
#
# THIS IS NOT A TOML PARSER AND MUST NOT BE TRUSTED LIKE ONE. It is a
# line-scanner that tracks the current [section] and accepts a plain
# double-quoted scalar. It does not understand escapes, multi-line strings,
# inline tables or dotted keys. Every value it produces is therefore treated as
# UNTRUSTED INPUT and validated before anything is deleted -- see safe_target.
#
# A review found why that matters: an earlier version took dirname() of the
# store path and recursively removed the result, so a perfectly valid
# `path = "/etc/meta.db"` meant `rm -rf /etc`. Nothing about the generated
# layout prevented that, and --config exists precisely so the layout can
# differ. The fix is structural rather than a bigger denylist: this script no
# longer DERIVES a directory to delete from a file path. It removes the files
# it can name, and removes a directory only when that directory is empty
# afterwards.
tomlstr() { # tomlstr <section> <key>
  [ -r "$CONFIG" ] || return 0
  awk -v want_s="$1" -v want_k="$2" '
    /^[[:space:]]*\[/ { s=$0; gsub(/^[[:space:]]*\[|\][[:space:]]*$/,"",s); next }
    {
      line=$0
      sub(/[[:space:]]*#.*$/,"",line)
      if (s != want_s) next
      if (match(line, "^[[:space:]]*" want_k "[[:space:]]*=[[:space:]]*\"")) {
        v = line; sub("^[^\"]*\"", "", v); sub("\".*$", "", v)
        print v; exit
      }
    }' "$CONFIG"
}

# safe_target refuses a deletion target that is not plausibly ours.
#
# Belt and braces on top of the structural fix: even though nothing is derived
# by dirname any more, a path that arrives from a config still decides what gets
# unlinked, so it is checked rather than trusted.
DENY="/ /bin /boot /dev /etc /home /lib /lib32 /lib64 /media /mnt /opt /proc /root /run /sbin /srv /sys /tmp /usr /usr/bin /usr/lib /usr/local /usr/local/bin /usr/sbin /var /var/backups /var/lib /var/log /var/run /var/tmp"
# safe_target <path> [strict] -- prints the canonical path, or fails.
#
# `strict` is passed for CONFIG-DERIVED paths and additionally refuses a target
# whose PARENT is a system location. That distinction matters: /usr/local/bin
# is a system location and the binary legitimately lives in it, but a store at
# /etc/meta.db is a config value reaching into one, and the two cannot be told
# apart by the path alone -- only by whether we chose it or a file did.
safe_target() { # safe_target <path> [strict]
  _t="${1:-}"
  _strict="${2:-}"
  [ -n "$_t" ] || return 1
  case "$_t" in
    /*) ;;
    *) warn "refusing a relative path: $_t"; return 1 ;;
  esac
  case "$_t" in
    *..*) warn "refusing a path containing '..': $_t"; return 1 ;;
  esac
  # Canonicalise without requiring existence, so a symlinked parent cannot
  # smuggle the target somewhere else.
  _c="$(readlink -m -- "$_t" 2>/dev/null || printf '%s' "$_t")"
  for _d in $DENY; do
    if [ "$_c" = "$_d" ]; then
      warn "refusing to touch $_c: it is a system location"
      return 1
    fi
  done
  # Two components minimum (/a/b), so a single top-level directory can never
  # be a target even if the denylist misses its name.
  _depth="$(printf '%s' "${_c#/}" | awk -F/ '{print NF}')"
  if [ "${_depth:-0}" -lt 2 ]; then
    warn "refusing $_c: too shallow to be an autodb path"
    return 1
  fi
  if [ "$_strict" = "strict" ]; then
    _parent="$(dirname -- "$_c")"
    for _d in $DENY; do
      if [ "$_parent" = "$_d" ]; then
        warn "refusing $_c: a config value must not place autodb files directly"
        warn "  in $_parent, which is a system location. Put them in a directory"
        warn "  of their own."
        return 1
      fi
    done
  fi
  printf '%s' "$_c"
}

_p="$(tomlstr meta path)"
_k="$(tomlstr security service_keyfile)"
META_ENGINE="$(tomlstr meta engine)"; [ -n "$META_ENGINE" ] || META_ENGINE="sqlite"

# The store FILE, validated. Its directory is NOT inferred as a delete target.
STORE_FILE=""
if [ -n "$_p" ]; then
  STORE_FILE="$(safe_target "$_p" strict)" || die "the config's [meta] path is not a safe target; refusing to continue"
fi
KEYFILE=""
if [ -n "$_k" ]; then
  KEYFILE="$(safe_target "$_k" strict)" || die "the config's [security] service_keyfile is not a safe target; refusing to continue"
fi
# The default directories are ours by construction, and still validated.
STATE_DIR="$(safe_target "$STATE_DIR")" || die "state directory is not a safe target"
KEY_DIR="$(safe_target "$KEY_DIR")"     || die "keyfile directory is not a safe target"
CONFIG_DIR="$(safe_target "$CONFIG_DIR")" || die "config directory is not a safe target"

# ------------------------------------------------------------------ inventory

# Printed before the inventory so a caller can assert on the target set without
# parsing prose. Every line is a path this script would unlink or rmdir.
if [ "$MODE" = "targets" ]; then
  [ -n "$STORE_FILE" ] && printf '%s\n%s-wal\n%s-shm\n' "$STORE_FILE" "$STORE_FILE" "$STORE_FILE"
  printf '%s\n' "$STATE_DIR/autodb.sock"
  [ -n "$KEYFILE" ] && printf '%s\n' "$KEYFILE"
  printf '%s\n' "$CONFIG"
  for f in ca.pem ca.key cert.pem key.pem intermediate.pem intermediate.key; do
    printf '%s\n' "$CONFIG_DIR/tls/$f"
  done
  printf '%s\n' "$CONFIG_DIR/tls" "$PREFIX/autodb" "$STATE_DIR" "$KEY_DIR" "$CONFIG_DIR"
  [ "$RM_SWAP" = "yes" ] && printf '%s\n' /swapfile
  exit 0
fi

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

# THE SERVICE STOPS FIRST, and that ordering is the whole basis of the backup
# below being coherent.
#
# A review caught this the wrong way round: the archive was staged while the
# daemon could still be writing, so copying meta.db with its -wal and -shm was
# not a snapshot at all -- a commit or checkpoint landing mid-copy splits the
# three files across states, and the header claimed a stopped-daemon rationale
# the code did not honour. Copying all three together is only equivalent to a
# checkpoint when there is no concurrent writer.
step "Service"
if [ -e "$UNIT" ]; then
  systemctl disable --now autodb-frontdoor 2>/dev/null || true
  # Wait for it to actually be gone, rather than assuming disable --now
  # returned after the process exited.
  _w=0
  while systemctl is-active --quiet autodb-frontdoor 2>/dev/null; do
    _w=$(( _w + 1 ))
    [ "$_w" -gt 30 ] && die "autodb-frontdoor did not stop; refusing to archive a live store"
    sleep 1
  done
  info "service stopped"
else
  info "no unit; nothing to stop"
fi

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
    [ -n "$KEYFILE" ] && [ -e "$KEYFILE" ] && \
      info "keyfile EXCLUDED from the archive and removed below: $KEYFILE"
  else
    info "no meta store to archive"
  fi
fi

# --------------------------------------------------------------------- remove

step "Unit"
if [ -e "$UNIT" ]; then
  rm -f "$UNIT"; systemctl daemon-reload
  systemctl reset-failed autodb-frontdoor 2>/dev/null || true
  info "unit removed"
else
  info "no unit"
fi

# ---------------------------------------------------------------------- files
#
# NAMED FILES, THEN EMPTY DIRECTORIES. Nothing here recurses, and that is the
# point: an uninstaller that computes a directory and removes it recursively is
# one bad config value away from deleting a system tree, which is exactly the
# defect a review found in the first version. Removing what we can name and
# then rmdir'ing means a directory holding something we did not put there
# SURVIVES, and says so, instead of being destroyed on our assumption.
step "Files"

rm_file() { [ -e "$1" ] && { rm -f "$1"; info "removed $1"; }; }

# The store and the two files that are part of it.
if [ -n "$STORE_FILE" ]; then
  rm_file "$STORE_FILE"; rm_file "$STORE_FILE-wal"; rm_file "$STORE_FILE-shm"
fi
# The socket the daemon binds, which lives beside the store by default.
rm_file "$STATE_DIR/autodb.sock"
rm_file "$KEYFILE"
rm_file "$CONFIG"
# TLS material is reissuable and ours by construction.
if [ -d "$CONFIG_DIR/tls" ]; then
  for f in ca.pem ca.key cert.pem key.pem intermediate.pem intermediate.key; do
    rm_file "$CONFIG_DIR/tls/$f"
  done
  rmdir "$CONFIG_DIR/tls" 2>/dev/null && info "removed $CONFIG_DIR/tls" || true
fi
rm_file "$PREFIX/autodb"

for d in "$STATE_DIR" "$KEY_DIR" "$CONFIG_DIR"; do
  [ -d "$d" ] || continue
  if rmdir "$d" 2>/dev/null; then
    info "removed $d"
  else
    warn "$d is not empty and was LEFT IN PLACE; it holds something this"
    warn "  installer did not create:"
    ls -A "$d" 2>/dev/null | sed 's/^/    /' >&2
  fi
done

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
info "the service keyfile: excluded from the archive AND deleted. It only ever"
info "  unwrapped the master key for unattended start; every user's own"
info "  passphrase-wrapped slot is still in the archive"
info "git, curl and other base packages"

say ""
say "Uninstalled."
