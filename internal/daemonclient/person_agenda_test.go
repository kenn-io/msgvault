package daemonclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestPersonAgendaMethodsUseGeneratedDaemonAPI(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	var idempotencyKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/people/7/agenda":
			assert.NoError(json.NewEncoder(w).Encode(map[string]any{"project": "msgvault", "items": []any{}}))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/people/7/agenda":
			idempotencyKey = r.Header.Get("Idempotency-Key")
			w.WriteHeader(http.StatusCreated)
			assert.NoError(json.NewEncoder(w).Encode(agendaMutationFixture("created")))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/people/7/agenda/links":
			w.WriteHeader(http.StatusCreated)
			assert.NoError(json.NewEncoder(w).Encode(agendaMutationFixture("linked")))
		case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/people/7/agenda/task-1":
			assert.NoError(json.NewEncoder(w).Encode(agendaMutationFixture("updated")))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/people/7/agenda/task-1":
			assert.NoError(json.NewEncoder(w).Encode(agendaMutationFixture("unlinked")))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{URL: server.URL, AllowInsecure: true})
	require.NoError(err)

	listed, err := client.ListPersonAgenda(t.Context(), 7)
	require.NoError(err)
	assert.Equal("msgvault", listed.Project)
	assert.Empty(listed.Items)

	created, err := client.CreatePersonAgendaItem(t.Context(), 7, "request-1", generated.PersonAgendaCreateRequest{Title: "created"})
	require.NoError(err)
	assert.Equal("request-1", idempotencyKey)
	assert.Equal("created", created.Title)

	linked, err := client.LinkPersonAgendaItem(t.Context(), 7, generated.PersonAgendaLinkRequest{Ref: "task-1"})
	require.NoError(err)
	assert.Equal("linked", linked.Title)

	updatedList := "gift ideas"
	updated, err := client.UpdatePersonAgendaItem(t.Context(), 7, "task-1", generated.PersonAgendaUpdateRequest{List: updatedList})
	require.NoError(err)
	assert.Equal("updated", updated.Title)

	unlinked, err := client.UnlinkPersonAgendaItem(t.Context(), 7, "task-1")
	require.NoError(err)
	assert.Equal("unlinked", unlinked.Title)
}

func agendaMutationFixture(title string) map[string]any {
	return map[string]any{"item": map[string]any{
		"uid": "01TASK", "ref": "task-1", "qualified_ref": "msgvault#task-1", "project": "msgvault",
		"title": title, "revision": "1", "list": "agenda", "status": "open", "state": "open",
	}}
}
