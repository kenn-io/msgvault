package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPersonTiersWithVerificationWithheld(t *testing.T) {
	st := testutil.NewTestStore(t)
	participant, err := st.EnsureParticipant("person@example.test", "Example Person", "example.test")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(participant)
	require.NoError(t, err)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	srv := NewServer(cfg, st, nil, testLogger())
	for _, path := range []string{"/api/v1/people/tiers", fmt.Sprintf("/api/v1/people/%d/tier", person.ID)} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		req.Header.Set(apiprotocol.VerificationHeader, apiprotocol.VerificationWithhold)
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, req)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		if path != "/api/v1/people/tiers" {
			var body store.PersonTierExplanation
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
			assert.Equal(t, person.ID, body.PersonID)
		}
	}
}

// The routes read the config file on each request, so an owner's edit to the
// running daemon's config takes effect, and a bad edit is refused rather than
// answered with defaults.
func TestPersonTierRoutesFollowConfigEdits(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	srv := NewServer(cfg, testutil.NewTestStore(t), nil, testLogger())
	edit := func(content string) {
		require.NoError(os.WriteFile(filepath.Join(cfg.HomeDir, "config.toml"), []byte(content), 0o600))
	}

	response := personRequest(t, srv, http.MethodGet, "/api/v1/people/tiers", nil, "")
	require.Equal(http.StatusOK, response.Code, response.Body.String())

	for _, content := range []string{
		"[people.tiers.tier2]\nmin_active_days_lng = 4\n",
		"[people.tiers.tier1]\nmin_outbound_short = 0\n",
	} {
		edit(content)
		for _, path := range []string{"/api/v1/people/tiers", "/api/v1/people/1/tier"} {
			response = personRequest(t, srv, http.MethodGet, path, nil, "")
			require.Equal(http.StatusServiceUnavailable, response.Code, response.Body.String())
			var body struct {
				Error string `json:"error"`
			}
			require.NoError(json.Unmarshal(response.Body.Bytes(), &body))
			assert.Equal("person_tiers_config_invalid", body.Error, content)
		}
	}

	// An unrelated daemon-only mistake does not block tiers.
	edit("[people.identity_scoring]\nunknown_key = 1\n")
	response = personRequest(t, srv, http.MethodGet, "/api/v1/people/tiers", nil, "")
	assert.Equal(http.StatusOK, response.Code, response.Body.String())
}
