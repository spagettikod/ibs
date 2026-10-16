package site

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"ibs/internal/blockcodec"
	"ibs/internal/keys"
)

var (
	ErrNoSuchBucket      = errors.New("bucket does not exist")
	ErrNoSuchKey         = errors.New("object does not exist")
	ErrBucketExists      = errors.New("bucket already exists")
	ErrBucketNotEmpty    = errors.New("bucket is not empty")
	ErrInvalidBucketName = errors.New("invalid bucket name")
	ErrNoSuchUpload      = errors.New("multipart upload does not exist")
	ErrInvalidPart       = errors.New("one or more parts could not be found or have a different etag")
	ErrInvalidPartOrder  = errors.New("parts must be listed in ascending order")
	ErrInvalidRange      = errors.New("requested range is not satisfiable")
)

// Copies is how many other sites keep a copy of every file. With the copy at
// this site, each file exists three times. With fewer other sites online,
// files have as many copies as there are sites, and the rest follow.
const Copies = 2

type BlockRef struct {
	ID   string `json:"id"`
	Size int64  `json:"size"` // plaintext bytes
}

type ObjectInfo struct {
	Bucket      string            `json:"bucket"`
	Key         string            `json:"key"`
	Size        int64             `json:"size"`
	ETag        string            `json:"etag"`
	ContentType string            `json:"content_type"`
	Modified    time.Time         `json:"modified"`
	Meta        map[string]string `json:"meta,omitempty"`
	Blocks      []BlockRef        `json:"-"`
}

type BucketInfo struct {
	Name        string    `json:"name"`
	Created     time.Time `json:"created"`
	Replication int       `json:"replication"`
	Objects     int64     `json:"objects"`
	Size        int64     `json:"size"`
	Pending     int64     `json:"pending"` // blocks still missing replicas
}

var bucketNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

func ValidBucketName(name string) bool {
	return bucketNameRe.MatchString(name) && !strings.Contains(name, "..") &&
		!regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+$`).MatchString(name)
}

// ---- Buckets ----

func (s *Site) CreateBucket(name string) error {
	if !ValidBucketName(name) {
		return ErrInvalidBucketName
	}
	wrapped := keys.Seal(s.ID.Master, keys.RandomKey(), []byte("bucket:"+name))
	_, err := s.DB.Exec(`INSERT INTO buckets(name, created, replication, wrapped_key) VALUES(?, ?, ?, ?)`,
		name, time.Now().Unix(), Copies, wrapped)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return ErrBucketExists
		}
		return err
	}
	s.markDirty()
	return nil
}

func (s *Site) DeleteBucket(name string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM buckets WHERE name = ?`, name).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return ErrNoSuchBucket
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM objects WHERE bucket = ?`, name).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrBucketNotEmpty
	}
	// Abort any multipart uploads still open in the bucket.
	rows, err := tx.Query(`SELECT p.blocks FROM parts p JOIN uploads u ON u.id = p.upload_id WHERE u.bucket = ?`, name)
	if err != nil {
		return err
	}
	var partBlocks []string
	for rows.Next() {
		var b string
		rows.Scan(&b)
		partBlocks = append(partBlocks, b)
	}
	rows.Close()
	for _, b := range partBlocks {
		if err := adjustRefs(tx, decodeBlocks(b), -1); err != nil {
			return err
		}
	}
	for _, q := range []string{
		`DELETE FROM parts WHERE upload_id IN (SELECT id FROM uploads WHERE bucket = ?)`,
		`DELETE FROM uploads WHERE bucket = ?`,
		`DELETE FROM buckets WHERE name = ?`,
	} {
		if _, err := tx.Exec(q, name); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.bucketKeys.Delete(name)
	s.markDirty()
	return nil
}

func (s *Site) ListBuckets() ([]BucketInfo, error) {
	rows, err := s.DB.Query(`SELECT b.name, b.created, b.replication,
			(SELECT COUNT(*) FROM objects o WHERE o.bucket = b.name),
			(SELECT COALESCE(SUM(size), 0) FROM objects o WHERE o.bucket = b.name)
		FROM buckets b ORDER BY b.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BucketInfo
	for rows.Next() {
		var b BucketInfo
		var created int64
		if err := rows.Scan(&b.Name, &created, &b.Replication, &b.Objects, &b.Size); err != nil {
			return nil, err
		}
		b.Created = time.Unix(created, 0).UTC()
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	pending := map[string]int64{}
	pr, err := s.DB.Query(`SELECT bucket, COUNT(*) FROM (
			SELECT b.bucket, bk.replication,
				(SELECT COUNT(*) FROM replicas r JOIN peers p ON p.site_id = r.site_id
					WHERE r.block_id = b.id AND r.state = 'ok' AND p.last_seen >= ?) AS n
			FROM blocks b JOIN buckets bk ON bk.name = b.bucket WHERE b.refs > 0)
		WHERE n < replication GROUP BY bucket`, time.Now().Add(-s.cfg.PeerGrace).Unix())
	if err != nil {
		return nil, err
	}
	defer pr.Close()
	for pr.Next() {
		var name string
		var n int64
		pr.Scan(&name, &n)
		pending[name] = n
	}
	for i := range out {
		out[i].Pending = pending[out[i].Name]
	}
	return out, pr.Err()
}

