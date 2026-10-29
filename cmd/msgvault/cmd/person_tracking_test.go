package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func runPersonTrackingCommand(
	ctx context.Context, t *testing.T, template *cobra.Command, jsonOutput bool, args ...string,
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
	command.SetContext(ctx)
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
	testCtx := withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})
	output, err := runPersonTrackingCommand(testCtx, t, personTrackCmd, false, "7")
	require.NoError(err)
	assert.Equal("Person 7: tracked\n", output)

	output, err = runPersonTrackingCommand(testCtx, t, personUntrackCmd, false, "7")
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
	testCtx := withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})
	output, err := runPersonTrackingCommand(testCtx, t, personUntrackCmd, true, "7")
	require.NoError(t, err)
	assert.JSONEq(t, `{"person_id":7,"tracked":false,"tracked_at":null}`, output)
}

func TestPersonTrackRejectsInvalidIDBeforeNetwork(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	t.Cleanup(server.Close)
	testCtx := withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})
	_, err := runPersonTrackingCommand(testCtx, t, personTrackCmd, false, "0")
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
	testCtx := withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})
	_, err := runPersonTrackingCommand(testCtx, t, personTrackCmd, false, "7")
	require.Error(t, err)
	assert.ErrorContains(t, err, "Person profile not found")
}

func TestPersonTrackSeveralPeopleInOrderAndStopsAtFirstRejection(t *testing.T) {
	assert := assert.New(t)
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/people/7/tracking":
			_, _ = w.Write([]byte(`{"person_id":7,"tracked":true,"tracked_at":"2026-08-19T12:00:00Z"}`))
		case "/api/v1/people/12/tracking":
			_, _ = w.Write([]byte(`{"person_id":12,"tracked":true,"tracked_at":"2026-08-19T12:00:01Z"}` + "\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"person_profile_not_found","message":"Person profile not found"}`))
		}
	}))
	t.Cleanup(server.Close)
	testCtx := withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})

	output, err := runPersonTrackingCommand(testCtx, t, personTrackCmd, false, "7", "12")
	require.NoError(t, err)
	assert.Equal("Person 7: tracked\nPerson 12: tracked\n", output)

	output, err = runPersonTrackingCommand(testCtx, t, personTrackCmd, true, "7", "12")
	require.NoError(t, err)
	lines := strings.Split(output, "\n")
	require.Len(t, lines, 3, "one JSON object per line and a trailing newline")
	assert.JSONEq(`{"person_id":7,"tracked":true,"tracked_at":"2026-08-19T12:00:00Z"}`, lines[0])
	assert.JSONEq(`{"person_id":12,"tracked":true,"tracked_at":"2026-08-19T12:00:01Z"}`, lines[1])
	assert.Empty(lines[2])

	paths = nil
	output, err = runPersonTrackingCommand(testCtx, t, personTrackCmd, false, "7", "99", "12")
	require.ErrorContains(t, err, "track person 99")
	require.ErrorContains(t, err, "Person profile not found")
	// The helper shares one buffer for output and Cobra's error and usage text.
	assert.Contains(output, "Person 7: tracked\n")
	assert.Equal([]string{"/api/v1/people/7/tracking", "/api/v1/people/99/tracking"}, paths)
}

func TestPersonTrackChecksEveryIDBeforeNetwork(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	t.Cleanup(server.Close)
	testCtx := withStoreResolverConfig(t, &config.Config{
		Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true},
	})
	_, err := runPersonTrackingCommand(testCtx, t, personTrackCmd, false, "7", "x")
	require.ErrorContains(t, err, "positive integer")
	assert.Zero(t, requests.Load())
}
