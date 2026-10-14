package s3api

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Examples from the AWS SigV4 documentation for Amazon S3.
const (
	exampleKey    = "AKIAIOSFODNN7EXAMPLE"
	exampleSecret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
)

var exampleTime = time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)

func lookup(id string) (string, bool) { return exampleSecret, id == exampleKey }

func TestHeaderAuthGetObject(t *testing.T) {
	r := httptest.NewRequest("GET", "http://examplebucket.s3.amazonaws.com/test.txt", nil)
	r.Header.Set("Range", "bytes=0-9")
	r.Header.Set("x-amz-content-sha256", emptySHA256)
	r.Header.Set("x-amz-date", "20130524T000000Z")
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request,SignedHeaders=host;range;x-amz-content-sha256;x-amz-date,Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41")
	if _, err := authenticate(r, lookup, exampleTime); err != nil {
		t.Fatalf("expected valid signature, got %v", err)
	}
	r.Header.Set("Range", "bytes=0-10")
	if _, err := authenticate(r, lookup, exampleTime); err == nil || err.Code != "SignatureDoesNotMatch" {
		t.Fatalf("expected signature mismatch, got %v", err)
	}
}

func TestPresignedURL(t *testing.T) {
	u := "http://examplebucket.s3.amazonaws.com/test.txt?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20130524%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20130524T000000Z&X-Amz-Expires=86400&X-Amz-SignedHeaders=host&X-Amz-Signature=aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	r := httptest.NewRequest("GET", u, nil)
	if _, err := authenticate(r, lookup, exampleTime.Add(time.Hour)); err != nil {
		t.Fatalf("expected valid presigned url, got %v", err)
	}
	if _, err := authenticate(r, lookup, exampleTime.Add(25*time.Hour)); err == nil {
		t.Fatal("expected expired presigned url to fail")
	}
}

func TestStreamingChunkSignatures(t *testing.T) {
	seed := "4f232c4386841ef735655705268965c44a0e4690baa4adea153f7db9fa80a0a9"
	chunks := []struct {
		size int
		sig  string
	}{
		{65536, "ad80c730a21e5b8d04586a2213dd63b9a0e99e0e2307b0ade35a65485a288648"},
		{1024, "0055627c9e194cb4542bae2aa5492e3c1575bbb81b612b7d234b86a503ef5497"},
		{0, "b6c6ea8a5354eaf15b3cb7646744f4275b71ea724fed81ceb9323e279d449df9"},
	}
	var body bytes.Buffer
	for _, c := range chunks {
		fmt.Fprintf(&body, "%x;chunk-signature=%s\r\n", c.size, c.sig)
		body.WriteString(strings.Repeat("a", c.size))
		body.WriteString("\r\n")
	}
	key := signingKey(exampleSecret, "20130524", "us-east-1", "s3")
	cr := newChunkedReader(bytes.NewReader(body.Bytes()), key, "20130524T000000Z", "20130524/us-east-1/s3/aws4_request", seed)
	out, err := io.ReadAll(cr)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 66560 {
		t.Fatalf("decoded %d bytes, want 66560", len(out))
	}

	tampered := bytes.Replace(body.Bytes(), []byte("aaaa"), []byte("aaab"), 1)
	cr = newChunkedReader(bytes.NewReader(tampered), key, "20130524T000000Z", "20130524/us-east-1/s3/aws4_request", seed)
	if _, err := io.ReadAll(cr); err == nil {
		t.Fatal("expected tampered chunk to fail verification")
	}
}

func TestUnsignedChunkedWithTrailer(t *testing.T) {
	body := "5\r\nhello\r\n6\r\n world\r\n0\r\nx-amz-checksum-crc32:AAAAAA==\r\n\r\n"
	out, err := io.ReadAll(newChunkedReader(strings.NewReader(body), nil, "", "", ""))
	if err != nil || string(out) != "hello world" {
		t.Fatalf("got %q, %v", out, err)
	}
}

func TestParseRange(t *testing.T) {
	cases := []struct {
		h          string
		start, len int64
		ok         bool
		err        bool
	}{
		{"bytes=0-9", 0, 10, true, false},
		{"bytes=10-", 10, 90, true, false},
		{"bytes=-5", 95, 5, true, false},
		{"bytes=90-200", 90, 10, true, false},
		{"bytes=100-", 0, 0, false, true},
		{"bytes=1-2,4-5", 0, 0, false, false},
	}
	for _, c := range cases {
		s, l, ok, err := parseRange(c.h, 100)
		if s != c.start || l != c.len || ok != c.ok || (err != nil) != c.err {
			t.Errorf("%s: got %d,%d,%v,%v", c.h, s, l, ok, err)
		}
	}
}

func TestUnauthenticatedRejected(t *testing.T) {
	r := httptest.NewRequest("GET", "http://localhost/", nil)
	if _, err := authenticate(r, lookup, time.Now()); err == nil || err.Status != http.StatusForbidden {
		t.Fatalf("expected access denied, got %v", err)
	}
}
