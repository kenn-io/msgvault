package cmd

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestPersonDiscoveryDirectoryCLIForwardsFilters(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/health" {
			_, err := w.Write([]byte(`{"status":"ok","api_schema_version":"5.1.0"}`))
			assert.NoError(err)
			return
		}
		gotPath = r.URL.Path
		assert.Equal("13", r.URL.Query().Get("organization_id"))
		assert.Equal("Example Cooperative", r.URL.Query().Get("organization"))
		assert.Equal("colleague", r.URL.Query().Get("category"))
		assert.Equal("active", r.URL.Query().Get("contact_state"))
		_, err := w.Write([]byte(`{"people":[],"next_cursor":"next-page"}`))
		assert.NoError(err)
	}))
	t.Cleanup(server.Close)
	ctx := withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})
	output, err := runPersonDirectoryCommand(ctx, t, "--organization-id", "13", "--organization", "Example Cooperative", "--category", "colleague", "--contact-state", "active", "--json")
	require.NoError(err)
	assert.Equal("/api/v1/people/directory", gotPath)
	assert.Contains(output, "next-page")
}

func TestPersonDiscoveryDirectoryCLIRefusesOldDaemonBeforeListing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	requests, ctx := personDirectoryTestResponse(t, http.StatusOK, `{"people":[]}`)
	_, err := runPersonDirectoryCommand(ctx, t, "--organization-id", "13", "--json")
	require.Error(err)
	assert.Contains(err.Error(), "5.1.0")
	assert.Empty(requests)
}
