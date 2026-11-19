package api

import (
	"fmt"
	"net/http"

	"go.kenn.io/msgvault/internal/store"
)

// releaseReadSnapshot finishes logical authorization and clears the request's snapshot.
func releaseReadSnapshot(r *http.Request) {
	if boundary, ok := r.Context().Value(readResponseKey{}).(*readResponseWriter); ok && boundary.release != nil {
		boundary.release()
	}
	*r = *r.WithContext(store.WithoutReadSnapshotContext(r.Context()))
}

// holdReadSnapshot keeps a scoped request's snapshot open past the first
// response byte, for a handler that streams from it. The middleware that began
// the read still releases it when the handler returns.
func holdReadSnapshot(r *http.Request) {
	if boundary, ok := r.Context().Value(readResponseKey{}).(*readResponseWriter); ok {
		boundary.release = nil
	}
}

type readResponseKey struct{}

type readResponseWriter struct {
	http.ResponseWriter

	release func()
}

func (w *readResponseWriter) WriteHeader(status int) {
	if w.release != nil {
		w.release()
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *readResponseWriter) Write(body []byte) (int, error) {
	if w.release != nil {
		w.release()
	}
	n, err := w.ResponseWriter.Write(body)
	if err != nil {
		return n, fmt.Errorf("write read response: %w", err)
	}
	return n, nil
}

func (w *readResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *readResponseWriter) Flush() { _ = http.NewResponseController(w.ResponseWriter).Flush() }
