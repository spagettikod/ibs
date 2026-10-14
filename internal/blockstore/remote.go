package blockstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Remote is a Store backed by a storage node's API. The client must use
// mutual TLS with the node's key pinned (see keys.ClientTLS).
type Remote struct {
	BaseURL string
	Client  *http.Client
}

func NewRemote(baseURL string, client *http.Client) *Remote {
	return &Remote{BaseURL: strings.TrimRight(baseURL, "/"), Client: client}
}

func (r *Remote) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.BaseURL+path, rd)
	if err != nil {
		return nil, err
	}
	return r.Client.Do(req)
}

func blockPath(ns, id string) string { return "/node/v1/blocks/" + ns + "/" + id }

func expect(resp *http.Response, codes ...int) error {
	for _, c := range codes {
		if resp.StatusCode == c {
			return nil
		}
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("node returned %s: %s", resp.Status, strings.TrimSpace(string(msg)))
}

func (r *Remote) Put(ctx context.Context, ns, id string, data []byte) error {
	resp, err := r.do(ctx, http.MethodPut, blockPath(ns, id), data)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return expect(resp, http.StatusNoContent, http.StatusOK)
}

func (r *Remote) Get(ctx context.Context, ns, id string) ([]byte, error) {
	resp, err := r.do(ctx, http.MethodGet, blockPath(ns, id), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if err := expect(resp, http.StatusOK); err != nil {
		return nil, err
	}
	return io.ReadAll(resp.Body)
}

func (r *Remote) Delete(ctx context.Context, ns, id string) error {
	resp, err := r.do(ctx, http.MethodDelete, blockPath(ns, id), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return expect(resp, http.StatusNoContent, http.StatusOK)
}

func (r *Remote) Has(ctx context.Context, ns, id string) (bool, error) {
	resp, err := r.do(ctx, http.MethodHead, blockPath(ns, id), nil)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	return true, expect(resp, http.StatusOK)
}

type StatResponse struct {
	Capacity int64 `json:"capacity"`
	Free     int64 `json:"free"`
}

func (r *Remote) getJSON(ctx context.Context, path string, out any) error {
	resp, err := r.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := expect(resp, http.StatusOK); err != nil {
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (r *Remote) Namespaces(ctx context.Context) ([]string, error) {
	var out []string
	return out, r.getJSON(ctx, "/node/v1/namespaces", &out)
}

func (r *Remote) List(ctx context.Context, ns string) ([]string, error) {
	var out []string
	return out, r.getJSON(ctx, "/node/v1/blocks/"+ns, &out)
}

func (r *Remote) Stat(ctx context.Context) (int64, int64, error) {
	resp, err := r.do(ctx, http.MethodGet, "/node/v1/stat", nil)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if err := expect(resp, http.StatusOK); err != nil {
		return 0, 0, err
	}
	var st StatResponse
	err = json.NewDecoder(resp.Body).Decode(&st)
	return st.Capacity, st.Free, err
}

// Handler serves a Store for the manager. It must be served over TLS that
// only admits the manager's key (see keys.ServerTLS).
//
// meta, when set, also serves the site's encrypted metadata snapshot.
func Handler(store Store, meta *MetaFile) http.Handler {
	mux := http.NewServeMux()
	if meta != nil {
		metadataHandlers(mux, meta)
	}
	mux.HandleFunc("POST /node/v1/verify/{ns}", verifyHandler(store))
	mux.HandleFunc("GET /node/v1/stat", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, f, err := store.Stat(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(StatResponse{Capacity: c, Free: f})
	}))
	mux.HandleFunc("GET /node/v1/namespaces", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		list, err := store.Namespaces(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(append([]string{}, list...))
	}))
	mux.HandleFunc("GET /node/v1/blocks/{ns}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		list, err := store.List(r.Context(), r.PathValue("ns"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(append([]string{}, list...))
	}))
	mux.HandleFunc("PUT /node/v1/blocks/{ns}/{id}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := store.Put(r.Context(), r.PathValue("ns"), r.PathValue("id"), data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("GET /node/v1/blocks/{ns}/{id}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := store.Get(r.Context(), r.PathValue("ns"), r.PathValue("id"))
		if err == ErrNotFound {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Write(data)
	}))
	mux.HandleFunc("HEAD /node/v1/blocks/{ns}/{id}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, err := store.Has(r.Context(), r.PathValue("ns"), r.PathValue("id"))
		switch {
		case err != nil:
			w.WriteHeader(http.StatusInternalServerError)
		case !ok:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	mux.HandleFunc("DELETE /node/v1/blocks/{ns}/{id}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := store.Delete(r.Context(), r.PathValue("ns"), r.PathValue("id")); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	return mux
}
