package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
)

// TestMain disables the remote API schema probe for the package: CLI tests
// stub a remote daemon with single-route handlers that do not serve
// /api/v1/health. The probe itself is exercised by the
// TestOpenRemoteStore*APISchema* tests, which re-enable it per test.
func TestMain(m *testing.M) {
	remoteAPISchemaCheckEnabled = false
	os.Exit(m.Run())
}

func remoteSchemaStub(t *testing.T, health func(w http.ResponseWriter)) *atomic.Int32 {
	t.Helper()
	var healthRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			healthRequests.Add(1)
			health(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"persons": []any{}})
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})
	remoteAPISchemaCheckEnabled = true
	t.Cleanup(func() { remoteAPISchemaCheckEnabled = false })
	return &healthRequests
}

func TestOpenRemoteStoreVerifiesAPISchemaOnMatchingMajor(t *testing.T) {
	require := require.New(t)
	healthRequests := remoteSchemaStub(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "api_schema_version": api.APISchemaVersion,
		})
	})

	client, info, err := OpenHTTPStore(t.Context())
	require.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	assert.Equal(t, HTTPStoreConfiguredRemote, info.Kind)
	assert.Equal(t, int32(1), healthRequests.Load())
}

func TestOpenRemoteStoreRejectsAPISchemaMajorMismatch(t *testing.T) {
	require := require.New(t)
	_ = remoteSchemaStub(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "api_schema_version": "1.44.0",
		})
	})

	_, _, err := OpenHTTPStore(t.Context())
	require.ErrorContains(err, `daemon API schema version "1.44.0" is incompatible`)
}

func TestOpenRemoteStoreRejectsOlderMinorSchema(t *testing.T) {
	require := require.New(t)
	_ = remoteSchemaStub(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "api_schema_version": "2.13.0",
		})
	})

	client, _, err := OpenHTTPStore(t.Context())
	if client != nil {
		t.Cleanup(func() { _ = client.Close() })
	}
	require.ErrorContains(err, "requires API schema 2.14.0 or newer")
}

func TestOpenRemoteStoreAcceptsCompatiblePreviousMinorSchema(t *testing.T) {
	healthRequests := remoteSchemaStub(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "api_schema_version": "2.15.0",
		})
	})

	client, _, err := OpenHTTPStore(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	assert.Equal(t, int32(1), healthRequests.Load())
}

func TestOpenRemoteStoreRejectsDaemonWithoutSchemaVersion(t *testing.T) {
	require := require.New(t)
	_ = remoteSchemaStub(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
	})

	_, _, err := OpenHTTPStore(t.Context())
	require.ErrorContains(err, "does not report an API schema version")
	require.ErrorContains(err, "upgrade the daemon")
}

func TestOpenRemoteStoreSurfacesHealthProbeFailure(t *testing.T) {
	require := require.New(t)
	_ = remoteSchemaStub(t, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	_, _, err := OpenHTTPStore(t.Context())
	require.ErrorContains(err, "verify remote daemon API schema version")
}

func TestDaemonRuntimeCompatibilityRejectsLegacyRecordWithoutSchemaVersion(t *testing.T) {
	require := require.New(t)

	current := &DaemonRuntime{API: daemonAPIVersion, APISchemaVersion: api.APISchemaVersion}
	require.NoError(daemonRuntimeCompatibilityError(current))

	legacy := &DaemonRuntime{API: daemonAPIVersion}
	err := daemonRuntimeCompatibilityError(legacy)
	require.ErrorContains(err, "does not report an API schema version")
	require.ErrorContains(err, "upgrade the daemon")

	previousMajor := &DaemonRuntime{API: daemonAPIVersion, APISchemaVersion: "1.44.0"}
	require.ErrorContains(daemonRuntimeCompatibilityError(previousMajor),
		`daemon API schema version "1.44.0" is incompatible`)

	previousSupportedMinor := &DaemonRuntime{API: daemonAPIVersion, APISchemaVersion: "2.15.0"}
	require.NoError(daemonRuntimeCompatibilityError(previousSupportedMinor))

	previousMinor := &DaemonRuntime{API: daemonAPIVersion, APISchemaVersion: "2.13.0"}
	require.ErrorContains(daemonRuntimeCompatibilityError(previousMinor),
		"requires API schema 2.14.0 or newer")
}

// agentDelegatedSchemaStub sets up a stub HTTP server that serves the health
// endpoint for openAgentDelegatedStore, enables the schema check, and restores
// state on cleanup.
func agentDelegatedSchemaStub(t *testing.T, sessionResponse string, health func(w http.ResponseWriter)) *atomic.Int32 {
	t.Helper()
	var healthRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			healthRequests.Add(1)
			health(w)
			return
		}
		if r.URL.Path == "/api/session" && sessionResponse != "" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(sessionResponse))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)

	// Set global agent-mode flags and restore on cleanup.
	oldURL := agentURL
	oldFile := agentTokenFile
	oldInsecure := agentAllowInsecure
	agentURL = server.URL
	agentAllowInsecure = true

	// Write a token file with a fake secret.
	tokenFile, err := os.CreateTemp(t.TempDir(), "agent-token-*")
	require.NoError(t, err)
	_, _ = tokenFile.WriteString("mva1_fakesecretfortesting")
	_ = tokenFile.Close()
	agentTokenFile = tokenFile.Name()

	t.Cleanup(func() {
		agentURL = oldURL
		agentTokenFile = oldFile
		agentAllowInsecure = oldInsecure
	})

	remoteAPISchemaCheckEnabled = true
	t.Cleanup(func() { remoteAPISchemaCheckEnabled = false })
	return &healthRequests
}

