#!/bin/bash
# Builds an IBS release: Linux binaries, signed releases and checksums. The launcher
# and relay images are released on their own, when they change: scripts/release-image.sh.
#
#   scripts/release.sh 1.2.0
#
# Options (environment):
#   ARCHES         binary architectures (default amd64)
#   IBS_SIGNING_KEY the release key, an Ed25519 key in OpenSSL's PEM format
#                  (default ~/.config/ibs/signing.key)
#   GITHUB_TOKEN   if set, create the GitHub release for tag v<version> and upload
#                  the binaries (the tag must already be pushed)
#   RELEASE_ONLY=1 skip building; only create the GitHub release from dist/<version>
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION=${1:?usage: scripts/release.sh <version>}
ARCHES=${ARCHES:-amd64}
REPO_API=https://api.github.com/repos/spagettikod/ibs
DIST=dist/$VERSION
SIGNING_KEY=${IBS_SIGNING_KEY:-$HOME/.config/ibs/signing.key}

# The notes must say what to update and which launcher and relay this works with.
notes=$(sed -n "/^## $VERSION\$/,/^## /p" CHANGELOG.md)
for line in "What to update:" "Works with:"; do
  if ! grep -q "^\*\*$line\*\*" <<<"$notes"; then
    echo "CHANGELOG.md: the $VERSION section needs a \"**$line**\" line" >&2; exit 1
  fi
done

if [ "${RELEASE_ONLY:-0}" != 1 ]; then
if [ -n "$(git status --porcelain)" ]; then
  echo "working tree is not clean" >&2; exit 1
fi

# The launcher and the relay are released on their own; remind when one changed since
# its last release. The launcher is checked against every package it is built from.
launcher_paths=$(go list -deps ./cmd/ibs-launcher | sed -n 's#^ibs/##p' | tr '\n' ' ')
for part in "launcher:$launcher_paths" "relay:cmd/ibs-relay internal/relay internal/keys :!internal/relay/client.go"; do
  last=$(git describe --tags --abbrev=0 --match "${part%%:*}-v*" 2>/dev/null || true)
  if [ -n "$last" ] && ! git diff --quiet "$last" HEAD -- ${part#*:}; then
    echo "note: the ${part%%:*} changed since $last; raise its version and run scripts/release-image.sh ${part%%:*}" >&2
  fi
done

echo "== UI"
(cd web && npm ci --no-audit --no-fund --silent && npm run build --silent)

# Plain archives owned by root, without macOS metadata when built on a Mac.
if tar --version | grep -q bsdtar; then
  TAR_FLAGS=(--no-xattrs --no-mac-metadata --uid 0 --gid 0 --uname root --gname root)
  export COPYFILE_DISABLE=1
else
  TAR_FLAGS=(--owner=0 --group=0 --numeric-owner)
fi

echo "== Binaries"
rm -rf "$DIST" && mkdir -p "$DIST"
LDFLAGS="-s -w -X ibs/internal/version.Version=$VERSION"
for arch in $ARCHES; do
  name=ibs-$VERSION-linux-$arch
  stage=$DIST/$name
  mkdir -p "$stage/systemd"
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags "$LDFLAGS" -o "$stage/ibs" ./cmd/ibs
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags "$LDFLAGS" -o "$stage/ibs-relay" ./cmd/ibs-relay
  # The signed release the launcher installs and sites pass on. OpenSSL signs the
  # manifest with the release key ($IBS_SIGNING_KEY, by default
  # ~/.config/ibs/signing.key); ibs-release never sees the key, and refuses a
  # signature the built-in key doesn't verify.
  sig=$DIST/sign-$arch && mkdir -p "$sig"
  # The release notes travel inside the signed release, so every site can show them.
  sed -n "/^## $VERSION\$/,/^## /p" CHANGELOG.md | sed '1d;$d' >"$sig/notes.md"
  # REQUIRES_AGE=1: the version reads only age; sites install it once converted.
  go run ./cmd/ibs-release manifest -version "$VERSION" -platform "linux/$arch" -notes "$sig/notes.md" ${REQUIRES_AGE:+-requires-age} "$stage/ibs" >"$sig/manifest.json"
  openssl pkeyutl -sign -rawin -inkey "$SIGNING_KEY" -in "$sig/manifest.json" -out "$sig/manifest.sig"
  go run ./cmd/ibs-release pack -o "$DIST/$name.ibsrelease" "$stage/ibs" "$sig/manifest.json" "$sig/manifest.sig"
  rm -rf "$sig"
  cp README.md LICENSE "$stage/"
  cp contrib/systemd/* "$stage/systemd/"
  tar -C "$DIST" -czf "$DIST/$name.tar.gz" "${TAR_FLAGS[@]}" "$name"
  rm -rf "$stage"
done
(cd "$DIST" && shasum -a 256 ./*.tar.gz ./*.ibsrelease | sed 's# \./# #' > SHA256SUMS)
cat "$DIST/SHA256SUMS"

fi

if [ -n "${GITHUB_TOKEN:-}" ]; then
  echo "== GitHub release v$VERSION"
  auth="Authorization: Bearer $GITHUB_TOKEN"
  notes=$(sed -n "/^## $VERSION/,/^## /p" CHANGELOG.md | sed '1d;$d')
  body=$(python3 -c 'import json,sys; print(json.dumps({"tag_name": sys.argv[1], "name": "IBS " + sys.argv[2], "body": sys.argv[3], "prerelease": "-" in sys.argv[2]}))' "v$VERSION" "$VERSION" "$notes")
  res=$(curl -s -w '\n%{http_code}' -H "$auth" -H 'Accept: application/vnd.github+json' -d "$body" "$REPO_API/releases")
  status=${res##*$'\n'}
  if [ "$status" != 201 ]; then
    echo "GitHub refused to create the release (HTTP $status):" >&2
    echo "${res%$'\n'*}" | python3 -c 'import json,sys; r=json.load(sys.stdin); print(" ", r.get("message", ""), *[e.get("code", "") + " " + e.get("field", "") for e in r.get("errors", [])])' >&2
    [ "$status" = 422 ] && echo "  If v$VERSION already exists, it is published already; delete it on GitHub to recreate it." >&2
    exit 1
  fi
  upload=$(echo "${res%$'\n'*}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["upload_url"].split("{")[0])')
  for f in "$DIST"/*.tar.gz "$DIST"/*.ibsrelease "$DIST/SHA256SUMS"; do
    curl -sf -H "$auth" -H 'Content-Type: application/octet-stream' --data-binary "@$f" "$upload?name=$(basename "$f")" >/dev/null
    echo "uploaded $(basename "$f")"
  done
fi
echo "done: $DIST"
