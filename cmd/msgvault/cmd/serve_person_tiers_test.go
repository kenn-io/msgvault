package cmd_test

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/cmd/msgvault/cmd"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPersonTiersDaemonAdapterHTTP(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	id, err := st.EnsureParticipant("daemon-tiers@example.test", "Daemon Tier Person", "example.test")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipantContext(t.Context(), id)
	require.NoError(t, err)
	cfg := config.NewDefaultConfig()
	cfg.People.Tiers.MinimumTier = 2
	adapter := cmd.PersonTierDaemonStoreForTest(st)
	srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, OperationGate: api.NewSerialOperationGate(), Logger: slog.New(slog.DiscardHandler)})
	request := func(method, path, body string) {
		t.Helper()
		r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, r)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	}
	request(http.MethodGet, "/api/v1/people-tiers/preview", "")
	request(http.MethodPost, "/api/v1/people-tiers/apply", `{}`)
	request(http.MethodGet, fmt.Sprintf("/api/v1/people/%d/tier", person.ID), "")
	request(http.MethodGet, "/api/v1/people-tiers/history", "")
	request(http.MethodGet, "/api/v1/people-tiers/candidates", "")
	request(http.MethodPut, fmt.Sprintf("/api/v1/people/%d/tier/opt-outs", person.ID), `{"lane":"enrichment","opted_out":true}`)
	outs, err := st.ListPersonTierOptOutsContext(t.Context(), person.ID)
	require.NoError(t, err)
	require.Len(t, outs, 1)
	assert.Equal(t, "api", outs[0].Actor)
	request(http.MethodPut, fmt.Sprintf("/api/v1/people/%d/tracking", person.ID), `{"tracked":true}`)
	tracking, err := st.GetPersonTrackingContext(t.Context(), person.ID)
	require.NoError(t, err)
	assert.Equal(t, "api", tracking.Actor)
	request(http.MethodPost, "/api/v1/people-tiers/reset", `{"confirm":true}`)
}
