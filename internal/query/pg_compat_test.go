package query_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type gmailIDsByMessageIDsResolver interface {
	GetGmailIDsByMessageIDs(ctx context.Context, ids []int64) ([]string, error)
}

type accountsByGmailIDsResolver interface {
	GetAccountsByGmailIDs(ctx context.Context, gmailIDs []string) ([]string, error)
}

// TestQueryEngine_PostgresPortability exercises the three SQL shapes
// the external review flagged as failing on PostgreSQL:
//
//   - Aggregate uses `FROM ( … ) AS agg` (PG rejects unaliased derived
//     tables in FROM).
//   - GetGmailIDsByFilter avoids SELECT DISTINCT entirely (PG rejects
//     DISTINCT when ORDER BY references columns missing from SELECT).
//   - ListMessages sorts via expressions that textually match the
//     SELECT list (PG enforces this for SELECT DISTINCT).
//
// Runs against whichever backend testutil.NewTestStore picks up — on
// CI / dev machines that's SQLite; setting MSGVAULT_TEST_DB to a
// postgres:// DSN exercises the PG path that the bugs were specific
// to. The test never asserts on dialect-specific error text; a bug
// would surface as a generic Scan/Exec failure on PG.
func TestQueryEngine_PostgresPortability(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "pgcompat@example.com")
	require.NoError(err, "GetOrCreateSource")

	convID, err := st.EnsureConversation(src.ID, "thread-1", "Thread 1")
	require.NoError(err, "EnsureConversation")

	aliceID, err := st.EnsureParticipant("alice@example.com", "Alice", "example.com")
	require.NoError(err, "EnsureParticipant alice")
	bobID, err := st.EnsureParticipant("bob@example.com", "Bob", "example.com")
	require.NoError(err, "EnsureParticipant bob")

	labelID, err := st.EnsureLabel(src.ID, "Label_1", "Important", "user")
	require.NoError(err, "EnsureLabel")

	base := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	for i := range 4 {
		mid, err := st.UpsertMessage(&store.Message{
			ConversationID:  convID,
			SourceID:        src.ID,
			SourceMessageID: gmailSourceID(i),
			MessageType:     "email",
			SentAt:          sql.NullTime{Time: base.Add(time.Duration(i) * time.Hour), Valid: true},
			Subject:         sql.NullString{String: subjectFor(i), Valid: true},
			Snippet:         sql.NullString{String: "snippet", Valid: true},
			SizeEstimate:    int64(1000 + i*250),
		})
		require.NoError(err, "UpsertMessage")
		require.NoError(st.ReplaceMessageRecipients(mid, "from", []int64{aliceID}, []string{"Alice"}),
			"ReplaceMessageRecipients from")

		require.NoError(st.ReplaceMessageRecipients(mid, "to", []int64{bobID}, []string{"Bob"}),
			"ReplaceMessageRecipients to")

		require.NoError(st.ReplaceMessageLabels(mid, []int64{labelID}),
			"ReplaceMessageLabels")
	}

	eng := query.NewEngine(st.DB(), st.IsPostgreSQL())
	ctx := context.Background()

	// (1) Aggregate — must not error with "syntax error near ')'" on PG.
	t.Run("aggregate_senders", func(t *testing.T) {
		rows, err := eng.Aggregate(ctx, query.ViewSenders, query.AggregateOptions{
			SortField:     query.SortByCount,
			SortDirection: query.SortDesc,
			Limit:         50,
		})
		require.NoError(err, "Aggregate")
		require.NotEmpty(rows, "Aggregate returned no rows; expected at least the Alice sender bucket")
	})

	// (2) GetGmailIDsByFilter — must not error from a SELECT DISTINCT +
	// ORDER BY collision on PG. Use a label filter to exercise the
	// previously-multiplying join (now an EXISTS subquery).
	t.Run("gmail_ids_by_filter_label", func(t *testing.T) {
		ids, err := eng.GetGmailIDsByFilter(ctx, query.MessageFilter{
			SourceID: &src.ID,
			Label:    "Important",
			Sorting: query.MessageSorting{
				Field:     query.MessageSortByDate,
				Direction: query.SortDesc,
			},
		})
		require.NoError(err, "GetGmailIDsByFilter")
		assert.Len(t, ids, 4, "label join must not multiply")
		// Confirm no duplicates after dropping DISTINCT — every message
		// row should appear exactly once because the label filter is an
		// EXISTS subquery, not a 1:N JOIN.
		seen := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			_, dup := seen[id]
			assert.False(t, dup, "duplicate id %q in result; EXISTS conversion broken", id)
			seen[id] = struct{}{}
		}
	})

	// (3) ListMessages sorted by size and subject — both previously
	// bound raw column references in ORDER BY that did not match the
	// COALESCE-wrapped expressions in the SELECT list. PG rejects that
	// combination under SELECT DISTINCT.
	for _, sort := range []struct {
		name  string
		field query.MessageSortField
	}{
		{"sort_by_size", query.MessageSortBySize},
		{"sort_by_subject", query.MessageSortBySubject},
		{"sort_by_date", query.MessageSortByDate},
	} {
		t.Run("list_messages_"+sort.name, func(t *testing.T) {
			msgs, err := eng.ListMessages(ctx, query.MessageFilter{
				SourceID: &src.ID,
				Sorting: query.MessageSorting{
					Field:     sort.field,
					Direction: query.SortDesc,
				},
				Pagination: query.Pagination{Limit: 50},
			})
			require.NoError(err, "ListMessages %s", sort.name)
			assert.Len(t, msgs, 4, "ListMessages %s", sort.name)
		})
	}
}

