#!/usr/bin/env bash
# build.sh — release build for hearth & hearth-agent (Linux/macOS/WSL).
# Windows targets go through scripts/build.ps1 (PowerShell) for native zip.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# Version: git tag wins (release builds match the tag exactly); fallback
# is internal/version (dev builds).
VERSION="$(git -C "$ROOT" describe --tags --abbrev=0 2>/dev/null | sed 's/^v//')"
[ -n "$VERSION" ] || VERSION="$(sed -n 's/.*Version = "\(.*\)"/\1/p' "$ROOT/internal/version/version.go")"
[ -n "$VERSION" ] || { echo "cannot determine version" >&2; exit 1; }
LDFLAGS="-s -w -X hearth/internal/version.Version=$VERSION"
DIST="$ROOT/dist/$VERSION"
mkdir -p "$DIST"
cd "$ROOT"

TARGETS=(
  "linux amd64"
  "linux arm64"
  "darwin amd64"
  "darwin arm64"
)

for t in "${TARGETS[@]}"; do
  set -- $t
  GOOS=$1 GOARCH=$2 CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" \
    -o "$DIST/hearth_${1}_${2}" ./cmd/hearth
  GOOS=$1 GOARCH=$2 CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" \
    -o "$DIST/hearth-agent_${1}_${2}" ./cmd/hearth-agent
  tar -czf "$DIST/hearth_${1}_${2}.tar.gz" -C "$DIST" \
    "hearth_${1}_${2}" "hearth-agent_${1}_${2}"
  rm "$DIST/hearth_${1}_${2}" "$DIST/hearth-agent_${1}_${2}"
  echo "built ${1}_${2}"
done

cd "$DIST"
sha256sum *.tar.gz > checksums.sha256
echo
ls -lh
