package blockstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	// Answers to a challenge, per intact block: ChallengeAnswer of the nonce
	// and the block's bytes. Only when a nonce was given (and the node is
	// new enough to answer).
	Answers map[string]string `json:"answers,omitempty"`
}

// ChallengeAnswer proves a store holds a block's exact bytes right now: the
// SHA-256 of a fresh random nonce followed by the bytes. It can't be worked
// out from the block's id or remembered from an earlier check.
func ChallengeAnswer(nonce, data []byte) string {
	h := sha256.New()
	h.Write(nonce)
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// Verifier checks stored blocks where they are, so a scrub doesn't move every
// block over the network: a block's id is the SHA-256 of its content, so no
// key is needed. With a nonce, intact blocks also answer that challenge.
type Verifier interface {
	Verify(ctx context.Context, ns string, ids []string, nonce []byte) (VerifyResult, error)
}

func (s *FS) Verify(ctx context.Context, ns string, ids []string, nonce []byte) (VerifyResult, error) {
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
			} else if nonce != nil {
				if res.Answers == nil {
					res.Answers = map[string]string{}
				}
				res.Answers[id] = ChallengeAnswer(nonce, data)
			}
		}
	}
	return res, nil
}

// Verify on a storage node; the nonce goes in the query.
func (r *Remote) Verify(ctx context.Context, ns string, ids []string, nonce []byte) (VerifyResult, error) {
	var res VerifyResult
	body, _ := json.Marshal(ids)
	path := "/node/v1/verify/" + ns
	if nonce != nil {
		path += "?nonce=" + hex.EncodeToString(nonce)
	}
	resp, err := r.do(ctx, http.MethodPost, path, body)
	if err != nil {
		return res, err
	}
	defer resp.Body.Close()
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
		var err error
		if err = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&ids); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var nonce []byte
		if q := r.URL.Query().Get("nonce"); q != "" {
			if nonce, err = hex.DecodeString(q); err != nil || len(nonce) < 16 {
				http.Error(w, "bad nonce", http.StatusBadRequest)
				return
			}
		}
		res, err := v.Verify(r.Context(), r.PathValue("ns"), ids, nonce)
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
