package cmd

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	matrixsource "go.kenn.io/msgvault/internal/matrix"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestRestrictCommandsStaged(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "cli@example.test")
	require.NoError(err)
	_, err = st.EnsureLabelsBatch(source.ID, map[string]store.LabelInfo{"x": {Name: "Private", Type: "user"}})
	require.NoError(err)
	deps := restrictionDeps{open: func(context.Context) (*store.Store, func(), error) { return st, func() {}, nil }}
	run := func(args ...string) (string, error) {
		cmd := newRestrictLocalCmd(deps)
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(t.Context())
		return out.String(), err
	}
	out, err := run("add", "label", "Private", "--source", strconv.FormatInt(source.ID, 10))
	require.NoError(err)
	assert.Contains(out, "do not yet restrict reads")
	out, err = run("list")
	require.NoError(err)
	assert.Contains(out, `"kind":"label"`)
	_, err = run("add", "label", "absent")
	require.Error(err)
	_, err = run("taint", "off")
	require.NoError(err)
	taint, err := st.AgentRestrictionTaint(t.Context())
	require.NoError(err)
	assert.False(taint)
	rules, err := st.ListAgentRestrictions(t.Context())
	require.NoError(err)
	require.Len(rules, 1)
	_, err = run("remove", strconv.FormatInt(rules[0].ID, 10))
	require.NoError(err)
}

func TestRestrictAmbiguousAndMissingTargetsDoNotWrite(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	first, err := st.GetOrCreateSource("imap", "first@example.test")
	require.NoError(err)
	second, err := st.GetOrCreateSource("imap", "second@example.test")
	require.NoError(err)
	for _, source := range []*store.Source{first, second} {
		_, err = st.EnsureLabelsBatch(source.ID, map[string]store.LabelInfo{"private": {Name: "Private", Type: "user"}})
		require.NoError(err)
	}
	deps := restrictionDeps{open: func(context.Context) (*store.Store, func(), error) { return st, func() {}, nil }}
	run := func(args ...string) error {
		cmd := newRestrictLocalCmd(deps)
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(args)
		return cmd.ExecuteContext(t.Context())
	}
	err = run("add", "label", "Private")
	require.ErrorContains(err, "ambiguous")
	for _, args := range [][]string{{"add", "label", "absent"}, {"add", "source", "999999"}, {"add", "person", "999999"}, {"add", "label", "Private", "--source", "999999"}} {
		require.Error(run(args...))
	}
	rules, err := st.ListAgentRestrictions(t.Context())
	require.NoError(err)
	assert.Empty(rules)
	require.NoError(run("add", "label", "Private", "--source", strconv.FormatInt(first.ID, 10)))
	rules, err = st.ListAgentRestrictions(t.Context())
	require.NoError(err)
	assert.Len(rules, 1)
}

func TestRestrictMatrixRemovalFencesConcurrentRules(t *testing.T) {
	for _, kind := range []string{"source", "label"} {
		t.Run(kind, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			dir := t.TempDir()
			st := testutil.NewSQLiteTestStoreAt(t, filepath.Join(dir, "msgvault.db"))
			_, err := st.GetOrCreateSource(sourceTypeMatrix, "@other:example.test")
			require.NoError(err)
			source, err := st.GetOrCreateSource(sourceTypeMatrix, "@removal:example.test")
			require.NoError(err)
			rule := store.AgentRestriction{Kind: kind}
			if kind == "source" {
				rule.SourceID = source.ID
			} else {
				rule.LabelID, err = st.EnsureLabel(source.ID, "private", "Private", "user")
				require.NoError(err)
				require.NotEqual(source.ID, rule.LabelID, "the label must resolve its source instead of using its own ID")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			logoutStarted, resumeLogout := make(chan struct{}), make(chan struct{})
			releaseLogout := sync.OnceFunc(func() { close(resumeLogout) })
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				close(logoutStarted)
				select {
				case <-resumeLogout:
					_, _ = w.Write([]byte(`{}`))
				case <-ctx.Done():
				}
			}))
			defer server.Close()
			defer releaseLogout()
			cfg := &config.Config{HomeDir: dir, Data: config.DataConfig{DataDir: dir}}
			require.NoError(matrixsource.SaveCredentials(cfg.TokensDir(), matrixsource.Credentials{
				Homeserver: server.URL, UserID: source.Identifier, DeviceID: "FIXTUREDEVICE", AccessToken: "synthetic-token",
			}))
			root := newTestRootCmd()
			root.SetContext(testInvocationContext(ctx, cfg, invocationOptions{}))
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			root.AddCommand(newRemoveAccountLocalTestCmd())
			root.SetArgs([]string{"remove-account", source.Identifier, "--type", sourceTypeMatrix, "--yes"})
			removed := make(chan error, 1)
			go func() { removed <- root.Execute() }()
			select {
			case <-logoutStarted:
			case <-ctx.Done():
				require.FailNow("removal did not reach Matrix logout")
			}
			_, addErr := st.AddAgentRestriction(ctx, rule)
			releaseLogout()
			select {
			case err := <-removed:
				require.NoError(err)
			case <-ctx.Done():
				require.FailNow("removal did not finish after logout")
			}
			require.ErrorIs(addErr, store.ErrSyncAlreadyActive)
			_, err = st.GetSourceByID(source.ID)
			require.ErrorIs(err, store.ErrSourceNotFound)
			rules, err := st.ListAgentRestrictions(ctx)
			require.NoError(err)
			assert.Empty(rules)
		})
	}
}

func TestRestrictMatrixRemovalPreservesCredentials(t *testing.T) {
	for _, kind := range []string{"source", "label"} {
		t.Run(kind, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			dir := t.TempDir()
			var logouts atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				logouts.Add(1)
				_, _ = w.Write([]byte(`{}`))
			}))
			t.Cleanup(server.Close)
			st := testutil.NewSQLiteTestStoreAt(t, filepath.Join(dir, "msgvault.db"))
			source, err := st.GetOrCreateSource(sourceTypeMatrix, "@restricted:example.test")
			require.NoError(err)
			rule := store.AgentRestriction{Kind: kind}
			if kind == "source" {
				rule.SourceID = source.ID
			} else {
				rule.LabelID, err = st.EnsureLabel(source.ID, "private", "Private", "user")
				require.NoError(err)
			}
			_, err = st.AddAgentRestriction(t.Context(), rule)
			require.NoError(err)
			cfg := &config.Config{HomeDir: dir, Data: config.DataConfig{DataDir: dir}}
			credentials := matrixsource.Credentials{Homeserver: server.URL, UserID: source.Identifier, DeviceID: "FIXTUREDEVICE", AccessToken: "synthetic-token"}
			require.NoError(matrixsource.SaveCredentials(cfg.TokensDir(), credentials))
			root := newTestRootCmd()
			root.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			root.AddCommand(newRemoveAccountLocalTestCmd())
			root.SetArgs([]string{"remove-account", source.Identifier, "--type", sourceTypeMatrix, "--yes"})
			require.ErrorIs(root.Execute(), store.ErrSourceAgentRestriction)
			assert.Zero(logouts.Load())
			retained, err := matrixsource.LoadCredentials(cfg.TokensDir(), source.Identifier)
			require.NoError(err)
			assert.Equal(credentials, retained)
			_, err = st.GetSourceByID(source.ID)
			require.NoError(err)
		})
	}
}