func (s *Site) BucketExists(name string) bool {
	_, err := s.bucketKey(name)
	return err == nil
}

func (s *Site) bucketKey(name string) ([]byte, error) {
	if k, ok := s.bucketKeys.Load(name); ok {
		return k.([]byte), nil
	}
	var wrapped []byte
	err := s.DB.QueryRow(`SELECT wrapped_key FROM buckets WHERE name = ?`, name).Scan(&wrapped)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoSuchBucket
	}
	if err != nil {
		return nil, err
	}
	k, err := keys.Open(s.ID.Master, wrapped, []byte("bucket:"+name))
	if err != nil {
		return nil, fmt.Errorf("unwrap bucket key: %w", err)
	}
	s.bucketKeys.Store(name, k)
	return k, nil
}

// ---- Writing ----

type writeResult struct {
	upload string // the blocks belong to it until committed
	blocks []BlockRef
	size   int64
	md5    []byte
}

// writeBlocks splits a stream into blocks, encrypts them and stores them
// locally. The blocks are unreferenced until the caller commits them.
//
// The blocks belong to the upload (res.upload) until they're committed with
// commitBlocks or given up with releaseUpload; on an error they're given up
// here.
func (s *Site) writeBlocks(ctx context.Context, bucket string, r io.Reader) (res *writeResult, err error) {
	key, err := s.bucketKey(bucket)
	if err != nil {
		return nil, err
	}
	h := md5.New()
	buf := make([]byte, blockcodec.BlockSize)
	upload := keys.RandomToken(12)
	res = &writeResult{upload: upload}
	defer func() {
		if err != nil {
			s.releaseUpload(upload)
		}
	}()
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			plain := buf[:n]
			h.Write(plain)
			id, ct := blockcodec.Encrypt(key, plain)
			if err := s.storeBlock(ctx, bucket, res.upload, id, ct); err != nil {
				return nil, fmt.Errorf("store block: %w", err)
			}
			res.blocks = append(res.blocks, BlockRef{ID: id, Size: int64(n)})
			res.size += int64(n)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	res.md5 = h.Sum(nil)
	return res, nil
}

// errBlockGone means a block an upload wrote isn't there any more; the upload
// fails rather than store a file with a hole in it.
var errBlockGone = errors.New("a block of this upload is missing; nothing was stored, try again")

