#!/bin/bash
# Tests rclone (S3 backend, provider "Other") against a throwaway IBS site.
# rclone runs from its official Docker image, so it needs no install.
# Needs: docker, the "ibs" image, curl.
#
#   docker build -t ibs . && test/s3/rclone.sh
#
# Cases marked "unsupported" document what IBS does not support yet; they
# pass when rclone fails as expected.
set -u
cd "$(dirname "$0")"
WORK=$(mktemp -d)
. ../lib/site.sh
RCLONE_IMAGE=${RCLONE_IMAGE:-rclone/rclone:1.68.2}
docker image inspect "$RCLONE_IMAGE" >/dev/null 2>&1 || docker pull -q "$RCLONE_IMAGE" >/dev/null
start_site

# rclone with an "ibs:" remote pointing at the test site; /work is $WORK.
# The cache folder persists between runs (bisync keeps its state there).
rc() {
  docker run --rm -i --add-host host.docker.internal:host-gateway -v "$WORK:/work" -v "$WORK/cache:/root/.cache" \
    -e RCLONE_CONFIG_IBS_TYPE=s3 -e RCLONE_CONFIG_IBS_PROVIDER=Other \
    -e RCLONE_CONFIG_IBS_ACCESS_KEY_ID="$AWS_ACCESS_KEY_ID" \
    -e RCLONE_CONFIG_IBS_SECRET_ACCESS_KEY="$AWS_SECRET_ACCESS_KEY" \
    -e RCLONE_CONFIG_IBS_ENDPOINT="$IBS_S3_DOCKER" -e RCLONE_CONFIG_IBS_REGION=us-east-1 \
    "$RCLONE_IMAGE" "$@"
}

# Make rc and its settings available to the "bash -c" test cases.
export -f rc
export WORK RCLONE_IMAGE

mkdir -p "$WORK/tree/sub" "$WORK/out"
for f in a b c; do echo "file $f" >"$WORK/tree/$f.txt"; done
echo d >"$WORK/tree/sub/d.txt"
head -c 1000 /dev/urandom >"$WORK/tree/random.bin"
mkdir -p "$WORK/bigdir"
head -c 30000000 /dev/urandom >"$WORK/bigdir/big.bin"
TZ=UTC touch -t 202001020304.05 "$WORK/tree/a.txt"

echo "Buckets and listing"
check "mkdir creates a bucket"                  rc mkdir ibs:rtest
check "lsd lists buckets"                       bash -c "rc lsd ibs: | grep -q rtest"
check "copy uploads a tree"                     rc copy /work/tree ibs:rtest/tree
check "ls lists files with sizes"               bash -c "[ \$(rc ls ibs:rtest/tree | wc -l) -eq 5 ]"
check "lsf lists a folder"                      bash -c "rc lsf ibs:rtest/tree | grep -q '^sub/$'"
check "lsjson works"                            bash -c "rc lsjson -R ibs:rtest/tree | grep -q '\"Path\":\"sub/d.txt\"'"
check "tree works"                              rc tree ibs:rtest
check "size counts objects"                     bash -c "rc size ibs:rtest/tree | grep -q 'Total objects: 5'"
check "modification time is preserved"          bash -c "rc lsl ibs:rtest/tree/a.txt | grep -q '2020-01-02 03:04:05'"

echo "Reading and integrity"
check "cat prints a file"                       bash -c "[ \"\$(rc cat ibs:rtest/tree/a.txt)\" = 'file a' ]"
check "md5sum matches local files"              bash -c "[ \"\$(rc md5sum ibs:rtest/tree/a.txt | cut -d' ' -f1)\" = \"\$(openssl md5 -r '$WORK/tree/a.txt' | cut -d' ' -f1)\" ]"
check "check (size and hash) passes"            rc check /work/tree ibs:rtest/tree
check "copy downloads unchanged"                bash -c "rc copy ibs:rtest/tree /work/out/tree && diff -r '$WORK/tree' '$WORK/out/tree'"

echo "Large files (multipart)"
check "copy a 30 MB file in 5 MB parts"         rc copy /work/bigdir ibs:rtest/bigdir --s3-upload-cutoff 5M --s3-chunk-size 5M
check "check (hash) on the multipart file"      rc check /work/bigdir ibs:rtest/bigdir
check "check --download compares content"       rc check /work/bigdir ibs:rtest/bigdir --download
check "download the large file unchanged"       bash -c "rc copyto ibs:rtest/bigdir/big.bin /work/out/big.bin && cmp '$WORK/bigdir/big.bin' '$WORK/out/big.bin'"
check "rcat streams stdin to an object"         bash -c "head -c 12000000 /dev/zero | rc rcat ibs:rtest/streamed.bin --s3-chunk-size 5M && [ \"\$(rc size ibs:rtest/streamed.bin --json | grep -o '\"bytes\":[0-9]*')\" = '\"bytes\":12000000' ]"

