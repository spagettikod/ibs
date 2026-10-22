package site

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"filippo.io/age"

	"ibs/internal/blockcodec"
	"ibs/internal/db"
	"ibs/internal/keys"
)

// Export writes a site's files out to a folder, without a running site or
// other sites: from the recovery phrase, the site's index (its database, or
// an encrypted metadata backup as nodes and other sites keep it) and folders
// holding its blocks (the site's own, a node's, or another site's copies).

// ExportOptions say what to export from where.
type ExportOptions struct {
	Phrase    string
	Index     string   // the site's database, or a metadata backup
	Blocks    []string // folders to search for blocks, at any depth
	Bucket    string   // only this bucket; "" for all
	Out       string   // where to write bucket/key
	PrintKeys bool     // only print the keys, for the age tool
	Log       io.Writer
}

// ExportResult says what was written, and the files that couldn't be,
// because blocks were missing or damaged.
type ExportResult struct {
	Files   int
	Bytes   int64
	Missing []string
}

var blockName = regexp.MustCompile(`^[0-9a-f]{64}$`)

func Export(opt ExportOptions) (*ExportResult, error) {
	seed, err := keys.DecodePhrase(opt.Phrase)
	if err != nil {
		return nil, err
	}
	id, err := keys.FromSeed(seed)
	if err != nil {
		return nil, err
	}
	index, cleanup, err := openIndex(id, opt.Index)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	// Each bucket's keys, which also proves the index belongs to the phrase.
	buckets := map[string]*bucketKeys{}
	rows, err := index.Query(`SELECT name, age_key FROM buckets ORDER BY name`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var name string
		var ageKey []byte
		rows.Scan(&name, &ageKey)
		if opt.Bucket != "" && name != opt.Bucket {
			continue
		}
		k, err := unwrapBucketKeys(id, name, ageKey)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("bucket %s: %w (is this the index of the site with this recovery phrase?)", name, err)
		}
		buckets[name] = k
	}
	rows.Close()
	if opt.Bucket != "" && buckets[opt.Bucket] == nil {
		return nil, fmt.Errorf("no bucket %q in the index", opt.Bucket)
	}

	if opt.PrintKeys {
		fmt.Fprintf(opt.Log, "# site (opens the index backup and wraps the bucket keys)\n%s\n", id.Age)
		for name, k := range buckets {
			fmt.Fprintf(opt.Log, "# bucket %s\n%s\n", name, k.age)
		}
		return &ExportResult{}, nil
	}

	// Where each block is: any file named by a block ID in the folders.
	where := map[string]string{}
	for _, dir := range opt.Blocks {
		filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && blockName.MatchString(d.Name()) {
				if _, ok := where[d.Name()]; !ok {
					where[d.Name()] = p
				}
			}
			return nil
		})
	}

	res := &ExportResult{}
	rows, err = index.Query(`SELECT bucket, key, size, modified, blocks FROM objects ORDER BY bucket, key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var bucket, key, raw string
		var size, modified int64
		rows.Scan(&bucket, &key, &size, &modified, &raw)
		k := buckets[bucket]
		if k == nil {
			continue
		}
		dest, err := exportPath(opt.Out, bucket, key)
		if err != nil {
			res.Missing = append(res.Missing, bucket+"/"+key+": "+err.Error())
			continue
		}
		if err := exportFile(k, decodeBlocks(raw), where, dest, size); err != nil {
			os.Remove(dest)
			res.Missing = append(res.Missing, bucket+"/"+key+": "+err.Error())
			fmt.Fprintf(opt.Log, "missing  %s/%s: %v\n", bucket, key, err)
			continue
		}
		mt := time.UnixMilli(modified)
		os.Chtimes(dest, mt, mt)
		res.Files++
		res.Bytes += size
	}
	return res, rows.Err()
}

// openIndex opens a copy of the index, never the site's live database:
// the database itself, or a decrypted metadata backup. The copy gets the
// columns of this version.
func openIndex(id *keys.Identity, path string) (*sql.DB, func(), error) {
	tmp, err := os.MkdirTemp("", "ibs-export-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { os.RemoveAll(tmp) }
	copyPath := filepath.Join(tmp, "index.db")
	head := make([]byte, 16)
	f, err := os.Open(path)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	io.ReadFull(f, head)
	f.Close()
	if bytes.HasPrefix(head, []byte("SQLite format 3\x00")) {
		ro, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
		if err == nil {
			_, err = ro.Exec(`VACUUM INTO ?`, copyPath)
			ro.Close()
		}
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("copy the index: %w", err)
		}
	} else {
		blob, err := os.ReadFile(path)
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		raw, err := openBackup(id, blob)
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("the index is neither a site database nor a metadata backup of this recovery phrase: %w", err)
		}
		if err := os.WriteFile(copyPath, raw, 0o600); err != nil {
			cleanup()
			return nil, nil, err
		}
	}
	d, err := db.Open(copyPath)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return d, func() { d.Close(); cleanup() }, nil
}

// unwrapBucketKeys opens a bucket's key with the site's.
func unwrapBucketKeys(id *keys.Identity, name string, ageKey []byte) (*bucketKeys, error) {
	raw, err := keys.OpenAge(id.Age, "bucket:"+name, ageKey)
	if err != nil {
		return nil, err
	}
	k, err := age.ParseHybridIdentity(string(raw))
	if err != nil {
		return nil, err
	}
	return &bucketKeys{age: k}, nil
}

// exportPath is where a file goes: out/bucket/key, never outside out.
func exportPath(out, bucket, key string) (string, error) {
	p := filepath.Join(out, bucket, filepath.FromSlash(key))
	if rel, err := filepath.Rel(out, p); err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", errors.New("key would be written outside the export folder")
	}
	return p, nil
}

func exportFile(k *bucketKeys, refs []BlockRef, where map[string]string, dest string, size int64) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	var written int64
	for _, r := range refs {
		p, ok := where[r.ID]
		if !ok {
			return fmt.Errorf("block %s not in the folders given", r.ID[:12])
		}
		ct, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		plain, err := decodeEntry(k, r, ct)
		if err != nil {
			return fmt.Errorf("block %s: %w", r.ID[:12], err)
		}
		if _, err := f.Write(plain); err != nil {
			return err
		}
		written += int64(len(plain))
	}
	if written != size {
		return fmt.Errorf("wrote %d bytes, the index says %d", written, size)
	}
	return f.Close()
}

// decodeEntry decrypts a file's part from its whole block.
func decodeEntry(k *bucketKeys, ref BlockRef, ct []byte) ([]byte, error) {
	if ref.Pos != nil {
		return blockcodec.DecryptAgeRange(k.age, ref.ID, ct, *ref.Pos, ref.Size)
	}
	return decryptBlock(k, ref.ID, ct)
}
