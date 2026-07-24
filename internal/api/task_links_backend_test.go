package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/taskclient"
	"go.kenn.io/msgvault/internal/tasklinks"
)

type backendListClient struct{ tasks []taskclient.Task }

func (c backendListClient) ListTasks(context.Context, string, int, string) (taskclient.TaskList, error) {
	return taskclient.TaskList{Tasks: c.tasks}, nil
}

func readyTaskBackendServer(t *testing.T, tasks []taskclient.Task) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/capabilities":
			assert.NoError(t, json.NewEncoder(w).Encode(taskclient.Capabilities{
				ProtocolVersion: taskclient.ProtocolVersion, RevisionReads: true, ConditionalMutation: true,
				ConflictResponses: true, IdempotentCreation: true, ProjectOperations: true, MetadataOperations: true,
			}))
		case "/api/v1/projects/project":
			assert.NoError(t, json.NewEncoder(w).Encode(taskclient.Project{ID: "project-id", Name: "project", Revision: "project-r1"}))
		case "/api/v1/projects/project/tasks":
			assert.NoError(t, json.NewEncoder(w).Encode(taskclient.TaskList{Tasks: tasks}))
		default:
			http.NotFound(w, r)
		}
	}))
}

type countingTaskServer struct {
	server    *httptest.Server
	listCalls atomic.Int64
	mu        sync.Mutex
	revision  string
}

func (s *countingTaskServer) setRevision(revision string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revision = revision
}

func (s *countingTaskServer) projectRevision() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revision
}

func newCountingTaskServer(t *testing.T, tasks []taskclient.Task) *countingTaskServer {
	t.Helper()
	s := &countingTaskServer{revision: "project-r1"}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/capabilities":
			assert.NoError(t, json.NewEncoder(w).Encode(taskclient.Capabilities{
				ProtocolVersion: taskclient.ProtocolVersion, RevisionReads: true, ConditionalMutation: true,
				ConflictResponses: true, IdempotentCreation: true, ProjectOperations: true, MetadataOperations: true,
			}))
		case "/api/v1/projects/project":
			assert.NoError(t, json.NewEncoder(w).Encode(taskclient.Project{ID: "project-id", Name: "project", Revision: s.projectRevision()}))
		case "/api/v1/projects/project/tasks":
			s.listCalls.Add(1)
			assert.NoError(t, json.NewEncoder(w).Encode(taskclient.TaskList{Tasks: tasks}))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.server.Close)
	return s
}

func TestTaskLinkBackendReusesFreshReverseIndex(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	identity := tasklinks.MessageIdentity{ArchiveUID: "archive-a", ArchiveRevision: "1", MessageID: 42,
		SourceType: "gmail", SourceIdentifier: "archive@example.com", SourceMessageID: "source-42"}
	linked := taskclient.Task{ID: "task-1", Project: "project", Title: "Linked title", Revision: "r1",
		Metadata: tasklinks.MetadataWithLink(nil, tasklinks.NewMailLink(identity, time.Now()))}
	server := newCountingTaskServer(t, []taskclient.Task{linked})
	now := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	index := tasklinks.NewIndexWithOptions(filepath.Join(t.TempDir(), "cache", "tasks.json"),
		tasklinks.IndexOptions{Now: func() time.Time { return now }})
	backend := &taskLinkBackend{config: config.TaskIntegrationConfig{Enabled: true, Endpoint: server.server.URL,
		APIKey: "test-key", DefaultProject: "project"}, index: index}

	first := backend.Lookup(context.Background(), identity)
	requirements.Len(first.Tasks, 1)
	assertions.EqualValues(1, server.listCalls.Load(), "first lookup scans the remote tasks")

	second := backend.Lookup(context.Background(), identity)
	requirements.Len(second.Tasks, 1)
	assertions.EqualValues(1, server.listCalls.Load(), "fresh index serves the second lookup without a remote scan")
	assertions.Equal(tasklinks.StateReady, second.State)

	server.setRevision("project-r2")
	third := backend.Lookup(context.Background(), identity)
	requirements.Len(third.Tasks, 1)
	assertions.EqualValues(2, server.listCalls.Load(), "remote project revision change forces a rebuild")

	now = now.Add(tasklinks.DefaultFreshFor + time.Second)
	fourth := backend.Lookup(context.Background(), identity)
	requirements.Len(fourth.Tasks, 1)
	assertions.EqualValues(3, server.listCalls.Load(), "expired freshness window forces a rebuild")
}

