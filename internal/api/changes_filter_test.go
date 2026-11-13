package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestChangesFilteredCursorPagination(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/?person_id=4&source_id=2&source_id=1&source_id=2&conversation_id=8&incoming_only=true", nil)
	filter, err := parseChangesFilter(r)
	requirements.NoError(err)
	assertions.Equal([]int64{1, 2}, filter.SourceIDs)
	assertions.Equal("7725ea5be9477647", filter.Digest())
	srv, st := newChangesServer(t)
	ids := seedChangedMessages(t, st, 3)
	setChangesWatermark(t, st, "2020-01-01 00:00:00.000", ids...)
	counted := &countingChangesStore{Store: st}
	srv.store = counted
	empty := getChangesPage(t, srv, "/api/v1/messages/changes?source_id=999999")
	assertions.Empty(empty.Messages)
	assertions.Equal(1, counted.calls)
	assertions.Equal(2001, counted.limit)
	src, err := st.GetOrCreateSource("gmail", "changes@example.com")
	requirements.NoError(err)
	query := fmt.Sprintf("source_id=%d&source_id=%d&limit=1", src.ID, src.ID)
	first := getChangesPage(t, srv, "/api/v1/messages/changes?"+query)
	requirements.Len(first.Messages, 1)
	assertions.True(first.HasMore)
	second := getChangesPage(t, srv, fmt.Sprintf("/api/v1/messages/changes?source_id=%d&cursor=%s&limit=1", src.ID, url.QueryEscape(first.NextCursor)))
	requirements.Len(second.Messages, 1)
	assertions.Equal(ids[1], second.Messages[0].ID)
	refused := doGet(t.Context(), srv, "/api/v1/messages/changes?cursor="+url.QueryEscape(first.NextCursor))
	assertions.Equal(http.StatusBadRequest, refused.Code, refused.Body.String())
	assertions.Contains(refused.Body.String(), "different filter")
	refused = doGet(t.Context(), srv, "/api/v1/messages/changes?source_id=999999&cursor="+url.QueryEscape(first.NextCursor))
	assertions.Equal(http.StatusBadRequest, refused.Code, refused.Body.String())
	assertions.Contains(refused.Body.String(), "different filter")
}

func TestChangesPersonMatchesLinkedSenders(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv, st := newChangesServer(t)
	first, err := st.EnsureParticipant("first@example.com", "First", "example.com")
	requirements.NoError(err)
	second, err := st.EnsureParticipant("second@example.com", "Second", "example.com")
	requirements.NoError(err)
	other, err := st.EnsureParticipant("other@example.com", "Other", "example.com")
	requirements.NoError(err)
	_, err = st.LinkParticipants(first, second)
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(first)
	requirements.NoError(err)
	ids := seedChangedMessages(t, st, 5)
	for i, sender := range []int64{first, second, other} {
		_, err = st.DB().ExecContext(t.Context(), st.Rebind("UPDATE messages SET sender_id = ? WHERE id = ?"), sender, ids[i])
		requirements.NoError(err)
	}
	_, err = st.DB().ExecContext(t.Context(), st.Rebind("INSERT INTO message_recipients (message_id, participant_id, recipient_type) VALUES (?, ?, 'from')"), ids[3], second)
	requirements.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind("INSERT INTO message_recipients (message_id, participant_id, recipient_type) VALUES (?, ?, 'from')"), ids[3], other)
	requirements.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind("UPDATE messages SET sender_id = ? WHERE id = ?"), other, ids[4])
	requirements.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind("INSERT INTO message_recipients (message_id, participant_id, recipient_type) VALUES (?, ?, 'from')"), ids[4], first)
	requirements.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind("UPDATE messages SET is_from_me = ? WHERE id = ?"), true, ids[1])
	requirements.NoError(err)
	setChangesWatermark(t, st, "2020-01-01 00:00:00.000", ids...)
	page := getChangesPage(t, srv, fmt.Sprintf("/api/v1/messages/changes?person_id=%d", person.ID))
	requirements.Len(page.Messages, 3)
	assertions.Equal(ids[0], page.Messages[0].ID)
	assertions.Equal(ids[1], page.Messages[1].ID)
	assertions.Equal(ids[3], page.Messages[2].ID)
	incoming := getChangesPage(t, srv, fmt.Sprintf("/api/v1/messages/changes?person_id=%d&incoming_only=true", person.ID))
	requirements.Len(incoming.Messages, 2)
	assertions.Equal(ids[0], incoming.Messages[0].ID)
	assertions.Equal(ids[3], incoming.Messages[1].ID)
	refused := doGet(t.Context(), srv, "/api/v1/messages/changes?person_id=999999999")
	assertions.Equal(http.StatusNotFound, refused.Code, refused.Body.String())
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
	requirements := require.New(t)
	operation := pathOperation(OpenAPIDocument().Paths["/api/v1/messages/changes"], http.MethodGet)
	requirements.NotNil(operation)
	for _, status := range []string{"404", "409", "422"} {
		response, ok := operation.Responses[status]
		requirements.True(ok, "missing response %s", status)
		requirements.NotNil(response.Content[applicationJSONMediaType])
		assert.Equal(t, "#/components/schemas/ErrorResponse", response.Content[applicationJSONMediaType].Schema.Ref)
	}
}

// linkBeforeChangesPage commits an identity link and message immediately before
// the real store reads the bounded page, without depending on goroutine timing.
type linkBeforeChangesPage struct {
	*store.Store

	before func()
}

func (s *linkBeforeChangesPage) ListChangedMessages(ctx context.Context, since store.ChangedMessagesCursor, limit int) (store.ChangedMessagePage, error) {
	if s.before != nil {
		before := s.before
		s.before = nil
		before()
	}
	return s.Store.ListChangedMessages(ctx, since, limit)
}

func TestChangesPersonIncludesIdentityLinkedBeforePage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, st := newChangesServer(t)
	first, err := st.EnsureParticipant("first@example.com", "First", "example.com")
	require.NoError(err)
	second, err := st.EnsureParticipant("second@example.com", "Second", "example.com")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(first)
	require.NoError(err)

	var messageID int64
	srv.store = &linkBeforeChangesPage{Store: st, before: func() {
		_, err := st.LinkParticipants(first, second)
		require.NoError(err)
		messageID = seedChangedMessages(t, st, 1)[0]
		_, err = st.DB().ExecContext(t.Context(), st.Rebind("UPDATE messages SET sender_id = ? WHERE id = ?"), second, messageID)
		require.NoError(err)
		setChangesWatermark(t, st, "2020-01-01 00:00:00.000", messageID)
	}}

	target := fmt.Sprintf("/api/v1/messages/changes?person_id=%d", person.ID)
	page := getChangesPage(t, srv, target)
	assert.Equal([]int64{messageID}, changedIDs(page))
	next := getChangesPage(t, srv, target+"&cursor="+url.QueryEscape(page.NextCursor))
	assert.Empty(next.Messages)
}
