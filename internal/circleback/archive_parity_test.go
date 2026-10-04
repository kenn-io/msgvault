package circleback

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// parityTranscript42 adds an entry with neither speaker nor speakerName.
const parityTranscript42 = `{
	"meetingId": 42,
	"transcript": [
		{"speaker": "Alice Smith", "text": "Welcome to the design review.", "start": 0},
		{"speaker": "Bob Jones", "text": "The mockups are ready.", "start": 65},
		{"text": "No speaker here", "start": 130}
	]
}`

// parityMeeting43 has a padded mixed-case organizer and one attendee listed
// twice in different case.
const parityMeeting43 = `{
	"id": 43,
	"name": "Budget Sync",
	"createdAt": "2026-06-11T09:05:00Z",
	"startTime": "2026-06-11T09:00:00Z",
	"endTime": "2026-06-11T09:30:00Z",
	"durationSeconds": 1800,
	"organizer": {"name": "Alice Smith", "email": " Alice@Example.COM "},
	"attendees": [
		{"name": "Bob Jones", "email": "bob@example.com"},
		{"name": "Bob Jones", "email": "Bob@Example.com"}
	],
	"notes": "Budget is on track."
}`

const parityTranscript43 = `{
	"meetingId": 43,
	"transcript": [
		{"speaker": "Bob Jones", "text": "Numbers are final.", "start": 5}
	]
}`

type circlebackArchiveRow struct {
	Subject                  string
	Snippet                  string
	SizeEstimate             int64
	SentAt                   string
	IsFromMe                 bool
	SourceIsFromMe           sql.NullBool
	IdentityIsFromMe         bool
	Sender                   string
	MessageType              string
	Body                     string
	Raw                      string
	RawFormat                string
	ConversationKey          string
	ConversationType         string
	ConversationTitle        string
	MessageCount             int64
	ParticipantCount         int64
	ConversationParticipants []string
	Recipients               []string
}

type circlebackArchive struct {
	Rows         map[string]circlebackArchiveRow
	Metadata     map[string]string
	Participants []string
	Counts       map[string]int
}

// circlebackArchiveSnapshot identifies participants by email, never by ID,
// so the same expectations hold on SQLite and PostgreSQL.
func circlebackArchiveSnapshot(t *testing.T, st *store.Store) circlebackArchive {
	t.Helper()
	require := require.New(t)
	archive := circlebackArchive{
		Rows:     map[string]circlebackArchiveRow{},
		Metadata: map[string]string{},
		Counts:   map[string]int{},
	}
	ids := map[string]int64{}
	func() {
		rows, err := st.DB().Query(`
			SELECT m.id, m.source_message_id, COALESCE(m.subject, ''), COALESCE(m.snippet, ''),
			       m.size_estimate, m.sent_at, m.is_from_me, m.source_is_from_me, m.identity_is_from_me,
			       COALESCE(p.email_address, ''), m.message_type, m.metadata,
			       c.source_conversation_id, c.conversation_type, COALESCE(c.title, ''),
			       c.message_count, c.participant_count
			FROM messages m
			JOIN conversations c ON c.id = m.conversation_id
			LEFT JOIN participants p ON p.id = m.sender_id
			ORDER BY m.source_message_id`)
		require.NoError(err)
		defer func() { require.NoError(rows.Close()) }()
		for rows.Next() {
			var id int64
			var key string
			var metadata sql.NullString
			var sentAt time.Time
			var row circlebackArchiveRow
			require.NoError(rows.Scan(&id, &key, &row.Subject, &row.Snippet, &row.SizeEstimate, &sentAt,
				&row.IsFromMe, &row.SourceIsFromMe, &row.IdentityIsFromMe, &row.Sender, &row.MessageType, &metadata,
				&row.ConversationKey, &row.ConversationType, &row.ConversationTitle,
				&row.MessageCount, &row.ParticipantCount))
			row.SentAt = sentAt.UTC().Format(time.RFC3339)
			ids[key] = id
			archive.Rows[key] = row
			archive.Metadata[key] = metadata.String
		}
		require.NoError(rows.Err())
	}()

	for key, id := range ids {
		row := archive.Rows[key]
		row.Body = circlebackMessageBody(t, st, id)
		raw, err := st.GetMessageRaw(id)
		require.NoError(err)
		row.Raw = string(raw)
		require.NoError(st.DB().QueryRow(st.Rebind(
			`SELECT raw_format FROM message_raw WHERE message_id = ?`), id).Scan(&row.RawFormat))
		row.ConversationParticipants = circlebackStringRows(t, st, `
			SELECT p.email_address || ':' || COALESCE(cp.role, '')
			FROM conversation_participants cp
			JOIN messages m ON m.conversation_id = cp.conversation_id
			JOIN participants p ON p.id = cp.participant_id
			WHERE m.id = ?
			ORDER BY 1`, id)
		row.Recipients = circlebackStringRows(t, st, `
			SELECT mr.recipient_type || ':' || p.email_address || ':' || COALESCE(mr.display_name, '')
			FROM message_recipients mr
			JOIN participants p ON p.id = mr.participant_id
			WHERE mr.message_id = ?
			ORDER BY 1`, id)
		archive.Rows[key] = row
	}
	archive.Participants = circlebackStringRows(t, st, `
		SELECT COALESCE(email_address, '') || ':' || COALESCE(display_name, '') || ':' || COALESCE(domain, '')
		FROM participants
		ORDER BY 1`)
	for _, table := range []string{"messages", "conversations", "participants", "conversation_participants", "message_recipients"} {
		var count int
		require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM "+table).Scan(&count), table)
		archive.Counts[table] = count
	}
	return archive
}