func TestNewEnginePostgresSatisfiesGmailIDsByMessageIDsResolver(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	st := testutil.NewTestStore(t)
	eng := query.NewEngine(st.DB(), true)

	resolver, ok := eng.(gmailIDsByMessageIDsResolver)
	require.True(ok, "PostgreSQL engine should expose the deletion message-id resolver")
	ids, err := resolver.GetGmailIDsByMessageIDs(context.Background(), nil)
	require.NoError(err, "empty input should not need a backend query")
	assert.Empty(ids)
}

func TestNewEnginePostgresSatisfiesAccountsByGmailIDsResolver(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	st := testutil.NewTestStore(t)
	eng := query.NewEngine(st.DB(), true)

	resolver, ok := eng.(accountsByGmailIDsResolver)
	require.True(ok, "PostgreSQL engine should expose the deletion account resolver")
	accounts, err := resolver.GetAccountsByGmailIDs(context.Background(), nil)
	require.NoError(err, "empty input should not need a backend query")
	assert.Empty(accounts)
}

func TestNewEnginePostgresSatisfiesMessageBodySearcher(t *testing.T) {
	st := testutil.NewTestStore(t)
	eng := query.NewEngine(st.DB(), true)

	_, ok := eng.(query.MessageBodySearcher)
	require.True(t, ok, "PostgreSQL engine should expose exact message body search")
}

