#!/bin/bash
# Tests updates end to end with the launcher image: two sites and a storage
# node start from a signed 1.8.0, a stand-in for GitHub offers 1.9.0, and:
#   - site A downloads 1.9.0 from "GitHub"; site B (GitHub off) fetches it from A
#   - A installs it, restarts into it, and its storage node follows
#   - A rolls back; untrusted releases are refused; a version that crashes on
#     start is undone by the launcher
# Releases are signed with a throwaway key. Needs: docker, go, curl, openssl.
#
#   test/updates/run.sh
set -u
cd "$(dirname "$0")"
. ../lib/site.sh
ROOT=$(cd ../.. && pwd)
WORK=$(mktemp -d)
ID=ibs-upd-$$
NET=$ID
cleanup() { docker rm -fv $ID-a $ID-b $ID-node $ID-bad $ID-gh $ID-c $ID-d >/dev/null 2>&1; docker network rm $NET >/dev/null 2>&1; rm -rf "$WORK"; }
[ "${KEEP:-}" = 1 ] || trap cleanup EXIT

LAUNCHER_VERSION=$(sed -n 's/^var Version = "\(.*\)"$/\1/p' "$ROOT/cmd/ibs-launcher/main.go")
ARCH=$(docker version --format '{{.Server.Arch}}')
echo "building releases for linux/$ARCH"
mkdir -p "$WORK/rel" "$WORK/gh"
(cd "$ROOT/web" && npm run build >/dev/null 2>&1)
openssl genpkey -algorithm ed25519 -out "$WORK/test.key" || exit 1
openssl genpkey -algorithm ed25519 -out "$WORK/other.key" || exit 1
go build -o "$WORK/ibs-release" "$ROOT/cmd/ibs-release" || exit 1
pubkey() { openssl pkey -in "$1" -pubout | "$WORK/ibs-release" pubkey; }
sign() { # key version program out: a release signed with key, as release.sh makes them
  "$WORK/ibs-release" manifest -version "$2" -platform "linux/$ARCH" "$3" >"$WORK/m.json" &&
  openssl pkeyutl -sign -rawin -inkey "$1" -in "$WORK/m.json" -out "$WORK/m.sig" &&
  "$WORK/ibs-release" pack -trust "$(pubkey "$1")" -o "$4" "$3" "$WORK/m.json" "$WORK/m.sig" >/dev/null
}
KEY=$(pubkey "$WORK/test.key")
release() { # version key file
  (cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH go build -trimpath -ldflags "-s -w -X ibs/internal/version.Version=$1" -o "$WORK/ibs-$1" ./cmd/ibs) &&
  sign "$2" "$1" "$WORK/ibs-$1" "$3"
}
release 1.8.0 "$WORK/test.key" "$WORK/rel/ibs-1.8.0.ibsrelease" || exit 1
release 1.9.0 "$WORK/test.key" "$WORK/gh/ibs-1.9.0-linux-$ARCH.ibsrelease" || exit 1
release 1.9.5-beta1 "$WORK/test.key" "$WORK/gh/ibs-1.9.5-beta1-linux-$ARCH.ibsrelease" || exit 1
release 1.9.1 "$WORK/other.key" "$WORK/rel/untrusted.ibsrelease" || exit 1
# 2.0.0 is a program that stops right away.
printf '#!/bin/sh\necho "2.0.0 is broken"; exit 3\n' >"$WORK/broken"
sign "$WORK/test.key" 2.0.0 "$WORK/broken" "$WORK/rel/broken.ibsrelease"
# Sites take 1.9.0 and leave the newer beta alone, unless betas are on.
printf '[{"tag_name":"v1.9.5-beta1","prerelease":true,"assets":[{"name":"ibs-1.9.5-beta1-linux-%s.ibsrelease","browser_download_url":"http://gh/ibs-1.9.5-beta1-linux-%s.ibsrelease"}]},
{"tag_name":"v1.9.0","assets":[{"name":"ibs-1.9.0-linux-%s.ibsrelease","browser_download_url":"http://gh/ibs-1.9.0-linux-%s.ibsrelease"}]}]' "$ARCH" "$ARCH" "$ARCH" "$ARCH" >"$WORK/gh/latest"
chmod -R a+rX "$WORK"
docker build -q -t ibs-launcher --target launcher "$ROOT" >/dev/null || exit 1

