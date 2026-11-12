package macoscalls

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	"github.com/mattn/go-sqlite3"
)

func fileDSN(path, mode string) string {
	path = filepath.ToSlash(path)
	if len(path) > 1 && path[1] == ':' {
		path = "/" + path
	}
	return (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=" + mode + "&_busy_timeout=50"}).String()
}

// backup includes committed WAL pages without checkpointing or changing the
// source's journal mode. Bounded steps allow cancellation, including on BUSY.
func backup(ctx context.Context, dest, source *sql.Conn) error {
	return dest.Raw(func(d any) error {
		destination, ok := d.(*sqlite3.SQLiteConn)
		if !ok {
			return errors.New("unexpected SQLite destination connection type")
		}
		return source.Raw(func(s any) (retErr error) {
			source, ok := s.(*sqlite3.SQLiteConn)
			if !ok {
				return errors.New("unexpected SQLite source connection type")
			}
			b, err := destination.Backup("main", source, "main")
			if err != nil {
				return fmt.Errorf("start SQLite snapshot backup: %w", err)
			}
			defer func() {
				if err := b.Finish(); err != nil {
					retErr = errors.Join(retErr, fmt.Errorf("finish SQLite snapshot backup: %w", err))
				}
			}()
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				done, err := b.Step(128)
				if err != nil {
					return fmt.Errorf("copy SQLite snapshot pages: %w", err)
				}
				if done {
					return nil
				}
				// SQLite reports BUSY/LOCKED as an incomplete step. Yield until a retry
				// or cancellation rather than spinning against another process's lock.
				timer := time.NewTimer(10 * time.Millisecond) // Waits for an external SQLite lock or backup progress.
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
			}
		})
	})
}

func copySnapshot(ctx context.Context, path, destination string) error {
	source, err := sql.Open("sqlite3", fileDSN(path, "ro"))
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	src, err := source.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	dest, err := sql.Open("sqlite3", fileDSN(destination, "rwc"))
	if err != nil {
		return err
	}
	defer func() { _ = dest.Close() }()
	dst, err := dest.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = dst.Close() }()
	err = backup(ctx, dst, src)
	if ctx.Err() != nil {
		return errors.Join(ctx.Err(), err)
	}
	return err
}
