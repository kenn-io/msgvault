package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
	"golang.org/x/oauth2"
)

func TestAddAccountRejectsInvalidAddress(t *testing.T) {
	saveAddAccountFlags(t)
	for _, email := range []string{"not-an-email", "", "User <user@example.com>", "user@example.com,other@example.com", " user@example.com", "user@example.com\n"} {
		t.Run(email, func(t *testing.T) {
			cmd := newAddAccountCmd()
			err := cmd.ValidateArgs([]string{email})
			require.Error(t, err)
			assert.ErrorContains(t, err, "email")
		})
	}
}

func TestAddAccountAcceptsEmailAddress(t *testing.T) {
	saveAddAccountFlags(t)
	for _, email := range []string{"user@example.com", "User.Name+archive@gmail.com", "user@googlemail.com"} {
		t.Run(email, func(t *testing.T) {
			cmd := newAddAccountCmd()
			require.NoError(t, cmd.ValidateArgs([]string{email}))
		})
	}
}

type gmailProfileTransport struct {
	email      string
	statusCode int
}

func (tr gmailProfileTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.String() != "https://gmail.googleapis.com/gmail/v1/users/me/profile" {
		return nil, fmt.Errorf("unexpected OAuth request: %s", req.URL)
	}
	body, err := json.Marshal(map[string]string{"emailAddress": tr.email})
	if err != nil {
		return nil, fmt.Errorf("encode Gmail profile fixture: %w", err)
	}
	return &http.Response{
		StatusCode: tr.statusCode,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    req,
	}, nil
}

// gmailProfileContext supplies only the external Gmail profile response.
// Account registration, token loading, and all database writes remain real.
// The in-memory transport accepts a cancelled context so an unexpected fallthrough
// to browser authorization fails before opening a browser or callback listener.
func gmailProfileContext(t *testing.T, email string) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	return context.WithValue(ctx, oauth2.HTTPClient, &http.Client{
		Transport: gmailProfileTransport{email: email, statusCode: http.StatusOK},
	})
}

// A cached token must prove mailbox ownership before add-account creates a
// source or changes its display name, even if its OAuth client is correct.
func TestAddAccountCachedTokenIdentity(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, legacy := range []bool{false, true} {
			for _, profile := range []string{"user@example.com", "other@example.com", "unavailable"} {
				t.Run(fmt.Sprintf("existing=%t/legacy=%t/profile=%s", existing, legacy, profile), func(t *testing.T) {
					assert, require := assert.New(t), require.New(t)
					saveAddAccountFlags(t)
					home := t.TempDir()
					secrets := filepath.Join(home, "client.json")
					require.NoError(os.WriteFile(secrets, []byte(fakeClientSecrets), 0600))
					savedCfg, savedLogger := cfg, logger
					t.Cleanup(func() { cfg, logger = savedCfg, savedLogger })
					cfg = &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}, OAuth: config.OAuthConfig{ClientSecrets: secrets}}
					logger = slog.New(slog.DiscardHandler)
					require.NoError(os.MkdirAll(cfg.TokensDir(), 0700))
					tokenFields := map[string]any{"access_token": "synthetic-token", "token_type": "Bearer"}
					if !legacy {
						tokenFields["client_id"] = "test.apps.googleusercontent.com"
						tokenFields["scopes"] = oauth.Scopes
					}
					token, err := json.Marshal(tokenFields)
					require.NoError(err)
					tokenPath := oauth.TokenFilePath(cfg.TokensDir(), "user@example.com")
					require.NoError(os.WriteFile(tokenPath, token, 0600))
					s, err := store.Open(cfg.DatabaseDSN())
					require.NoError(err)
					t.Cleanup(func() { _ = s.Close() })
					require.NoError(s.InitSchema())
					if existing {
						source, err := s.GetOrCreateSource("gmail", "user@example.com")
						require.NoError(err)
						require.NoError(s.UpdateSourceDisplayName(source.ID, "Original"))
					}
					ctx := gmailProfileContext(t, profile)
					if profile == "unavailable" {
						ctx = context.WithValue(ctx, oauth2.HTTPClient, &http.Client{
							Transport: gmailProfileTransport{statusCode: http.StatusServiceUnavailable},
						})
					}
					cmd := &cobra.Command{Use: addAccountUse, RunE: runAddAccountLocal}
					registerAddAccountFlags(cmd)
					cmd.SetArgs([]string{"user@example.com", "--display-name", "Updated", "--no-default-identity"})
					err = cmd.ExecuteContext(ctx)
					if profile == "user@example.com" {
						require.NoError(err)
						source, err := findGmailSource(s, "user@example.com")
						require.NoError(err)
						assert.Equal("Updated", source.DisplayName.String)
						return
					}
					require.Error(err)
					if profile == "other@example.com" {
						var mismatch *oauth.TokenMismatchError
						require.ErrorAs(err, &mismatch)
						if existing {
							assert.Contains(err.Error(), "https://msgvault.io/usage/multi-account/#recovering-an-older-mislabeled-gmail-account")
						} else {
							assert.Contains(err.Error(), "msgvault add-account other@example.com")
						}
					} else {
						assert.Contains(err.Error(), "HTTP 503")
					}
					source, lookupErr := findGmailSource(s, "user@example.com")
					if existing {
						require.NoError(lookupErr)
						assert.Equal("Original", source.DisplayName.String)
					} else {
						require.ErrorIs(lookupErr, errGmailSourceNotFound)
					}
					after, readErr := os.ReadFile(tokenPath)
					require.NoError(readErr)
					assert.Equal(token, after)
				})
			}
		}
	}
}
