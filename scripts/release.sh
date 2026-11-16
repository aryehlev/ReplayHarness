#!/bin/sh
# Builds release archives for every platform into dist/ and, with --publish,
# creates the GitHub release with them.
#
#   scripts/release.sh v0.1.0              build only
#   scripts/release.sh v0.1.0 --publish    build, then create the release (needs gh)
set -eu

tag=${1:?usage: scripts/release.sh vX.Y.Z [--publish]}
case $tag in v[0-9]*) ;; *) echo "tag must look like v0.1.0" >&2; exit 2 ;; esac
version=${tag#v}
cd "$(dirname "$0")/.."

rm -rf dist && mkdir -p dist
for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do
	os=${target%/*}
	arch=${target#*/}
	name=replayharness_${os}_${arch}
	work=dist/$name
	mkdir -p "$work"
	bin=replayharness
	[ "$os" = windows ] && bin=replayharness.exe
	CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
		-ldflags "-s -w -X main.version=$version" -o "$work/$bin" ./cmd/replayharness
	cp README.md LICENSE-MIT LICENSE-APACHE "$work/"
	if [ "$os" = windows ]; then
		(cd "$work" && zip -q "../$name.zip" "$bin" README.md LICENSE-MIT LICENSE-APACHE)
	else
		tar -C "$work" -czf "dist/$name.tar.gz" "$bin" README.md LICENSE-MIT LICENSE-APACHE
	fi
	rm -rf "$work"
	echo "built dist/$name"
done
(cd dist && shasum -a 256 replayharness_* > checksums.txt)

if [ "${2:-}" = --publish ]; then
	gh release create "$tag" dist/* --title "$tag" --generate-notes --verify-tag 2>/dev/null ||
		gh release create "$tag" dist/* --title "$tag" --generate-notes --target main
fi
