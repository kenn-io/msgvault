package cmd

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/circleback"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/plaud"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPlaudCommandDiscovery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, name := range []string{"add-plaud", "sync-plaud"} {
		cmd, _, err := rootCmd.Find([]string{name})
		require.NoError(err)
		assert.Equal(name, cmd.Name())
	}
	cmd, _, err := rootCmd.Find([]string{"sync-plaud"})
	require.NoError(err)
	for _, name := range []string{"limit", "full", "after", "probe", "build-cache", "no-build-cache"} {
		require.NotNil(cmd.Flags().Lookup(name))
	}
}
func TestPlaudRemoteRefusesOAuthBeforeProxy(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	server, requests := newDaemonCLIRunnerTestServer(t, nil, `{"type":"complete"}`)
	ctx := configureRemoteDaemonForTest(t, server.URL)
	invocationFromContext(ctx).cfg.Plaud = []config.PlaudSource{{Identifier: "work", AccountEmail: "owner@example.com"}}
	cmd := newAddPlaudCmd()
	cmd.SetContext(ctx)
	cmd.SetArgs([]string{"work"})
	err := cmd.Execute()
	require.Error(err)
	assert.Contains(err.Error(), "daemon host")
	assert.Contains(err.Error(), "SSH")
	assert.Zero(requests.Load())
}
func TestPlaudInvalidSyncFlagsBeforeProxy(t *testing.T) {
	for _, args := range [][]string{{"--limit=-1"}, {"--after=not-a-date"}} {
		t.Run(args[0], func(t *testing.T) {
			server, requests := newDaemonCLIRunnerTestServer(t, nil, `{"type":"complete"}`)
			cmd := newSyncPlaudCmd()
			cmd.SetContext(configureRemoteDaemonForTest(t, server.URL))
			cmd.SetArgs(args)
			require.Error(t, cmd.Execute())
			assert.Zero(t, requests.Load())
		})
	}
}
func TestPlaudProbeRequiresIdentifierForMultipleAccounts(t *testing.T) {
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Plaud = []config.PlaudSource{
		{Identifier: "work", AccountEmail: "owner@example.com"},
		{Identifier: "personal", AccountEmail: "personal@example.com"},
	}
	ctx, cancel := context.WithCancel(testInvocationContext(context.Background(), cfg, invocationOptions{}))
	cancel()
	cmd := newSyncPlaudCmd()
	cmd.SetContext(ctx)
	cmd.SetArgs([]string{"--probe"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple [[plaud]] sources configured; pass an identifier")
}
func TestPlaudRegistrationRejectsLiveMismatchBeforeCreatingSource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	for _, email := range []string{"other@example.com", "Display <owner@example.com>"} {
		_, err := registerPlaudAccount(st, "work", "owner@example.com", email)
		require.Error(err)
		sources, err := st.ListSources("plaud")
		require.NoError(err)
		assert.Empty(sources)
	}
	src, err := registerPlaudAccount(st, "work", " Owner@Example.COM ", "owner@example.com")
	require.NoError(err)
	_, err = registerPlaudAccount(st, "work", "other@example.com", "other@example.com")
	require.Error(err)
	got, err := st.GetSourceByTypeAndIdentifier("plaud", "work")
	require.NoError(err)
	assert.Equal(src.ID, got.ID)
}
func TestPlaudOwnerMismatchStopsBeforeAuthorization(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()

	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	state := testInvocationWithConfig(cfg)
	func() {
		st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
		require.NoError(err)
		defer cleanup()
		_, err = registerPlaudAccount(st, "work", "owner@example.com", "owner@example.com")
		require.NoError(err)
	}()

	cfg.Plaud = []config.PlaudSource{{
		Identifier:   "work",
		AccountEmail: "replacement@example.com",
		Endpoint:     server.URL,
	}}
	cmd := newAddPlaudLocalCmd()
	cmd.SetContext(testInvocationContext(context.Background(), cfg, invocationOptions{}))
	cmd.SetArgs([]string{"work"})
	err := cmd.Execute()
	require.ErrorContains(err, "plaud source owner differs or is unconfirmed")
	assert.Zero(requests.Load())
}
func TestPlaudOwnerPreflightAllowsUnboundSource(t *testing.T) {
	require := require.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	state := testInvocationWithConfig(cfg)
	func() {
		st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
		require.NoError(err)
		defer cleanup()
		_, err = st.GetOrCreateSource("plaud", "work")
		require.NoError(err)
	}()

	require.NoError(validatePlaudOwnerBeforeAuthorization(state, "work", "owner@example.com"))
}
func TestPlaudScheduledMissingSourceStopsBeforeAuth(t *testing.T) {
	st := testutil.NewTestStore(t)
	ctx, cancel := context.WithCancel(testInvocationContext(context.Background(), config.NewDefaultConfig(), invocationOptions{}))
	cancel()
	err := runConfiguredPlaudSync(ctx, st, config.PlaudSource{Identifier: "work", AccountEmail: "owner@example.com"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "add-plaud work")
}
func TestPlaudPartialCanceledImportRefreshesDetachedContext(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	refreshErr := errors.New("refresh failed")
	err := finishScheduledPlaudImport(ctx, "work", &plaud.ImportSummary{MeetingsAdded: 1}, context.Canceled, func(refreshCtx context.Context, name string) error {
		calls++
		require.NoError(refreshCtx.Err())
		assert.Equal("plaud:work", name)
		return refreshErr
	})
	assert.Equal(1, calls)
	require.ErrorIs(err, context.Canceled)
	require.ErrorIs(err, refreshErr)
	calls = 0
	err = finishPlaudImport(context.Background(), "work", &plaud.ImportSummary{}, errors.New("failed"), func() error { calls++; return nil })
	require.Error(err)
	assert.Zero(calls)
}

type plaudProbeFixture struct{}

func (plaudProbeFixture) ToolInventory(context.Context) ([]plaud.ToolInfo, error) {
	return []plaud.ToolInfo{{Name: "list_files", Description: "Find recordings", InputSchema: []byte(`{"type":"object"}`)}}, nil
}
func (plaudProbeFixture) ListFiles(context.Context, int, int) (plaud.FilePage, error) {
	return plaud.FilePage{Files: []plaud.File{{ID: "private-id", Name: "Private meeting title"}}}, nil
}
func TestPlaudProbePrintsToolsAndCountsOnly(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var out bytes.Buffer
	require.NoError(runPlaudProbe(t.Context(), &out, plaudProbeFixture{}))
	assert.Contains(out.String(), "list_files")
	assert.Contains(out.String(), "1")
	assert.NotContains(out.String(), "private-id")
	assert.NotContains(out.String(), "Private meeting title")
}
func TestRemovePlaudAccountDeletesOnlyItsToken(t *testing.T) {
	require := require.New(t)
	tmp := t.TempDir()
	cfg := &config.Config{HomeDir: tmp, Data: config.DataConfig{DataDir: tmp}}
	st, err := store.Open(filepath.Join(tmp, "msgvault.db"))
	require.NoError(err)
	require.NoError(st.InitSchema())
	_, err = st.GetOrCreateSource("plaud", "work")
	require.NoError(err)
	require.NoError(st.Close())
	mgr := plaud.NewManager("", cfg.TokensDir(), nil)
	require.NoError(os.MkdirAll(cfg.TokensDir(), 0700))
	token := mgr.TokenPath("work")
	other := mgr.TokenPath("other")
	cb := circleback.NewManager("", cfg.TokensDir(), nil).TokenPath("work")
	for _, p := range []string{token, other, cb} {
		require.NoError(os.WriteFile(p, []byte(`{}`), 0600))
	}
	root := newTestRootCmd()
	root.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	root.AddCommand(newRemoveAccountLocalTestCmd())
	root.SetArgs([]string{"remove-account", "work", "--yes", "--type", "plaud"})
	require.NoError(root.Execute())
	_, err = os.Stat(token)
	require.ErrorIs(err, os.ErrNotExist)
	for _, p := range []string{other, cb} {
		_, err = os.Stat(p)
		require.NoError(err)
	}
}

func TestPlaudManualMissingSourceStopsBeforeAuth(t *testing.T) {
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	tmp := t.TempDir()
	cfg := &config.Config{HomeDir: tmp, Data: config.DataConfig{DataDir: tmp}, Plaud: []config.PlaudSource{{Identifier: "work", AccountEmail: "owner@example.com", Endpoint: "http://127.0.0.1:1/mcp"}}}
	cmd := newSyncPlaudCmd()
	cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	cmd.SetArgs([]string{"work"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "add-plaud work")
}

func TestPlaudManualCacheClassification(t *testing.T) {
	assert.True(t, manualSyncCLICommand([]string{"sync-plaud", "work"}))
	assert.False(t, manualSyncCLICommand([]string{"sync-plaud", "work", "--probe"}))
	assert.False(t, manualSyncCLICommand([]string{"sync-plaud", "work", "--probe=true"}))
}
