package slack

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/msgvault/internal/fileutil"
)

// tokenFile stores credentials per workspace user, matching the source ID.
// Two users of one workspace must not share or overwrite credentials.
type tokenFile struct {
	AccessToken string `json:"access_token"`
	Cookie      string `json:"cookie,omitempty"`
	TeamID      string `json:"team_id"`
	TeamDomain  string `json:"team_domain"`
	UserID      string `json:"user_id"`
}

func tokenPath(tokensDir, teamID, userID string) string {
	return filepath.Join(tokensDir, "slack_"+teamID+"_"+userID+".json")
}

// SaveToken saves an OAuth user token. Use SaveCredentials for browser sessions.
func SaveToken(tokensDir, teamID, teamDomain, userID, token string) error {
	return SaveCredentials(tokensDir, teamID, teamDomain, userID, Credentials{Token: token})
}

// SaveCredentials atomically stores a validated credential pair in a 0600 file.
func SaveCredentials(tokensDir, teamID, teamDomain, userID string, credentials Credentials) error {
	if err := credentials.Validate(); err != nil {
		return err
	}
	if err := fileutil.SecureMkdirAll(tokensDir, 0700); err != nil {
		return fmt.Errorf("create tokens dir: %w", err)
	}
	data, err := json.Marshal(tokenFile{ // serialized to a 0600 file on the user's own machine
		AccessToken: credentials.Token, Cookie: credentials.Cookie,
		TeamID: teamID, TeamDomain: teamDomain, UserID: userID,
	}, json.Deterministic(true))
	if err != nil {
		return err
	}
	if err := fileutil.SecureReplaceFile(tokenPath(tokensDir, teamID, userID), data, 0o600); err != nil {
		return fmt.Errorf("write token file: %w", err)
	}
	return nil
}

// LoadToken loads an OAuth user token. Browser callers must use LoadCredentials
// so they do not lose the accompanying cookie.
func LoadToken(tokensDir, teamID, userID string) (string, error) {
	credentials, err := LoadCredentials(tokensDir, teamID, userID)
	if err != nil {
		return "", err
	}
	if credentials.Cookie != "" {
		return "", errors.New("slack browser session requires LoadCredentials")
	}
	return credentials.Token, nil
}

// LoadCredentials loads stored credentials and checks their workspace and user.
// Existing OAuth token files, which have no cookie field, remain compatible.
func LoadCredentials(tokensDir, teamID, userID string) (Credentials, error) {
	data, err := os.ReadFile(tokenPath(tokensDir, teamID, userID))
	if err != nil {
		if os.IsNotExist(err) {
			return Credentials{}, fmt.Errorf("no Slack token for %s in workspace %s (run 'add-slack' first)", userID, teamID)
		}
		return Credentials{}, fmt.Errorf("read slack token: %w", err)
	}
	var tf tokenFile
	if err := json.Unmarshal(data, &tf); err != nil {
		return Credentials{}, errors.New("parse slack token: invalid credential JSON")
	}
	if (tf.TeamID != "" && tf.TeamID != teamID) || (tf.UserID != "" && tf.UserID != userID) {
		return Credentials{}, fmt.Errorf("slack token file for %s/%s holds identity %s/%s — re-run 'add-slack'",
			teamID, userID, tf.TeamID, tf.UserID)
	}
	credentials := Credentials{Token: tf.AccessToken, Cookie: tf.Cookie}
	if err := credentials.Validate(); err != nil {
		return Credentials{}, err
	}
	return credentials, nil
}

// LoadClient restores the complete credential pair for every live sync path.
func LoadClient(baseURL, tokensDir, teamID, userID string) (*Client, error) {
	credentials, err := LoadCredentials(tokensDir, teamID, userID)
	if err != nil {
		return nil, err
	}
	return NewClientWithCredentials(baseURL, credentials)
}

// DeleteToken removes a workspace user's token file. Missing file is not an error.
func DeleteToken(tokensDir, teamID, userID string) error {
	err := os.Remove(tokenPath(tokensDir, teamID, userID))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