func TestTaskLinkBackendConcurrentLookupsShareOneRebuild(t *testing.T) {
	assertions := assert.New(t)
	identity := tasklinks.MessageIdentity{ArchiveUID: "archive-a", ArchiveRevision: "1", MessageID: 42,
		SourceType: "gmail", SourceIdentifier: "archive@example.com", SourceMessageID: "source-42"}
	linked := taskclient.Task{ID: "task-1", Project: "project", Title: "Linked title", Revision: "r1",
		Metadata: tasklinks.MetadataWithLink(nil, tasklinks.NewMailLink(identity, time.Now()))}
	server := newCountingTaskServer(t, []taskclient.Task{linked})
	index := tasklinks.NewIndex(filepath.Join(t.TempDir(), "cache", "tasks.json"), nil)
	backend := &taskLinkBackend{config: config.TaskIntegrationConfig{Enabled: true, Endpoint: server.server.URL,
		APIKey: "test-key", DefaultProject: "project"}, index: index}

	const lookups = 8
	results := make([]tasklinks.LookupResult, lookups)
	var wg sync.WaitGroup
	for i := range lookups {
		wg.Go(func() {
			results[i] = backend.Lookup(context.Background(), identity)
		})
	}
	wg.Wait()

	assertions.EqualValues(1, server.listCalls.Load(), "concurrent lookups share a single rebuild")
	for i, result := range results {
		assertions.Lenf(result.Tasks, 1, "lookup %d sees the rebuilt index", i)
		assertions.Equal(tasklinks.StateReady, result.State)
	}
}

