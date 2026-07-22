package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func seedConversation(t *testing.T, messageType string, count int) (*Server, int64, []int64) {
	t.Helper()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("test", "archive@example.com")
	require.NoError(t, err)
	conversationType := map[bool]string{true: "direct_chat", false: "email_thread"}[messageType != "email"]
	conversationID, err := st.EnsureConversationWithType(
		source.ID, "conversation-1", conversationType, "Synthetic conversation",
	)
	require.NoError(t, err)

	ids := make([]int64, 0, count)
	for i := range count {
		// Pairs deliberately share timestamps. The ID tiebreaker must keep paging stable.
		sentAt := time.Date(2026, time.January, i/2+1, 12, 0, 0, 0, time.UTC)
		id, upsertErr := st.UpsertMessage(&store.Message{
			ConversationID:  conversationID,
			SourceID:        source.ID,
			SourceMessageID: fmt.Sprintf("source-%02d", i),
			MessageType:     messageType,
			SentAt:          sql.NullTime{Time: sentAt, Valid: true},
			Subject:         sql.NullString{String: fmt.Sprintf("Message %02d", i), Valid: true},
			Snippet:         sql.NullString{String: fmt.Sprintf("Preview %02d", i), Valid: true},
		})
		require.NoError(t, upsertErr)
		ids = append(ids, id)
		require.NoError(t, st.UpsertMessageBody(id,
			sql.NullString{String: fmt.Sprintf("Body %02d", i), Valid: true},
			sql.NullString{String: fmt.Sprintf("<p>Body %02d</p>", i), Valid: true},
		))
	}

	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, nil, testLogger())
	return srv, conversationID, ids
}

func TestConversationWindowIsAnchoredBoundedAndChronological(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, conversationID, ids := seedConversation(t, "email", 8)
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/v1/conversations/%d?anchor=%d&before=2&after=2", conversationID, ids[3]), nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "body: %s", w.Body.String())
	var response ConversationResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&response))
	assert.Equal(conversationID, response.ID)
	assert.Equal(ids[3], response.AnchorID)
	require.Len(response.Messages, 5)
	assert.Equal([]int64{ids[1], ids[2], ids[3], ids[4], ids[5]}, []int64{
		response.Messages[0].ID, response.Messages[1].ID, response.Messages[2].ID,
		response.Messages[3].ID, response.Messages[4].ID,
	})
	assert.True(response.HasBefore)
	assert.True(response.HasAfter)
	assert.Equal("Body 03", response.Messages[2].Body)
	assert.Equal("<p>Body 03</p>", response.Messages[2].BodyHTML)
}

func TestConversationWindowReturnsIndividualChatMessages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, conversationID, ids := seedConversation(t, "imessage", 3)
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/v1/conversations/%d?anchor=%d&before=1&after=1", conversationID, ids[1]), nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "body: %s", w.Body.String())
	var response ConversationResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&response))
	require.Len(response.Messages, 3)
	assert.Equal("imessage", response.Messages[0].MessageType)
	assert.Equal(ids[1], response.AnchorID)
}

func TestConversationWindowCapsInlineBodiesAndServesOmittedBodiesByID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("test", "archive@example.com")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(
		source.ID, "oversized-thread", "email_thread", "Oversized thread",
	)
	require.NoError(err)

	// Three bodies of just under half the budget each: the anchor (first) and
	// one more fit, the third exceeds the remaining budget.
	bodySize := store.ConversationInlineBodyBudget/2 - 16
	ids := make([]int64, 0, 3)
	for i := range 3 {
		id, upsertErr := st.UpsertMessage(&store.Message{
			ConversationID:  conversationID,
			SourceID:        source.ID,
			SourceMessageID: fmt.Sprintf("oversized-%d", i),
			MessageType:     "email",
			SentAt:          sql.NullTime{Time: time.Date(2026, time.January, i+1, 12, 0, 0, 0, time.UTC), Valid: true},
			Snippet:         sql.NullString{String: fmt.Sprintf("Preview %d", i), Valid: true},
		})
		require.NoError(upsertErr)
		require.NoError(st.UpsertMessageBody(id,
			sql.NullString{String: strings.Repeat("b", bodySize), Valid: true},
			sql.NullString{}))
		ids = append(ids, id)
	}
	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, nil, testLogger())

	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/v1/conversations/%d?anchor=%d", conversationID, ids[0]), nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code)
	// The wire response stays bounded: inline bodies never exceed the budget
	// (plus the always-inlined anchor), regardless of cumulative thread size.
	assert.Less(w.Body.Len(), store.ConversationInlineBodyBudget+bodySize+1<<16)
	var response ConversationResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&response))
	require.Len(response.Messages, 3)

	inlineTotal := 0
	for _, message := range response.Messages {
		inlineTotal += len(message.Body) + len(message.BodyHTML)
	}
	assert.LessOrEqual(inlineTotal, store.ConversationInlineBodyBudget+bodySize)

	assert.False(response.Messages[0].BodyOmitted, "anchor body is always inline")
	assert.Len(response.Messages[0].Body, bodySize)
	assert.False(response.Messages[1].BodyOmitted)
	assert.Len(response.Messages[1].Body, bodySize)
	assert.True(response.Messages[2].BodyOmitted)
	assert.Empty(response.Messages[2].Body)
	assert.Equal("Preview 2", response.Messages[2].Snippet)

	// The omitted body is fully served by the single-message PK lookup.
	req = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/messages/%d", ids[2]), nil)
	w = httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	require.Equal(http.StatusOK, w.Code)
	var detail MessageDetail
	require.NoError(json.NewDecoder(w.Body).Decode(&detail))
	assert.Equal(strings.Repeat("b", bodySize), detail.Body)
	assert.False(detail.BodyOmitted)
}

