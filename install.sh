#!/usr/bin/env sh
#
# autodb installer.
#
# Downloads a verified release binary for this platform, or builds from
# source with the Go toolchain when no prebuilt binary fits (or when you
# ask for it). POSIX sh — no bashisms, no dependencies beyond curl/tar
# and, for the source path, git + go.
#
#   curl -fsSL https://raw.githubusercontent.com/yongjohnlee80/autodb/main/install.sh | sh
#
# Read it first. Piping a script from the internet into a shell is a
# thing you should only do after looking at what it does.

set -eu

REPO="yongjohnlee80/autodb"
MIN_GO_MAJOR=1
MIN_GO_MINOR=25

VERSION="latest"
PREFIX="${AUTODB_PREFIX:-$HOME/.local/bin}"
MODE="auto"          # auto | binary | source
KEEP_TMP=0

say()  { printf '%s\n' "$*"; }
info() { printf '  %s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<'USAGE'
autodb installer

USAGE:
  install.sh [OPTIONS]

OPTIONS:
  --version <tag>   Release to install (e.g. v0.3.0). Default: latest
  --prefix <dir>    Install directory. Default: $AUTODB_PREFIX or ~/.local/bin
  --source          Build from source with Go, even if a binary is available
  --binary          Only use a prebuilt binary; fail instead of building
  --keep-tmp        Leave the working directory behind (for debugging)
  -h, --help        Show this help

ENVIRONMENT:
  AUTODB_PREFIX     Same as --prefix

By default the script downloads the release tarball for your platform,
verifies its published SHA-256 checksum, and installs the binary as
`autodb` in the prefix. If no prebuilt binary matches your OS/arch it
falls back to building from source, which needs git and Go 1.25+.
USAGE
}

while [ $# -gt 0 ]; do
  case "$1" in
    --version) [ $# -ge 2 ] || die "--version needs a value"; VERSION="$2"; shift 2 ;;
    --prefix)  [ $# -ge 2 ] || die "--prefix needs a value";  PREFIX="$2";  shift 2 ;;
    --source)  MODE="source"; shift ;;
    --binary)  MODE="binary"; shift ;;
    --keep-tmp) KEEP_TMP=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown option: $1 (try --help)" ;;
  esac
done

need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"; }

# ---------------------------------------------------------------- platform

detect_platform() {
  os="$(uname -s)"
  arch="$(uname -m)"
  case "$os" in
    Linux)  GOOS="linux" ;;
    Darwin) GOOS="darwin" ;;
    *) GOOS="" ;;
  esac
  case "$arch" in
    x86_64|amd64)  GOARCH="amd64" ;;
    aarch64|arm64) GOARCH="arm64" ;;
    *) GOARCH="" ;;
  esac
}

# ------------------------------------------------------------------ helpers

resolve_latest() {
  # Follow the /releases/latest redirect rather than hitting the API, which
  # is rate-limited for unauthenticated callers and would need a JSON parser.
  url="$(curl -fsSL -o /dev/null -w '%{url_effective}' \
    "https://github.com/$REPO/releases/latest" 2>/dev/null)" \
    || die "could not reach GitHub to resolve the latest release"
  tag="${url##*/}"
  case "$tag" in
    v*) printf '%s\n' "$tag" ;;
    *)  die "could not parse a version tag out of '$url'" ;;
  esac
}

verify_sha256() {
  # $1 = file, $2 = file containing "<sum>  <name>"
  want="$(cut -d' ' -f1 < "$2")"
  if command -v sha256sum >/dev/null 2>&1; then
    got="$(sha256sum "$1" | cut -d' ' -f1)"
  elif command -v shasum >/dev/null 2>&1; then
    got="$(shasum -a 256 "$1" | cut -d' ' -f1)"
  else
    die "neither sha256sum nor shasum found — cannot verify the download"
  fi
  [ "$want" = "$got" ] || die "checksum mismatch: expected $want, got $got"
  info "checksum verified"
}