// TestQueryEngine_CaseInsensitiveSearch_Subject verifies that
// `subject:` terms passed through query.Engine.Search match
// case-insensitively on both SQLite and PostgreSQL. SQLite's LIKE is
// ASCII-case-insensitive by default; PostgreSQL's LIKE is
// case-sensitive, so an unwrapped `m.subject LIKE ?` would mis-miss
// rows that the equivalent store API path (which lowercases) returns.
// Bare-LIKE divergence was H3 in the codex review.
func TestQueryEngine_CaseInsensitiveSearch_Subject(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "case-search@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(src.ID, "case-thread", "case thread")
	require.NoError(err, "EnsureConversation")
	mid, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "case-msg-1",
		MessageType:     "email",
		SentAt:          sql.NullTime{Time: time.Date(2024, 7, 1, 12, 0, 0, 0, time.UTC), Valid: true},
		Subject:         sql.NullString{String: "Quarterly Invoice", Valid: true},
		Snippet:         sql.NullString{String: "see attached", Valid: true},
		SizeEstimate:    1024,
	})
	require.NoError(err, "UpsertMessage")
	_ = mid

	eng := query.NewEngine(st.DB(), st.IsPostgreSQL())
	ctx := context.Background()

	for _, term := range []string{"invoice", "INVOICE", "Invoice"} {
		got, err := eng.Search(ctx,
			&search.Query{SubjectTerms: []string{term}}, 50, 0)
		require.NoError(err, "Search subject=%q", term)
		assert.Len(t, got, 1, "subject:%q against stored subject %q", term, "Quarterly Invoice")
	}
}

// TestQueryEngine_MultiFromNoDuplication is the real regression guard for
// the from-side EXISTS conversion (dialect-parity-store-query-1). It builds
// ONE message that legitimately has TWO 'from' participants sharing both a
// domain and a display name — the exact shape that a plain 1:N
// message_recipients JOIN would multiply into two result rows once
// SELECT DISTINCT was dropped. It asserts that:
//
//   - ListMessages under filter.Domain returns exactly one row,
//   - ListMessages under filter.SenderName returns exactly one row, and
//   - SubAggregate (which shares buildFilterJoinsAndConditions) counts the
//     message once, not twice.
//
// Runs on whichever backend testutil.NewTestStore selects; setting
// MSGVAULT_TEST_DB to a postgres:// DSN exercises the PG path too, since
// NewPostgreSQLEngine wraps the same dialect-parameterized builder.
func TestQueryEngine_MultiFromNoDuplication(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "multifrom@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(src.ID, "thread-mf", "Thread MF")
	require.NoError(err, "EnsureConversation")

	// Two distinct 'from' participants that share a domain AND a display
	// name. A single message will carry both as 'from' rows.
	const dupDomain = "dup.example"
	const dupName = "Dup Sender"
	from1, err := st.EnsureParticipant("first@"+dupDomain, dupName, dupDomain)
	require.NoError(err, "EnsureParticipant from1")
	from2, err := st.EnsureParticipant("second@"+dupDomain, dupName, dupDomain)
	require.NoError(err, "EnsureParticipant from2")
	// An unrelated message in another domain so aggregates have >1 bucket.
	otherFrom, err := st.EnsureParticipant("other@other.example", "Other", "other.example")
	require.NoError(err, "EnsureParticipant other")

	base := time.Date(2024, 7, 1, 9, 0, 0, 0, time.UTC)
	multiID, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "gmail-multi-from",
		MessageType:     "email",
		SentAt:          sql.NullTime{Time: base, Valid: true},
		Subject:         sql.NullString{String: "multi from", Valid: true},
		Snippet:         sql.NullString{String: "snippet", Valid: true},
		SizeEstimate:    1234,
	})
	require.NoError(err, "UpsertMessage multi")
	// The crux: two 'from' rows on one message, both in dupDomain/dupName.
	require.NoError(st.ReplaceMessageRecipients(multiID, "from",
		[]int64{from1, from2}, []string{dupName, dupName}),
		"ReplaceMessageRecipients two from rows")

	otherID, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "gmail-other-from",
		MessageType:     "email",
		SentAt:          sql.NullTime{Time: base.Add(time.Hour), Valid: true},
		Subject:         sql.NullString{String: "other from", Valid: true},
		Snippet:         sql.NullString{String: "snippet", Valid: true},
		SizeEstimate:    1000,
	})
	require.NoError(err, "UpsertMessage other")
	require.NoError(st.ReplaceMessageRecipients(otherID, "from",
		[]int64{otherFrom}, []string{"Other"}), "ReplaceMessageRecipients other")

	eng := query.NewEngine(st.DB(), st.IsPostgreSQL())
	ctx := context.Background()

	t.Run("list_messages_domain", func(t *testing.T) {
		msgs, err := eng.ListMessages(ctx, query.MessageFilter{
			SourceID:   &src.ID,
			Domain:     dupDomain,
			Sorting:    query.MessageSorting{Field: query.MessageSortByDate, Direction: query.SortDesc},
			Pagination: query.Pagination{Limit: 50},
		})
		require.NoError(err, "ListMessages domain")
		assert.Len(t, msgs, 1, "multi-from message must appear exactly once under Domain filter")
		assert.Equal(t, multiID, msgs[0].ID, "the multi-from message")
	})

	t.Run("list_messages_sender_name", func(t *testing.T) {
		msgs, err := eng.ListMessages(ctx, query.MessageFilter{
			SourceID:   &src.ID,
			SenderName: dupName,
			Sorting:    query.MessageSorting{Field: query.MessageSortByDate, Direction: query.SortDesc},
			Pagination: query.Pagination{Limit: 50},
		})
		require.NoError(err, "ListMessages sender name")
		assert.Len(t, msgs, 1, "multi-from message must appear exactly once under SenderName filter")
		assert.Equal(t, multiID, msgs[0].ID, "the multi-from message")
	})

	// SubAggregate shares buildFilterJoinsAndConditions. Group by ViewTime
	// (one bucket per message's year — a single-valued grouping that does
	// not itself multiply) so the count isolates the Domain filter-join:
	// the multi-from message must be counted once, not once per 'from' row.
	t.Run("subaggregate_domain_count", func(t *testing.T) {
		rows, err := eng.SubAggregate(ctx,
			query.MessageFilter{SourceID: &src.ID, Domain: dupDomain},
			query.ViewTime,
			query.AggregateOptions{
				TimeGranularity: query.TimeYear,
				SortField:       query.SortByCount,
				SortDirection:   query.SortDesc,
				Limit:           50,
			},
		)
		require.NoError(err, "SubAggregate domain")
		var total int64
		for _, r := range rows {
			total += r.Count
		}
		assert.Equal(t, int64(1), total,
			"Domain sub-filter must count the multi-from message once, not per 'from' row")
	})
}