func circlebackFTSRow(t *testing.T, st *store.Store, sourceMessageID string) []string {
	t.Helper()
	var subject, body, from, to string
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT f.subject, f.body, f.from_addr, f.to_addr
		FROM messages_fts f
		JOIN messages m ON m.id = f.rowid
		WHERE m.source_message_id = ?`), sourceMessageID).Scan(&subject, &body, &from, &to))
	return []string{subject, body, from, to}
}

// TestCirclebackArchiveRowsMatchBase pins the rows Circleback writes after a
// create, a snapshot-hash skip, and a forced rewrite. The expected values were
// captured from the provider's own write path before it moved onto
// meetingarchive.
func TestCirclebackArchiveRowsMatchBase(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := &fakeSource{
		meetings: map[string]json.RawMessage{
			"42": json.RawMessage(meeting42),
			"43": json.RawMessage(parityMeeting43),
		},
		transcripts: map[string]json.RawMessage{
			"42": json.RawMessage(parityTranscript42),
			"43": json.RawMessage(parityTranscript43),
		},
		orderedIDs: []string{"42", "43"},
	}
	imp, st := newTestImporter(t, f)

	body42 := "Design Review\nWhen: 2026-06-10 17:00\nAttendees: Alice Smith, Bob Jones, Guest Speaker\n\n## Decisions\n- Ship the new layout\n\nAction items:\n- Update mockups (Bob Jones) [pending]\n\nTags: design\n\nTranscript:\n[00:00] Alice Smith: Welcome to the design review.\n[01:05] Bob Jones: The mockups are ready.\n[02:10] Unknown: No speaker here"
	body43 := "Budget Sync\nWhen: 2026-06-11 09:00\nAttendees: Bob Jones, Bob Jones\n\nBudget is on track.\n\nTranscript:\n[00:05] Bob Jones: Numbers are final."
	want := map[string]circlebackArchiveRow{
		"meeting:42": {
			Subject:                  "Design Review",
			Snippet:                  string([]rune(body42)[:200]),
			SizeEstimate:             328,
			SentAt:                   "2026-06-10T17:00:00Z",
			IsFromMe:                 true,
			SourceIsFromMe:           sql.NullBool{Bool: false, Valid: true},
			IdentityIsFromMe:         true,
			Sender:                   "alice@example.com",
			MessageType:              "meeting_transcript",
			Body:                     body42,
			Raw:                      `{"meeting":{"id":42,"name":"Design Review","createdAt":"2026-06-10T17:05:00Z","startTime":"2026-06-10T17:00:00Z","endTime":"2026-06-10T17:45:00Z","durationSeconds":2700,"organizer":{"name":"Alice Smith","email":"alice@example.com"},"attendees":[{"name":"Alice Smith","email":"alice@example.com"},{"name":"Bob Jones","email":"bob@example.com"},{"name":"Guest Speaker"}],"notes":"## Decisions\n- Ship the new layout","actionItems":[{"title":"Update mockups","assignee":"Bob Jones","status":"pending"}],"tags":["design"],"meetingUrl":"https://meet.example.com/design","recordingUrl":"https://cdn.example.com/rec/42.mp4"},"transcript":{"meetingId":42,"transcript":[{"speaker":"Alice Smith","text":"Welcome to the design review.","start":0},{"speaker":"Bob Jones","text":"The mockups are ready.","start":65},{"text":"No speaker here","start":130}]}}`,
			RawFormat:                "circleback_json",
			ConversationKey:          "meeting:42",
			ConversationType:         "meeting",
			ConversationTitle:        "Design Review",
			MessageCount:             1,
			ParticipantCount:         2,
			ConversationParticipants: []string{"alice@example.com:member", "bob@example.com:member"},
			Recipients:               []string{"from:alice@example.com:Alice Smith", "to:alice@example.com:Alice Smith", "to:bob@example.com:Bob Jones"},
		},
		"meeting:43": {
			Subject:                  "Budget Sync",
			Snippet:                  body43,
			SizeEstimate:             138,
			SentAt:                   "2026-06-11T09:00:00Z",
			IsFromMe:                 true,
			SourceIsFromMe:           sql.NullBool{Bool: false, Valid: true},
			IdentityIsFromMe:         true,
			Sender:                   "alice@example.com",
			MessageType:              "meeting_transcript",
			Body:                     body43,
			Raw:                      `{"meeting":{"id":43,"name":"Budget Sync","createdAt":"2026-06-11T09:05:00Z","startTime":"2026-06-11T09:00:00Z","endTime":"2026-06-11T09:30:00Z","durationSeconds":1800,"organizer":{"name":"Alice Smith","email":" Alice@Example.COM "},"attendees":[{"name":"Bob Jones","email":"bob@example.com"},{"name":"Bob Jones","email":"Bob@Example.com"}],"notes":"Budget is on track."},"transcript":{"meetingId":43,"transcript":[{"speaker":"Bob Jones","text":"Numbers are final.","start":5}]}}`,
			RawFormat:                "circleback_json",
			ConversationKey:          "meeting:43",
			ConversationType:         "meeting",
			ConversationTitle:        "Budget Sync",
			MessageCount:             1,
			ParticipantCount:         1,
			ConversationParticipants: []string{"bob@example.com:member"},
			Recipients:               []string{"from:alice@example.com:Alice Smith", "to:bob@example.com:Bob Jones"},
		},
	}
	wantMetadata := map[string]string{
		"meeting:42": `{"platform":"circleback","meeting_id":"42","created_at":"2026-06-10T17:05:00Z","scheduled_start":"2026-06-10T17:00:00Z","scheduled_end":"2026-06-10T17:45:00Z","duration_seconds":2700,"organizer_email":"alice@example.com","meeting_url":"https://meet.example.com/design","recording_url":"https://cdn.example.com/rec/42.mp4","recording_url_fetched_at":"2026-07-09T12:00:00Z","tags":["design"],"action_items":[{"title":"Update mockups","status":"pending","assignee":"Bob Jones"}],"transcript_state":"present","transcript_segments":3,"account_identifier":"alice@example.com","snapshot_hash":"20a29e085d64474f165855ab580e15b15d25565e651e68c28373cb4f2b692155"}`,
		"meeting:43": `{"platform":"circleback","meeting_id":"43","created_at":"2026-06-11T09:05:00Z","scheduled_start":"2026-06-11T09:00:00Z","scheduled_end":"2026-06-11T09:30:00Z","duration_seconds":1800,"organizer_email":"alice@example.com","transcript_state":"present","transcript_segments":1,"account_identifier":"alice@example.com","snapshot_hash":"443ccca2ab81ff495e082587bef0a14a16a423b13041efcb7594e84f9266b225"}`,
	}
	wantParticipants := []string{"alice@example.com:Alice Smith:example.com", "bob@example.com:Bob Jones:example.com"}
	wantCounts := map[string]int{"messages": 2, "conversations": 2, "participants": 2, "conversation_participants": 3, "message_recipients": 5}
	wantFTS := []string{"Design Review", body42, "alice@example.com", "alice@example.com bob@example.com"}

	check := func(label string) {
		got := circlebackArchiveSnapshot(t, st)
		for key, metadata := range got.Metadata {
			assert.JSONEq(wantMetadata[key], metadata, "%s metadata %s", label, key)
		}
		assert.Equal(want, got.Rows, label)
		assert.Equal(wantParticipants, got.Participants, label)
		assert.Equal(wantCounts, got.Counts, label)
		assert.Contains(got.Rows["meeting:42"].Body, "[02:10] Unknown: No speaker here", label)
		if st.FTS5Available() && !st.IsPostgreSQL() {
			assert.Equal(wantFTS, circlebackFTSRow(t, st, "meeting:42"), label)
		}
	}

	initial, err := imp.Import(context.Background(), ImportOptions{
		Identifier: "alice@example.com", AccountEmail: "alice@example.com",
	})
	require.NoError(err)
	assert.EqualValues(2, initial.MeetingsAdded)
	check("initial")

	incremental, err := imp.Import(context.Background(), ImportOptions{
		Identifier: "alice@example.com", AccountEmail: "alice@example.com",
	})
	require.NoError(err)
	assert.EqualValues(0, incremental.MeetingsAdded)
	assert.EqualValues(0, incremental.MeetingsUpdated, "matching snapshot hashes skip the write")
	check("incremental")

	forced, err := imp.Import(context.Background(), ImportOptions{
		Identifier: "alice@example.com", AccountEmail: "alice@example.com", Full: true,
	})
	require.NoError(err)
	assert.EqualValues(0, forced.MeetingsAdded)
	assert.EqualValues(2, forced.MeetingsUpdated)
	check("full")
}

// TestCirclebackCountsWriteWhenStatsMaintenanceFails pins that a committed
// meeting still counts as added when conversation-stat maintenance fails.
func TestCirclebackCountsWriteWhenStatsMaintenanceFails(t *testing.T) {
	testutil.SkipIfPostgres(t, "uses a SQLite trigger to fail conversation stat maintenance")
	assert := assert.New(t)
	require := require.New(t)
	f := &fakeSource{
		meetings:    map[string]json.RawMessage{"42": json.RawMessage(meeting42)},
		transcripts: map[string]json.RawMessage{"42": json.RawMessage(transcript42)},
	}
	imp, st := newTestImporter(t, f)
	_, err := st.DB().Exec(`
		CREATE TRIGGER fail_circleback_stats
		BEFORE UPDATE OF participant_count ON conversations
		BEGIN
			SELECT RAISE(ABORT, 'forced stats failure');
		END
	`)
	require.NoError(err)

	sum, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})

	require.Error(err)
	require.NotNil(sum)
	assert.EqualValues(1, sum.MeetingsAdded)
	var messages int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
	assert.Equal(1, messages)
}

// TestCirclebackAdoptsArchiverRules covers the meetingarchive rules Circleback
// meetings gain by saving through Upsert: envelope addresses on recipient rows
// and one FTS entry for an attendee listed twice.
func TestCirclebackAdoptsArchiverRules(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := &fakeSource{
		meetings:    map[string]json.RawMessage{"43": json.RawMessage(parityMeeting43)},
		transcripts: map[string]json.RawMessage{"43": json.RawMessage(parityTranscript43)},
	}
	imp, st := newTestImporter(t, f)

	_, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com", AccountEmail: "alice@example.com"})
	require.NoError(err)

	assert.Equal([]string{"from:alice@example.com:alice@example.com", "to:bob@example.com:bob@example.com"}, circlebackStringRows(t, st, `
		SELECT mr.recipient_type || ':' || p.email_address || ':' || COALESCE(mr.email_address, '')
		FROM message_recipients mr
		JOIN participants p ON p.id = mr.participant_id
		ORDER BY 1`))
	if st.FTS5Available() && !st.IsPostgreSQL() {
		assert.Equal("bob@example.com", circlebackFTSRow(t, st, "meeting:43")[3])
	}
}
