// Package s3api serves the S3-compatible API of a site manager.
package s3api

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"ibs/internal/keys"
	"ibs/internal/site"
)

type Handler struct {
	Site   *site.Site
	Domain string // optional base domain for virtual-host style requests
}

const timeFormat = "2006-01-02T15:04:05.000Z"

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("x-amz-request-id", strings.ToUpper(keys.RandomToken(8)))
	w.Header().Set("Server", "IBS")
	start := time.Now()
	sw := &statusWriter{ResponseWriter: w, status: 200}
	defer func() {
		slog.Debug("s3", "method", r.Method, "uri", r.RequestURI, "status", sw.status, "took", time.Since(start))
	}()

	body, aerr := authenticate(r, h.Site.AccessKeySecret, time.Now().UTC())
	if aerr != nil {
		writeError(sw, r, aerr)
		return
	}
	bucket, key := h.parsePath(r)
	pol, ok := h.Site.AccessKeyPolicy(accessKeyOf(r))
	if !ok {
		writeError(sw, r, errAccessDenied("The access key does not exist."))
		return
	}
	if aerr := authorize(r, pol, bucket, key); aerr != nil {
		writeError(sw, r, aerr)
		return
	}
	if err := h.route(sw, r, body, bucket, key, pol); err != nil {
		e := toS3Error(err)
		if e.Status >= 500 {
			slog.Error("s3 request failed", "method", r.Method, "uri", r.RequestURI, "err", err)
		}
		if sw.wrote {
			return // headers already sent; nothing sensible to add
		}
		writeError(sw, r, e)
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusWriter) WriteHeader(code int) {
	s.status, s.wrote = code, true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	s.wrote = true
	return s.ResponseWriter.Write(b)
}

func (h *Handler) parsePath(r *http.Request) (bucket, key string) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	if h.Domain != "" {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if b, ok := strings.CutSuffix(host, "."+h.Domain); ok && b != "" {
			return b, path
		}
	}
	bucket, key, _ = strings.Cut(path, "/")
	return bucket, key
}

// authorize checks a request against what its access key may do: reading
// (GET, HEAD) needs the bucket, anything else full access to it; a copy also
// reading its source.
func authorize(r *http.Request, pol site.KeyPolicy, bucket, key string) *Error {
	denied := func(b string) *Error {
		return errAccessDenied(fmt.Sprintf("This access key may not do that in bucket %q.", b))
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		if !pol.CanRead(bucket) {
			return denied(bucket)
		}
		return nil
	}
	if bucket == "" || !pol.CanWrite(bucket) {
		if pol.Access != site.AccessFull {
			return errAccessDenied("This access key is read only.")
		}
		return denied(bucket)
	}
	if r.Header.Get("x-amz-copy-source") != "" {
		if src, _, err := copySource(r); err == nil && !pol.CanRead(src) {
			return denied(src)
		}
	}
	return nil
}

