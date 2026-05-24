package store

import (
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"time"
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
			got := escapeLike(tt.input)
			if got != tt.want {
				t.Errorf("escapeLike(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// openTestStore creates a temporary store for internal tests.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := st.InitSchema(); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
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
	if err != nil {
		t.Fatalf("UpsertMessage(%q): %v", sourceMessageID, err)
	}
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
			if !got.Equal(tt.want) {
				t.Errorf(
					"parseSQLiteTime(%q) = %v, want %v",
					tt.input, got, tt.want,
				)
			}
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
			if err := n.Scan(tt.src); err != nil {
				t.Fatalf("Scan(%T %v): unexpected error %v", tt.src, tt.src, err)
			}
			if n.Valid != tt.wantValid {
				t.Errorf("Valid = %v, want %v", n.Valid, tt.wantValid)
			}
			if !n.Time.Equal(tt.wantTime) {
				t.Errorf("Time = %v, want %v", n.Time, tt.wantTime)
			}
		})
	}

	t.Run("unsupported type errors", func(t *testing.T) {
		var n nullableTimestamp
		if err := n.Scan(42); err == nil {
			t.Fatalf("Scan(int): expected error, got nil")
		}
	})
}

func TestGetMessageCcBcc(t *testing.T) {
	st := openTestStore(t)

	source, err := st.GetOrCreateSource("gmail", "test@example.com")
	if err != nil {
		t.Fatalf("GetOrCreateSource: %v", err)
	}
	convID, err := st.EnsureConversation(source.ID, "thread-1", "Thread")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}
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
		if _, err := db.Exec(
			`INSERT INTO participants (id, email_address, domain, created_at, updated_at)
			 VALUES (?, ?, 'example.com', datetime('now'), datetime('now'))`,
			p.id, p.email,
		); err != nil {
			t.Fatalf("insert participant %s: %v", p.email, err)
		}
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
		if _, err := db.Exec(
			`INSERT INTO message_recipients (message_id, participant_id, recipient_type)
			 VALUES (?, ?, ?)`,
			msgID, r.participantID, r.recipientType,
		); err != nil {
			t.Fatalf("insert recipient %s: %v", r.recipientType, err)
		}
	}

	// Test GetMessage
	m, err := st.GetMessage(msgID)
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	if len(m.To) != 1 || m.To[0] != "to@example.com" {
		t.Errorf("To = %v, want [to@example.com]", m.To)
	}
	gotCc := slices.Clone(m.Cc)
	slices.Sort(gotCc)
	wantCc := []string{"cc1@example.com", "cc2@example.com"}
	if !slices.Equal(gotCc, wantCc) {
		t.Errorf("Cc = %v, want %v", m.Cc, wantCc)
	}
	if len(m.Bcc) != 1 || m.Bcc[0] != "bcc@example.com" {
		t.Errorf("Bcc = %v, want [bcc@example.com]", m.Bcc)
	}
}

func TestListMessagesCcBcc(t *testing.T) {
	st := openTestStore(t)

	source, err := st.GetOrCreateSource("gmail", "test@example.com")
	if err != nil {
		t.Fatalf("GetOrCreateSource: %v", err)
	}
	convID, err := st.EnsureConversation(source.ID, "thread-1", "Thread")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}
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
		if _, err := db.Exec(
			`INSERT INTO participants (id, email_address, domain, created_at, updated_at)
			 VALUES (?, ?, 'example.com', datetime('now'), datetime('now'))`,
			p.id, p.email,
		); err != nil {
			t.Fatalf("insert participant %s: %v", p.email, err)
		}
	}
	for _, r := range []struct {
		participantID int
		recipientType string
	}{
		{10, "cc"},
		{11, "bcc"},
	} {
		if _, err := db.Exec(
			`INSERT INTO message_recipients (message_id, participant_id, recipient_type)
			 VALUES (?, ?, ?)`, msgID, r.participantID, r.recipientType,
		); err != nil {
			t.Fatalf("insert recipient %s: %v", r.recipientType, err)
		}
	}

	messages, total, err := st.ListMessages(0, 100)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if total != 1 {
		t.Fatalf("total = %d, want 1", total)
	}
	if len(messages[0].Cc) != 1 || messages[0].Cc[0] != "cc@example.com" {
		t.Errorf("Cc = %v, want [cc@example.com]", messages[0].Cc)
	}
	if len(messages[0].Bcc) != 1 || messages[0].Bcc[0] != "bcc@example.com" {
		t.Errorf("Bcc = %v, want [bcc@example.com]", messages[0].Bcc)
	}
}

func TestSearchMessagesLikeLiteralWildcards(t *testing.T) {
	st := openTestStore(t)

	// Create a source and conversation
	source, err := st.GetOrCreateSource("gmail", "test@example.com")
	if err != nil {
		t.Fatalf("GetOrCreateSource: %v", err)
	}
	convID, err := st.EnsureConversation(source.ID, "thread-1", "Thread")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}

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
			if err != nil {
				t.Fatalf("searchMessagesLike(%q): %v", tt.query, err)
			}
			if total != tt.wantCount {
				t.Errorf("total = %d, want %d", total, tt.wantCount)
			}
			if len(messages) != tt.wantLen {
				t.Errorf("len(messages) = %d, want %d", len(messages), tt.wantLen)
			}
		})
	}
}