docker network create $NET >/dev/null
docker run -d --name $ID-gh --network $NET --network-alias gh -v "$WORK/gh:/www:ro" busybox:1.36 httpd -f -p 80 -h /www >/dev/null
site() { # name alias [docker args...]
  local name=$1 alias=$2; shift 2
  docker run -d --name $name --network $NET --network-alias $alias -p 127.0.0.1::8080 -v "$WORK/rel:/rel:ro" \
    -e IBS_SITE_NAME=$alias -e IBS_ADMIN_PASSWORD=test-password -e IBS_STORAGE_LIMIT=200M -e IBS_CHECK_INTERVAL=2s \
    -e IBS_PEER_ADVERTISE=$alias:7443 -e IBS_BINARY=/rel/ibs-1.8.0.ibsrelease -e IBS_TRUSTED_KEYS="$KEY" \
    -e IBS_PROBATION=8s -e IBS_RELEASES_URL=http://gh/latest "$@" ibs-launcher >/dev/null
}
site $ID-a site-a
site $ID-b site-b -e IBS_RELEASES_URL=http://nowhere.invalid/latest
port() { docker port $1 8080/tcp | head -1 | cut -d: -f2; }
api() { # container method path [json]
  local p jar="$WORK/jar-$1"; p=$(port $1)
  [ -s "$jar" ] || curl -s -c "$jar" -H 'Content-Type: application/json' -d '{"password":"test-password"}' "http://127.0.0.1:$p/api/login" >/dev/null
  curl -s -b "$jar" -X "$2" -H 'X-IBS: 1' -H 'Content-Type: application/json' ${4:+-d "$4"} "http://127.0.0.1:$p$3"
}
upload() { local p; p=$(port $1); curl -s -b "$WORK/jar-$1" -H 'X-IBS: 1' -F "file=@$2" "http://127.0.0.1:$p/api/updates/upload"; }
running() { curl -s "http://127.0.0.1:$(port $1)/api/state" | grep -q "\"version\":\"$2\""; }
wait_for() { local t=$1; shift; for _ in $(seq 1 "$t"); do "$@" && return 0; sleep 1; done; return 1; }
export -f api port running wait_for upload
export WORK

