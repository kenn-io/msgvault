package cmd

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/fileutil"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/mcpevents"
)

func TestDaemonMCPEventsRequireAuthorizedRuntimeCatalog(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"enabled", 200, `{"events":[{"name":"msgvault.message_archived","delivery":["webhook"]}]}`, true},
		{"disabled", 200, `{"events":[]}`, false},
		{"old daemon", 404, `{}`, false},
		{"owner rejected", 403, `{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			ctx := withStoreResolverConfig(t, &config.Config{Server: config.ServerConfig{APIKey: "key"}})
			c := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/health":
					_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"3.5.0","mcp_events":true}`))
				case "/api/v1/mcp/events/list":
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				default:
					http.NotFound(w, r)
				}
			})
			opts := daemonMCPServeOptions(ctx, c, invocationFromContext(ctx))
			assert.Equal(tc.want, opts.Events != nil)
		})
	}
}
func TestDaemonMCPEventsDisabledHealthSkipsCatalogProbe(t *testing.T) {
	for _, tc := range []struct {
		name   string
		health string
	}{
		{name: "API 3.4 with Events flag is denied", health: `{"status":"ok","api_schema_version":"3.4.0","mcp_events":true}`},
		{name: "schema without Events field", health: `{"status":"ok","api_schema_version":"3.4.0"}`},
		{name: "Events disabled", health: `{"status":"ok","api_schema_version":"3.4.0","mcp_events":false}`},
		{name: "older schema 3.3", health: `{"status":"ok","api_schema_version":"3.3.0","mcp_events":true}`},
		{name: "older schema 3.2", health: `{"status":"ok","api_schema_version":"3.2.0","mcp_events":true}`},
		{name: "older schema 3.1", health: `{"status":"ok","api_schema_version":"3.1.0","mcp_events":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			ctx := withStoreResolverConfig(t, &config.Config{Server: config.ServerConfig{APIKey: "key"}})
			var catalogRequests atomic.Int32
			client := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/health" {
					_, _ = w.Write([]byte(tc.health))
					return
				}
				if r.URL.Path == "/api/v1/mcp/events/list" {
					catalogRequests.Add(1)
					_, _ = w.Write([]byte(`{"events":[{"name":"msgvault.message_archived"}]}`))
					return
				}
				http.NotFound(w, r)
			})
			opts := daemonMCPServeOptions(ctx, client, invocationFromContext(ctx))
			assert.Nil(opts.Events)
			assert.Zero(catalogRequests.Load())
		})
	}
}

func TestMCPEventsStatusOnlyPrintsSafeFields(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var out bytes.Buffer
	require.NoError(writeMCPEventsStatus(&out, []mcpevents.SubscriptionStatus{{ID: "sub_fixture", Name: "msgvault.message_archived", ScopeKind: "conversation", ScopeID: "42", State: "active", PendingAttempt: 2, DeadLetterCount: 3, LoopGuardSkips: 4}}, true))
	assert.Contains(out.String(), "sub_fixture")
	assert.NotContains(out.String(), "secret")
	assert.NotContains(out.String(), "callback")
	out.Reset()
	require.NoError(writeMCPEventsStatus(&out, nil, false))
	assert.Contains(out.String(), "No MCP Events subscriptions")
}

func TestMCPIndependentCredentialAlwaysSuppressesEvents(t *testing.T) {
	for _, flagName := range []string{"http-token-env", "http-token-file"} {
		t.Run(flagName, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/health":
					_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"3.5.0","mcp_events":true}`))
				case "/api/v1/mcp/events/list":
					_, _ = w.Write([]byte(`{"events":[{"name":"msgvault.message_archived"}]}`))
				default:
					http.NotFound(w, r)
				}
			}))
			defer daemon.Close()
			cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "same-fixture-key"}, Remote: config.RemoteConfig{URL: daemon.URL, APIKey: "same-fixture-key", AllowInsecure: true}}
			ctx := withStoreResolverConfig(t, cfg)
			prevAddr, prevServe, prevCtx := mcpHTTPAddr, serveMCPHTTPWithOptions, mcpCmd.Context()
			fileFlag, envFlag := mcpCmd.Flags().Lookup("http-token-file"), mcpCmd.Flags().Lookup("http-token-env")
			fileValue, envValue, fileChanged, envChanged := fileFlag.Value.String(), envFlag.Value.String(), fileFlag.Changed, envFlag.Changed
			t.Cleanup(func() {
				mcpHTTPAddr = prevAddr
				serveMCPHTTPWithOptions = prevServe
				mcpCmd.SetContext(prevCtx)
				_ = fileFlag.Value.Set(fileValue)
				_ = envFlag.Value.Set(envValue)
				fileFlag.Changed = fileChanged
				envFlag.Changed = envChanged
			})
			fileFlag.Changed = false
			envFlag.Changed = false
			if flagName == "http-token-env" {
				t.Setenv("MSGVAULT_TEST_MCP_KEY", "same-fixture-key")
				require.NoError(mcpCmd.Flags().Set(flagName, "MSGVAULT_TEST_MCP_KEY"))
			} else {
				path := filepath.Join(t.TempDir(), "key")
				require.NoError(fileutil.SecureWriteFile(path, []byte("same-fixture-key"), 0600))
				require.NoError(mcpCmd.Flags().Set(flagName, path))
			}
			mcpHTTPAddr = "127.0.0.1:9876"
			mcpCmd.SetContext(ctx)
			called := false
			serveMCPHTTPWithOptions = func(_ context.Context, opts mcpserver.ServeOptions, httpOpts mcpserver.HTTPOptions) error {
				called = true
				assert.Nil(opts.Events)
				assert.True(httpOpts.IndependentCredential)
				assert.Equal("same-fixture-key", httpOpts.APIKey)
				return nil
			}
			require.NoError(mcpCmd.RunE(mcpCmd, nil))
			assert.True(called)
		})
	}
}

