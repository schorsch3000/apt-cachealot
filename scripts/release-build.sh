#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
# Builds the release archives and a SHA256SUMS file.
# usage: scripts/release-build.sh <version, e.g. v1.2.3> [outdir, default dist]
set -euo pipefail
version=${1:?usage: release-build.sh vX.Y.Z [outdir]}
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
out=$(realpath -m "${2:-dist}")
rm -rf "$out"
mkdir -p "$out"
# same bytes for the same commit: fixed file times taken from the commit, no gzip name/time
mtime=$(git log -1 --format=%ct 2>/dev/null || echo 0)

for target in linux/amd64 linux/arm64 linux/arm/7; do
  IFS=/ read -r goos goarch goarm <<<"$target"
  name="apt-cachealot_${version}_${goos}_${goarch}${goarm:+v$goarm}"
  work=$(mktemp -d)
  mkdir "$work/$name"
  CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch GOARM=${goarm:-} \
    go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$work/$name/apt-cachealot" ./cmd/apt-cachealot
  cp LICENSE THIRD_PARTY_LICENSES README.md "$work/$name/"
  tar -C "$work" --sort=name --mtime="@$mtime" --owner=0 --group=0 --numeric-owner -cf - "$name" | gzip -9n > "$out/$name.tar.gz"
  rm -rf "$work"
  echo "built $name.tar.gz"
done
(cd "$out" && sha256sum ./*.tar.gz | sed 's|\./||' > SHA256SUMS)
