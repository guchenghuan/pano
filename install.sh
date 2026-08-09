#!/bin/sh
# pano installer — detect the platform, download the matching prebuilt
# binary from the latest GitHub release, verify its checksum, and install
# it into a directory on PATH.
#
#   curl -fsSL https://raw.githubusercontent.com/OWNER/pano/main/install.sh | sh
#
set -eu

REPO="OWNER/pano" # ← 推 GitHub 后把 OWNER 换成你的用户名
BIN="pano"

main() {
	os=$(uname -s | tr '[:upper:]' '[:lower:]')
	machine=$(uname -m)
	case "$os" in
	darwin | linux) ;;
	*) fail "unsupported OS: $os (pano builds for macOS and Linux)" ;;
	esac
	case "$machine" in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) fail "unsupported architecture: $machine" ;;
	esac

	pkg="${BIN}_${os}_${arch}.tar.gz"
	base="https://github.com/${REPO}/releases/latest/download"
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT

	info "downloading ${pkg} (latest release)"
	download "${base}/${pkg}" "$tmp/$pkg"
	download "${base}/${pkg}.sha256" "$tmp/$pkg.sha256"

	want=$(awk '{print $1}' "$tmp/$pkg.sha256")
	got=$(sha256_of "$tmp/$pkg")
	[ "$want" = "$got" ] || fail "checksum mismatch for $pkg (want $want, got $got)"

	tar -xzf "$tmp/$pkg" -C "$tmp"
	[ -f "$tmp/$BIN" ] || fail "archive did not contain $BIN"

	dest=$(install_dir)
	mv "$tmp/$BIN" "$dest/$BIN"
	chmod +x "$dest/$BIN"
	info "installed: $dest/$BIN"

	case ":$PATH:" in
	*":$dest:"*) ;;
	*) warn "$dest is not on your PATH — add it, e.g.: export PATH=\"$dest:\$PATH\"" ;;
	esac

	info "done — run: $BIN"
}

download() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$1" -o "$2" || fail "download failed: $1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q "$1" -O "$2" || fail "download failed: $1"
	else
		fail "need curl or wget"
	fi
}

sha256_of() {
	if command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{print $1}'
	elif command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	else
		fail "need shasum or sha256sum for checksum verification"
	fi
}

install_dir() {
	if [ -w /usr/local/bin ]; then
		echo /usr/local/bin
	elif [ -w /opt/homebrew/bin ]; then
		echo /opt/homebrew/bin
	else
		mkdir -p "$HOME/.local/bin"
		echo "$HOME/.local/bin"
	fi
}

info() { printf '\033[32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[33mwarning:\033[0m %s\n' "$*" >&2; }
fail() {
	printf '\033[31merror:\033[0m %s\n' "$*" >&2
	exit 1
}

main "$@"
