package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestChangesFilteredCursorPagination(t *testing.T) {
	srv, st := newChangesServer(t)
	ids := seedChangedMessages(t, st, 3)
	setChangesWatermark(t, st, "2020-01-01 00:00:00.000", ids...)
	counted := &countingChangesStore{Store: st}
	srv.store = counted
	empty := getChangesPage(t, srv, "/api/v1/messages/changes?source_id=999999")
	assert.Empty(t, empty.Messages)
	assert.Equal(t, 1, counted.calls)
	assert.Equal(t, 2001, counted.limit)
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
	refused = doGet(t.Context(), srv, "/api/v1/messages/changes?source_id=999999&cursor="+url.QueryEscape(first.NextCursor))
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

func TestChangesScanPages(t *testing.T) {
	at := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	bound := at.Add(time.Hour)
	rows := make([]store.ChangedMessage, 2001)
	for i := range rows {
		rows[i] = store.ChangedMessage{ID: int64(i - 1), SourceID: 1, ContentChangedAt: at}
	}
	from := store.ChangedMessagesFrom(at)
	after := store.ChangedMessagesAfter(at, 0)
	for _, tc := range []struct {
		name         string
		page         store.ChangedMessagePage
		since        store.ChangedMessagesCursor
		source       int64
		limit, count int
		next         store.ChangedMessagesCursor
		more         bool
	}{
		{"limit", store.ChangedMessagePage{Messages: rows, ServerTime: bound.Add(time.Second), CompleteThrough: bound}, from, 1, 1, 1, store.ChangedMessagesAfter(at, -1), true},
		{"sentinel", store.ChangedMessagePage{Messages: rows, ServerTime: bound.Add(time.Second), CompleteThrough: bound}, from, 2, 100, 0, store.ChangedMessagesAfter(at, 1998), true},
		{"exhaustion", store.ChangedMessagePage{Messages: rows[:2000], ServerTime: bound.Add(time.Second), CompleteThrough: bound}, from, 2, 100, 0, store.ChangedMessagesFrom(bound), false},
		{"no bound", store.ChangedMessagePage{ServerTime: bound}, after, 1, 1, 0, after, false},
		{"earlier bound", store.ChangedMessagePage{ServerTime: bound, CompleteThrough: at.Add(-time.Second)}, after, 1, 1, 0, after, false},
		{"equal bound", store.ChangedMessagePage{ServerTime: bound, CompleteThrough: at}, after, 1, 1, 0, after, false},
		{"above clock", store.ChangedMessagePage{ServerTime: at.Add(-time.Second), CompleteThrough: at.Add(-2 * time.Second)}, after, 1, 1, 0, store.ChangedMessagesFrom(at.Add(-2 * time.Second)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, next, more := scanChanges(tc.page, tc.since, changesFilter{SourceIDs: []int64{tc.source}}, tc.limit)
			assert.Len(t, got, tc.count)
			assert.Equal(t, tc.next, next)
			assert.Equal(t, tc.more, more)
		})
	}
}

func TestChangesFilterDigestStable(t *testing.T) {
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/?person_id=4&source_id=2&source_id=1&source_id=2&conversation_id=8&incoming_only=true", nil)
	filter, err := parseChangesFilter(r)
	require.NoError(t, err)
	assert.Equal(t, "7725ea5be9477647", filter.digest())
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
	ids := seedChangedMessages(t, st, 5)
	for i, sender := range []int64{first, second, other} {
		_, err = st.DB().ExecContext(t.Context(), st.Rebind("UPDATE messages SET sender_id = ? WHERE id = ?"), sender, ids[i])
		require.NoError(t, err)
	}
	_, err = st.DB().ExecContext(t.Context(), st.Rebind("INSERT INTO message_recipients (message_id, participant_id, recipient_type) VALUES (?, ?, 'from')"), ids[3], second)
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind("INSERT INTO message_recipients (message_id, participant_id, recipient_type) VALUES (?, ?, 'from')"), ids[3], other)
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind("UPDATE messages SET sender_id = ?, is_from_me = ? WHERE id = ?"), other, true, ids[4])
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind("INSERT INTO message_recipients (message_id, participant_id, recipient_type) VALUES (?, ?, 'from')"), ids[4], first)
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind("UPDATE messages SET is_from_me = ? WHERE id = ?"), true, ids[1])
	require.NoError(t, err)
	setChangesWatermark(t, st, "2020-01-01 00:00:00.000", ids...)
	page := getChangesPage(t, srv, fmt.Sprintf("/api/v1/messages/changes?person_id=%d", person.ID))
	require.Len(t, page.Messages, 3)
	assert.Equal(t, ids[0], page.Messages[0].ID)
	assert.Equal(t, ids[1], page.Messages[1].ID)
	assert.Equal(t, ids[3], page.Messages[2].ID)
	incoming := getChangesPage(t, srv, fmt.Sprintf("/api/v1/messages/changes?person_id=%d&incoming_only=true", person.ID))
	require.Len(t, incoming.Messages, 2)
	assert.Equal(t, ids[0], incoming.Messages[0].ID)
	assert.Equal(t, ids[3], incoming.Messages[1].ID)
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

func TestChangesPersonErrorsAreDeclared(t *testing.T) {
	operation := pathOperation(OpenAPIDocument().Paths["/api/v1/messages/changes"], http.MethodGet)
	require.NotNil(t, operation)
	for _, status := range []string{"404", "409", "422"} {
		response, ok := operation.Responses[status]
		require.True(t, ok, "missing response %s", status)
		require.NotNil(t, response.Content[applicationJSONMediaType])
		assert.Equal(t, "#/components/schemas/ErrorResponse", response.Content[applicationJSONMediaType].Schema.Ref)
	}
}
