package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestPersonAgendaCommandsUseDaemonKataContract(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	type request struct {
		method         string
		path           string
		idempotencyKey string
		body           map[string]any
	}
	requests := make([]request, 0, 5)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entry := request{method: r.Method, path: r.URL.Path, idempotencyKey: r.Header.Get("Idempotency-Key")}
		if r.Body != nil && r.Method != http.MethodGet && r.Method != http.MethodDelete {
			assert.NoError(json.NewDecoder(r.Body).Decode(&entry.body))
		}
		requests = append(requests, entry)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			assert.NoError(json.NewEncoder(w).Encode(map[string]any{"project": "msgvault", "items": []any{agendaCLIItem("task-1", "Ask")}}))
			return
		}
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
		}
		assert.NoError(json.NewEncoder(w).Encode(map[string]any{"item": agendaCLIItem("task-1", "Ask")}))
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})

	output, err := executePersonAgendaCommand(t, "list", "7", "--json")
	require.NoError(err)
	assert.Contains(output, `"project":"msgvault"`)

	output, err = executePersonAgendaCommand(t, "create", "7", "--title", "Ask", "--body", "Context", "--priority", "0", "--idempotency-key", "retry-1", "--json")
	require.NoError(err)
	assert.Contains(output, `"ref":"task-1"`)

	_, err = executePersonAgendaCommand(t, "link", "7", "task-1", "--list", "gift ideas")
	require.NoError(err)
	_, err = executePersonAgendaCommand(t, "edit", "7", "task-1", "--list", "gift ideas")
	require.NoError(err)
	_, err = executePersonAgendaCommand(t, "unlink", "7", "task-1")
	require.NoError(err)

	require.Len(requests, 5)
	assert.Equal(request{method: http.MethodGet, path: "/api/v1/people/7/agenda"}, requests[0])
	assert.Equal("retry-1", requests[1].idempotencyKey)
	assert.Equal(map[string]any{"title": "Ask", "body": "Context", "priority": float64(0)}, requests[1].body)
	assert.Equal("/api/v1/people/7/agenda/links", requests[2].path)
	assert.Equal(map[string]any{"ref": "task-1", "list": "gift ideas"}, requests[2].body)
	assert.Equal(http.MethodPatch, requests[3].method)
	assert.Equal(map[string]any{"list": "gift ideas"}, requests[3].body)
	assert.Equal(http.MethodDelete, requests[4].method)
}

func TestPersonAgendaCreateGeneratesRetryKey(t *testing.T) {
	var key string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key = r.Header.Get("Idempotency-Key")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"item": agendaCLIItem("task-1", "Ask")}))
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})
	output, err := executePersonAgendaCommand(t, "create", "7", "--title", "Ask")
	require.NoError(t, err)
	require.NotEmpty(t, key)
	assert.Contains(t, output, key, "the generated key must be available for a retry")
}

func executePersonAgendaCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()
	command := newPersonAgendaCommand()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs(args)
	err := command.Execute()
	return output.String(), err
}

func agendaCLIItem(ref, title string) map[string]any {
	return map[string]any{
		"uid": "01TASK", "ref": ref, "qualified_ref": "msgvault#" + ref, "project": "msgvault",
		"title": title, "revision": "1", "list": "agenda", "status": "open", "state": "open",
	}
}

func TestPersonAgendaListTextShowsVirtualHierarchy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"project":"msgvault","truncated":true,"items":[{"uid":"01TASK","ref":"task-1","qualified_ref":"msgvault#task-1","project":"msgvault","title":"Ask","revision":"1","list":"gift ideas","status":"open","state":"open"}]}`))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})

	output, err := executePersonAgendaCommand(t, "list", "7")
	require.NoError(t, err)
	assert.True(t, strings.Contains(output, "gift ideas") && strings.Contains(output, "task-1") && strings.Contains(output, "Ask"), output)
	assert.Contains(t, output, "More open tasks are linked to this person.")
}
