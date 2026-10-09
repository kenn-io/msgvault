package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestSourceMaintenanceCLIThroughDaemon(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	from, err := st.GetOrCreateSource("beeper", "history-example")
	require.NoError(err)
	into, err := st.GetOrCreateSource("beeper", "live-example")
	require.NoError(err)
	cfg := testConfigValue()
	srv := api.NewServer(cfg, &storeAPIAdapter{store: st, config: cfg, logger: testLoggerValue()}, nil, testLoggerValue())
	httpServer := httptest.NewServer(srv.Router())
	t.Cleanup(httpServer.Close)
	ctx := configureRemoteDaemonForTest(t, httpServer.URL)
	var output bytes.Buffer
	update := newUpdateAccountCmd()
	update.SetContext(ctx)
	update.SetOut(&output)
	update.SetArgs([]string{from.Identifier, "--identifier", "historical-alias", "--history-only=true"})
	require.NoError(update.Execute())
	settings, err := st.GetSourceSettingsContext(t.Context(), from.ID)
	require.NoError(err)
	assert.Equal("historical-alias", settings.Alias)
	assert.True(settings.HistoryOnly)
	resume := newUpdateAccountCmd()
	resume.SetContext(ctx)
	resume.SetOut(&output)
	resume.SetArgs([]string{from.Identifier, "--history-only=false", "--accept-reanchor=false"})
	require.NoError(resume.Execute(), "explicit false history-only is an intentional update")
	settings, err = st.GetSourceSettingsContext(t.Context(), from.ID)
	require.NoError(err)
	assert.False(settings.HistoryOnly)
	assert.Equal("historical-alias", settings.Alias)
	merge := newMergeAccountCmd()
	merge.SetContext(ctx)
	merge.SetOut(&output)
	output.Reset()
	merge.SetArgs([]string{"--from", "historical-alias", "--into", into.Identifier, "--dry-run", "--json"})
	require.NoError(merge.Execute())
	var preview store.SourceMergeResult
	require.NoError(json.Unmarshal(output.Bytes(), &preview))
	assert.Equal(from.ID, preview.FromSourceID)
	assert.Equal(into.ID, preview.IntoSourceID)
	assert.True(preview.DryRun)
	settings, err = st.GetSourceSettingsContext(t.Context(), from.ID)
	require.NoError(err)
	assert.Zero(settings.MergedIntoSourceID)
}

func TestSourceMaintenanceCLIRejectsInvalidUpdatesBeforeDaemon(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags []string
		want  string
	}{
		{"false acceptance", []string{"--accept-reanchor=false"}, "nothing to update"},
		{"empty alias", []string{"--identifier", ""}, "identifier alias must be nonempty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("beeper", "history-example")
			require.NoError(err)
			cfg := testConfigValue()
			srv := api.NewServer(cfg, &storeAPIAdapter{store: st, config: cfg, logger: testLoggerValue()}, nil, testLoggerValue())
			router := srv.Router()
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				router.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			ctx := configureRemoteDaemonForTest(t, server.URL)
			savedDisplayName, savedSourceID := updateDisplayName, updateAccountSourceID
			t.Cleanup(func() { updateDisplayName, updateAccountSourceID = savedDisplayName, savedSourceID })
			cmd := newUpdateAccountCmd()
			cmd.SetContext(ctx)
			cmd.SilenceUsage = true
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			cmd.SetArgs(append([]string{source.Identifier}, tc.flags...))
			require.ErrorContains(cmd.Execute(), tc.want)
			assert.False(cmd.SilenceUsage, "invalid flags must return a local usage error")
			assert.Zero(requests.Load(), "invalid flags must not contact the daemon")
		})
	}
}
