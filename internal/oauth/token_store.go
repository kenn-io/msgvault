package oauth

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/fileutil"
)

// TokenStore owns Google token IO for one namespace beneath the configured
// tokens directory. Command keys never depend on local paths or symlinks.
type TokenStore struct {
	root      string
	namespace string
	commands  config.OAuthTokenCommands
}
type tokenSnapshot struct {
	data   []byte
	exists bool
}

// NewTokenStore returns the shared Google token namespace under root, the
// configured tokens directory.
func NewTokenStore(root string, commands config.OAuthTokenCommands) *TokenStore {
	return &TokenStore{root: root, commands: commands}
}

// Namespace returns the store for a slash-separated namespace under the same
// root, such as a dedicated Google Contacts authorization.
func (s *TokenStore) Namespace(namespace string) *TokenStore {
	return &TokenStore{root: s.root, namespace: namespace, commands: s.commands}
}

func (s *TokenStore) dir() string {
	return filepath.Join(s.root, filepath.FromSlash(s.namespace))
}

func (s *TokenStore) path(email string) string {
	if s.commands.Enabled() {
		return TokenFilePath(s.dir(), email)
	}
	return safeTokenFilePath(s.dir(), email)
}

// environment identifies a record by namespace and account so stores keep
// working when the data directory moves or another machine uses them.
func (s *TokenStore) environment(email string) []string {
	return []string{"MSGVAULT_TOKEN_NAMESPACE=" + s.namespace, "MSGVAULT_ACCOUNT=" + email}
}

// Read returns the stored token bytes, or an error matching os.ErrNotExist
// when the account has no token in this namespace.
func (s *TokenStore) Read(ctx context.Context, email string) ([]byte, error) {
	var data []byte
	var err error
	if s.commands.Enabled() {
		data, err = runSecretCommand(ctx, s.commands.ReadCommand, s.environment(email), nil)
		if exit, ok := errors.AsType[*commandExitError](err); ok && exit.code == 3 {
			return nil, os.ErrNotExist
		}
	} else {
		data, err = os.ReadFile(s.path(email))
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("read token: %w", err)
	}
	return data, nil
}
func (s *TokenStore) withLock(ctx context.Context, email string, fn func() error) (err error) {
	if err := fileutil.SecureMkdirAll(s.dir(), 0700); err != nil {
		return err
	}
	lock := flock.New(s.path(email)+".lock", flock.SetPermissions(0600))
	locked, err := lock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return fmt.Errorf("lock token: %w", err)
	}
	if !locked {
		return fmt.Errorf("lock token: %w", ctx.Err())
	}
	defer func() { err = errors.Join(err, lock.Unlock()) }()
	return fn()
}

// Write stores data for email without comparing it to the current token.
func (s *TokenStore) Write(ctx context.Context, email string, data []byte) error {
	return s.replace(ctx, email, data, nil)
}
func (s *TokenStore) replace(ctx context.Context, email string, data []byte, expected *tokenSnapshot) error {
	return s.withLock(ctx, email, func() error {
		if expected != nil {
			current, err := s.Read(ctx, email)
			exists := err == nil
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("read token before save: %w", err)
			}
			if exists != expected.exists || !bytes.Equal(current, expected.data) {
				return ErrTokenChanged
			}
		}
		if s.commands.Enabled() {
			if _, err := runSecretCommand(ctx, s.commands.WriteCommand, s.environment(email), data); err != nil {
				return fmt.Errorf("write token: %w", err)
			}
			return nil
		}
		if err := fileutil.SecureReplaceFile(s.path(email), data, 0600); err != nil {
			return fmt.Errorf("write token file: %w", err)
		}
		return nil
	})
}

// Delete removes the token for email. A missing token is not an error.
func (s *TokenStore) Delete(ctx context.Context, email string) error {
	return s.withLock(ctx, email, func() error {
		if s.commands.Enabled() {
			if _, err := runSecretCommand(ctx, s.commands.DeleteCommand, s.environment(email), nil); err != nil {
				return fmt.Errorf("delete token: %w", err)
			}
			return nil
		}
		err := os.Remove(s.path(email))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	})
}

// List runs the configured command to enumerate accounts in the secret store.
func (s *TokenStore) List(ctx context.Context) ([]string, error) {
	data, err := runSecretCommand(ctx, s.commands.ListCommand, s.environment(""), nil)
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	var accounts []string
	if json.Unmarshal(data, &accounts) != nil || accounts == nil {
		return nil, errors.New("list tokens: command must return a JSON array of accounts")
	}
	for _, account := range accounts {
		if strings.TrimSpace(account) == "" || strings.ContainsAny(account, "\x00\r\n") {
			return nil, errors.New("list tokens: invalid account")
		}
	}
	return accounts, nil
}
