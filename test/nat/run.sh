#!/bin/bash
# Brings up two sites behind separate NAT routers, pairs them through the
# relay and checks that they end up with a direct (hole punched) connection.
set -e
cd "$(dirname "$0")"
MODE=${1:-normal}
if [ "$MODE" = symmetric ]; then export NAT_FLAGS=--random-fully; fi
dc() { docker compose -p ibs-nat "$@"; }
api() { # site-ip method path [json]
  local jar=/tmp/jar-$1
  dc exec -T ctl sh -c "grep -qs ibs_session $jar || curl -s -c $jar -H 'Content-Type: application/json' -d '{\"password\":\"changeme123\"}' http://$1:8080/api/login >/dev/null;
    curl -s -b $jar -X $2 -H 'X-IBS: 1' -H 'Content-Type: application/json' ${4:+-d '$4'} http://$1:8080$3"
}
dc down -v --remove-orphans >/dev/null 2>&1 || true
[ "${KEEP:-}" = 1 ] || trap 'dc down -v --remove-orphans >/dev/null 2>&1' EXIT
dc up -d
echo "waiting for sites..."; sleep 15
echo "site-a reaching site-b directly (should fail, only routers in between):"
dc exec -T site-a sh -c 'wget -q -T 3 -O- http://10.2.0.10:8080/api/state 2>&1 || echo "  not reachable: OK"'
echo "relay sees the sites as:"
dc logs relay | grep -o "site registered.*" || true
INVITE=$(api 10.1.0.10 POST /api/peers/invite | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
api 10.2.0.10 POST /api/peers/join "{\"token\":\"$INVITE\"}" >/dev/null
echo "paired; waiting for the direct connection (up to 60s)..."
for i in $(seq 1 30); do
  PATHS=$(api 10.1.0.10 GET /api/peers)
  if echo "$PATHS" | grep -q '"kind":"quic"'; then break; fi
  if [ "$MODE" = symmetric ] && [ $i -ge 25 ]; then break; fi
  sleep 2
done
echo "$PATHS" | sed -n 's/.*"name":"\([^"]*\)".*"path":{"kind":"\([^"]*\)".*"rtt_ms":\([0-9.]*\).*/  site-a -> \1: \2, ping \3 ms/p'
dc logs site-a site-b | grep -E "direct connection established" | sed 's/.*msg=/  /' || true
EP=$(dc logs site-a | grep "direct connection established" | tail -1 | sed -n 's/.*endpoint=\([^ ]*\).*/\1/p')
if [ "$MODE" = symmetric ]; then
  if echo "$PATHS" | grep -q '"kind":"relay"' && echo "$PATHS" | grep -q '"online":true'; then
    echo "PASS: symmetric NAT on both sides, sites stay connected through the relay"
  else
    echo "FAIL: expected a working relayed connection"; exit 1
  fi
elif echo "$PATHS" | grep -q '"kind":"quic"' && [[ "$EP" == 172.31.0.3:* ]]; then
  echo "PASS: direct connection through both NATs (site-a reached site-b at router-b's public address $EP)"
else
  echo "FAIL: no direct connection through the NATs (endpoint: ${EP:-none})"; exit 1
fi
