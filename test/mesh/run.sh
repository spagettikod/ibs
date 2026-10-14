#!/bin/bash
# Tests that an invite brings a site into the whole IBS: sites that were never
# invited to each other connect through what their peers tell them and copies
# reach them; a site that leaves is forgotten everywhere (also by sites that
# were offline) and can rejoin with a new invite.
# Needs: docker and the "ibs" and "ibs-relay" images.
#
#   docker build -t ibs . && docker build -t ibs-relay --target relay . && test/mesh/run.sh
set -u
cd "$(dirname "$0")"
. ../lib/site.sh
dc() { docker compose -p ibs-mesh "$@"; }
api() { # site method path [json]
  local jar=/tmp/jar-$1
  dc exec -T ctl sh -c "[ -s $jar ] || curl -s -c $jar -H 'Content-Type: application/json' -d '{\"password\":\"changeme123\"}' http://$1:8080/api/login >/dev/null;
    curl -s -b $jar -X $2 -H 'X-IBS: 1' -H 'Content-Type: application/json' ${4:+-d '$4'} http://$1:8080$3"
}
peer_of() { # site other: is other an online peer of site?
  api "$1" GET /api/peers | tr '{' '\n' | grep "\"name\":\"$2\"" | grep -q '"online":true'
}
not_peer_of() { ! api "$1" GET /api/peers | grep -q "\"name\":\"$2\""; }
wait_for() { # seconds command...
  local t=$1; shift
  for _ in $(seq 1 "$t"); do "$@" && return 0; sleep 1; done
  return 1
}
invite() { api "$1" POST /api/peers/invite | sed -n 's/.*"token":"\([^"]*\)".*/\1/p'; }
trap 'dc down -v --remove-orphans >/dev/null 2>&1' EXIT

dc down -v --remove-orphans >/dev/null 2>&1
dc up -d >/dev/null 2>&1 || { echo "cannot start the sites"; exit 1; }
for s in site-a site-b site-c; do
  check "$s starts"                              wait_for 180 bash -c "$(declare -f api dc); api $s GET /api/state | grep -q '\"authenticated\":true'"
done

echo "Joining"
check "site-b joins site-a with an invite"      bash -c "$(declare -f api dc); api site-b POST /api/peers/join '{\"token\":\"$(invite site-a)\"}' | grep -q site_id"
check "site-a joins site-c with an invite"      bash -c "$(declare -f api dc); api site-a POST /api/peers/join '{\"token\":\"$(invite site-c)\"}' | grep -q site_id"
check "site-b and site-c share no relay yet"    bash -c "$(declare -f api dc); ! api site-b GET /api/relays | grep -q relay-2"

echo "The IBS connects the rest"
check "site-b connects to site-c on its own"    wait_for 60 peer_of site-b site-c
check "site-c connects to site-b on its own"    wait_for 60 peer_of site-c site-b
check "site-b registered with site-c's relay"   bash -c "$(declare -f api dc); api site-b GET /api/relays | grep -q relay-2"

echo "Copies"
api site-b POST /api/buckets '{"name":"mesh"}' >/dev/null
dc exec -T ctl sh -c "head -c 300000 /dev/urandom > /tmp/data.bin && curl -s -b /tmp/jar-site-b -H 'X-IBS: 1' -F 'file=@/tmp/data.bin;filename=data.bin' http://site-b:8080/api/buckets/mesh/upload" >/dev/null
copies_done() { api site-b GET /api/dashboard | grep -q '"under_replicated":0' && api site-c GET /api/peers | tr '{' '\n' | grep '"name":"site-b"' | grep -qv '"held_blocks":0'; }
check "copies reach site-a and site-c"          wait_for 60 copies_done

echo "Leaving"
check "site-c leaves the IBS"                   bash -c "$(declare -f api dc); api site-c POST /api/ibs/leave; [ \$? -eq 0 ]"
check "site-a and site-b forget site-c"         wait_for 20 bash -c "$(declare -f api dc not_peer_of); not_peer_of site-a site-c && not_peer_of site-b site-c"
check "site-c has no sites left"                bash -c "$(declare -f api dc); [ \"\$(api site-c GET /api/peers)\" = '[]' ]"
check "site-c deleted site-b's copies"          bash -c "$(declare -f api dc); api site-c GET /api/dashboard | grep -q '\"held_bytes\":0'"
check "site-c doesn't come back on its own"     bash -c "sleep 12; $(declare -f api dc not_peer_of); not_peer_of site-a site-c && not_peer_of site-b site-c"

echo "Rejoining"
check "site-c joins site-b with a new invite"   bash -c "$(declare -f api dc); api site-c POST /api/peers/join '{\"token\":\"$(invite site-b)\"}' | grep -q site_id"
check "site-c connects to site-a again"         wait_for 60 peer_of site-c site-a
check "site-a connects to site-c again"         wait_for 60 peer_of site-a site-c

echo "Leaving while a site is offline"
dc stop site-a >/dev/null 2>&1
api site-c POST /api/ibs/leave >/dev/null
check "site-b forgets site-c"                   wait_for 20 not_peer_of site-b site-c
dc start site-a >/dev/null 2>&1
check "site-a finds out when it's back"         wait_for 60 bash -c "$(declare -f api dc not_peer_of); not_peer_of site-a site-c"
check "and site-b doesn't take site-c back"     bash -c "sleep 12; $(declare -f api dc not_peer_of); not_peer_of site-b site-c && not_peer_of site-a site-c"

finish
