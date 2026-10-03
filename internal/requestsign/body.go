package requestsign

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"os"
	"sync"

	"go.kenn.io/msgvault/internal/fileutil"
)

var ErrBodyTooLarge = errors.New("request body exceeds signing byte limit")

type spoolBody struct {
	*os.File

	once sync.Once
}

func (b *spoolBody) Close() error {
	var err error
	b.once.Do(func() { err = b.File.Close(); _ = os.Remove(b.Name()) })
	return err
}

type contextReader struct {
	ctx context.Context
	src io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.src.Read(p)
}

// Spool hashes at most maxBytes bytes into a private temporary file. The replacement
// body owns file cleanup; callers must close it or invoke cleanup on every path.
func Spool(r *http.Request, maxBytes int64) (string, func(), error) {
	if maxBytes <= 0 || maxBytes > MaxRequestBytes {
		return "", nil, ErrBodyTooLarge
	}
	if err := r.Context().Err(); err != nil {
		return "", nil, err
	}
	if r.ContentLength > maxBytes {
		if r.Body != nil {
			_ = r.Body.Close()
		}
		return "", nil, ErrBodyTooLarge
	}
	if r.Body == nil || r.Body == http.NoBody {
		digest := sha256.Sum256(nil)
		r.Body = http.NoBody
		r.ContentLength = 0
		r.GetBody = nil
		return "sha-256=:" + base64.StdEncoding.EncodeToString(digest[:]) + ":", func() {}, nil
	}
	original := r.Body
	defer func() { _ = original.Close() }()
	stop := context.AfterFunc(r.Context(), func() { _ = original.Close() })
	defer stop()
	file, err := os.CreateTemp("", "msgvault-request-*")
	if err != nil {
		return "", nil, err
	}
	body := &spoolBody{File: file}
	cleanup := func() { _ = body.Close() }
	if err := fileutil.SecureChmod(file.Name(), 0o600); err != nil {
		cleanup()
		return "", nil, err
	}
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(contextReader{ctx: r.Context(), src: original}, maxBytes+1))
	if err == nil {
		err = r.Context().Err()
	}
	if err != nil {
		cleanup()
		return "", nil, err
	}
	if size > maxBytes {
		cleanup()
		return "", nil, ErrBodyTooLarge
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return "", nil, err
	}
	r.Body = body
	r.ContentLength = size
	r.GetBody = nil
	return "sha-256=:" + base64.StdEncoding.EncodeToString(hash.Sum(nil)) + ":", cleanup, nil
}