func TestMCPDefaultRemoteKeyIsCheckedAfterDaemonResolution(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, name := range []string{"MSGVAULT_API_KEY", "MSGVAULT_API_KEY_FILE", "MSGVAULT_API_KEY_ENV"} {
		t.Setenv(name, "")
		require.NoError(os.Unsetenv(name))
	}
	savedAddr, savedInsecure := mcpHTTPAddr, mcpHTTPAllowInsecure
	t.Cleanup(func() { mcpHTTPAddr = savedAddr; mcpHTTPAllowInsecure = savedInsecure })
	mcpHTTPAddr = "0.0.0.0:9876"
	mcpHTTPAllowInsecure = false
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Remote: config.RemoteConfig{URL: "https://daemon.example", APIKey: "fixture-owner"}}
	ctx := withStoreResolverConfig(t, cfg)
	command := &cobra.Command{}
	command.SetContext(ctx)
	command.Flags().String("http-token-file", "", "")
	command.Flags().String("http-token-env", "", "")
	command.Flags().String("http", "", "")
	address, _, err := prepareMCPHTTP(command, cfg)
	require.NoError(err)
	assert.Equal("0.0.0.0:9876", address)
}

func TestMCPRemoteOwnerKeyDoesNotReadUnusedServerCredential(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, name := range []string{"MSGVAULT_API_KEY", "MSGVAULT_API_KEY_FILE", "MSGVAULT_API_KEY_ENV"} {
		t.Setenv(name, "")
		require.NoError(os.Unsetenv(name))
	}
	savedAddr, savedInsecure := mcpHTTPAddr, mcpHTTPAllowInsecure
	t.Cleanup(func() { mcpHTTPAddr = savedAddr; mcpHTTPAllowInsecure = savedInsecure })
	mcpHTTPAddr = "0.0.0.0:9876"
	mcpHTTPAllowInsecure = false
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKeyFile: filepath.Join(t.TempDir(), "unused-missing.key")}, Remote: config.RemoteConfig{URL: "https://daemon.example", APIKey: "fixture-owner"}}
	ctx := withStoreResolverConfig(t, cfg)
	command := &cobra.Command{}
	command.SetContext(ctx)
	command.Flags().String("http-token-file", "", "")
	command.Flags().String("http-token-env", "", "")
	command.Flags().String("http", "", "")
	address, _, err := prepareMCPHTTP(command, cfg)
	require.NoError(err)
	assert.Equal("0.0.0.0:9876", address)
}