// adjustRefs adds delta references to blocks. Taking a reference (delta > 0)
// also ends the blocks' upload, and requires every block to exist.
func adjustRefs(tx *sql.Tx, blocks []BlockRef, delta int) error {
	for _, b := range blocks {
		res, err := tx.Exec(`UPDATE blocks SET refs = refs + ?, upload = CASE WHEN ? > 0 THEN '' ELSE upload END WHERE id = ?`,
			delta, delta, b.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 && delta > 0 {
			return errBlockGone
		}
	}
	return nil
}

// releaseUpload gives up the blocks of an upload that didn't commit; the
// garbage collector deletes them.
func (s *Site) releaseUpload(upload string) {
	s.DB.Exec(`UPDATE blocks SET upload = '' WHERE upload = ?`, upload)
}

func encodeBlocks(b []BlockRef) string {
	if b == nil {
		b = []BlockRef{}
	}
	out, _ := json.Marshal(b)
	return string(out)
}

func decodeBlocks(s string) []BlockRef {
	var b []BlockRef
	json.Unmarshal([]byte(s), &b)
	return b
}

// commitObject atomically replaces an object's metadata and moves block
// references from the old version to the new one.
func (s *Site) commitObject(tx *sql.Tx, o *ObjectInfo) error {
	var old string
	err := tx.QueryRow(`SELECT blocks FROM objects WHERE bucket = ? AND key = ?`, o.Bucket, o.Key).Scan(&old)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM buckets WHERE name = ?`, o.Bucket).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return ErrNoSuchBucket
	}
	if err := adjustRefs(tx, o.Blocks, 1); err != nil {
		return err
	}
	if old != "" {
		if err := adjustRefs(tx, decodeBlocks(old), -1); err != nil {
			return err
		}
	}
	meta, _ := json.Marshal(o.Meta)
	_, err = tx.Exec(`INSERT INTO objects(bucket, key, size, etag, content_type, modified, user_meta, blocks)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(bucket, key) DO UPDATE SET size = excluded.size, etag = excluded.etag,
			content_type = excluded.content_type, modified = excluded.modified,
			user_meta = excluded.user_meta, blocks = excluded.blocks`,
		o.Bucket, o.Key, o.Size, o.ETag, o.ContentType, o.Modified.UnixMilli(), string(meta), encodeBlocks(o.Blocks))
	return err
}

func (s *Site) inTx(f func(tx *sql.Tx) error) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := f(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.markDirty()
	return nil
}

func (s *Site) PutObject(ctx context.Context, bucket, key string, r io.Reader, contentType string, meta map[string]string) (*ObjectInfo, error) {
	if key == "" || len(key) > 1024 {
		return nil, errors.New("invalid object key")
	}
	wr, err := s.writeBlocks(ctx, bucket, r)
	if err != nil {
		return nil, err
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	o := &ObjectInfo{
		Bucket: bucket, Key: key, Size: wr.size, ETag: `"` + hex.EncodeToString(wr.md5) + `"`,
		ContentType: contentType, Modified: time.Now().UTC().Truncate(time.Millisecond), Meta: meta, Blocks: wr.blocks,
	}
	if err := s.inTx(func(tx *sql.Tx) error { return s.commitObject(tx, o) }); err != nil {
		s.releaseUpload(wr.upload)
		return nil, err
	}
	return o, nil
}

func (s *Site) DeleteObject(bucket, key string) error {
	if !s.BucketExists(bucket) {
		return ErrNoSuchBucket
	}
	return s.inTx(func(tx *sql.Tx) error {
		var blocks string
		err := tx.QueryRow(`SELECT blocks FROM objects WHERE bucket = ? AND key = ?`, bucket, key).Scan(&blocks)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // S3 delete is idempotent
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM objects WHERE bucket = ? AND key = ?`, bucket, key); err != nil {
			return err
		}
		return adjustRefs(tx, decodeBlocks(blocks), -1)
	})
}

