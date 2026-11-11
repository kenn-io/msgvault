package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

func TestChangesFilterValidation(t *testing.T) {
	srv, _ := newChangesServer(t)
	for _, tc := range []struct{ query, code string }{
		{"person_id=0", "invalid_person_id"},
		{"person_id=bad", "invalid_person_id"},
		{"source_id=0", "invalid_source_id"},
		{"conversation_id=bad", "invalid_conversation_id"},
		{"incoming_only=maybe", "invalid_incoming_only"},
		{"source_id=" + strings.Repeat("1,", 50) + "1", "invalid_source_id"},
		{"conversation_id=" + strings.Repeat("1,", 50) + "1", "invalid_conversation_id"},
	} {
		t.Run(tc.query, func(t *testing.T) {
			w := doGet(t.Context(), srv, "/api/v1/messages/changes?"+tc.query)
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			assert.Equal(t, tc.code, decodeErrorEnvelope(t, w).Error)
		})
	}
}

func TestChangesFilterCursorAndScan(t *testing.T) {
	srv, st := newChangesServer(t)
	ids := seedChangedMessages(t, st, 3)
	setChangesWatermark(t, st, "2020-01-01 00:00:00.000", ids...)
	src, err := st.GetOrCreateSource("gmail", "changes@example.com")
	require.NoError(t, err)
	query := fmt.Sprintf("source_id=%d&source_id=%d&limit=1", src.ID, src.ID)
	first := getChangesPage(t, srv, "/api/v1/messages/changes?"+query)
	require.Len(t, first.Messages, 1)
	assert.True(t, first.HasMore)
	second := getChangesPage(t, srv, fmt.Sprintf("/api/v1/messages/changes?source_id=%d&cursor=%s&limit=1", src.ID, url.QueryEscape(first.NextCursor)))
	require.Len(t, second.Messages, 1)
	assert.NotEqual(t, first.Messages[0].ID, second.Messages[0].ID)
	refused := doGet(t.Context(), srv, "/api/v1/messages/changes?cursor="+url.QueryEscape(first.NextCursor))
	assert.Equal(t, http.StatusBadRequest, refused.Code, refused.Body.String())
	assert.Contains(t, refused.Body.String(), "different filter")
}

func TestChangesFilterMatchesAllDimensions(t *testing.T) {
	f := changesFilter{PersonID: 4, participants: []int64{10, 11}, SourceIDs: []int64{1, 2}, ConversationIDs: []int64{8}, IncomingOnly: true}
	for _, tc := range []struct {
		name string
		row  store.ChangedMessage
		want bool
	}{
		{"first identity", store.ChangedMessage{SenderID: 10, SourceID: 1, ConversationID: 8}, true},
		{"second identity", store.ChangedMessage{SenderID: 11, SourceID: 2, ConversationID: 8}, true},
		{"other sender", store.ChangedMessage{SenderID: 12, SourceID: 1, ConversationID: 8}, false},
		{"other source", store.ChangedMessage{SenderID: 10, SourceID: 3, ConversationID: 8}, false},
		{"other conversation", store.ChangedMessage{SenderID: 10, SourceID: 1, ConversationID: 9}, false},
		{"outgoing", store.ChangedMessage{SenderID: 10, SourceID: 1, ConversationID: 8, IsFromMe: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) { assert.Equal(t, tc.want, f.matches(tc.row)) })
	}
}

func TestChangesScanResumesWithinReadAndAtBudget(t *testing.T) {
	at := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	bound := at.Add(time.Hour)
	page := store.ChangedMessagePage{ServerTime: bound.Add(time.Second), CompleteThrough: bound}
	for i := range 2001 {
		page.Messages = append(page.Messages, store.ChangedMessage{ID: int64(i - 1), SourceID: 1, ContentChangedAt: at})
	}
	filter := changesFilter{SourceIDs: []int64{1}}
	rows, next, more := scanChanges(page, store.ChangedMessagesFrom(at), filter, 1)
	require.Len(t, rows, 1)
	assert.Equal(t, int64(-1), rows[0].ID)
	id, present := next.AfterID()
	assert.True(t, present)
	assert.Equal(t, int64(-1), id)
	assert.True(t, more)
	filter.SourceIDs = []int64{2}
	rows, next, more = scanChanges(page, store.ChangedMessagesFrom(at), filter, 100)
	assert.Empty(t, rows)
	id, present = next.AfterID()
	assert.True(t, present)
	assert.Equal(t, int64(1998), id)
	assert.True(t, more)
	page.Messages = page.Messages[:2000]
	rows, next, more = scanChanges(page, store.ChangedMessagesFrom(at), filter, 100)
	assert.Empty(t, rows)
	assert.False(t, more)
	assert.Equal(t, bound, next.At())
	_, present = next.AfterID()
	assert.False(t, present)
}

