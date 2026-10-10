package cmd

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
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
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestShowThreadDelegatedThroughRoot(t *testing.T) {
	requirements := require.New(t)
	previousCheck := remoteAPISchemaCheckEnabled
	remoteAPISchemaCheckEnabled = true
	t.Cleanup(func() { remoteAPISchemaCheckEnabled = previousCheck })
	st := testutil.NewTestStore(t)
	src, id, err := testutil.CreateIndexedSourceMessage(st, "reader@example.org", "message-1", "Plan", "Synthetic reply")
	requirements.NoError(err)
	other, _, err := testutil.CreateIndexedSourceMessage(st, "other@example.org", "message-2", "Other", "Outside the grant")
	requirements.NoError(err)
	cfg := &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}
	srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st}, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
	server := httptest.NewServer(srv.Router())
	t.Cleanup(server.Close)
	owner, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner", AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(owner.Close()) })
	for _, tc := range []struct {
		name, permission, wantError, wantCode string
		sourceID                              int64
		wantStatus                            int
		wantCause                             error
		oldSchema                             bool
	}{
		{name: "message read", permission: "message.read", sourceID: src.ID},
		{name: "missing permission", permission: "search.read", sourceID: src.ID, wantCode: "permission_denied", wantStatus: http.StatusForbidden},
		{name: "outside source grant", permission: "message.read", sourceID: other.ID, wantCause: store.ErrMessageNotFound},
		{name: "old schema", permission: "message.read", sourceID: src.ID, oldSchema: true, wantError: "agent archive reads require daemon API schema"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			grant, err := owner.IssueAgentToken(t.Context(), "reader", []string{tc.permission}, []int64{tc.sourceID}, nil, time.Time{})
			require.NoError(err)
			tokenFile := filepath.Join(t.TempDir(), "agent.token")
			require.NoError(os.WriteFile(tokenFile, []byte(grant.Secret), 0600))
			endpoint := server.URL
			if tc.oldSchema {
				old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/v1/health" {
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"3.8.0"}`))
						return
					}
					srv.Router().ServeHTTP(w, r)
				}))
				t.Cleanup(old.Close)
				endpoint = old.URL
			}
			root := newRootCommand()
			root.AddCommand(newShowThreadCmd())
			root.SetArgs([]string{"--agent-url", endpoint, "--agent-token-file", tokenFile, "--agent-allow-insecure", "show-thread", strconv.FormatInt(id, 10), "--json"})
			var stderr bytes.Buffer
			root.SetErr(&stderr)
			done := captureStdout(t)
			err = executeRootContext(t.Context(), root)
			output := done()
			if tc.wantCause != nil {
				require.ErrorIs(err, tc.wantCause)
				assert.NotContains(output, "Synthetic reply")
				return
			}
			if tc.wantCode != "" {
				var apiErr *daemonclient.APIError
				require.ErrorAs(err, &apiErr)
				assert.Equal(tc.wantCode, apiErr.Code)
				assert.Equal(tc.wantStatus, apiErr.Status)
				assert.NotContains(output, "Synthetic reply")
				return
			}
			if tc.wantError != "" {
				require.ErrorContains(err, tc.wantError)
				assert.NotContains(output, "Synthetic reply")
				return
			}
			require.NoError(err)
			assert.Contains(output, "Synthetic reply")
			assert.NotContains(output, "Outside the grant")
		})
	}
}

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
