package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestPersonGetContactState(t *testing.T) {
	for _, scenario := range []struct {
		name, version                    string
		probeFails, stateFails, hasState bool
	}{
		{"older daemon", "5.0.0", false, false, false},
		{"optional subresource unavailable", "5.3.0", false, true, false},
		{"version probe fails", "", true, false, false},
		{"current daemon", "5.3.0", false, false, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			check := assert.New(t)
			req := require.New(t)
			var stateRequested bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/people/7":
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"id":7,"vcard_uid":"person-7","revision":2,"participant_ids":[],"created_at":"2026-07-29T12:00:00Z","updated_at":"2026-07-29T12:00:00Z"}`))
				case "/api/v1/health":
					if scenario.probeFails {
						http.Error(w, "unavailable", http.StatusInternalServerError)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"` + scenario.version + `"}`))
				case "/api/v1/people/7/contact-state":
					stateRequested = true
					if scenario.stateFails {
						http.Error(w, "unavailable", http.StatusInternalServerError)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"person_id":7,"interaction_count":0,"cadence_status":"unknown","stale":false,"computed_at":"0001-01-01T00:00:00Z"}`))
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			ctx := withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})
			savedJSON := personJSON
			personJSON = true
			t.Cleanup(func() { personJSON = savedJSON })
			get := &cobra.Command{Use: personGetCmd.Use, Args: personGetCmd.Args, RunE: personGetCmd.RunE}
			get.Flags().String("vcard-uid", "", "")
			var got bytes.Buffer
			get.SetOut(&got)
			get.SetArgs([]string{"7"})
			req.NoError(get.ExecuteContext(ctx))
			var person map[string]any
			req.NoError(json.Unmarshal(got.Bytes(), &person))
			check.InDelta(7, person["id"], 0)
			if scenario.hasState {
				check.Contains(person, "contact_state")
			} else {
				check.NotContains(person, "contact_state")
			}
			check.Equal(scenario.stateFails || scenario.hasState, stateRequested)
		})
	}
}
