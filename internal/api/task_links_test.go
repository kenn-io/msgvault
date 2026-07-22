package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/taskclient"
	"go.kenn.io/msgvault/internal/tasklinks"
)

type taskLinkMessageStore struct {
	*mockStore

	message *APIMessage
}

func (s *taskLinkMessageStore) GetMessage(id int64) (*APIMessage, error) {
	if s.message == nil || s.message.ID != id {
		return nil, errors.New("not found")
	}
	return s.message, nil
}

func (s *taskLinkMessageStore) GetMessageContext(_ context.Context, id int64) (*APIMessage, error) {
	return s.GetMessage(id)
}

type fakeTaskLinkOperations struct {
	createKeys []string
	creates    []taskclient.TaskCreate
	identities []tasklinks.MessageIdentity
	lookup     tasklinks.LookupResult
	search     []tasklinks.TaskSummary
	err        error
}

func (f *fakeTaskLinkOperations) Create(_ context.Context, key string, create taskclient.TaskCreate, identity tasklinks.MessageIdentity, addedAt time.Time) (taskclient.Task, error) {
	f.createKeys = append(f.createKeys, key)
	f.identities = append(f.identities, identity)
	create.Metadata = tasklinks.MetadataWithLink(create.Metadata, tasklinks.NewMailLink(identity, addedAt))
	f.creates = append(f.creates, create)
	return taskclient.Task{ID: "task-1", Project: "project", Title: create.Title, Revision: "r1"}, f.err
}
func (f *fakeTaskLinkOperations) Link(_ context.Context, _ string, identity tasklinks.MessageIdentity, _ time.Time) (taskclient.Task, error) {
	f.identities = append(f.identities, identity)
	return taskclient.Task{ID: "task-1", Project: "project", Title: "Linked", Revision: "r1"}, f.err
}
func (f *fakeTaskLinkOperations) Unlink(_ context.Context, _ string, identity tasklinks.MessageIdentity) (taskclient.Task, error) {
	f.identities = append(f.identities, identity)
	return taskclient.Task{ID: "task-1", Project: "project", Title: "Linked", Revision: "r2"}, f.err
}
func (f *fakeTaskLinkOperations) Lookup(context.Context, tasklinks.MessageIdentity) tasklinks.LookupResult {
	return f.lookup
}
func (f *fakeTaskLinkOperations) Search(context.Context, string) ([]tasklinks.TaskSummary, error) {
	return f.search, f.err
}

func emailMessage(messageType string) *APIMessage {
	return &APIMessage{ID: 42, SourceID: 3, SourceMessageID: "source-42", ConversationID: 7,
		Subject: "Synthetic subject", From: "sender@example.com", SentAt: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC), MessageType: messageType}
}

func taskLinkServer(t *testing.T, messageType string, operations TaskLinkOperations) *Server {
	t.Helper()
	return NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "test-key"}},
		Store: &taskLinkMessageStore{mockStore: &mockStore{}, message: emailMessage(messageType)}, Logger: testLogger(), TaskLinkOperations: operations,
		TaskIdentityResolver: func(_ context.Context, message *APIMessage) (tasklinks.MessageIdentity, error) {
			return tasklinks.MessageIdentity{ArchiveUID: "archive-a", ArchiveRevision: "1", MessageID: message.ID, ConversationID: message.ConversationID,
				Subject: message.Subject, From: message.From, SentAt: message.SentAt, SourceType: "gmail",
				SourceIdentifier: "archive@example.com", SourceMessageID: message.SourceMessageID}, nil
		},
	})
}

func TestCreateTaskForEmailDerivesStableServerIdempotency(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ops := &fakeTaskLinkOperations{}
	srv := taskLinkServer(t, "email", ops)
	body := []byte(`{"title":"Follow up","description":"Synthetic notes","priority":"high","labels":["mail","follow-up"],"added_at":"2026-07-19T01:02:03Z"}`)
	for range 2 {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/messages/42/tasks", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Api-Key", "test-key")
		req.Header.Set("X-Request-Id", "browser-request-1")
		resp := httptest.NewRecorder()
		srv.Router().ServeHTTP(resp, req)
		require.Equal(http.StatusCreated, resp.Code, resp.Body.String())
		assert.NotContains(resp.Body.String(), "archive-a")
	}
	require.Len(ops.createKeys, 2)
	assert.Equal(ops.createKeys[0], ops.createKeys[1])
	assert.NotEqual("browser-request-1", ops.createKeys[0])
	require.Len(ops.creates, 2)
	assert.Equal(ops.creates[0], ops.creates[1], "a browser retry must send the identical external create payload")
	links := tasklinks.MailLinks(ops.creates[0].Metadata)
	require.Len(links, 1)
	assert.Equal("2026-07-19T01:02:03Z", links[0].AddedAt)
	assert.Equal("high", ops.creates[0].Priority)
	assert.Equal([]string{"mail", "follow-up"}, ops.creates[0].Labels)
}

