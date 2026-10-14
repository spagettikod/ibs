package s3api

import (
	"encoding/base64"
	"encoding/xml"
	"errors"
	"net/http"

	"ibs/internal/site"
)

type Error struct {
	Code    string
	Message string
	Status  int
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func errAccessDenied(msg string) *Error {
	return &Error{"AccessDenied", msg, http.StatusForbidden}
}

func errAuthMalformed(msg string) *Error {
	return &Error{"AuthorizationHeaderMalformed", msg, http.StatusBadRequest}
}

func errNotImplemented(msg string) *Error {
	return &Error{"NotImplemented", msg, http.StatusNotImplemented}
}

func errInvalidArgument(msg string) *Error {
	return &Error{"InvalidArgument", msg, http.StatusBadRequest}
}

var errMalformedXML = &Error{"MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema.", http.StatusBadRequest}

// toS3Error maps internal errors to S3 error responses.
func toS3Error(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	var de *digestError
	if errors.As(err, &de) {
		switch de.code {
		case "SignatureDoesNotMatch":
			return &Error{de.code, "A chunk signature did not match.", http.StatusForbidden}
		case "BadDigest":
			return &Error{de.code, "The Content-MD5 you specified did not match what we received.", http.StatusBadRequest}
		default:
			return &Error{de.code, "The provided x-amz-content-sha256 does not match what was computed.", http.StatusBadRequest}
		}
	}
	switch {
	case errors.Is(err, errChunk):
		return &Error{"IncompleteBody", "The aws-chunked request body is malformed.", http.StatusBadRequest}
	case errors.Is(err, site.ErrNoSuchBucket):
		return &Error{"NoSuchBucket", "The specified bucket does not exist.", http.StatusNotFound}
	case errors.Is(err, site.ErrNoSuchKey):
		return &Error{"NoSuchKey", "The specified key does not exist.", http.StatusNotFound}
	case errors.Is(err, site.ErrBucketExists):
		return &Error{"BucketAlreadyOwnedByYou", "Your previous request to create the named bucket succeeded and you already own it.", http.StatusConflict}
	case errors.Is(err, site.ErrBucketNotEmpty):
		return &Error{"BucketNotEmpty", "The bucket you tried to delete is not empty.", http.StatusConflict}
	case errors.Is(err, site.ErrInvalidBucketName):
		return &Error{"InvalidBucketName", "The specified bucket is not valid.", http.StatusBadRequest}
	case errors.Is(err, site.ErrNoSuchUpload):
		return &Error{"NoSuchUpload", "The specified multipart upload does not exist.", http.StatusNotFound}
	case errors.Is(err, site.ErrInvalidPart):
		return &Error{"InvalidPart", err.Error(), http.StatusBadRequest}
	case errors.Is(err, site.ErrInvalidPartOrder):
		return &Error{"InvalidPartOrder", err.Error(), http.StatusBadRequest}
	case errors.Is(err, site.ErrNoSpace):
		return &Error{"StorageFull", "Storage is full: " + site.ErrNoSpace.Error() + ".", http.StatusInsufficientStorage}
	case errors.Is(err, site.ErrInvalidRange):
		return &Error{"InvalidRange", "The requested range is not satisfiable.", http.StatusRequestedRangeNotSatisfiable}
	}
	return &Error{"InternalError", err.Error(), http.StatusInternalServerError}
}

func writeError(w http.ResponseWriter, r *http.Request, e *Error) {
	for _, h := range []string{"Content-Length", "ETag", "Last-Modified", "Accept-Ranges", "Content-Disposition"} {
		w.Header().Del(h)
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(e.Status)
	if r.Method == http.MethodHead {
		return
	}
	writeXMLBody(w, struct {
		XMLName   xml.Name `xml:"Error"`
		Code      string
		Message   string
		Resource  string
		RequestID string `xml:"RequestId"`
	}{Code: e.Code, Message: e.Message, Resource: r.URL.Path, RequestID: w.Header().Get("x-amz-request-id")})
}

func writeXMLBody(w http.ResponseWriter, v any) {
	w.Write([]byte(xml.Header))
	xml.NewEncoder(w).Encode(v)
}

func writeXML(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/xml")
	writeXMLBody(w, v)
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
