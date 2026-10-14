// Package blockstore stores opaque, content-addressed blocks on a node.
// Blocks live in namespaces: "own" for the site's own blocks, and the owner's
// site id for blocks held on behalf of other sites.
package blockstore

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
)

var ErrNotFound = errors.New("block not found")

const OwnNS = "own"

type Store interface {
	Put(ctx context.Context, ns, id string, data []byte) error
	Get(ctx context.Context, ns, id string) ([]byte, error)
	Delete(ctx context.Context, ns, id string) error
	Has(ctx context.Context, ns, id string) (bool, error)
	Stat(ctx context.Context) (capacity, free int64, err error)
	// Namespaces and List enumerate stored blocks, for reconciliation.
	Namespaces(ctx context.Context) ([]string, error)
	List(ctx context.Context, ns string) ([]string, error)
}

var safeName = regexp.MustCompile(`^[a-z0-9]{2,64}$`)

func ValidName(s string) bool { return safeName.MatchString(s) }

// FS stores blocks as files under Dir/<ns>/<id[:2]>/<id>.
// How much of the disk IBS may use is a per-node limit kept by the manager.
type FS struct {
	Dir string
}

func (s *FS) path(ns, id string) (string, error) {
	if !ValidName(ns) || !ValidName(id) || len(id) < 4 {
		return "", errors.New("invalid block name")
	}
	return filepath.Join(s.Dir, ns, id[:2], id), nil
}

func (s *FS) Put(_ context.Context, ns, id string, data []byte) error {
	p, err := s.path(ns, id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

func (s *FS) Get(_ context.Context, ns, id string) ([]byte, error) {
	p, err := s.path(ns, id)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return b, err
}

func (s *FS) Delete(_ context.Context, ns, id string) error {
	p, err := s.path(ns, id)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (s *FS) Has(_ context.Context, ns, id string) (bool, error) {
	p, err := s.path(ns, id)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (s *FS) Namespaces(context.Context) ([]string, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && ValidName(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out, err
}

func (s *FS) List(_ context.Context, ns string) ([]string, error) {
	if !ValidName(ns) {
		return nil, errors.New("invalid namespace")
	}
	var out []string
	err := filepath.WalkDir(filepath.Join(s.Dir, ns), func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.IsDir() && ValidName(d.Name()) && len(d.Name()) >= 4 {
			out = append(out, d.Name())
		}
		return nil
	})
	return out, err
}

// Stat reports the size of the disk holding the store and its free space.
func (s *FS) Stat(_ context.Context) (int64, int64, error) {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return 0, 0, err
	}
	return diskUsage(s.Dir)
}
