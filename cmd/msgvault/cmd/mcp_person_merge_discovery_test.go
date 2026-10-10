package cmd

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
)

// The response wrapper exercises compatibility with old or changing daemon
// discovery contracts while retaining the real native discovery route.
func TestMCPPersonMergeDiscoveryClearsPriorAdmission(t *testing.T) {
	for _, tc := range []struct {
		name            string
		read, wantError bool
	}{
		{"older daemon", false, false},
		{"failed discovery", false, true},
		{"wrong descriptor version", false, true},
		{"different principal mode", false, true},
		{"incompatible profile route", false, false},
		{"incompatible merge route", true, false},
		{"missing merge body field", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			fixture := newDraftReplyFixture(t)
			var changed atomic.Bool
			server := mcpDraftTestDaemon(t, fixture.grantedAdapter(), func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if !changed.Load() || r.URL.Path != "/api/v1/mcp/capabilities" {
						next.ServeHTTP(w, r)
						return
					}
					if tc.name == "failed discovery" {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					recorder := httptest.NewRecorder()
					next.ServeHTTP(recorder, r)
					var descriptor apiprotocol.MCPCapabilities
					if err := json.Unmarshal(recorder.Body.Bytes(), &descriptor); err != nil {
						assertions.NoError(err)
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					switch tc.name {
					case "wrong descriptor version":
						descriptor.Version = 2
					case "different principal mode":
						descriptor.Delegated = true
					}
					for i := range descriptor.Routes {
						route := &descriptor.Routes[i]
						if tc.name == "incompatible profile route" && route.OperationID == "getPersonProfile" {
							route.Method = http.MethodPost
						}
						if tc.name == "incompatible merge route" && route.OperationID == "mergePersons" {
							route.Path = "/api/v1/unsupported"
						}
						if tc.name == "missing merge body field" && route.OperationID == "mergePersons" {
							route.RequestProperties = nil
						}
					}
					w.Header().Set("Content-Type", "application/json")
					assertions.NoError(json.MarshalWrite(w, descriptor))
				})
			})
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner-test-key", AllowInsecure: true})
			requirements.NoError(err)
			t.Cleanup(func() { _ = client.Close() })
			opts := mcpserver.ServeOptions{CalendarOnly: true, DraftToolsOnly: true}
			requirements.NoError(applyMCPDiscovery(t.Context(), client, &opts, "3.4.0", false))
			requirements.NotNil(opts.PersonMerge)
			requirements.False(opts.SuppressPersonMergeWrites)
			changed.Store(true)
			version := "3.4.0"
			if tc.name == "older daemon" {
				version = "2.0.0"
			}
			err = applyMCPDiscovery(t.Context(), client, &opts, version, false)
			if tc.wantError {
				requirements.Error(err)
			} else {
				requirements.NoError(err)
			}
			if tc.read {
				assertions.NotNil(opts.PersonMerge)
			} else {
				assertions.Nil(opts.PersonMerge)
			}
			assertions.True(opts.SuppressPersonMergeWrites)
			assertions.Nil(opts.PersonCardDAV)
		})
	}
}