func TestTaskLinkBackendSurfacesCachePersistenceFailuresWithoutIdentityRelabeling(t *testing.T) {
	identity := tasklinks.MessageIdentity{ArchiveUID: "archive-a", ArchiveRevision: "1", MessageID: 42,
		SourceType: "gmail", SourceIdentifier: "archive@example.com", SourceMessageID: "source-42"}
	linkedTask := func(id, title string) taskclient.Task {
		return taskclient.Task{ID: id, Project: "project", Title: title, Revision: "r1",
			Metadata: tasklinks.MetadataWithLink(nil, tasklinks.NewMailLink(identity, time.Now()))}
	}
	readyServer := readyTaskBackendServer(t, []taskclient.Task{linkedTask("fresh", "Fresh current-session title")})
	t.Cleanup(readyServer.Close)

	t.Run("fresh unsupported persistence keeps scanned current-session data", func(t *testing.T) {
		assertions := assert.New(t)
		index := tasklinks.NewIndexWithOptions(filepath.Join(t.TempDir(), "cache", "tasks.json"), tasklinks.IndexOptions{
			Now: time.Now, PersistencePolicy: func() error { return tasklinks.ErrDiskCacheSecurityUnsupported },
		})
		backend := &taskLinkBackend{config: config.TaskIntegrationConfig{Enabled: true, Endpoint: readyServer.URL, APIKey: "test-key", DefaultProject: "project"}, index: index}

		result := backend.Lookup(context.Background(), identity)

		assertions.Equal(tasklinks.StateReady, result.State)
		assertions.Equal(tasklinks.ReasonCachePersistenceUnsupported, result.Reason)
		assertions.True(result.Complete)
		assertions.Len(result.Tasks, 1)
		assertions.Equal("Fresh current-session title", result.Tasks[0].Title)
	})

	t.Run("generic fresh save failure is not relabeled as wrong project", func(t *testing.T) {
		assertions := assert.New(t)
		index := tasklinks.NewIndexWithOptions(filepath.Join(t.TempDir(), "cache", "tasks.json"), tasklinks.IndexOptions{
			Now: time.Now, PersistencePolicy: func() error { return errors.New("synthetic disk failure") },
		})
		backend := &taskLinkBackend{config: config.TaskIntegrationConfig{Enabled: true, Endpoint: readyServer.URL, APIKey: "test-key", DefaultProject: "project"}, index: index}

		result := backend.Lookup(context.Background(), identity)

		assertions.Equal(tasklinks.StateStale, result.State)
		assertions.Equal(tasklinks.ReasonPersistenceFailure, result.Reason)
		assertions.False(result.Complete)
		assertions.Empty(result.Tasks)
	})

	t.Run("refresh adopts safe data but authentication still redacts it", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		persistenceSupported := true
		index := tasklinks.NewIndexWithOptions(filepath.Join(t.TempDir(), "cache", "tasks.json"), tasklinks.IndexOptions{
			Now: time.Now,
			PersistencePolicy: func() error {
				if persistenceSupported {
					return nil
				}
				return tasklinks.ErrDiskCacheSecurityUnsupported
			},
		})
		cacheIdentity := tasklinks.CacheIdentity{Project: "project", ArchiveUID: "archive-a", ArchiveRevision: "1"}
		_, err := index.Rebuild(context.Background(), backendListClient{tasks: []taskclient.Task{linkedTask("old", "Last-good title")}}, cacheIdentity, "project-r0", 100)
		requirements.NoError(err)
		persistenceSupported = false
		backend := &taskLinkBackend{config: config.TaskIntegrationConfig{Enabled: true, Endpoint: readyServer.URL, APIKey: "test-key", DefaultProject: "project"}, index: index}

		result := backend.Lookup(context.Background(), identity)

		assertions.Equal(tasklinks.StateReady, result.State)
		assertions.Equal(tasklinks.ReasonCachePersistenceUnsupported, result.Reason)
		assertions.True(result.Complete)
		requirements.Len(result.Tasks, 1)
		assertions.Equal("Fresh current-session title", result.Tasks[0].Title)

		wrongProjectServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/api/v1/capabilities" {
				assert.NoError(t, json.NewEncoder(w).Encode(taskclient.Capabilities{
					ProtocolVersion: taskclient.ProtocolVersion, RevisionReads: true, ConditionalMutation: true,
					ConflictResponses: true, IdempotentCreation: true, ProjectOperations: true, MetadataOperations: true,
				}))
				return
			}
			assert.NoError(t, json.NewEncoder(w).Encode(taskclient.Project{ID: "other-id", Name: "other", Revision: "p1"}))
		}))
		t.Cleanup(wrongProjectServer.Close)
		incompatibleServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			assert.NoError(t, json.NewEncoder(w).Encode(taskclient.Capabilities{ProtocolVersion: taskclient.ProtocolVersion}))
		}))
		t.Cleanup(incompatibleServer.Close)
		authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
		t.Cleanup(authServer.Close)
		unavailableServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		unavailableEndpoint := unavailableServer.URL
		unavailableServer.Close()

		for _, test := range []struct {
			name     string
			endpoint string
			state    tasklinks.IndexState
			reason   string
			redacted bool
		}{
			{name: "wrong project", endpoint: wrongProjectServer.URL, state: tasklinks.StateWrongProject, reason: "wrong_project"},
			{name: "incompatible", endpoint: incompatibleServer.URL, state: tasklinks.StateIncompatible, reason: "incompatible"},
			{name: "authentication required", endpoint: authServer.URL, state: tasklinks.StateAuthenticationRequired, reason: "authentication_required", redacted: true},
			{name: "unavailable", endpoint: unavailableEndpoint, state: tasklinks.StateUnavailable, reason: tasklinks.ReasonUnavailable},
		} {
			t.Run(test.name, func(t *testing.T) {
				assertions := assert.New(t)
				backend.config.Endpoint = test.endpoint

				degraded := backend.Lookup(context.Background(), identity)

				assertions.Equal(test.state, degraded.State)
				assertions.Equal(test.reason, degraded.Reason)
				assertions.False(degraded.Complete)
				if test.redacted {
					assertions.Empty(degraded.Tasks)
				} else {
					assertions.Len(degraded.Tasks, 1)
					assertions.Equal("Fresh current-session title", degraded.Tasks[0].Title)
				}
			})
		}
	})
}

