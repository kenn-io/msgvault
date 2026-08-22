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
}
