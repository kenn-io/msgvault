package tasklinks

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/taskclient"
)

func testIdentity() MessageIdentity {
	return MessageIdentity{ArchiveUID: "archive-a", MessageID: 42, ConversationID: 7,
		Subject: "Synthetic subject", From: "sender@example.com", SentAt: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC),
		SourceType: "gmail", SourceIdentifier: "archive@example.com", SourceMessageID: "source-42"}
}

func TestResolvePrecedenceAndArchiveRecovery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	identity := testIdentity()
	links := []MailLink{
		{MessageID: 42},
		{ArchiveUID: "archive-a", MessageID: 42},
		{ArchiveUID: "archive-other", MessageID: 999, SourceType: "gmail", SourceIdentifier: "archive@example.com", SourceMessageID: "source-42"},
	}
	matches := Resolve(links, identity)
	require.Len(matches, 1)
	assert.Equal(MatchSource, matches[0].Kind)
	assert.True(matches[0].RecoveredFromAnotherArchive)

	identity.SourceMessageID = "changed"
	matches = Resolve(links, identity)
	require.Len(matches, 1)
	assert.Equal(MatchArchive, matches[0].Kind)

	identity.ArchiveUID = "rebuilt"
	matches = Resolve(links, identity)
	require.Len(matches, 1)
	assert.Equal(MatchLegacy, matches[0].Kind)
}

func TestSnapshotIsBoundedSanitizedAndHasNoContent(t *testing.T) {
	assert := assert.New(t)
	identity := testIdentity()
	identity.Subject = "hello\x00\n" + string(make([]byte, 2000))
	link := NewMailLink(identity, time.Now())
	assert.NotContains(link.Subject, "\x00")
	assert.LessOrEqual(len(link.Subject), MaxSnapshotFieldBytes)
	encoded := MetadataWithLink(map[string]any{"unrelated": map[string]any{"kept": true}}, link)
	assert.Contains(encoded, "unrelated")
	assert.NotContains(encoded, "body")
	assert.NotContains(encoded, "attachments")
}

type mutationClient struct {
	tasks      []taskclient.Task
	mutateErrs []error
	mutations  []map[string]any
}

func (c *mutationClient) CreateTask(_ context.Context, project, key string, create taskclient.TaskCreate) (taskclient.Task, error) {
	return taskclient.Task{ID: "task-new", Project: project, Title: create.Title, Revision: "r1", Metadata: create.Metadata}, nil
}
func (c *mutationClient) GetTask(_ context.Context, _, _ string) (taskclient.Task, error) {
	t := c.tasks[0]
	if len(c.tasks) > 1 {
		c.tasks = c.tasks[1:]
	}
	return t, nil
}
func (c *mutationClient) MutateMetadata(_ context.Context, project, id, _ string, metadata map[string]any) (taskclient.Task, error) {
	c.mutations = append(c.mutations, metadata)
	if len(c.mutateErrs) > 0 {
		err := c.mutateErrs[0]
		c.mutateErrs = c.mutateErrs[1:]
		if err != nil {
			return taskclient.Task{}, err
		}
	}
	return taskclient.Task{ID: id, Project: project, Title: "Task", Revision: "next", Metadata: metadata}, nil
}

func TestMutationsAreIdempotentMergeOnlyAndRetryOnce(t *testing.T) {
	assert := assert.New(t)
	identity := testIdentity()
	existing := taskclient.Task{ID: "task-1", Project: "project", Title: "Task", Revision: "r1", Metadata: map[string]any{"owner": "kept"}}
	retried := existing
	retried.Revision = "r2"
	client := &mutationClient{tasks: []taskclient.Task{existing, retried}, mutateErrs: []error{taskclient.ErrConflict, nil}}
	service := Service{Client: client, Project: "project"}

	linked, err := service.Link(context.Background(), "task-1", identity)
	require.NoError(t, err)
	assert.Equal("kept", linked.Metadata["owner"])
	assert.Len(client.mutations, 2)
	assert.Equal("kept", client.mutations[0]["owner"])

	client.tasks = []taskclient.Task{linked}
	_, err = service.Link(context.Background(), "task-1", identity)
	require.NoError(t, err)
	assert.Len(client.mutations, 2, "duplicate link is a no-op")

	client.tasks = []taskclient.Task{linked}
	unlinked, err := service.Unlink(context.Background(), "task-1", identity)
	require.NoError(t, err)
	assert.Equal("kept", unlinked.Metadata["owner"])
}

func TestUnlinkRemovesEverySupportedRepresentationOfTheMessage(t *testing.T) {
	identity := testIdentity()
	existing := taskclient.Task{
		ID: "task-1", Project: "project", Title: "Task", Revision: "r1",
		Metadata: map[string]any{MailLinksKey: []any{
			MailLink{ArchiveUID: "archive-other", MessageID: 999, SourceType: identity.SourceType, SourceIdentifier: identity.SourceIdentifier, SourceMessageID: identity.SourceMessageID},
			MailLink{ArchiveUID: identity.ArchiveUID, MessageID: identity.MessageID},
			MailLink{MessageID: identity.MessageID},
			MailLink{MessageID: 999},
		}},
	}
	client := &mutationClient{tasks: []taskclient.Task{existing}}

	unlinked, err := (Service{Client: client, Project: "project"}).Unlink(context.Background(), "task-1", identity)

	require.NoError(t, err)
	links := MailLinks(unlinked.Metadata)
	require.Len(t, links, 1)
	assert.Equal(t, int64(999), links[0].MessageID)
}