func TestConversationWindowRejectsMissingOrForeignAnchor(t *testing.T) {
	srv, conversationID, ids := seedConversation(t, "email", 2)

	for name, target := range map[string]struct {
		path string
		code string
	}{
		"missing conversation": {
			path: fmt.Sprintf("/api/v1/conversations/999999?anchor=%d", ids[0]),
			code: "conversation_not_found",
		},
		"foreign anchor": {
			path: fmt.Sprintf("/api/v1/conversations/%d?anchor=999999", conversationID),
			code: "conversation_anchor_not_found",
		},
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, target.path, nil)
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, req)
			assert.Equal(t, http.StatusNotFound, w.Code)
			var response ErrorResponse
			require.NoError(t, json.NewDecoder(w.Body).Decode(&response))
			assert.Equal(t, target.code, response.Error)
		})
	}
}

func TestConversationWindowValidatesBounds(t *testing.T) {
	srv, conversationID, ids := seedConversation(t, "email", 1)
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/v1/conversations/%d", conversationID), nil)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code, "missing anchor")

	for _, query := range []string{
		"before=-1", "after=51",
		"start=not-a-time",
		"start=2026-01-02T00:00:00Z&end=2026-01-01T00:00:00Z",
		"start=2026-01-01T00:00:00Z&end=2026-01-01T00:00:00Z",
	} {
		req = httptest.NewRequest(http.MethodGet,
			fmt.Sprintf("/api/v1/conversations/%d?anchor=%d&%s", conversationID, ids[0], query), nil)
		w = httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		assert.Equal(t, http.StatusBadRequest, w.Code, query)
	}
	req = httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/v1/conversations/%d?anchor=bad", conversationID), nil)
	w = httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code, "anchor=bad")
}

// seedConversationAcrossDays creates count messages split across two UTC
// days (the first half on day one, the second half on day two) so tests can
// exercise start/end range scoping. Messages within a day get distinct
// minute offsets to keep chronological ordering stable.
func seedConversationAcrossDays(t *testing.T, count int) (*Server, int64, []int64, time.Time) {
	t.Helper()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("test", "archive@example.com")
	require.NoError(t, err)
	conversationID, err := st.EnsureConversationWithType(
		source.ID, "conversation-ranged", "email_thread", "Ranged conversation",
	)
	require.NoError(t, err)

	day1 := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, time.January, 2, 12, 0, 0, 0, time.UTC)
	ids := make([]int64, 0, count)
	for i := range count {
		day := day1
		if i >= count/2 {
			day = day2
		}
		sentAt := day.Add(time.Duration(i) * time.Minute)
		id, err := st.UpsertMessage(&store.Message{
			ConversationID:  conversationID,
			SourceID:        source.ID,
			SourceMessageID: fmt.Sprintf("ranged-%02d", i),
			MessageType:     "email",
			SentAt:          sql.NullTime{Time: sentAt, Valid: true},
			Subject:         sql.NullString{String: fmt.Sprintf("Message %02d", i), Valid: true},
			Snippet:         sql.NullString{String: fmt.Sprintf("Preview %02d", i), Valid: true},
		})
		require.NoError(t, err)
		ids = append(ids, id)
	}

	srv := NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, nil, testLogger())
	return srv, conversationID, ids, day2
}

func TestConversationWindowScopesToTimeRangeExcludingOtherDays(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, conversationID, ids, day2 := seedConversationAcrossDays(t, 6)

	start := day2.Truncate(24 * time.Hour)
	end := start.AddDate(0, 0, 1)
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf(
		"/api/v1/conversations/%d?anchor=%d&start=%s&end=%s",
		conversationID, ids[4], start.Format(time.RFC3339), end.Format(time.RFC3339),
	), nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	require.Equal(http.StatusOK, w.Code, "body: %s", w.Body.String())
	var response ConversationResponse
	require.NoError(json.NewDecoder(w.Body).Decode(&response))
	assert.Equal(int64(3), response.Total)
	require.Len(response.Messages, 3)
	assert.Equal([]int64{ids[3], ids[4], ids[5]}, []int64{
		response.Messages[0].ID, response.Messages[1].ID, response.Messages[2].ID,
	})
	assert.False(response.HasBefore, "day-1 messages exist but are outside the requested range")
	assert.False(response.HasAfter)
}

func TestConversationWindowRejectsAnchorOutsideTimeRange(t *testing.T) {
	srv, conversationID, ids, day2 := seedConversationAcrossDays(t, 6)

	start := day2.Truncate(24 * time.Hour)
	end := start.AddDate(0, 0, 1)
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf(
		"/api/v1/conversations/%d?anchor=%d&start=%s&end=%s",
		conversationID, ids[0], start.Format(time.RFC3339), end.Format(time.RFC3339),
	), nil)
	w := httptest.NewRecorder()

	srv.Router().ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	var response ErrorResponse
	require.NoError(t, json.NewDecoder(w.Body).Decode(&response))
	assert.Equal(t, "conversation_anchor_outside_range", response.Error)
}
