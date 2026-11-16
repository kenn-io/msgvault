package api

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCardDAVRunHistoryMapsBindingConflictCount(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	run, err := st.StartCardDAVSyncRunContext(t.Context(), store.CardDAVSyncRunStart{AccountID: store.DefaultCardDAVAccountID, Trigger: store.CardDAVSyncTriggerScheduled})
	require.NoError(err)
	_, err = st.FinishCardDAVSyncRunContext(t.Context(), run.ID, store.CardDAVSyncRunFinish{State: store.CardDAVSyncRunSucceeded, BindingConflicts: new(int64(1))})
	require.NoError(err)

	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}}
	controller := &CardDAVController{cfg: cfg, store: st}
	response := getCardDAVRead(t, cardDAVReadServer(t, cfg, controller, nil), "/api/v1/carddav/runs")
	require.Equal(200, response.Code, response.Body.String())
	var body struct {
		Runs []map[string]any `json:"runs"`
	}
	require.NoError(json.NewDecoder(response.Body).Decode(&body))
	require.Len(body.Runs, 1)
	assert.InDelta(float64(1), body.Runs[0]["binding_conflicts"], 0)
}
