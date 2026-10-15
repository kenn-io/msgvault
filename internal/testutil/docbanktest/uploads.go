// Package docbanktest provides the Docbank attachment upload fixture.
package docbanktest

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
)

const (
	FaultDrop           = -1 // close the connection after reading the upload
	FaultInvalidReceipt = -2 // answer with a body that is not a receipt
	FaultTimeout        = -3
	FaultDigestMismatch = -4
)

// UploadHandler validates uploads and returns idempotent receipts with optional faults.
type UploadHandler struct {
	t            *testing.T
	UploadFaults sync.Map // blob hash to HTTP status or fault constant
	Nodes        sync.Map
	Uploaded     sync.Map // blob hash to [name, content type]
	Uploads      atomic.Int32
}

func NewUploadHandler(t *testing.T) *UploadHandler {
	t.Helper()
	return &UploadHandler{t: t}
}

func (r *UploadHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	assert := assert.New(r.t)
	assert.Equal("/api/v1/uploads", req.URL.Path)
	assert.Equal("7", req.URL.Query().Get("parent_id"))
	r.Uploads.Add(1)
	hash := req.Header.Get("X-Docbank-Blob-Hash")
	fault, _ := r.UploadFaults.Load(hash)
	if code, ok := fault.(int); ok && code > 0 {
		w.WriteHeader(code)
		return
	}
	parts, err := req.MultipartReader()
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	part, err := parts.NextPart()
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(part)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	size, sizeErr := strconv.ParseInt(req.Header.Get("X-Docbank-Blob-Size"), 10, 64)
	_, extraErr := parts.NextPart()
	valid := assert.NoError(sizeErr)
	valid = assert.Equal(int64(len(body)), size) && valid
	valid = assert.Equal(fmt.Sprintf("%x", sha256.Sum256(body)), hash) && valid
	valid = assert.Equal("file", part.FormName()) && valid
	valid = assert.Equal(req.URL.Query().Get("name"), part.FileName()) && valid
	valid = assert.ErrorIs(extraErr, io.EOF) && valid
	if !valid {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	key := [3]string{req.URL.Query().Get("parent_id"), req.URL.Query().Get("name"), hash}
	node, existed := r.Nodes.LoadOrStore(key, int64(r.Uploads.Load()+7))
	status := "added"
	if existed {
		status = "skipped"
	}
	r.Uploaded.Store(hash, [2]string{part.FileName(), part.Header.Get("Content-Type")})
	switch fault {
	case FaultTimeout:
		_, _ = io.Copy(io.Discard, req.Body)
		<-req.Context().Done()
		return
	case FaultDrop:
		hijacker, ok := w.(http.Hijacker)
		if assert.True(ok) {
			conn, _, err := hijacker.Hijack()
			if assert.NoError(err) {
				assert.NoError(conn.Close())
			}
		}
		return
	case FaultInvalidReceipt:
		_, _ = io.WriteString(w, `{}`)
		return
	}
	receiptHash := hash
	if fault == FaultDigestMismatch {
		receiptHash = strings.Repeat("0", 64)
	}
	//nolint:gosec // test server echoes a synthetic hash header
	_, _ = fmt.Fprintf(w, `{"status":%q,"computed_hash":%q,"computed_size":%d,"node":{"id":%d,"parent_id":7,"kind":"file","name":%q,"blob_hash":%q,"size":%d,"current_version_id":"version"}}`, status, receiptHash, len(body), node, part.FileName(), receiptHash, len(body))
}
