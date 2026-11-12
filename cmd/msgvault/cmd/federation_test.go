package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/messageimport"
	"go.kenn.io/msgvault/internal/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFederationHubOrigin(t *testing.T) {
	for _, tc := range []struct {
		input, want string
		valid       bool
	}{
		{"https://hub.example.test/", "https://hub.example.test", true},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080", true},
		{"http://hub.example.test", "", false}, {"https://hub.example.test/path", "", false},
		{"https://hub.example.test/?query=1", "", false}, {"https://user@hub.example.test", "", false}, {"https://hub.example.test/#", "", false},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, err := federationHubOrigin(tc.input)
			if tc.valid {
				require.NoError(t, err)
				assert.Equal(t, tc.want, got)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

func TestFederationEnrollOutput(t *testing.T) {
	st := testutil.NewTestStore(t)
	serverCfg := config.NewDefaultConfig()
	serverCfg.Data.DataDir = t.TempDir()
	serverCfg.Federation.Role = "hub"
	serverCfg.Server.APIKey = "owner-fixture"
	apiServer := api.NewServerWithOptions(api.ServerOptions{Config: serverCfg, Store: st, FederationStore: st, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { require.NoError(t, apiServer.Shutdown(context.WithoutCancel(t.Context()))) })
	srv := httptest.NewServer(apiServer.Router())
	t.Cleanup(srv.Close)
	clientCfg := config.NewDefaultConfig()
	clientCfg.Data.DataDir = t.TempDir()
	clientCfg.Remote = config.RemoteConfig{URL: srv.URL, APIKey: "owner-fixture", AllowInsecure: true}
	ctx := withStoreResolverConfig(t, clientCfg)
	template, _, err := rootCmd.Find([]string{"federation", "enroll"})
	require.NoError(t, err)
	var stdout, stderr bytes.Buffer
	command := &cobra.Command{Use: template.Use, Args: template.Args, RunE: template.RunE}
	command.Flags().AddFlagSet(template.Flags())
	command.Flags().VisitAll(func(flag *pflag.Flag) { require.NoError(t, flag.Value.Set(flag.DefValue)); flag.Changed = false })
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	command.SetArgs([]string{"--spoke-instance", strings.Repeat("a", 64), "--member", "alice"})
	require.NoError(t, command.ExecuteContext(ctx))
	token := strings.TrimSpace(stdout.String())
	assert.True(t, strings.HasPrefix(token, "mvf1_"))
	assert.NotContains(t, stderr.String(), token)
	assert.Contains(t, stderr.String(), "Join (upcoming, not in this release): msgvault federation join <hub-url>")
	assert.NotContains(t, stderr.String(), srv.URL)

	listTemplate, _, err := rootCmd.Find([]string{"federation", "enrollments", "list"})
	require.NoError(t, err)
	list := &cobra.Command{Use: listTemplate.Use, Args: listTemplate.Args, RunE: listTemplate.RunE}
	list.Flags().AddFlagSet(listTemplate.Flags())
	var output bytes.Buffer
	list.SetOut(&output)
	require.NoError(t, list.ExecuteContext(ctx))
	assert.Contains(t, output.String(), "\talice\talice\tactive\t-\n")
	entries, err := st.ListFederationEnrollmentsContext(t.Context())
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.NoError(t, st.TouchFederationEnrollmentContext(t.Context(), entries[0].EnrollmentUID))
	output.Reset()
	require.NoError(t, list.ExecuteContext(ctx))
	assert.Contains(t, output.String(), "\talice\talice\tactive\t")
	assert.NotContains(t, output.String(), "\t-\n")
	stdout.Reset()
	stderr.Reset()
	command.SetArgs([]string{"--spoke-instance", strings.Repeat("a", 64), "--member", "alice", "--hub-url", "https://$HOME;echo.example.test"})
	require.NoError(t, command.ExecuteContext(ctx))
	assert.Regexp(t, `^mvf1_[A-Za-z0-9_-]+\n$`, stdout.String())
	assert.NotContains(t, stderr.String(), strings.TrimSpace(stdout.String()))
	assert.Contains(t, stderr.String(), "msgvault federation join 'https://$HOME;echo.example.test' --hub-instance ")
	stdout.Reset()
	stderr.Reset()
	command.SetArgs([]string{"--spoke-instance", strings.Repeat("a", 64), "--member", "alice", "--hub-url", "https://hub.example.test", "--json"})
	require.NoError(t, command.ExecuteContext(ctx))
	var jsonOutput struct {
		HubURL string `json:"hub_url"`
		Token  string `json:"token"`
	}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &jsonOutput))
	assert.Equal(t, "https://hub.example.test", jsonOutput.HubURL)
	assert.Regexp(t, `^mvf1_[A-Za-z0-9_-]+$`, jsonOutput.Token)
	assert.NotContains(t, stderr.String(), jsonOutput.Token)
	command.SetArgs([]string{"--spoke-instance", strings.Repeat("a", 64), "--member", "Alice"})
	err = command.ExecuteContext(ctx)
	assert.ErrorContains(t, err, "member must be 1–64 lowercase ASCII")
}

func TestFederationHubRejectsMessageImport(t *testing.T) {
	body := `{"source":{"identifier":"feeds","account_email":"user@example.com"},"message":{"external_id":"entry-42","message_type":"feed_item","conversation":{"external_id":"feed-7","title":"Example"},"sender":{"name":"Example"},"title":"Example","sent_at":"2026-11-07T10:00:00-05:00","body_markdown":"Example"}}`
	for _, tc := range []struct {
		role            string
		status, sources int
	}{{"off", http.StatusCreated, 1}, {"hub", http.StatusInternalServerError, 0}} {
		t.Run(tc.role, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			if tc.role == "hub" {
				require.NoError(t, st.ConfigureFederationHubContext(t.Context()))
			}
			cfg := config.NewDefaultConfig()
			cfg.Data.DataDir = t.TempDir()
			cfg.Federation.Role = tc.role
			cfg.Server.APIKey = "owner-fixture"
			logger := slog.New(slog.DiscardHandler)
			adapter := &storeAPIAdapter{store: st, messageImporter: messageimport.NewImporter(st, messageimport.Hooks{}).WithLogger(logger)}
			server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, FederationStore: st, Logger: logger})
			t.Cleanup(func() { require.NoError(t, server.Shutdown(context.WithoutCancel(t.Context()))) })
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/import/message", strings.NewReader(body))
			req.Header.Set("X-Api-Key", "owner-fixture")
			req.Header.Set("Content-Type", "application/json")
			resp := httptest.NewRecorder()
			server.Router().ServeHTTP(resp, req)
			assert.Equal(t, tc.status, resp.Code, resp.Body.String())
			sources, err := st.ListSourcesContext(t.Context(), "")
			require.NoError(t, err)
			assert.Len(t, sources, tc.sources)
		})
	}
}