func (h *Handler) route(w http.ResponseWriter, r *http.Request, body io.Reader, bucket, key string, pol site.KeyPolicy) error {
	q := r.URL.Query()
	if bucket == "" {
		if r.Method == http.MethodGet {
			return h.listBuckets(w, pol)
		}
		return errNotImplemented("unsupported service operation")
	}
	if key == "" {
		switch r.Method {
		case http.MethodPut:
			if unsupportedSub(q, "acl", "versioning", "policy", "cors", "lifecycle", "tagging", "encryption", "website", "logging", "notification", "replication", "object-lock") {
				return errNotImplemented("bucket configuration is not supported")
			}
			return h.createBucket(w, r, bucket)
		case http.MethodHead:
			if !h.Site.BucketExists(bucket) {
				return site.ErrNoSuchBucket
			}
			return nil
		case http.MethodDelete:
			if err := h.Site.DeleteBucket(bucket); err != nil {
				return err
			}
			w.WriteHeader(http.StatusNoContent)
			return nil
		case http.MethodPost:
			if q.Has("delete") {
				return h.deleteObjects(w, r, body, bucket)
			}
		case http.MethodGet:
			switch {
			case q.Has("location"):
				if !h.Site.BucketExists(bucket) {
					return site.ErrNoSuchBucket
				}
				writeXML(w, struct {
					XMLName xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ LocationConstraint"`
				}{})
				return nil
			case q.Has("versioning"):
				if !h.Site.BucketExists(bucket) {
					return site.ErrNoSuchBucket
				}
				writeXML(w, struct {
					XMLName xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ VersioningConfiguration"`
				}{})
				return nil
			case q.Has("uploads"):
				return h.listUploads(w, r, bucket)
			case q.Get("list-type") == "2":
				return h.listObjectsV2(w, r, bucket)
			case unsupportedSub(q, "acl", "policy", "cors", "lifecycle", "tagging", "encryption", "website", "logging", "notification", "replication", "object-lock", "versions", "ownershipControls", "publicAccessBlock", "requestPayment", "accelerate", "analytics", "metrics", "inventory", "intelligent-tiering"):
				return &Error{"NotImplemented", "This bucket subresource is not supported.", http.StatusNotImplemented}
			default:
				return h.listObjectsV1(w, r, bucket)
			}
		}
		return errNotImplemented("unsupported bucket operation")
	}

	switch r.Method {
	case http.MethodPut, http.MethodPost:
		// IBS has one storage class. Refuse others rather than ignore them,
		// so nobody believes data moved to a cheaper or archive tier.
		if sc := r.Header.Get("X-Amz-Storage-Class"); sc != "" && sc != "STANDARD" {
			return &Error{"InvalidStorageClass", "IBS only supports the STANDARD storage class.", http.StatusBadRequest}
		}
	}
	switch r.Method {
	case http.MethodPut:
		if q.Has("uploadId") {
			if r.Header.Get("X-Amz-Copy-Source") != "" {
				return h.uploadPartCopy(w, r, bucket, key)
			}
			return h.uploadPart(w, r, body, bucket, key)
		}
		if unsupportedSub(q, "acl", "tagging", "retention", "legal-hold") {
			return errNotImplemented("object subresource is not supported")
		}
		if r.Header.Get("X-Amz-Copy-Source") != "" {
			return h.copyObject(w, r, bucket, key)
		}
		return h.putObject(w, r, body, bucket, key)
	case http.MethodGet, http.MethodHead:
		if q.Has("uploadId") && r.Method == http.MethodGet {
			return h.listParts(w, r, bucket, key)
		}
		if q.Has("tagging") && r.Method == http.MethodGet {
			return h.getObjectTagging(w, bucket, key)
		}
		if unsupportedSub(q, "acl", "tagging", "retention", "legal-hold", "attributes", "torrent") {
			return errNotImplemented("object subresource is not supported")
		}
		return h.getObject(w, r, bucket, key)
	case http.MethodDelete:
		if q.Has("uploadId") {
			if err := h.Site.AbortUpload(bucket, key, q.Get("uploadId")); err != nil {
				return err
			}
			w.WriteHeader(http.StatusNoContent)
			return nil
		}
		if err := h.Site.DeleteObject(bucket, key); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	case http.MethodPost:
		if q.Has("uploads") {
			return h.createUpload(w, r, bucket, key)
		}
		if q.Has("uploadId") {
			return h.completeUpload(w, r, body, bucket, key)
		}
	}
	return errNotImplemented("unsupported object operation")
}

func unsupportedSub(q url.Values, names ...string) bool {
	for _, n := range names {
		if q.Has(n) {
			return true
		}
	}
	return false
}

// ---- Buckets ----

type owner struct {
	ID          string
	DisplayName string
}

func (h *Handler) owner() owner { return owner{ID: h.Site.ID.SiteID, DisplayName: h.Site.Name()} }

