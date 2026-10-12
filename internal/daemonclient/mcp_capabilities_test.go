package daemonclient_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
)

// The HTTP peer varies an external compatibility contract that a native daemon
// cannot emit: malformed, oversized or redirected discovery responses.
func TestMCPDiscoveryRejectsUntrustedContracts(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	t.Cleanup(destination.Close)
	for _, tc := range []struct {
		name, version, body string
		redirect, valid     bool
	}{
		{"identity-only daemon", "3.15.0", `{"version":1,"delegated":false,"routes":[],"identity_operations":[]}`, false, true},
		{"current", "3.16.0", `{"version":1,"delegated":false,"routes":[],"identity_operations":[]}`, false, true},
		{"contact-only daemon", "3.14.0", `{"version":1,"delegated":false,"routes":[]}`, false, false},
		{"unknown API major", "4.0.0", `{"version":1,"routes":[]}`, false, false},
		{"unknown descriptor", "3.16.0", `{"version":2,"delegated":false,"routes":[]}`, false, false},
		{"missing caller mode", "3.16.0", `{"version":1,"routes":[]}`, false, false},
		{"missing routes", "3.16.0", `{"version":1,"delegated":false}`, false, false},
		{"malformed", "3.16.0", `{`, false, false},
		{"oversized", "3.16.0", strings.Repeat(" ", 1<<20) + `{"version":1,"delegated":false,"routes":[]}`, false, false},
		{"duplicate", "3.16.0", `{"version":1,"delegated":false,"routes":[{"operation_id":"same","method":"GET","path":"/api/v1/example"},{"operation_id":"same","method":"GET","path":"/api/v1/example"}]}`, false, false},
		{"invalid method", "3.16.0", `{"version":1,"delegated":false,"routes":[{"operation_id":"example","method":"TRACE","path":"/api/v1/example"}]}`, false, false},
		{"redirect", "3.16.0", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/health" {
					_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"` + tc.version + `"}`))
					return
				}
				if tc.redirect {
					http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
					return
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(peer.Close)
			client, err := daemonclient.New(daemonclient.Config{URL: peer.URL, AllowInsecure: true})
			requirements.NoError(err)
			t.Cleanup(func() {
				assertions.NoError(client.Close())
			})
			descriptor, err := client.MCPCapabilities(t.Context())
			if tc.valid {
				requirements.NoError(err)
				requirements.NotNil(descriptor)
			} else {
				requirements.ErrorIs(err, daemonclient.ErrMCPCapabilitiesUnavailable)
				assertions.Nil(descriptor)
			}
		})
	}
	assert.Zero(t, redirected.Load(), "discovery must never forward credentials through a redirect")
}

func TestMCPDiscoverySchemaProbeDoesNotFollowRedirect(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	var followed atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed.Add(1)
		_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"3.16.0"}`))
	}))
	t.Cleanup(destination.Close)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
			return
		}
		_, _ = w.Write([]byte(`{"version":1,"delegated":false,"routes":[]}`))
	}))
	t.Cleanup(peer.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: peer.URL, AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() {
		assertions.NoError(client.Close())
	})
	descriptor, err := client.MCPCapabilities(t.Context())
	requirements.ErrorIs(err, daemonclient.ErrMCPCapabilitiesUnavailable)
	assertions.Nil(descriptor)
	assertions.Zero(followed.Load(), "the schema probe is part of credential-bearing discovery")
}
