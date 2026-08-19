#!/bin/sh
# scrapemychats installer for macOS and Linux.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/saigyo/scrapemychats/main/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/saigyo/scrapemychats/main/install.sh | sh -s -- /path/to/dir
#
# It downloads the latest release archive from GitHub, unzips the
# `scrapemychats` binary into ~/Documents/scrapemychats (or a directory you
# pass as the first argument), and makes it executable.
#
# The macOS binaries are signed with a Developer ID and notarized by Apple, so
# they run without a Gatekeeper block however you download them. This installer
# is just the convenient path: it auto-detects Intel vs Apple Silicon and drops
# the binary in place for you.
set -eu

REPO="saigyo/scrapemychats"
PROJECT="scrapemychats"

err() { printf '%s\n' "$*" >&2; }

need() {
  command -v "$1" >/dev/null 2>&1 || {
    err "Error: required tool '$1' is not installed."
    exit 1
  }
}

need curl
need unzip

# --- Detect OS ---------------------------------------------------------------
os="$(uname -s)"
case "$os" in
  Darwin) GOOS="darwin" ;;
  Linux)  GOOS="linux" ;;
  *)
    err "Unsupported OS: $os. On Windows, download the zip from:"
    err "  https://github.com/$REPO/releases/latest"
    exit 1
    ;;
esac

# --- Detect architecture -----------------------------------------------------
# Must match GoReleaser's goarch values (amd64 / arm64). macOS ships per-arch
# binaries (Intel = amd64, Apple Silicon = arm64), same as Linux.
arch="$(uname -m)"
case "$arch" in
  x86_64 | amd64)        GOARCH="amd64" ;;
  arm64 | aarch64)       GOARCH="arm64" ;;
  *)
    err "Unsupported architecture: $arch"
    exit 1
    ;;
esac

# --- Compose the archive name ------------------------------------------------
# This MUST match .goreleaser.yaml archives.name_template EXACTLY:
#   {{ .ProjectName }}_{{ .Os }}_{{ .Arch }}.zip
# macOS  -> scrapemychats_darwin_amd64.zip / scrapemychats_darwin_arm64.zip
# Linux  -> scrapemychats_linux_amd64.zip  / scrapemychats_linux_arm64.zip
ARCHIVE="${PROJECT}_${GOOS}_${GOARCH}.zip"

URL="https://github.com/${REPO}/releases/latest/download/${ARCHIVE}"

# --- Destination -------------------------------------------------------------
DEST="${1:-$HOME/Documents/$PROJECT}"
mkdir -p "$DEST"

# --- Download + extract ------------------------------------------------------
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT INT TERM

printf 'Downloading %s\n' "$URL"
if ! curl -fsSL "$URL" -o "$TMP/$ARCHIVE"; then
  err "Download failed. Has a release been published yet?"
  err "See: https://github.com/$REPO/releases/latest"
  exit 1
fi

printf 'Extracting into %s\n' "$DEST"
# -o overwrites without prompting, making re-runs idempotent.
unzip -o -q "$TMP/$ARCHIVE" -d "$DEST"

BIN="$DEST/$PROJECT"
if [ ! -f "$BIN" ]; then
  err "Extracted archive did not contain '$PROJECT'."
  exit 1
fi
chmod +x "$BIN"

# --- Done --------------------------------------------------------------------
cat <<EOF

Installed to: $BIN

To run it:
  - Open the folder $DEST and double-click "$PROJECT", or
  - Run it from a terminal:
        "$BIN"

A Chrome or Edge window will open — log into ChatGPT (pick the right
workspace), then leave it running. When it finishes it builds the viewer and
your files land next to the app in $DEST. Closing the window or pressing
Ctrl+C is safe; re-running resumes where it left off.
EOF