// listBuckets lists the buckets the key may use.
func (h *Handler) listBuckets(w http.ResponseWriter, pol site.KeyPolicy) error {
	buckets, err := h.Site.ListBuckets()
	if err != nil {
		return err
	}
	type bucket struct {
		Name         string
		CreationDate string
	}
	res := struct {
		XMLName xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListAllMyBucketsResult"`
		Owner   owner
		Buckets []bucket `xml:"Buckets>Bucket"`
	}{Owner: h.owner()}
	for _, b := range buckets {
		if pol.Buckets == nil || pol.Buckets[b.Name] {
			res.Buckets = append(res.Buckets, bucket{b.Name, b.Created.Format(timeFormat)})
		}
	}
	writeXML(w, res)
	return nil
}

func (h *Handler) createBucket(w http.ResponseWriter, r *http.Request, bucket string) error {
	if err := h.Site.CreateBucket(bucket); err != nil {
		return err
	}
	w.Header().Set("Location", "/"+bucket)
	return nil
}

// ---- Listing ----

type listEntry struct {
	Key          string
	LastModified string
	ETag         string
	Size         int64
	StorageClass string
	Owner        *owner `xml:",omitempty"`
}

type commonPrefix struct {
	Prefix string
}

func encodeKey(s string, urlEnc bool) string {
	if urlEnc {
		return strings.ReplaceAll(url.QueryEscape(s), "%2F", "/")
	}
	return s
}

func (h *Handler) entries(res *site.ListResult, urlEnc, withOwner bool) ([]listEntry, []commonPrefix) {
	var out []listEntry
	o := h.owner()
	for _, obj := range res.Objects {
		e := listEntry{Key: encodeKey(obj.Key, urlEnc), LastModified: obj.Modified.Format(timeFormat), ETag: obj.ETag, Size: obj.Size, StorageClass: "STANDARD"}
		if withOwner {
			e.Owner = &o
		}
		out = append(out, e)
	}
	var cps []commonPrefix
	for _, p := range res.CommonPrefixes {
		cps = append(cps, commonPrefix{encodeKey(p, urlEnc)})
	}
	return out, cps
}

func maxKeys(q url.Values) (int, error) {
	if v := q.Get("max-keys"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return 0, errInvalidArgument("max-keys must be a non-negative integer")
		}
		return min(n, 1000), nil
	}
	return 1000, nil
}

func (h *Handler) listObjectsV1(w http.ResponseWriter, r *http.Request, bucket string) error {
	q := r.URL.Query()
	mk, err := maxKeys(q)
	if err != nil {
		return err
	}
	urlEnc := q.Get("encoding-type") == "url"
	res := &site.ListResult{}
	if mk > 0 {
		if res, err = h.Site.ListObjects(bucket, q.Get("prefix"), q.Get("delimiter"), q.Get("marker"), mk); err != nil {
			return err
		}
	} else if !h.Site.BucketExists(bucket) {
		return site.ErrNoSuchBucket
	}
	contents, cps := h.entries(res, urlEnc, true)
	out := struct {
		XMLName        xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListBucketResult"`
		Name           string
		Prefix         string
		Marker         string
		NextMarker     string `xml:",omitempty"`
		MaxKeys        int
		Delimiter      string `xml:",omitempty"`
		IsTruncated    bool
		EncodingType   string `xml:",omitempty"`
		Contents       []listEntry
		CommonPrefixes []commonPrefix
	}{
		Name: bucket, Prefix: encodeKey(q.Get("prefix"), urlEnc), Marker: encodeKey(q.Get("marker"), urlEnc),
		MaxKeys: mk, Delimiter: encodeKey(q.Get("delimiter"), urlEnc), IsTruncated: res.Truncated,
		Contents: contents, CommonPrefixes: cps,
	}
	if res.Truncated {
		out.NextMarker = encodeKey(res.NextMarker, urlEnc)
	}
	if urlEnc {
		out.EncodingType = "url"
	}
	writeXML(w, out)
	return nil
}