// CopyObject copies an object: its data is written again as new blocks, so
// each file owns its blocks, as on a disk.
func (s *Site) CopyObject(ctx context.Context, srcBucket, srcKey, dstBucket, dstKey string, replaceMeta bool, contentType string, meta map[string]string) (*ObjectInfo, error) {
	src, err := s.HeadObject(srcBucket, srcKey)
	if err != nil {
		return nil, err
	}
	if !s.BucketExists(dstBucket) {
		return nil, ErrNoSuchBucket
	}
	if !replaceMeta {
		contentType, meta = src.ContentType, src.Meta
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(s.WriteObject(ctx, pw, src, 0, src.Size)) }()
	o, err := s.PutObject(ctx, dstBucket, dstKey, pr, contentType, meta)
	pr.CloseWithError(err)
	return o, err
}

// ---- Reading ----

func (s *Site) HeadObject(bucket, key string) (*ObjectInfo, error) {
	if !s.BucketExists(bucket) {
		return nil, ErrNoSuchBucket
	}
	o := &ObjectInfo{Bucket: bucket, Key: key}
	var modified int64
	var meta, blocks string
	err := s.DB.QueryRow(`SELECT size, etag, content_type, modified, user_meta, blocks FROM objects WHERE bucket = ? AND key = ?`,
		bucket, key).Scan(&o.Size, &o.ETag, &o.ContentType, &modified, &meta, &blocks)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoSuchKey
	}
	if err != nil {
		return nil, err
	}
	o.Modified = time.UnixMilli(modified).UTC()
	json.Unmarshal([]byte(meta), &o.Meta)
	o.Blocks = decodeBlocks(blocks)
	return o, nil
}

// WriteObject streams length bytes of the object starting at offset to w.
// The next block is fetched while the current one is being written.
func (s *Site) WriteObject(ctx context.Context, w io.Writer, o *ObjectInfo, offset, length int64) error {
	if offset < 0 || length < 0 || offset+length > o.Size {
		return ErrInvalidRange
	}
	key, err := s.bucketKey(o.Bucket)
	if err != nil {
		return err
	}
	type piece struct {
		ref        BlockRef
		start, end int64 // slice of the block's plaintext to emit
	}
	var pieces []piece
	var pos int64
	for _, b := range o.Blocks {
		bStart, bEnd := pos, pos+b.Size
		pos = bEnd
		lo, hi := max(offset, bStart), min(offset+length, bEnd)
		if lo < hi {
			pieces = append(pieces, piece{b, lo - bStart, hi - bStart})
		}
	}
	type result struct {
		data []byte
		err  error
	}
	fetch := func(p piece) <-chan result {
		ch := make(chan result, 1)
		go func() {
			ct, err := s.readBlock(ctx, p.ref.ID)
			if err != nil {
				ch <- result{err: err}
				return
			}
			plain, err := blockcodec.Decrypt(key, p.ref.ID, ct)
			ch <- result{plain, err}
		}()
		return ch
	}
	var next <-chan result
	if len(pieces) > 0 {
		next = fetch(pieces[0])
	}
	for i, p := range pieces {
		res := <-next
		if i+1 < len(pieces) {
			next = fetch(pieces[i+1])
		}
		if res.err != nil {
			return res.err
		}
		if int64(len(res.data)) < p.end {
			return fmt.Errorf("block %s shorter than expected", p.ref.ID[:12])
		}
		if _, err := w.Write(res.data[p.start:p.end]); err != nil {
			return err
		}
	}
	return nil
}

// ---- Listing ----

type ListResult struct {
	Objects        []ObjectInfo
	CommonPrefixes []string
	Truncated      bool
	NextMarker     string
}

// prefixEnd returns the smallest string greater than every string with the
// given prefix, or "" when there is none.
func prefixEnd(p string) string {
	b := []byte(p)
	for len(b) > 0 && b[len(b)-1] == 0xff {
		b = b[:len(b)-1]
	}
	if len(b) == 0 {
		return ""
	}
	b[len(b)-1]++
	return string(b)
}

