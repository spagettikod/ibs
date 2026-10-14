package blockstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// MetaFile keeps the latest encrypted metadata snapshot of the node's site.
// Only the manager can read or write it (the node API only admits the
// manager's key), and only the site's recovery phrase can decrypt it.
type MetaFile struct {
	Dir string
	mu  sync.Mutex
}

var ErrNoMetadata = errors.New("no metadata snapshot stored")

func (m *MetaFile) paths() (blob, gen string) {
	return filepath.Join(m.Dir, "site-metadata.bin"), filepath.Join(m.Dir, "site-metadata.gen")
}

func (m *MetaFile) Generation() (int64, error) {
	_, genPath := m.paths()
	b, err := os.ReadFile(genPath)
	if errors.Is(err, os.ErrNotExist) {
		return -1, ErrNoMetadata
	}
	if err != nil {
		return -1, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}

// Put stores a snapshot unless a newer generation is already stored.
func (m *MetaFile) Put(gen int64, blob []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, err := m.Generation(); err == nil && cur > gen {
		return fmt.Errorf("have newer generation %d", cur)
	}
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return err
	}
	blobPath, genPath := m.paths()
	for path, data := range map[string][]byte{blobPath: blob, genPath: []byte(strconv.FormatInt(gen, 10))} {
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			return err
		}
		if err := os.Rename(tmp, path); err != nil {
			return err
		}
	}
	return nil
}

func (m *MetaFile) Get() (int64, []byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	gen, err := m.Generation()
	if err != nil {
		return -1, nil, err
	}
	blobPath, _ := m.paths()
	b, err := os.ReadFile(blobPath)
	return gen, b, err
}

func metadataHandlers(mux *http.ServeMux, meta *MetaFile) {
	mux.HandleFunc("PUT /node/v1/metadata", func(w http.ResponseWriter, r *http.Request) {
		gen, err := strconv.ParseInt(r.URL.Query().Get("generation"), 10, 64)
		if err != nil {
			http.Error(w, "generation required", http.StatusBadRequest)
			return
		}
		blob, err := io.ReadAll(io.LimitReader(r.Body, 1<<30))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := meta.Put(gen, blob); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	serve := func(w http.ResponseWriter, r *http.Request) {
		gen, blob, err := meta.Get()
		if errors.Is(err, ErrNoMetadata) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("X-IBS-Generation", strconv.FormatInt(gen, 10))
		if r.Method == http.MethodGet {
			w.Write(blob)
		}
	}
	mux.HandleFunc("GET /node/v1/metadata", serve)
	mux.HandleFunc("HEAD /node/v1/metadata", serve)
}

// PutMetadata stores an encrypted metadata snapshot on the node.
func (r *Remote) PutMetadata(ctx context.Context, gen int64, blob []byte) error {
	resp, err := r.do(ctx, http.MethodPut, "/node/v1/metadata?generation="+strconv.FormatInt(gen, 10), blob)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return expect(resp, http.StatusNoContent, http.StatusConflict)
}

// MetadataGeneration returns the generation the node holds, or -1.
func (r *Remote) MetadataGeneration(ctx context.Context) (int64, error) {
	resp, err := r.do(ctx, http.MethodHead, "/node/v1/metadata", nil)
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return -1, nil
	}
	if err := expect(resp, http.StatusOK); err != nil {
		return -1, err
	}
	return strconv.ParseInt(resp.Header.Get("X-IBS-Generation"), 10, 64)
}

// GetMetadata fetches the node's snapshot.
func (r *Remote) GetMetadata(ctx context.Context) (int64, []byte, error) {
	resp, err := r.do(ctx, http.MethodGet, "/node/v1/metadata", nil)
	if err != nil {
		return -1, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return -1, nil, ErrNoMetadata
	}
	if err := expect(resp, http.StatusOK); err != nil {
		return -1, nil, err
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, resp.Body); err != nil {
		return -1, nil, err
	}
	gen, err := strconv.ParseInt(resp.Header.Get("X-IBS-Generation"), 10, 64)
	return gen, buf.Bytes(), err
}
