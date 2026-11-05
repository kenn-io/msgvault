package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

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