func (s *Site) ListObjects(bucket, prefix, delimiter, marker string, maxKeys int) (*ListResult, error) {
	if !s.BucketExists(bucket) {
		return nil, ErrNoSuchBucket
	}
	if maxKeys <= 0 || maxKeys > 1000 {
		maxKeys = 1000
	}
	res := &ListResult{}
	from, inclusive := prefix, true
	if marker > from {
		from, inclusive = marker, false
		// Resuming after a common prefix skips everything under it.
		if delimiter != "" && strings.HasSuffix(marker, delimiter) && strings.HasPrefix(marker, prefix) {
			if end := prefixEnd(marker); end != "" {
				from, inclusive = end, true
			}
		}
	}
	upper := prefixEnd(prefix)
	count := 0
	lastPrefix := ""
	for {
		op := ">"
		if inclusive {
			op = ">="
		}
		q := `SELECT key, size, etag, content_type, modified FROM objects WHERE bucket = ? AND key ` + op + ` ?`
		args := []any{bucket, from}
		if upper != "" {
			q += ` AND key < ?`
			args = append(args, upper)
		}
		q += ` ORDER BY key LIMIT 1000`
		rows, err := s.DB.Query(q, args...)
		if err != nil {
			return nil, err
		}
		var batch []ObjectInfo
		for rows.Next() {
			o := ObjectInfo{Bucket: bucket}
			var modified int64
			if err := rows.Scan(&o.Key, &o.Size, &o.ETag, &o.ContentType, &modified); err != nil {
				rows.Close()
				return nil, err
			}
			o.Modified = time.UnixMilli(modified).UTC()
			batch = append(batch, o)
		}
		rows.Close()
		if len(batch) == 0 {
			return res, nil
		}
		skipped := false
		for _, o := range batch {
			if delimiter != "" {
				if i := strings.Index(o.Key[len(prefix):], delimiter); i >= 0 {
					cp := o.Key[:len(prefix)+i+len(delimiter)]
					if cp == lastPrefix {
						continue
					}
					if count == maxKeys {
						res.Truncated = true
						return res, nil
					}
					res.CommonPrefixes = append(res.CommonPrefixes, cp)
					res.NextMarker, lastPrefix = cp, cp
					count++
					if end := prefixEnd(cp); end != "" {
						from, inclusive, skipped = end, true, true
						break
					}
					continue
				}
			}
			if count == maxKeys {
				res.Truncated = true
				return res, nil
			}
			res.Objects = append(res.Objects, o)
			res.NextMarker = o.Key
			count++
		}
		if !skipped {
			from, inclusive = batch[len(batch)-1].Key, false
		}
	}
}

// ---- Multipart uploads ----

