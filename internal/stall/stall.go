// Package stall limits HTTP requests by progress instead of total time: a
// request fails when nothing happened for a while (no bytes sent, no answer,
// no bytes received), however long it takes while it is working. Connecting
// keeps its own short limits (the transport's dial and handshake timeouts).
package stall

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"
)

// Timeout is how long a request may go without progress.
var Timeout = 30 * time.Second

// ErrStalled is returned when a request made no progress for Timeout.
var ErrStalled = errors.New("no progress for 30 seconds: the other side stopped responding")

// Watch cancels its context when not touched for its timeout.
type Watch struct {
	t *time.Timer
	d time.Duration
}

// Touch records progress.
func (w *Watch) Touch() { w.t.Reset(w.d) }

// With returns a context that ends (cause ErrStalled) when w isn't touched
// for Timeout, and a function to release it.
func With(ctx context.Context) (context.Context, *Watch, context.CancelFunc) {
	ctx, stop := context.WithCancelCause(ctx)
	w := &Watch{d: Timeout}
	w.t = time.AfterFunc(w.d, func() { stop(ErrStalled) })
	return ctx, w, func() { w.t.Stop(); stop(nil) }
}

// Err reports err as ErrStalled when that is why ctx ended.
func Err(ctx context.Context, err error) error {
	if err != nil && errors.Is(context.Cause(ctx), ErrStalled) {
		return ErrStalled
	}
	return err
}

// Reader touches w whenever data passes.
func Reader(r io.Reader, w *Watch) io.Reader { return reader{r, w} }

type reader struct {
	r io.Reader
	w *Watch
}

func (r reader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.w.Touch()
	}
	return n, err
}

type body struct {
	io.Reader
	c    io.Closer
	done func()
}

func (b body) Close() error {
	err := b.c.Close()
	b.done()
	return err
}

// Do sends req with client, limited by progress: sending the body, waiting
// for the answer and reading it each reset the clock. The response body must
// be closed, as always; that also releases the watch. The client should have
// no total Timeout of its own.
func Do(client *http.Client, req *http.Request) (*http.Response, error) {
	ctx, w, cancel := With(req.Context())
	req = req.WithContext(ctx)
	if req.Body != nil && req.Body != http.NoBody {
		orig := req.Body
		req.Body = body{Reader(orig, w), orig, func() {}}
		if get := req.GetBody; get != nil {
			req.GetBody = func() (io.ReadCloser, error) {
				b, err := get()
				if err != nil {
					return nil, err
				}
				return body{Reader(b, w), b, func() {}}, nil
			}
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		return nil, Err(ctx, err)
	}
	w.Touch()
	resp.Body = body{errReader{Reader(resp.Body, w), ctx}, resp.Body, cancel}
	return resp, nil
}

// errReader reports a stalled read as ErrStalled.
type errReader struct {
	r   io.Reader
	ctx context.Context
}

func (e errReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil && err != io.EOF {
		err = Err(e.ctx, err)
	}
	return n, err
}
