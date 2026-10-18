package site

import (
	"context"
	"database/sql"
	"time"
)

// Folder figures for the bucket browser: per folder (a key prefix ending in
// "/", at any depth), the files under it, their size and the newest change.
// S3 has no real folders, so they're kept in their own table. A folder is
// counted the first time it's shown, in the background (countFolder), with
// one statement, so nothing waits for more than that; until then the browser
// shows a spinner. From then on every file stored or deleted updates the
// counted folders it's in, in the same transaction. Deleting a file may
// remove a folder's newest one, so its latest change is then recalculated in
// the background (latest -1 until then).

// FolderInfo is what the bucket browser shows for a folder. Latest is in unix
// milliseconds; -1 while it's being recalculated.
type FolderInfo struct {
	Objects int64 `json:"objects"`
	Bytes   int64 `json:"bytes"`
	Latest  int64 `json:"latest"`
}

// ancestors lists the folders a key is in: "a/b/c" -> "a/", "a/b/".
func ancestors(key string) []string {
	var out []string
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			out = append(out, key[:i+1])
		}
	}
	return out
}

// folderStored counts a stored file in its counted folders: objects and
// bytes are what it adds (0 objects when it replaced a file of the same name).
// Folders not counted yet are counted when shown.
func folderStored(tx *sql.Tx, bucket, key string, objects, bytes, modified int64) error {
	for _, p := range ancestors(key) {
		if _, err := tx.Exec(`UPDATE folders SET objects = objects + ?, bytes = bytes + ?,
				latest = CASE WHEN latest < 0 THEN -1 ELSE max(latest, ?) END WHERE bucket = ? AND prefix = ?`,
			objects, bytes, modified, bucket, p); err != nil {
			return err
		}
	}
	return nil
}

// folderDeleted takes a deleted file out of its folders; their newest change
// is recalculated (folderLatestLoop).
func folderDeleted(tx *sql.Tx, bucket, key string, bytes int64) error {
	for _, p := range ancestors(key) {
		if _, err := tx.Exec(`UPDATE folders SET objects = objects - 1, bytes = bytes - ?, latest = -1 WHERE bucket = ? AND prefix = ?`,
			bytes, bucket, p); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM folders WHERE bucket = ? AND prefix = ? AND objects <= 0`, bucket, p); err != nil {
			return err
		}
	}
	return nil
}

// Folders returns the figures of the given folders of a bucket. Folders not
// counted yet are missing; they're counted in the background.
func (s *Site) Folders(bucket string, prefixes []string) map[string]FolderInfo {
	out := map[string]FolderInfo{}
	for _, p := range prefixes {
		var f FolderInfo
		if s.DB.QueryRow(`SELECT objects, bytes, latest FROM folders WHERE bucket = ? AND prefix = ?`, bucket, p).
			Scan(&f.Objects, &f.Bytes, &f.Latest) == nil {
			out[p] = f
			continue
		}
		if _, queued := s.folderQueued.LoadOrStore(bucket+"\x00"+p, true); !queued {
			select {
			case s.folderQueue <- [2]string{bucket, p}:
			default: // busy; asked again when shown again
				s.folderQueued.Delete(bucket + "\x00" + p)
			}
		}
	}
	return out
}

// countFolder counts a folder's files in one statement, which is atomic, so
// a file stored meanwhile is either counted here or updates the row after.
func (s *Site) countFolder(bucket, prefix string) {
	s.DB.Exec(`INSERT INTO folders(bucket, prefix, objects, bytes, latest)
		SELECT ?, ?, COUNT(*), COALESCE(SUM(size), 0), COALESCE(MAX(modified), 0)
		FROM objects WHERE bucket = ? AND key >= ? AND key < ? HAVING COUNT(*) > 0
		ON CONFLICT(bucket, prefix) DO NOTHING`, bucket, prefix, bucket, prefix, prefixEnd(prefix))
	s.folderQueued.Delete(bucket + "\x00" + prefix)
}

// folderLatestLoop recalculates the newest change of folders that lost a
// file.
// It also counts the folders the bucket browser asked for (Folders).
func (s *Site) folderLatestLoop(ctx context.Context) {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case f := <-s.folderQueue:
			s.countFolder(f[0], f[1])
			// Count what else was asked meanwhile, then tell the page.
			for more := true; more; {
				select {
				case f := <-s.folderQueue:
					s.countFolder(f[0], f[1])
				default:
					more = false
				}
			}
			s.events.notify(TopicData)
		case <-tick.C:
			if s.refreshFolderLatest() > 0 {
				s.events.notify(TopicData)
			}
		}
	}
}

// refreshFolderLatest recalculates up to 200 folders' newest change.
func (s *Site) refreshFolderLatest() int {
	rows, err := s.DB.Query(`SELECT bucket, prefix FROM folders WHERE latest < 0 LIMIT 200`)
	if err != nil {
		return 0
	}
	var todo [][2]string
	for rows.Next() {
		var b, p string
		rows.Scan(&b, &p)
		todo = append(todo, [2]string{b, p})
	}
	rows.Close()
	for _, t := range todo {
		var latest int64
		s.DB.QueryRow(`SELECT COALESCE(MAX(modified), 0) FROM objects WHERE bucket = ? AND key >= ? AND key < ?`,
			t[0], t[1], prefixEnd(t[1])).Scan(&latest)
		s.DB.Exec(`UPDATE folders SET latest = ? WHERE bucket = ? AND prefix = ? AND latest < 0`, latest, t[0], t[1])
	}
	return len(todo)
}
