package blockstore

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"ibs/internal/blockcodec"
)

// Damaged is a block that failed a check: missing, or its content doesn't
// match its id.
type Damaged struct {
	ID      string `json:"id"`
	Missing bool   `json:"missing,omitempty"`
}

// VerifyResult is the outcome of checking a batch of blocks.
type VerifyResult struct {
	Damaged []Damaged `json:"damaged"`
	Bytes   int64     `json:"bytes"` // bytes read
}

// ErrNoVerify means the node is too old to check blocks itself.
var ErrNoVerify = errors.New("node can't check blocks itself")

// Verifier checks stored blocks where they are, so a scrub doesn't move every
// block over the network: a block's id is the SHA-256 of its content, so no
// key is needed.
type Verifier interface {
	Verify(ctx context.Context, ns string, ids []string) (VerifyResult, error)
}

func (s *FS) Verify(ctx context.Context, ns string, ids []string) (VerifyResult, error) {
	var res VerifyResult
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		data, err := s.Get(ctx, ns, id)
		switch {
		case errors.Is(err, ErrNotFound):
			res.Damaged = append(res.Damaged, Damaged{ID: id, Missing: true})
		case err != nil:
			return res, err
		default:
			res.Bytes += int64(len(data))
			if !blockcodec.Verify(id, data) {
				res.Damaged = append(res.Damaged, Damaged{ID: id})
			}
		}
	}
	return res, nil
}

func (r *Remote) Verify(ctx context.Context, ns string, ids []string) (VerifyResult, error) {
	var res VerifyResult
	body, _ := json.Marshal(ids)
	resp, err := r.do(ctx, http.MethodPost, "/node/v1/verify/"+ns, body)
	if err != nil {
		return res, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return res, ErrNoVerify
	}
	if err := expect(resp, http.StatusOK); err != nil {
		return res, err
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&res)
	return res, err
}

func verifyHandler(store Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, ok := store.(Verifier)
		if !ok {
			http.NotFound(w, r)
			return
		}
		var ids []string
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&ids); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		res, err := v.Verify(r.Context(), r.PathValue("ns"), ids)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if res.Damaged == nil {
			res.Damaged = []Damaged{}
		}
		json.NewEncoder(w).Encode(res)
	}
}
