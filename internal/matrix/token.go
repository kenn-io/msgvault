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
// PickleKey protects the local Olm store and is generated independently for
// every account. Recovery keys and passphrases are deliberately absent.
type Credentials struct {
	Homeserver  string `json:"homeserver"`
	UserID      string `json:"user_id"`
	DeviceID    string `json:"device_id"`
	AccessToken string `json:"access_token"`
	PickleKey   string `json:"pickle_key"`
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
	if err := fileutil.SecureMkdirAll(tokensDir, 0o700); err != nil {
		return fmt.Errorf("create tokens dir: %w", err)
	}
	data, err := json.Marshal(creds, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("encode Matrix credentials: %w", err)
	}
	path := tokenPath(tokensDir, creds.UserID)
	if err := secureReplaceCredentials(path, data, 0o600); err != nil {
		return fmt.Errorf("write Matrix credentials: %w", err)
	}
	return nil
}

// LoadCredentials loads and identity-checks one Matrix credential file.
func LoadCredentials(tokensDir, userID string) (Credentials, error) {
	data, err := os.ReadFile(tokenPath(tokensDir, userID))
	if err != nil {
		if os.IsNotExist(err) {
			return Credentials{}, fmt.Errorf("no Matrix credentials for %s (run 'add-matrix' first)", userID)
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
	if creds.PickleKey == "" {
		creds.PickleKey, err = generatePickleKey()
		if err != nil {
			return Credentials{}, err
		}
		if err := SaveCredentials(tokensDir, creds); err != nil {
			return Credentials{}, fmt.Errorf("upgrade Matrix credentials with crypto key: %w", err)
		}
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
	_, err := os.Stat(tokenPath(tokensDir, userID))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("check Matrix credentials: %w", err)
}

// CryptoStorePath returns the per-device persistent crypto database path.
// Device scoping lets a failed or intentional re-login start with a clean Olm
// identity without colliding with the previous dedicated device.
func CryptoStorePath(dataDir, userID, deviceID string) string {
	return filepath.Join(dataDir, "matrix", accountKey(userID), accountKey(deviceID), "crypto.db")
}

// DeleteCryptoStore removes local crypto state for one dedicated device.
func DeleteCryptoStore(dataDir, userID, deviceID string) error {
	path := filepath.Dir(CryptoStorePath(dataDir, userID, deviceID))
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove Matrix crypto store: %w", err)
	}
	return nil
}

// DeleteAccountCryptoStores removes all local crypto state for an account.
func DeleteAccountCryptoStores(dataDir, userID string) error {
	path := filepath.Join(dataDir, "matrix", accountKey(userID))
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove Matrix account crypto stores: %w", err)
	}
	return nil
}
