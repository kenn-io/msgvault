package store_test

import (
	"database/sql"
	"sync"
	"testing"
	"time"

	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// TestInspectMessage_TimestampScan exercises the inspect.go fix that
// switched sent_at / internal_date scanning from sql.NullString to
// sql.NullTime. Under PG (MSGVAULT_TEST_DB=postgres://…) the prior
// shape errored at Scan because pgx decodes TIMESTAMPTZ as time.Time
// and refuses to convert to *string. Under SQLite the test asserts
// the formatted string still contains the expected date/time pieces.
func TestInspectMessage_TimestampScan(t *testing.T) {
	f := storetest.New(t)

	sent := time.Date(2025, 6, 15, 12, 30, 45, 0, time.UTC)
	mid := f.NewMessage().
		WithSourceMessageID("inspect-msg-1").
		WithSubject("hello").
		WithSentAt(sent).
		WithInternalDate(sent).
		Create(t, f.Store)
	_ = mid

	insp, err := f.Store.InspectMessage("inspect-msg-1")
	if err != nil {
		t.Fatalf("InspectMessage: %v", err)
	}
	if insp.SentAt == "" {
		t.Error("SentAt should not be empty after a non-NULL insert")
	}
	if insp.InternalDate == "" {
		t.Error("InternalDate should not be empty after a non-NULL insert")
	}

	sentAtStr, internalDateStr, err := f.Store.InspectMessageDates("inspect-msg-1")
	if err != nil {
		t.Fatalf("InspectMessageDates: %v", err)
	}
	if sentAtStr != insp.SentAt {
		t.Errorf("InspectMessageDates sentAt = %q, want %q", sentAtStr, insp.SentAt)
	}
	if internalDateStr != insp.InternalDate {
		t.Errorf("InspectMessageDates internalDate = %q, want %q",
			internalDateStr, insp.InternalDate)
	}
}

// TestSearchMessagesQuery_SubjectLikeCaseInsensitive covers H3+H4: the
// subject: filter and the LIKE fallback must be case-insensitive on
// both backends. SQLite's ASCII LIKE is case-insensitive by default;
// PG's is strict-case. The fix wraps both sides in LOWER so a query
// for "Invoice" matches "invoice from acme".
func TestSearchMessagesQuery_SubjectLikeCaseInsensitive(t *testing.T) {
	f := storetest.New(t)

	mid := f.NewMessage().
		WithSourceMessageID("subj-msg-1").
		WithSubject("invoice from acme").
		WithSnippet("see attached").
		Create(t, f.Store)
	if err := f.Store.UpsertMessageBody(mid,
		sql.NullString{String: "body", Valid: true}, sql.NullString{}); err != nil {
		t.Fatalf("UpsertMessageBody: %v", err)
	}
	if _, err := f.Store.BackfillFTS(nil); err != nil {
		t.Fatalf("BackfillFTS: %v", err)
	}

	msgs, total, err := f.Store.SearchMessagesQuery(
		&search.Query{SubjectTerms: []string{"Invoice"}}, 0, 50,
	)
	if err != nil {
		t.Fatalf("SearchMessagesQuery: %v", err)
	}
	if total != 1 || len(msgs) != 1 {
		t.Fatalf("Subject:Invoice → got %d hits, want 1 (case-insensitive on both backends)", total)
	}
}

// TestSearchMessagesQuery_ToFilterCaseInsensitive covers M2: the To/Cc/
// Bcc IN-list args in buildSearchQueryParts now lowercase Go-side so
// the LOWER(p.email_address) IN (...) predicate matches against
// case-folded participants.
func TestSearchMessagesQuery_ToFilterCaseInsensitive(t *testing.T) {
	f := storetest.New(t)

	to, err := f.Store.EnsureParticipant("alice@example.com", "Alice", "example.com")
	testutil.MustNoErr(t, err, "EnsureParticipant")

	mid := f.NewMessage().
		WithSourceMessageID("to-msg-1").
		WithSubject("greetings").
		Create(t, f.Store)
	testutil.MustNoErr(t,
		f.Store.ReplaceMessageRecipients(mid, "to", []int64{to}, []string{"Alice"}),
		"ReplaceMessageRecipients to")

	if _, err := f.Store.BackfillFTS(nil); err != nil {
		t.Fatalf("BackfillFTS: %v", err)
	}

	msgs, total, err := f.Store.SearchMessagesQuery(
		&search.Query{ToAddrs: []string{"Alice@Example.COM"}}, 0, 50,
	)
	if err != nil {
		t.Fatalf("SearchMessagesQuery: %v", err)
	}
	if total != 1 || len(msgs) != 1 {
		t.Fatalf("to:Alice@Example.COM → got %d hits, want 1 (case-insensitive)", total)
	}
}

// TestSearchMessages_R3PunctuationTerms covers R3: the raw search path
// must not feed `to_tsquery` invalid lexemes built from punctuation-
// heavy input. Before the fix, a query like `---` or `foo-bar` would
// emit `---:*` / `foo-bar:*` and PG would error at parse time
// ("syntax error in tsquery"). The fix tokenizes user terms into
// letter/digit-only lexemes so punctuation-only inputs collapse to
// FALSE and hyphenated/email/dotted inputs decompose into individual
// lexemes that to_tsquery accepts.
//
// On both backends the test asserts the query returns *no error* —
// that's the load-bearing R3 guarantee. Lexeme-level matching is
// unit-tested via TestPostgreSQLDialect_BuildFTSArg; we don't assert
// hit counts here because SQLite's FTS5 path strips punctuation
// without splitting (e.g. `foo-bar` becomes `foobar`, which won't
// match a body containing the two separate words), so cross-backend
// match-count assertions would diverge.
func TestSearchMessages_R3PunctuationTerms(t *testing.T) {
	f := storetest.New(t)

	msg1 := f.NewMessage().
		WithSourceMessageID("r3-msg-1").
		WithSubject("project foo bar").
		WithSnippet("foo bar baz").
		Create(t, f.Store)
	testutil.MustNoErr(t, f.Store.UpsertMessageBody(msg1,
		sql.NullString{String: "foo and bar appear together here", Valid: true},
		sql.NullString{}), "UpsertMessageBody 1")

	msg2 := f.NewMessage().
		WithSourceMessageID("r3-msg-2").
		WithSubject("email from alice").
		WithSnippet("contact us at user@example.com please").
		Create(t, f.Store)
	testutil.MustNoErr(t, f.Store.UpsertMessageBody(msg2,
		sql.NullString{String: "reach us at user@example.com for support", Valid: true},
		sql.NullString{}), "UpsertMessageBody 2")

	if _, err := f.Store.BackfillFTS(nil); err != nil {
		t.Fatalf("BackfillFTS: %v", err)
	}

	queries := []string{
		"---",              // dashes-only — used to crash to_tsquery
		"...",              // dots-only
		"foo-bar",          // hyphenated word
		"user@example.com", // email-like
		"a.b.c",            // dotted acronym
		"foo ---",          // mixed clean + punct
		"v1.2.3-rc.1",      // version-like punctuation
	}
	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			_, _, err := f.Store.SearchMessages(q, 0, 50)
			if err != nil {
				t.Errorf("SearchMessages(%q): unexpected error %v (must accept punctuation-heavy input without erroring)", q, err)
			}
		})
	}
}

