#!/bin/bash
# Builds and pushes the launcher or relay image. Each is a program of its own with its
# own version (in its source) and changelog, released only when it changes:
#
#   scripts/release-image.sh launcher   ghcr.io/spagettikod/ibs/launcher, cmd/ibs-launcher/main.go
#   scripts/release-image.sh relay      ghcr.io/spagettikod/ibs/relay, internal/relay/version.go
#
# Neither gets a GitHub release: sites look up IBS versions in the repository's GitHub
# releases, which must stay IBS's.
#
# Options (environment): REGISTRY (default ghcr.io/spagettikod), PLATFORMS (default
# linux/amd64), PUSH=0 to build without pushing.
set -euo pipefail
cd "$(dirname "$0")/.."

case "${1:-}" in
  launcher) src=cmd/ibs-launcher/main.go; changelog=cmd/ibs-launcher/CHANGELOG.md; name="IBS launcher" ;;
  relay) src=internal/relay/version.go; changelog=cmd/ibs-relay/CHANGELOG.md; name="IBS relay" ;;
  *) echo "usage: scripts/release-image.sh launcher|relay" >&2; exit 1 ;;
esac
REGISTRY=${REGISTRY:-ghcr.io/spagettikod}
PLATFORMS=${PLATFORMS:-linux/amd64}
VERSION=$(sed -n 's/^var Version = "\(.*\)"$/\1/p' "$src")
[ -n "$VERSION" ] || { echo "no version in $src" >&2; exit 1; }
if [ -n "$(git status --porcelain)" ]; then
  echo "working tree is not clean" >&2; exit 1
fi
if ! grep -q "^## $VERSION\$" "$changelog"; then
  echo "$changelog has no section for $VERSION" >&2; exit 1
fi
if ! sed -n "/^## $VERSION\$/,/^## /p" "$changelog" | grep -q '^\*\*Works with:\*\*'; then
  echo "$changelog: the $VERSION section needs a \"**Works with:**\" line (which IBS versions)" >&2; exit 1
fi

OUT=--push
[ "${PUSH:-1}" = 0 ] && OUT=--load
image=$REGISTRY/ibs/$1:$VERSION
tags=(-t "$image")
[[ $VERSION != *-* ]] && tags+=(-t "${image%:*}:latest")
docker buildx build --platform "$PLATFORMS" --target "$1" "${tags[@]}" $OUT .
echo "built $image"
echo "tag it: git tag -a $1-v$VERSION -m \"$name $VERSION\" && git push origin $1-v$VERSION"
