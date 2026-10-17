package s3api

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	algorithm                = "AWS4-HMAC-SHA256"
	amzDateFormat            = "20060102T150405Z"
	emptySHA256              = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	unsignedPayload          = "UNSIGNED-PAYLOAD"
	streamingSigned          = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	streamingSignedTrailer   = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
	streamingUnsignedTrailer = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
	maxSkew                  = 15 * time.Minute
)

type SecretLookup func(accessKey string) (string, bool)

type sigParams struct {
	accessKey     string
	date          string // yyyymmdd
	region        string
	service       string
	signedHeaders []string
	signature     string
	amzDate       string
	payloadHash   string
	presigned     bool
}

func (p *sigParams) scope() string {
	return p.date + "/" + p.region + "/" + p.service + "/aws4_request"
}

// accessKeyOf returns the access key of a request authenticate accepted.
func accessKeyOf(r *http.Request) string {
	if strings.HasPrefix(r.Header.Get("Authorization"), algorithm+" ") {
		if p, err := parseAuthHeader(r); err == nil {
			return p.accessKey
		}
		return ""
	}
	cred := r.URL.Query().Get("X-Amz-Credential")
	id, _, _ := strings.Cut(cred, "/")
	return id
}

// authenticate verifies a SigV4 request (header or presigned URL) and
// returns the request body to use, which verifies the payload as it is read.
func authenticate(r *http.Request, lookup SecretLookup, now time.Time) (io.Reader, *Error) {
	var p *sigParams
	var aerr *Error
	q := r.URL.Query()
	switch {
	case strings.HasPrefix(r.Header.Get("Authorization"), algorithm+" "):
		p, aerr = parseAuthHeader(r)
	case q.Get("X-Amz-Algorithm") == algorithm:
		p, aerr = parsePresigned(r, now)
	case r.Header.Get("Authorization") != "":
		return nil, errNotImplemented("only AWS Signature Version 4 is supported")
	default:
		return nil, errAccessDenied("anonymous access is not allowed")
	}
	if aerr != nil {
		return nil, aerr
	}
	if p.service != "s3" {
		return nil, errAuthMalformed("credential scope service must be s3")
	}
	secret, ok := lookup(p.accessKey)
	if !ok {
		return nil, &Error{Code: "InvalidAccessKeyId", Message: "The access key does not exist.", Status: http.StatusForbidden}
	}
	t, err := time.Parse(amzDateFormat, p.amzDate)
	if err != nil {
		return nil, errAuthMalformed("invalid X-Amz-Date")
	}
	if !p.presigned && (t.Sub(now) > maxSkew || now.Sub(t) > maxSkew) {
		return nil, &Error{Code: "RequestTimeTooSkewed", Message: "The difference between the request time and the server's time is too large.", Status: http.StatusForbidden}
	}
	if !strings.HasPrefix(p.amzDate, p.date) {
		return nil, errAuthMalformed("credential date does not match X-Amz-Date")
	}
	key := signingKey(secret, p.date, p.region, p.service)
	creq, err := canonicalRequest(r, p)
	if err != nil {
		return nil, errAuthMalformed(err.Error())
	}
	sts := algorithm + "\n" + p.amzDate + "\n" + p.scope() + "\n" + hexSHA256([]byte(creq))
	want := hex.EncodeToString(hmacSHA256(key, []byte(sts)))
	if !hmac.Equal([]byte(want), []byte(p.signature)) {
		return nil, &Error{Code: "SignatureDoesNotMatch", Message: "The request signature we calculated does not match the signature you provided.", Status: http.StatusForbidden}
	}

	switch p.payloadHash {
	case unsignedPayload:
		return r.Body, nil
	case streamingUnsignedTrailer:
		return newChunkedReader(r.Body, nil, "", "", ""), nil
	case streamingSigned, streamingSignedTrailer:
		return newChunkedReader(r.Body, key, p.amzDate, p.scope(), p.signature), nil
	default:
		if len(p.payloadHash) != 64 {
			return nil, errAuthMalformed("invalid x-amz-content-sha256")
		}
		return &hashCheckReader{r: r.Body, h: sha256.New(), want: p.payloadHash, code: "XAmzContentSHA256Mismatch"}, nil
	}
}

