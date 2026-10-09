package carddav

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/oauth"
)

func TestGoogleAuthorizationReusesOnlyMatchingCredentials(t *testing.T) {
	for _, tc := range []struct {
		name, mailClient, mailEmail string
		dedicatedClient             string
		malformedDedicated          bool
		wantScope                   string
	}{
		{name: "matching mail authorization", mailClient: "contacts-client", mailEmail: "person@example.com", wantScope: oauth.ScopeGmailReadonly},
		{name: "different mail client", mailClient: "mail-client", mailEmail: "person@example.com"},
		{name: "unknown mail client", mailEmail: "person@example.com"},
		{name: "different account", mailClient: "contacts-client", mailEmail: "other@example.com"},
		{name: "existing separate authorization takes precedence", mailClient: "contacts-client", mailEmail: "person@example.com", dedicatedClient: "contacts-client", wantScope: oauth.ScopeCalendarReadonly},
		{name: "malformed separate authorization falls back to matching mail", mailClient: "contacts-client", mailEmail: "person@example.com", malformedDedicated: true, wantScope: oauth.ScopeGmailReadonly},
		{name: "malformed separate authorization without mail can reauthorize", malformedDedicated: true},
		{name: "rotated separate client can reauthorize", mailClient: "contacts-client", mailEmail: "person@example.com", dedicatedClient: "previous-client", wantScope: oauth.ScopeCalendarReadonly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			required := require.New(t)
			dir := t.TempDir()
			secrets := filepath.Join(dir, "client.json")
			required.NoError(os.WriteFile(secrets, []byte(`{"web":{"client_id":"contacts-client","client_secret":"synthetic-secret","auth_uri":"https://accounts.example/authorize","token_uri":"https://accounts.example/token","redirect_uris":["https://archive.example/"]}}`), 0600))
			mail, err := json.Marshal(map[string]any{"access_token": "mail-access", "client_id": tc.mailClient, "scopes": []string{oauth.ScopeGmailReadonly}})
			required.NoError(err)
			mailPath := filepath.Join(dir, tc.mailEmail+".json")
			if tc.mailEmail != "" {
				required.NoError(os.WriteFile(mailPath, mail, 0600))
			}
			if tc.malformedDedicated {
				isolated := filepath.Join(dir, filepath.FromSlash(googleTokenNamespace("contacts")))
				required.NoError(os.MkdirAll(isolated, 0700))
				required.NoError(os.WriteFile(filepath.Join(isolated, "person@example.com.json"), []byte("{"), 0600))
			}
			if tc.dedicatedClient != "" {
				isolated := filepath.Join(dir, filepath.FromSlash(googleTokenNamespace("contacts")))
				required.NoError(os.MkdirAll(isolated, 0700))
				data, err := json.Marshal(map[string]any{"access_token": "contacts-access", "client_id": tc.dedicatedClient, "scopes": []string{oauth.ScopeCalendarReadonly}})
				required.NoError(err)
				required.NoError(os.WriteFile(filepath.Join(isolated, "person@example.com.json"), data, 0600))
			}
			mgr, err := NewGoogleOAuthManagerWithCredentials(t.Context(), config.OAuthApp{ClientSecrets: secrets}, dir, config.OAuthTokenCommands{}, "contacts", "person@example.com", nil)
			required.NoError(err)
			flow, err := mgr.BeginWebAuthorization(t.Context(), "person@example.com", "https://archive.example/")
			required.NoError(err)
			parsed, err := url.Parse(flow.URL)
			required.NoError(err)
			want := []string{oauth.ScopeCardDAV, oauth.ScopeUserinfoEmail}
			if tc.wantScope != "" {
				want = append(want, tc.wantScope)
			}
			assertions.ElementsMatch(want, strings.Fields(parsed.Query().Get("scope")))
			if tc.mailEmail != "" {
				unchanged, err := os.ReadFile(mailPath)
				required.NoError(err)
				assertions.Equal(mail, unchanged)
			}
		})
	}
}
