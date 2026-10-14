#!/bin/sh
# When the container starts as root (the default), hand the data folders to
# the unprivileged ibs user, which matters for bind-mounted host folders that
# Docker creates as root, then drop privileges. IBS_BIN selects the program
# (ibs or ibs-relay).
bin="${IBS_BIN:-ibs}"
if [ "$(id -u)" = 0 ]; then
  for d in /data /join ${IBS_RELAY_TOKEN_OUT:+"$(dirname "$IBS_RELAY_TOKEN_OUT")"} \
           ${IBS_JOIN_TOKEN_OUT:+"$(dirname "$IBS_JOIN_TOKEN_OUT")"}; do
    [ -d "$d" ] && chown ibs:ibs "$d" 2>/dev/null
  done
  exec su-exec ibs "$bin" "$@"
fi
exec "$bin" "$@"
