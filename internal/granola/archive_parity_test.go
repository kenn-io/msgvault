package granola

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

const parityNoteID = "not_Pp11Qq22Rr33Ss"

// parityNote has no calendar event, a padded mixed-case owner, a duplicated
// attendee in different case, and a transcript segment without timestamps.
func parityNote(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"id":         parityNoteID,
		"object":     "note",
		"title":      "Roadmap Check-in",
		"owner":      map[string]any{"name": "Alice Smith", "email": " Alice@Example.COM "},
		"created_at": "2026-06-03T10:00:00Z",
		"updated_at": "2026-06-03T10:40:00Z",
		"attendees": []map[string]any{
			{"name": "Dana Lee", "email": "dana@example.com"},
			{"name": "Dana Lee", "email": "Dana@Example.com"},
		},
		"summary_text": "Dana confirmed the roadmap dates.",
		"transcript": []map[string]any{
			{"speaker": map[string]any{"source": "microphone"}, "text": "Dates look right."},
		},
	})
	require.NoError(t, err)
	return raw
}

type granolaArchiveRow struct {
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

type granolaArchive struct {
	Rows         map[string]granolaArchiveRow
	Metadata     map[string]string
	Participants []string
	Counts       map[string]int
}

func granolaArchiveSnapshot(t *testing.T, st *store.Store) granolaArchive {
	t.Helper()
	require := require.New(t)
	archive := granolaArchive{
		Rows:     map[string]granolaArchiveRow{},
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
			var row granolaArchiveRow
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
		require.NoError(st.DB().QueryRow(st.Rebind(
			`SELECT body_text FROM message_bodies WHERE message_id = ?`), id).Scan(&row.Body))
		raw, err := st.GetMessageRaw(id)
		require.NoError(err)
		row.Raw = string(raw)
		require.NoError(st.DB().QueryRow(st.Rebind(
			`SELECT raw_format FROM message_raw WHERE message_id = ?`), id).Scan(&row.RawFormat))
		row.ConversationParticipants = parityStrings(t, st, `
			SELECT p.email_address || ':' || COALESCE(cp.role, '')
			FROM conversation_participants cp
			JOIN messages m ON m.conversation_id = cp.conversation_id
			JOIN participants p ON p.id = cp.participant_id
			WHERE m.id = ?
			ORDER BY 1`, id)
		row.Recipients = parityStrings(t, st, `
			SELECT mr.recipient_type || ':' || p.email_address || ':' || COALESCE(mr.display_name, '')
			FROM message_recipients mr
			JOIN participants p ON p.id = mr.participant_id
			WHERE mr.message_id = ?
			ORDER BY 1`, id)
		archive.Rows[key] = row
	}
	archive.Participants = parityStrings(t, st, `
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

func granolaFTSRow(t *testing.T, st *store.Store, sourceMessageID string) []string {
	t.Helper()
	var subject, body, from, to string
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT f.subject, f.body, f.from_addr, f.to_addr
		FROM messages_fts f
		JOIN messages m ON m.id = f.rowid
		WHERE m.source_message_id = ?`), sourceMessageID).Scan(&subject, &body, &from, &to))
	return []string{subject, body, from, to}
}

func parityStrings(t *testing.T, st *store.Store, query string, args ...any) []string {
	t.Helper()
	rows, err := st.DB().Query(st.Rebind(query), args...)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var values []string
	for rows.Next() {
		var value string
		require.NoError(t, rows.Scan(&value))
		values = append(values, value)
	}
	require.NoError(t, rows.Err())
	return values
}

