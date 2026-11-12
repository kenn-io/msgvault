package client

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestGeneratedIntroductionClientDecodesListAndDetail(t *testing.T) {
	const record = `{"intro_id":"intro-0123456789abcdef0123456789abcdef","source_id":7,"person_a_id":11,"person_b_id":12,"status":"prepared","revision":2,"title":"Hello","body":"Please meet","created_by":"owner","created_at":"2026-11-12T10:00:00Z","updated_at":"2026-11-12T11:00:00Z","cancelled_at":null,"members":[{"person_id":11,"participant_id":21,"user_id":"alice@example.com"}],"blockers":[{"person_id":12,"code":"no_identity_on_account"}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/intros":
			assert.Equal(t, "7", r.URL.Query().Get("source_id"))
			assert.Equal(t, "10", r.URL.Query().Get("limit"))
			assert.Equal(t, "previous-cursor", r.URL.Query().Get("after_id"))
			_, _ = w.Write([]byte(`{"items":[` + record + `],"has_more":true,"next_after_id":"next-cursor"}`))
		case "/api/v1/intros/intro-0123456789abcdef0123456789abcdef":
			_, _ = w.Write([]byte(record))
		default:
			assert.Fail(t, "unexpected request", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	client, err := New(server.URL)
	require.NoError(t, err)

	sourceID, limit, cursor := int64(7), int64(10), "previous-cursor"
	list, err := client.ListIntroductionsWithResponse(t.Context(), &generated.ListIntroductionsRequestOptions{
		Query: &generated.ListIntroductionsQuery{SourceID: &sourceID, Limit: &limit, AfterID: &cursor},
	})
	require.NoError(t, err)
	require.NotNil(t, list.JSON200)
	require.Len(t, list.JSON200.Items, 1)
	assert.True(t, list.JSON200.HasMore)
	assert.Equal(t, "next-cursor", list.JSON200.NextAfterID)
	assertIntroductionRecord(t, list.JSON200.Items[0])

	detail, err := client.GetIntroductionWithResponse(t.Context(), &generated.GetIntroductionRequestOptions{
		PathParams: &generated.GetIntroductionPath{ID: "intro-0123456789abcdef0123456789abcdef"},
	})
	require.NoError(t, err)
	require.NotNil(t, detail.JSON200)
	assertIntroductionRecord(t, *detail.JSON200)
}

func assertIntroductionRecord(t *testing.T, record generated.IntroductionRecord) {
	t.Helper()
	assert.Equal(t, "intro-0123456789abcdef0123456789abcdef", record.IntroID)
	assert.Equal(t, int64(7), record.SourceID)
	assert.Equal(t, int64(11), record.PersonAId)
	assert.Equal(t, int64(12), record.PersonBId)
	assert.Equal(t, "prepared", string(record.Status))
	assert.Equal(t, int64(2), record.Revision)
	assert.Equal(t, "Hello", record.Title)
	assert.Equal(t, "Please meet", record.Body)
	assert.Equal(t, "owner", record.CreatedBy)
	assert.Equal(t, time.Date(2026, time.November, 12, 10, 0, 0, 0, time.UTC), record.CreatedAt)
	assert.Equal(t, time.Date(2026, time.November, 12, 11, 0, 0, 0, time.UTC), record.UpdatedAt)
	assert.Nil(t, record.CancelledAt)
	require.Len(t, record.Members, 1)
	assert.Equal(t, int64(11), record.Members[0].PersonID)
	assert.Equal(t, int64(21), record.Members[0].ParticipantID)
	assert.Equal(t, "alice@example.com", record.Members[0].UserID)
	require.Len(t, record.Blockers, 1)
	assert.Equal(t, int64(12), record.Blockers[0].PersonID)
	assert.Equal(t, "no_identity_on_account", record.Blockers[0].Code)
}
