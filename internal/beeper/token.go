package beeper

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/msgvault/internal/fileutil"
)

// tokenFile stores the Beeper Desktop access token. There is a single Beeper
// Desktop per machine (loopback-only API), so the file is a singleton rather
// than per-identifier.
type tokenFile struct {
	AccessToken string `json:"access_token"`
}

// tokenPath returns the path of the Beeper Desktop token file.
func tokenPath(tokensDir string) string {
	return filepath.Join(tokensDir, "beeper.json")
}

// SaveToken saves the Beeper Desktop access token. Uses atomic temp-file +
// rename to avoid partial writes, and fileutil.Secure* helpers for Windows
// DACL hardening.
func SaveToken(tokensDir, token string) error {
	if err := fileutil.SecureMkdirAll(tokensDir, 0700); err != nil {
		return fmt.Errorf("create tokens dir: %w", err)
	}
	data, err := json.Marshal(tokenFile{AccessToken: token}) //nolint:gosec // serialized to a 0600 file on the user's own machine
	if err != nil {
		return err
	}
	path := tokenPath(tokensDir)

	tmpFile, err := os.CreateTemp(tokensDir, ".beeper-token-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp token file: %w", err)
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write temp token file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp token file: %w", err)
	}
	if err := fileutil.SecureChmod(tmpPath, 0600); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("chmod temp token file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename temp token file: %w", err)
	}
	return nil
}

// LoadToken loads the Beeper Desktop access token.
func LoadToken(tokensDir string) (string, error) {
	data, err := os.ReadFile(tokenPath(tokensDir))
	if err != nil {
		if os.IsNotExist(err) {
			return "", errors.New("no Beeper Desktop token found (run 'add-beeper' first)")
		}
		return "", fmt.Errorf("read beeper token: %w", err)
	}
	var tf tokenFile
	if err := json.Unmarshal(data, &tf); err != nil {
		return "", fmt.Errorf("parse beeper token: %w", err)
	}
	return tf.AccessToken, nil
}

// DeleteToken removes the Beeper Desktop token file. Missing file is not an error.
func DeleteToken(tokensDir string) error {
	err := os.Remove(tokenPath(tokensDir))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
