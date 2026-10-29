// Package api contains the Telegram MTProto client integration.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/gofrs/flock"
	"github.com/gotd/td/session"
	"go.kenn.io/msgvault/internal/fileutil"
)

// SessionStore persists credentials and a gotd session in a protected file.
type SessionStore struct {
	dir     string
	ownerID int64
	secret  *sessionCredentials
}

type sessionCredentials struct {
	AppID   int    `json:"api_id"`
	AppHash string `json:"api_hash"`
}

type sessionFile struct {
	AppID   int    `json:"api_id"`
	AppHash string `json:"api_hash"`
	OwnerID int64  `json:"owner_id"`
	Session []byte `json:"session"`
}

func NewSessionStore(dir string, appID int, appHash string, ownerID int64) *SessionStore {
	return &SessionStore{dir: dir, ownerID: ownerID, secret: &sessionCredentials{AppID: appID, AppHash: appHash}}
}

// OpenSessionStore loads application credentials for a previously saved account.
func OpenSessionStore(dir string, ownerID int64) (*SessionStore, error) {
	s := &SessionStore{dir: dir, ownerID: ownerID}
	if ownerID <= 0 {
		return nil, errors.New("invalid Telegram owner ID")
	}
	stored, err := s.readFile()
	if err != nil {
		return nil, err
	}
	s.secret = &sessionCredentials{AppID: stored.AppID, AppHash: stored.AppHash}
	if err := s.validate(); err != nil {
		return nil, err
	}
	return s, nil
}

// ApplicationCredentials exposes credentials only for constructing a client.
func (s *SessionStore) ApplicationCredentials() (int, string) {
	if s.secret == nil {
		return 0, ""
	}
	return s.secret.AppID, s.secret.AppHash
}

func (s SessionStore) String() string                 { return fmt.Sprintf("Telegram session for user%d", s.ownerID) }
func (s SessionStore) GoString() string               { return s.String() }
func (s SessionStore) Format(state fmt.State, _ rune) { _, _ = io.WriteString(state, s.String()) }

func (s *SessionStore) path() string {
	return filepath.Join(s.dir, fmt.Sprintf("telegram_%d.json", s.ownerID))
}

func (s *SessionStore) validate() error {
	if s.ownerID <= 0 || s.secret == nil || s.secret.AppID <= 0 || int64(s.secret.AppID) > math.MaxInt32 || s.secret.AppHash == "" {
		return errors.New("invalid Telegram session credentials")
	}
	return nil
}

func (s *SessionStore) LoadSession(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.validate(); err != nil {
		return nil, err
	}
	stored, err := s.readFile()
	if err != nil {
		return nil, err
	}
	if stored.AppID != s.secret.AppID || stored.AppHash != s.secret.AppHash {
		return nil, errors.New("Telegram session credential mismatch")
	}
	return stored.Session, nil
}

