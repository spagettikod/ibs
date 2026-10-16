package blockstore

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ibs/internal/blockcodec"
)

// A storage node answers a challenge for intact blocks only, and a request
// without a nonce (from an older manager) gets no answers.
func TestVerifyChallengeOnNode(t *testing.T) {
	fs := &FS{Dir: t.TempDir()}
	ctx := context.Background()
	id, ct := blockcodec.Encrypt(make([]byte, 32), []byte("hello"))
	fs.Put(ctx, "own", id, ct)
	bad, _ := blockcodec.Encrypt(make([]byte, 32), []byte("other"))
	fs.Put(ctx, "own", bad, []byte("rot"))

	ask := func(query string) VerifyResult {
		body, _ := json.Marshal([]string{id, bad})
		req := httptest.NewRequest(http.MethodPost, "/node/v1/verify/own"+query, bytes.NewReader(body))
		req.SetPathValue("ns", "own")
		w := httptest.NewRecorder()
		verifyHandler(fs)(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body)
		}
		var res VerifyResult
		json.Unmarshal(w.Body.Bytes(), &res)
		return res
	}
	nonce := bytes.Repeat([]byte{7}, 32)
	res := ask("?nonce=" + hex.EncodeToString(nonce))
	if len(res.Answers) != 1 || res.Answers[id] != ChallengeAnswer(nonce, ct) {
		t.Fatalf("answers %v", res.Answers)
	}
	if len(res.Damaged) != 1 || res.Damaged[0].ID != bad {
		t.Fatalf("damaged %v", res.Damaged)
	}
	if res := ask(""); len(res.Answers) != 0 || len(res.Damaged) != 1 {
		t.Fatalf("without a nonce: %+v", res)
	}
}
