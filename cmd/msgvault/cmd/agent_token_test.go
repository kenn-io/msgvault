package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// runAgentTokenCommand runs a single agent-token subcommand with the supplied
// args and returns its combined stdout/stderr output.
func runAgentTokenCommand(
	t *testing.T, template *cobra.Command, args ...string,
) (string, error) {
	t.Helper()
	var output bytes.Buffer
	cmd := &cobra.Command{Use: template.Use, Args: template.Args, RunE: template.RunE}
	cmd.Flags().AddFlagSet(template.Flags())
	cmd.Flags().VisitAll(func(flag *pflag.Flag) {
		require.NoError(t, flag.Value.Set(flag.DefValue))
		flag.Changed = false
	})
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return output.String(), err
}

// agentTokenIssueResponseJSON returns a minimal JSON issue response for tests.
func agentTokenIssueResponseJSON(secret string) string {
	resp := agentTokenIssueFixture{
		ID:          "tok_abc123",
		Label:       "Test Agent",
		Permissions: []string{"draft.create"},
		Sources:     []agentTokenFixtureSource{{ID: 1, Type: "imap", Identifier: "alice@example.com"}},
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		Secret:      secret,
	}
	b, err := json.Marshal(resp)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// agentTokenListResponseJSON returns a minimal JSON list response for tests.
func agentTokenListResponseJSON() string {
	resp := agentTokenListFixture{
		Tokens: []agentTokenFixtureView{{
			ID:          "tok_abc123",
			Label:       "Test Agent",
			Permissions: []string{"draft.create"},
			Sources:     []agentTokenFixtureSource{{ID: 1, Type: "imap", Identifier: "alice@example.com"}},
			CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		}},
	}
	b, err := json.Marshal(resp)
	if err != nil {
		panic(err)
	}
	return string(b)
}

type agentTokenFixtureSource struct {
	ID         int64  `json:"id"`
	Type       string `json:"type"`
	Identifier string `json:"identifier"`
}

type agentTokenFixtureView struct {
	ID          string                    `json:"id"`
	Label       string                    `json:"label"`
	Permissions []string                  `json:"permissions"`
	Sources     []agentTokenFixtureSource `json:"sources"`
	CreatedAt   string                    `json:"created_at"`
}

type agentTokenIssueFixture struct {
	ID          string                    `json:"id"`
	Label       string                    `json:"label"`
	Permissions []string                  `json:"permissions"`
	Sources     []agentTokenFixtureSource `json:"sources"`
	CreatedAt   string                    `json:"created_at"`
	Secret      string                    `json:"secret"`
	DaemonURL   string                    `json:"daemon_url"`
}

type agentTokenListFixture struct {
	Tokens []agentTokenFixtureView `json:"tokens"`
}

func TestDelegatedDraftSourceScopeThroughHTTP(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fixture := newDraftReplyFixture(t)
	adapter := fixture.grantedAdapter()
	providerCalls := 0
	clientFactory := adapter.draftClientFactory
	adapter.draftClientFactory = func(ctx context.Context, source *store.Source) (*imaplib.Client, error) {
		providerCalls++
		return clientFactory(ctx, source)
	}
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{
			HomeDir: t.TempDir(),
			Server:  config.ServerConfig{APIKey: "owner-test-key", AgentAccess: true},
		},
		Store:  adapter,
		Logger: slog.New(slog.DiscardHandler),
	}).Router())
	t.Cleanup(server.Close)

	issue := func(sourceID int64) string {
		body, err := json.Marshal(map[string]any{
			"label":       "test-agent",
			"permissions": []string{"draft.create"},
			"source_ids":  []int64{sourceID},
		})
		require.NoError(err)
		req, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/agent-tokens", bytes.NewReader(body))
		require.NoError(err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Api-Key", "owner-test-key")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(http.StatusCreated, resp.StatusCode)
		var issued agentTokenIssueFixture
		require.NoError(json.NewDecoder(resp.Body).Decode(&issued))
		return issued.Secret
	}

	run := func(secret string) []api.CLIRunEvent {
		args := []string{
			"draft-reply", strconv.FormatInt(fixture.parentID, 10),
			"--from", testutil.IMAPTestUsername, "--body", "reply body", "--json",
		}
		body, err := json.Marshal(map[string]any{"args": args})
		require.NoError(err)
		req, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/cli/run", bytes.NewReader(body))
		require.NoError(err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Msgvault-Agent-Token", secret)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(http.StatusOK, resp.StatusCode)
		var events []api.CLIRunEvent
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			var event api.CLIRunEvent
			require.NoError(json.Unmarshal(scanner.Bytes(), &event))
			events = append(events, event)
		}
		require.NoError(scanner.Err())
		return events
	}

	events := run(issue(fixture.source.ID))
	require.Len(events, 2)
	assert.Equal(cliStreamStdout, events[0].Type)
	var result draftReplyOutput
	require.NoError(json.Unmarshal([]byte(events[0].Data), &result))
	assert.Equal(draftReplyStatusCreated, result.Status)
	assert.Equal(fixture.source.ID, result.SourceID)
	assert.Equal("Drafts", result.Mailbox)
	assert.Equal("complete", events[1].Type)
	assert.Equal(1, providerCalls)

	secondSource, err := fixture.store.GetOrCreateSource("imap", "other@example.com")
	require.NoError(err)
	events = run(issue(secondSource.ID))
	require.Len(events, 1)
	assert.Equal("error", events[0].Type)
	assert.Equal("not_permitted", events[0].Error)
	assert.Equal(1, providerCalls, "an out-of-grant source must be rejected before provider work")
}