// TestQueryEngine_FTSBodySearch is the regression guard for cf-4: on
// PostgreSQL, query.Engine.Search must take the tsvector FTS path
// (search_fts @@ to_tsquery), not the subject/snippet LIKE fallback. The bug
// was that hasFTSTable ran a hardcoded SQLite-only `SELECT 1 FROM messages_fts`
// probe even on PG, where that relation does not exist; the probe errored, FTS
// was cached as unavailable, and PG Search silently degraded to LIKE over
// subject/snippet only — so body-only terms returned zero rows.
//
// The discriminating term ("zorblax") lives ONLY in the body, never in the
// subject or snippet, so the LIKE fallback cannot match it. A passing result
// therefore proves the FTS path is engaged. Runs on whichever backend
// testutil.NewTestStore selects, so it doubles as a SQLite<->PG parity test:
// both backends must return the body-only hit.
func TestQueryEngine_FTSBodySearch(t *testing.T) {
	assert :=
		assert.
			New(t)

	require := require.New(t)
	st := testutil.NewTestStore(t)
	if !st.FTS5Available() {
		t.Skip("full-text search index not available on this backend")
	}

	src, err := st.GetOrCreateSource("gmail", "fts-body@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(src.ID, "fts-thread", "fts thread")
	require.NoError(err, "EnsureConversation")

	const bodyOnlyTerm = "zorblax"
	mid, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "fts-body-msg-1",
		MessageType:     "email",
		SentAt:          sql.NullTime{Time: time.Date(2024, 8, 1, 12, 0, 0, 0, time.UTC), Valid: true},
		// Neither subject nor snippet contains the discriminating term, so a
		// LIKE-over-subject/snippet fallback cannot match it.
		Subject:      sql.NullString{String: "Quarterly report", Valid: true},
		Snippet:      sql.NullString{String: "see attached summary", Valid: true},
		SizeEstimate: 2048,
	})
	require.NoError(err, "UpsertMessage")
	body := "The hidden keyword " + bodyOnlyTerm + " appears only in the message body."
	require.NoError(st.UpsertMessageBody(mid,
		sql.NullString{String: body, Valid: true},
		sql.NullString{}), "UpsertMessageBody")

	// Index the message for full-text search (search_fts on PG, messages_fts on SQLite).
	require.NoError(st.UpsertFTS(mid, "Quarterly report", body, "", "", ""), "UpsertFTS")

	eng := query.NewEngine(st.DB(), st.IsPostgreSQL())
	ctx := context.Background()

	// (1) Body-only TextTerms search must find the message — only the FTS path
	// can, since the term is absent from subject/snippet.
	got, err := eng.Search(ctx, &search.Query{TextTerms: []string{bodyOnlyTerm}}, 50, 0)
	require.NoError(err, "Search TextTerms=%q", bodyOnlyTerm)
	assert.Len(got, 1,
		"body-only term %q must be found via FTS (subject/snippet LIKE fallback would return 0)", bodyOnlyTerm)
	if len(got) == 1 {
		assert.Equal(mid, got[0].ID, "the indexed message")
	}

	// (2) Control: a subject term is found on both the FTS and LIKE paths.
	gotSubj, err := eng.Search(ctx, &search.Query{TextTerms: []string{"Quarterly"}}, 50, 0)
	require.NoError(err, "Search TextTerms=Quarterly")
	assert.Len(gotSubj, 1, "subject term must match")
}