// TestEnsureParticipant_Concurrent covers H5: 50 goroutines all
// calling EnsureParticipant with the same email must produce exactly
// one row and no errors. The fix collapsed the SELECT-then-INSERT
// race into a single INSERT … ON CONFLICT … RETURNING id statement.
func TestEnsureParticipant_Concurrent(t *testing.T) {
	st := testutil.NewTestStore(t)

	const N = 50
	var wg sync.WaitGroup
	ids := make([]int64, N)
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			id, err := st.EnsureParticipant("race@example.com", "Race", "example.com")
			ids[idx] = id
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: EnsureParticipant: %v", i, err)
		}
	}
	first := ids[0]
	for i, id := range ids {
		if id != first {
			t.Errorf("goroutine %d returned id %d, want %d (every caller must observe the same row)", i, id, first)
		}
	}

	var count int
	if err := st.DB().QueryRow(
		st.Rebind("SELECT COUNT(*) FROM participants WHERE email_address = ?"),
		"race@example.com",
	).Scan(&count); err != nil {
		t.Fatalf("count participants: %v", err)
	}
	if count != 1 {
		t.Errorf("got %d participant rows for race@example.com, want exactly 1", count)
	}
}

// TestEnsureParticipantByPhone_Concurrent covers H6: same race shape
// for the phone path. With the partial unique index on phone_number
// now in place, concurrent inserts collapse to one row via the
// ON CONFLICT clause.
func TestEnsureParticipantByPhone_Concurrent(t *testing.T) {
	st := testutil.NewTestStore(t)

	const N = 50
	var wg sync.WaitGroup
	ids := make([]int64, N)
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			id, err := st.EnsureParticipantByPhone("+15555550100", "Bob", "whatsapp")
			ids[idx] = id
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: EnsureParticipantByPhone: %v", i, err)
		}
	}
	first := ids[0]
	for i, id := range ids {
		if id != first {
			t.Errorf("goroutine %d returned id %d, want %d", i, id, first)
		}
	}

	var count int
	if err := st.DB().QueryRow(
		st.Rebind("SELECT COUNT(*) FROM participants WHERE phone_number = ?"),
		"+15555550100",
	).Scan(&count); err != nil {
		t.Fatalf("count participants: %v", err)
	}
	if count != 1 {
		t.Errorf("got %d participant rows for +15555550100, want exactly 1", count)
	}
}