type PartInfo struct {
	Number   int       `json:"number"`
	ETag     string    `json:"etag"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

type UploadInfo struct {
	ID      string
	Bucket  string
	Key     string
	Created time.Time
}

func (s *Site) CreateUpload(bucket, key, contentType string, meta map[string]string) (string, error) {
	if !s.BucketExists(bucket) {
		return "", ErrNoSuchBucket
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	id := keys.RandomToken(24)
	m, _ := json.Marshal(meta)
	_, err := s.DB.Exec(`INSERT INTO uploads(id, bucket, key, created, content_type, user_meta) VALUES(?, ?, ?, ?, ?, ?)`,
		id, bucket, key, time.Now().Unix(), contentType, string(m))
	return id, err
}

func (s *Site) uploadBucket(uploadID, bucket, key string) error {
	var b, k string
	err := s.DB.QueryRow(`SELECT bucket, key FROM uploads WHERE id = ?`, uploadID).Scan(&b, &k)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (b != bucket || k != key) {
		return ErrNoSuchUpload
	}
	return err
}

func (s *Site) UploadPart(ctx context.Context, bucket, key, uploadID string, num int, r io.Reader) (string, error) {
	if num < 1 || num > 10000 {
		return "", errors.New("part number must be between 1 and 10000")
	}
	if err := s.uploadBucket(uploadID, bucket, key); err != nil {
		return "", err
	}
	s.uploadActive(uploadID, 1)
	defer s.uploadActive(uploadID, -1)
	wr, err := s.writeBlocks(ctx, bucket, r)
	if err != nil {
		return "", err
	}
	etag := `"` + hex.EncodeToString(wr.md5) + `"`
	err = s.inTx(func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM uploads WHERE id = ?`, uploadID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return ErrNoSuchUpload
		}
		var old string
		err := tx.QueryRow(`SELECT blocks FROM parts WHERE upload_id = ? AND num = ?`, uploadID, num).Scan(&old)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := adjustRefs(tx, wr.blocks, 1); err != nil {
			return err
		}
		if old != "" {
			if err := adjustRefs(tx, decodeBlocks(old), -1); err != nil {
				return err
			}
		}
		_, err = tx.Exec(`INSERT INTO parts(upload_id, num, etag, size, blocks, modified) VALUES(?, ?, ?, ?, ?, ?)
			ON CONFLICT(upload_id, num) DO UPDATE SET etag = excluded.etag, size = excluded.size,
				blocks = excluded.blocks, modified = excluded.modified`,
			uploadID, num, etag, wr.size, encodeBlocks(wr.blocks), time.Now().UnixMilli())
		return err
	})
	if err != nil {
		s.releaseUpload(wr.upload)
	}
	return etag, err
}

func (s *Site) ListParts(bucket, key, uploadID string) ([]PartInfo, error) {
	if err := s.uploadBucket(uploadID, bucket, key); err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(`SELECT num, etag, size, modified FROM parts WHERE upload_id = ? ORDER BY num`, uploadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PartInfo
	for rows.Next() {
		var p PartInfo
		var m int64
		if err := rows.Scan(&p.Number, &p.ETag, &p.Size, &m); err != nil {
			return nil, err
		}
		p.Modified = time.UnixMilli(m).UTC()
		out = append(out, p)
	}
	return out, rows.Err()
}

type CompletePart struct {
	Number int
	ETag   string
}

func normalizeETag(e string) string { return strings.Trim(strings.TrimSpace(e), `"`) }

func (s *Site) CompleteUpload(bucket, key, uploadID string, parts []CompletePart) (*ObjectInfo, error) {
	if err := s.uploadBucket(uploadID, bucket, key); err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return nil, ErrInvalidPart
	}
	for i := 1; i < len(parts); i++ {
		if parts[i].Number <= parts[i-1].Number {
			return nil, ErrInvalidPartOrder
		}
	}
	var o *ObjectInfo
	err := s.inTx(func(tx *sql.Tx) error {
		var contentType, meta string
		err := tx.QueryRow(`SELECT content_type, user_meta FROM uploads WHERE id = ?`, uploadID).Scan(&contentType, &meta)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoSuchUpload
		}
		if err != nil {
			return err
		}
		stored := map[int]struct {
			etag   string
			size   int64
			blocks string
		}{}
		rows, err := tx.Query(`SELECT num, etag, size, blocks FROM parts WHERE upload_id = ?`, uploadID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var num int
			var p struct {
				etag   string
				size   int64
				blocks string
			}
			rows.Scan(&num, &p.etag, &p.size, &p.blocks)
			stored[num] = p
		}
		rows.Close()
		var all []BlockRef
		var size int64
		h := md5.New()
		for _, cp := range parts {
			p, ok := stored[cp.Number]
			if !ok || normalizeETag(p.etag) != normalizeETag(cp.ETag) {
				return ErrInvalidPart
			}
			raw, _ := hex.DecodeString(normalizeETag(p.etag))
			h.Write(raw)
			all = append(all, decodeBlocks(p.blocks)...)
			size += p.size
		}
		o = &ObjectInfo{
			Bucket: bucket, Key: key, Size: size,
			ETag:        fmt.Sprintf(`"%s-%d"`, hex.EncodeToString(h.Sum(nil)), len(parts)),
			ContentType: contentType, Modified: time.Now().UTC().Truncate(time.Millisecond), Blocks: all,
		}
		json.Unmarshal([]byte(meta), &o.Meta)
		if err := s.commitObject(tx, o); err != nil {
			return err
		}
		// Release the references held by every part, used or not.
		for _, p := range stored {
			if err := adjustRefs(tx, decodeBlocks(p.blocks), -1); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`DELETE FROM parts WHERE upload_id = ?`, uploadID); err != nil {
			return err
		}
		_, err = tx.Exec(`DELETE FROM uploads WHERE id = ?`, uploadID)
		return err
	})
	return o, err
}

