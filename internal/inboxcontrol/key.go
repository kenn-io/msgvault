package inboxcontrol

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"go.kenn.io/msgvault/internal/fileutil"
)

// LoadPreviewKey reads existing daemon state without creating or changing it.
// Fail closed on malformed, public or nonregular files; never rotate a key
// implicitly, since that would invalidate pending previews after a restart.
func LoadPreviewKey(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("%w: inspect preview key %q: %w", ErrInternal, path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: preview key %q must be a regular file", ErrInternal, path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("%w: preview key %q permissions %04o must restrict access to its owner", ErrInternal, path, info.Mode().Perm())
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%w: open preview key %q: %w", ErrInternal, path, err)
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("%w: inspect opened preview key %q: %w", ErrInternal, path, err)
	}
	if !os.SameFile(info, opened) {
		return nil, fmt.Errorf("%w: preview key %q changed while opening", ErrInternal, path)
	}
	key, err := io.ReadAll(io.LimitReader(f, 33))
	if err != nil {
		return nil, fmt.Errorf("%w: read preview key %q: %w", ErrInternal, path, err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("%w: preview key %q must contain exactly 32 bytes", ErrInternal, path)
	}
	return key, nil
}

// EnsurePreviewKey runs only during daemon startup, before requests or sync.
// Exclusive creation preserves any existing installation key. Sync the file
// before returning it; a partial failed creation is removed, never reused.
func EnsurePreviewKey(path string) ([]byte, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return LoadPreviewKey(path)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, ErrInternal
	}
	if err := fileutil.SecureMkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, ErrInternal
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, ErrInternal
	}
	f, err := fileutil.SecureOpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return LoadPreviewKey(path)
	}
	if err != nil {
		return nil, ErrInternal
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if _, err := f.Write(key); err != nil {
		return nil, ErrInternal
	}
	if err := f.Sync(); err != nil {
		return nil, ErrInternal
	}
	if err := f.Close(); err != nil {
		return nil, ErrInternal
	}
	ok = true
	return key, nil
}