echo "Starting"
check "site-a runs 1.8.0 from IBS_BINARY"        wait_for 60 running $ID-a 1.8.0
check "site-b runs 1.8.0"                        wait_for 60 running $ID-b 1.8.0
api $ID-b PUT /api/updates/settings '{"github":false}' >/dev/null
TOKEN=$(api $ID-a GET /api/nodes/join-token | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
# The node has no release settings: it gets IBS from its manager.
docker run -d --name $ID-node --network $NET -e IBS_MANAGER=site-a:7443 -e IBS_NODE_ADVERTISE=$ID-node:7480 \
  -e IBS_STORAGE_LIMIT=200M -e IBS_PROBATION=8s -e IBS_CHECK_INTERVAL=2s \
  ibs-launcher node "$TOKEN" >/dev/null
# Same manager key, wrong join secret.
BAD=$(python3 -c 'import base64,json,sys; t=sys.argv[1]; p,b=t.split(".",1); j=json.loads(base64.urlsafe_b64decode(b+"=="*2)); j["s"]="wrong"; print(p+"."+base64.urlsafe_b64encode(json.dumps(j).encode()).decode().rstrip("="))' "$TOKEN")
docker run -d --name $ID-bad --network $NET -e IBS_MANAGER=site-a:7443 ibs-launcher node "$BAD" >/dev/null
node_runs() { api $ID-a GET /api/nodes | tr '{' '\n' | grep -v '"local":true' | grep -q "\"version\":\"$1\""; }
export -f node_runs; export ID
check "the storage node joins, running 1.8.0"    wait_for 60 node_runs 1.8.0
check "it got IBS from its manager"              bash -c "docker logs $ID-node 2>&1 | grep -q 'got IBS from the manager.*version=1.8.0'"
check "a wrong join secret gets nothing"         wait_for 20 bash -c "docker logs $ID-bad 2>&1 | grep -q 'join token is not valid'"
check "and that node never tries GitHub"         bash -c "! docker logs $ID-bad 2>&1 | grep -qi 'latest release'"
check "site-b joins site-a"                      bash -c "api $ID-b POST /api/peers/join \"{\\\"token\\\":\\\"\$(api $ID-a POST /api/peers/invite | sed -n 's/.*\"token\":\"\\([^\"]*\\)\".*/\\1/p')\\\"}\" | grep -q site_id"

echo "Finding new versions"
check "site-a downloads 1.9.0 from GitHub"       wait_for 90 bash -c "api $ID-a GET /api/updates | grep -q '\"version\":\"1.9.0\"'"
check "site-b (GitHub off) gets it from site-a"  wait_for 120 bash -c "api $ID-b GET /api/updates | grep -q '\"version\":\"1.9.0\"'"
check "both say 1.9.0 is available"              bash -c "api $ID-a GET /api/updates | grep -q '\"newer\":\"1.9.0\"' && api $ID-b GET /api/updates | grep -q '\"newer\":\"1.9.0\"'"
check "the beta is left alone by default"       bash -c "! api $ID-a GET /api/updates | grep -q '1.9.5-beta1'"
check "nothing installs on its own"              bash -c "running $ID-a 1.8.0 && running $ID-b 1.8.0"

echo "Updating"
api $ID-a POST /api/updates/install '{"version":"1.9.0"}' >/dev/null
check "site-a restarts into 1.9.0"               wait_for 60 running $ID-a 1.9.0
check "its storage node follows"                 wait_for 90 node_runs 1.9.0
check "site-b sees that site-a runs 1.9.0"       wait_for 30 bash -c "api $ID-b GET /api/updates | tr '{' '\\n' | grep '\"version\":\"1.9.0\"' | grep -q '\"sites\":\\[\"site-a\"\\]'"
check "site-b still runs 1.8.0"                  running $ID-b 1.8.0

echo "Rolling back"
sleep 9 # let 1.9.0 count as good
api $ID-a POST /api/updates/install '{"version":"1.8.0"}' >/dev/null
check "site-a rolls back to 1.8.0"               wait_for 60 running $ID-a 1.8.0
check "its storage node follows back"            wait_for 90 node_runs 1.8.0

echo "Untrusted releases"
check "a release signed by another key is refused" bash -c "upload $ID-a $WORK/rel/untrusted.ibsrelease | grep -q 'not signed by a trusted key'"
OTHER=$(pubkey "$WORK/other.key")
check "after adding that key it is accepted"     bash -c "api $ID-a POST /api/updates/keys '{\"key\":\"$OTHER\",\"name\":\"other\"}' >/dev/null && upload $ID-a $WORK/rel/untrusted.ibsrelease | grep -q '\"version\":\"1.9.1\"'"
OTHER_ID=$(api $ID-a GET /api/updates | grep -o '"id":"[0-9a-f]*","name":"other"' | sed 's/"id":"\([0-9a-f]*\)".*/\1/')
check "its key can't be removed while 1.9.1 is installed" bash -c "api $ID-a DELETE /api/updates/keys/$OTHER_ID | grep -q 'remove them first'"
check "after removing 1.9.1, the key can be removed" bash -c "api $ID-a DELETE /api/updates/versions/1.9.1 >/dev/null && ! api $ID-a DELETE /api/updates/keys/$OTHER_ID | grep -q error && ! api $ID-a GET /api/updates | grep -q '\"name\":\"other\"'"
TEST_ID=$(api $ID-a GET /api/updates | grep -o '"id":"[0-9a-f]*","name":"From IBS_TRUSTED_KEYS"' | sed 's/"id":"\([0-9a-f]*\)".*/\1/')
check "a key from IBS_TRUSTED_KEYS can't be removed here" bash -c "[ -n '$TEST_ID' ] && api $ID-a DELETE /api/updates/keys/$TEST_ID | grep -q IBS_TRUSTED_KEYS"
check "the launcher's own version is shown"       bash -c "api $ID-a GET /api/updates | grep -q '\"launcher_version\":\"$LAUNCHER_VERSION\"'"
check "the built-in key can't be removed"        bash -c "api $ID-a DELETE /api/updates/keys/\$(api $ID-a GET /api/updates | grep -o '\"id\":\"[0-9a-f]*\",\"name\":\"[^\"]*\",\"key\":\"[^\"]*\",\"default\":true' | sed -n 's/\"id\":\"\\([0-9a-f]*\\)\".*/\\1/p') | grep -q \"can't be removed\""

echo "A version that crashes"
sleep 9 # let 1.8.0 count as good again
upload $ID-a "$WORK/rel/broken.ibsrelease" >/dev/null
api $ID-a POST /api/updates/install '{"version":"2.0.0"}' >/dev/null
check "the launcher goes back to 1.8.0"          wait_for 60 bash -c "sleep 2; running $ID-a 1.8.0"
check "2.0.0 is marked as failed"                wait_for 10 bash -c "api $ID-a GET /api/updates | tr '{' '\\n' | grep '\"version\":\"2.0.0\"' | grep -q '\"bad\":true'"
check "the launcher logged it"                   bash -c "docker logs $ID-a 2>&1 | grep -q 'stopped during its first minutes'"

echo "Betas"
api $ID-a PUT /api/updates/settings '{"betas":true}' >/dev/null
check "with betas on, site-a downloads the beta" wait_for 30 bash -c "api $ID-a GET /api/updates | grep -q '\"newer\":\"1.9.5-beta1\"'"
sleep 30 # more than a round of fetching between sites (12 check intervals)
check "site-b (betas off) doesn't fetch it"     bash -c "! api $ID-b GET /api/updates | grep -q '1.9.5-beta1'"
site $ID-c site-c -e IBS_BINARY=
check "a new launcher downloads the latest stable release" wait_for 60 running $ID-c 1.9.0
site $ID-d site-d -e IBS_BINARY= -e IBS_BETA=true
check "with IBS_BETA=true, the latest beta"      wait_for 60 running $ID-d 1.9.5-beta1
check "and betas count as new versions there"   bash -c "api $ID-d GET /api/updates | grep -q '\"betas\":true'"

finish
