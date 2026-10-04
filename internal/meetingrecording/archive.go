// Package meetingrecording streams verified meeting audio into the shared
// content-addressed attachment layout. Providers own retrieval and occurrence state.
package meetingrecording

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"

	"go.kenn.io/kit/packstore"
)

var (
	ErrSizeLimit = errors.New("recording exceeds media byte limit")
	ErrNotAudio  = errors.New("recording is not recognized audio")
)

type Blob struct {
	Path string
	Hash string
	Size int64
}

// StoreAudio publishes only a complete bounded stream with a recognized audio
// signature. root uses the same loose layout as attachmentstore and export.
// The caller owns and closes reader; no bytes are published on acquisition errors.
func StoreAudio(ctx context.Context, root string, reader io.Reader, maxBytes int64) (Blob, error) {
	if root == "" || reader == nil || maxBytes <= 0 {
		return Blob{}, errors.New("recording storage requires a directory, reader and positive byte limit")
	}
	if err := ctx.Err(); err != nil {
		return Blob{}, err
	}
	buffered := bufio.NewReader(&boundedReader{ctx: ctx, r: reader, remaining: maxBytes})
	header, err := buffered.Peek(12)
	if err != nil && !errors.Is(err, io.EOF) {
		return Blob{}, fmt.Errorf("read recording header: %w", err)
	}
	if !audioHeader(header) {
		return Blob{}, ErrNotAudio
	}
	layout, err := packstore.NewLayout(root, packstore.LayoutOptions{Staging: packstore.StagingStoreDirectory, StagingDir: "."})
	if err != nil {
		return Blob{}, fmt.Errorf("recording layout: %w", err)
	}
	loose, err := packstore.NewLooseStore(layout)
	if err != nil {
		return Blob{}, fmt.Errorf("recording storage: %w", err)
	}
	result, err := loose.Write(ctx, buffered, packstore.WriteOptions{Durability: packstore.DurablePublication, Dedup: packstore.VerifyFullHash, MaxBytes: maxBytes})
	if err != nil {
		return Blob{}, fmt.Errorf("store recording: %w", err)
	}
	hash := result.Hash.String()
	return Blob{Path: path.Join(hash[:2], hash), Hash: hash, Size: result.Size}, nil
}

func audioHeader(h []byte) bool {
	return len(h) >= 12 && bytes.Equal(h[:4], []byte("RIFF")) && bytes.Equal(h[8:12], []byte("WAVE")) ||
		len(h) >= 3 && bytes.Equal(h[:3], []byte("ID3")) ||
		len(h) >= 2 && h[0] == 0xff && h[1]&0xe0 == 0xe0 ||
		len(h) >= 4 && (bytes.Equal(h[:4], []byte("fLaC")) || bytes.Equal(h[:4], []byte("OggS")))
}

type boundedReader struct {
	ctx       context.Context
	r         io.Reader
	remaining int64
}

func (r *boundedReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > r.remaining+1 {
		p = p[:r.remaining+1]
	}
	n, err := r.r.Read(p)
	r.remaining -= int64(n)
	if r.remaining < 0 {
		return 0, ErrSizeLimit
	}
	return n, err
}