func TestCreateTaskRequiresBrowserRequestID(t *testing.T) {
	srv := taskLinkServer(t, "email", &fakeTaskLinkOperations{})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/messages/42/tasks", bytes.NewReader([]byte(`{"title":"Follow up","added_at":"2026-07-19T01:02:03Z"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", "test-key")
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	assert.Equal(t, http.StatusBadRequest, resp.Code, resp.Body.String())
}

func TestCreateTaskOpenAPIRequiresBrowserRequestID(t *testing.T) {
	doc := OpenAPIDocument()
	operation := doc.Paths["/api/v1/messages/{id}/tasks"].Post
	require.NotNil(t, operation)
	for _, parameter := range operation.Parameters {
		if parameter.Name == "X-Request-Id" {
			assert.True(t, parameter.Required)
			return
		}
	}
	require.Fail(t, "X-Request-Id parameter missing from task create contract")
}

func TestCreateTaskRejectsOversizedAndTrailingJSONBodies(t *testing.T) {
	srv := taskLinkServer(t, "email", &fakeTaskLinkOperations{})
	for _, tc := range []struct {
		name string
		body []byte
		want int
	}{
		{name: "oversized", body: append([]byte(`{"title":"`), append(bytes.Repeat([]byte("x"), MaxTaskLinkRequestBytes), []byte(`","added_at":"2026-07-19T01:02:03Z"}`)...)...), want: http.StatusRequestEntityTooLarge},
		{name: "trailing object", body: []byte(`{"title":"Follow up","added_at":"2026-07-19T01:02:03Z"} {}`), want: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/messages/42/tasks", bytes.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Api-Key", "test-key")
			req.Header.Set("X-Request-Id", "browser-request-1")
			resp := httptest.NewRecorder()
			srv.Router().ServeHTTP(resp, req)
			assert.Equal(t, tc.want, resp.Code, resp.Body.String())
		})
	}
}

func TestTaskLinkAPIsTreatLegacyBlankMessageTypeAsEmail(t *testing.T) {
	srv := taskLinkServer(t, "", &fakeTaskLinkOperations{})
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{method: http.MethodGet, path: "/api/v1/messages/42/tasks", want: http.StatusOK},
		{method: http.MethodPost, path: "/api/v1/messages/42/tasks", body: `{"task_id":"task-1","added_at":"2026-07-19T01:02:03Z"}`, want: http.StatusCreated},
		{method: http.MethodDelete, path: "/api/v1/messages/42/tasks/task-1", want: http.StatusOK},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Api-Key", "test-key")
		req.Header.Set("X-Request-Id", "browser-request-1")
		resp := httptest.NewRecorder()
		srv.Router().ServeHTTP(resp, req)
		assert.Equal(t, tc.want, resp.Code, resp.Body.String())
	}
}

func TestTaskLinkAPIsRejectNonEmailConcreteRows(t *testing.T) {
	srv := taskLinkServer(t, "imessage", &fakeTaskLinkOperations{})
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/messages/42/tasks"},
		{http.MethodPost, "/api/v1/messages/42/tasks"},
		{http.MethodDelete, "/api/v1/messages/42/tasks/task-1"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, bytes.NewReader([]byte(`{"task_id":"task-1","title":"Synthetic"}`)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Api-Key", "test-key")
		req.Header.Set("X-Request-Id", "browser-request-1")
		resp := httptest.NewRecorder()
		srv.Router().ServeHTTP(resp, req)
		assert.Equal(t, http.StatusUnprocessableEntity, resp.Code, resp.Body.String())
	}
}

func TestTaskLookupAlwaysReturnsIndexAuthorityFields(t *testing.T) {
	assert := assert.New(t)
	now := time.Date(2026, 7, 19, 1, 0, 0, 0, time.UTC)
	ops := &fakeTaskLinkOperations{lookup: tasklinks.LookupResult{IndexStatus: tasklinks.IndexStatus{
		State: tasklinks.StatePartial, Complete: false, LastScan: now, RemoteRevision: "remote-1", Reason: tasklinks.ReasonSafetyLimit},
		Tasks: []tasklinks.TaskSummary{{ID: "task-1", Title: "Linked task"}}}}
	srv := taskLinkServer(t, "email", ops)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/messages/42/tasks", nil)
	req.Header.Set("X-Api-Key", "test-key")
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
	var got TaskLinkLookupResponse
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &got))
	assert.False(got.Complete)
	assert.Equal(tasklinks.StatePartial, got.State)
	assert.Equal("remote-1", got.RemoteRevision)
	assert.Equal(tasklinks.ReasonSafetyLimit, got.Reason)
	assert.Equal(now, got.LastScan)
	require.Len(t, got.Tasks, 1)
	assert.Equal("archive-a", got.OutboundMetadata.ArchiveUID)
	assert.Equal(int64(42), got.OutboundMetadata.MessageID)
	assert.Equal(int64(7), got.OutboundMetadata.ConversationID)
	assert.Equal("Synthetic subject", got.OutboundMetadata.Subject)
	assert.Equal("sender@example.com", got.OutboundMetadata.From)
	assert.Equal("gmail", got.OutboundMetadata.SourceType)
	assert.Equal("archive@example.com", got.OutboundMetadata.SourceIdentifier)
	assert.Equal("source-42", got.OutboundMetadata.SourceMessageID)
}