func (h *Handler) listObjectsV2(w http.ResponseWriter, r *http.Request, bucket string) error {
	q := r.URL.Query()
	mk, err := maxKeys(q)
	if err != nil {
		return err
	}
	urlEnc := q.Get("encoding-type") == "url"
	marker := q.Get("start-after")
	if tok := q.Get("continuation-token"); tok != "" {
		b, err := base64.RawURLEncoding.DecodeString(tok)
		if err != nil {
			return errInvalidArgument("The continuation token provided is incorrect")
		}
		marker = string(b)
	}
	res := &site.ListResult{}
	if mk > 0 {
		if res, err = h.Site.ListObjects(bucket, q.Get("prefix"), q.Get("delimiter"), marker, mk); err != nil {
			return err
		}
	} else if !h.Site.BucketExists(bucket) {
		return site.ErrNoSuchBucket
	}
	contents, cps := h.entries(res, urlEnc, q.Get("fetch-owner") == "true")
	out := struct {
		XMLName               xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListBucketResult"`
		Name                  string
		Prefix                string
		Delimiter             string `xml:",omitempty"`
		MaxKeys               int
		KeyCount              int
		IsTruncated           bool
		EncodingType          string `xml:",omitempty"`
		ContinuationToken     string `xml:",omitempty"`
		NextContinuationToken string `xml:",omitempty"`
		StartAfter            string `xml:",omitempty"`
		Contents              []listEntry
		CommonPrefixes        []commonPrefix
	}{
		Name: bucket, Prefix: encodeKey(q.Get("prefix"), urlEnc), Delimiter: encodeKey(q.Get("delimiter"), urlEnc),
		MaxKeys: mk, KeyCount: len(contents) + len(cps), IsTruncated: res.Truncated,
		ContinuationToken: q.Get("continuation-token"), StartAfter: encodeKey(q.Get("start-after"), urlEnc),
		Contents: contents, CommonPrefixes: cps,
	}
	if res.Truncated {
		out.NextContinuationToken = base64.RawURLEncoding.EncodeToString([]byte(res.NextMarker))
	}
	if urlEnc {
		out.EncodingType = "url"
	}
	writeXML(w, out)
	return nil
}

// ---- Objects ----

func userMeta(r *http.Request) map[string]string {
	meta := map[string]string{}
	for k, v := range r.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-amz-meta-") && len(v) > 0 {
			meta[strings.TrimPrefix(lk, "x-amz-meta-")] = v[0]
		}
	}
	return meta
}

// withContentMD5 verifies the Content-MD5 header, if any, before the object
// is committed.
func withContentMD5(r *http.Request, body io.Reader) (io.Reader, error) {
	v := r.Header.Get("Content-MD5")
	if v == "" {
		return body, nil
	}
	if raw, err := base64.StdEncoding.DecodeString(v); err != nil || len(raw) != md5.Size {
		return nil, &Error{"InvalidDigest", "The Content-MD5 you specified is not valid.", http.StatusBadRequest}
	}
	return &hashCheckReader{r: body, h: md5.New(), want: v, b64: true, code: "BadDigest"}, nil
}

func (h *Handler) putObject(w http.ResponseWriter, r *http.Request, body io.Reader, bucket, key string) error {
	body, err := withContentMD5(r, body)
	if err != nil {
		return err
	}
	o, err := h.Site.PutObject(r.Context(), bucket, key, body, r.Header.Get("Content-Type"), userMeta(r))
	if err != nil {
		return err
	}
	w.Header().Set("ETag", o.ETag)
	return nil
}

// copySource parses the x-amz-copy-source header ("bucket/key").
func copySource(r *http.Request) (bucket, key string, err error) {
	src, err := url.PathUnescape(r.Header.Get("X-Amz-Copy-Source"))
	if err != nil {
		return "", "", errInvalidArgument("invalid x-amz-copy-source")
	}
	src, _, _ = strings.Cut(src, "?versionId=")
	bucket, key, ok := strings.Cut(strings.TrimPrefix(src, "/"), "/")
	if !ok || key == "" {
		return "", "", errInvalidArgument("invalid x-amz-copy-source")
	}
	return bucket, key, nil
}