func TestMutationPreservesUnrelatedRawMailLinkFieldsLosslessly(t *testing.T) {
	require := require.New(t)
	identity := testIdentity()
	originalLink := map[string]any{
		"message_id":  float64(8),
		"archive_uid": "another-archive",
		"provider_extension": map[string]any{
			"nested": []any{"keep", float64(3)},
		},
	}
	existing := taskclient.Task{
		ID: "task-1", Project: "project", Title: "Task", Revision: "r1",
		Metadata: map[string]any{
			"owner":      "kept",
			MailLinksKey: []any{originalLink},
		},
	}
	client := &mutationClient{tasks: []taskclient.Task{existing}}
	_, err := (Service{Client: client, Project: "project"}).Link(context.Background(), "task-1", identity)
	require.NoError(err)
	require.Len(client.mutations, 1)
	rawLinks, ok := client.mutations[0][MailLinksKey].([]any)
	require.True(ok)
	require.Len(rawLinks, 2)
	assert.Equal(t, originalLink, rawLinks[0], "unrelated link objects and extension fields must be byte-semantically preserved")
}

func TestMutationFailsClosedForUnparseableRawMailLinkElement(t *testing.T) {
	existing := taskclient.Task{
		ID: "task-1", Project: "project", Title: "Task", Revision: "r1",
		Metadata: map[string]any{MailLinksKey: []any{map[string]any{"message_id": float64(8)}, "opaque-extension"}},
	}
	client := &mutationClient{tasks: []taskclient.Task{existing}}
	_, err := (Service{Client: client, Project: "project"}).Link(context.Background(), "task-1", testIdentity())
	require.Error(t, err)
	require.ErrorIs(t, err, ErrUnsafeMailLinks)
	assert.Empty(t, client.mutations, "unsafe metadata must not be overwritten")
}

func TestMutationSecondConflictIsSurfaced(t *testing.T) {
	existing := taskclient.Task{ID: "task-1", Project: "project", Title: "Task", Revision: "r1"}
	retried := existing
	retried.Revision = "r2"
	client := &mutationClient{tasks: []taskclient.Task{existing, retried}, mutateErrs: []error{taskclient.ErrConflict, taskclient.ErrConflict}}
	_, err := (Service{Client: client, Project: "project"}).Link(context.Background(), "task-1", testIdentity())
	require.ErrorIs(t, err, taskclient.ErrConflict)
	assert.Len(t, client.mutations, 2)
}

func TestCreateRetryExercisesIdempotentHTTPContractWithIdenticalPayload(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	createdByKey := map[string]taskclient.Task{}
	payloadByKey := map[string][]byte{}
	createCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method)
		assert.Equal("/api/v1/projects/project/tasks", r.URL.Path)
		key := r.Header.Get("Idempotency-Key")
		assert.NotEmpty(key)
		payload, err := io.ReadAll(r.Body)
		assert.NoError(err)
		if existing, ok := createdByKey[key]; ok {
			if !bytes.Equal(payloadByKey[key], payload) {
				w.WriteHeader(http.StatusConflict)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			assert.NoError(json.NewEncoder(w).Encode(existing))
			return
		}
		var create taskclient.TaskCreate
		assert.NoError(json.Unmarshal(payload, &create))
		createCalls++
		created := taskclient.Task{ID: "task-created", Project: "project", Title: create.Title, Revision: "r1", Metadata: create.Metadata}
		createdByKey[key], payloadByKey[key] = created, append([]byte(nil), payload...)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		assert.NoError(json.NewEncoder(w).Encode(created))
	}))
	t.Cleanup(server.Close)
	client, err := taskclient.New(taskclient.ClientOptions{Endpoint: server.URL, APIKey: "test-key"})
	require.NoError(err)
	addedAt := time.Date(2026, 7, 19, 1, 2, 3, 0, time.UTC)
	service := Service{Client: client, Project: "project", Now: func() time.Time { return addedAt }}

	first, err := service.Create(context.Background(), "stable-key", taskclient.TaskCreate{Title: "Synthetic", Priority: "high", Labels: []string{"mail"}}, testIdentity())
	require.NoError(err)
	second, err := service.Create(context.Background(), "stable-key", taskclient.TaskCreate{Title: "Synthetic", Priority: "high", Labels: []string{"mail"}}, testIdentity())
	require.NoError(err)
	assert.Equal(first, second)
	assert.Equal(1, createCalls, "the external service must create only once for a repeated key and payload")
	links := MailLinks(first.Metadata)
	require.Len(links, 1)
	assert.Equal(addedAt.Format(time.RFC3339), links[0].AddedAt)
}
