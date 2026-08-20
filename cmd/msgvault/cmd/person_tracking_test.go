package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func runPersonTrackingCommand(
	t *testing.T, template *cobra.Command, jsonOutput bool, args ...string,
) (string, error) {
	t.Helper()
	savedJSON := personJSON
	personJSON = jsonOutput
	t.Cleanup(func() { personJSON = savedJSON })
	var output bytes.Buffer
	command := &cobra.Command{Use: template.Use, Args: template.Args, RunE: template.RunE}
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs(args)
	err := command.Execute()
	return output.String(), err
}

func TestPersonTrackAndUntrackReplaceState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assert.Equal(http.MethodPut, r.Method)
		assert.Equal("/api/v1/people/7/tracking", r.URL.Path)
		var body struct {
			Tracked bool `json:"tracked"`
		}
		if !assert.NoError(json.NewDecoder(r.Body).Decode(&body)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if body.Tracked {
			_, _ = w.Write([]byte(`{"person_id":7,"tracked":true,"tracked_at":"2026-08-19T12:00:00Z"}`))
			return
		}
		_, _ = w.Write([]byte(`{"person_id":7,"tracked":false,"tracked_at":null}`))
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	output, err := runPersonTrackingCommand(t, personTrackCmd, false, "7")
	require.NoError(err)
	assert.Equal("Person 7: tracked\n", output)

	output, err = runPersonTrackingCommand(t, personUntrackCmd, false, "7")
	require.NoError(err)
	assert.Equal("Person 7: untracked\n", output)
	assert.Equal(int32(2), requests.Load())
}

func TestPersonUntrackJSONIncludesNullTrackedAt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"person_id":7,"tracked":false,"tracked_at":null}`))
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	output, err := runPersonTrackingCommand(t, personUntrackCmd, true, "7")
	require.NoError(t, err)
	assert.JSONEq(t, `{"person_id":7,"tracked":false,"tracked_at":null}`, output)
}

func TestPersonTrackRejectsInvalidIDBeforeNetwork(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	_, err := runPersonTrackingCommand(t, personTrackCmd, false, "0")
	require.Error(t, err)
	require.ErrorContains(t, err, "positive integer")
	assert.Zero(t, requests.Load())
}

func TestPersonTrackReturnsStructuredAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"person_profile_not_found","message":"Person profile not found"}`))
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	_, err := runPersonTrackingCommand(t, personTrackCmd, false, "7")
	require.Error(t, err)
	assert.ErrorContains(t, err, "Person profile not found")
}
