package carddav

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const cardDAVTokenFilename = "carddav.json" // #nosec G101 -- This is a credential filename, not a credential value.

var ErrCredentialNotBound = errors.New("CardDAV credential is not bound to a connection")

// Credential binds a password to the exact durable connection it may
// authenticate. The generation closes the crash window between publishing the
// filesystem pair and replacing the discovery snapshot: startup fails closed
// until config, token, and database all describe the same connection.
type Credential struct {
	Password             string `json:"password"`
	BaseURL              string `json:"base_url,omitempty"`
	Username             string `json:"username,omitempty"`
	ConnectionGeneration int64  `json:"connection_generation,omitempty"`
}

type credentialPermissionBackend interface {
	secureDirectory(path string) error
	secureFile(file *os.File) error
	verifyFile(file *os.File) error
}

// SavePassword atomically replaces the CardDAV token file. The password is
// deliberately kept out of config and durable database records.
func SavePassword(tokenDir, password string) error {
	return savePasswordWithPermissions(tokenDir, password, nativeCredentialPermissions{})
}

func savePasswordWithPermissions(tokenDir, password string, permissions credentialPermissionBackend) error {
	return saveCredentialWithPermissions(tokenDir, Credential{Password: password}, permissions)
}

// SaveCredential atomically publishes an identity-bound CardDAV credential.
func SaveCredential(tokenDir string, credential Credential) error {
	if credential.Password == "" || credential.BaseURL == "" || credential.Username == "" || credential.ConnectionGeneration <= 0 {
		return errors.New("CardDAV credential requires a connection identity")
	}
	return saveCredentialWithPermissions(tokenDir, credential, nativeCredentialPermissions{})
}

// RemoveCredential removes a published CardDAV credential. Missing files are
// already the desired state and therefore succeed.
func RemoveCredential(tokenDir string) error {
	err := os.Remove(filepath.Join(tokenDir, cardDAVTokenFilename))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove CardDAV token file: %w", err)
	}
	return nil
}

func saveCredentialWithPermissions(tokenDir string, credential Credential, permissions credentialPermissionBackend) error {
	if err := permissions.secureDirectory(tokenDir); err != nil {
		return fmt.Errorf("secure CardDAV token directory: %w", err)
	}

	temporary, err := os.CreateTemp(tokenDir, ".carddav-*.json")
	if err != nil {
		return fmt.Errorf("create CardDAV token file: %w", err)
	}
	temporaryPath := temporary.Name()
	keep := false
	defer func() {
		_ = temporary.Close()
		if !keep {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := permissions.secureFile(temporary); err != nil {
		return fmt.Errorf("secure CardDAV token file: %w", err)
	}
	// #nosec G117 -- The credential is intentionally marshaled only into the already-hardened private token file.
	if err := json.NewEncoder(temporary).Encode(credential); err != nil {
		return fmt.Errorf("encode CardDAV token file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync CardDAV token file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close CardDAV token file: %w", err)
	}
	target := filepath.Join(tokenDir, cardDAVTokenFilename)
	if err := os.Rename(temporaryPath, target); err != nil {
		return fmt.Errorf("replace CardDAV token file: %w", err)
	}
	// Rename publishes the already-hardened filesystem object; its mode/DACL
	// travels with it. Keep publication as the final fallible operation so an
	// error before replacement always leaves the prior credential intact.
	keep = true
	return nil
}

// LoadPassword reads the private CardDAV token file and rejects files exposed
// to group or other users. Errors never include the token contents.
func LoadPassword(tokenDir string) (string, error) {
	return loadPasswordWithPermissions(tokenDir, nativeCredentialPermissions{})
}

func loadPasswordWithPermissions(tokenDir string, permissions credentialPermissionBackend) (string, error) {
	credential, err := loadCredentialWithPermissions(tokenDir, permissions)
	if err != nil {
		return "", err
	}
	return credential.Password, nil
}

// LoadCredential reads an identity-bound private credential. Legacy
// password-only files remain readable through LoadPassword, but are rejected
// here so the daemon can never pair them with an arbitrary configured origin.
func LoadCredential(tokenDir string) (Credential, error) {
	credential, err := loadCredentialWithPermissions(tokenDir, nativeCredentialPermissions{})
	if err != nil {
		return Credential{}, err
	}
	if credential.BaseURL == "" || credential.Username == "" || credential.ConnectionGeneration <= 0 {
		return Credential{}, ErrCredentialNotBound
	}
	return credential, nil
}

// LoadLegacyPassword reads only the historical password-only token shape.
// Partially bound records fail closed instead of being silently rebound.
func LoadLegacyPassword(tokenDir string) (string, error) {
	credential, err := loadCredentialWithPermissions(tokenDir, nativeCredentialPermissions{})
	if err != nil {
		return "", err
	}
	if credential.BaseURL != "" || credential.Username != "" || credential.ConnectionGeneration != 0 {
		return "", errors.New("CardDAV credential is not a legacy password-only record")
	}
	return credential.Password, nil
}

func loadCredentialWithPermissions(tokenDir string, permissions credentialPermissionBackend) (Credential, error) {
	path := filepath.Join(tokenDir, cardDAVTokenFilename)
	file, err := os.Open(path)
	if err != nil {
		return Credential{}, fmt.Errorf("open CardDAV token file: %w", err)
	}
	defer file.Close() //nolint:errcheck // read-only file
	if err := permissions.verifyFile(file); err != nil {
		return Credential{}, fmt.Errorf("verify CardDAV token file permissions: %w", err)
	}
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	var saved Credential
	if err := decoder.Decode(&saved); err != nil {
		return Credential{}, fmt.Errorf("decode CardDAV token file: %w", err)
	}
	if saved.Password == "" {
		return Credential{}, errors.New("CardDAV token file contains an empty password")
	}
	return saved, nil
}
