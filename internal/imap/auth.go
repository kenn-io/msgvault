package imap

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/msgvault/internal/fileutil"
)

type credentialsFile struct {
	Password string `json:"password"`
}

// CredentialsPath returns the path to the credentials file for the given identifier.
func CredentialsPath(tokensDir, identifier string) string {
	hash := sha256.Sum256([]byte(identifier))
	prefix := fmt.Sprintf("%x", hash[:8])
	return filepath.Join(tokensDir, "imap_"+prefix+".json")
}

// SaveCredentials saves an IMAP password for the given identifier.
// Uses atomic temp-file + rename to avoid partial writes, and
// fileutil.Secure* helpers for Windows DACL hardening.
func SaveCredentials(tokensDir, identifier, password string) error {
	if err := fileutil.SecureMkdirAll(tokensDir, 0700); err != nil {
		return fmt.Errorf("create tokens dir: %w", err)
	}
	creds := credentialsFile{Password: password}
	data, err := json.Marshal(creds)
	if err != nil {
		return err
	}
	path := CredentialsPath(tokensDir, identifier)

	// Atomic write via temp file + rename (matches OAuth token storage).
	tmpFile, err := os.CreateTemp(tokensDir, ".imap-cred-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp credentials file: %w", err)
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write temp credentials file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp credentials file: %w", err)
	}
	if err := fileutil.SecureChmod(tmpPath, 0600); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("chmod temp credentials file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename temp credentials file: %w", err)
	}
	return nil
}

// LoadCredentials loads an IMAP password for the given identifier.
func LoadCredentials(tokensDir, identifier string) (string, error) {
	path := CredentialsPath(tokensDir, identifier)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no credentials found for %s (run 'add-imap' first)", identifier)
		}
		return "", fmt.Errorf("read credentials: %w", err)
	}
	var creds credentialsFile
	if err := json.Unmarshal(data, &creds); err != nil {
		return "", fmt.Errorf("parse credentials: %w", err)
	}
	return creds.Password, nil
}

// HasCredentials returns true if credentials exist for the given identifier.
func HasCredentials(tokensDir, identifier string) bool {
	path := CredentialsPath(tokensDir, identifier)
	_, err := os.Stat(path)
	return err == nil
}
