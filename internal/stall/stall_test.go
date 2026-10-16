package stall

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// slowReader hands out data in small pieces with a pause before each.
type slowReader struct {
	left  int
	pause time.Duration
}

func (r *slowReader) Read(p []byte) (int, error) {
	if r.left == 0 {
		return 0, io.EOF
	}
	time.Sleep(r.pause)
	n := min(len(p), 4, r.left)
	r.left -= n
	return n, nil
}

// Transfers that keep moving go on however long they take, in both
// directions; one that stops fails soon after.
func TestStall(t *testing.T) {
	old := Timeout
	Timeout = 300 * time.Millisecond
	defer func() { Timeout = old }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/slow-download": // over a second in all, a piece every 120 ms
			for range 10 {
				w.Write([]byte("data"))
				w.(http.Flusher).Flush()
				time.Sleep(120 * time.Millisecond)
			}
		case "/stops": // one piece, then nothing
			w.Write([]byte("data"))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "/upload":
			n, _ := io.Copy(io.Discard, r.Body)
			w.Write([]byte{byte(n)})
		case "/thinks": // takes too long to answer
			<-r.Context().Done()
		}
	}))
	defer srv.Close()
	run := func(method, path string, body io.Reader) ([]byte, error) {
		req, _ := http.NewRequest(method, srv.URL+path, body)
		resp, err := Do(http.DefaultClient, req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		return io.ReadAll(resp.Body)
	}
	if b, err := run(http.MethodGet, "/slow-download", nil); err != nil || len(b) != 40 {
		t.Fatalf("slow but steady download: %d bytes, %v", len(b), err)
	}
	if b, err := run(http.MethodPost, "/upload", &slowReader{left: 40, pause: 120 * time.Millisecond}); err != nil || !bytes.Equal(b, []byte{40}) {
		t.Fatalf("slow but steady upload: %v, %v", b, err)
	}
	for _, path := range []string{"/stops", "/thinks"} {
		start := time.Now()
		if _, err := run(http.MethodGet, path, nil); !errors.Is(err, ErrStalled) {
			t.Fatalf("%s: %v, want ErrStalled", path, err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("%s: took %v to notice", path, d)
		}
	}
}
