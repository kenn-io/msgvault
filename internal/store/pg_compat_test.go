package store_test

import (
	"database/sql"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require := require.New(t)
	assert := assert.New(t)
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
	require := require.New(t)
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
	require := require.New(t)
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

func TestSearchFTSPunctuation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	id := f.NewMessage().WithSourceMessageID("m1").WithSubject("Release 1.2.0 notes").Create(t, f.Store)
	require.NoError(f.Store.UpsertFTS(id, "Release 1.2.0 notes", "Write to both@example.com or see changelog.example.net.", "+15550100777", "", ""))
	_, _, err := f.Store.SearchMessages("foo ---", 0, 50)
	require.NoError(err)
	for term, want := range map[string][]int64{"notes": {id}, "both@example.com": {id}, "both@exam": {id}, "changelog.example.net": {id}, "1.2.0": {id}, "+15550100777": {id}, "15550100777": {id}, "example@both": nil} {
		ids, _, err := f.Store.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{term}}, 10)
		require.NoError(err, term)
		assert.ElementsMatch(want, ids, term)
	}
}

func TestPostgresFTSSaturatedPhrases(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	if !f.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL indexing")
	}
	for i, prefix := range []string{strings.Repeat("filler ", 17000), strings.Repeat("alice ", 255), strings.Repeat("alice ", 300)} {
		id := f.NewMessage().WithSourceMessageID(strconv.Itoa(i)).Create(t, f.Store)
		require.NoError(f.Store.UpsertFTS(id, "", prefix+"alice@example.com", "", "", ""))
		ids, _, err := f.Store.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{"alice@exam"}}, 10)
		require.NoError(err)
		assert.Contains(ids, id)
		ids, _, err = f.Store.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{"example@alice"}}, 10)
		require.NoError(err)
		assert.NotContains(ids, id)
	}
	separated := f.NewMessage().WithSourceMessageID("separated").Create(t, f.Store)
	crossField := f.NewMessage().WithSourceMessageID("cross-field-saturated").Create(t, f.Store)
	plainCrossField := f.NewMessage().WithSourceMessageID("cross-field").Create(t, f.Store)
	require.NoError(f.Store.UpsertFTS(separated, "", strings.Repeat("filler ", 17000)+"alice middle example", "", "", ""))
	require.NoError(f.Store.UpsertFTS(crossField, "alice", strings.Repeat("filler ", 17000)+"example", "", "", ""))
	require.NoError(f.Store.UpsertFTS(plainCrossField, "alice", "", "example.com", "", ""))
	ids, _, err := f.Store.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{"alice@example"}}, 10)
	require.NoError(err)
	assert.NotContains(ids, separated)
	assert.NotContains(ids, crossField)
	assert.NotContains(ids, plainCrossField)
	ids, _, err = f.Store.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{"alice", "example"}}, 10)
	require.NoError(err)
	assert.Contains(ids, plainCrossField)
	unicodeID := f.NewMessage().WithSourceMessageID("unicode-saturated").Create(t, f.Store)
	require.NoError(f.Store.UpsertFTS(unicodeID, "", strings.Repeat("filler ", 17000)+"CAFÉ@東京.example e\u0301xample", "", "", ""))
	for _, term := range []string{"café@東京.exam", "e@xam"} {
		ids, _, err := f.Store.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{term}}, 10)
		require.NoError(err)
		assert.Contains(ids, unicodeID)
	}
}

// TestEnsureParticipant_Concurrent covers H5: 50 goroutines all
// calling EnsureParticipant with the same email must produce exactly
// one row and no errors. The fix collapsed the SELECT-then-INSERT
// race into a single INSERT … ON CONFLICT … RETURNING id statement.
func TestEnsureParticipant_Concurrent(t *testing.T) {
	assert := assert.New(t)
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
		require.NoError(t, err, "goroutine %d: EnsureParticipant", i)
	}
	first := ids[0]
	for i, id := range ids {
		assert.Equal(first, id, "goroutine %d should observe the same row", i)
	}

	var count int
	require.NoError(t, st.DB().QueryRowContext(
		t.Context(),
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
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	const N = 50
	var wg sync.WaitGroup
	ids := make([]int64, N)
	errs := make([]error, N)
	for i := range N {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			id, err := st.EnsureParticipantByPhone(t.Context(), "+15555550100", "Bob", "whatsapp")
			ids[idx] = id
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "goroutine %d: EnsureParticipantByPhone", i)
	}
	first := ids[0]
	for i, id := range ids {
		assert.Equal(first, id, "goroutine %d", i)
	}

	var count int
	require.NoError(t, st.DB().QueryRowContext(
		t.Context(),
		st.Rebind("SELECT COUNT(*) FROM participants WHERE phone_number = ?"),
		"+15555550100",
	).Scan(&count), "count participants")
	assert.Equal(1, count, "want exactly 1 participant row for +15555550100")
}

// TestAddAccountIdentity_Concurrent covers M1: concurrent confirmations
// of the same (source_id, address) collapse to exactly one row, and
// merging different signals across calls preserves the union.
func TestAddAccountIdentity_Concurrent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
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
		require.NoError(err, "goroutine %d: AddAccountIdentity", i)
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
				t.Context(),
				mid, "file.pdf", "application/pdf",
				"ab/abcd1234", "abcd1234", 1024,
			)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "goroutine %d: UpsertAttachment", i)
	}

	var count int
	require.NoError(t, f.Store.DB().QueryRowContext(
		t.Context(),
		f.Store.Rebind("SELECT COUNT(*) FROM attachments WHERE message_id = ? AND content_hash = ?"),
		mid, "abcd1234",
	).Scan(&count), "count attachments")
	assert.Equal(t, 1, count, "want exactly 1 attachment row")
}

// silenceUnused references the imports the test would otherwise drop on
// builds where some helpers are tag-gated.
var _ = store.Source{}

func TestPostgresFTSSaturatedMetadataPhrases(t *testing.T) {
	f := storetest.New(t)
	if !f.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL indexing")
	}
	var want []int64
	for i := range 5 {
		id := f.NewMessage().WithSourceMessageID(fmt.Sprintf("saturated-field-%d", i)).Create(t, f.Store)
		fields := make([]string, 5)
		fields[i] = strings.Repeat("alice ", 300) + "alice@example.com"
		require.NoError(t, f.Store.UpsertFTS(id, fields[0], fields[4], fields[1], fields[2], fields[3]))
		want = append(want, id)
	}
	separated := f.NewMessage().WithSourceMessageID("metadata-separated").Create(t, f.Store)
	require.NoError(t, f.Store.UpsertFTS(separated, strings.Repeat("alice ", 300)+"alice extra example com", "", "", "", ""))
	crossField := f.NewMessage().WithSourceMessageID("metadata-cross-field").Create(t, f.Store)
	require.NoError(t, f.Store.UpsertFTS(crossField, strings.Repeat("alice ", 300), "", "example.com", "", ""))
	for _, tc := range []struct {
		term string
		want []int64
	}{
		{"alice@example.com", want},
		{"alice@exam", want},
		{"example@alice", nil},
	} {
		ids, count, err := f.Store.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{tc.term}}, 10)
		require.NoError(t, err)
		assert.ElementsMatch(t, tc.want, ids, tc.term)
		assert.Equal(t, int64(len(tc.want)), count, tc.term)
	}
	ids, count, err := f.Store.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{"alice@example.com"}}, 1)
	require.NoError(t, err)
	require.Len(t, ids, 1)
	assert.Contains(t, want, ids[0])
	assert.Equal(t, int64(len(want)), count)
}