// TestQueryDialect_HasFTSTableSQL_SchemaScoped pins cr2-8: the PG query
// dialect's FTS existence probe must be scoped to the connection's
// current_schema(), matching the store dialect. Without the scope, a
// schema-scoped connection whose own messages table lacks search_fts would
// falsely report FTS available because a SIBLING schema (or public) has a
// messages.search_fts column — then the actual search errors with
// "column m.search_fts does not exist".
//
// Two schemas are created: one WITH messages.search_fts, one WITHOUT. The
// connection's search_path is scoped to the one WITHOUT it; the probe must
// return 0 (FTS not available here) even though a sibling has the column.
func TestQueryDialect_HasFTSTableSQL_SchemaScoped(t *testing.T) {
	require := require.New(t)
	dbURL := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(dbURL, "postgres://") && !strings.HasPrefix(dbURL, "postgresql://") {
		t.Skip("cr2-8 schema-scoping test requires MSGVAULT_TEST_DB pointing at PostgreSQL")
	}

	buf := make([]byte, 8)
	_, err := rand.Read(buf)
	require.NoError(err, "random suffix")
	suffix := hex.EncodeToString(buf)
	hasSchema := "cr2_8_has_" + suffix
	noSchema := "cr2_8_no_" + suffix

	setupDB, err := sql.Open("pgx", dbURL)
	require.NoError(err, "open setup connection")
	defer func() { _ = setupDB.Close() }()
	for _, s := range []struct {
		name    string
		withFTS bool
	}{{hasSchema, true}, {noSchema, false}} {
		_, err := setupDB.Exec("CREATE SCHEMA " + s.name)
		require.NoErrorf(err, "create schema %s", s.name)
		cols := "id BIGINT"
		if s.withFTS {
			cols += ", search_fts TSVECTOR"
		}
		_, err = setupDB.Exec("CREATE TABLE " + s.name + ".messages (" + cols + ")")
		require.NoErrorf(err, "create messages in %s", s.name)
	}
	t.Cleanup(func() {
		_, _ = setupDB.Exec("DROP SCHEMA " + hasSchema + " CASCADE")
		_, _ = setupDB.Exec("DROP SCHEMA " + noSchema + " CASCADE")
	})

	// Connection scoped to the schema WITHOUT search_fts.
	sep := "?"
	if strings.Contains(dbURL, "?") {
		sep = "&"
	}
	scopedDB, err := sql.Open("pgx", dbURL+sep+"search_path="+noSchema)
	require.NoError(err, "open scoped connection")
	defer func() { _ = scopedDB.Close() }()

	var count int
	require.NoError(
		scopedDB.QueryRow(query.PostgreSQLQueryDialect{}.HasFTSTableSQL()).Scan(&count),
		"run HasFTSTableSQL probe")

	assert.Equal(t, 0, count,
		"schema-scoped FTS probe must not count a sibling schema's messages.search_fts")
}