func (h *Handler) copyObject(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	srcBucket, srcKey, err := copySource(r)
	if err != nil {
		return err
	}
	replace := strings.EqualFold(r.Header.Get("X-Amz-Metadata-Directive"), "REPLACE")
	if srcBucket == bucket && srcKey == key && !replace {
		return &Error{"InvalidRequest", "This copy request is illegal because it is trying to copy an object to itself without changing the object's metadata.", http.StatusBadRequest}
	}
	o, err := h.Site.CopyObject(r.Context(), srcBucket, srcKey, bucket, key, replace, r.Header.Get("Content-Type"), userMeta(r))
	if err != nil {
		return err
	}
	writeXML(w, struct {
		XMLName      xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ CopyObjectResult"`
		LastModified string
		ETag         string
	}{LastModified: o.Modified.Format(timeFormat), ETag: o.ETag})
	return nil
}

// parseRange handles a single "bytes=" range. It returns ok=false when the
// header should be ignored.
func parseRange(h string, size int64) (start, length int64, ok bool, err error) {
	spec, found := strings.CutPrefix(h, "bytes=")
	if !found || strings.Contains(spec, ",") {
		return 0, 0, false, nil
	}
	a, b, _ := strings.Cut(strings.TrimSpace(spec), "-")
	switch {
	case a == "":
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false, nil
		}
		if size == 0 {
			return 0, 0, false, site.ErrInvalidRange
		}
		n = min(n, size)
		return size - n, n, true, nil
	default:
		s, err := strconv.ParseInt(a, 10, 64)
		if err != nil || s < 0 {
			return 0, 0, false, nil
		}
		if s >= size {
			return 0, 0, false, site.ErrInvalidRange
		}
		e := size - 1
		if b != "" {
			if e, err = strconv.ParseInt(b, 10, 64); err != nil || e < s {
				return 0, 0, false, nil
			}
			e = min(e, size-1)
		}
		return s, e - s + 1, true, nil
	}
}

func (h *Handler) getObject(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	o, err := h.Site.HeadObject(bucket, key)
	if err != nil {
		return err
	}
	// Conditional requests.
	if m := r.Header.Get("If-Match"); m != "" && m != "*" && strings.Trim(m, `"`) != strings.Trim(o.ETag, `"`) {
		return &Error{"PreconditionFailed", "At least one of the preconditions you specified did not hold.", http.StatusPreconditionFailed}
	}
	if t, err := http.ParseTime(r.Header.Get("If-Unmodified-Since")); err == nil && o.Modified.Truncate(time.Second).After(t) {
		return &Error{"PreconditionFailed", "At least one of the preconditions you specified did not hold.", http.StatusPreconditionFailed}
	}
	notModified := false
	if m := r.Header.Get("If-None-Match"); m != "" {
		notModified = m == "*" || strings.Trim(m, `"`) == strings.Trim(o.ETag, `"`)
	} else if t, err := http.ParseTime(r.Header.Get("If-Modified-Since")); err == nil && !o.Modified.Truncate(time.Second).After(t) {
		notModified = true
	}

	hdr := w.Header()
	hdr.Set("ETag", o.ETag)
	hdr.Set("Last-Modified", o.Modified.Format(http.TimeFormat))
	hdr.Set("Content-Type", o.ContentType)
	hdr.Set("Accept-Ranges", "bytes")
	// Set user metadata without Go's header canonicalization, so clients
	// get the keys back in the case they were stored (S3 uses lower case).
	for _, k := range site.SortedMetaKeys(o.Meta) {
		hdr["x-amz-meta-"+k] = []string{o.Meta[k]}
	}
	q := r.URL.Query()
	for param, header := range map[string]string{
		"response-content-type": "Content-Type", "response-content-disposition": "Content-Disposition",
		"response-cache-control": "Cache-Control", "response-content-encoding": "Content-Encoding",
		"response-content-language": "Content-Language", "response-expires": "Expires",
	} {
		if v := q.Get(param); v != "" {
			hdr.Set(header, v)
		}
	}
	if notModified {
		w.WriteHeader(http.StatusNotModified)
		return nil
	}

	start, length := int64(0), o.Size
	status := http.StatusOK
	if rh := r.Header.Get("Range"); rh != "" {
		s, l, ok, err := parseRange(rh, o.Size)
		if err != nil {
			hdr.Set("Content-Range", fmt.Sprintf("bytes */%d", o.Size))
			return err
		}
		if ok {
			start, length, status = s, l, http.StatusPartialContent
			hdr.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", s, s+l-1, o.Size))
		}
	}
	hdr.Set("Content-Length", strconv.FormatInt(length, 10))
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return nil
	}
	// Fetch the first block before committing to a status code so a
	// missing block can still be reported as an error.
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(h.Site.WriteObject(r.Context(), pw, o, start, length)) }()
	defer pr.Close()
	first := make([]byte, 32<<10)
	n, err := io.ReadAtLeast(pr, first, min(int(length), 1))
	if err != nil && !(length == 0 && errors.Is(err, io.EOF)) && n == 0 {
		return err
	}
	w.WriteHeader(status)
	w.Write(first[:n])
	if _, err := io.Copy(w, pr); err != nil {
		slog.Warn("s3 get object interrupted", "bucket", bucket, "key", key, "err", err)
	}
	return nil
}

