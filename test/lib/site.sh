# Shared helpers for the S3 client tests (source this file).
#
# start_site starts a throwaway IBS site in Docker (image "ibs"), creates an
# access key and exports:
#   IBS_WEB, IBS_S3          admin UI and S3 endpoint URLs (on 127.0.0.1)
#   IBS_S3_DOCKER            S3 endpoint as seen from other containers
#   AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_DEFAULT_REGION
# The site and its data are removed when the script exits.
#
# check / expect_fail run one test case each and count the results;
# finish prints a summary and sets the exit code.

PASS=0
FAIL=0
FAILED=()

start_site() {
  SITE=ibs-test-$$
  docker run -d --name "$SITE" -p 127.0.0.1::8080 -p 127.0.0.1::9000 \
    -e IBS_SITE_NAME=test -e IBS_ADMIN_PASSWORD=test-password -e IBS_RELEASES_URL=http://127.0.0.1:9/releases \
    -e IBS_STORAGE_LIMIT="${STORAGE_LIMIT:-200M}" ibs >/dev/null || exit 1
  trap 'docker rm -fv "$SITE" >/dev/null 2>&1; rm -rf "$WORK"' EXIT
  IBS_WEB=http://$(docker port "$SITE" 8080/tcp | head -1)
  IBS_S3=http://$(docker port "$SITE" 9000/tcp | head -1)
  IBS_S3_DOCKER=http://host.docker.internal:${IBS_S3##*:}
  for _ in $(seq 1 300); do # up to a minute, also when other suites run alongside
    curl -sf "$IBS_WEB/api/state" | grep -q '"setup":true' && break
    sleep 0.2
  done
  local jar=$WORK/cookies
  curl -sf -c "$jar" -H 'Content-Type: application/json' -d '{"password":"test-password"}' "$IBS_WEB/api/login" >/dev/null || {
    echo "cannot log in to the test site"; exit 1; }
  local key
  key=$(curl -sf -b "$jar" -H 'X-IBS: 1' -H 'Content-Type: application/json' -d '{"name":"test"}' "$IBS_WEB/api/keys")
  export AWS_ACCESS_KEY_ID=$(sed -n 's/.*"id":"\([^"]*\)".*/\1/p' <<<"$key")
  export AWS_SECRET_ACCESS_KEY=$(sed -n 's/.*"secret":"\([^"]*\)".*/\1/p' <<<"$key")
  export AWS_DEFAULT_REGION=us-east-1
  export IBS_WEB IBS_S3 IBS_S3_DOCKER
  echo "test site: S3 $IBS_S3"
}

# check NAME COMMAND... : passes when the command succeeds.
check() {
  local name=$1; shift
  if out=$("$@" 2>&1); then
    PASS=$((PASS + 1)); echo "  ok    $name"
  else
    FAIL=$((FAIL + 1)); FAILED+=("$name"); echo "  FAIL  $name"; echo "$out" | sed 's/^/        /' | tail -5
    return 1
  fi
}

# expect_fail NAME PATTERN COMMAND... : passes when the command fails and
# its output contains PATTERN (e.g. an S3 error code).
expect_fail() {
  local name=$1 pattern=$2; shift 2
  if out=$("$@" 2>&1); then
    FAIL=$((FAIL + 1)); FAILED+=("$name"); echo "  FAIL  $name (succeeded, expected $pattern)"
  elif grep -q -- "$pattern" <<<"$out"; then
    PASS=$((PASS + 1)); echo "  ok    $name"
  else
    FAIL=$((FAIL + 1)); FAILED+=("$name"); echo "  FAIL  $name (expected $pattern)"; echo "$out" | sed 's/^/        /' | tail -5
  fi
}

finish() {
  echo
  echo "$PASS passed, $FAIL failed"
  for f in ${FAILED[@]+"${FAILED[@]}"}; do echo "  failed: $f"; done
  [ "$FAIL" -eq 0 ]
}
