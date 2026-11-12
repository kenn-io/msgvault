package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

type federationFailWriter struct{}

func (federationFailWriter) Write([]byte) (int, error) { return 0, errors.New("output failed") }

func TestFederationHubOrigin(t *testing.T) {
	for _, tc := range []struct {
		input, want string
		valid       bool
	}{
		{"https://hub.example.test/", "https://hub.example.test", true},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080", true},
		{"http://hub.example.test", "", false}, {"https://hub.example.test/path", "", false},
		{"https://hub.example.test:0", "", false}, {"https://hub.example.test:99999", "", false},
		{"https://hub.example.test/?query=1", "", false}, {"https://user@hub.example.test", "", false}, {"https://hub.example.test/#", "", false},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, err := federationHubOrigin(tc.input)
			if tc.valid {
				require.NoError(t, err)
				assert.Equal(t, tc.want, got)
			} else {
				assert.ErrorContains(t, err, "--hub-url")
			}
		})
	}
}

func TestFederationEnrollOutput(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	serverCfg := config.NewDefaultConfig()
	serverCfg.Data.DataDir = t.TempDir()
	serverCfg.Federation.Role = "hub"
	serverCfg.Server.APIKey = "owner-fixture"
	apiServer := api.NewServerWithOptions(api.ServerOptions{Config: serverCfg, Store: st, FederationStore: st, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { requirements.NoError(apiServer.Shutdown(context.WithoutCancel(t.Context()))) })
	srv := httptest.NewServer(apiServer.Router())
	t.Cleanup(srv.Close)
	clientCfg := config.NewDefaultConfig()
	clientCfg.Data.DataDir = t.TempDir()
	clientCfg.Remote = config.RemoteConfig{URL: srv.URL, APIKey: "owner-fixture", AllowInsecure: true}
	ctx := withStoreResolverConfig(t, clientCfg)
	template, _, err := rootCmd.Find([]string{"federation", "enroll"})
	requirements.NoError(err)
	var stdout, stderr bytes.Buffer
	command := &cobra.Command{Use: template.Use, Args: template.Args, RunE: template.RunE}
	silenceUsageInRunE(command)
	command.Flags().AddFlagSet(template.Flags())
	command.Flags().VisitAll(func(flag *pflag.Flag) { requirements.NoError(flag.Value.Set(flag.DefValue)); flag.Changed = false })
	t.Cleanup(func() {
		command.Flags().VisitAll(func(flag *pflag.Flag) { assert.NoError(t, flag.Value.Set(flag.DefValue)); flag.Changed = false })
	})
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	t.Run("invalid port does not enroll", func(t *testing.T) {
		for _, origin := range []string{"https://hub.example.test:0", "https://hub.example.test:99999"} {
			command.SetArgs([]string{"--spoke-instance", strings.Repeat("a", 64), "--member", "alice", "--hub-url", origin})
			require.ErrorContains(t, command.ExecuteContext(ctx), "--hub-url")
			entries, err := st.ListFederationEnrollmentsContext(t.Context())
			require.NoError(t, err)
			assert.Empty(t, entries)
			assert.Empty(t, stdout.String())
		}
		flag := command.Flags().Lookup("hub-url")
		require.NoError(t, flag.Value.Set(flag.DefValue))
		flag.Changed = false
	})
	t.Run("token and join hint", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		command.SetArgs([]string{"--spoke-instance", strings.Repeat("a", 64), "--member", "alice"})
		requirements.NoError(command.ExecuteContext(ctx))
		token := strings.TrimSpace(stdout.String())
		assertions.True(strings.HasPrefix(token, "mvf1_"))
		assertions.NotContains(stderr.String(), token)
		assertions.Contains(stderr.String(), "msgvault federation join <hub-url>")
		assertions.NotContains(stderr.String(), srv.URL)
	})

	t.Run("list", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		entries, err := st.ListFederationEnrollmentsContext(t.Context())
		requirements.NoError(err)
		if len(entries) == 0 {
			_, _, err = st.CreateFederationEnrollmentContext(t.Context(), strings.Repeat("a", 64), "alice", "")
			requirements.NoError(err)
		}
		listTemplate, _, err := rootCmd.Find([]string{"federation", "enrollments", "list"})
		requirements.NoError(err)
		list := &cobra.Command{Use: listTemplate.Use, Args: listTemplate.Args, RunE: listTemplate.RunE}
		list.Flags().AddFlagSet(listTemplate.Flags())
		var output bytes.Buffer
		list.SetOut(&output)
		requirements.NoError(list.ExecuteContext(ctx))
		assertions.Contains(output.String(), "\talice\talice\tactive\n")
		list.SetOut(federationFailWriter{})
		assertions.ErrorContains(list.ExecuteContext(ctx), "write federation enrollments: output failed")
	})
	t.Run("quoted join origin", func(t *testing.T) {
		stdout.Reset()
		stderr.Reset()
		command.SetArgs([]string{"--spoke-instance", strings.Repeat("a", 64), "--member", "alice", "--hub-url", "https://hub.example.test;false"})
		require.NoError(t, command.ExecuteContext(ctx))
		assert.Contains(t, stderr.String(), "msgvault federation join 'https://hub.example.test;false'")
	})
	t.Run("JSON output", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		stdout.Reset()
		stderr.Reset()
		command.SetArgs([]string{"--spoke-instance", strings.Repeat("a", 64), "--member", "alice", "--hub-url", "https://hub.example.test", "--json"})
		requirements.NoError(command.ExecuteContext(ctx))
		var jsonOutput struct {
			Token string `json:"token"`
		}
		requirements.NoError(json.Unmarshal(stdout.Bytes(), &jsonOutput))
		assertions.Regexp(`^mvf1_[A-Za-z0-9_-]+$`, jsonOutput.Token)
		assertions.NotContains(stderr.String(), jsonOutput.Token)
	})
	t.Run("server error", func(t *testing.T) {
		assertions := assert.New(t)
		command.SetArgs([]string{"--spoke-instance", strings.Repeat("a", 64), "--member", "Alice"})
		err := command.ExecuteContext(ctx)
		assertions.ErrorContains(err, "member")
	})
}

