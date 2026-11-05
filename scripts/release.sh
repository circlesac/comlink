#!/usr/bin/env bash
# Releases comlink: computes the CalVer version from the git tags, builds every target into dist/, and
# publishes a GitHub Release tagged v<version> on the current commit. Run it from a clean main checkout,
# locally or from .github/workflows/release.yml; it needs go, npx (for @circlesac/oneup), zip and an
# authenticated gh.
#
#   scripts/release.sh            build, then publish
#   scripts/release.sh build      only dist/ (the workflow attests these files before publishing)
#   scripts/release.sh publish    publish what build left in dist/
set -euo pipefail
cd "$(dirname "$0")/.."

TARGETS="darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64 windows/arm64"

build() {
	if [ "$(git rev-parse --abbrev-ref HEAD)" != main ] || [ -n "$(git status --porcelain)" ]; then
		echo "release: run from a clean main checkout" >&2
		exit 1
	fi
	version=$(npx --yes @circlesac/oneup version --source git --target version.go | tail -1)
	# The version is written only for this build; nothing commits it.
	trap 'git checkout -- version.go' EXIT
	rm -rf dist && mkdir -p dist
	for target in $TARGETS; do
		os=${target%/*} arch=${target#*/}
		name="comlink-$os-$arch"
		exe=comlink
		[ "$os" = windows ] && exe=comlink.exe
		work="dist/$name"
		mkdir -p "$work"
		GOOS=$os GOARCH=$arch CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "$work/$exe" .
		if [ "$os" = windows ]; then
			(cd "$work" && zip -q "../$name.zip" "$exe")
		else
			tar -C "$work" -czf "dist/$name.tar.gz" "$exe"
		fi
		rm -rf "$work"
	done
	go run github.com/CycloneDX/cyclonedx-gomod/cmd/cyclonedx-gomod@v1.12.0 mod -licenses -json -output dist/comlink.sbom.json
	cp bin/install.sh dist/install.sh
	(cd dist && shasum -a 256 ./*.tar.gz ./*.zip comlink.sbom.json install.sh | sed 's# \./# #' > checksums.txt)
	echo "$version" > dist/VERSION
	echo "release: built $version"
}

publish() {
	version=$(cat dist/VERSION)
	gh release create "v$version" --target "$(git rev-parse HEAD)" --title "v$version" --generate-notes \
		dist/*.tar.gz dist/*.zip dist/comlink.sbom.json dist/install.sh dist/checksums.txt
	echo "release: published v$version"
}

case "${1:-all}" in
build) build ;;
publish) publish ;;
all) build && publish ;;
*)
	echo "usage: scripts/release.sh [build|publish]" >&2
	exit 2
	;;
esac
