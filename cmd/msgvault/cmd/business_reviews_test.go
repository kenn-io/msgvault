package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/businessreviews"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/oauth"
)

func TestBusinessReviewsCLIExactReplyAndSeparateConfirm(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var got businessreviews.Request
	run := func(_ context.Context, r businessreviews.Request) (*businessreviews.Result, error) {
		got = r
		return &businessreviews.Result{}, nil
	}
	cmd := newBusinessReviewsCmd(run)
	cmd.SetIn(strings.NewReader("Thank you é\n"))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{"preview-reply", "shop", "review"})
	require.NoError(cmd.ExecuteContext(t.Context()))
	assert.Equal(businessreviews.Request{Action: "preview_reply", Target: "shop", ReviewID: "review", Comment: "Thank you é\n"}, got)
	cmd = newBusinessReviewsCmd(run)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs([]string{"confirm-reply", "--confirmation-token", "issued", "--confirmed"})
	require.NoError(cmd.ExecuteContext(t.Context()))
	assert.Equal(businessreviews.Request{Action: "confirm_reply", ConfirmationToken: "issued", Confirmed: true}, got)
}
func TestBusinessReviewsCLIRejectsBeforeRunner(t *testing.T) {
	for _, args := range [][]string{{"confirm-reply", "--confirmation-token", "issued"}, {"list", "shop", "--page-size", "51"}, {"get", "shop", "../r"}, {"preview-reply", "shop", "r", "--comment", "unsafe argv"}} {
		calls := 0
		cmd := newBusinessReviewsCmd(func(context.Context, businessreviews.Request) (*businessreviews.Result, error) {
			calls++
			return nil, nil //nolint:nilnil // Rejected input must never reach this runner.
		})
		cmd.SetIn(strings.NewReader("Reply"))
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		require.Error(t, cmd.ExecuteContext(t.Context()))
		assert.Zero(t, calls)
	}
	cmd := newBusinessReviewsCmd(func(context.Context, businessreviews.Request) (*businessreviews.Result, error) {
		require.Fail(t, "invalid input reached runner")
		return nil, nil //nolint:nilnil // FailNow above makes this placeholder unreachable.
	})
	cmd.SetIn(strings.NewReader(strings.Repeat("é", 2049)))
	cmd.SetArgs([]string{"preview-reply", "shop", "r"})
	require.Error(t, cmd.ExecuteContext(t.Context()))
}
func TestBusinessReviewArgsRedacted(t *testing.T) {
	got := strings.Join(sanitizeArgs([]string{"business-reviews", "preview-reply", "CANARY_TARGET", "CANARY_REVIEW", "--page-token=CANARY_PAGE", "--confirmation-token", "CANARY_TOKEN"}), " ")
	assert.NotContains(t, got, "CANARY")
	assert.Contains(t, got, "business-reviews")
	assert.Contains(t, got, "preview-reply")
}

func TestBusinessReviewsStartupLogRedactsLeafPositionals(t *testing.T) {
	previousLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	for i, args := range [][]string{
		{"get", "canary-target", "canary-review-id"},
		{"preview-reply", "canary-target", "canary-review-id"},
		{"receipt", "canary-receipt-id"},
		{"get", "canary-target", "canary-invalid/review"},
	} {
		t.Run(fmt.Sprintf("%s-%d", args[0], i), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			home := t.TempDir()
			logPath := filepath.Join(home, "startup.log")
			root := newRootCommand()
			root.AddCommand(newBusinessReviewsCmd(func(context.Context, businessreviews.Request) (*businessreviews.Result, error) {
				return &businessreviews.Result{}, nil
			}))
			root.SetIn(strings.NewReader("Exact reply"))
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			root.SetArgs(append([]string{"--home", home, "--log-file", logPath, "--log-level", "debug", "business-reviews"}, args...))
			err := executeRootContext(t.Context(), root)
			if strings.Contains(args[len(args)-1], "/") {
				require.ErrorIs(err, businessreviews.ErrInvalid)
			} else {
				require.NoError(err)
			}
			data, err := os.ReadFile(logPath)
			require.NoError(err)
			assert.Contains(string(data), "msgvault startup args", "the real root debug callback must run")
			assert.Contains(string(data), "<redacted>")
			assert.NotContains(string(data), "canary-", "provider identifiers must not reach the owned log")
		})
	}
}

