package cmd

import (
	"encoding/json/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPersonCardsMCPCapabilityGate(t *testing.T) {
	for _, version := range []string{"3.25.0", "3.26.0"} {
		t.Run(version, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				assert.NoError(t, json.MarshalWrite(w, map[string]any{"status": "ok", "api_schema_version": version}))
			}))
			t.Cleanup(server.Close)
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
			requirements.NoError(err)
			t.Cleanup(func() { assert.NoError(t, client.Close()) })
			opts := daemonMCPServeOptions(t.Context(), client, nil)
			assertions.Equal(version == "3.26.0", opts.PersonCards != nil)
			delegated, err := delegatedMCPServeOptions(t.Context(), client)
			requirements.NoError(err)
			assertions.Nil(delegated.PersonCards)
		})
	}
}