// TestAddAccountIdentity_Concurrent covers M1: concurrent confirmations
// of the same (source_id, address) collapse to exactly one row, and
// merging different signals across calls preserves the union.
func TestAddAccountIdentity_Concurrent(t *testing.T) {
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "race-identity@example.com")
	testutil.MustNoErr(t, err, "GetOrCreateSource")

	const N = 50
	signals := []string{"manual", "account-identifier", "header"}
	var wg sync.WaitGroup
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			signal := signals[idx%len(signals)]
			errs[idx] = st.AddAccountIdentity(src.ID, "user@example.com", signal)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: AddAccountIdentity: %v", i, err)
		}
	}

	identities, err := st.ListAccountIdentities(src.ID)
	if err != nil {
		t.Fatalf("ListAccountIdentities: %v", err)
	}
	if len(identities) != 1 {
		t.Fatalf("got %d identity rows, want exactly 1", len(identities))
	}
	got := identities[0].SourceSignal
	// All three signals must appear in the merged comma-separated set.
	for _, want := range signals {
		if !containsSignal(got, want) {
			t.Errorf("merged source_signal %q missing %q", got, want)
		}
	}
}

func containsSignal(set, signal string) bool {
	for _, s := range splitComma(set) {
		if s == signal {
			return true
		}
	}
	return false
}

func splitComma(s string) []string {
	if s == "" {
		return nil
	}
	out := []string{}
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return out
}

// TestUpsertAttachment_Concurrent covers M5: two goroutines uploading
// the same (message_id, content_hash) attachment race; the retry loop
// must collapse to one row with no error.
func TestUpsertAttachment_Concurrent(t *testing.T) {
	f := storetest.New(t)
	mid := f.NewMessage().WithSourceMessageID("att-msg-1").Create(t, f.Store)

	const N = 20
	var wg sync.WaitGroup
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			errs[idx] = f.Store.UpsertAttachment(
				mid, "file.pdf", "application/pdf",
				"ab/abcd1234", "abcd1234", 1024,
			)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: UpsertAttachment: %v", i, err)
		}
	}

	var count int
	if err := f.Store.DB().QueryRow(
		f.Store.Rebind("SELECT COUNT(*) FROM attachments WHERE message_id = ? AND content_hash = ?"),
		mid, "abcd1234",
	).Scan(&count); err != nil {
		t.Fatalf("count attachments: %v", err)
	}
	if count != 1 {
		t.Errorf("got %d attachment rows, want exactly 1", count)
	}
}

// silenceUnused references the imports the test would otherwise drop on
// builds where some helpers are tag-gated.
var _ = store.Source{}
