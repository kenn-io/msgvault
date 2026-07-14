package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
)

func TestEscapeLike(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"plain text", "hello", "hello"},
		{"percent", "100% done", `100\% done`},
		{"underscore", "file_name", `file\_name`},
		{"backslash", `path\to`, `path\\to`},
		{"all special", `50%_off\sale`, `50\%\_off\\sale`},
		{"empty", "", ""},
		{"multiple percents", "%%", `\%\%`},
		{"adjacent specials", `%_\`, `\%\_\\`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, escapeLike(tt.input), "escapeLike(%q)", tt.input)
		})
	}
}

// openTestStore creates a temporary store for internal tests.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	require.NoError(t, err, "Open")
	require.NoError(t, st.InitSchema(), "InitSchema")
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// seedMessage inserts a message with the given subject and snippet, returning its ID.
// SentAt is left NULL so COALESCE returns NULL (avoids SQLite string-vs-time scan issue).
func seedMessage(t *testing.T, st *Store, sourceID, convID int64, sourceMessageID, subject, snippet string) int64 {
	t.Helper()
	id, err := st.UpsertMessage(&Message{
		ConversationID:  convID,
		SourceID:        sourceID,
		SourceMessageID: sourceMessageID,
		MessageType:     "email",
		Subject:         sql.NullString{String: subject, Valid: true},
		Snippet:         sql.NullString{String: snippet, Valid: true},
		SizeEstimate:    100,
	})
	require.NoError(t, err, "UpsertMessage(%q)", sourceMessageID)
	return id
}

func TestParseSQLiteTime(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  time.Time
	}{
		{
			"space-separated with fractional seconds and TZ",
			"2024-06-15 10:30:45.123456-07:00",
			time.Date(2024, 6, 15, 10, 30, 45, 123456000, time.FixedZone("", -7*3600)),
		},
		{
			"T-separated with fractional seconds and TZ",
			"2024-06-15T10:30:45.123456-07:00",
			time.Date(2024, 6, 15, 10, 30, 45, 123456000, time.FixedZone("", -7*3600)),
		},
		{
			"space-separated with fractional seconds no TZ",
			"2024-06-15 10:30:45.500",
			time.Date(2024, 6, 15, 10, 30, 45, 500000000, time.UTC),
		},
		{
			"T-separated with fractional seconds no TZ",
			"2024-06-15T10:30:45.500",
			time.Date(2024, 6, 15, 10, 30, 45, 500000000, time.UTC),
		},
		{
			"space-separated basic (datetime('now') format)",
			"2024-06-15 10:30:45",
			time.Date(2024, 6, 15, 10, 30, 45, 0, time.UTC),
		},
		{
			"T-separated basic",
			"2024-06-15T10:30:45",
			time.Date(2024, 6, 15, 10, 30, 45, 0, time.UTC),
		},
		{
			"space-separated without seconds",
			"2024-06-15 10:30",
			time.Date(2024, 6, 15, 10, 30, 0, 0, time.UTC),
		},
		{
			"T-separated without seconds",
			"2024-06-15T10:30",
			time.Date(2024, 6, 15, 10, 30, 0, 0, time.UTC),
		},
		{
			"date only",
			"2024-06-15",
			time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC),
		},
		{
			"RFC3339 with Z",
			"2024-06-15T10:30:45Z",
			time.Date(2024, 6, 15, 10, 30, 45, 0, time.UTC),
		},
		{
			"RFC3339 with offset",
			"2024-06-15T10:30:45+05:30",
			time.Date(2024, 6, 15, 10, 30, 45, 0, time.FixedZone("", 5*3600+30*60)),
		},
		{
			"RFC3339Nano",
			"2024-06-15T10:30:45.123456789Z",
			time.Date(2024, 6, 15, 10, 30, 45, 123456789, time.UTC),
		},
		{
			"empty string returns zero time",
			"",
			time.Time{},
		},
		{
			"garbage returns zero time",
			"not-a-date",
			time.Time{},
		},
		{
			"unix timestamp string returns zero time",
			"1718451045",
			time.Time{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseSQLiteTime(tt.input)
			assert.True(t, got.Equal(tt.want),
				"parseSQLiteTime(%q) = %v, want %v", tt.input, got, tt.want)
		})
	}
}

// TestNullableTimestampScan exercises the scanner used by ListMessages
// /SearchMessages /GetMessages for the COALESCE(sent_at, received_at,
// internal_date) expression. SQLite's go-sqlite3 driver can return
// that computed column as string or []byte (no declared datetime
// affinity); pgx/v5 always delivers time.Time for TIMESTAMP columns.
// The scanner must accept all of these without erroring.
func TestNullableTimestampScan(t *testing.T) {
	tref := time.Date(2024, 6, 15, 10, 30, 45, 0, time.UTC)

	tests := []struct {
		name      string
		src       any
		wantValid bool
		wantTime  time.Time
	}{
		{"nil", nil, false, time.Time{}},
		{"time.Time", tref, true, tref},
		{"zero time.Time treated as invalid", time.Time{}, false, time.Time{}},
		{"string SQLite datetime", "2024-06-15 10:30:45", true, tref},
		{"string RFC3339", "2024-06-15T10:30:45Z", true, tref},
		{"[]byte SQLite datetime", []byte("2024-06-15 10:30:45"), true, tref},
		{"empty string -> invalid", "", false, time.Time{}},
		{"unparseable string -> invalid", "not-a-date", false, time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var n nullableTimestamp
			require.NoError(t, n.Scan(tt.src), "Scan(%T %v)", tt.src, tt.src)
			assert.Equal(t, tt.wantValid, n.Valid, "Valid")
			assert.True(t, n.Time.Equal(tt.wantTime), "Time = %v, want %v", n.Time, tt.wantTime)
		})
	}

	t.Run("unsupported type errors", func(t *testing.T) {
		var n nullableTimestamp
		require.Error(t, n.Scan(42), "Scan(int): expected error")
	})
}

// TestContextAwareReadsHonorCancellation verifies the context-aware store read
// paths thread the caller's context into their SQL, so an abandoned or
// timed-out request is cancelled instead of running (including batch hydration)
// to completion on a background context.
func TestContextAwareReadsHonorCancellation(t *testing.T) {
	require := require.New(t)
	st := openTestStore(t)

	source, err := st.GetOrCreateSource("gmail", "test@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(source.ID, "thread-1", "Thread")
	require.NoError(err, "EnsureConversation")
	msgID := seedMessage(t, st, source.ID, convID, "msg-ctx", "Ctx test", "snippet")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err = st.ListMessagesContext(ctx, 0, 10)
	require.ErrorIs(err, context.Canceled, "ListMessagesContext must honor a cancelled context")

	_, err = st.GetMessageContext(ctx, msgID)
	require.ErrorIs(err, context.Canceled, "GetMessageContext must honor a cancelled context")

	_, err = st.GetMessagesSummariesByIDsContext(ctx, []int64{msgID})
	require.ErrorIs(err, context.Canceled, "GetMessagesSummariesByIDsContext must honor a cancelled context")

	_, err = st.GetStatsContext(ctx)
	require.ErrorIs(err, context.Canceled, "GetStatsContext must honor a cancelled context")

	_, _, err = st.SearchMessagesQueryContext(ctx, &search.Query{TextTerms: []string{"ctx"}}, 0, 10)
	require.ErrorIs(err, context.Canceled, "SearchMessagesQueryContext must honor a cancelled context")
}

func TestGetMessageCcBcc(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := openTestStore(t)

	source, err := st.GetOrCreateSource("gmail", "test@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(source.ID, "thread-1", "Thread")
	require.NoError(err, "EnsureConversation")
	msgID := seedMessage(t, st, source.ID, convID, "msg-cc-bcc", "CC/BCC test", "snippet")

	db := st.DB()

	// Insert participants
	for _, p := range []struct {
		id    int
		email string
	}{
		{1, "to@example.com"},
		{2, "cc1@example.com"},
		{3, "cc2@example.com"},
		{4, "bcc@example.com"},
	} {
		_, err := db.Exec(
			`INSERT INTO participants (id, email_address, domain, created_at, updated_at)
			 VALUES (?, ?, 'example.com', datetime('now'), datetime('now'))`,
			p.id, p.email,
		)
		require.NoError(err, "insert participant %s", p.email)
	}

	// Insert message_recipients
	for _, r := range []struct {
		participantID int
		recipientType string
	}{
		{1, "to"},
		{2, "cc"},
		{3, "cc"},
		{4, "bcc"},
	} {
		_, err := db.Exec(
			`INSERT INTO message_recipients (message_id, participant_id, recipient_type)
			 VALUES (?, ?, ?)`,
			msgID, r.participantID, r.recipientType,
		)
		require.NoError(err, "insert recipient %s", r.recipientType)
	}

	// Test GetMessage
	m, err := st.GetMessage(msgID)
	require.NoError(err, "GetMessage")
	assert.Equal(source.ID, m.SourceID, "SourceID")
	require.Len(m.To, 1)
	assert.Equal("to@example.com", m.To[0], "To[0]")
	gotCc := slices.Clone(m.Cc)
	slices.Sort(gotCc)
	wantCc := []string{"cc1@example.com", "cc2@example.com"}
	assert.True(slices.Equal(gotCc, wantCc), "Cc = %v, want %v", m.Cc, wantCc)
	require.Len(m.Bcc, 1)
	assert.Equal("bcc@example.com", m.Bcc[0], "Bcc[0]")
}

func TestListMessagesCcBcc(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := openTestStore(t)

	source, err := st.GetOrCreateSource("gmail", "test@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(source.ID, "thread-1", "Thread")
	require.NoError(err, "EnsureConversation")
	msgID := seedMessage(t, st, source.ID, convID, "msg-list-cc", "List CC test", "snippet")

	db := st.DB()

	// Insert CC and BCC participants
	for _, p := range []struct {
		id    int
		email string
	}{
		{10, "cc@example.com"},
		{11, "bcc@example.com"},
	} {
		_, err := db.Exec(
			`INSERT INTO participants (id, email_address, domain, created_at, updated_at)
			 VALUES (?, ?, 'example.com', datetime('now'), datetime('now'))`,
			p.id, p.email,
		)
		require.NoError(err, "insert participant %s", p.email)
	}
	for _, r := range []struct {
		participantID int
		recipientType string
	}{
		{10, "cc"},
		{11, "bcc"},
	} {
		_, err := db.Exec(
			`INSERT INTO message_recipients (message_id, participant_id, recipient_type)
			 VALUES (?, ?, ?)`, msgID, r.participantID, r.recipientType,
		)
		require.NoError(err, "insert recipient %s", r.recipientType)
	}

	messages, total, err := st.ListMessages(0, 100)
	require.NoError(err, "ListMessages")
	require.Equal(int64(1), total, "total")
	assert.Equal(source.ID, messages[0].SourceID, "SourceID")
	require.Len(messages[0].Cc, 1)
	assert.Equal("cc@example.com", messages[0].Cc[0], "Cc[0]")
	require.Len(messages[0].Bcc, 1)
	assert.Equal("bcc@example.com", messages[0].Bcc[0], "Bcc[0]")
}

// TestListAndSearchSurfacePhoneAndIdentifierParticipants exercises the
// store-backed list/search path for SMS-style participants that have no
// email_address: phone-only (synctech-sms phone backups) and identifier-
// only with a contact name (synctech-sms short codes routed through
// EnsureParticipantByIdentifier). Before the participantDisplaySQL fix
// these rendered with blank From/To because the SELECT only read
// p.email_address.
func TestListAndSearchSurfacePhoneAndIdentifierParticipants(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := openTestStore(t)

	source, err := st.GetOrCreateSource("synctech-sms", "+15550000001")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(source.ID, "text:1,2", "Alice")
	require.NoError(err, "EnsureConversation")

	phoneSenderID, err := st.EnsureParticipantByPhone("+15551234567", "Alice", "synctech-sms")
	require.NoError(err, "EnsureParticipantByPhone")
	ownerID, err := st.EnsureParticipantByPhone("+15550000001", "Me", "synctech-sms")
	require.NoError(err, "EnsureParticipantByPhone(owner)")
	shortCodeID, err := st.EnsureParticipantByIdentifier("synctech-sms", "22000", "ShortCode Alerts")
	require.NoError(err, "EnsureParticipantByIdentifier")

	// Phone-backed sender, phone-backed recipient.
	smsID, err := st.UpsertMessage(&Message{
		ConversationID:  convID,
		SourceID:        source.ID,
		SourceMessageID: "sms-phone",
		MessageType:     "sms",
		Subject:         sql.NullString{String: "hello", Valid: true},
		Snippet:         sql.NullString{String: "hello from sms", Valid: true},
		SenderID:        sql.NullInt64{Int64: phoneSenderID, Valid: true},
		SizeEstimate:    14,
	})
	require.NoError(err, "UpsertMessage(sms-phone)")
	require.NoError(st.ReplaceMessageRecipients(smsID, "from", []int64{phoneSenderID}, []string{""}),
		"ReplaceMessageRecipients(from)")
	require.NoError(st.ReplaceMessageRecipients(smsID, "to", []int64{ownerID}, []string{""}),
		"ReplaceMessageRecipients(to)")
	require.NoError(st.UpsertFTS(smsID, "hello", "hello from sms", "", "", ""),
		"UpsertFTS(sms-phone)")

	// Short-code sender with a contact name but no email/phone.
	shortID, err := st.UpsertMessage(&Message{
		ConversationID:  convID,
		SourceID:        source.ID,
		SourceMessageID: "sms-shortcode",
		MessageType:     "sms",
		Subject:         sql.NullString{String: "code", Valid: true},
		Snippet:         sql.NullString{String: "your code is 123456", Valid: true},
		SenderID:        sql.NullInt64{Int64: shortCodeID, Valid: true},
		SizeEstimate:    20,
	})
	require.NoError(err, "UpsertMessage(sms-shortcode)")
	require.NoError(st.ReplaceMessageRecipients(shortID, "from", []int64{shortCodeID}, []string{""}),
		"ReplaceMessageRecipients(short-from)")
	require.NoError(st.UpsertFTS(shortID, "code", "your code is 123456", "", "", ""),
		"UpsertFTS(sms-shortcode)")

	// WhatsApp-style sender: messages.sender_id is set but no 'from'
	// row exists in message_recipients. The store-backed SELECT used
	// to join participants only through mr.participant_id, so this
	// message rendered with a blank From.
	waSenderID, err := st.EnsureParticipantByPhone("+15559998888", "Carol", "whatsapp")
	require.NoError(err, "EnsureParticipantByPhone(whatsapp)")
	waID, err := st.UpsertMessage(&Message{
		ConversationID:  convID,
		SourceID:        source.ID,
		SourceMessageID: "wa-sender-only",
		MessageType:     "whatsapp",
		Subject:         sql.NullString{String: "wa", Valid: true},
		Snippet:         sql.NullString{String: "wa snippet", Valid: true},
		SenderID:        sql.NullInt64{Int64: waSenderID, Valid: true},
		SizeEstimate:    10,
	})
	require.NoError(err, "UpsertMessage(wa-sender-only)")
	require.NoError(st.UpsertFTS(waID, "wa", "wa snippet", "", "", ""),
		"UpsertFTS(wa-sender-only)")

	want := map[string]struct {
		from string
		to   []string
	}{
		"sms-phone":      {from: "Alice <+15551234567>", to: []string{"Me <+15550000001>"}},
		"sms-shortcode":  {from: "ShortCode Alerts", to: nil},
		"wa-sender-only": {from: "Carol <+15559998888>", to: nil},
	}

	// ListMessages and SearchMessages take different code paths but
	// share the same scanner/recipient loader.
	msgs, _, err := st.ListMessages(0, 100)
	require.NoError(err, "ListMessages")
	gotByID := make(map[string]APIMessage, len(msgs))
	for _, m := range msgs {
		row, err := st.GetMessage(m.ID)
		require.NoError(err, "GetMessage(%d)", m.ID)
		// Detail call populates To from getRecipients; pull both for
		// per-key assertions.
		m.To = row.To
		gotByID[sourceMessageIDForTest(t, st, m.ID)] = m
	}
	for srcMsgID, w := range want {
		got, ok := gotByID[srcMsgID]
		require.True(ok, "ListMessages missing %s", srcMsgID)
		assert.Equal(w.from, got.From, "%s ListMessages From", srcMsgID)
		if len(w.to) == 0 {
			assert.Empty(got.To, "%s ListMessages To", srcMsgID)
		} else {
			require.Len(got.To, len(w.to), "%s ListMessages To", srcMsgID)
			assert.Equal(w.to[0], got.To[0], "%s ListMessages To[0]", srcMsgID)
		}
	}

	// SearchMessagesLike covers the FTS-disabled search code path used
	// when the FTS index errors at runtime. It composes the same SELECT
	// columns, so the no-FTS path must surface phone/name too.
	results, _, err := st.SearchMessagesQuery(&search.Query{TextTerms: []string{"123456"}}, 0, 100)
	require.NoError(err, "SearchMessagesQuery")
	require.Len(results, 1, "SearchMessagesQuery results")
	assert.Equal(want["sms-shortcode"].from, results[0].From, "SearchMessagesQuery From")
}

func sourceMessageIDForTest(t *testing.T, st *Store, id int64) string {
	t.Helper()
	var sourceMsgID string
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT source_message_id FROM messages WHERE id = ?`), id).Scan(&sourceMsgID),
		"lookup source_message_id")
	return sourceMsgID
}

