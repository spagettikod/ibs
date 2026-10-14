#!/bin/bash
# Runs every test: go vet and the Go tests, then the Docker suites, several at
# a time (JOBS, default 3; more can overload Docker Desktop), longest first.
# Each suite has its own containers and networks; the two NAT runs share one
# network, so they run one after the other. Logs go to test/logs/<suite>.log.
#
#   test/run-all.sh
#   JOBS=1 test/run-all.sh   # one suite at a time
set -u
cd "$(dirname "$0")/.."
LOGS=test/logs
rm -rf "$LOGS" && mkdir -p "$LOGS"
t0=$(date +%s)

echo "== go vet, go test"
if ! { go vet ./... && go test -race -count=1 ./...; } >"$LOGS/go.log" 2>&1; then
  grep -v "no test files" "$LOGS/go.log" | tail -30
  exit 1
fi
echo "== images"
(cd web && npm run build --silent >/dev/null) &&
  docker build -q -t ibs . >/dev/null && docker build -q -t ibs-relay --target relay . >/dev/null || exit 1

suites=(
  "updates:test/updates/run.sh"
  "tls:test/tls/run.sh"
  "nat:test/nat/run.sh && test/nat/run.sh symmetric"
  "mesh:test/mesh/run.sh"
  "aws-cli:test/s3/aws-cli.sh"
  "rclone:test/s3/rclone.sh"
  "scrub:test/scrub/run.sh"
)
JOBS=${JOBS:-3}
echo "== suites ($JOBS at a time, logs in $LOGS)"
for s in "${suites[@]}"; do
  while [ "$(jobs -rp | wc -l)" -ge "$JOBS" ]; do sleep 1; done
  name=${s%%:*}
  (
    start=$(date +%s)
    bash -c "${s#*:}" >"$LOGS/$name.log" 2>&1
    echo "$? $(($(date +%s) - start))" >"$LOGS/$name.result"
  ) &
done
wait

failed=0
for s in "${suites[@]}"; do
  name=${s%%:*}
  read -r code secs <"$LOGS/$name.result"
  summary=$(grep -E "passed, [0-9]+ failed|^PASS|^FAIL" "$LOGS/$name.log" | tr '\n' ' ')
  if [ "$code" = 0 ]; then
    printf "  ok    %-8s %4ss  %s\n" "$name" "$secs" "$summary"
  else
    failed=1
    printf "  FAIL  %-8s %4ss  %s\n" "$name" "$secs" "$summary"
    grep -E "  FAIL|^FAIL" -A3 "$LOGS/$name.log" | head -20 | sed 's/^/        /'
  fi
done
echo "total $(($(date +%s) - t0))s"
exit $failed