func TestTaskLookupDisclosesExactSanitizedOutboundMetadata(t *testing.T) {
	assert := assert.New(t)
	identity := tasklinks.MessageIdentity{
		ArchiveUID: "  archive\x00-" + strings.Repeat("a", tasklinks.MaxSnapshotFieldBytes),
		MessageID:  42, ConversationID: 7,
		Subject:          "  subject\x00-" + strings.Repeat("s", tasklinks.MaxSnapshotFieldBytes),
		From:             "  sender\x00-" + strings.Repeat("f", tasklinks.MaxSnapshotFieldBytes),
		SentAt:           time.Date(2026, 7, 19, 1, 2, 3, 0, time.FixedZone("offset", -5*60*60)),
		SourceType:       "  gmail\x00-" + strings.Repeat("t", tasklinks.MaxSnapshotFieldBytes),
		SourceIdentifier: "  archive@example.com\x00-" + strings.Repeat("i", tasklinks.MaxSnapshotFieldBytes),
		SourceMessageID:  "  source-42\x00-" + strings.Repeat("m", tasklinks.MaxSnapshotFieldBytes),
	}
	want := tasklinks.NewMailLink(identity, time.Time{})

	got := taskLinkLookupResponse(identity, tasklinks.LookupResult{}).OutboundMetadata

	assert.Equal(want.ArchiveUID, got.ArchiveUID)
	assert.Equal(want.MessageID, got.MessageID)
	assert.Equal(want.ConversationID, got.ConversationID)
	assert.Equal(want.Subject, got.Subject)
	assert.Equal(want.From, got.From)
	assert.Equal(want.SentAt, got.SentAt)
	assert.Equal(want.SourceType, got.SourceType)
	assert.Equal(want.SourceIdentifier, got.SourceIdentifier)
	assert.Equal(want.SourceMessageID, got.SourceMessageID)
	assert.NotContains(got.Subject, "\x00")
	assert.LessOrEqual(len(got.Subject), tasklinks.MaxSnapshotFieldBytes)
}

func TestTaskSearchReturnsConfiguredProjectResults(t *testing.T) {
	ops := &fakeTaskLinkOperations{search: []tasklinks.TaskSummary{{ID: "task-1", Title: "Synthetic result", Revision: "r1"}}}
	srv := taskLinkServer(t, "email", ops)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/integrations/tasks/search?q=Synthetic", nil)
	req.Header.Set("X-Api-Key", "test-key")
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
	var got TaskSearchResponse
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &got))
	assert.Equal(t, ops.search, got.Tasks)
}

func TestTaskLinkSecondConflictIsSurfaced(t *testing.T) {
	ops := &fakeTaskLinkOperations{err: taskclient.ErrConflict}
	srv := taskLinkServer(t, "email", ops)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/messages/42/tasks", bytes.NewReader([]byte(`{"task_id":"task-1","added_at":"2026-07-19T01:02:03Z"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", "test-key")
	req.Header.Set("X-Request-Id", "browser-request-1")
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	assert.Equal(t, http.StatusConflict, resp.Code, resp.Body.String())
}

func TestTaskLinkRequestRejectedIsSurfacedAsClientError(t *testing.T) {
	ops := &fakeTaskLinkOperations{err: taskclient.ErrRequestRejected}
	srv := taskLinkServer(t, "email", ops)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/messages/42/tasks", bytes.NewReader([]byte(`{"task_id":"task-1","added_at":"2026-07-19T01:02:03Z"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", "test-key")
	req.Header.Set("X-Request-Id", "browser-request-1")
	resp := httptest.NewRecorder()
	srv.Router().ServeHTTP(resp, req)
	require.Equal(t, http.StatusUnprocessableEntity, resp.Code, resp.Body.String())
	assert.Contains(t, resp.Body.String(), "task_request_rejected")
}