func TestBusinessReviewsDisconnectWithoutClientCredentials(t *testing.T) {
	previousLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	for _, source := range []string{"missing", "malformed", "command"} {
		t.Run(source, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			home := t.TempDir()
			secrets := filepath.Join(home, "client.json")
			credentialConfig := fmt.Sprintf("client_secrets = %q", secrets)
			switch source {
			case "malformed":
				require.NoError(os.WriteFile(secrets, []byte("invalid JSON"), 0600))
			case "command":
				credentialConfig = fmt.Sprintf("client_secrets_command = [%q]", filepath.Join(home, "missing-command"))
			}
			path := filepath.Join(home, "config.toml")
			content := fmt.Sprintf(`[data]
data_dir = %q
[server]
api_key = "synthetic-owner-key"
daemon_auto_start = false
[oauth.apps.business]
%s
[[business_reviews]]
name = "shop"
provider = "google"
email = "owner@example.com"
oauth_app = "business"
location = "accounts/a/locations/l"
enabled = false
own_project = true
`, home, credentialConfig)
			require.NoError(os.WriteFile(path, []byte(content), 0600))
			cfg, err := config.Load(path, home)
			require.NoError(err)
			manager := oauth.NewStoredTokenManager(cfg.TokensDir(), cfg.OAuth.Tokens).WithTokenNamespace(businessReviewNamespace("business"))
			tokenPath := manager.TokenPath("owner@example.com")
			require.NoError(os.MkdirAll(filepath.Dir(tokenPath), 0700))
			require.NoError(os.WriteFile(tokenPath, []byte(`{"access_token":"synthetic"}`), 0600))
			otherPath := oauth.NewStoredTokenManager(cfg.TokensDir(), cfg.OAuth.Tokens).TokenPath("owner@example.com")
			require.NoError(os.WriteFile(otherPath, []byte(`{"access_token":"unrelated"}`), 0600))
			var invalidations atomic.Int32
			mux := http.NewServeMux()
			mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{Service: daemonService, Version: Version}))
			mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, r *http.Request) {
				assert.Equal("synthetic-owner-key", r.Header.Get("X-Api-Key"))
				w.WriteHeader(http.StatusOK)
			})
			mux.HandleFunc("/api/v1/business-reviews/control", func(w http.ResponseWriter, r *http.Request) {
				assert.Equal("synthetic-owner-key", r.Header.Get("X-Api-Key"))
				assert.FileExists(tokenPath, "daemon invalidation must precede credential deletion")
				var request businessreviews.Request
				if !assert.NoError(json.NewDecoder(r.Body).Decode(&request)) {
					http.Error(w, "invalid body", http.StatusBadRequest)
					return
				}
				assert.Equal(businessreviews.Request{Action: "invalidate_target", Target: "shop"}, request)
				invalidations.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{}`)
			})
			server := httptest.NewServer(mux)
			defer server.Close()
			host, portText, err := net.SplitHostPort(server.Listener.Addr().String())
			require.NoError(err)
			port, err := strconv.Atoi(portText)
			require.NoError(err)
			_, _, err = writeDaemonRuntime(home, host, port, Version, "synthetic-owner-key")
			require.NoError(err)
			root := newRootCommand()
			root.AddCommand(newBusinessReviewsCmd(nil))
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			root.SetArgs([]string{"--home", home, "--config", path, "--no-log-file", "business-reviews", "disconnect", "shop"})
			assert.NoError(executeRootContext(t.Context(), root)) //nolint:testifylint // Keep the independent cleanup checks running if the command fails.
			assert.Equal(int32(1), invalidations.Load())
			assert.NoFileExists(tokenPath)
			assert.FileExists(otherPath, "unrelated OAuth tokens remain")
		})
	}
}

func TestBusinessReviewsSetupFailureStagesAreRedacted(t *testing.T) {
	previousLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	const canary = "private-canary"
	for _, tc := range []struct{ name, action, stage string }{
		{"remove", "disconnect", "remove stored business-review token"},
		{"load", "authorize", "load configured business-review OAuth app credentials"},
		{"authorize", "authorize", "authorize business-review app"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			home := t.TempDir()
			secrets := filepath.Join(home, canary+"-client.json")
			clientJSON := `{"installed":{"client_id":"synthetic-client","client_secret":"private-canary","redirect_uris":["http://localhost"],"auth_uri":"https://oauth.example.invalid/auth","token_uri":"https://oauth.example.invalid/token"}}`
			if tc.name == "load" {
				clientJSON = "invalid JSON " + canary
			}
			require.NoError(os.WriteFile(secrets, []byte(clientJSON), 0600))
			configPath := filepath.Join(home, "config.toml")
			content := fmt.Sprintf(`[data]
data_dir = %q
[server]
api_key = "synthetic-owner-key"
daemon_auto_start = false
[oauth.apps.business]
client_secrets = %q
[[business_reviews]]
name = "shop"
provider = "google"
email = "private-canary@example.com"
oauth_app = "business"
location = "accounts/a/locations/l"
enabled = false
own_project = true
`, home, secrets)
			require.NoError(os.WriteFile(configPath, []byte(content), 0600))
			cfg, err := config.Load(configPath, home)
			require.NoError(err)
			manager := oauth.NewStoredTokenManager(cfg.TokensDir(), cfg.OAuth.Tokens).WithTokenNamespace(businessReviewNamespace("business"))
			tokenPath := manager.TokenPath("private-canary@example.com")
			// A nonempty directory makes both file deletion and authorization's
			// initial token read fail locally, before any OAuth browser/network flow.
			require.NoError(os.MkdirAll(tokenPath, 0700))
			require.NoError(os.WriteFile(filepath.Join(tokenPath, "entry"), []byte(canary), 0600))
			var invalidations atomic.Int32
			mux := http.NewServeMux()
			mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{Service: daemonService, Version: Version}))
			mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
			mux.HandleFunc("/api/v1/business-reviews/control", func(w http.ResponseWriter, r *http.Request) {
				assert.Equal("synthetic-owner-key", r.Header.Get("X-Api-Key"))
				var request businessreviews.Request
				if !assert.NoError(json.NewDecoder(r.Body).Decode(&request)) {
					http.Error(w, "invalid body", http.StatusBadRequest)
					return
				}
				assert.Equal(businessreviews.Request{Action: "invalidate_target", Target: "shop"}, request)
				invalidations.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{}`)
			})
			server := httptest.NewServer(mux)
			defer server.Close()
			host, portText, err := net.SplitHostPort(server.Listener.Addr().String())
			require.NoError(err)
			port, err := strconv.Atoi(portText)
			require.NoError(err)
			_, _, err = writeDaemonRuntime(home, host, port, Version, "synthetic-owner-key")
			require.NoError(err)
			logPath := filepath.Join(home, "setup.log")
			root := newRootCommand()
			root.AddCommand(newBusinessReviewsCmd(nil))
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			args := []string{"--home", home, "--config", configPath, "--log-file", logPath, "--log-level", "debug", "business-reviews", tc.action, "shop"}
			if tc.action == "authorize" {
				args = append(args, "--headless")
			}
			root.SetArgs(args)
			err = executeRootContext(t.Context(), root)
			require.ErrorIs(err, businessreviews.ErrUnavailable)
			assert.Contains(err.Error(), tc.stage)
			assert.NotContains(err.Error(), canary)
			assert.Equal(int32(1), invalidations.Load())
			logs, err := os.ReadFile(logPath)
			require.NoError(err)
			assert.Contains(string(logs), "error", "the real root exit logger must record the setup failure")
			assert.Contains(string(logs), tc.stage)
			assert.NotContains(string(logs), canary)
		})
	}
}
