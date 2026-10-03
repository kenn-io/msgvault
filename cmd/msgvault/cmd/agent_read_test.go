package cmd

import (
	"database/sql"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestRemoteAgentReadCommands(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("test", "reader@example.test")
	requirements.NoError(err)
	conv, err := st.EnsureConversation(src.ID, "thread", "Synthetic")
	requirements.NoError(err)
	id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: "message-1", MessageType: "email", Subject: sql.NullString{String: "glacier", Valid: true}})
	requirements.NoError(err)
	requirements.NoError(st.UpsertMessageBody(id, sql.NullString{String: "synthetic body", Valid: true}, sql.NullString{}))
	requirements.NoError(st.UpsertFTS(id, "glacier", "synthetic body", src.Identifier, "", ""))
	cfg := &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}
	srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st}, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: slog.New(slog.DiscardHandler)})
	server := httptest.NewServer(srv.Router())
	t.Cleanup(server.Close)
	owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner", AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { _ = owner.Close() })
	grant, err := owner.IssueAgentToken(t.Context(), "reader", []string{"search.read", "message.read", "stats.read"}, []int64{src.ID}, nil)
	requirements.NoError(err)
	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	requirements.NoError(os.WriteFile(tokenFile, []byte(grant.Secret), 0600))
	for _, tc := range []struct {
		cmd  *cobra.Command
		args []string
		want string
	}{{searchCmd, []string{"glacier", "--json"}, "glacier"}, {showMessageCmd, []string{strconv.FormatInt(id, 10), "--json"}, "synthetic body"}, {statsCmd, nil, "Messages:"}} {
		t.Run(tc.cmd.Name(), func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			ctx := testInvocationContext(t.Context(), nil, invocationOptions{agentURL: server.URL, agentTokenFile: tokenFile, agentAllowInsecure: true, agentURLChanged: true, agentTokenChanged: true})
			pre := &cobra.Command{Use: tc.cmd.Use}
			pre.SetContext(ctx)
			requirements.NoError(rootCmd.PersistentPreRunE(pre, nil))
			done := captureStdout(t)
			output, err := runAgentTokenCommand(ctx, t, tc.cmd, tc.args...)
			output += done()
			requirements.NoError(err)
			assertions.Contains(output, tc.want)
		})
	}
	requirements.NoError(owner.RevokeAgentToken(t.Context(), grant.ID))
	agent, err := daemonclient.New(daemonclient.Config{URL: server.URL, AgentToken: grant.Secret, AllowInsecure: true})
	requirements.NoError(err)
	_, err = agent.GetCLIStats(t.Context(), "", "")
	requirements.Error(err)
}

func TestAgentTokenExpiryParser(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		want  time.Time
		bad   bool
	}{{"", time.Time{}, false}, {"24h", now.Add(24 * time.Hour), false}, {"2026-10-02T12:00:00Z", now.Add(24 * time.Hour), false}, {"-1h", time.Time{}, true}, {"2026-09-30T12:00:00Z", time.Time{}, true}, {"invalid", time.Time{}, true}} {
		got, err := parseAgentTokenExpiry(tc.value, now)
		if tc.bad {
			requirements.Error(err)
			continue
		}
		requirements.NoError(err)
		assertions.Equal(tc.want, got)
	}
}

func TestAgentTokenExpiryLabel(t *testing.T) {
	past := time.Unix(1, 0).UTC()
	future := time.Now().UTC().Add(time.Hour)
	for _, tc := range []struct {
		name    string
		expires *time.Time
		want    string
	}{
		{"no expiry", nil, "none"},
		{"active", &future, future.Format(time.RFC3339)},
		{"expired", &past, past.Format(time.RFC3339) + " (expired)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, agentExpiryLabel(tc.expires))
		})
	}
}
