package store_test

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPersonCardMeetingsNewestFirstPagesAndPreservesDefault(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	ctx := t.Context()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	source, err := st.GetOrCreateSource("meeting_import", "meetings@example.test")
	requirements.NoError(err)
	var ids []int64
	for index := range 102 {
		// The two newest notes share a timestamp, exercising the ref tie-break.
		at := start.Add(time.Duration(min(index, 100)) * time.Hour)
		key := fmt.Sprintf("meeting-%03d", index)
		id, err := st.PersistMessageContext(ctx, &store.MessagePersistData{
			Message: &store.Message{
				SourceID: source.ID, SourceMessageID: key, MessageType: "meeting_transcript",
				SentAt: sql.NullTime{Time: at, Valid: true}, Subject: sql.NullString{String: "Synthetic meeting", Valid: true},
			},
			Conversation: &store.ConversationPersistData{SourceConversationID: key, ConversationType: "meeting", Title: "Synthetic meeting"},
			RawMIME:      []byte(`{"attendees":[{"email":"guest@example.test"}],"transcript":"Synthetic evidence"}`), RawFormat: "meeting_json",
		})
		requirements.NoError(err)
		ids = append(ids, id)
	}
	query := store.PersonMeetingsQuery{Emails: []string{"guest@example.test"}, From: start, To: end, Limit: 100}
	oldest, oldestCursor, err := st.ListPersonMeetingsContext(ctx, store.OwnerScope(), query)
	requirements.NoError(err)
	requirements.Len(oldest, 100)
	requirements.NotEmpty(oldestCursor)
	assertions.Equal(fmt.Sprintf("msg:%d", ids[0]), oldest[0].Ref)
	assertions.Equal(start.Add(99*time.Hour), oldest[99].Start)
	assertions.True(oldest[0].HasTranscript)

	// This legacy fingerprint is SHA-256 of the pre-sort-option wire query:
	// owner scope, no participant IDs, one email, and the explicit window above.
	legacy := struct {
		From  time.Time `json:"From"`
		To    time.Time `json:"To"`
		Start time.Time `json:"Start"`
		Ref   string    `json:"Ref"`
		Query string    `json:"Query"`
	}{start, end, oldest[99].Start, oldest[99].Ref, "6b4f50eae85946dbef6412a2ec46424acd6c12c99a7438f1de77f7fceae3ca2e"}
	encoded, err := json.Marshal(legacy)
	requirements.NoError(err)
	query.Cursor = base64.RawURLEncoding.EncodeToString(encoded)
	lastOldest, cursor, err := st.ListPersonMeetingsContext(ctx, store.OwnerScope(), query)
	requirements.NoError(err, "existing ascending cursors remain usable")
	requirements.Len(lastOldest, 2)
	assertions.Empty(cursor)
	assertions.Equal(start.Add(100*time.Hour), lastOldest[0].Start)
	assertions.Equal(start.Add(100*time.Hour), lastOldest[1].Start)

	query.Cursor = ""
	query.NewestFirst = true
	newest, newestCursor, err := st.ListPersonMeetingsContext(ctx, store.OwnerScope(), query)
	requirements.NoError(err)
	requirements.Len(newest, 100)
	requirements.NotEmpty(newestCursor)
	assertions.Equal(start.Add(100*time.Hour), newest[0].Start)
	assertions.Equal(start.Add(100*time.Hour), newest[1].Start)
	assertions.Less(newest[0].Ref, newest[1].Ref, "equal dates retain ascending ref order")
	assertions.Equal(start.Add(2*time.Hour), newest[99].Start)
	query.Cursor = newestCursor
	lastNewest, cursor, err := st.ListPersonMeetingsContext(ctx, store.OwnerScope(), query)
	requirements.NoError(err)
	requirements.Len(lastNewest, 2)
	assertions.Empty(cursor)
	assertions.Equal(start.Add(time.Hour), lastNewest[0].Start)
	assertions.Equal(start, lastNewest[1].Start)

	query.NewestFirst = false
	_, _, err = st.ListPersonMeetingsContext(ctx, store.OwnerScope(), query)
	requirements.ErrorIs(err, store.ErrInvalidArgument, "descending cursors cannot resume ascending reads")
	query.NewestFirst = true
	query.Cursor = oldestCursor
	_, _, err = st.ListPersonMeetingsContext(ctx, store.OwnerScope(), query)
	requirements.ErrorIs(err, store.ErrInvalidArgument, "ascending cursors cannot resume descending reads")

	query.Cursor = ""
	query.Limit = 1
	firstTie, cursor, err := st.ListPersonMeetingsContext(ctx, store.OwnerScope(), query)
	requirements.NoError(err)
	requirements.Len(firstTie, 1)
	requirements.NotEmpty(cursor)
	query.Cursor = cursor
	secondTie, _, err := st.ListPersonMeetingsContext(ctx, store.OwnerScope(), query)
	requirements.NoError(err)
	requirements.Len(secondTie, 1)
	assertions.Equal(firstTie[0].Start, secondTie[0].Start)
	assertions.Less(firstTie[0].Ref, secondTie[0].Ref, "cursor resumes inside a tied timestamp")
}
