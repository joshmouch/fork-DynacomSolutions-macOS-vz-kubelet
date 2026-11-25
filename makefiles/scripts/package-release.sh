#!/usr/bin/env bash
# Build the release archive for the version in VERSION: dist/macos-vz-kubelet_<version>_darwin_arm64.tar.gz and its
# .sha256. ci.yml runs it on the cluster's macOS runner and uploads dist/ as the run's artifact; the release publishes
# those bytes (gh release create <version> with both files), so the archive is the one the tested commit built.
set -euo pipefail
cd "$(dirname "$0")/../.."
VERSION=$(tr -d '[:space:]' <VERSION)
ASSET="macos-vz-kubelet_${VERSION}_darwin_arm64.tar.gz"
mkdir -p dist
go build -trimpath -ldflags "-X main.buildVersion=${VERSION}" -o dist/MacOSVK ./cmd/virtual-kubelet
codesign -s - --entitlements resources/vz.entitlements dist/MacOSVK
codesign -d --entitlements - dist/MacOSVK 2>&1 | grep -q com.apple.security.virtualization
# Stable metadata keeps the archive bytes a function of the binary.
touch -t 202001010000 dist/MacOSVK
tar -C dist -cf - MacOSVK | gzip -n > "dist/${ASSET}"
(cd dist && shasum -a 256 "${ASSET}" | tee "${ASSET}.sha256")
rm dist/MacOSVK
