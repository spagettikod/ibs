#!/bin/bash
# Tests HTTPS with a certificate from an ACME CA through the DNS-01 challenge.
# Runs Pebble (Let's Encrypt's test CA) and its mock DNS server next to a
# throwaway site; the site uses lego's "exec" DNS provider to set the TXT
# records. Needs: docker, curl, openssl, the "ibs" image.
#
#   docker build -t ibs . && test/tls/run.sh
set -u
cd "$(dirname "$0")"
. ../lib/site.sh
WORK=$(mktemp -d)
ID=ibs-tls-$$
NET=$ID
cleanup() { docker rm -fv "$ID-site" "$ID-site2" "$ID-web" $ID-pebble $ID-dns >/dev/null 2>&1; docker network rm "$NET" >/dev/null 2>&1; docker volume rm "$ID-data" >/dev/null 2>&1; rm -rf "$WORK"; }
[ "${KEEP:-}" = 1 ] || trap cleanup EXIT

echo "building Pebble"
docker build -q -t ibs-pebble -f Dockerfile.pebble . >/dev/null || exit 1
docker network create "$NET" >/dev/null || exit 1
docker run -d --name $ID-dns --network "$NET" --network-alias dns ibs-pebble \
  pebble-challtestsrv -defaultIPv6 "" -http01 "" -https01 "" -tlsalpn01 "" -doh "" >/dev/null || exit 1
docker run -d --name $ID-pebble --network "$NET" --network-alias pebble -p 127.0.0.1::15000 \
  -e PEBBLE_VA_NOSLEEP=1 -w / ibs-pebble \
  pebble -config /test/config/pebble-config.json -dnsserver dns:8053 >/dev/null || exit 1

# The site trusts Pebble's API certificate and runs the DNS hook from $WORK.
# The mock DNS server can't answer the SOA queries lego's propagation check
# needs, so the site just waits briefly (IBS_TLS_DNS_PROPAGATION_WAIT).
cp dns-hook.sh "$WORK/"
docker cp -q $ID-pebble:/test/certs/pebble.minica.pem "$WORK/pebble-api.pem"
chmod -R a+rX "$WORK"

start_tls_site() { # name [extra docker args...]
  local name=$1; shift
  docker run -d --name "$name" --network "$NET" -p 127.0.0.1::8080 -p 127.0.0.1::9000 \
    -v "$WORK:/hook:ro" -v "$ID-data:/data" \
    -e IBS_SITE_NAME=tls -e IBS_ADMIN_PASSWORD=test-password -e IBS_RELEASES_URL=http://127.0.0.1:9/releases -e IBS_STORAGE_LIMIT=200M \
    -e IBS_TLS_DOMAINS=ibs.test,s3.ibs.test -e IBS_TLS_DNS_PROVIDER=exec -e EXEC_PATH=/hook/dns-hook.sh \
    -e IBS_TLS_CA=https://pebble:14000/dir -e LEGO_CA_CERTIFICATES=/hook/pebble-api.pem \
    -e IBS_TLS_DNS_PROPAGATION_WAIT=2s "$@" ibs >/dev/null
}
wait_log() { # container pattern seconds
  for _ in $(seq 1 $(($3 * 2))); do
    docker logs "$1" 2>&1 | grep -q "$2" && return 0
    sleep 0.5
  done
  docker logs "$1" 2>&1 | tail -20
  return 1
}

start_tls_site "$ID-site" || exit 1
WEB_PORT=$(docker port "$ID-site" 8080/tcp | head -1 | cut -d: -f2)
S3_PORT=$(docker port "$ID-site" 9000/tcp | head -1 | cut -d: -f2)
WEB=https://ibs.test:$WEB_PORT
S3=https://s3.ibs.test:$S3_PORT
CURL=(curl -sf --cacert "$WORK/root.pem" --resolve "ibs.test:$WEB_PORT:127.0.0.1" --resolve "s3.ibs.test:$S3_PORT:127.0.0.1")

