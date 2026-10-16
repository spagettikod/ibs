#!/bin/bash
# Removes what the test suites leave behind in Docker on this machine, and
# nothing else (no docker prune):
#   - containers and networks of the suites, also from runs that were stopped
#   - the images the suites build (ibs, ibs-relay, ibs-launcher, ibs-pebble)
#     and the ones only they download (rclone, curl, busybox)
#   - untagged layers of earlier IBS builds (labelled with the IBS repository)
#   - Docker's build cache beyond 2 GB (every image build adds layers; it grew
#     to 14 GB between runs)
# The local demo (compose project "ibs") and the alpine base image are left
# alone.
#
#   test/cleanup.sh
set -u

containers=$(docker ps -aq --filter name='^ibs-upd-' --filter name='^ibs-scrub-' --filter name='^ibs-test-' --filter name='^ibs-tls-' \
  --filter name='^relcheck$' --filter name='^uicheck$' --filter name='^rv-')
for p in ibs-mesh ibs-nat; do
  containers="$containers $(docker ps -aq --filter label=com.docker.compose.project=$p)"
done
[ -n "${containers// /}" ] && docker rm -fv $containers >/dev/null

for p in ibs-mesh ibs-nat; do
  vols=$(docker volume ls -q --filter label=com.docker.compose.project=$p)
  [ -n "$vols" ] && docker volume rm $vols >/dev/null
done
vols=$(docker volume ls -q --filter name='^ibs-tls-')
[ -n "$vols" ] && docker volume rm $vols >/dev/null
nets=$(docker network ls -q --filter name='^ibs-upd-' --filter name='^ibs-scrub-' --filter name='^ibs-tls-' --filter name='^rv$' \
  --filter label=com.docker.compose.project=ibs-mesh --filter label=com.docker.compose.project=ibs-nat)
[ -n "$nets" ] && docker network rm $nets >/dev/null 2>&1

images=$(docker images -q --filter reference=ibs --filter reference=ibs-relay --filter reference=ibs-launcher \
  --filter reference=ibs-pebble --filter reference='ghcr.io/spagettikod/ibs/launcher' --filter reference='ghcr.io/spagettikod/ibs/relay' \
  --filter reference='rclone/rclone' --filter reference='curlimages/curl' --filter reference='busybox:1.36')
dangling=$(docker images -q --filter dangling=true --filter label=org.opencontainers.image.source=https://github.com/spagettikod/ibs)
all=$(echo $images $dangling | tr ' ' '\n' | sort -u)
[ -n "$all" ] && docker rmi -f $all >/dev/null 2>&1
docker builder prune -f --max-used-space 2GB >/dev/null 2>&1
docker system df | sed -n '1,2p;5p'