func parseAuthHeader(r *http.Request) (*sigParams, *Error) {
	p := &sigParams{}
	fields := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), algorithm+" "), ",")
	for _, f := range fields {
		k, v, ok := strings.Cut(strings.TrimSpace(f), "=")
		if !ok {
			continue
		}
		switch k {
		case "Credential":
			if err := p.parseCredential(v); err != nil {
				return nil, err
			}
		case "SignedHeaders":
			p.signedHeaders = strings.Split(v, ";")
		case "Signature":
			p.signature = v
		}
	}
	if p.accessKey == "" || p.signature == "" || len(p.signedHeaders) == 0 {
		return nil, errAuthMalformed("authorization header is incomplete")
	}
	p.amzDate = r.Header.Get("X-Amz-Date")
	if p.amzDate == "" {
		if t, err := http.ParseTime(r.Header.Get("Date")); err == nil {
			p.amzDate = t.UTC().Format(amzDateFormat)
		}
	}
	p.payloadHash = r.Header.Get("X-Amz-Content-Sha256")
	if p.payloadHash == "" {
		if r.ContentLength > 0 {
			return nil, errAuthMalformed("x-amz-content-sha256 header is required")
		}
		p.payloadHash = emptySHA256
	}
	return p, nil
}

func parsePresigned(r *http.Request, now time.Time) (*sigParams, *Error) {
	q := r.URL.Query()
	p := &sigParams{presigned: true, payloadHash: unsignedPayload}
	if err := p.parseCredential(q.Get("X-Amz-Credential")); err != nil {
		return nil, err
	}
	p.amzDate = q.Get("X-Amz-Date")
	p.signedHeaders = strings.Split(q.Get("X-Amz-SignedHeaders"), ";")
	p.signature = q.Get("X-Amz-Signature")
	if h := q.Get("X-Amz-Content-Sha256"); h != "" {
		p.payloadHash = h
	}
	expires, err := strconv.Atoi(q.Get("X-Amz-Expires"))
	if err != nil || expires < 1 || expires > 7*24*3600 {
		return nil, errAuthMalformed("invalid X-Amz-Expires")
	}
	t, err := time.Parse(amzDateFormat, p.amzDate)
	if err != nil {
		return nil, errAuthMalformed("invalid X-Amz-Date")
	}
	if now.After(t.Add(time.Duration(expires) * time.Second)) {
		return nil, errAccessDenied("Request has expired")
	}
	if t.Sub(now) > maxSkew {
		return nil, errAccessDenied("Request is not valid yet")
	}
	return p, nil
}

func (p *sigParams) parseCredential(v string) *Error {
	parts := strings.Split(v, "/")
	if len(parts) != 5 || parts[4] != "aws4_request" {
		return errAuthMalformed("invalid credential scope")
	}
	p.accessKey, p.date, p.region, p.service = parts[0], parts[1], parts[2], parts[3]
	return nil
}

