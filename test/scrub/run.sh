#!/bin/bash
# Tests bit-rot protection with three sites: blocks are damaged on disk, and
#   - reading a damaged block repairs it from another site,
#   - a scrub finds and repairs damaged blocks,
#   - a scrub checks the copies on other sites too, by a challenge only the
#     exact bytes answer, and has damaged ones copied again,
#   - a block with no intact copy left is reported with the file using it, and
#     downloading that file fails instead of returning damaged data,
#   - a scrub can be paused, resumed and stopped.
# Needs: docker and the "ibs" image.
#
#   docker build -t ibs . && test/scrub/run.sh
set -u
cd "$(dirname "$0")"
. ../lib/site.sh
WORK=$(mktemp -d)
ID=ibs-scrub-$$
NET=$ID
cleanup() { docker rm -fv $ID-a $ID-b $ID-c >/dev/null 2>&1; docker network rm $NET >/dev/null 2>&1; rm -rf "$WORK"; }
[ "${KEEP:-}" = 1 ] || trap cleanup EXIT

docker network create $NET >/dev/null
site() { # name alias [docker args...]
  local name=$1 alias=$2; shift 2
  docker run -d --name $name --network $NET --network-alias $alias -p 127.0.0.1::8080 \
    -e IBS_SITE_NAME=$alias -e IBS_ADMIN_PASSWORD=test-password -e IBS_STORAGE_LIMIT=200M -e IBS_CHECK_INTERVAL=2s \
    -e IBS_PEER_ADVERTISE=$alias:7443 "$@" ibs >/dev/null
}
site $ID-a site-a
site $ID-b site-b
site $ID-c site-c -e IBS_SCRUB_RATE=1M # slow with its own 50 MB, to pause and stop it
port() { docker port $1 8080/tcp | head -1 | cut -d: -f2; }
api() { # container method path [json]
  local p jar="$WORK/jar-$1"; p=$(port $1)
  [ -s "$jar" ] || curl -s -c "$jar" -H 'Content-Type: application/json' -d '{"password":"test-password"}' "http://127.0.0.1:$p/api/login" >/dev/null
  curl -s -b "$jar" -X "$2" -H 'X-IBS: 1' -H 'Content-Type: application/json' ${4:+-d "$4"} "http://127.0.0.1:$p$3"
}
wait_for() { local t=$1; shift; for _ in $(seq 1 "$t"); do "$@" && return 0; sleep 1; done; return 1; }
download() { curl -s -o "$WORK/got" -w '%{http_code}' -b "$WORK/jar-$ID-a" "http://127.0.0.1:$(port $ID-a)/api/buckets/rot/download?key=data.bin"; }
intact() { [ "$(download)" = 200 ] && cmp -s "$WORK/got" "$WORK/data.bin"; }
# damage CONTAINER WHERE: overwrite the block files: own (the site's blocks) or held (kept for others)
damage() {
  local filter='-path */own/*'; [ "$2" = held ] && filter='! -path */own/*'
  docker exec $1 sh -c "find /data -type f -path '*/blocks/*' $filter | while read f; do printf rot > \"\$f\"; done"
}
scrub_done() { api $1 GET /api/scrub | grep -q '"current":null' && api $1 GET /api/scrub | grep -q '"last":{'; }
export -f api port wait_for download intact damage scrub_done
export WORK ID

for s in a b c; do check "site-$s starts" wait_for 180 bash -c "api $ID-$s GET /api/state | grep -q '\"authenticated\":true'"; done
token() { api $1 POST /api/peers/invite | sed -n 's/.*"token":"\([^"]*\)".*/\1/p'; }
check "site-b joins site-a"                    bash -c "api $ID-b POST /api/peers/join '{\"token\":\"$(token $ID-a)\"}' | grep -q site_id"
check "site-c joins site-a"                    bash -c "api $ID-c POST /api/peers/join '{\"token\":\"$(token $ID-a)\"}' | grep -q site_id"
api $ID-a POST /api/buckets '{"name":"rot"}' >/dev/null
head -c 300000 /dev/urandom > "$WORK/data.bin"
curl -s -b "$WORK/jar-$ID-a" -H 'X-IBS: 1' -F "file=@$WORK/data.bin;filename=data.bin" "http://127.0.0.1:$(port $ID-a)/api/buckets/rot/upload" >/dev/null
held_by() { api $1 GET /api/peers | tr '{' '\n' | grep '"name":"site-a"' | grep -qv '"held_blocks":0'; }
export -f held_by
check "the file is copied to site-b and site-c" wait_for 90 bash -c "held_by $ID-b && held_by $ID-c && api $ID-a GET /api/dashboard | grep -q '\"under_replicated\":0'"