func (s *Site) AbortUpload(bucket, key, uploadID string) error {
	if err := s.uploadBucket(uploadID, bucket, key); err != nil {
		return err
	}
	return s.inTx(func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT blocks FROM parts WHERE upload_id = ?`, uploadID)
		if err != nil {
			return err
		}
		var all []string
		for rows.Next() {
			var b string
			rows.Scan(&b)
			all = append(all, b)
		}
		rows.Close()
		for _, b := range all {
			if err := adjustRefs(tx, decodeBlocks(b), -1); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`DELETE FROM parts WHERE upload_id = ?`, uploadID); err != nil {
			return err
		}
		_, err = tx.Exec(`DELETE FROM uploads WHERE id = ?`, uploadID)
		return err
	})
}

// uploadActive counts parts of a multipart upload that are arriving now.
func (s *Site) uploadActive(id string, delta int) {
	s.activeMu.Lock()
	defer s.activeMu.Unlock()
	if s.activeUploads == nil {
		s.activeUploads = map[string]int{}
	}
	if s.activeUploads[id] += delta; s.activeUploads[id] <= 0 {
		delete(s.activeUploads, id)
	}
}

// removeStaleUploads aborts unfinished multipart uploads that got nothing for
// staleUpload: no part finished, and none arriving. The client that started
// them gave up; their parts would take space nobody sees. Tools start such a
// file over rather than continue it.
func (s *Site) removeStaleUploads() {
	cutoff := time.Now().Add(-staleUpload)
	rows, err := s.DB.Query(`SELECT u.id, u.bucket, u.key FROM uploads u
		WHERE u.created < ? AND COALESCE((SELECT MAX(modified) FROM parts p WHERE p.upload_id = u.id), 0) < ?`,
		cutoff.Unix(), cutoff.UnixMilli())
	if err != nil {
		return
	}
	type up struct{ id, bucket, key string }
	var stale []up
	for rows.Next() {
		var u up
		rows.Scan(&u.id, &u.bucket, &u.key)
		stale = append(stale, u)
	}
	rows.Close()
	for _, u := range stale {
		s.activeMu.Lock()
		busy := s.activeUploads[u.id] > 0
		s.activeMu.Unlock()
		if !busy && s.AbortUpload(u.bucket, u.key, u.id) == nil {
			s.log.Info("removed an unfinished upload: nothing arrived for an hour", "bucket", u.bucket, "key", u.key)
		}
	}
}

func (s *Site) ListUploads(bucket, prefix string) ([]UploadInfo, error) {
	if !s.BucketExists(bucket) {
		return nil, ErrNoSuchBucket
	}
	rows, err := s.DB.Query(`SELECT id, key, created FROM uploads WHERE bucket = ? ORDER BY key, created`, bucket)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UploadInfo
	for rows.Next() {
		u := UploadInfo{Bucket: bucket}
		var c int64
		if err := rows.Scan(&u.ID, &u.Key, &c); err != nil {
			return nil, err
		}
		u.Created = time.Unix(c, 0).UTC()
		if strings.HasPrefix(u.Key, prefix) {
			out = append(out, u)
		}
	}
	return out, rows.Err()
}

// SortedMetaKeys is a helper for deterministic header output.
func SortedMetaKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
