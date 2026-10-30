package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"golang.org/x/time/rate"
)

// ErrMediaTooLarge indicates media was skipped because it exceeds the cap.
var ErrMediaTooLarge = errors.New("Telegram media exceeds size limit")

type mediaDCs interface {
	DC(context.Context, int, int64) (telegram.CloseInvoker, error)
	CDN(context.Context, int, int64) (telegram.CloseInvoker, error)
}

type mediaRPC struct {
	*tg.Client
	dcs     mediaDCs
	limiter *rate.Limiter
}

func (r mediaRPC) CDN(ctx context.Context, dc int, max int64) (downloader.CDN, io.Closer, error) {
	connection, err := r.dcs.CDN(ctx, dc, max)
	if err != nil {
		return nil, nil, err
	}
	if connection == nil {
		return nil, nil, errors.New("missing Telegram CDN connection")
	}
	guarded := &Invoker{next: connection, limiter: r.limiter, maxWait: 5 * time.Minute}
	return tg.NewClient(guarded), connection, nil
}

// DownloadMediaFromDC downloads using the file's data center and follows
// bounded FILE_MIGRATE redirects. The caller closes and removes the result.
func DownloadMediaFromDC(ctx context.Context, client *telegram.Client, ref *MediaReference, dir string, maxBytes int64, limiter *rate.Limiter) (*os.File, error) {
	if client == nil {
		return nil, errors.New("missing Telegram client")
	}
	return downloadMediaFromDC(ctx, client, ref, dir, maxBytes, limiter)
}

func downloadMediaFromDC(ctx context.Context, dcs mediaDCs, ref *MediaReference, dir string, maxBytes int64, limiter *rate.Limiter) (*os.File, error) {
	if dcs == nil || ref == nil || ref.Location == nil || ref.DCID <= 0 || ref.Size < -1 || maxBytes <= 0 || limiter == nil {
		return nil, errors.New("invalid Telegram media reference")
	}
	if ref.Size > maxBytes {
		return nil, ErrMediaTooLarge
	}
	dc := ref.DCID
	visited := make(map[int]bool)
	for len(visited) < 4 {
		if visited[dc] {
			return nil, errors.New("repeated Telegram media data center migration")
		}
		visited[dc] = true
		connection, err := dcs.DC(ctx, dc, 1)
		if err != nil {
			return nil, fmt.Errorf("open Telegram media data center: %w", err)
		}
		if connection == nil {
			return nil, errors.New("missing Telegram media data center connection")
		}
		guarded := &Invoker{next: connection, limiter: limiter, maxWait: 5 * time.Minute}
		rpc := mediaRPC{Client: tg.NewClient(guarded), dcs: dcs, limiter: limiter}
		builder := downloader.NewDownloader().WithAllowCDN(true).Download(rpc, ref.Location)
		file, downloadErr := DownloadMedia(ctx, builder, dir, ref.Size, maxBytes)
		if closeErr := connection.Close(); closeErr != nil {
			if file != nil {
				_ = file.Close()
				_ = os.Remove(file.Name())
			}
			return nil, errors.Join(downloadErr, fmt.Errorf("close Telegram media data center: %w", closeErr))
		}
		if downloadErr == nil {
			return file, nil
		}
		if migration, ok := tgerr.AsType(downloadErr, "FILE_MIGRATE"); ok && migration.Argument > 0 {
			dc = migration.Argument
			continue
		}
		return nil, downloadErr
	}
	return nil, errors.New("too many Telegram media data center migrations")
}

type cappedMediaWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *cappedMediaWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.remaining {
		return 0, ErrMediaTooLarge
	}
	n, err := w.writer.Write(data)
	w.remaining -= int64(n)
	return n, err
}

// DownloadMedia streams to a private temporary file and enforces the byte cap
// even when Telegram's size is unknown or understated. On success the caller
// owns the open file and must close and remove it after storing the content.
// declaredSize is -1 when unknown.
func DownloadMedia(ctx context.Context, builder *downloader.Builder, dir string, declaredSize, maxBytes int64) (_ *os.File, retErr error) {
	if builder == nil || declaredSize < -1 || maxBytes <= 0 {
		return nil, errors.New("invalid Telegram media download options")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if declaredSize > maxBytes {
		return nil, ErrMediaTooLarge
	}
	root, err := os.Lstat(dir)
	if err != nil || !root.IsDir() || root.Mode()&os.ModeSymlink != 0 || insecureMode(root) {
		return nil, errors.New("Telegram media temporary directory is not protected")
	}
	file, err := os.CreateTemp(dir, "telegram-media-*")
	if err != nil {
		return nil, fmt.Errorf("create Telegram media temporary file: %w", err)
	}
	defer func() {
		if retErr != nil {
			_ = file.Close()
			_ = os.Remove(file.Name())
		}
	}()
	after, err := os.Lstat(dir)
	if err != nil || !os.SameFile(root, after) || !after.IsDir() || insecureMode(after) {
		return nil, errors.New("Telegram media temporary directory changed")
	}
	writer := &cappedMediaWriter{writer: file, remaining: maxBytes}
	if _, err := builder.Stream(ctx, writer); err != nil {
		return nil, fmt.Errorf("download Telegram media: %w", err)
	}
	if declaredSize >= 0 && maxBytes-writer.remaining != declaredSize {
		return nil, errors.New("Telegram media size does not match the downloaded data")
	}
	if err := file.Sync(); err != nil {
		return nil, fmt.Errorf("sync Telegram media: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind Telegram media: %w", err)
	}
	return file, nil
}
