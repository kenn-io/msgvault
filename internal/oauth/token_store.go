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

// TokenStore owns Google token IO. Command keys never depend on local symlinks.
type TokenStore struct {
	dir      string
	commands config.OAuthTokenCommands
}
type tokenSnapshot struct {
	data   []byte
	exists bool
}

func NewTokenStore(dir string, commands config.OAuthTokenCommands) *TokenStore {
	return &TokenStore{dir: dir, commands: commands}
}
func (s *TokenStore) path(email string) string {
	if s.commands.Enabled() {
		return filepath.Join(s.dir, sanitizeEmail(email)+".json")
	}
	return (&Manager{tokensDir: s.dir}).tokenPath(email)
}
func (s *TokenStore) environment(email string) []string {
	env := []string{"MSGVAULT_TOKEN_DIR=" + s.dir, "MSGVAULT_ACCOUNT=", "MSGVAULT_TOKEN_PATH="}
	if email != "" {
		env[1] = "MSGVAULT_ACCOUNT=" + email
		env[2] = "MSGVAULT_TOKEN_PATH=" + s.path(email)
	}
	return env
}

func (s *TokenStore) Read(ctx context.Context, email string) ([]byte, error) {
	var data []byte
	var err error
	if s.commands.Enabled() {
		if err := s.commands.Validate(); err != nil {
			return nil, err
		}
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
	if err := fileutil.SecureMkdirAll(s.dir, 0700); err != nil {
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
			if err := s.commands.Validate(); err != nil {
				return err
			}
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

func (s *TokenStore) Delete(ctx context.Context, email string) error {
	return s.withLock(ctx, email, func() error {
		if s.commands.Enabled() {
			if err := s.commands.Validate(); err != nil {
				return err
			}
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

// List runs the list command. File mode finds aliases by reading the token directory.
func (s *TokenStore) List(ctx context.Context) ([]string, error) {
	if err := s.commands.Validate(); err != nil {
		return nil, err
	}
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