// TestGranolaArchiveRowsMatchBase pins the rows Granola writes after a create,
// a no-op re-ingest, and a forced rewrite. The expected values were captured
// from the provider's own write path before it moved onto meetingarchive.
func TestGranolaArchiveRowsMatchBase(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fullRaw := loadFixture(t, "note_full.json")
	parityRaw := parityNote(t)
	api := &fakeAPI{
		notes: map[string][]byte{
			"not_Ab12Cd34Ef56Gh": fullRaw,
			parityNoteID:         parityRaw,
		},
		orderedIDs: []string{"not_Ab12Cd34Ef56Gh", parityNoteID},
	}
	imp, st := newTestImporter(t, api)

	fullBody := "Quarterly Planning Review\nWhen: 2026-06-01 15:00 - 16:00\nAttendees: Alice Smith, Bob Jones\n\n## Quarterly Planning Review\n\n- Agreed on **three priorities**\n- Budget approved\n\nTranscript:\n[00:00] Alice Smith: Let's get started with the quarterly review.\n[01:11] Bob Jones: Sounds good. I have the budget numbers ready.\n[1:01:32] Speaker C: The deadline for phase one is July fifteenth."
	parityBody := "Roadmap Check-in\nAttendees: Dana Lee, Dana Lee\n\nDana confirmed the roadmap dates.\n\nTranscript:\n[00:00] Me: Dates look right."
	wantFull := granolaArchiveRow{
		Subject:                  "Quarterly Planning Review",
		Snippet:                  fullBody[:strings.Index(fullBody, "[00:00] Alice ")+len("[00:00] Alice ")],
		SizeEstimate:             383,
		SentAt:                   "2026-06-01T15:00:00Z",
		IsFromMe:                 false,
		SourceIsFromMe:           sql.NullBool{Bool: false, Valid: true},
		IdentityIsFromMe:         false,
		Sender:                   "bob@example.com",
		MessageType:              "meeting_transcript",
		Body:                     fullBody,
		Raw:                      string(fullRaw),
		RawFormat:                "granola_json",
		ConversationKey:          "meeting:not_Ab12Cd34Ef56Gh",
		ConversationType:         "meeting",
		ConversationTitle:        "Quarterly Planning Review",
		MessageCount:             1,
		ParticipantCount:         3,
		ConversationParticipants: []string{"alice@example.com:member", "bob@example.com:member", "carol@example.com:member"},
		Recipients:               []string{"from:bob@example.com:Bob Jones", "to:alice@example.com:Alice Smith", "to:bob@example.com:Bob Jones", "to:carol@example.com:"},
	}
	wantParity := granolaArchiveRow{
		Subject:                  "Roadmap Check-in",
		Snippet:                  parityBody,
		SizeEstimate:             124,
		SentAt:                   "2026-06-03T10:00:00Z",
		IsFromMe:                 true,
		SourceIsFromMe:           sql.NullBool{Bool: false, Valid: true},
		IdentityIsFromMe:         true,
		Sender:                   "alice@example.com",
		MessageType:              "meeting_transcript",
		Body:                     parityBody,
		Raw:                      string(parityRaw),
		RawFormat:                "granola_json",
		ConversationKey:          "meeting:" + parityNoteID,
		ConversationType:         "meeting",
		ConversationTitle:        "Roadmap Check-in",
		MessageCount:             1,
		ParticipantCount:         1,
		ConversationParticipants: []string{"dana@example.com:member"},
		Recipients:               []string{"from:alice@example.com:Alice Smith", "to:dana@example.com:Dana Lee"},
	}
	wantMetadata := map[string]string{
		"not_Ab12Cd34Ef56Gh": `{"platform":"granola","note_id":"not_Ab12Cd34Ef56Gh","web_url":"https://notes.example.com/d/f3e45e0f-24cc-480b-9a6c-8b1f5e3d7a2c","created_at":"2026-06-01T15:02:11Z","updated_at":"2026-06-01T16:45:00Z","scheduled_start":"2026-06-01T15:00:00Z","scheduled_end":"2026-06-01T16:00:00Z","duration_seconds":3600,"organizer_email":"bob@example.com","calendar_event_id":"evt_2su99n6iiik37_20260601T150000Z","folders":["Planning"],"transcript_segments":3,"account_identifier":"alice@example.com"}`,
		parityNoteID:         `{"platform":"granola","note_id":"not_Pp11Qq22Rr33Ss","created_at":"2026-06-03T10:00:00Z","updated_at":"2026-06-03T10:40:00Z","organizer_email":"alice@example.com","transcript_segments":1,"account_identifier":"alice@example.com"}`,
	}
	wantParticipantsAfterFirst := []string{"alice@example.com:Alice Smith:example.com", "bob@example.com:Bob Jones:example.com", "carol@example.com::example.com"}
	wantParticipants := []string{"alice@example.com:Alice Smith:example.com", "bob@example.com:Bob Jones:example.com", "carol@example.com::example.com", "dana@example.com:Dana Lee:example.com"}
	wantFTS := []string{"Quarterly Planning Review", fullBody, "bob@example.com", "alice@example.com bob@example.com carol@example.com"}

	check := func(label string, want map[string]granolaArchiveRow, participants []string, counts map[string]int) {
		got := granolaArchiveSnapshot(t, st)
		for key, metadata := range got.Metadata {
			assert.JSONEq(wantMetadata[key], metadata, "%s metadata %s", label, key)
		}
		assert.Equal(want, got.Rows, label)
		assert.Equal(participants, got.Participants, label)
		assert.Equal(counts, got.Counts, label)
		if st.FTS5Available() && !st.IsPostgreSQL() {
			assert.Equal(wantFTS, granolaFTSRow(t, st, "not_Ab12Cd34Ef56Gh"), label)
		}
	}

	first, err := imp.Import(context.Background(), ImportOptions{
		Identifier: "alice@example.com", AccountEmail: "alice@example.com", Limit: 1,
	})
	require.NoError(err)
	assert.EqualValues(1, first.NotesAdded)
	check("limited", map[string]granolaArchiveRow{"not_Ab12Cd34Ef56Gh": wantFull}, wantParticipantsAfterFirst,
		map[string]int{"messages": 1, "conversations": 1, "participants": 3, "conversation_participants": 3, "message_recipients": 4})

	second, err := imp.Import(context.Background(), ImportOptions{
		Identifier: "alice@example.com", AccountEmail: "alice@example.com",
	})
	require.NoError(err)
	assert.EqualValues(1, second.NotesAdded)
	both := map[string]granolaArchiveRow{"not_Ab12Cd34Ef56Gh": wantFull, parityNoteID: wantParity}
	wantCounts := map[string]int{"messages": 2, "conversations": 2, "participants": 4, "conversation_participants": 4, "message_recipients": 6}
	check("unlimited", both, wantParticipants, wantCounts)

	// A stale derived preview with unchanged evidence is left alone by an
	// incremental sync; --full must rewrite it.
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET snippet = ? WHERE source_message_id = ?`), "stale", parityNoteID)
	require.NoError(err)
	forced, err := imp.Import(context.Background(), ImportOptions{
		Identifier: "alice@example.com", AccountEmail: "alice@example.com", Full: true,
	})
	require.NoError(err)
	assert.EqualValues(0, forced.NotesAdded)
	assert.EqualValues(2, forced.NotesUpdated)
	check("full", both, wantParticipants, wantCounts)
}

// TestGranolaCountsWriteWhenStatsMaintenanceFails pins that a committed note
// still counts as added when conversation-stat maintenance fails afterwards.
func TestGranolaCountsWriteWhenStatsMaintenanceFails(t *testing.T) {
	testutil.SkipIfPostgres(t, "uses a SQLite trigger to fail conversation stat maintenance")
	assert := assert.New(t)
	require := require.New(t)
	api := &fakeAPI{notes: map[string][]byte{"not_Ab12Cd34Ef56Gh": loadFixture(t, "note_full.json")}}
	imp, st := newTestImporter(t, api)
	_, err := st.DB().Exec(`
		CREATE TRIGGER fail_granola_stats
		BEFORE UPDATE OF participant_count ON conversations
		BEGIN
			SELECT RAISE(ABORT, 'forced stats failure');
		END
	`)
	require.NoError(err)

	sum, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})

	require.Error(err)
	require.NotNil(sum)
	assert.EqualValues(1, sum.NotesAdded)
	var messages int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
	assert.Equal(1, messages)
}

// TestGranolaAdoptsArchiverRules covers the meetingarchive rules Granola notes
// gain by saving through Upsert: envelope addresses on recipient rows, trimmed
// display names, invalid addresses dropped, and one FTS entry per attendee.
func TestGranolaAdoptsArchiverRules(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw, err := json.Marshal(map[string]any{
		"id":         "not_Rules1",
		"title":      "Rules Check",
		"owner":      map[string]any{"name": " Bob Jones ", "email": "bob@example.com"},
		"created_at": "2026-06-04T10:00:00Z",
		"updated_at": "2026-06-04T10:30:00Z",
		"attendees": []map[string]any{
			{"name": "Carol Diaz", "email": "carol@example.com"},
			{"name": "Carol Diaz", "email": "Carol@Example.com"},
			{"name": "Not An Address", "email": "not-an-email"},
		},
		"summary_text": "Rules summary.",
	})
	require.NoError(err)
	imp, st := newTestImporter(t, &fakeAPI{notes: map[string][]byte{"not_Rules1": raw}})

	_, err = imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com", AccountEmail: "alice@example.com"})
	require.NoError(err)

	assert.Equal([]string{"from:bob@example.com:Bob Jones", "to:carol@example.com:Carol Diaz"},
		granolaArchiveSnapshot(t, st).Rows["not_Rules1"].Recipients)
	assert.Equal([]string{"from:bob@example.com:bob@example.com", "to:carol@example.com:carol@example.com"}, parityStrings(t, st, `
		SELECT mr.recipient_type || ':' || p.email_address || ':' || COALESCE(mr.email_address, '')
		FROM message_recipients mr
		JOIN participants p ON p.id = mr.participant_id
		ORDER BY 1`))
	var invalid int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM participants WHERE email_address = ?`), "not-an-email").Scan(&invalid))
	assert.Zero(invalid, "a value without @ must not become a participant")
	if st.FTS5Available() && !st.IsPostgreSQL() {
		assert.Equal("carol@example.com", granolaFTSRow(t, st, "not_Rules1")[3])
	}
}
