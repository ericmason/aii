#!/bin/sh
# Install a prebuilt aii binary — no Go toolchain required.
#
# Detects your OS/arch, downloads the matching archive from the latest
# GitHub release, verifies its SHA-256 checksum, and installs the binary
# to ~/.local/bin (override with AII_INSTALL_DIR).
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/ericmason/aii/main/scripts/install.sh | sh
#
#   # pin a version / change the target dir:
#   AII_VERSION=v0.5.1 AII_INSTALL_DIR=/usr/local/bin sh install.sh
#
# Windows users: download the .zip from the releases page instead.

set -eu

REPO="ericmason/aii"
BINARY="aii"
INSTALL_DIR="${AII_INSTALL_DIR:-${HOME}/.local/bin}"

die() { echo "error: $*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

# --- download helpers (curl or wget) ------------------------------------

fetch() { # URL -> stdout
    if have curl; then curl -fsSL "$1"
    elif have wget; then wget -qO- "$1"
    else die "need curl or wget on PATH"; fi
}
download() { # URL FILE
    if have curl; then curl -fsSL "$1" -o "$2"
    elif have wget; then wget -qO "$2" "$1"
    else die "need curl or wget on PATH"; fi
}
sha256() { # FILE -> hex digest (or empty if no tool)
    if have sha256sum; then sha256sum "$1" | awk '{print $1}'
    elif have shasum; then shasum -a 256 "$1" | awk '{print $1}'
    else echo ""; fi
}

# --- detect platform ----------------------------------------------------

os=$(uname -s)
case "$os" in
    Darwin) os=darwin ;;
    Linux)  os=linux ;;
    *) die "unsupported OS '$os' — download the Windows .zip from https://github.com/${REPO}/releases/latest" ;;
esac

arch=$(uname -m)
case "$arch" in
    x86_64|amd64)   arch=amd64 ;;
    arm64|aarch64)  arch=arm64 ;;
    *) die "unsupported architecture '$arch'" ;;
esac

# --- resolve version ----------------------------------------------------

version="${AII_VERSION:-}"
if [ -z "$version" ]; then
    version=$(fetch "https://api.github.com/repos/${REPO}/releases/latest" \
        | grep '"tag_name"' | head -n1 \
        | sed -E 's/.*"tag_name" *: *"([^"]+)".*/\1/')
fi
[ -n "$version" ] || die "could not determine latest version (set AII_VERSION=vX.Y.Z)"
version="v${version#v}"   # normalize: ensure a single leading 'v'

asset="${BINARY}-${version}-${os}-${arch}.tar.gz"
base="https://github.com/${REPO}/releases/download/${version}"

echo "Installing ${BINARY} ${version} (${os}/${arch})…"

# --- download + verify --------------------------------------------------

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

download "${base}/${asset}" "${tmp}/${asset}" \
    || die "download failed: ${base}/${asset} (does this release ship a ${os}/${arch} build?)"
download "${base}/SHA256SUMS" "${tmp}/SHA256SUMS" \
    || die "could not download SHA256SUMS"

expected=$(awk -v f="$asset" '{name=$2; sub(/^\*/,"",name); if(name==f) print $1}' "${tmp}/SHA256SUMS")
actual=$(sha256 "${tmp}/${asset}")
if [ -n "$expected" ] && [ -n "$actual" ]; then
    [ "$expected" = "$actual" ] || die "checksum mismatch for ${asset}
  expected: ${expected}
  actual:   ${actual}"
    echo "Checksum verified."
else
    echo "warning: skipped checksum verification (no sha256 tool or no sums entry)" >&2
fi

# --- extract + install --------------------------------------------------

tar -xzf "${tmp}/${asset}" -C "${tmp}" || die "failed to extract ${asset}"
src="${tmp}/${asset%.tar.gz}/${BINARY}"
[ -f "$src" ] || die "binary not found in archive (looked for ${BINARY})"

mkdir -p "$INSTALL_DIR"
if install -m 0755 "$src" "${INSTALL_DIR}/${BINARY}" 2>/dev/null; then :
else
    cp "$src" "${INSTALL_DIR}/${BINARY}" && chmod 0755 "${INSTALL_DIR}/${BINARY}" \
        || die "could not install to ${INSTALL_DIR} (try a writable AII_INSTALL_DIR)"
fi

echo "Installed to ${INSTALL_DIR}/${BINARY}"
"${INSTALL_DIR}/${BINARY}" version 2>/dev/null || true

case ":${PATH}:" in
    *":${INSTALL_DIR}:"*) ;;
    *) echo
       echo "note: ${INSTALL_DIR} is not on your PATH. Add this to your shell rc:"
       echo "  export PATH=\"${INSTALL_DIR}:\$PATH\"" ;;
esac