// TestOpenAgentDelegatedStoreVerifiesAPISchema verifies that openAgentDelegatedStore
// calls verifyRemoteAPISchemaVersion when the probe is enabled, and accepts a
// matching schema version.
func TestOpenAgentDelegatedStoreVerifiesAPISchema(t *testing.T) {
	require := require.New(t)
	healthRequests := agentDelegatedSchemaStub(t, `{"auth_mode":"delegated"}`, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "api_schema_version": api.APISchemaVersion,
		})
	})

	client, info, err := openAgentDelegatedStore(t.Context())
	require.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	assert.Equal(t, HTTPStoreAgentDelegated, info.Kind)
	assert.Equal(t, int32(1), healthRequests.Load(), "schema check must hit /api/v1/health")
}

// TestOpenAgentDelegatedStoreRejectsMismatchedSchema verifies that
// openAgentDelegatedStore rejects a daemon with an incompatible API schema.
func TestOpenAgentDelegatedStoreRejectsMismatchedSchema(t *testing.T) {
	require := require.New(t)
	_ = agentDelegatedSchemaStub(t, "", func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "api_schema_version": "1.0.0",
		})
	})

	_, _, err := openAgentDelegatedStore(t.Context())
	require.ErrorContains(err, "incompatible")
}

func TestOpenAgentDelegatedStoreReportsAuthenticationFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	agentDelegatedSchemaStub(t, "", func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized","message":"Invalid or missing API key"}`))
	})

	_, _, err := openAgentDelegatedStore(t.Context())
	require.ErrorContains(err, "agent authentication failed")
	assert.NotContains(err.Error(), "schema version")
	var apiErr *daemonclient.APIError
	require.ErrorAs(err, &apiErr)
	assert.Equal(http.StatusUnauthorized, apiErr.Status)
}

func TestOpenAgentDelegatedStoreRequiresDelegatedAuthentication(t *testing.T) {
	for _, tc := range []struct{ name, session string }{
		{"pre-delegation keyless daemon", `{"auth_mode":"loopback"}`},
		{"owner API key", `{"auth_mode":"api_key"}`},
		{"owner session", `{"auth_mode":"session"}`},
		{"unauthenticated", `{"auth_mode":"required"}`},
		{"missing auth mode", `{}`},
		{"missing session endpoint", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agentDelegatedSchemaStub(t, tc.session, func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"status": "ok", "api_schema_version": api.APISchemaVersion,
				})
			})

			client, _, err := OpenHTTPStore(t.Context())
			if client != nil {
				t.Cleanup(func() { _ = client.Close() })
			}
			require.ErrorContains(t, err, "verify agent authentication")
			assert.Nil(t, client, "no client may be returned without delegated authentication")
		})
	}
}
