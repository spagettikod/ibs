#!/bin/bash
# Tests room for a site's own files with four sites. Site-b is small; it holds
# copies of site-a's files, then needs the space for its own:
#   - with no other copy of site-a's files anywhere, site-a keeps them on
#     site-b and site-b's upload gets "storage is full",
#   - once site-c holds copies too, site-b pushes site-a's copies away and its
#     upload works; site-a's files stay readable,
#   - site-a's dashboard says the IBS is running out of space, and site-b
#     takes no new copies for a while (IBS_ROOM_COOLDOWN),
#   - when site-d joins, site-a's files get all their copies back.
# Needs: docker and the "ibs" image.
#
#   docker build -t ibs . && test/room/run.sh
set -u
cd "$(dirname "$0")"
. ../lib/site.sh
WORK=$(mktemp -d)
ID=ibs-room-$$
NET=$ID
cleanup() { docker rm -fv $ID-a $ID-b $ID-c $ID-d >/dev/null 2>&1; docker network rm $NET >/dev/null 2>&1; rm -rf "$WORK"; }
[ "${KEEP:-}" = 1 ] || trap cleanup EXIT

docker network create $NET >/dev/null
site() { # name alias limit [docker args...]
  local name=$1 alias=$2 limit=$3; shift 3
  docker run -d --name $name --network $NET --network-alias $alias -p 127.0.0.1::8080 \
    -e IBS_SITE_NAME=$alias -e IBS_ADMIN_PASSWORD=test-password -e IBS_RELEASES_URL=http://127.0.0.1:9/releases -e IBS_STORAGE_LIMIT=$limit -e IBS_CHECK_INTERVAL=2s \
    -e IBS_PEER_ADVERTISE=$alias:7443 "$@" ibs >/dev/null
}
site $ID-a site-a 300M
site $ID-b site-b 60M -e IBS_ROOM_COOLDOWN=45s
site $ID-c site-c 300M
site $ID-d site-d 300M
port() { docker port $1 8080/tcp | head -1 | cut -d: -f2; }
api() { # container method path [json]
  local p jar="$WORK/jar-$1"; p=$(port $1)
  grep -qs ibs_session "$jar" || curl -s -c "$jar" -H 'Content-Type: application/json' -d '{"password":"test-password"}' "http://127.0.0.1:$p/api/login" >/dev/null
  curl -s -b "$jar" -X "$2" -H 'X-IBS: 1' -H 'Content-Type: application/json' ${4:+-d "$4"} "http://127.0.0.1:$p$3"
}
wait_for() { local t=$1; shift; for _ in $(seq 1 "$t"); do "$@" && return 0; sleep 1; done; return 1; }
# upload CONTAINER NAME: uploads a 4 MB file (one block) to the bucket "files"; prints the HTTP status
upload() {
  head -c 4000000 /dev/urandom > "$WORK/$2"
  api $1 GET /api/state >/dev/null # log in
  curl -s -o /dev/null -w '%{http_code}' -b "$WORK/jar-$1" -H 'X-IBS: 1' -F "file=@$WORK/$2;filename=$2" \
    "http://127.0.0.1:$(port $1)/api/buckets/files/upload"
}
intact() { # container name
  curl -s -o "$WORK/got" -b "$WORK/jar-$1" "http://127.0.0.1:$(port $1)/api/buckets/files/download?key=$2" && cmp -s "$WORK/got" "$WORK/$2"
}
# held CONTAINER: how many blocks of site-a the site holds
held() { api $1 GET /api/peers | tr '{' '\n' | grep '"name":"site-a"' | sed -n 's/.*"held_blocks":\([0-9]*\).*/\1/p'; }
dash() { api $ID-a GET /api/dashboard; }
export -f api port wait_for held dash
export WORK ID

for s in a b c d; do check "site-$s starts" wait_for 180 bash -c "api $ID-$s GET /api/state | grep -q '\"authenticated\":true'"; done
token() { api $1 POST /api/peers/invite | sed -n 's/.*"token":"\([^"]*\)".*/\1/p'; }
join() { api $1 POST /api/peers/join "{\"token\":\"$(token $ID-a)\"}" | grep -q site_id; }
for s in a b; do api $ID-$s POST /api/buckets '{"name":"files"}' >/dev/null; done
check "site-b joins site-a"                     join $ID-b

echo "Only one copy elsewhere"
for i in 1 2 3 4 5; do upload $ID-a a$i.bin >/dev/null; done
check "site-a's 5 files are copied to site-b"   wait_for 90 bash -c '[ "$(held $ID-b)" = 5 ]'
codes=""
for i in $(seq 1 12); do codes="$codes $(upload $ID-b b$i.bin)"; done
ok=$(grep -o 200 <<<"$codes" | wc -l | tr -d ' ')
check "site-b fills up with its own files"      bash -c "[ $ok -ge 8 ]"
check "then its upload gets storage is full"    bash -c "[ $ok -lt 12 ]"
check "site-a's copies stay on site-b"          bash -c '[ "$(held $ID-b)" = 5 ]'

echo "Pushing copies away"
check "site-c joins site-a"                     join $ID-c
check "site-a's files are copied to site-c"     wait_for 90 bash -c '[ "$(held $ID-c)" = 5 ]'
codes=""
for i in 13 14 15; do codes="$codes $(upload $ID-b b$i.bin)"; done
check "site-b's upload works now"               bash -c "[ '$codes' = ' 200 200 200' ]"
check "site-b gave back site-a's copies"        bash -c "docker logs $ID-b 2>&1 | grep -q \"gave back other sites' copies\""
check "site-a released them"                    bash -c "docker logs $ID-a 2>&1 | grep -q 'released copies to a site that needs room'"
check "site-b holds fewer of them"              wait_for 30 bash -c '[ "$(held $ID-b)" -lt 5 ]'
check "site-a's files read back intact"         bash -c "$(declare -f intact); export WORK; for i in 1 2 3 4 5; do intact $ID-a a\$i.bin || exit 1; done"
check "site-a's dashboard: running out of space" wait_for 30 bash -c "dash | grep -q '\"no_room\":[1-9]'"
check "site-b takes no new copies meanwhile"    bash -c "api $ID-b GET /api/dashboard | grep -q '\"offered\":0,'"

echo "Space appears"
check "site-d joins site-a"                     join $ID-d
check "site-a's files get all copies back"      wait_for 120 bash -c "dash | grep -q '\"under_replicated\":0' && dash | grep -q '\"no_room\":0'"

finish
