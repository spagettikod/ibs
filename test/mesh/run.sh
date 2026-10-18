#!/bin/bash
# Tests that an invite brings a site into the whole IBS: sites that were never
# invited to each other connect through what their peers tell them and copies
# reach them; a site that leaves is forgotten everywhere (also by sites that
# were offline) and can rejoin with a new invite.
# Relays added or removed on one site are added or removed on the others.
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
[ "${KEEP:-}" = 1 ] || trap 'dc down -v --remove-orphans >/dev/null 2>&1' EXIT

dc down -v --remove-orphans >/dev/null 2>&1
dc up -d >/dev/null 2>&1 || { echo "cannot start the sites"; exit 1; }
for s in site-a site-b site-c; do
  check "$s starts"                              wait_for 180 bash -c "$(declare -f api dc); api $s GET /api/state | grep -q '\"authenticated\":true'" ||
    { echo "        $s's log:"; dc logs --tail 15 "$s" 2>&1 | sed 's/^/        /'; }
done

echo "Joining"
check "site-b joins site-a with an invite"      bash -c "$(declare -f api dc); api site-b POST /api/peers/join '{\"token\":\"$(invite site-a)\"}' | grep -q site_id"
check "site-a joins site-c with an invite"      bash -c "$(declare -f api dc); api site-a POST /api/peers/join '{\"token\":\"$(invite site-c)\"}' | grep -q site_id"

echo "The IBS connects the rest"
check "site-b connects to site-c on its own"    wait_for 60 peer_of site-b site-c
check "site-c connects to site-b on its own"    wait_for 60 peer_of site-c site-b
check "site-b registered with site-c's relay"   wait_for 30 bash -c "$(declare -f api dc); api site-b GET /api/relays | grep -q relay-2"

echo "Copies"
api site-b POST /api/buckets '{"name":"mesh"}' >/dev/null
dc exec -T ctl sh -c "head -c 300000 /dev/urandom > /tmp/data.bin && curl -s -b /tmp/jar-site-b -H 'X-IBS: 1' -F 'file=@/tmp/data.bin;filename=data.bin' http://site-b:8080/api/buckets/mesh/upload" >/dev/null
copies_done() { api site-b GET /api/dashboard | grep -q '"under_replicated":0' && api site-c GET /api/peers | tr '{' '\n' | grep '"name":"site-b"' | grep -qv '"held_blocks":0'; }
check "copies reach site-a and site-c"          wait_for 60 copies_done

echo "A relay moves"
# relay-1 gets another address: only its new name reaches it now.
# A restart drops the connections to it, as a real move to a new IP would.
docker network disconnect ibs-mesh_net ibs-mesh-relay-1-1 >/dev/null &&
  docker network connect --alias relay-1-moved --alias relay-1-renamed ibs-mesh_net ibs-mesh-relay-1-1 >/dev/null &&
  docker restart ibs-mesh-relay-1-1 >/dev/null
R1=$(api site-a GET /api/relays | tr '{' '\n' | grep '"address":"relay-1:7444"' | sed -n 's/.*"id":"\([0-9a-f]*\)".*/\1/p')
api site-a PATCH /api/relays/$R1 '{"address":"relay-1-moved:7444"}' >/dev/null
check "site-a reaches it at its new address"    wait_for 30 bash -c "$(declare -f api dc); api site-a GET /api/relays | tr '{' '\\n' | grep '\"address\":\"relay-1-moved:7444\"' | grep -q '\"connected\":true'"
relay_moved() { api "$1" GET /api/relays | tr '{' '\n' | grep '"address":"relay-1-moved:7444"' | grep -q '"connected":true'; }
check "site-b, also on it, follows on its own" wait_for 90 bash -c "$(declare -f api dc relay_moved); relay_moved site-b"

echo "A relay is renamed"
# Both names reach relay-1: site-b's still works, yet it takes the newer one.
api site-a PATCH /api/relays/$R1 '{"address":"relay-1-renamed:7444"}' >/dev/null
relay_renamed() { api "$1" GET /api/relays | tr '{' '\n' | grep '"address":"relay-1-renamed:7444"' | grep -q '"connected":true'; }
check "site-a uses the new name"                wait_for 30 bash -c "$(declare -f api dc relay_renamed); relay_renamed site-a"
check "site-b takes it although the old works"  wait_for 90 bash -c "$(declare -f api dc relay_renamed); relay_renamed site-b"
check "site-b says where it came from"          bash -c "$(declare -f dc); dc logs site-b 2>&1 | grep -q 'relay has a new address, set on another site'"

