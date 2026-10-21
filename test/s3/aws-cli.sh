#!/bin/bash
# Tests every S3 operation IBS supports with the AWS CLI, against a
# throwaway site in Docker. Needs: docker, the "ibs" image, aws, curl.
#
#   docker build -t ibs . && test/s3/aws-cli.sh
#
# Keep this in sync with the S3 support table in the README: every
# supported operation has at least one case here.
set -u
cd "$(dirname "$0")"
WORK=$(mktemp -d)
. ../lib/site.sh
start_site

S3="aws --endpoint-url $IBS_S3"
s3() { $S3 s3 "$@"; }
api() { $S3 s3api "$@"; }
same() { cmp -s "$1" "$2"; }

# Test files
head -c 1000 /dev/urandom >"$WORK/small.bin"
head -c 20000000 /dev/urandom >"$WORK/big.bin"   # > 8 MB: multipart upload by the CLI
: >"$WORK/empty.bin"
echo "hello ibs" >"$WORK/hello.txt"
mkdir -p "$WORK/tree/sub/deeper"
for f in a b c; do echo "$f" >"$WORK/tree/$f.txt"; done
echo d >"$WORK/tree/sub/d.txt"; echo e >"$WORK/tree/sub/deeper/e.txt"

# Builds an object from the first 1000 bytes of big.bin with upload-part-copy.
part_copy_range() {
  local up etag
  up=$(api create-multipart-upload --bucket test --key ranged.bin --query UploadId --output text) || return 1
  [ -n "$up" ] && [ "$up" != None ] || { echo "create-multipart-upload returned no UploadId: '$up'"; return 1; }
  etag=$(api upload-part-copy --bucket test --key ranged.bin --upload-id "$up" --part-number 1 \
    --copy-source test/big.bin --copy-source-range bytes=0-999 --query CopyPartResult.ETag --output text) || return 1
  [ -n "$etag" ] && [ "$etag" != None ] || { echo "upload-part-copy returned no ETag: '$etag'"; return 1; }
  api complete-multipart-upload --bucket test --key ranged.bin --upload-id "$up" \
    --multipart-upload "{\"Parts\":[{\"PartNumber\":1,\"ETag\":$etag}]}" >/dev/null || return 1
  s3 cp s3://test/ranged.bin - | cmp -s - <(head -c 1000 "$WORK/big.bin")
}

echo "Buckets"
check "mb creates a bucket"                     s3 mb s3://test
check "mb second bucket"                        s3 mb s3://other
check "ls lists buckets"                        bash -c "$S3 s3 ls | grep -q ' test$'"
check "head-bucket on existing bucket"          api head-bucket --bucket test
expect_fail "head-bucket on missing bucket"     "404" api head-bucket --bucket missing
expect_fail "mb existing bucket"                "BucketAlreadyOwnedByYou" s3 mb s3://test
expect_fail "mb invalid name"                   "InvalidBucketName" api create-bucket --bucket Bad_Name
check "get-bucket-location"                     api get-bucket-location --bucket test
check "get-bucket-versioning (not versioned)"   api get-bucket-versioning --bucket test
expect_fail "put-bucket-versioning not supported" "NotImplemented" \
  api put-bucket-versioning --bucket test --versioning-configuration Status=Enabled

echo "Uploads and downloads"
check "cp uploads a small file"                 s3 cp "$WORK/small.bin" s3://test/small.bin
check "cp downloads it unchanged"               bash -c "$S3 s3 cp s3://test/small.bin '$WORK/small.out' && cmp '$WORK/small.bin' '$WORK/small.out'"
check "cp large file (multipart upload)"        s3 cp "$WORK/big.bin" s3://test/big.bin
check "cp large file downloads unchanged"       bash -c "$S3 s3 cp s3://test/big.bin '$WORK/big.out' && cmp '$WORK/big.bin' '$WORK/big.out'"
check "multipart ETag has part count"           bash -c "$S3 s3api head-object --bucket test --key big.bin | grep -q 'ETag.*-[0-9]'"
check "empty file round trip"                   bash -c "$S3 s3 cp '$WORK/empty.bin' s3://test/empty && $S3 s3 cp s3://test/empty '$WORK/empty.out' && [ ! -s '$WORK/empty.out' ]"
check "cp from stdin and to stdout"             bash -c "echo streamed | $S3 s3 cp - s3://test/stream.txt && [ \"\$($S3 s3 cp s3://test/stream.txt -)\" = streamed ]"
check "overwrite replaces the content"          bash -c "$S3 s3 cp '$WORK/hello.txt' s3://test/small.bin && [ \"\$($S3 s3 cp s3://test/small.bin -)\" = 'hello ibs' ]"
for key in "with space.txt" "plus+sign.txt" "ünïcödé/файл.txt" "a/b/c/deep.txt" "percent%20.txt"; do
  check "key '$key' round trip"                 bash -c "$S3 s3 cp '$WORK/hello.txt' 's3://test/$key' && [ \"\$($S3 s3 cp 's3://test/$key' -)\" = 'hello ibs' ]"
