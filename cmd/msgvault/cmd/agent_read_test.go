package cmd

import (
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestRemoteAgentReadCommands(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	st := testutil.NewTestStore(t)
	src, id, err := testutil.CreateIndexedSourceMessage(st, "reader@example.test", "message-1", "glacier", "synthetic body")
	requirements.NoError(err)
	cfg := &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}
	srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st}, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: slog.New(slog.DiscardHandler)})
	server := httptest.NewServer(srv.Router())
	t.Cleanup(server.Close)
	owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner", AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { _ = owner.Close() })
	grant, err := owner.IssueAgentToken(t.Context(), "reader", []string{"search.read", "message.read", "stats.read"}, []int64{src.ID}, nil, time.Time{})
	requirements.NoError(err)
	_, err = st.CreateCollection("selected", "", []int64{src.ID})
	requirements.NoError(err)
	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	requirements.NoError(os.WriteFile(tokenFile, []byte(grant.Secret), 0600))
	for _, tc := range []struct {
		cmd  *cobra.Command
		args []string
		want string
	}{{searchCmd, []string{"glacier", "--json"}, "glacier"}, {showMessageCmd, []string{strconv.FormatInt(id, 10), "--json"}, "synthetic body"}, {statsCmd, nil, "Messages:"}, {statsCmd, []string{"--account", src.Identifier}, "Stats for account"}, {statsCmd, []string{"--collection", "selected"}, "Stats for collection"}} {
		t.Run(tc.cmd.Name(), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			ctx := testInvocationContext(t.Context(), nil, invocationOptions{agentURL: server.URL, agentTokenFile: tokenFile, agentAllowInsecure: true, agentURLChanged: true, agentTokenChanged: true})
			pre := &cobra.Command{Use: tc.cmd.Use}
			root := &cobra.Command{Use: "msgvault"}
			root.SetContext(ctx)
			root.AddCommand(pre)
			requirements.NoError(rootCmd.PersistentPreRunE(pre, nil))
			done := captureStdout(t)
			var output string
			var err error
			stderr := captureStderrDuring(t, func() {
				output, err = runAgentTokenCommand(ctx, t, tc.cmd, tc.args...)
			})
			output += done()
			requirements.NoError(err)
			assertions.Contains(output, tc.want)
			if tc.cmd == statsCmd {
				assertions.NotContains(output, "Size:")
				assertions.NotContains(output, "Size is global")
			}
			if tc.cmd == searchCmd {
				assertions.Contains(stderr, "the search index is being checked")
				assertions.True(strings.HasPrefix(strings.TrimSpace(output), "["), "JSON remains an array")
			}
		})
	}
	ownerConfig := &config.Config{Remote: config.RemoteConfig{URL: server.URL, APIKey: "owner", AllowInsecure: true}}
	ownerContext := testInvocationContext(t.Context(), ownerConfig, invocationOptions{})
	for _, args := range [][]string{nil, {"--account", src.Identifier}, {"--collection", "selected"}} {
		output, err := runAgentTokenCommand(ownerContext, t, statsCmd, args...)
		requirements.NoError(err)
		assertions.Contains(output, "Size:")
		if len(args) > 0 {
			assertions.Contains(output, "Size is global")
		}
	}
}

func TestAgentTokenExpiryParser(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		want  time.Time
		bad   bool
	}{{"", time.Time{}, false}, {"24h", now.Add(24 * time.Hour), false}, {"2026-10-02T12:00:00Z", now.Add(24 * time.Hour), false}, {"-1h", time.Time{}, true}, {"0s", time.Time{}, true}, {"2026-09-30T12:00:00Z", time.Time{}, true}, {"2026-10-01T12:00:00Z", time.Time{}, true}, {"invalid", time.Time{}, true}, {"0001-01-01T00:00:00Z", time.Time{}, true}, {"0001-01-01T01:00:00+01:00", time.Time{}, true}} {
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
			assertions := assert.New(t)
			assertions.Equal(tc.want, agentExpiryLabel(tc.expires))
		})
	}
}