func (s *SessionStore) readFile() (sessionFile, error) {
	var stored sessionFile
	root, err := os.Lstat(s.dir)
	if os.IsNotExist(err) {
		return stored, session.ErrNotFound
	}
	if err != nil {
		return stored, fmt.Errorf("inspect Telegram tokens directory: %w", err)
	}
	if root.Mode()&os.ModeSymlink != 0 || !root.IsDir() || insecureMode(root) {
		return stored, errors.New("Telegram tokens path is not a direct directory")
	}
	info, err := os.Lstat(s.path())
	if os.IsNotExist(err) {
		return stored, session.ErrNotFound
	}
	if err != nil {
		return stored, fmt.Errorf("inspect Telegram session: %w", err)
	}
	if !info.Mode().IsRegular() || insecureMode(info) {
		return stored, errors.New("Telegram session is not a protected regular file")
	}
	file, err := os.Open(s.path())
	if err != nil {
		return stored, fmt.Errorf("open Telegram session: %w", err)
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || insecureMode(opened) {
		return stored, errors.New("Telegram session changed while opening")
	}
	after, err := os.Lstat(s.dir)
	if err != nil || !os.SameFile(root, after) || !after.IsDir() || insecureMode(after) {
		return stored, errors.New("Telegram tokens directory changed while opening")
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return stored, fmt.Errorf("read Telegram session: %w", err)
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		return stored, errors.New("invalid Telegram session file")
	}
	if stored.OwnerID != s.ownerID {
		return stored, errors.New("Telegram session owner mismatch")
	}
	return stored, nil
}

func (s *SessionStore) StoreSession(ctx context.Context, data []byte) (retErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.validate(); err != nil {
		return err
	}
	if err := fileutil.SecureMkdirAll(s.dir, 0700); err != nil {
		return fmt.Errorf("create Telegram tokens directory: %w", err)
	}
	before, err := os.Lstat(s.dir)
	if err != nil {
		return fmt.Errorf("inspect Telegram tokens directory: %w", err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() || insecureMode(before) {
		return errors.New("Telegram tokens path is not a direct directory")
	}
	lockPath := filepath.Join(s.dir, ".telegram-token.lock")
	if err := validateLock(lockPath); err != nil {
		return err
	}
	lock := flock.New(lockPath, flock.SetPermissions(0600))
	locked, err := lock.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		return fmt.Errorf("lock Telegram token store: %w", err)
	}
	if !locked {
		return ctx.Err()
	}
	defer func() {
		if err := lock.Unlock(); err != nil && retErr == nil {
			retErr = fmt.Errorf("unlock Telegram token store: %w", err)
		}
	}()
	after, err := os.Lstat(s.dir)
	if err != nil || !os.SameFile(before, after) || after.Mode()&os.ModeSymlink != 0 || insecureMode(after) {
		return errors.New("Telegram tokens directory changed while locking")
	}
	if err := validateLock(lockPath); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	payload, err := json.Marshal(sessionFile{AppID: s.secret.AppID, AppHash: s.secret.AppHash, OwnerID: s.ownerID, Session: data})
	if err != nil {
		return errors.New("serialize Telegram session")
	}
	if err := fileutil.SecureReplaceFile(s.path(), payload, 0600); err != nil {
		return fmt.Errorf("replace Telegram session: %w", err)
	}
	return nil
}

func validateLock(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect Telegram credential lock: %w", err)
	}
	if !info.Mode().IsRegular() || insecureMode(info) {
		return errors.New("Telegram credential lock is not a protected regular file")
	}
	return nil
}

// WithLifecycleLock serializes account registration and sync operations.
func (s *SessionStore) WithLifecycleLock(fn func() error) error {
	return s.WithLifecycleLockContext(context.Background(), fn)
}

// WithLifecycleLockContext acquires the lifecycle lock until ctx is canceled.
func (s *SessionStore) WithLifecycleLockContext(ctx context.Context, fn func() error) (retErr error) {
	if fn == nil {
		return errors.New("Telegram lifecycle operation is missing")
	}
	if err := s.validate(); err != nil {
		return err
	}
	return WithLifecycleLockContext(ctx, s.dir, fn)
}

// WithLifecycleLockContext serializes credential registration and removal.
func WithLifecycleLockContext(ctx context.Context, dir string, fn func() error) (retErr error) {
	if fn == nil {
		return errors.New("Telegram lifecycle operation is missing")
	}
	if err := fileutil.SecureMkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create Telegram tokens directory: %w", err)
	}
	before, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect Telegram tokens directory: %w", err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() || insecureMode(before) {
		return errors.New("Telegram tokens path is not a direct directory")
	}
	lockPath := filepath.Join(dir, ".telegram-lifecycle.lock")
	if err := validateLock(lockPath); err != nil {
		return err
	}
	lock := flock.New(lockPath, flock.SetPermissions(0600))
	locked, err := lock.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		return fmt.Errorf("lock Telegram lifecycle: %w", err)
	}
	if !locked {
		return ctx.Err()
	}
	defer func() {
		if err := lock.Unlock(); err != nil && retErr == nil {
			retErr = fmt.Errorf("unlock Telegram lifecycle: %w", err)
		}
	}()
	after, err := os.Lstat(dir)
	if err != nil || !os.SameFile(before, after) || after.Mode()&os.ModeSymlink != 0 || insecureMode(after) {
		return errors.New("Telegram tokens directory changed while locking")
	}
	if err := validateLock(lockPath); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}

func insecureMode(info os.FileInfo) bool {
	return runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0
}

// DeleteSession removes a protected owner session. The caller holds the lifecycle lock.
func DeleteSession(ctx context.Context, dir string, ownerID int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ownerID <= 0 {
		return errors.New("invalid Telegram owner ID")
	}
	root, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect Telegram tokens directory: %w", err)
	}
	if root.Mode()&os.ModeSymlink != 0 || !root.IsDir() || insecureMode(root) {
		return errors.New("Telegram tokens path is not a protected directory")
	}
	path := filepath.Join(dir, fmt.Sprintf("telegram_%d.json", ownerID))
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect Telegram session: %w", err)
	}
	if !info.Mode().IsRegular() || insecureMode(info) {
		return errors.New("Telegram session is not a protected regular file")
	}
	after, err := os.Lstat(dir)
	if err != nil || !os.SameFile(root, after) || !after.IsDir() || insecureMode(after) {
		return errors.New("Telegram tokens directory changed during removal")
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove Telegram session: %w", err)
	}
	return nil
}