// getObjectTagging reports an empty tag set: objects have no tags. Clients
// such as the AWS CLI ask for tags when copying between buckets.
func (h *Handler) getObjectTagging(w http.ResponseWriter, bucket, key string) error {
	if _, err := h.Site.HeadObject(bucket, key); err != nil {
		return err
	}
	writeXML(w, struct {
		XMLName xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ Tagging"`
		TagSet  struct{}
	}{})
	return nil
}

type deleteRequest struct {
	Quiet   bool
	Objects []struct {
		Key string
	} `xml:"Object"`
}

func (h *Handler) deleteObjects(w http.ResponseWriter, r *http.Request, body io.Reader, bucket string) error {
	var req deleteRequest
	if err := xml.NewDecoder(io.LimitReader(body, 4<<20)).Decode(&req); err != nil {
		return errMalformedXML
	}
	if len(req.Objects) > 1000 {
		return errMalformedXML
	}
	if !h.Site.BucketExists(bucket) {
		return site.ErrNoSuchBucket
	}
	type deleted struct{ Key string }
	type delErr struct {
		Key     string
		Code    string
		Message string
	}
	res := struct {
		XMLName xml.Name  `xml:"http://s3.amazonaws.com/doc/2006-03-01/ DeleteResult"`
		Deleted []deleted `xml:"Deleted"`
		Errors  []delErr  `xml:"Error"`
	}{}
	for _, o := range req.Objects {
		if err := h.Site.DeleteObject(bucket, o.Key); err != nil {
			e := toS3Error(err)
			res.Errors = append(res.Errors, delErr{o.Key, e.Code, e.Message})
		} else if !req.Quiet {
			res.Deleted = append(res.Deleted, deleted{o.Key})
		}
	}
	writeXML(w, res)
	return nil
}

// ---- Multipart ----

func (h *Handler) createUpload(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	id, err := h.Site.CreateUpload(bucket, key, r.Header.Get("Content-Type"), userMeta(r))
	if err != nil {
		return err
	}
	writeXML(w, struct {
		XMLName  xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ InitiateMultipartUploadResult"`
		Bucket   string
		Key      string
		UploadID string `xml:"UploadId"`
	}{Bucket: bucket, Key: key, UploadID: id})
	return nil
}

func (h *Handler) uploadPart(w http.ResponseWriter, r *http.Request, body io.Reader, bucket, key string) error {
	q := r.URL.Query()
	num, err := strconv.Atoi(q.Get("partNumber"))
	if err != nil {
		return errInvalidArgument("partNumber must be an integer")
	}
	body, err = withContentMD5(r, body)
	if err != nil {
		return err
	}
	etag, err := h.Site.UploadPart(r.Context(), bucket, key, q.Get("uploadId"), num, body)
	if err != nil {
		if strings.Contains(err.Error(), "part number") {
			return errInvalidArgument(err.Error())
		}
		return err
	}
	w.Header().Set("ETag", etag)
	return nil
}