done

echo "Object metadata"
check "put-object with content type and metadata" api put-object --bucket test --key meta.txt --body "$WORK/hello.txt" \
  --content-type text/plain --metadata color=blue,size=small
check "head-object returns content type"        bash -c "$S3 s3api head-object --bucket test --key meta.txt | grep -q '\"ContentType\": \"text/plain\"'"
check "head-object returns user metadata"       bash -c "$S3 s3api head-object --bucket test --key meta.txt | grep -q '\"color\": \"blue\"'"
MD5=$(openssl md5 -binary "$WORK/hello.txt" | base64)
check "put-object with correct Content-MD5"     api put-object --bucket test --key md5.txt --body "$WORK/hello.txt" --content-md5 "$MD5"
expect_fail "put-object with wrong Content-MD5" "BadDigest" \
  api put-object --bucket test --key md5bad.txt --body "$WORK/small.bin" --content-md5 "$MD5"
check "put-object with STANDARD storage class"  api put-object --bucket test --key std.txt --body "$WORK/hello.txt" --storage-class STANDARD
expect_fail "other storage classes are refused" "InvalidStorageClass" \
  api put-object --bucket test --key glacier.txt --body "$WORK/hello.txt" --storage-class GLACIER
expect_fail "head-object on missing key"        "404" api head-object --bucket test --key nope

echo "Ranges and conditions"
check "get-object byte range"                   bash -c "$S3 s3api get-object --bucket test --key big.bin --range bytes=1000-1999 '$WORK/range.out' >/dev/null && [ \$(wc -c <'$WORK/range.out') -eq 1000 ] && dd if='$WORK/big.bin' bs=1000 skip=1 count=1 2>/dev/null | cmp -s - '$WORK/range.out'"
check "get-object suffix range"                 bash -c "$S3 s3api get-object --bucket test --key big.bin --range bytes=-100 '$WORK/tail.out' >/dev/null && tail -c 100 '$WORK/big.bin' | cmp -s - '$WORK/tail.out'"
expect_fail "range beyond the end"              "InvalidRange" api get-object --bucket test --key small.bin --range bytes=999999- "$WORK/x"
ETAG=$($S3 s3api head-object --bucket test --key meta.txt --query ETag --output text)
check "get-object If-Match current ETag"        api get-object --bucket test --key meta.txt --if-match "$ETAG" "$WORK/x"
expect_fail "get-object If-Match other ETag"    "PreconditionFailed" api get-object --bucket test --key meta.txt --if-match '"0000"' "$WORK/x"
expect_fail "get-object If-None-Match same ETag" "304" api get-object --bucket test --key meta.txt --if-none-match "$ETAG" "$WORK/x"
check "get-object response-content-type override" bash -c "$S3 s3api get-object --bucket test --key meta.txt --response-content-type application/x-test '$WORK/x' | grep -q application/x-test"

echo "Presigned URLs"
check "presigned GET URL works with curl"       bash -c "curl -sf \"\$($S3 s3 presign s3://test/meta.txt --expires-in 60)\" | grep -q 'hello ibs'"
check "unsigned request is refused"             bash -c "[ \$(curl -s -o /dev/null -w '%{http_code}' $IBS_S3/test/meta.txt) = 403 ]"

