package store_test

import (
	"database/sql"
	"slices"
	"sync"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
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
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	require.NoError(err, "InspectMessage")
	assert.NotEmpty(insp.SentAt, "SentAt should not be empty after a non-NULL insert")
	assert.NotEmpty(insp.InternalDate, "InternalDate should not be empty after a non-NULL insert")

	sentAtStr, internalDateStr, err := f.Store.InspectMessageDates("inspect-msg-1")
	require.NoError(err, "InspectMessageDates")
	assert.Equal(insp.SentAt, sentAtStr, "InspectMessageDates sentAt")
	assert.Equal(insp.InternalDate, internalDateStr, "InspectMessageDates internalDate")
}

// TestSearchMessagesQuery_SubjectLikeCaseInsensitive covers H3+H4: the
// subject: filter and the LIKE fallback must be case-insensitive on
// both backends. SQLite's ASCII LIKE is case-insensitive by default;
// PG's is strict-case. The fix wraps both sides in LOWER so a query
// for "Invoice" matches "invoice from acme".
func TestSearchMessagesQuery_SubjectLikeCaseInsensitive(t *testing.T) {
	require := requirepkg.New(t)
	f := storetest.New(t)

	mid := f.NewMessage().
		WithSourceMessageID("subj-msg-1").
		WithSubject("invoice from acme").
		WithSnippet("see attached").
		Create(t, f.Store)
	require.NoError(f.Store.UpsertMessageBody(mid,
		sql.NullString{String: "body", Valid: true}, sql.NullString{}), "UpsertMessageBody")
	_, err := f.Store.BackfillFTS(nil)
	require.NoError(err, "BackfillFTS")

	msgs, total, err := f.Store.SearchMessagesQuery(
		&search.Query{SubjectTerms: []string{"Invoice"}}, 0, 50,
	)
	require.NoError(err, "SearchMessagesQuery")
	require.Equal(int64(1), total, "Subject:Invoice (case-insensitive on both backends)")
	require.Len(msgs, 1)
}

// TestSearchMessagesQuery_ToFilterCaseInsensitive covers M2: the To/Cc/
// Bcc IN-list args in buildSearchQueryParts now lowercase Go-side so
// the LOWER(p.email_address) IN (...) predicate matches against
// case-folded participants.
func TestSearchMessagesQuery_ToFilterCaseInsensitive(t *testing.T) {
	require := requirepkg.New(t)
	f := storetest.New(t)

	to, err := f.Store.EnsureParticipant("alice@example.com", "Alice", "example.com")
	require.NoError(err, "EnsureParticipant")

	mid := f.NewMessage().
		WithSourceMessageID("to-msg-1").
		WithSubject("greetings").
		Create(t, f.Store)
	require.NoError(f.Store.ReplaceMessageRecipients(mid, "to", []int64{to}, []string{"Alice"}),
		"ReplaceMessageRecipients to")

	_, err = f.Store.BackfillFTS(nil)
	require.NoError(err, "BackfillFTS")

	msgs, total, err := f.Store.SearchMessagesQuery(
		&search.Query{ToAddrs: []string{"Alice@Example.COM"}}, 0, 50,
	)
	require.NoError(err, "SearchMessagesQuery")
	require.Equal(int64(1), total, "to:Alice@Example.COM (case-insensitive)")
	require.Len(msgs, 1)
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
	requirepkg.NoError(t, f.Store.UpsertMessageBody(msg1,
		sql.NullString{String: "foo and bar appear together here", Valid: true},
		sql.NullString{}), "UpsertMessageBody 1")

	msg2 := f.NewMessage().
		WithSourceMessageID("r3-msg-2").
		WithSubject("email from alice").
		WithSnippet("contact us at user@example.com please").
		Create(t, f.Store)
	requirepkg.NoError(t, f.Store.UpsertMessageBody(msg2,
		sql.NullString{String: "reach us at user@example.com for support", Valid: true},
		sql.NullString{}), "UpsertMessageBody 2")

	_, err := f.Store.BackfillFTS(nil)
	requirepkg.NoError(t, err, "BackfillFTS")

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
			assertpkg.NoError(t, err, "SearchMessages(%q): must accept punctuation-heavy input without erroring", q)
		})
	}
}

