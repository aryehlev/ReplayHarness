#!/bin/sh
# Installs the replayharness binary from GitHub Releases.
#
#   curl -fsSL https://raw.githubusercontent.com/aryehlev/ReplayHarness/main/install.sh | sh
#
# Environment:
#   REPLAYHARNESS_VERSION      a release tag such as v0.1.0 (default: the latest release)
#   REPLAYHARNESS_INSTALL_DIR  where to put the binary (default: /usr/local/bin if writable, else ~/.local/bin)
set -eu

repo=aryehlev/ReplayHarness

case $(uname -s) in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*) echo "replayharness: unsupported OS $(uname -s). Download a binary from https://github.com/$repo/releases" >&2; exit 1 ;;
esac
case $(uname -m) in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) echo "replayharness: unsupported CPU $(uname -m). Download a binary from https://github.com/$repo/releases" >&2; exit 1 ;;
esac

version=${REPLAYHARNESS_VERSION:-latest}
if [ "$version" = latest ]; then
	base=https://github.com/$repo/releases/latest/download
else
	base=https://github.com/$repo/releases/download/$version
fi
archive=replayharness_${os}_${arch}.tar.gz

dir=${REPLAYHARNESS_INSTALL_DIR:-}
if [ -z "$dir" ]; then
	if [ -w /usr/local/bin ]; then dir=/usr/local/bin; else dir=$HOME/.local/bin; fi
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "Downloading $archive ($version)"
curl -fsSL "$base/$archive" -o "$tmp/$archive"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"

expected=$(grep " $archive\$" "$tmp/checksums.txt" | cut -d' ' -f1)
if command -v sha256sum >/dev/null 2>&1; then
	actual=$(sha256sum "$tmp/$archive" | cut -d' ' -f1)
else
	actual=$(shasum -a 256 "$tmp/$archive" | cut -d' ' -f1)
fi
if [ -z "$expected" ] || [ "$expected" != "$actual" ]; then
	echo "replayharness: checksum mismatch for $archive" >&2
	exit 1
fi

tar -xzf "$tmp/$archive" -C "$tmp" replayharness
mkdir -p "$dir"
mv "$tmp/replayharness" "$dir/replayharness"
chmod +x "$dir/replayharness"

echo "Installed $("$dir/replayharness" version) to $dir/replayharness"
case ":$PATH:" in
	*":$dir:"*) ;;
	*) echo "Add $dir to your PATH, e.g.: echo 'export PATH=\"$dir:\$PATH\"' >> ~/.zshrc" ;;
esac