func TestSearchMessagesLikeLiteralWildcards(t *testing.T) {
	st := openTestStore(t)

	// Create a source and conversation
	source, err := st.GetOrCreateSource("gmail", "test@example.com")
	require.NoError(t, err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(source.ID, "thread-1", "Thread")
	require.NoError(t, err, "EnsureConversation")

	// Seed messages: one with literal %, one with literal _, one plain,
	// plus confounding rows that would match if wildcards weren't escaped.
	seedMessage(t, st, source.ID, convID, "msg-pct", "100% off sale", "great deal")
	seedMessage(t, st, source.ID, convID, "msg-pct-confound", "100 days sale", "another deal") // would match "100%" if % is a wildcard
	seedMessage(t, st, source.ID, convID, "msg-us", "file_name.txt", "attachment info")
	seedMessage(t, st, source.ID, convID, "msg-us-confound", "fileXname.txt", "another attachment") // would match "file_name" if _ is a wildcard
	seedMessage(t, st, source.ID, convID, "msg-plain", "plain subject", "nothing special")

	tests := []struct {
		name      string
		query     string
		wantCount int64
		wantLen   int // number of result rows
	}{
		{
			name:      "literal percent matches only percent message not confounding row",
			query:     "100%",
			wantCount: 1,
			wantLen:   1,
		},
		{
			name:      "literal underscore matches only underscore message not confounding row",
			query:     "file_name",
			wantCount: 1,
			wantLen:   1,
		},
		{
			name:      "plain query still works",
			query:     "plain",
			wantCount: 1,
			wantLen:   1,
		},
		{
			name:      "no match returns zero",
			query:     "nonexistent",
			wantCount: 0,
			wantLen:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			messages, total, err := st.searchMessagesLike(tt.query, 0, 100)
			require.NoError(t, err, "searchMessagesLike(%q)", tt.query)
			assert.Equal(t, tt.wantCount, total, "total")
			assert.Len(t, messages, tt.wantLen, "len(messages)")
		})
	}
}