// TestAgentTokenIssueOutputsSecret verifies that the issue subcommand (row 6):
//   - sends POST /api/v1/agent-tokens with the correct JSON body
//   - displays the token ID, label, permissions, and one-time secret in plain
//     text output
//   - does NOT include the secret in a subsequent list response
func TestAgentTokenIssueOutputsSecret(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const wantSecret = "mva1_dGVzdHNlY3JldGZvcnVuaXR0ZXN0aW5ncHVycG9zZXM"

	var gotMethod, gotPath string
	var gotBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(agentTokenIssueResponseJSON(wantSecret)))
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	output, err := runAgentTokenCommand(t, agentTokenIssueCmd,
		"--label", "Test Agent",
		"--permissions", "draft.create",
		"--source-ids", "1",
	)
	require.NoError(err)

	assert.Equal(http.MethodPost, gotMethod)
	assert.Equal("/api/v1/agent-tokens", gotPath)
	assert.Equal("Test Agent", gotBody["label"])

	assert.Contains(output, "tok_abc123")
	assert.Contains(output, "Test Agent")
	assert.Contains(output, wantSecret, "one-time secret must appear in issue output")
	output, err = runAgentTokenCommand(t, agentTokenIssueCmd,
		"--label", "Test Agent", "--permissions", "draft.create", "--source-ids", "1", "--json")
	require.NoError(err)
	assert.True(strings.HasSuffix(output, "\n"), "JSON output must end with a newline")
}

// TestAgentTokenListFormatsTable verifies that the list subcommand (row 18):
//   - sends GET /api/v1/agent-tokens
//   - formats the response as a human-readable table
//   - does NOT include any secret value in the output
func TestAgentTokenListFormatsTable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var gotMethod, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(agentTokenListResponseJSON()))
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	output, err := runAgentTokenCommand(t, agentTokenListCmd)
	require.NoError(err)

	assert.Equal(http.MethodGet, gotMethod)
	assert.Equal("/api/v1/agent-tokens", gotPath)

	assert.Contains(output, "tok_abc123")
	assert.Contains(output, "Test Agent")
	assert.Contains(output, "alice@example.com", "source identifier must appear in list output")
	assert.NotContains(output, "mva1_", "secret must never appear in list output")
}

// TestAgentTokenRevokeCallsDelete verifies that the revoke subcommand (row 22):
//   - sends DELETE /api/v1/agent-tokens/{id}
//   - prints a confirmation message on success
func TestAgentTokenRevokeCallsDelete(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var gotMethod, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	output, err := runAgentTokenCommand(t, agentTokenRevokeCmd, "tok_abc123")
	require.NoError(err)

	assert.Equal(http.MethodDelete, gotMethod)
	assert.Equal("/api/v1/agent-tokens/tok_abc123", gotPath)
	assert.Contains(output, "revoked")
}