echo "Server-side copy and move"
check "copyto within the remote"                bash -c "rc copyto ibs:rtest/tree/b.txt ibs:rtest/copy.txt && [ \"\$(rc cat ibs:rtest/copy.txt)\" = 'file b' ]"
check "server-side copy of a large file"        bash -c "rc copy ibs:rtest/bigdir ibs:rtest/bigdir2 --s3-copy-cutoff 5M && rc check /work/bigdir ibs:rtest/bigdir2 --download"
check "moveto renames"                          bash -c "rc moveto ibs:rtest/copy.txt ibs:rtest/moved.txt && ! rc lsf ibs:rtest | grep -q '^copy.txt$' && rc lsf ibs:rtest | grep -q '^moved.txt$'"
check "touch sets the modification time"        bash -c "rc touch ibs:rtest/moved.txt -t 2021-06-07T08:09:10 && rc lsl ibs:rtest/moved.txt | grep -q '2021-06-07 08:09:10'"
check "touch creates an empty file"             bash -c "rc touch ibs:rtest/new-empty.txt && rc lsf ibs:rtest | grep -q '^new-empty.txt$'"

echo "Sync"
check "sync uploads changes only"               bash -c "rc sync /work/tree ibs:rtest/synced && echo changed >'$WORK/tree/c.txt' && rc sync /work/tree ibs:rtest/synced -v 2>&1 | grep -q 'c.txt: Copied'"
check "sync deletes removed files"              bash -c "rm '$WORK/tree/b.txt' && rc sync /work/tree ibs:rtest/synced && ! rc lsf ibs:rtest/synced | grep -q '^b.txt$'"
check "check after sync passes"                 rc check /work/tree ibs:rtest/synced
# bisync needs both sides to exist; on S3 a folder exists once it has a file.
check "bisync between two folders"              bash -c "mkdir -p '$WORK/bi' && echo x >'$WORK/bi/x.txt' && rc copy /work/bi ibs:rtest/bi && rc bisync /work/bi ibs:rtest/bi --resync && echo y >'$WORK/bi/y.txt' && rc bisync /work/bi ibs:rtest/bi && rc lsf ibs:rtest/bi | grep -q '^y.txt$'"

echo "Links and housekeeping"
# The URL is signed for host.docker.internal; curl connects to 127.0.0.1 instead.
check "link creates a working public URL"       bash -c "url=\$(rc link ibs:rtest/tree/a.txt --expire 1h) && [ \"\$(curl -sf --connect-to host.docker.internal:${IBS_S3##*:}:127.0.0.1:${IBS_S3##*:} \"\$url\")\" = 'file a' ]"
check "cleanup aborts stale multipart uploads"  rc cleanup ibs:rtest
check "deletefile removes one file"             bash -c "rc deletefile ibs:rtest/moved.txt && ! rc lsf ibs:rtest | grep -q '^moved.txt$'"
check "delete removes files under a path"       bash -c "rc delete ibs:rtest/synced && [ -z \"\$(rc lsf ibs:rtest/synced)\" ]"
check "purge removes a bucket with contents"    rc purge ibs:rtest
check "rmdir removes an empty bucket"           bash -c "rc mkdir ibs:empty && rc rmdir ibs:empty && ! rc lsd ibs: | grep -q empty"

echo "Unsupported (expected to fail)"
check "mkdir for the remaining cases"           rc mkdir ibs:rtest2
check "copy a file for the remaining cases"     rc copyto /work/tree/c.txt ibs:rtest2/v.bin
expect_fail "unsupported: --s3-versions listing (no versioning)" "NotImplemented" rc lsl ibs:rtest2 --s3-versions
# rclone logs the error but exits successfully, so look at its output.
check "unsupported: settier is refused (one storage class)" bash -c "rc settier GLACIER ibs:rtest2/v.bin 2>&1 | grep -q InvalidStorageClass"
expect_fail "unsupported: about (no quota info for S3)" "doesn't support about\|not supported" rc about ibs:

finish