// TestSearchMessagesLikePaginationStability is the C3 regression for the
// searchMessagesLike LIKE fallback (api.go), whose ORDER BY gained the
// ", m.id DESC" tiebreaker. The seeded rows leave sent_at NULL, so
// COALESCE(sent_at, received_at, internal_date) is NULL for all of them — the
// ambiguous shared-sort-key case. Without the PK tiebreaker, LIMIT/OFFSET paging
// over them could drop or duplicate an id across adjacent pages. This path is
// only reachable white-box (unexported method) and is engine-agnostic SQL, so it
// is exercised here on SQLite; the cross-backend store-API paths are covered by
// TestStoreAPI_PaginationStability_IdenticalSentAt.
func TestSearchMessagesLikePaginationStability(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := openTestStore(t)

	source, err := st.GetOrCreateSource("gmail", "test@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(source.ID, "thread-1", "Thread")
	require.NoError(err, "EnsureConversation")

	const n = 5
	const tag = "likepage"
	wantIDs := make(map[int64]struct{}, n)
	for i := range n {
		// Shared subject token so the LIKE filter selects exactly these rows;
		// sent_at left NULL (seedMessage default) => identical sort key.
		mid := seedMessage(t, st, source.ID, convID,
			fmt.Sprintf("like-msg-%d", i), tag+" subject", "snippet body")
		wantIDs[mid] = struct{}{}
	}

	seen := make([]int64, 0, n)
	for offset := range n {
		msgs, _, err := st.searchMessagesLike(tag, offset, 1)
		require.NoError(err, "searchMessagesLike offset=%d", offset)
		require.Lenf(msgs, 1, "page at offset=%d returned no row (pagination skipped a row)", offset)
		seen = append(seen, msgs[0].ID)
	}

	gotSet := make(map[int64]struct{}, len(seen))
	for _, id := range seen {
		_, dup := gotSet[id]
		assert.Falsef(dup, "id %d appeared on more than one page (unstable pagination)", id)
		gotSet[id] = struct{}{}
	}
	assert.Len(seen, n, "paged ids should cover every row exactly once")
	for id := range wantIDs {
		_, ok := gotSet[id]
		assert.Truef(ok, "expected id %d missing from paged results (row skipped)", id)
	}
}