// TestOpenAgentDelegatedStore verifies that the --agent-url / --agent-token-file
// flags authenticate with a grant issued by the real daemon.
func TestOpenAgentDelegatedStore(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "user@example.com")
	require.NoError(err)
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{
			HomeDir: t.TempDir(),
			Server:  config.ServerConfig{APIKey: "owner-test-key", AgentAccess: true},
		},
		Store:  &storeAPIAdapter{store: st},
		Logger: slog.New(slog.DiscardHandler),
	}).Router())
	t.Cleanup(server.Close)
	owner, err := daemonclient.New(daemonclient.Config{
		URL: server.URL, APIKey: "owner-test-key", AllowInsecure: true,
	})
	require.NoError(err)
	t.Cleanup(func() { _ = owner.Close() })
	grant, err := owner.IssueAgentToken(t.Context(), "test agent", []string{"draft.create"}, []int64{source.ID})
	require.NoError(err)

	// Write the token to a temp file.
	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	require.NoError(os.WriteFile(tokenFile, []byte(grant.Secret+"\n"), 0o600))

	// Set package-level flags.
	oldURL, oldFile, oldInsecure := agentURL, agentTokenFile, agentAllowInsecure
	agentURL = server.URL
	agentTokenFile = tokenFile
	agentAllowInsecure = true
	t.Cleanup(func() {
		agentURL = oldURL
		agentTokenFile = oldFile
		agentAllowInsecure = oldInsecure
	})

	client, info, err := OpenHTTPStore(t.Context())
	require.NoError(err)
	require.NotNil(client)
	t.Cleanup(func() { _ = client.Close() })
	assert.Equal(HTTPStoreAgentDelegated, info.Kind)
	assert.Equal(server.URL, info.URL)

	// The daemon requires authentication for this route.
	_, err = client.GetHealth(t.Context())
	require.NoError(err)
}

// TestOpenAgentDelegatedStoreRejectsLocalFlag verifies that combining --local
// with --agent-url is an error.
func TestOpenAgentDelegatedStoreRejectsLocalFlag(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("mva1_abc"), 0o600))

	oldURL, oldFile, oldUseLocal := agentURL, agentTokenFile, useLocal
	agentURL = "http://daemon:8080"
	agentTokenFile = tokenFile
	useLocal = true
	t.Cleanup(func() {
		agentURL = oldURL
		agentTokenFile = oldFile
		useLocal = oldUseLocal
	})

	_, _, err := OpenHTTPStore(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "incompatible", err.Error())
}

// TestOpenAgentDelegatedStoreRequiresBothFlags verifies that providing only one
// of --agent-url or --agent-token-file is an error (either flag triggers
// delegated mode; the other is then required).
func TestOpenAgentDelegatedStoreRequiresBothFlags(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "agent.token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("mva1_abc"), 0o600))

	t.Run("agent-url alone errors", func(t *testing.T) {
		oldURL, oldFile := agentURL, agentTokenFile
		agentURL = "http://daemon:8080"
		agentTokenFile = ""
		t.Cleanup(func() { agentURL = oldURL; agentTokenFile = oldFile })

		_, _, err := openAgentDelegatedStore(t.Context())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--agent-token-file")
	})

	t.Run("agent-token-file alone errors", func(t *testing.T) {
		oldURL, oldFile := agentURL, agentTokenFile
		agentURL = ""
		agentTokenFile = tokenFile
		t.Cleanup(func() { agentURL = oldURL; agentTokenFile = oldFile })

		_, _, err := openAgentDelegatedStore(t.Context())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--agent-url")
	})
}