func TestFederationHubRejectsMessageImport(t *testing.T) {
	body := `{"source":{"identifier":"feeds","account_email":"user@example.com"},"message":{"external_id":"entry-42","message_type":"feed_item","conversation":{"external_id":"feed-7","title":"Example"},"sender":{"name":"Example"},"title":"Example","sent_at":"2026-11-07T10:00:00-05:00","body_markdown":"Example"}}`
	for _, tc := range []struct {
		role            string
		status, sources int
	}{{"off", http.StatusCreated, 1}, {"hub", http.StatusInternalServerError, 0}} {
		t.Run(tc.role, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st := testutil.NewTestStore(t)
			if tc.role == "hub" {
				requirements.NoError(st.ConfigureFederationHubContext(t.Context()))
			}
			cfg := config.NewDefaultConfig()
			cfg.Data.DataDir = t.TempDir()
			cfg.Federation.Role = tc.role
			cfg.Server.APIKey = "owner-fixture"
			logger := slog.New(slog.DiscardHandler)
			adapter := &storeAPIAdapter{store: st, messageImporter: messageimport.NewImporter(st, messageimport.Hooks{}).WithLogger(logger)}
			server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, FederationStore: st, Logger: logger})
			t.Cleanup(func() { requirements.NoError(server.Shutdown(context.WithoutCancel(t.Context()))) })
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/import/message", strings.NewReader(body))
			req.Header.Set("X-Api-Key", "owner-fixture")
			req.Header.Set("Content-Type", "application/json")
			resp := httptest.NewRecorder()
			server.Router().ServeHTTP(resp, req)
			assertions.Equal(tc.status, resp.Code, resp.Body.String())
			sources, err := st.ListSourcesContext(t.Context(), "")
			requirements.NoError(err)
			assertions.Len(sources, tc.sources)
		})
	}
}
