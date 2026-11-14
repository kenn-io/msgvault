package forge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/msgvault/internal/fileutil"
)

// Token is a stored forge token and the account it belongs to.
type Token struct {
	AccessToken string `json:"access_token"`
	// BaseURL is the instance's web base URL, with its scheme.
	BaseURL   string   `json:"base_url"`
	Instance  Instance `json:"instance"`
	AccountID string   `json:"account_id"`
	Login     string   `json:"login"`
}

// tokenPath names an account's token file by its forge and a digest of its
// source identifier, which holds characters a file name can't.
func tokenPath(tokensDir, forge, sourceIdentifier string) string {
	digest := sha256.Sum256([]byte(sourceIdentifier))
	return filepath.Join(tokensDir, forge+"_"+hex.EncodeToString(digest[:8])+".json")
}

// SaveToken stores an account's token in a file only the user can read,
// replacing it atomically.
func SaveToken(tokensDir, forge string, token Token) error {
	if err := fileutil.SecureMkdirAll(tokensDir, 0o700); err != nil {
		return fmt.Errorf("create tokens dir: %w", err)
	}
	data, err := json.Marshal(token, json.Deterministic(true)) // serialized to a 0600 file on the user's own machine
	if err != nil {
		return err
	}
	path := tokenPath(tokensDir, forge, SourceIdentifier(token.Instance, token.AccountID))
	if err := fileutil.SecureReplaceFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write %s token file: %w", forge, err)
	}
	return nil
}

// LoadToken reads the token stored for a source.
func LoadToken(tokensDir, forge, sourceIdentifier string) (Token, error) {
	data, err := os.ReadFile(tokenPath(tokensDir, forge, sourceIdentifier))
	if errors.Is(err, os.ErrNotExist) {
		return Token{}, fmt.Errorf("no %s token for %s (add the account first)", forge, sourceIdentifier)
	}
	if err != nil {
		return Token{}, fmt.Errorf("read %s token: %w", forge, err)
	}
	var token Token
	if err := json.Unmarshal(data, &token); err != nil {
		return Token{}, fmt.Errorf("parse %s token: %w", forge, err)
	}
	if token.AccessToken == "" || SourceIdentifier(token.Instance, token.AccountID) != sourceIdentifier {
		return Token{}, fmt.Errorf("%s token file for %s holds another account; add the account again", forge, sourceIdentifier)
	}
	return token, nil
}

// DeleteToken removes a source's token. A missing file is not an error.
func DeleteToken(tokensDir, forge, sourceIdentifier string) error {
	if err := os.Remove(tokenPath(tokensDir, forge, sourceIdentifier)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