func TestChangesPersonMatchesLinkedSenders(t *testing.T) {
	srv, st := newChangesServer(t)
	first, err := st.EnsureParticipant("first@example.com", "First", "example.com")
	require.NoError(t, err)
	second, err := st.EnsureParticipant("second@example.com", "Second", "example.com")
	require.NoError(t, err)
	other, err := st.EnsureParticipant("other@example.com", "Other", "example.com")
	require.NoError(t, err)
	_, err = st.LinkParticipants(first, second)
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(first)
	require.NoError(t, err)
	ids := seedChangedMessages(t, st, 3)
	for i, sender := range []int64{first, second, other} {
		_, err = st.DB().ExecContext(t.Context(), st.Rebind("UPDATE messages SET sender_id = ? WHERE id = ?"), sender, ids[i])
		require.NoError(t, err)
	}
	setChangesWatermark(t, st, "2020-01-01 00:00:00.000", ids...)
	page := getChangesPage(t, srv, fmt.Sprintf("/api/v1/messages/changes?person_id=%d", person.ID))
	require.Len(t, page.Messages, 2)
	assert.Equal(t, ids[0], page.Messages[0].ID)
	assert.Equal(t, ids[1], page.Messages[1].ID)
	refused := doGet(t.Context(), srv, "/api/v1/messages/changes?person_id=999999999")
	assert.Equal(t, http.StatusNotFound, refused.Code, refused.Body.String())
}

type countingChangesStore struct {
	*store.Store
	calls int
	limit int
}

func (s *countingChangesStore) ListChangedMessages(ctx context.Context, since store.ChangedMessagesCursor, limit int) (store.ChangedMessagePage, error) {
	s.calls++
	s.limit = limit
	return s.Store.ListChangedMessages(ctx, since, limit)
}

func TestChangesFilteredReadsOneBoundedPage(t *testing.T) {
	srv, st := newChangesServer(t)
	ids := seedChangedMessages(t, st, 3)
	setChangesWatermark(t, st, "2020-01-01 00:00:00.000", ids...)
	counted := &countingChangesStore{Store: st}
	srv.store = counted
	page := getChangesPage(t, srv, "/api/v1/messages/changes?source_id=999999")
	assert.Empty(t, page.Messages)
	pos, err := decodeChangesCursor(page.NextCursor)
	require.NoError(t, err)
	assert.True(t, pos.cursor.At().After(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)))
	assert.Equal(t, 1, counted.calls)
	assert.Equal(t, 2001, counted.limit)
}

func TestChangesScanEmptyPageBound(t *testing.T) {
	sinceTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	since := store.ChangedMessagesAfter(sinceTime, 0)
	filter := changesFilter{SourceIDs: []int64{1}}
	for _, bound := range []time.Time{{}, sinceTime.Add(-time.Second), sinceTime} {
		page := store.ChangedMessagePage{ServerTime: sinceTime.Add(time.Second), CompleteThrough: bound}
		rows, next, more := scanChanges(page, since, filter, 1)
		assert.Empty(t, rows)
		assert.False(t, more)
		assert.Equal(t, since, next)
	}
	page := store.ChangedMessagePage{ServerTime: sinceTime.Add(-time.Second), CompleteThrough: sinceTime.Add(-2 * time.Second)}
	_, next, more := scanChanges(page, since, filter, 1)
	assert.False(t, more)
	assert.Equal(t, page.CompleteThrough, next.At())
	_, present := next.AfterID()
	assert.False(t, present)
}

func TestChangesInvalidFilterPrecedesArchiveCapability(t *testing.T) {
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: &mockStore{}, Logger: testLogger()})
	relaxChangeFeedRateLimit(t, srv)
	w := doGet(t.Context(), srv, "/api/v1/messages/changes?source_id=invalid")
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "invalid_source_id")
}

func TestChangesPersonErrorsAreDeclared(t *testing.T) {
	operation := pathOperation(OpenAPIDocument().Paths["/api/v1/messages/changes"], http.MethodGet)
	require.NotNil(t, operation)
	for _, status := range []string{"404", "422"} {
		response, ok := operation.Responses[status]
		require.True(t, ok, "missing response %s", status)
		require.NotNil(t, response.Content[applicationJSONMediaType])
		assert.Equal(t, "#/components/schemas/ErrorResponse", response.Content[applicationJSONMediaType].Schema.Ref)
	}
}
