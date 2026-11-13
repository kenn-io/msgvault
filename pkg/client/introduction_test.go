package client

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestGeneratedIntroductionClientDecodesListAndDetail(t *testing.T) {
	for _, tc := range []struct {
		name             string
		meetingMessageID *int64
	}{{"omitted context", nil}, {"present context", new(int64(42))}} {
		t.Run(tc.name, func(t *testing.T) {
			checkGeneratedIntroductionClient(t, tc.meetingMessageID)
		})
	}
}

func checkGeneratedIntroductionClient(t *testing.T, meetingMessageID *int64) {
	t.Helper()
	record := `{"intro_id":"intro-0123456789abcdef0123456789abcdef","source_id":7,"person_a_id":11,"person_b_id":12,"status":"prepared","revision":2,"title":"","body":"","created_by":"owner","created_at":"2026-11-12T10:00:00Z","updated_at":"2026-11-12T11:00:00Z","cancelled_at":null,"members":[{"person_id":11,"participant_id":21,"user_id":""}],"blockers":[{"person_id":12,"code":"no_identity_on_account"}]}`
	if meetingMessageID != nil {
		record = strings.Replace(record, `"source_id":7`, fmt.Sprintf(`"source_id":7,"meeting_message_id":%d`, *meetingMessageID), 1)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/intros":
			assert.Equal(t, "7", r.URL.Query().Get("source_id"))
			assert.Equal(t, "10", r.URL.Query().Get("limit"))
			assert.Equal(t, "previous-cursor", r.URL.Query().Get("after_id"))
			_, _ = w.Write([]byte(`{"items":[` + record + `],"has_more":false,"next_after_id":""}`))
		case "/api/v1/intros/intro-0123456789abcdef0123456789abcdef":
			cancelled := strings.Replace(record, `"status":"prepared"`, `"status":"cancelled"`, 1)
			cancelled = strings.Replace(cancelled, `"cancelled_at":null`, `"cancelled_at":"2026-11-12T11:00:00Z"`, 1)
			_, _ = w.Write([]byte(cancelled))
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
	assert.False(t, list.JSON200.HasMore)
	assert.Empty(t, list.JSON200.NextAfterID)
	require.NoError(t, list.JSON200.Validate())
	assertIntroductionRecord(t, list.JSON200.Items[0])
	assert.Equal(t, meetingMessageID, list.JSON200.Items[0].MeetingMessageID)
	encoded, err := json.Marshal(list.JSON200.Items[0])
	require.NoError(t, err)
	assert.JSONEq(t, record, string(encoded))
	invalid := list.JSON200.Items[0]
	for _, id := range []int64{0, -1} {
		invalid.MeetingMessageID = &id
		require.Error(t, invalid.Validate(), "context IDs must be positive")
	}

	detail, err := client.GetIntroductionWithResponse(t.Context(), &generated.GetIntroductionRequestOptions{
		PathParams: &generated.GetIntroductionPath{ID: "intro-0123456789abcdef0123456789abcdef"},
	})
	require.NoError(t, err)
	require.NotNil(t, detail.JSON200)
	assert.Equal(t, "cancelled", detail.JSON200.Status)
	assert.Equal(t, meetingMessageID, detail.JSON200.MeetingMessageID)
	require.NoError(t, detail.JSON200.Validate())
}

func assertIntroductionRecord(t *testing.T, record generated.IntroductionRecord) {
	t.Helper()
	assert.Equal(t, "intro-0123456789abcdef0123456789abcdef", record.IntroID)
	assert.Equal(t, int64(7), record.SourceID)
	assert.Equal(t, int64(11), record.PersonAId)
	assert.Equal(t, int64(12), record.PersonBId)
	assert.Equal(t, "prepared", record.Status)
	assert.Equal(t, int64(2), record.Revision)
	assert.Empty(t, record.Title)
	assert.Empty(t, record.Body)
	assert.Equal(t, "owner", record.CreatedBy)
	assert.Equal(t, time.Date(2026, time.November, 12, 10, 0, 0, 0, time.UTC), record.CreatedAt)
	assert.Equal(t, time.Date(2026, time.November, 12, 11, 0, 0, 0, time.UTC), record.UpdatedAt)
	assert.Nil(t, record.CancelledAt)
	require.Len(t, record.Members, 1)
	assert.Equal(t, int64(11), record.Members[0].PersonID)
	assert.Equal(t, int64(21), record.Members[0].ParticipantID)
	assert.Empty(t, record.Members[0].UserID)
	require.Len(t, record.Blockers, 1)
	assert.Equal(t, int64(12), record.Blockers[0].PersonID)
	assert.Equal(t, "no_identity_on_account", record.Blockers[0].Code)
}