echo "Certificate"
check "certificate issued through DNS-01"       wait_log "$ID-site" "certificate issued" 120
curl -sfk "https://127.0.0.1:$(docker port $ID-pebble 15000/tcp | head -1 | cut -d: -f2)/roots/0" >"$WORK/root.pem"
check "web interface serves it"                 bash -c "$(printf '%q ' "${CURL[@]}") $WEB/api/state | grep -q '\"setup\":true'"
check "issued by the ACME CA"                   bash -c "openssl s_client -connect 127.0.0.1:$WEB_PORT -servername ibs.test </dev/null 2>/dev/null | openssl x509 -noout -issuer | grep -qi pebble"
check "certificate names both domains"          bash -c "openssl s_client -connect 127.0.0.1:$WEB_PORT -servername ibs.test </dev/null 2>/dev/null | openssl x509 -noout -ext subjectAltName | grep -q 's3.ibs.test'"
check "plain HTTP API points to HTTPS"         bash -c "curl -s -w '%{http_code}' http://127.0.0.1:$WEB_PORT/api/state | tr -d '\n' | grep -q '\"https_url\":\"https://ibs.test:$WEB_PORT/api/state\".*421'"
check "plain HTTP pages redirect to HTTPS"      bash -c "curl -s -o /dev/null -w '%{http_code} %{redirect_url}' http://127.0.0.1:$WEB_PORT/ | grep -q '^308 https://ibs.test:$WEB_PORT/'"

echo "Web interface and S3 over HTTPS"
check "login works and the cookie is Secure"    bash -c "$(printf '%q ' "${CURL[@]}") -c '$WORK/jar' -H 'Content-Type: application/json' -d '{\"password\":\"test-password\"}' $WEB/api/login >/dev/null && grep -q 'TRUE.*ibs_session' '$WORK/jar'"
check "certificate status in the API"           bash -c "$(printf '%q ' "${CURL[@]}") -b '$WORK/jar' $WEB/api/tls | grep -q '\"active\":true.*\"not_after\"'"
KEY=$("${CURL[@]}" -b "$WORK/jar" -H 'X-IBS: 1' -H 'Content-Type: application/json' -d '{"name":"t"}' "$WEB/api/keys")
AK=$(sed -n 's/.*"id":"\([^"]*\)".*/\1/p' <<<"$KEY")
SK=$(sed -n 's/.*"secret":"\([^"]*\)".*/\1/p' <<<"$KEY")
S3C=("${CURL[@]}" --aws-sigv4 "aws:amz:us-east-1:s3" --user "$AK:$SK")
check "S3: create a bucket"                     "${S3C[@]}" -X PUT "$S3/tls-bucket"
check "S3: upload"                              "${S3C[@]}" -X PUT --data-binary "hello over https" "$S3/tls-bucket/hello.txt"
check "S3: download"                            bash -c "[ \"\$($(printf '%q ' "${S3C[@]}") $S3/tls-bucket/hello.txt)\" = 'hello over https' ]"

echo "Restart and configuration"
docker rm -f "$ID-site" >/dev/null
start_tls_site "$ID-site2"
check "restart reuses the stored certificate"   wait_log "$ID-site2" "certificate loaded" 30
check "no new certificate after restart"        bash -c "sleep 3; ! docker logs $ID-site2 2>&1 | grep -q 'requesting certificate'"
docker logs "$ID-site2" >"${TLS_LOG:-/dev/null}" 2>&1; docker rm -f "$ID-site2" >/dev/null
check "unknown DNS provider is rejected"        bash -c "docker run --rm -e IBS_TLS_DOMAINS=x.test -e IBS_TLS_DNS_PROVIDER=nope ibs 2>&1 | grep -q 'unsupported DNS provider.*nope.*supported: acme-dns'"
check "missing provider credentials stop the start" bash -c "docker run --rm -e IBS_TLS_DOMAINS=x.test -e IBS_TLS_DNS_PROVIDER=cloudflare ibs 2>&1 | grep -q 'CLOUDFLARE_DNS_API_TOKEN'"
check "domains without a provider are rejected" bash -c "docker run --rm -e IBS_TLS_DOMAINS=x.test ibs 2>&1 | grep -q 'a DNS provider is required'"
docker volume rm "$ID-data" >/dev/null 2>&1