// TestQueryEngine_SearchDateRange_TimezoneStable pins cr2-9: the search
// date-range filter must bind time.Time directly rather than a naive,
// offset-less "2006-01-02 15:04:05" string. A formatted string compared to a
// PG TIMESTAMPTZ column is cast using the session TimeZone, so under a
// non-UTC session the UTC-midnight boundary shifts and excludes rows it
// should include. Binding time.Time lets pgx encode an explicit offset, which
// is timezone-stable.
//
// The message sits at 2024-03-15 02:00 UTC; the after-bound is 2024-03-15
// 00:00 UTC. Under America/Los_Angeles a naive '2024-03-15 00:00:00' would be
// read as 07:00 UTC and exclude the message. With time.Time binding it is
// included regardless of session TimeZone.
func TestQueryEngine_SearchDateRange_TimezoneStable(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("cr2-9 timezone-stability is PostgreSQL-specific (SQLite has no session TimeZone for TIMESTAMPTZ casts)")
	}

	src, err := st.GetOrCreateSource("gmail", "tz-search@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversation(src.ID, "tz-thread", "tz thread")
	require.NoError(err, "EnsureConversation")

	mid, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: "tz-msg-1",
		MessageType:     "email",
		SentAt:          sql.NullTime{Time: time.Date(2024, 3, 15, 2, 0, 0, 0, time.UTC), Valid: true},
		Subject:         sql.NullString{String: "tz boundary", Valid: true},
		Snippet:         sql.NullString{String: "snippet", Valid: true},
		SizeEstimate:    1024,
	})
	require.NoError(err, "UpsertMessage")

	// Pin to a single connection so SET TIME ZONE (and the test schema's
	// search_path, applied per-connection by pgx) both stick for the search.
	db := st.DB()
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		_, _ = db.Exec("SET TIME ZONE 'UTC'")
		db.SetMaxOpenConns(0)
	})

	after := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)
	before := time.Date(2024, 3, 16, 0, 0, 0, 0, time.UTC)
	q := &search.Query{AfterDate: &after, BeforeDate: &before}

	eng := query.NewEngine(db, true)
	ctx := context.Background()

	for _, tz := range []string{"UTC", "America/Los_Angeles"} {
		_, err := db.ExecContext(ctx, "SET TIME ZONE '"+tz+"'")
		require.NoErrorf(err, "SET TIME ZONE %q", tz)
		got, err := eng.Search(ctx, q, 50, 0)
		require.NoErrorf(err, "Search under TZ=%q", tz)
		require.Lenf(got, 1,
			"date-range search must include the 02:00 UTC message under TZ=%q (timezone-stable bind)", tz)
		assert.Equalf(t, mid, got[0].ID, "the in-range message under TZ=%q", tz)
	}
}

func gmailSourceID(i int) string {
	return "gmail-msg-" + string(rune('a'+i))
}

func subjectFor(i int) string {
	switch i {
	case 0:
		return "alpha subject"
	case 1:
		return "beta subject"
	case 2:
		return "gamma subject"
	default:
		return "delta subject"
	}
}