echo "Reading"
damage $ID-a own
check "a damaged block is repaired when read"  intact
check "from another site's copy"               bash -c "docker logs $ID-a 2>&1 | grep -q 'healed block from replica'"

echo "Scrubbing"
damage $ID-a own
check "a scrub starts by hand"                 bash -c "api $ID-a POST /api/scrub/start | grep -q '\"state\":\"running\"\|\"last\":{'"
check "and finishes"                           wait_for 60 scrub_done $ID-a
check "it repaired the damaged block"          bash -c "api $ID-a GET /api/scrub | grep -q '\"what\":\"block\",\"block_id\":\"[0-9a-f]*\",\"place\":\"[^\"]*\",\"problem\":\"damaged\",\"outcome\":\"repaired\"'"
check "the file reads back intact"             intact
check "nothing is left unrepaired"             bash -c "api $ID-a GET /api/scrub | grep -q '\"unresolved\":0'"

echo "Copies on other sites"
damage $ID-b held
api $ID-a POST /api/scrub/start >/dev/null
check "site-a's scrub finishes"                wait_for 60 scrub_done $ID-a
check "it checked the copies on site-b and site-c" bash -c "api $ID-a GET /api/scrub | grep -q '\"last\":{[^}]*\"copies\":[1-9]'"
check "it finds the damaged copy on site-b"    bash -c "api $ID-a GET /api/scrub | grep -q '\"what\":\"remote\",\"block_id\":\"[0-9a-f]*\",\"place\":\"site-b\",\"problem\":\"damaged\",\"outcome\":\"replaced\"'"
check "and shows it under copies on other sites" bash -c "api $ID-a GET /api/scrub/copies | grep -q '\"name\":\"site-b\",\"copies\":[0-9]*,\"scrubbed\":[1-9][0-9]*,\"checked\":[0-9]*,\"bad\":1'"
check "site-c's copies all passed the challenge" bash -c "api $ID-a GET /api/scrub/copies | grep -q '\"name\":\"site-c\",\"copies\":[0-9]*,\"scrubbed\":[1-9][0-9]*,\"checked\":[1-9][0-9]*,\"bad\":0'"
check "site-b gets a new copy"                 wait_for 60 bash -c "held_by $ID-b && api $ID-a GET /api/dashboard | grep -q '\"under_replicated\":0'"
check "site-b's own scrub leaves fresh copies to their owner" bash -c "api $ID-b POST /api/scrub/start >/dev/null; sleep 1; api $ID-b GET /api/scrub | grep -q '\"total\":0'"

echo "Controls"
api $ID-c POST /api/buckets '{"name":"big"}' >/dev/null
head -c 50000000 /dev/urandom > "$WORK/big.bin"
curl -s -b "$WORK/jar-$ID-c" -H 'X-IBS: 1' -F "file=@$WORK/big.bin;filename=big.bin" "http://127.0.0.1:$(port $ID-c)/api/buckets/big/upload" >/dev/null
api $ID-c POST /api/scrub/start >/dev/null
check "site-c's scrub runs"                    bash -c "api $ID-c GET /api/scrub | grep -q '\"state\":\"running\"'"
check "it pauses"                              bash -c "api $ID-c POST /api/scrub/pause | grep -q '\"state\":\"paused\"'"
check "it resumes"                             bash -c "api $ID-c POST /api/scrub/resume | grep -q '\"state\":\"running\"'"
check "it stops"                               bash -c "api $ID-c POST /api/scrub/stop | grep -q '\"current\":null,\"last\":{[^}]*\"state\":\"stopped\"'"

echo "No intact copy left"
check "site-b's scrub finishes"                wait_for 60 scrub_done $ID-b
damage $ID-a own
damage $ID-b held
damage $ID-c held
api $ID-a POST /api/scrub/start >/dev/null
check "the scrub finishes"                     wait_for 90 scrub_done $ID-a
check "it reports the block as not repaired"   bash -c "api $ID-a GET /api/scrub | grep -q '\"outcome\":\"failed\"'"
check "naming the file that uses it"           bash -c "api $ID-a GET /api/scrub | grep -q 'rot/data.bin'"
check "and counts it as unresolved"            bash -c "api $ID-a GET /api/scrub | grep -q '\"unresolved\":1'"
check "downloading the file fails"             bash -c "[ \"\$(download)\" != 200 ]"

check "the schedule can be changed"            bash -c "api $ID-a PUT /api/scrub/settings '{\"interval_days\":7}' | grep -q '\"interval_days\":7'"
check "a monthly scrub is scheduled by default" bash -c "api $ID-b GET /api/scrub | grep -q '\"interval_days\":30,\"next_due\":[0-9]'"

finish
