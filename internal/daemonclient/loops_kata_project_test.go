package daemonclient_test

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
)

func TestOpenLoopKataProjectReadsDaemonSettings(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		status           int
	}{
		{"configured", `{"settings":[{"key":"integrations.kata.default_project","value":{"string":"remote-project"}}]}`, "remote-project", http.StatusOK},
		{"missing", `{"settings":[]}`, "", http.StatusOK},
		{"empty", `{"settings":[{"key":"integrations.kata.default_project","value":{"string":""}}]}`, "", http.StatusOK},
		{"wrong type", `{"settings":[{"key":"integrations.kata.default_project","value":{"integer":7}}]}`, "", http.StatusOK},
		{"pending restart", `{"pending_restart":true,"settings":[{"key":"integrations.kata.default_project","value":{"string":"remote-project"}}]}`, "", http.StatusOK},
		{"unavailable", `{"error":"unavailable"}`, "", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/v1/settings", r.URL.Path)
				assert.Equal(t, http.MethodGet, r.Method)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(server.Close)
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, Context: t.Context()})
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, client.Close()) })
			project, err := client.OpenLoopKataProject(t.Context())
			assert.Equal(t, tc.want, project)
			if tc.want == "" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
func TestOpenLoopKataProjectFollowsDaemonRestart(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		project := "first-project"
		if calls.Add(1) > 1 {
			project = "second-project"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"settings":[{"key":"integrations.kata.default_project","value":{"string":"` + project + `"}}]}`))
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, Context: t.Context()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	first, err := client.OpenLoopKataProject(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "first-project", first)
	second, err := client.OpenLoopKataProject(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "second-project", second)
	assert.Equal(t, int64(2), calls.Load())
}