go_version_ok() {
  raw="$(go env GOVERSION 2>/dev/null || true)"   # e.g. go1.25.3
  raw="${raw#go}"
  major="${raw%%.*}"
  rest="${raw#*.}"
  minor="${rest%%.*}"
  case "$major$minor" in *[!0-9]*|"") return 1 ;; esac
  [ "$major" -gt "$MIN_GO_MAJOR" ] && return 0
  [ "$major" -eq "$MIN_GO_MAJOR" ] && [ "$minor" -ge "$MIN_GO_MINOR" ] && return 0
  return 1
}

install_file() {
  # $1 = built/extracted binary
  mkdir -p "$PREFIX" || die "cannot create $PREFIX"
  [ -w "$PREFIX" ] || die "$PREFIX is not writable. Re-run with --prefix <dir>, or create it with the right ownership. This script never calls sudo for you."
  chmod +x "$1"
  # mv across filesystems can fail; cp then remove is portable.
  cp "$1" "$PREFIX/autodb.tmp.$$" && mv "$PREFIX/autodb.tmp.$$" "$PREFIX/autodb"
  info "installed $PREFIX/autodb"
}

# --------------------------------------------------------------- strategies

install_binary() {
  need curl; need tar
  asset="autodb-$VERSION-$GOOS-$GOARCH.tar.gz"
  base="https://github.com/$REPO/releases/download/$VERSION"
  info "downloading $asset"
  curl -fsSL -o "$TMP/$asset"        "$base/$asset"        || return 1
  curl -fsSL -o "$TMP/$asset.sha256" "$base/$asset.sha256" || return 1
  verify_sha256 "$TMP/$asset" "$TMP/$asset.sha256"
  tar -xzf "$TMP/$asset" -C "$TMP"
  built="$TMP/autodb-$VERSION-$GOOS-$GOARCH"
  [ -f "$built" ] || die "the tarball did not contain the expected binary"
  install_file "$built"
}

install_source() {
  need git; need go
  go_version_ok || die "Go $MIN_GO_MAJOR.$MIN_GO_MINOR+ is required to build from source (found ${raw:-none})"
  info "cloning $REPO at $VERSION"
  git clone --depth 1 --branch "$VERSION" "https://github.com/$REPO.git" "$TMP/src" >/dev/null 2>&1 \
    || die "could not clone $REPO at $VERSION"
  commit="$(git -C "$TMP/src" rev-parse --short HEAD)"
  date="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  info "building (go $(go env GOVERSION), CGO disabled)"
  ( cd "$TMP/src" && CGO_ENABLED=0 go build \
      -ldflags "-X main.version=$VERSION -X main.commit=$commit -X main.buildDate=$date" \
      -o "$TMP/autodb" ./cmd/autodb ) || die "build failed"
  install_file "$TMP/autodb"
}

# -------------------------------------------------------------------- main

need curl
detect_platform

[ "$VERSION" = "latest" ] && VERSION="$(resolve_latest)"
say "autodb $VERSION"

TMP="$(mktemp -d)" || die "could not create a temporary directory"
# shellcheck disable=SC2064
[ "$KEEP_TMP" -eq 1 ] || trap "rm -rf '$TMP'" EXIT INT TERM

case "$MODE" in
  source)
    install_source
    ;;
  binary)
    [ -n "$GOOS" ] && [ -n "$GOARCH" ] \
      || die "no prebuilt binary for $(uname -s)/$(uname -m); re-run with --source"
    install_binary || die "download failed for $GOOS/$GOARCH at $VERSION"
    ;;
  *)
    if [ -n "$GOOS" ] && [ -n "$GOARCH" ] && install_binary; then
      :
    else
      warn "no prebuilt binary for $(uname -s)/$(uname -m) at $VERSION — building from source"
      install_source
    fi
    ;;
esac

# ------------------------------------------------------------------ report

"$PREFIX/autodb" --version >/dev/null 2>&1 \
  || die "$PREFIX/autodb was installed but does not run"

say ""
say "$("$PREFIX/autodb" --version)"

case ":${PATH}:" in
  *":$PREFIX:"*) say "Run: autodb --ui" ;;
  *)
    say ""
    warn "$PREFIX is not on your PATH."
    say "  Add it to your shell profile:"
    say ""
    say "      export PATH=\"$PREFIX:\$PATH\""
    say ""
    say "  Or run it directly: $PREFIX/autodb --ui"
    ;;
esac
