package matrix

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
	"go.kenn.io/msgvault/internal/fileutil"
)

// Credentials are the durable credentials for one dedicated Matrix device.
type Credentials struct {
	Homeserver  string `json:"homeserver"`
	UserID      string `json:"user_id"`
	DeviceID    string `json:"device_id"`
	AccessToken string `json:"access_token"`
}

var secureReplaceCredentials = fileutil.SecureReplaceFile

// WithCredentialLifecycleLock serializes Matrix device creation and removal
// across processes. The callback must re-check both credential and source
// state after acquiring the lock.
func WithCredentialLifecycleLock(tokensDir string, fn func() error) (retErr error) {
	if fn == nil {
		return errors.New("matrix credential lifecycle operation is missing")
	}
	if err := fileutil.SecureMkdirAll(tokensDir, 0o700); err != nil {
		return fmt.Errorf("create tokens dir: %w", err)
	}
	lifecycleLock := flock.New(filepath.Join(tokensDir, ".matrix-lifecycle.lock"), flock.SetPermissions(0o600))
	if err := lifecycleLock.Lock(); err != nil {
		return fmt.Errorf("lock Matrix credential lifecycle: %w", err)
	}
	defer func() {
		if err := lifecycleLock.Unlock(); err != nil && retErr == nil {
			retErr = fmt.Errorf("unlock Matrix credential lifecycle: %w", err)
		}
	}()
	return fn()
}

func accountKey(userID string) string {
	sum := sha256.Sum256([]byte(userID))
	return hex.EncodeToString(sum[:12])
}

func tokenPath(tokensDir, userID string) string {
	return filepath.Join(tokensDir, "matrix_"+accountKey(userID)+".json")
}

// SaveCredentials atomically writes credentials to a 0600 file.
func SaveCredentials(tokensDir string, creds Credentials) error {
	return writeCredentials(tokensDir, tokenPath(tokensDir, creds.UserID), creds)
}

// LoadCredentials loads and identity-checks one Matrix credential file.
func LoadCredentials(tokensDir, userID string) (Credentials, error) {
	creds, err := readCredentials(tokenPath(tokensDir, userID), userID)
	if os.IsNotExist(err) {
		return Credentials{}, fmt.Errorf("no Matrix credentials for %s (run 'add-matrix' first)", userID)
	}
	return creds, err
}

// SavePendingCredentials durably records a renewed login that has not yet
// replaced the account's credential file, so a failed renewal can resume it.
func SavePendingCredentials(tokensDir string, creds Credentials) error {
	return writeCredentials(tokensDir, pendingTokenPath(tokensDir, creds.UserID), creds)
}

// LoadPendingCredentials returns the unfinished renewal for an account, if any.
func LoadPendingCredentials(tokensDir, userID string) (Credentials, bool, error) {
	creds, err := readCredentials(pendingTokenPath(tokensDir, userID), userID)
	if os.IsNotExist(err) {
		return Credentials{}, false, nil
	}
	if err != nil {
		return Credentials{}, false, fmt.Errorf("pending Matrix login: %w", err)
	}
	return creds, true, nil
}

// PendingCredentialsExist reports whether an account has an unfinished renewal.
func PendingCredentialsExist(tokensDir, userID string) (bool, error) {
	return fileExists(pendingTokenPath(tokensDir, userID))
}

// DeletePendingCredentials removes an account's unfinished renewal record.
func DeletePendingCredentials(tokensDir, userID string) error {
	err := os.Remove(pendingTokenPath(tokensDir, userID))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func pendingTokenPath(tokensDir, userID string) string {
	return filepath.Join(tokensDir, "pending_matrix_"+accountKey(userID)+".json")
}

func writeCredentials(tokensDir, path string, creds Credentials) error {
	if err := fileutil.SecureMkdirAll(tokensDir, 0o700); err != nil {
		return fmt.Errorf("create tokens dir: %w", err)
	}
	data, err := json.Marshal(creds, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("encode Matrix credentials: %w", err)
	}
	if err := secureReplaceCredentials(path, data, 0o600); err != nil {
		return fmt.Errorf("write Matrix credentials: %w", err)
	}
	return nil
}

func readCredentials(path, userID string) (Credentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Credentials{}, err
		}
		return Credentials{}, fmt.Errorf("read Matrix credentials: %w", err)
	}
	var creds Credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return Credentials{}, fmt.Errorf("parse Matrix credentials: %w", err)
	}
	if creds.UserID != userID || creds.Homeserver == "" || creds.DeviceID == "" || creds.AccessToken == "" {
		return Credentials{}, fmt.Errorf("matrix credential file for %s is incomplete or belongs to %s", userID, creds.UserID)
	}
	return creds, nil
}

// DeleteCredentials removes the dedicated device credential for an account.
func DeleteCredentials(tokensDir, userID string) error {
	err := os.Remove(tokenPath(tokensDir, userID))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// CredentialsExist reports whether an account has a stored device credential.
func CredentialsExist(tokensDir, userID string) (bool, error) {
	return fileExists(tokenPath(tokensDir, userID))
}

func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("check Matrix credentials: %w", err)
}
