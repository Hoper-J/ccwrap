#!/bin/sh
# ccwrap installer — downloads a prebuilt release binary, verifies its sha256
# against the published checksums.txt, and installs it.
#
#   curl -fsSL https://raw.githubusercontent.com/Hoper-J/ccwrap/main/install.sh | sh
#
# Env knobs:
#   CCWRAP_VERSION  pin a version (e.g. 0.2.0 or v0.2.0); default = latest release
#   CCWRAP_BINDIR   install dir; default = /usr/local/bin if writable else ~/.local/bin
set -eu

REPO="Hoper-J/ccwrap"
BIN="ccwrap"

err() { printf 'install.sh: %s\n' "$1" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

# --- detect platform ---------------------------------------------------------
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  darwin) os=darwin ;;
  linux)  os=linux ;;
  *) err "unsupported OS '$os' (ccwrap supports macOS and Linux)" ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) err "unsupported architecture '$arch'" ;;
esac

# --- download helper ---------------------------------------------------------
if have curl; then
  dl() { curl -fsSL "$1" -o "$2"; }
  dl_stdout() { curl -fsSL "$1"; }
elif have wget; then
  dl() { wget -qO "$2" "$1"; }
  dl_stdout() { wget -qO- "$1"; }
else
  err "need curl or wget"
fi

# --- resolve version ---------------------------------------------------------
ver="${CCWRAP_VERSION:-}"
if [ -z "$ver" ]; then
  ver=$(dl_stdout "https://api.github.com/repos/${REPO}/releases/latest" \
    | grep '"tag_name":' | head -1 | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/')
  [ -n "$ver" ] || err "could not determine latest version (set CCWRAP_VERSION)"
fi
tag="$ver"
case "$tag" in v*) ;; *) tag="v$tag" ;; esac   # download path uses the v-prefixed tag
num="${tag#v}"                                  # archive filenames use the bare number

base="https://github.com/${REPO}/releases/download/${tag}"
archive="${BIN}_${num}_${os}_${arch}.tar.gz"

# --- fetch + verify ----------------------------------------------------------
tmp=$(mktemp -d 2>/dev/null || mktemp -d -t ccwrap)
trap 'rm -rf "$tmp"' EXIT INT TERM

printf 'Downloading %s (%s/%s)...\n' "$tag" "$os" "$arch"
dl "${base}/${archive}" "${tmp}/${archive}" || err "download failed: ${base}/${archive}"
dl "${base}/checksums.txt" "${tmp}/checksums.txt" || err "checksums download failed"

if have sha256sum; then
  sha_cmd="sha256sum"
elif have shasum; then
  sha_cmd="shasum -a 256"
else
  err "need sha256sum or shasum to verify the download"
fi

want=$(grep " ${archive}\$" "${tmp}/checksums.txt" | awk '{print $1}')
[ -n "$want" ] || err "no checksum for ${archive} in checksums.txt"
got=$(cd "$tmp" && $sha_cmd "$archive" | awk '{print $1}')
[ "$want" = "$got" ] || err "checksum mismatch for ${archive} (want ${want}, got ${got})"
printf 'Checksum OK.\n'
# Note: this checks integrity against checksums.txt (fetched over the same TLS
# connection) — it is NOT a signature check. For full supply-chain verification
# (cosign signature pinned to the release's signing identity), see SECURITY.md.

tar -xzf "${tmp}/${archive}" -C "$tmp" || err "extract failed"
[ -f "${tmp}/${BIN}" ] || err "archive did not contain ${BIN}"
chmod +x "${tmp}/${BIN}"

# --- choose bindir + install -------------------------------------------------
bindir="${CCWRAP_BINDIR:-}"
if [ -z "$bindir" ]; then
  if [ -w /usr/local/bin ] 2>/dev/null; then
    bindir=/usr/local/bin
  else
    bindir="${HOME}/.local/bin"
  fi
fi
mkdir -p "$bindir"

if mv "${tmp}/${BIN}" "${bindir}/${BIN}" 2>/dev/null; then
  :
elif have sudo && [ "$bindir" = /usr/local/bin ]; then
  printf 'Need sudo to write to %s\n' "$bindir"
  sudo mv "${tmp}/${BIN}" "${bindir}/${BIN}"
else
  err "could not install to ${bindir} (set CCWRAP_BINDIR to a writable dir)"
fi

printf 'Installed %s to %s/%s\n' "$BIN" "$bindir" "$BIN"
case ":${PATH}:" in
  *":${bindir}:"*) ;;
  *) printf 'Note: %s is not on your PATH — add it so you can run %s directly.\n' "$bindir" "$BIN" ;;
esac
"${bindir}/${BIN}" version 2>/dev/null || true
