package slack

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/msgvault/internal/fileutil"
)

// tokenFile stores one workspace user's token plus the identity it resolved
// to at add-slack time. Tokens are per-workspace-user, matching the source
// identifier: two users of the same workspace on one machine must not share
// (or clobber) each other's token.
type tokenFile struct {
	AccessToken string `json:"access_token"`
	TeamID      string `json:"team_id"`
	TeamDomain  string `json:"team_domain"`
	UserID      string `json:"user_id"`
}

// tokenPath returns the token file path for a workspace user.
func tokenPath(tokensDir, teamID, userID string) string {
	return filepath.Join(tokensDir, "slack_"+teamID+"_"+userID+".json")
}

// SaveToken saves a workspace's user token atomically (temp file + rename)
// with fileutil.Secure* hardening.
func SaveToken(tokensDir, teamID, teamDomain, userID, token string) error {
	if err := fileutil.SecureMkdirAll(tokensDir, 0700); err != nil {
		return fmt.Errorf("create tokens dir: %w", err)
	}
	data, err := json.Marshal(tokenFile{ //nolint:gosec // serialized to a 0600 file on the user's own machine
		AccessToken: token, TeamID: teamID, TeamDomain: teamDomain, UserID: userID,
	})
	if err != nil {
		return err
	}
	path := tokenPath(tokensDir, teamID, userID)

	tmpFile, err := os.CreateTemp(tokensDir, ".slack-token-*.tmp")
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

// LoadToken loads a workspace user's stored token, validating that the file
// belongs to the requested identity.
func LoadToken(tokensDir, teamID, userID string) (string, error) {
	data, err := os.ReadFile(tokenPath(tokensDir, teamID, userID))
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no Slack token for %s in workspace %s (run 'add-slack' first)", userID, teamID)
		}
		return "", fmt.Errorf("read slack token: %w", err)
	}
	var tf tokenFile
	if err := json.Unmarshal(data, &tf); err != nil {
		return "", fmt.Errorf("parse slack token: %w", err)
	}
	if (tf.TeamID != "" && tf.TeamID != teamID) || (tf.UserID != "" && tf.UserID != userID) {
		return "", fmt.Errorf("slack token file for %s/%s holds identity %s/%s — re-run 'add-slack'",
			teamID, userID, tf.TeamID, tf.UserID)
	}
	return tf.AccessToken, nil
}

// DeleteToken removes a workspace user's token file. Missing file is not an error.
func DeleteToken(tokensDir, teamID, userID string) error {
	err := os.Remove(tokenPath(tokensDir, teamID, userID))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
