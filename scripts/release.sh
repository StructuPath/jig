#!/usr/bin/env bash
# release.sh — cross-compile the jig binary for the platforms in use (R19).
#
# Four targets, one command: linux and darwin × amd64 and arm64. This is
# possible at all because jig's SQLite driver is pure Go (KTD3) — with a cgo
# driver, each of these would need its own C toolchain, and the release would
# stop being a thing one operator can run.
#
# Usage:
#   scripts/release.sh [version]
#
# The version defaults to `git describe`, and is stamped into the binary
# (`jig version`) so a binary found on a machine can name the source it came
# from. Output lands in dist/ as one .tar.gz per platform plus SHA256SUMS.
#
# Deliberately NOT here, per the plan's scope boundary: double-build
# reproducibility verification and tag-provenance checks. Those are
# supply-chain rigor for a tool with consumers beyond its builder, and
# pretending to them with a single build would be worse than their absence.
set -euo pipefail

cd "$(dirname "$0")/.."

version="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
dist="${DIST_DIR:-dist}"

rm -rf "$dist"
mkdir -p "$dist"

for platform in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
    os="${platform%/*}"
    arch="${platform#*/}"
    name="jig_${version}_${os}_${arch}"
    stage="$dist/$name"
    mkdir -p "$stage"

    # CGO_ENABLED=0: no C toolchain, no host libc pinned into the artifact.
    # -trimpath: the builder's absolute paths are not part of the product.
    echo "building $name"
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build \
        -trimpath \
        -ldflags "-s -w -X main.version=${version}" \
        -o "$stage/jig" \
        ./cmd/jig

    tar -czf "$dist/$name.tar.gz" -C "$stage" jig
    rm -rf "$stage"
done

# Checksums, under whichever tool this platform ships.
(
    cd "$dist"
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum ./*.tar.gz >SHA256SUMS
    else
        shasum -a 256 ./*.tar.gz >SHA256SUMS
    fi
)

echo
echo "release artifacts in $dist:"
ls -1 "$dist"