echo "Copy and move"
check "cp within a bucket"                      bash -c "$S3 s3 cp s3://test/meta.txt s3://test/copy.txt && [ \"\$($S3 s3 cp s3://test/copy.txt -)\" = 'hello ibs' ]"
check "copy keeps metadata"                     bash -c "$S3 s3api head-object --bucket test --key copy.txt | grep -q '\"color\": \"blue\"'"
check "get-object-tagging (no tags)"            bash -c "$S3 s3api get-object-tagging --bucket test --key meta.txt | grep -q TagSet"
check "copy-object with REPLACE metadata"       bash -c "$S3 s3api copy-object --bucket test --key replaced.txt --copy-source test/meta.txt --metadata-directive REPLACE --metadata color=red >/dev/null && $S3 s3api head-object --bucket test --key replaced.txt | grep -q '\"color\": \"red\"'"
check "cp large object within a bucket (part copy)" bash -c "$S3 s3 cp s3://test/big.bin s3://test/big-copy.bin && $S3 s3 cp s3://test/big-copy.bin '$WORK/bigcopy.out' && cmp '$WORK/big.bin' '$WORK/bigcopy.out'"
check "upload-part-copy with a source range"    part_copy_range
check "cp across buckets"                       bash -c "$S3 s3 cp s3://test/big.bin s3://other/big.bin && $S3 s3 cp s3://other/big.bin '$WORK/other.out' && cmp '$WORK/big.bin' '$WORK/other.out'"
check "mv moves an object"                      bash -c "$S3 s3 mv s3://test/copy.txt s3://test/moved.txt && ! $S3 s3api head-object --bucket test --key copy.txt >/dev/null 2>&1 && $S3 s3api head-object --bucket test --key moved.txt >/dev/null"
expect_fail "copy from missing key"             "NoSuchKey" api copy-object --bucket test --key x --copy-source test/missing

echo "Listing"
check "cp --recursive uploads a tree"           s3 cp "$WORK/tree" s3://test/tree --recursive
check "ls shows folders and files"              bash -c "out=\$($S3 s3 ls s3://test/tree/); grep -q 'PRE sub/' <<<\"\$out\" && grep -q 'a.txt' <<<\"\$out\""
check "ls --recursive lists all keys"           bash -c "[ \$($S3 s3 ls s3://test/tree --recursive | wc -l) -eq 5 ]"
check "list-objects-v2 with delimiter"          bash -c "$S3 s3api list-objects-v2 --bucket test --prefix tree/ --delimiter / | grep -q '\"Prefix\": \"tree/sub/\"'"
for i in $(seq -w 1 25); do echo "$i" >"$WORK/page-$i"; done
check "upload 25 objects"                       s3 cp "$WORK" s3://test/page/ --recursive --exclude '*' --include 'page-*'
check "list-objects-v2 paging (page size 10)"   bash -c "[ \$($S3 s3api list-objects-v2 --bucket test --prefix page/ --page-size 10 --query 'length(Contents)') -eq 25 ]"
check "list-objects v1 paging (page size 7)"    bash -c "[ \$($S3 s3api list-objects --bucket test --prefix page/ --page-size 7 --query 'length(Contents)') -eq 25 ]"
check "list-objects-v2 start-after"             bash -c "[ \$($S3 s3api list-objects-v2 --bucket test --prefix page/ --start-after page/page-20 --query 'length(Contents)') -eq 5 ]"
expect_fail "ls missing bucket"                 "NoSuchBucket" s3 ls s3://missing

echo "Sync"
check "sync uploads a tree"                     s3 sync "$WORK/tree" s3://test/synced
check "sync again uploads nothing"              bash -c "[ -z \"\$($S3 s3 sync '$WORK/tree' s3://test/synced)\" ]"
check "sync downloads a tree"                   bash -c "$S3 s3 sync s3://test/synced '$WORK/down' && diff -r '$WORK/tree' '$WORK/down'"
check "sync --delete removes extra objects"     bash -c "rm '$WORK/tree/a.txt' && $S3 s3 sync '$WORK/tree' s3://test/synced --delete | grep -q 'delete: s3://test/synced/a.txt'"

