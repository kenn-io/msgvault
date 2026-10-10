package cmd

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
)

// Only discovery metadata is varied to test the external compatibility
// contract. All version/authentication and native discovery handling remain real.
func TestScopedCardDAVDiscoveryAdmitsExactIndependentRoutes(t *testing.T) {
	for _, tc := range []struct {
		name                              string
		preview, approve, reconcile, fail bool
	}{
		{"all", true, true, true, false},
		{"recovery only", false, false, true, false},
		{"preview method", false, true, true, false},
		{"approval missing key", true, false, true, false},
		{"approval missing token", true, false, true, false},
		{"recovery path", true, true, false, false},
		{"recovery method", true, true, false, false},
		{"old daemon", false, false, false, false},
		{"failed discovery", false, false, false, true},
		{"wrong principal", false, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			var changed atomic.Bool
			fixture := newDraftReplyFixture(t)
			server := mcpDraftTestDaemon(t, fixture.grantedAdapter(), func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/api/v1/mcp/capabilities" {
						next.ServeHTTP(w, r)
						return
					}
					if changed.Load() && tc.name == "failed discovery" {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					recorder := httptest.NewRecorder()
					next.ServeHTTP(recorder, r)
					var descriptor apiprotocol.MCPCapabilities
					if !assertions.NoError(json.Unmarshal(recorder.Body.Bytes(), &descriptor)) {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					// Replace only the contract under test so native receipt
					// recovery admission cannot mask an incompatible variant.
					descriptor.Routes = slices.DeleteFunc(descriptor.Routes, func(route apiprotocol.MCPRouteDescriptor) bool {
						switch route.OperationID {
						case "previewScopedCardDAVPublication", "approveScopedCardDAVPublication", "reconcileScopedCardDAVPublication":
							return true
						}
						return false
					})
					routes := []apiprotocol.MCPRouteDescriptor{
						{OperationID: "previewScopedCardDAVPublication", Method: http.MethodGet, Path: "/api/v1/carddav/scoped/publications/{person_id}/preview"},
						{OperationID: "approveScopedCardDAVPublication", Method: http.MethodPost, Path: "/api/v1/carddav/scoped/publications/{person_id}/approve", RequestProperties: []string{"approval_token", "idempotency_key"}},
						{OperationID: "reconcileScopedCardDAVPublication", Method: http.MethodPost, Path: "/api/v1/carddav/scoped/publications/{person_id}/reconcile", RequestProperties: []string{"approval_token", "idempotency_key"}},
					}
					if changed.Load() {
						switch tc.name {
						case "recovery only":
							routes = routes[2:]
						case "preview method":
							routes[0].Method = http.MethodPost
						case "approval missing key":
							routes[1].RequestProperties = []string{"approval_token"}
						case "approval missing token":
							routes[1].RequestProperties = []string{"idempotency_key"}
						case "recovery path":
							routes[2].Path = "/api/v1/unsupported"
						case "recovery method":
							routes[2].Method = http.MethodGet
						case "wrong principal":
							descriptor.Delegated = true
						}
					}
					descriptor.Routes = append(descriptor.Routes, routes...)
					w.Header().Set("Content-Type", "application/json")
					assertions.NoError(json.MarshalWrite(w, descriptor))
				})
			})
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner-test-key", AllowInsecure: true})
			requirements.NoError(err)
			t.Cleanup(func() { assertions.NoError(client.Close()) })
			opts := mcpserver.ServeOptions{}
			requirements.NoError(applyMCPDiscovery(t.Context(), client, &opts, "3.4.0", false))
			requirements.NotNil(opts.ScopedCardDAVPreview)
			requirements.NotNil(opts.ScopedCardDAVApprove)
			requirements.NotNil(opts.ScopedCardDAVReconcile)
			changed.Store(true)
			version := "3.4.0"
			if tc.name == "old daemon" {
				version = "2.0.0"
			}
			err = applyMCPDiscovery(t.Context(), client, &opts, version, false)
			if tc.fail {
				requirements.Error(err)
			} else {
				requirements.NoError(err)
			}
			assertions.Equal(tc.preview, opts.ScopedCardDAVPreview != nil)
			assertions.Equal(tc.approve, opts.ScopedCardDAVApprove != nil)
			assertions.Equal(tc.reconcile, opts.ScopedCardDAVReconcile != nil)
			assertions.Nil(opts.PersonCardDAV)
			assertions.False(opts.AllowCardDAVWrites, "discovery must not opt in to writes")
		})
	}
}