// uploadPartCopy fills a multipart part from (a range of) an existing
// object. Clients use it to copy large objects. Within a bucket, block-aligned
// parts share the existing blocks, so no extra space is used.
func (h *Handler) uploadPartCopy(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	q := r.URL.Query()
	num, err := strconv.Atoi(q.Get("partNumber"))
	if err != nil {
		return errInvalidArgument("partNumber must be an integer")
	}
	srcBucket, srcKey, err := copySource(r)
	if err != nil {
		return err
	}
	src, err := h.Site.HeadObject(srcBucket, srcKey)
	if err != nil {
		return err
	}
	start, length := int64(0), src.Size
	if rh := r.Header.Get("X-Amz-Copy-Source-Range"); rh != "" {
		s, l, ok, err := parseRange(rh, src.Size)
		if err != nil || !ok {
			return &Error{"InvalidArgument", "The x-amz-copy-source-range is not valid.", http.StatusBadRequest}
		}
		start, length = s, l
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(h.Site.WriteObject(r.Context(), pw, src, start, length)) }()
	etag, err := h.Site.UploadPart(r.Context(), bucket, key, q.Get("uploadId"), num, pr)
	pr.CloseWithError(err)
	if err != nil {
		return err
	}
	writeXML(w, struct {
		XMLName      xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ CopyPartResult"`
		LastModified string
		ETag         string
	}{LastModified: time.Now().UTC().Format(timeFormat), ETag: etag})
	return nil
}

func (h *Handler) completeUpload(w http.ResponseWriter, r *http.Request, body io.Reader, bucket, key string) error {
	var req struct {
		Parts []struct {
			PartNumber int
			ETag       string
		} `xml:"Part"`
	}
	if err := xml.NewDecoder(io.LimitReader(body, 8<<20)).Decode(&req); err != nil {
		return errMalformedXML
	}
	var parts []site.CompletePart
	for _, p := range req.Parts {
		parts = append(parts, site.CompletePart{Number: p.PartNumber, ETag: p.ETag})
	}
	o, err := h.Site.CompleteUpload(bucket, key, r.URL.Query().Get("uploadId"), parts)
	if err != nil {
		return err
	}
	writeXML(w, struct {
		XMLName  xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ CompleteMultipartUploadResult"`
		Location string
		Bucket   string
		Key      string
		ETag     string
	}{Location: "/" + bucket + "/" + key, Bucket: bucket, Key: key, ETag: o.ETag})
	return nil
}

func (h *Handler) listParts(w http.ResponseWriter, r *http.Request, bucket, key string) error {
	id := r.URL.Query().Get("uploadId")
	parts, err := h.Site.ListParts(bucket, key, id)
	if err != nil {
		return err
	}
	type part struct {
		PartNumber   int
		LastModified string
		ETag         string
		Size         int64
	}
	res := struct {
		XMLName     xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListPartsResult"`
		Bucket      string
		Key         string
		UploadID    string `xml:"UploadId"`
		IsTruncated bool
		Parts       []part `xml:"Part"`
	}{Bucket: bucket, Key: key, UploadID: id}
	for _, p := range parts {
		res.Parts = append(res.Parts, part{p.Number, p.Modified.Format(timeFormat), p.ETag, p.Size})
	}
	writeXML(w, res)
	return nil
}

func (h *Handler) listUploads(w http.ResponseWriter, r *http.Request, bucket string) error {
	ups, err := h.Site.ListUploads(bucket, r.URL.Query().Get("prefix"))
	if err != nil {
		return err
	}
	type upload struct {
		Key       string
		UploadID  string `xml:"UploadId"`
		Initiated string
	}
	res := struct {
		XMLName     xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListMultipartUploadsResult"`
		Bucket      string
		IsTruncated bool
		Uploads     []upload `xml:"Upload"`
	}{Bucket: bucket}
	for _, u := range ups {
		res.Uploads = append(res.Uploads, upload{u.Key, u.ID, u.Created.Format(timeFormat)})
	}
	writeXML(w, res)
	return nil
}
