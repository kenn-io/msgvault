package cmd

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestSyncFullGmailSkipsSpamTrashBeforeAccountRegistration(t *testing.T) {
	resetSyncFullFlagsForTest(t)
	const email = "user@example.com"
	_, state := setupCommandOAuth(t)
	state.cfg.Analytics.AutoBuildCache = false
	state.cfg.Accounts = []config.AccountSchedule{
		{Email: "User@Example.com", GmailSkipSpamTrash: true},
	}
	putCommandToken(t, state, email, []string{oauth.ScopeGmailReadonly})

	// Only Gmail HTTP responses are simulated. The command starts with a
	// token and no registered source in its disposable archive.
	savedTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = savedTransport })
	var includeSpamTrash []string
	http.DefaultTransport = testTransport(func(req *http.Request) (*http.Response, error) {
		var body string
		switch req.Method + " " + req.URL.Host + req.URL.Path {
		case "GET gmail.googleapis.com/gmail/v1/users/me/profile":
			body = `{"emailAddress":"user@example.com","historyId":"100"}`
		case "GET gmail.googleapis.com/gmail/v1/users/me/labels":
			body = `{"labels":[]}`
		case "GET gmail.googleapis.com/gmail/v1/users/me/messages":
			includeSpamTrash = append(includeSpamTrash, req.URL.Query().Get("includeSpamTrash"))
			body = `{"messages":[]}`
		default:
			return nil, fmt.Errorf("unexpected request: %s %s", req.Method, req.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})

	cmd := &cobra.Command{Use: "sync-full [account]", RunE: runSyncFullLocalForTest}
	cmd.SetArgs([]string{email})
	require.NoError(t, cmd.ExecuteContext(withInvocation(t.Context(), state)))
	assert.Equal(t, []string{"false"}, includeSpamTrash)

	s, err := store.Open(state.cfg.DatabaseDSN())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	source, err := s.GetSourceByTypeAndIdentifier(t.Context(), sourceTypeGmail, email)
	require.NoError(t, err)
	assert.Equal(t, "100", source.SyncCursor.String)
}

func TestGmailSkipsSpamTrashResolvesEntriesLikeSchedules(t *testing.T) {
	dir := t.TempDir()
	s := testutil.NewSQLiteTestStoreAt(t, filepath.Join(dir, "msgvault.db"))
	src, err := s.GetOrCreateSource(sourceTypeGmail, "user@example.com")
	require.NoError(t, err)
	require.NoError(t, s.UpdateSourceDisplayName(src.ID, "Work Mail"))
	_, err = s.GetOrCreateSource(sourceTypeGmail, "other@example.com")
	require.NoError(t, err)

	tests := []struct {
		name    string
		entries string
		want    bool
	}{
		{"identifier in another case", `email = "User@Example.com"` + "\ngmail_skip_spam_trash = true", true},
		{"display name", `email = "work mail"` + "\ngmail_skip_spam_trash = true", true},
		{"another account", `email = "other@example.com"` + "\ngmail_skip_spam_trash = true", false},
		{"setting off", `email = "user@example.com"` + "\nschedule = \"0 2 * * *\"", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configDir := t.TempDir()
			configPath := filepath.Join(configDir, "config.toml")
			require.NoError(t, os.WriteFile(configPath, []byte("[[accounts]]\n"+tt.entries+"\n"), 0o600))
			cfg, err := config.Load(configPath, configDir)
			require.NoError(t, err)

			got, err := gmailSkipsSpamTrash(t.Context(), cfg, s, src)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
