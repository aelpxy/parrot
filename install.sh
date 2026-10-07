#!/bin/sh

set -eu

repo="${PARROT_REPO:-aelpxy/parrot}"
version="${PARROT_VERSION:-latest}"

if [ -t 2 ] && [ -z "${NO_COLOR:-}" ]; then
	accent=$(printf '\033[38;5;86m')
	green=$(printf '\033[1;38;5;42m')
	red=$(printf '\033[1;38;5;204m')
	faint=$(printf '\033[2m')
	reset=$(printf '\033[0m')
else
	accent='' green='' red='' faint='' reset=''
fi

note() { printf '%s•%s %s\n' "$faint" "$reset" "$1" >&2; }
ok() { printf '%s✓%s %s\n' "$green" "$reset" "$1" >&2; }
fail() {
	printf '%s✗%s %s\n' "$red" "$reset" "$1" >&2
	[ -n "${2:-}" ] && printf '  %s%s%s\n' "$faint" "$2" "$reset" >&2
	exit 1
}

detect_platform() {
	case "$(uname -s)" in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	*) fail "unsupported system: $(uname -s)" "parrot runs on Linux and macOS" ;;
	esac

	case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) fail "unsupported CPU: $(uname -m)" "parrot builds for amd64 and arm64" ;;
	esac
}

download() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --retry 3 -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$2" "$1"
	else
		fail "neither curl nor wget is installed"
	fi
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d ' ' -f 1
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | cut -d ' ' -f 1
	else
		fail "no sha256sum or shasum found to verify the download"
	fi
}

pick_dir() {
	sudo=''

	if [ -n "${PARROT_INSTALL_DIR:-}" ]; then
		dir="$PARROT_INSTALL_DIR"
		mkdir -p "$dir" 2>/dev/null || true
		[ -w "$dir" ] || sudo=sudo
		return
	fi

	dir=/usr/local/bin

	if [ -d "$dir" ] && [ -w "$dir" ]; then
		return
	fi

	# Under `curl | sh` stdin is the script, so check for a terminal sudo can prompt on instead.
	if command -v sudo >/dev/null 2>&1 && { (: </dev/tty) 2>/dev/null || sudo -n true 2>/dev/null; }; then
		sudo=sudo
		return
	fi

	dir="$HOME/.local/bin"
	mkdir -p "$dir"
}

main() {
	detect_platform

	asset="parrot-$os-$arch"

	if [ -n "${PARROT_DOWNLOAD_URL:-}" ]; then
		base="$PARROT_DOWNLOAD_URL"
	elif [ "$version" = latest ]; then
		base="https://github.com/$repo/releases/latest/download"
	else
		base="https://github.com/$repo/releases/download/$version"
	fi

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	trap 'exit 130' INT TERM

	note "downloading $asset ($version)"
	download "$base/$asset" "$tmp/parrot" || fail "download failed: $base/$asset" "check that the release exists and has a $asset asset"
	download "$base/checksums.txt" "$tmp/checksums.txt" || fail "download failed: $base/checksums.txt"

	want=$(awk -v f="$asset" '$2 == f { print $1 }' "$tmp/checksums.txt")
	[ -n "$want" ] || fail "no checksum for $asset in checksums.txt"
	[ "$(sha256 "$tmp/parrot")" = "$want" ] || fail "checksum mismatch for $asset" "the download may be corrupted or tampered with; nothing was installed"

	chmod 755 "$tmp/parrot"
	pick_dir

	[ -n "$sudo" ] && note "installing to $dir needs sudo"
	$sudo mkdir -p "$dir"
	$sudo mv "$tmp/parrot" "$dir/parrot"

	ok "installed $("$dir/parrot" version) to $dir/parrot"

	case ":$PATH:" in
	*":$dir:"*) ;;
	*) note "$dir is not on your PATH; add it to your shell profile" ;;
	esac

	printf '  %sget started:%s %sparrot --help%s\n' "$faint" "$reset" "$accent" "$reset" >&2
}

main
