#!/bin/sh
# lego "exec" DNS provider hook: sets the challenge TXT record in pebble-challtestsrv.
#   dns-hook.sh present|cleanup <fqdn> <value>
case $1 in
  present) path=set-txt; body="{\"host\":\"$2\",\"value\":\"$3\"}" ;;
  cleanup) path=clear-txt; body="{\"host\":\"$2\"}" ;;
  *) exit 0 ;;
esac
wget -q -O /dev/null --post-data "$body" "http://dns:8055/$path"