echo "Set up in the web interface"
# A new site, created through the setup wizard's API with HTTPS settings.
docker run -d --name "$ID-web" --network "$NET" -p 127.0.0.1::8080 -p 127.0.0.1::9000 -v "$WORK:/hook:ro" \
  -e IBS_STORAGE_LIMIT=200M -e LEGO_CA_CERTIFICATES=/hook/pebble-api.pem ibs >/dev/null
P=$(docker port "$ID-web" 8080/tcp | head -1 | cut -d: -f2)
P3=$(docker port "$ID-web" 9000/tcp | head -1 | cut -d: -f2)
PLAIN=http://127.0.0.1:$P
SECURE=(curl -sf --cacert "$WORK/root.pem" --resolve "web.test:$P:127.0.0.1")
J=(-H 'X-IBS: 1' -H 'Content-Type: application/json')
JH="-H 'X-IBS: 1' -H 'Content-Type: application/json'"
for _ in $(seq 1 50); do curl -sf "$PLAIN/api/state" >/dev/null && break; sleep 0.2; done
TLS='{"domains":["web.test"],"provider":"exec","env":{"EXEC_PATH":"/hook/dns-hook.sh"},"ca":"https://pebble:14000/dir","propagation_wait":"2s"}'
check "wizard: bad HTTPS settings are refused"  bash -c "curl -s $JH -d '{\"site_name\":\"w\",\"password\":\"test-password\",\"tls\":{\"domains\":[\"web.test\"],\"provider\":\"cloudflare\"}}' $PLAIN/api/setup/create | grep -q 'HTTPS: DNS provider cloudflare'"
check "wizard: create the site with HTTPS"      bash -c "curl -sf -c '$WORK/jar2' $JH -d '{\"site_name\":\"w\",\"password\":\"test-password\",\"tls\":$TLS}' $PLAIN/api/setup/create >/dev/null"
check "certificate issued"                      wait_log "$ID-web" "certificate issued" 120
check "plain HTTP keeps working"                bash -c "curl -sf -b '$WORK/jar2' $PLAIN/api/tls | grep -q '\"active\":true'"
check "HTTPS on the same port"                  bash -c "$(printf '%q ' "${SECURE[@]}") https://web.test:$P/api/state | grep -q '\"setup\":true'"
"${SECURE[@]}" -c "$WORK/jar3" "${J[@]}" -d '{"password":"test-password"}' "https://web.test:$P/api/login" >/dev/null
REQ=$(sed 's/}$/,"require_https":true}/' <<<"$TLS")
check "require HTTPS is refused over plain HTTP" bash -c "curl -s -b '$WORK/jar2' $JH -X PUT -d '$REQ' $PLAIN/api/tls | grep -q 'over HTTPS before requiring'"
check "require HTTPS over HTTPS"                bash -c "$(printf '%q ' "${SECURE[@]}") -b '$WORK/jar3' $JH -X PUT -d '$REQ' https://web.test:$P/api/tls | grep -q '\"require_https\":true'"
check "no new certificate for the same names"   bash -c "sleep 2; [ \$(docker logs $ID-web 2>&1 | grep -c 'requesting certificate') = 1 ]"
check "then plain HTTP points to HTTPS"         bash -c "curl -s -o /dev/null -w '%{http_code}' $PLAIN/api/state | grep -q 421"
check "and plain S3 is refused"                 bash -c "curl -s http://127.0.0.1:$P3/ | grep -q InsecureRequest"
# Lose the HTTPS folder: it comes back from the site's metadata backup.
check "HTTPS comes back from the site's backup" bash -c "docker exec $ID-web sh -c 'rm -rf /data/tls/*' && docker restart $ID-web >/dev/null && sleep 3 && docker logs $ID-web 2>&1 | grep -q 'HTTPS settings restored' && docker logs --since 10s $ID-web 2>&1 | grep -q 'certificate loaded'"
check "without a new certificate"               bash -c "[ \$(docker logs $ID-web 2>&1 | grep -c 'requesting certificate') = 1 ]"
# The restart gives the container a new host port.
check "https-off turns it off"                  bash -c "docker exec -u ibs $ID-web ibs https-off | grep -q removed && docker restart $ID-web >/dev/null && sleep 2 && curl -sf http://\$(docker port $ID-web 8080/tcp | head -1)/api/state >/dev/null"
docker rm -fv "$ID-web" >/dev/null

finish