echo "Multipart API"
UP=$($S3 s3api create-multipart-upload --bucket test --key parts.bin --query UploadId --output text)
head -c 6000000 /dev/urandom >"$WORK/part1"; head -c 1000 /dev/urandom >"$WORK/part2"
E1=$($S3 s3api upload-part --bucket test --key parts.bin --upload-id "$UP" --part-number 1 --body "$WORK/part1" --query ETag --output text)
E2=$($S3 s3api upload-part --bucket test --key parts.bin --upload-id "$UP" --part-number 2 --body "$WORK/part2" --query ETag --output text)
check "create-multipart-upload and upload-part" test -n "$UP$E1$E2"
check "list-multipart-uploads shows the upload" bash -c "$S3 s3api list-multipart-uploads --bucket test | grep -q '$UP'"
check "list-parts shows both parts"             bash -c "[ \$($S3 s3api list-parts --bucket test --key parts.bin --upload-id '$UP' --query 'length(Parts)') -eq 2 ]"
check "complete-multipart-upload"               api complete-multipart-upload --bucket test --key parts.bin --upload-id "$UP" \
  --multipart-upload "{\"Parts\":[{\"PartNumber\":1,\"ETag\":$E1},{\"PartNumber\":2,\"ETag\":$E2}]}"
check "completed object is both parts"          bash -c "$S3 s3 cp s3://test/parts.bin '$WORK/parts.out' && cat '$WORK/part1' '$WORK/part2' | cmp -s - '$WORK/parts.out'"
UP2=$($S3 s3api create-multipart-upload --bucket test --key aborted.bin --query UploadId --output text)
check "abort-multipart-upload"                  api abort-multipart-upload --bucket test --key aborted.bin --upload-id "$UP2"
expect_fail "upload-part after abort"           "NoSuchUpload" \
  api upload-part --bucket test --key aborted.bin --upload-id "$UP2" --part-number 1 --body "$WORK/part2"

