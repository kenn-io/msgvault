package personagenda

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kata "go.kenn.io/kata"
	"go.kenn.io/msgvault/internal/taskclient"
)

type integrationPeople map[int64][]string

func (p integrationPeople) ListPersonUIDsContext(_ context.Context, id int64) ([]string, error) {
	return append([]string(nil), p[id]...), nil
}

type allowKataAccess struct{}

func (allowKataAccess) Authorize(context.Context, kata.AccessRequest) (kata.AccessDecision, error) {
	return kata.AccessDecision{TransactionFence: func(context.Context, kata.Transaction) error { return nil }}, nil
}

func TestPersonAgendaAgainstPinnedKataService(t *testing.T) {
	t.Parallel()
	service, err := kata.New(t.Context(), kata.Config{DSN: filepath.Join(t.TempDir(), "kata.db"), Access: allowKataAccess{}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	project, err := service.EnsureProject(t.Context(), kata.ProjectSpec{UID: "01HZNQ7VFPK1XGD8R5MABCD4EX", Name: "msgvault"})
	require.NoError(t, err)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal := kata.Principal{Subject: "msgvault-integration", Actor: "Msgvault integration"}
		service.Handler().ServeHTTP(w, r.WithContext(kata.WithPrincipal(r.Context(), principal)))
	}))
	t.Cleanup(server.Close)
	client, err := taskclient.ConnectKata(t.Context(), taskclient.IntegrationConfig{Enabled: true, Endpoint: server.URL, HTTPClient: server.Client(), DefaultProject: "msgvault"})
	require.NoError(t, err)
	people := integrationPeople{1: {"person-a", "retired-person-a"}, 2: {"person-b"}, 3: {"bounded", "retired-bounded"}, 4: {"empty-person"}}
	agenda := Service{Tasks: client, People: people, Project: "msgvault"}

	t.Run("same todo for different people", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		input := CreateInput{Title: "Review the project proposal", Body: "Read the attached proposal and send comments."}
		first, err := agenda.Create(t.Context(), 1, "same-todo-person-a", input)
		require.NoError(err)
		second, err := agenda.Create(t.Context(), 2, "same-todo-person-b", input)
		require.NoError(err)
		assert.NotEqual(first.UID, second.UID)
		retried, err := agenda.Create(t.Context(), 2, "same-todo-person-b", input)
		require.NoError(err)
		assert.Equal(second.UID, retried.UID)
		_, err = agenda.Unlink(t.Context(), 1, first.UID)
		require.NoError(err)
		_, err = agenda.Unlink(t.Context(), 2, second.UID)
		require.NoError(err)
	})

	t.Run("one person and virtual list", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		input := CreateInput{Title: "Prepare notes", Body: "Synthetic context", PriorityValue: new(int64(1)), Labels: []string{"agenda"}}
		created, err := agenda.Create(t.Context(), 1, "stable-create-key", input)
		require.NoError(err)
		assert.NotEmpty(created.UID)
		assert.Equal("agenda", created.List)
		assert.Equal("Synthetic context", created.Body)
		assert.Equal(int64(1), *created.PriorityValue)
		retried, err := agenda.Create(t.Context(), 1, "stable-create-key", input)
		require.NoError(err)
		assert.Equal(created.UID, retried.UID)

		_, err = agenda.Link(t.Context(), 2, created.Ref, "")
		require.ErrorIs(err, ErrAlreadyLinked)
		direct, err := client.GetTask(t.Context(), "msgvault", created.UID)
		require.NoError(err)
		assert.Equal("person-a", direct.Metadata[PersonMetadataKey])
		assert.Equal("agenda", direct.Metadata[ListMetadataKey])

		_, err = client.MutateMetadataKey(t.Context(), "msgvault", created.UID, "external.key", nil, "kept")
		require.NoError(err)
		moved, err := agenda.Update(t.Context(), 1, created.UID, UpdateInput{List: new("Gift ideas")})
		require.NoError(err)
		assert.Equal("gift ideas", moved.List)
		_, err = agenda.Update(t.Context(), 2, created.UID, UpdateInput{List: new("work")})
		require.ErrorIs(err, taskclient.ErrNotFound)
		_, err = agenda.Unlink(t.Context(), 2, created.UID)
		require.NoError(err)
		direct, err = client.GetTask(t.Context(), "msgvault", created.UID)
		require.NoError(err)
		assert.Equal("person-a", direct.Metadata[PersonMetadataKey])

		_, err = agenda.Unlink(t.Context(), 1, created.UID)
		require.NoError(err)
		direct, err = client.GetTask(t.Context(), "msgvault", created.UID)
		require.NoError(err)
		assert.NotContains(direct.Metadata, PersonMetadataKey)
		assert.Equal("kept", direct.Metadata["external.key"])
		assert.Equal("gift ideas", direct.Metadata[ListMetadataKey])
		_, err = agenda.Link(t.Context(), 2, created.UID, "")
		require.NoError(err)
	})

	t.Run("aliases and server filtering", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		alias, err := client.CreateTask(t.Context(), "msgvault", "alias", taskclient.KataCreate{Title: "Alias task", Metadata: map[string]any{PersonMetadataKey: "retired-person-a", "waiting.for": "someone"}})
		require.NoError(err)
		linked, err := agenda.Link(t.Context(), 1, alias.UID, "")
		require.NoError(err)
		assert.Equal(alias.Revision, linked.Revision)
		for i := range 5 {
			_, err := client.CreateTask(t.Context(), "msgvault", fmt.Sprintf("unrelated-%d", i), taskclient.KataCreate{Title: []string{"Buy a desk", "Schedule lunch", "Renew parking", "Plant tomatoes", "Return package"}[i], Body: strings.Repeat("x", 256*1024), Metadata: map[string]any{PersonMetadataKey: "unrelated-person"}})
			require.NoError(err)
		}
		_, err = client.CreateTask(t.Context(), "msgvault", "malformed-unrelated", taskclient.KataCreate{Title: "Malformed unrelated task", Metadata: map[string]any{PersonMetadataKey: []string{"other-person"}, ListMetadataKey: 7}})
		require.NoError(err)
		closed, err := agenda.Create(t.Context(), 1, "closed", CreateInput{Title: "Cancelled task"})
		require.NoError(err)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, fmt.Sprintf("%s/api/v1/projects/%d/issues/%s/actions/close", server.URL, project.Project.ID, closed.UID), strings.NewReader(`{"reason":"wontfix","message":"Cancelled this synthetic task because it is no longer needed for the example agenda."}`))
		require.NoError(err)
		request.Header.Set("Content-Type", "application/json")
		response, err := server.Client().Do(request)
		require.NoError(err)
		body, err := io.ReadAll(response.Body)
		require.NoError(err)
		require.NoError(response.Body.Close())
		require.Equal(http.StatusOK, response.StatusCode, "%s", body)

		listed, err := agenda.List(t.Context(), 1)
		require.NoError(err)
		require.Len(listed.Items, 1)
		assert.Equal(alias.UID, listed.Items[0].UID)
		assert.Equal("msgvault#"+alias.Ref, listed.Items[0].QualifiedRef)
		assert.Equal(StateOpen, listed.Items[0].State)
		assert.Equal("person-a", listed.PersonUID)
		assert.Equal([]string{"person-a", "retired-person-a"}, listed.PersonUIDs)
		assert.False(listed.Truncated)
		_, err = agenda.Unlink(t.Context(), 1, alias.UID)
		require.NoError(err)
		direct, err := client.GetTask(t.Context(), "msgvault", alias.UID)
		require.NoError(err)
		assert.NotContains(direct.Metadata, PersonMetadataKey)
		assert.Equal("someone", direct.Metadata["waiting.for"])
	})

	t.Run("malformed owned metadata is not overwritten", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		for i, metadata := range []map[string]any{
			{PersonMetadataKey: []string{"person-a"}},
			{PersonMetadataKey: ""},
			{PersonMetadataKey: 7},
			{PersonMetadataKey: "person-a", ListMetadataKey: 7},
			{PersonMetadataKey: "person-a", ListMetadataKey: strings.Repeat("x", 81)},
		} {
			task, err := client.CreateTask(t.Context(), "msgvault", fmt.Sprintf("malformed-%d", i), taskclient.KataCreate{Title: fmt.Sprintf("Malformed owned metadata %d", i), Metadata: metadata})
			require.NoError(err)
			want := ErrUnsafePersonMetadata
			if i >= 3 {
				want = ErrUnsafeListMetadata
			}
			_, err = agenda.Link(t.Context(), 1, task.UID, "work")
			require.ErrorIs(err, want)
			_, err = agenda.Unlink(t.Context(), 1, task.UID)
			require.ErrorIs(err, want)
			_, err = agenda.Update(t.Context(), 1, task.UID, UpdateInput{List: new("work")})
			require.ErrorIs(err, want)
			after, err := client.GetTask(t.Context(), "msgvault", task.UID)
			require.NoError(err)
			assert.Equal(task.Revision, after.Revision)
			assert.Equal(task.Metadata, after.Metadata)
		}
	})

	t.Run("conditional writes preserve unrelated changes", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		task, err := client.CreateTask(t.Context(), "msgvault", "guards", taskclient.KataCreate{Title: "Guarded link"})
		require.NoError(err)
		_, err = client.MutateMetadataKey(t.Context(), "msgvault", task.UID, "external", nil, "kept")
		require.NoError(err)
		_, err = client.MutateMetadata(t.Context(), "msgvault", task.UID, task.Revision, map[string]any{PersonMetadataKey: "person-a", ListMetadataKey: "work"})
		require.ErrorIs(err, taskclient.ErrConflict)
		linked, err := client.MutateMetadataKey(t.Context(), "msgvault", task.UID, PersonMetadataKey, nil, "person-b")
		require.NoError(err)
		assert.Equal("kept", linked.Metadata["external"])
		assert.NotContains(linked.Metadata, ListMetadataKey)
		_, err = client.MutateMetadataKey(t.Context(), "msgvault", task.UID, PersonMetadataKey, nil, "person-a")
		require.ErrorIs(err, taskclient.ErrConflict)
		_, err = client.MutateMetadataKey(t.Context(), "msgvault", task.UID, PersonMetadataKey, "person-a", nil)
		require.ErrorIs(err, taskclient.ErrConflict)
		after, err := client.GetTask(t.Context(), "msgvault", task.UID)
		require.NoError(err)
		assert.Equal("person-b", after.Metadata[PersonMetadataKey])
	})

	t.Run("bounded across canonical and retired UID", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		for i := range 101 {
			uid := "bounded"
			if i >= 60 {
				uid = "retired-bounded"
			}
			_, err := client.CreateTask(t.Context(), "msgvault", fmt.Sprintf("bounded-%d", i), taskclient.KataCreate{Title: fmt.Sprintf("Task %03d", i), Metadata: map[string]any{PersonMetadataKey: uid}})
			require.NoError(err)
		}
		listed, err := agenda.List(t.Context(), 3)
		require.NoError(err)
		assert.Len(listed.Items, 100)
		assert.True(listed.Truncated)
		assert.Equal([]string{"bounded", "retired-bounded"}, listed.PersonUIDs)
		empty, err := agenda.List(t.Context(), 4)
		require.NoError(err)
		assert.NotNil(empty.Items)
		assert.Empty(empty.Items)
		assert.False(empty.Truncated)
	})
}