echo "Relays spread"
has_relay() { api "$1" GET /api/relays | grep -q "\"address\":\"$2"; }
check "site-c, on relay-2, uses relay-1 too"   wait_for 60 bash -c "$(declare -f api dc has_relay); has_relay site-c relay-1"
T3=$(dc exec -T relay-3 cat /share/token)
api site-a POST /api/relays "{\"address\":\"relay-3:7444\",\"token\":\"$T3\"}" >/dev/null
relay3_up() { api "$1" GET /api/relays | tr '{' '\n' | grep '"address":"relay-3:7444"' | grep -q '"connected":true'; }
check "a relay added on site-a"                 wait_for 30 bash -c "$(declare -f api dc relay3_up); relay3_up site-a"
check "is added on site-b and site-c too"       wait_for 60 bash -c "$(declare -f api dc relay3_up); relay3_up site-b && relay3_up site-c"
check "and they say where it came from"         bash -c "$(declare -f dc); dc logs site-b 2>&1 | grep -q 'relay added on another site.*relay-3'"
R2=$(api site-a GET /api/relays | tr '{' '\n' | grep '"address":"relay-2:7444"' | sed -n 's/.*"id":"\([0-9a-f]*\)".*/\1/p')
api site-a DELETE /api/relays/$R2 >/dev/null
check "site-a removes relay-2; the others too"  wait_for 60 bash -c "$(declare -f api dc has_relay); ! has_relay site-b relay-2 && ! has_relay site-c relay-2"
check "and say where that came from"           bash -c "$(declare -f dc); dc logs site-b 2>&1 | grep -q 'relay removed on another site'"
check "it doesn't come back"                    bash -c "sleep 15; $(declare -f api dc has_relay); ! has_relay site-a relay-2 && ! has_relay site-b relay-2"
check "site-c still reaches the others"         wait_for 30 bash -c "$(declare -f api dc peer_of); peer_of site-c site-a && peer_of site-c site-b"

echo "Leaving"
check "site-c leaves the IBS"                   bash -c "$(declare -f api dc); api site-c POST /api/ibs/leave; [ \$? -eq 0 ]"
check "site-a and site-b forget site-c"         wait_for 20 bash -c "$(declare -f api dc not_peer_of); not_peer_of site-a site-c && not_peer_of site-b site-c"
check "site-c has no sites left"                bash -c "$(declare -f api dc); [ \"\$(api site-c GET /api/peers)\" = '[]' ]"
check "site-c deleted site-b's copies"          wait_for 30 bash -c "$(declare -f api dc); api site-c GET /api/dashboard | grep -q '\"held_bytes\":0'"
check "site-c doesn't come back on its own"     bash -c "sleep 12; $(declare -f api dc not_peer_of); not_peer_of site-a site-c && not_peer_of site-b site-c"

echo "Rejoining with a short code"
code() { api "$1" POST /api/peers/invite | sed -n 's/.*"code":"\([^"]*\)".*/\1/p'; }
WRONG=$(code site-b)
check "invites come with a short code"          bash -c "echo '$WRONG' | grep -Eq '^relay-[0-9a-z-]+(:[0-9]+)? [0-9]+-[a-z]+-[a-z]+-[a-z]+$'"
GUESS=$(echo "$WRONG" | sed -E 's/-[a-z]+$/-zebra/'); [ "$GUESS" = "$WRONG" ] && GUESS=$(echo "$WRONG" | sed -E 's/-[a-z]+$/-yacht/')
check "a wrong word is refused"                 bash -c "$(declare -f api dc); api site-c POST /api/peers/join '{\"token\":\"$GUESS\"}' | grep -q \"don't match\""
check "and the code is spent"                   bash -c "$(declare -f api dc); ! api site-c POST /api/peers/join '{\"token\":\"$WRONG\"}' | grep -q site_id"
check "site-c joins site-b with a new code"     bash -c "$(declare -f api dc); api site-c POST /api/peers/join '{\"token\":\"$(code site-b)\"}' | grep -q site_id"
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
