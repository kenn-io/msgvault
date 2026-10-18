package cmd_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	cmd "go.kenn.io/msgvault/cmd/msgvault/cmd"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func runRealTierCLI(t *testing.T, url string, args ...string) string {
	t.Helper()
	command := cmd.PersonTiersCommandForTest(t.Context(), url)
	var output, stderr bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&stderr)
	command.SetArgs(args)
	require.NoError(t, command.Execute(), stderr.String())
	return output.String()
}

func realTierCLICount(t *testing.T, st *store.Store, table string) int {
	t.Helper()
	var count int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&count))
	return count
}

func TestPersonTiersCLIChangesAuthorityThroughRealHTTPHandlers(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	ids := make([]int64, 0, 3)
	for _, name := range []string{"alpha", "beta", "gamma"} {
		participant, err := st.EnsureParticipant("tier-cli-"+name+"@example.test", "Synthetic Tier "+name, "example.test")
		require.NoError(err)
		person, _, err := st.CreatePersonFromParticipantContext(t.Context(), participant)
		require.NoError(err)
		ids = append(ids, person.ID)
	}
	cfg := config.NewDefaultConfig()
	// The configured floor makes the outcome independent of dates and clocks:
	// this archive has no events and every saved person computes Tier2.
	cfg.People.Tiers.MinimumTier = 2
	daemon := api.NewServerWithOptions(api.ServerOptions{
		Config: cfg, Store: cmd.PersonTierDaemonStoreForTest(st), OperationGate: api.NewSerialOperationGate(),
		Logger: slog.New(slog.DiscardHandler),
	})
	router := daemon.Router()
	var applyRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/people-tiers/apply" {
			applyRequests.Add(1)
		}
		router.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)

	var preview store.PersonTierPage
	require.NoError(json.Unmarshal([]byte(runRealTierCLI(t, server.URL, "preview", "--limit", "1", "--json")), &preview))
	require.Len(preview.People, 1)
	assert.Equal(ids[0], preview.People[0].PersonID)
	assert.Equal(2, preview.People[0].Decision.ComputedTier)
	assert.NotEmpty(preview.NextCursor)
	assert.Zero(realTierCLICount(t, st, "person_tier_state"))
	assert.Zero(realTierCLICount(t, st, "person_tracking"))
	assert.Zero(realTierCLICount(t, st, "person_tier_events"))

	var bounded store.PersonTierPage
	require.NoError(json.Unmarshal([]byte(runRealTierCLI(t, server.URL, "apply", "--yes", "--limit", "1", "--max", "1", "--json")), &bounded))
	require.Len(bounded.People, 1)
	assert.NotEmpty(bounded.RunID)
	assert.NotEmpty(bounded.NextCursor)
	assert.Equal(int32(1), applyRequests.Load())
	assert.Equal(1, realTierCLICount(t, st, "person_tracking"), "--max bounds committed people")
	assert.Equal(1, realTierCLICount(t, st, "person_tier_state"))
	untouched, err := st.GetPersonTrackingContext(t.Context(), ids[1])
	require.NoError(err)
	assert.False(untouched.Tracked)

	// A fresh invocation begins a new run and drains three bounded pages.
	runRealTierCLI(t, server.URL, "apply", "--yes", "--limit", "1")
	assert.Equal(int32(4), applyRequests.Load())
	assert.Equal(3, realTierCLICount(t, st, "person_tracking"))
	for _, id := range ids {
		tracking, err := st.GetPersonTrackingContext(t.Context(), id)
		require.NoError(err)
		assert.True(tracking.Tracked)
		assert.Equal("auto", tracking.Provenance)
	}
	finished, err := st.PersonTierRunContext(t.Context(), "manual")
	require.NoError(err)
	assert.Nil(finished)

	var explanation store.PersonTierExplanation
	require.NoError(json.Unmarshal([]byte(runRealTierCLI(t, server.URL, "explain", strconv.FormatInt(ids[0], 10), "--json")), &explanation))
	assert.Equal(2, explanation.Prior.Tier)
	require.NotNil(explanation.Tracking)
	assert.Equal("auto", explanation.Tracking.Provenance)
	assert.False(explanation.EnrichmentEligible)

	runRealTierCLI(t, server.URL, "opt-out", strconv.FormatInt(ids[0], 10), "--lane", "tracking", "--reason", "synthetic pause")
	removed, err := st.GetPersonTrackingContext(t.Context(), ids[0])
	require.NoError(err)
	assert.False(removed.Tracked)
	outs, err := st.ListPersonTierOptOutsContext(t.Context(), ids[0])
	require.NoError(err)
	require.Len(outs, 1)
	assert.Equal("tracking", outs[0].Lane)
	assert.Equal("api", outs[0].Actor)
	assert.Equal("synthetic pause", outs[0].Reason)

	_, err = st.SetPersonTrackingActorContext(t.Context(), ids[2], true, "synthetic-manual-actor")
	require.NoError(err)
	var reset store.PersonTierResetResult
	require.NoError(json.Unmarshal([]byte(runRealTierCLI(t, server.URL, "reset", "--yes", "--json")), &reset))
	assert.Equal(3, reset.Cleared)
	assert.Empty(reset.Deferred)
	assert.Zero(realTierCLICount(t, st, "person_tier_state"))
	assert.Equal(1, realTierCLICount(t, st, "person_tracking"))
	manual, err := st.GetPersonTrackingContext(t.Context(), ids[2])
	require.NoError(err)
	assert.True(manual.Tracked)
	assert.Equal("manual", manual.Provenance)
	assert.Equal("synthetic-manual-actor", manual.Actor)
	outs, err = st.ListPersonTierOptOutsContext(t.Context(), ids[0])
	require.NoError(err)
	require.Len(outs, 1)
	assert.Equal("synthetic pause", outs[0].Reason)

	var history api.PersonTierHistoryResponse
	require.NoError(json.Unmarshal([]byte(runRealTierCLI(t, server.URL, "history", "--person", strconv.FormatInt(ids[0], 10), "--json")), &history))
	require.NotEmpty(history.Events)
	assert.Equal("reset", history.Events[0].Reason)
}