func TestOpenHTTPStoreRejectsExplicitEmptyAgentFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--agent-url="},
		{"--agent-token-file="},
		{"--agent-url=", "--agent-token-file="},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			require := require.New(t)
			withAgentFlags(t, "", "")
			withStoreResolverConfig(t, &config.Config{
				Remote: config.RemoteConfig{URL: "https://daemon.example", APIKey: "owner-test-key"},
			})
			for _, name := range []string{"agent-url", "agent-token-file"} {
				flag := rootCmd.PersistentFlags().Lookup(name)
				changed := flag.Changed
				flag.Changed = false
				t.Cleanup(func() { flag.Changed = changed })
			}
			cmd := &cobra.Command{Use: "draft-reply"}
			cmd.Flags().AddFlagSet(rootCmd.PersistentFlags())
			require.NoError(cmd.ParseFlags(args))

			client, info, err := OpenHTTPStore(t.Context())
			if client != nil {
				t.Cleanup(func() { _ = client.Close() })
			}
			require.ErrorContains(err, "--agent-url is required", "selected store: %s", info.Kind)
		})
	}
}

// TestAgentModeRejectsConfigFlag verifies that --config is rejected in
// agent-delegated mode for a delegated-capable command.
func TestAgentModeRejectsConfigFlag(t *testing.T) {
	oldURL, oldFile := agentURL, agentTokenFile
	oldCfg := cfgFile
	agentURL = "http://daemon:8080"
	agentTokenFile = "/tmp/token"
	cfgFile = "/tmp/config.toml"
	t.Cleanup(func() {
		agentURL = oldURL
		agentTokenFile = oldFile
		cfgFile = oldCfg
	})

	cmd := &cobra.Command{Use: "draft-reply"}
	err := rootCmd.PersistentPreRunE(cmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--config")
}

// TestAgentModeRejectsHomeFlag verifies that --home is rejected in
// agent-delegated mode for a delegated-capable command.
func TestAgentModeRejectsHomeFlag(t *testing.T) {
	oldURL, oldFile := agentURL, agentTokenFile
	oldHome := homeDir
	agentURL = "http://daemon:8080"
	agentTokenFile = "/tmp/token"
	homeDir = "/tmp/home"
	t.Cleanup(func() {
		agentURL = oldURL
		agentTokenFile = oldFile
		homeDir = oldHome
	})

	cmd := &cobra.Command{Use: "draft-reply"}
	err := rootCmd.PersistentPreRunE(cmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--home")
}

// TestOpenAgentDelegatedStoreRejectsNonexistentTokenFile verifies that a missing
// token file produces a clear error (P3: token-file read-error branch).
func TestOpenAgentDelegatedStoreRejectsNonexistentTokenFile(t *testing.T) {
	oldURL, oldFile := agentURL, agentTokenFile
	agentURL = "https://daemon:8080"
	agentTokenFile = filepath.Join(t.TempDir(), "does-not-exist.token")
	t.Cleanup(func() { agentURL = oldURL; agentTokenFile = oldFile })

	_, _, err := openAgentDelegatedStore(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read agent token file")
}

// TestOpenAgentDelegatedStoreRejectsEmptyTokenFile verifies that an empty or
// whitespace-only token file produces a clear error (P3: empty-file branch).
func TestOpenAgentDelegatedStoreRejectsEmptyTokenFile(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "empty.token")

	t.Run("empty file", func(t *testing.T) {
		require.NoError(t, os.WriteFile(tokenFile, []byte(""), 0o600))
		oldURL, oldFile := agentURL, agentTokenFile
		agentURL = "https://daemon:8080"
		agentTokenFile = tokenFile
		t.Cleanup(func() { agentURL = oldURL; agentTokenFile = oldFile })

		_, _, err := openAgentDelegatedStore(t.Context())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "is empty")
	})

	t.Run("whitespace only", func(t *testing.T) {
		require.NoError(t, os.WriteFile(tokenFile, []byte("   \n\t  \n"), 0o600))
		oldURL, oldFile := agentURL, agentTokenFile
		agentURL = "https://daemon:8080"
		agentTokenFile = tokenFile
		t.Cleanup(func() { agentURL = oldURL; agentTokenFile = oldFile })

		_, _, err := openAgentDelegatedStore(t.Context())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "is empty")
	})
}