func canonicalRequest(r *http.Request, p *sigParams) (string, error) {
	rawPath := r.URL.EscapedPath()
	if r.RequestURI != "" && strings.HasPrefix(r.RequestURI, "/") {
		rawPath, _, _ = strings.Cut(r.RequestURI, "?")
	}
	path, err := url.PathUnescape(rawPath)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(r.Method)
	b.WriteByte('\n')
	b.WriteString(uriEncode(path, false))
	b.WriteByte('\n')
	b.WriteString(canonicalQuery(r.URL.RawQuery))
	b.WriteByte('\n')
	for _, h := range p.signedHeaders {
		b.WriteString(h)
		b.WriteByte(':')
		b.WriteString(headerValue(r, h))
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(strings.Join(p.signedHeaders, ";"))
	b.WriteByte('\n')
	b.WriteString(p.payloadHash)
	return b.String(), nil
}

func headerValue(r *http.Request, name string) string {
	switch name {
	case "host":
		return r.Host
	case "content-length":
		if v := r.Header.Get("Content-Length"); v != "" {
			return v
		}
		return strconv.FormatInt(r.ContentLength, 10)
	case "transfer-encoding":
		if len(r.TransferEncoding) > 0 {
			return strings.Join(r.TransferEncoding, ",")
		}
	}
	vals := r.Header.Values(name)
	for i, v := range vals {
		vals[i] = strings.Join(strings.Fields(v), " ")
	}
	return strings.Join(vals, ",")
}

func canonicalQuery(raw string) string {
	type kv struct{ k, v string }
	var pairs []kv
	for part := range strings.SplitSeq(raw, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		dk, err1 := url.QueryUnescape(strings.ReplaceAll(k, "+", "%2B"))
		dv, err2 := url.QueryUnescape(strings.ReplaceAll(v, "+", "%2B"))
		if err1 != nil || err2 != nil {
			dk, dv = k, v
		}
		if dk == "X-Amz-Signature" {
			continue
		}
		pairs = append(pairs, kv{uriEncode(dk, true), uriEncode(dv, true)})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(p.k)
		b.WriteByte('=')
		b.WriteString(p.v)
	}
	return b.String()
}

// uriEncode implements the AWS URI encoding: everything except unreserved
// characters is percent-encoded; '/' is kept unless encodeSlash is set.
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

func hexSHA256(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func signingKey(secret, date, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	k = hmacSHA256(k, []byte(region))
	k = hmacSHA256(k, []byte(service))
	return hmacSHA256(k, []byte("aws4_request"))
}

// hashCheckReader fails at EOF if the content digest does not match.
type hashCheckReader struct {
	r    io.Reader
	h    hash.Hash
	want string // hex, or base64 when b64 is set
	b64  bool
	code string
}

var errDigest = errors.New("digest mismatch")

type digestError struct{ code string }

func (e *digestError) Error() string { return e.code }
func (e *digestError) Unwrap() error { return errDigest }

func (c *hashCheckReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.h.Write(p[:n])
	if err == io.EOF {
		sum := c.h.Sum(nil)
		got := hex.EncodeToString(sum)
		if c.b64 {
			got = b64(sum)
		}
		if got != c.want {
			return n, &digestError{c.code}
		}
	}
	return n, err
}

// chunkedReader decodes an aws-chunked body, verifying chunk signatures when
// a signing key is given. Trailing headers (checksums) are read and ignored.
type chunkedReader struct {
	r       *bufio.Reader
	key     []byte
	amzDate string
	scope   string
	prevSig string
	buf     []byte
	done    bool
}

const maxChunk = 16 << 20

func newChunkedReader(r io.Reader, key []byte, amzDate, scope, seedSig string) *chunkedReader {
	return &chunkedReader{r: bufio.NewReaderSize(r, 64<<10), key: key, amzDate: amzDate, scope: scope, prevSig: seedSig}
}

var errChunk = errors.New("malformed aws-chunked body")

func (c *chunkedReader) Read(p []byte) (int, error) {
	for len(c.buf) == 0 {
		if c.done {
			return 0, io.EOF
		}
		if err := c.next(); err != nil {
			return 0, err
		}
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}

func (c *chunkedReader) readLine() (string, error) {
	line, err := c.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (c *chunkedReader) next() error {
	header, err := c.readLine()
	if err != nil {
		return errChunk
	}
	sizeHex, ext, _ := strings.Cut(header, ";")
	size, err := strconv.ParseInt(strings.TrimSpace(sizeHex), 16, 64)
	if err != nil || size < 0 || size > maxChunk {
		return errChunk
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(c.r, data); err != nil {
		return errChunk
	}
	if c.key != nil {
		sig := strings.TrimPrefix(strings.TrimSpace(ext), "chunk-signature=")
		sts := "AWS4-HMAC-SHA256-PAYLOAD\n" + c.amzDate + "\n" + c.scope + "\n" + c.prevSig + "\n" + emptySHA256 + "\n" + hexSHA256(data)
		want := hex.EncodeToString(hmacSHA256(c.key, []byte(sts)))
		if !hmac.Equal([]byte(want), []byte(sig)) {
			return &digestError{"SignatureDoesNotMatch"}
		}
		c.prevSig = sig
	}
	if size == 0 {
		// Trailer headers until an empty line (or end of body).
		for {
			line, err := c.readLine()
			if err != nil || line == "" {
				break
			}
		}
		c.done = true
		return nil
	}
	crlf := make([]byte, 2)
	if _, err := io.ReadFull(c.r, crlf); err != nil || !bytes.Equal(crlf, []byte("\r\n")) {
		return errChunk
	}
	c.buf = data
	return nil
}
