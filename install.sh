#!/bin/sh
# Passway installer: downloads the passway CLI, verifies its checksum and
# installs it. Env: PASSWAY_VERSION, PASSWAY_INSTALL_DIR, PASSWAY_DOWNLOAD_URL.
set -eu

fail() {
	echo "error: $*" >&2
	exit 1
}

case "$(uname -s)" in
Linux) os=linux ;;
Darwin) os=darwin ;;
*) fail "unsupported OS $(uname -s); download a build from https://github.com/mohammed19n/passway/releases" ;;
esac

case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) fail "unsupported architecture $(uname -m); download a build from https://github.com/mohammed19n/passway/releases" ;;
esac

base="${PASSWAY_DOWNLOAD_URL:-https://github.com/mohammed19n/passway/releases/latest/download}"
if [ -n "${PASSWAY_VERSION:-}" ]; then
	base="${PASSWAY_DOWNLOAD_URL:-https://github.com/mohammed19n/passway/releases}/download/v${PASSWAY_VERSION#v}"
fi

if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -qO "$2" "$1"; }
else
	fail "curl or wget is required"
fi

if command -v sha256sum >/dev/null 2>&1; then
	hash_of() { sha256sum "$1" | cut -d ' ' -f 1; }
elif command -v shasum >/dev/null 2>&1; then
	hash_of() { shasum -a 256 "$1" | cut -d ' ' -f 1; }
else
	fail "sha256sum or shasum is required"
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

file="passway_${os}_${arch}.tar.gz"
fetch "$base/$file" "$tmp/$file" || fail "download failed: $base/$file"
fetch "$base/checksums.txt" "$tmp/checksums.txt" || fail "download failed: $base/checksums.txt"

want="$(awk -v f="$file" '$2 == f || $2 == "*" f { print $1; exit }' "$tmp/checksums.txt")"
[ -n "$want" ] || fail "no checksum listed for $file"
got="$(hash_of "$tmp/$file")"
[ "$want" = "$got" ] || fail "checksum mismatch for $file; nothing installed"

tar -xzf "$tmp/$file" -C "$tmp" passway || fail "could not extract $file"

if [ -n "${PASSWAY_INSTALL_DIR:-}" ]; then
	dir="$PASSWAY_INSTALL_DIR"
elif [ -w /usr/local/bin ]; then
	dir=/usr/local/bin
else
	dir="$HOME/.local/bin"
fi
mkdir -p "$dir"
cp "$tmp/passway" "$dir/passway.new"
chmod 755 "$dir/passway.new"
mv -f "$dir/passway.new" "$dir/passway"

ver="$("$dir/passway" version 2>/dev/null | awk '{print $2; exit}' || true)"
echo "passway ${ver:-${PASSWAY_VERSION:-latest}} installed to $dir/passway"

case ":$PATH:" in
*":$dir:"*) ;;
*) echo "note: $dir is not on your PATH; add it with: export PATH=\"$dir:\$PATH\"" ;;
esac
echo "next: passway login <token>"
