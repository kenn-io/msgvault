package github

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"go.kenn.io/msgvault/internal/fileutil"
)

// tokenFile stores one account's token and the account it resolved to
// when it was added.
type tokenFile struct {
	AccessToken string `json:"access_token"`
	AccountID   int64  `json:"account_id"`
	Login       string `json:"login"`
}

// Token is a stored token and the account it belongs to.
type Token struct {
	AccessToken string
	AccountID   int64
	Login       string
}

func tokenPath(tokensDir string, accountID int64) string {
	return filepath.Join(tokensDir, "github_"+strconv.FormatInt(accountID, 10)+".json")
}

// SaveToken stores an account's token in a file only the user can read,
// replacing it atomically.
func SaveToken(tokensDir string, token Token) error {
	if err := fileutil.SecureMkdirAll(tokensDir, 0o700); err != nil {
		return fmt.Errorf("create tokens dir: %w", err)
	}
	data, err := json.Marshal(tokenFile{ // serialized to a 0600 file on the user's own machine
		AccessToken: token.AccessToken, AccountID: token.AccountID, Login: token.Login,
	}, json.Deterministic(true))
	if err != nil {
		return err
	}
	if err := fileutil.SecureReplaceFile(tokenPath(tokensDir, token.AccountID), data, 0o600); err != nil {
		return fmt.Errorf("write GitHub token file: %w", err)
	}
	return nil
}

// LoadToken reads the token stored for an account.
func LoadToken(tokensDir string, accountID int64) (Token, error) {
	data, err := os.ReadFile(tokenPath(tokensDir, accountID))
	if errors.Is(err, os.ErrNotExist) {
		return Token{}, fmt.Errorf("no GitHub token for account %d (run 'add-github' first)", accountID)
	}
	if err != nil {
		return Token{}, fmt.Errorf("read GitHub token: %w", err)
	}
	var file tokenFile
	if err := json.Unmarshal(data, &file); err != nil {
		return Token{}, fmt.Errorf("parse GitHub token: %w", err)
	}
	if file.AccountID != accountID || file.AccessToken == "" {
		return Token{}, fmt.Errorf("GitHub token file for account %d holds another account; run 'add-github' again", accountID)
	}
	return Token{AccessToken: file.AccessToken, AccountID: file.AccountID, Login: file.Login}, nil
}

// DeleteToken removes an account's token. A missing file is not an error.
func DeleteToken(tokensDir string, accountID int64) error {
	if err := os.Remove(tokenPath(tokensDir, accountID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