echo "Access keys with limits"
# newkey JSON: creates an access key with the given settings; prints id:secret
newkey() {
  curl -sf -b "$WORK/cookies" -H 'X-IBS: 1' -H 'Content-Type: application/json' -d "$1" "$IBS_WEB/api/keys" |
    sed -n 's/.*"id":"\([^"]*\)".*"secret":"\([^"]*\)".*/\1:\2/p'
}
# as KEY COMMAND...: runs the command with that access key
as() { local k=$1; shift; AWS_ACCESS_KEY_ID=${k%%:*} AWS_SECRET_ACCESS_KEY=${k#*:} "$@"; }
export -f as
s3 cp "$WORK/hello.txt" s3://test/keys.txt >/dev/null
RO=$(newkey '{"name":"read test","access":"read","buckets":["test"]}')
OB=$(newkey '{"name":"write other","access":"full","buckets":["other"]}')
check "keys with limits are created"            bash -c "[ -n '$RO' ] && [ -n '$OB' ]"
check "a key lists only its buckets"            bash -c "out=\$(as '$RO' $S3 s3 ls) && grep -q ' test\$' <<<\"\$out\" && ! grep -q ' other\$' <<<\"\$out\""
check "a read-only key downloads"               bash -c "as '$RO' $S3 s3 cp s3://test/keys.txt - | cmp -s - '$WORK/hello.txt'"
expect_fail "a read-only key can't upload"      "AccessDenied" as "$RO" $S3 s3 cp "$WORK/hello.txt" s3://test/ro.txt
expect_fail "a read-only key can't delete"      "AccessDenied" as "$RO" $S3 s3api delete-object --bucket test --key keys.txt
expect_fail "a key can't read other buckets"    "AccessDenied" as "$RO" $S3 s3 ls s3://other/
check "a key for one bucket writes there"       as "$OB" $S3 s3 cp "$WORK/hello.txt" s3://other/ob.txt
expect_fail "but not in another bucket"         "AccessDenied" as "$OB" $S3 s3 cp "$WORK/hello.txt" s3://test/ob.txt
expect_fail "nor copy from a bucket it can't read" "403" as "$OB" $S3 s3 cp s3://test/keys.txt s3://other/copied.txt
expect_fail "nor copy-object from it"           "AccessDenied" as "$OB" $S3 s3api copy-object --bucket other --key copied.txt --copy-source test/keys.txt
expect_fail "nor create buckets"                "AccessDenied" as "$OB" $S3 s3 mb s3://made-by-limited-key
OBID=${OB%%:*}
check "a key's limits can be changed"           curl -sf -b "$WORK/cookies" -X PUT -H 'X-IBS: 1' -H 'Content-Type: application/json' \
  -d '{"name":"now read only","access":"read","buckets":["other"]}' "$IBS_WEB/api/keys/$OBID"
expect_fail "and apply at once, same secret"    "AccessDenied" as "$OB" $S3 s3 cp "$WORK/hello.txt" s3://other/ob2.txt
check "the full key still sees everything"      bash -c "out=\$($S3 s3 ls) && grep -q ' test\$' <<<\"\$out\" && grep -q ' other\$' <<<\"\$out\""

echo "Many small files (they share blocks inside IBS)"
mkdir -p "$WORK/many"
for i in $(seq 1 300); do head -c $((RANDOM % 5000 + 1)) /dev/urandom >"$WORK/many/f$i.bin"; done
blocks() { curl -sf -b "$WORK/cookies" "$IBS_WEB/api/dashboard" | python3 -c 'import json,sys; print(json.load(sys.stdin)["replication"]["blocks"])'; }
fewer_blocks() { for _ in $(seq 1 40); do [ "$(blocks)" -lt 60 ] && return 0; sleep 1; done; echo "blocks: $(blocks)"; return 1; }
check "sync uploads 300 small files"            s3 sync "$WORK/many" s3://test/many
check "they end up in far fewer blocks"         fewer_blocks
check "sync again uploads nothing"              bash -c "[ -z \"\$($S3 s3 sync '$WORK/many' s3://test/many)\" ]"
check "all download unchanged"                  bash -c "$S3 s3 sync s3://test/many '$WORK/many-down' >/dev/null && diff -r '$WORK/many' '$WORK/many-down'"
check "a range inside one of them"              bash -c "head -c 2000 /dev/urandom >'$WORK/r.bin' && $S3 s3 cp '$WORK/r.bin' s3://test/many-r.bin >/dev/null && sleep 12 && \
  $S3 s3api get-object --bucket test --key many-r.bin --range bytes=100-1099 '$WORK/r.part' >/dev/null && cmp '$WORK/r.part' <(tail -c +101 '$WORK/r.bin' | head -c 1000)"
check "overwriting and deleting some"           bash -c "for i in \$(seq 1 50); do head -c 777 /dev/urandom >'$WORK/many/f'\$i.bin; done && rm '$WORK/many'/f1[0-9][0-9].bin && \
  $S3 s3 sync '$WORK/many' s3://test/many --delete >/dev/null"
check "then all download as they are now"       bash -c "sleep 12 && $S3 s3 sync s3://test/many '$WORK/many-down2' >/dev/null && diff -r '$WORK/many' '$WORK/many-down2'"

echo "Deleting"
check "rm deletes one object"                   bash -c "$S3 s3 rm s3://test/moved.txt && ! $S3 s3api head-object --bucket test --key moved.txt >/dev/null 2>&1"
check "delete-objects deletes several"          api delete-objects --bucket test --delete '{"Objects":[{"Key":"meta.txt"},{"Key":"md5.txt"}]}'
check "rm on a missing key succeeds"            s3 rm s3://test/never-existed
check "rm --recursive deletes a prefix"         bash -c "$S3 s3 rm s3://test/page --recursive >/dev/null && [ -z \"\$($S3 s3 ls s3://test/page/)\" ]"
expect_fail "rb non-empty bucket"               "BucketNotEmpty" s3 rb s3://other
check "rb --force empties and removes"          s3 rb s3://other --force
check "rb --force on the test bucket"           s3 rb s3://test --force
check "no buckets left"                         bash -c "[ -z \"\$($S3 s3 ls)\" ]"

echo "Storage full"
check "mb for the full-storage test"            s3 mb s3://full
head -c 250000000 /dev/urandom >"$WORK/huge.bin" # beyond the site's 200M limit
expect_fail "upload beyond the storage limit"   "StorageFull" s3 cp "$WORK/huge.bin" s3://full/huge.bin

finish
