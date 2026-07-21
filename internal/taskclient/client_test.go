package taskclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientProtocolOperations(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	type observedRequest struct {
		Method         string
		Path           string
		Query          string
		Authorization  string
		IdempotencyKey string
		IfMatch        string
	}
	requests := make(chan observedRequest, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- observedRequest{
			Method:         r.Method,
			Path:           r.URL.Path,
			Query:          r.URL.RawQuery,
			Authorization:  r.Header.Get("Authorization"),
			IdempotencyKey: r.Header.Get("Idempotency-Key"),
			IfMatch:        r.Header.Get("If-Match"),
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/test-project":
			w.Header().Set("ETag", `"project-revision"`)
			writeTestJSON(t, w, Project{ID: "project-test", Name: "test-project"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/test-project/tasks":
			var create TaskCreate
			assertions.NoError(json.NewDecoder(r.Body).Decode(&create))
			assertions.Equal("Synthetic task", create.Title)
			w.WriteHeader(http.StatusCreated)
			writeTestJSON(t, w, Task{ID: "task-test", Project: "test-project", Title: create.Title, Revision: `"r1"`})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/test-project/tasks":
			writeTestJSON(t, w, TaskList{Tasks: []Task{{ID: "task-test", Project: "test-project", Title: "Synthetic task", Revision: `"r1"`}}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/test-project/tasks/task-test":
			w.Header().Set("ETag", `"r1"`)
			writeTestJSON(t, w, Task{ID: "task-test", Project: "test-project", Title: "Synthetic task", Revision: `"r1"`})
		case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/projects/test-project/tasks/task-test/metadata":
			w.Header().Set("ETag", `"r2"`)
			writeTestJSON(t, w, Task{ID: "task-test", Project: "test-project", Title: "Synthetic task", Revision: `"r2"`})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client := newLoopbackClient(t, server.URL, "operation-test-key", nil)
	ctx := context.Background()

	project, err := client.ResolveProject(ctx, "test-project")
	requirements.NoError(err)
	assertions.Equal("project-test", project.ID)
	request := <-requests
	assertions.Equal("Bearer operation-test-key", request.Authorization)

	created, err := client.CreateTask(ctx, "test-project", "retry-stable-key", TaskCreate{Title: "Synthetic task"})
	requirements.NoError(err)
	assertions.Equal("task-test", created.ID)
	request = <-requests
	assertions.Equal("retry-stable-key", request.IdempotencyKey)

	searched, err := client.SearchTasks(ctx, "test-project", "Synthetic", 25)
	requirements.NoError(err)
	assertions.Len(searched.Tasks, 1)
	request = <-requests
	assertions.Contains(request.Query, "q=Synthetic")
	assertions.Contains(request.Query, "limit=25")

	listed, err := client.ListTasks(ctx, "test-project", 10, "cursor-test")
	requirements.NoError(err)
	assertions.Len(listed.Tasks, 1)
	request = <-requests
	assertions.Contains(request.Query, "limit=10")
	assertions.Contains(request.Query, "cursor=cursor-test")

	read, err := client.GetTask(ctx, "test-project", "task-test")
	requirements.NoError(err)
	assertions.Equal(`"r1"`, read.Revision)
	<-requests

	mutated, err := client.MutateMetadata(ctx, "test-project", "task-test", `"r1"`, map[string]any{"mail_links": []any{}})
	requirements.NoError(err)
	assertions.Equal(`"r2"`, mutated.Revision)
	request = <-requests
	assertions.Equal(`"r1"`, request.IfMatch)
}

func TestClientRequiresMutationSafetyHeaders(t *testing.T) {
	client, err := New(ClientOptions{Endpoint: "https://tasks.example.com", APIKey: "test-key"})
	require.NoError(t, err)

	_, err = client.CreateTask(context.Background(), "test-project", "", TaskCreate{Title: "Synthetic task"})
	require.ErrorIs(t, err, ErrIdempotencyKeyRequired)

	_, err = client.MutateMetadata(context.Background(), "test-project", "task-test", "", map[string]any{})
	require.ErrorIs(t, err, ErrRevisionRequired)
}

func TestClientValidatesResponseShapesAndAuthentication(t *testing.T) {
	t.Run("authentication response", func(t *testing.T) {
		server := capabilityServer(t, Capabilities{}, http.StatusUnauthorized)
		client := newLoopbackClient(t, server.URL, "rejected-test-key", nil)

		_, err := client.Capabilities(context.Background())

		assert.ErrorIs(t, err, ErrAuthenticationRequired)
	})

	t.Run("missing task identity", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeTestJSON(t, w, Task{Title: "Missing identity"})
		}))
		t.Cleanup(server.Close)
		client := newLoopbackClient(t, server.URL, "shape-test-key", nil)

		_, err := client.GetTask(context.Background(), "test-project", "task-test")

		assert.ErrorIs(t, err, ErrInvalidResponse)
	})
}

func TestClientUsesETagAsTaskRevision(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"etag-revision"`)
		writeTestJSON(t, w, Task{ID: "task-test", Project: "test-project", Title: "Synthetic task"})
	}))
	t.Cleanup(server.Close)
	client := newLoopbackClient(t, server.URL, "etag-test-key", nil)

	task, err := client.GetTask(context.Background(), "test-project", "task-test")

	require.NoError(t, err)
	assert.Equal(t, `"etag-revision"`, task.Revision)
}

func TestCapabilitiesClassifyReachedHTTPResponses(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		want       error
	}{
		{name: "no content", statusCode: http.StatusNoContent, want: ErrIncompatible},
		{name: "non-enumerated client response", statusCode: http.StatusTeapot, want: ErrIncompatible},
		{name: "method not allowed", statusCode: http.StatusMethodNotAllowed, want: ErrIncompatible},
		{name: "not implemented", statusCode: http.StatusNotImplemented, want: ErrIncompatible},
		{name: "conflict", statusCode: http.StatusConflict, want: ErrIncompatible},
		{name: "precondition failed", statusCode: http.StatusPreconditionFailed, want: ErrIncompatible},
		{name: "unauthorized", statusCode: http.StatusUnauthorized, want: ErrAuthenticationRequired},
		{name: "forbidden", statusCode: http.StatusForbidden, want: ErrAuthenticationRequired},
		{name: "internal server error", statusCode: http.StatusInternalServerError, want: ErrUnreachable},
		{name: "service unavailable", statusCode: http.StatusServiceUnavailable, want: ErrUnreachable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := capabilityServer(t, Capabilities{}, test.statusCode)
			client := newLoopbackClient(t, server.URL, "protocol-test-key", nil)

			_, err := client.Capabilities(context.Background())

			require.ErrorIs(t, err, test.want)
		})
	}

	t.Run("unreachable network", func(t *testing.T) {
		server := capabilityServer(t, compatibleCapabilities(), http.StatusOK)
		endpoint := server.URL
		server.Close()
		client := newLoopbackClient(t, endpoint, "protocol-test-key", nil)

		_, err := client.Capabilities(context.Background())

		require.ErrorIs(t, err, ErrUnreachable)
	})
}

func TestTaskOperationsPreserveConflictResponses(t *testing.T) {
	for _, statusCode := range []int{http.StatusConflict, http.StatusPreconditionFailed} {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(statusCode)
			}))
			t.Cleanup(server.Close)
			client := newLoopbackClient(t, server.URL, "conflict-test-key", nil)

			_, err := client.MutateMetadata(context.Background(), "test-project", "task-test", `"r1"`, map[string]any{})

			require.ErrorIs(t, err, ErrConflict)
		})
	}
}

func TestTaskOperationsClassifyValidationResponsesAsRequestRejections(t *testing.T) {
	for _, statusCode := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity} {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(statusCode)
			}))
			t.Cleanup(server.Close)
			client := newLoopbackClient(t, server.URL, "validation-test-key", nil)

			_, err := client.CreateTask(context.Background(), "test-project", "request-id", TaskCreate{Title: "Synthetic task"})

			require.ErrorIs(t, err, ErrRequestRejected)
			assert.NotErrorIs(t, err, ErrIncompatible)
		})
	}
}

func TestResolveProjectBindsConfiguredIdentityAndRevision(t *testing.T) {
	t.Run("unrelated project", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeTestJSON(t, w, Project{ID: "project-other-id", Name: "other-project", Revision: `"p1"`})
		}))
		t.Cleanup(server.Close)
		client := newLoopbackClient(t, server.URL, "project-test-key", nil)

		_, err := client.ResolveProject(context.Background(), "test-project")

		require.ErrorIs(t, err, ErrWrongProject)
	})

	t.Run("missing revision", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeTestJSON(t, w, Project{ID: "project-test-id", Name: "test-project"})
		}))
		t.Cleanup(server.Close)
		client := newLoopbackClient(t, server.URL, "project-test-key", nil)

		_, err := client.ResolveProject(context.Background(), "test-project")

		require.ErrorIs(t, err, ErrInvalidResponse)
	})
}