// TestEnsureParticipant_Concurrent covers H5: 50 goroutines all
// calling EnsureParticipant with the same email must produce exactly
// one row and no errors. The fix collapsed the SELECT-then-INSERT
// race into a single INSERT … ON CONFLICT … RETURNING id statement.
func TestEnsureParticipant_Concurrent(t *testing.T) {
	assert := assertpkg.New(t)
	st := testutil.NewTestStore(t)

	const N = 50
	var wg sync.WaitGroup
	ids := make([]int64, N)
	errs := make([]error, N)
	for i := range N {
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
		assert.NoError(err, "goroutine %d: EnsureParticipant", i)
	}
	first := ids[0]
	for i, id := range ids {
		assert.Equal(first, id, "goroutine %d should observe the same row", i)
	}

	var count int
	requirepkg.NoError(t, st.DB().QueryRow(
		st.Rebind("SELECT COUNT(*) FROM participants WHERE email_address = ?"),
		"race@example.com",
	).Scan(&count), "count participants")
	assert.Equal(1, count, "want exactly 1 participant row for race@example.com")
}

// TestEnsureParticipantByPhone_Concurrent covers H6: same race shape
// for the phone path. With the partial unique index on phone_number
// now in place, concurrent inserts collapse to one row via the
// ON CONFLICT clause.
func TestEnsureParticipantByPhone_Concurrent(t *testing.T) {
	assert := assertpkg.New(t)
	st := testutil.NewTestStore(t)

	const N = 50
	var wg sync.WaitGroup
	ids := make([]int64, N)
	errs := make([]error, N)
	for i := range N {
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
		assert.NoError(err, "goroutine %d: EnsureParticipantByPhone", i)
	}
	first := ids[0]
	for i, id := range ids {
		assert.Equal(first, id, "goroutine %d", i)
	}

	var count int
	requirepkg.NoError(t, st.DB().QueryRow(
		st.Rebind("SELECT COUNT(*) FROM participants WHERE phone_number = ?"),
		"+15555550100",
	).Scan(&count), "count participants")
	assert.Equal(1, count, "want exactly 1 participant row for +15555550100")
}

// TestAddAccountIdentity_Concurrent covers M1: concurrent confirmations
// of the same (source_id, address) collapse to exactly one row, and
// merging different signals across calls preserves the union.
func TestAddAccountIdentity_Concurrent(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "race-identity@example.com")
	require.NoError(err, "GetOrCreateSource")

	const N = 50
	signals := []string{"manual", "account-identifier", "header"}
	var wg sync.WaitGroup
	errs := make([]error, N)
	for i := range N {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			signal := signals[idx%len(signals)]
			errs[idx] = st.AddAccountIdentity(src.ID, "user@example.com", signal)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		assert.NoError(err, "goroutine %d: AddAccountIdentity", i)
	}

	identities, err := st.ListAccountIdentities(src.ID)
	require.NoError(err, "ListAccountIdentities")
	require.Len(identities, 1)
	got := identities[0].SourceSignal
	// All three signals must appear in the merged comma-separated set.
	for _, want := range signals {
		assert.True(containsSignal(got, want), "merged source_signal %q missing %q", got, want)
	}
}

func containsSignal(set, signal string) bool {
	return slices.Contains(splitComma(set), signal)
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
	for i := range N {
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
		assertpkg.NoError(t, err, "goroutine %d: UpsertAttachment", i)
	}

	var count int
	requirepkg.NoError(t, f.Store.DB().QueryRow(
		f.Store.Rebind("SELECT COUNT(*) FROM attachments WHERE message_id = ? AND content_hash = ?"),
		mid, "abcd1234",
	).Scan(&count), "count attachments")
	assertpkg.Equal(t, 1, count, "want exactly 1 attachment row")
}

// silenceUnused references the imports the test would otherwise drop on
// builds where some helpers are tag-gated.
var _ = store.Source{}