func TestTaskLinkBackendDegradedStatesUseOnlySafeCache(t *testing.T) {
	identity := tasklinks.MessageIdentity{ArchiveUID: "archive-a", ArchiveRevision: "1", MessageID: 42,
		SourceType: "gmail", SourceIdentifier: "archive@example.com", SourceMessageID: "source-42"}
	cacheIdentity := tasklinks.CacheIdentity{Project: "project", ArchiveUID: "archive-a", ArchiveRevision: "1"}
	newBackend := func(t *testing.T, cfg config.TaskIntegrationConfig) *taskLinkBackend {
		t.Helper()
		index := tasklinks.NewIndex(filepath.Join(t.TempDir(), "cache", "tasks.json"), time.Now)
		linked := taskclient.Task{ID: "task-1", Project: "project", Title: "Cached title", Revision: "r1",
			Metadata: tasklinks.MetadataWithLink(nil, tasklinks.NewMailLink(identity, time.Now()))}
		_, err := index.Rebuild(context.Background(), backendListClient{tasks: []taskclient.Task{linked}}, cacheIdentity, "remote-1", 100)
		require.NoError(t, err)
		return &taskLinkBackend{config: cfg, index: index}
	}

	t.Run("disabled retains identity-safe titles", func(t *testing.T) {
		backend := newBackend(t, config.TaskIntegrationConfig{Enabled: false, DefaultProject: "project"})
		result := backend.Lookup(context.Background(), identity)
		assert.Equal(t, tasklinks.StateDisabled, result.State)
		require.Len(t, result.Tasks, 1)
		assert.Equal(t, "Cached title", result.Tasks[0].Title)
	})

	t.Run("authentication redacts titles", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		t.Cleanup(server.Close)
		backend := newBackend(t, config.TaskIntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: "bad-key", DefaultProject: "project"})
		result := backend.Lookup(context.Background(), identity)
		assert.Equal(t, tasklinks.StateAuthenticationRequired, result.State)
		assert.Empty(t, result.Tasks)
	})

	t.Run("incompatible retains identity-safe titles", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			assert.NoError(t, json.NewEncoder(w).Encode(taskclient.Capabilities{ProtocolVersion: taskclient.ProtocolVersion}))
		}))
		t.Cleanup(server.Close)
		backend := newBackend(t, config.TaskIntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: "test-key", DefaultProject: "project"})
		result := backend.Lookup(context.Background(), identity)
		assert.Equal(t, tasklinks.StateIncompatible, result.State)
		require.Len(t, result.Tasks, 1)
	})

	t.Run("wrong project retains cache only for configured identity", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v1/capabilities":
				assert.NoError(t, json.NewEncoder(w).Encode(taskclient.Capabilities{ProtocolVersion: taskclient.ProtocolVersion,
					RevisionReads: true, ConditionalMutation: true, ConflictResponses: true, IdempotentCreation: true,
					ProjectOperations: true, MetadataOperations: true}))
			default:
				assert.NoError(t, json.NewEncoder(w).Encode(taskclient.Project{ID: "other-id", Name: "other", Revision: "p1"}))
			}
		}))
		t.Cleanup(server.Close)
		backend := newBackend(t, config.TaskIntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: "test-key", DefaultProject: "project"})
		result := backend.Lookup(context.Background(), identity)
		assert.Equal(t, tasklinks.StateWrongProject, result.State)
		require.Len(t, result.Tasks, 1)
	})
}
