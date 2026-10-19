package cmd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	cmd "go.kenn.io/msgvault/cmd/msgvault/cmd"
	"go.kenn.io/msgvault/internal/daemonclient"
)

func TestPersonTierMCPRequiresDaemonSchema324(t *testing.T) {
	for _, tc := range []struct {
		version      string
		status       int
		want         bool
		wantExisting bool
	}{
		{"3.23.0", http.StatusOK, false, true}, {"3.24.0", http.StatusOK, true, true},
		{"3.25.0", http.StatusOK, true, true}, {"unknown", http.StatusOK, false, false},
		{"", http.StatusOK, false, false}, {"3.24.0", http.StatusServiceUnavailable, false, false},
	} {
		t.Run(tc.version+http.StatusText(tc.status), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var probes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/health" {
					http.NotFound(w, r)
					return
				}
				probes.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				assert.NoError(json.NewEncoder(w).Encode(map[string]any{"status": "ok", "api_schema_version": tc.version}))
			}))
			t.Cleanup(server.Close)
			backend, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
			require.NoError(err)
			t.Cleanup(func() { assert.NoError(backend.Close()) })
			options := cmd.PersonTierMCPOptionsForTest(t.Context(), backend)
			assert.Equal(tc.want, options.PersonTierBackend != nil)
			assert.Equal(tc.wantExisting, options.PeopleBackend != nil)
			assert.Equal(tc.wantExisting, options.PersonAgendaBackend != nil)
			assert.Equal(int32(1), probes.Load(), "reuse the existing daemon schema probe")
		})
	}
}
