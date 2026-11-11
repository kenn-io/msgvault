package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSQLite_BackfillFTS_ColumnsizeZero(t *testing.T) {
	if isPostgresTestDB() {
		t.Skip("SQLite FTS5 table option")
	}
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	_, err := f.Store.DB().ExecContext(t.Context(), `DROP TABLE messages_fts;
		CREATE VIRTUAL TABLE messages_fts USING fts5(
			message_id UNINDEXED, subject, body, from_addr, to_addr, cc_addr, columnsize=0)`)
	require.NoError(err)
	ids := f.CreateMessages(2)
	for _, id := range ids {
		require.NoError(f.Store.UpsertMessageBody(id,
			sql.NullString{String: "orchard", Valid: true}, sql.NullString{}))
	}
	_, err = f.Store.BackfillFTS(nil)
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), "DELETE FROM messages_fts WHERE rowid = ?", ids[0])
	require.NoError(err)
	n, err := f.Store.BackfillFTS(nil)
	require.NoError(err)
	assert.Equal(int64(1), n)
	assertFTSContains(t, f.Store, "orchard", 2)
}

func TestStore_BackfillFTS_ResumesCommittedBatch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	if !f.Store.FTS5Available() {
		t.Skip("FTS5 not available")
	}
	first := f.CreateMessage("msg-0")
	// Keep the second message in the next 5,000-ID backfill batch.
	second := seedMessageAtID(t, f.Store, 5000, first+5000)
	for _, id := range []int64{first, second} {
		require.NoError(f.Store.UpsertMessageBody(id,
			sql.NullString{String: "orchard", Valid: true}, sql.NullString{}))
	}
	_, err := f.Store.BackfillFTS(nil)
	require.NoError(err)
	assertFTSContains(t, f.Store, "orchard", 2)
	if isPostgresTestDB() {
		_, err = f.Store.DB().ExecContext(t.Context(), "UPDATE messages SET search_fts = NULL WHERE id IN ($1, $2)", first, second)
	} else {
		_, err = f.Store.DB().ExecContext(t.Context(), "DELETE FROM messages_fts WHERE rowid IN (?, ?)", first, second)
	}
	require.NoError(err)

	interrupted := errors.New("second batch interrupted")
	restore := f.Store.SetBackfillFTSBatchErrHookForTest(func(fromID, _ int64) error {
		if fromID >= second {
			return interrupted
		}
		return nil
	})
	t.Cleanup(restore)
	n, err := f.Store.BackfillFTS(nil)
	require.ErrorIs(err, interrupted)
	assert.Equal(int64(1), n, "first batch committed before the interruption")
	matches, total, err := f.Store.SearchMessages("orchard", 0, 10)
	require.NoError(err)
	assert.Equal(int64(1), total)
	require.Len(matches, 1)
	assert.Equal(first, matches[0].ID)
	restore()

	n, err = f.Store.BackfillFTS(nil)
	require.NoError(err)
	assert.Equal(int64(1), n, "retry indexes only the second-batch gap")
	assertFTSContains(t, f.Store, "orchard", 2)
}

// TestPG_BackfillFTSUpgradesLegacyEntries rebuilds version-2 entries from
// stored data and keeps only subject, sender, and recipient text it lacks.
func TestPG_BackfillFTSUpgradesLegacyEntries(t *testing.T) {
	skipUnlessPostgres(t)
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	ids := f.CreateMessages(2)
	_, err := f.Store.BackfillFTS(nil)
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), `UPDATE messages
		SET search_fts = to_tsvector('simple', 'obsolete'), indexing_version = $1
		WHERE id = $2`, store.CurrentFTSIndexingVersion-1, ids[1])
	require.NoError(err)

	n, err := f.Store.BackfillFTS(nil)
	require.NoError(err)
	assert.Equal(int64(1), n, "only the outdated entry needs backfill")
	assertFTSContains(t, f.Store, "obsolete", 0)
	db := f.Store.DB()
	legacy := func(id int64, vector string, args ...any) {
		t.Helper()
		_, err := db.ExecContext(t.Context(), `UPDATE messages SET search_fts = `+vector+`, indexing_version = 2 WHERE id = $1`, append([]any{id}, args...)...)
		require.NoError(err)
	}
	withBody := func(id int64, body string) int64 {
		t.Helper()
		require.NoError(f.Store.UpsertMessageBody(id, sql.NullString{String: body, Valid: true}, sql.NullString{}))
		return id
	}
	compound := f.CreateMessage("compound")
	legacy(compound, `setweight(to_tsvector('simple', 'before foo-bar after'), 'A')`)
	url := withBody(f.CreateMessage("url"), "see https://example.test/path")
	legacy(url, `to_tsvector('simple', 'see https://example.test/path')`)
	slack := withBody(f.CreateMessage("slack"), strings.Repeat("the ", 300))
	legacy(slack, `setweight(to_tsvector('simple', 'Lantern Harbor'), 'A') || setweight(to_tsvector('simple', 'Lantern Sender'), 'B') || setweight(to_tsvector('simple', $2), 'D')`, strings.Repeat("the ", 300))
	phone := f.CreateMessage("imazing")
	legacy(phone, `setweight(to_tsvector('simple', '+15550123457'), 'C')`)
	recipients := f.CreateMessage("recipients")
	require.NoError(f.Store.ReplaceMessageRecipients(recipients, "to", []int64{f.EnsureParticipant("alice", "", "")}, nil))
	require.NoError(f.Store.ReplaceMessageRecipients(recipients, "cc", []int64{f.EnsureParticipant("example.com", "", "")}, nil))
	legacy(recipients, `setweight(to_tsvector('simple', 'alice example.com zed'), 'C')`)
	legacy(withBody(f.CreateMessage("unsearchable"), "hidden terminal@example.test"), `''::tsvector`)
	gapped := f.CreateMessage("gapped")
	_, err = db.ExecContext(t.Context(), `UPDATE messages SET subject = 'different stored subject' WHERE id = $1`, gapped)
	require.NoError(err)
	legacy(gapped, `setweight(to_tsvector('simple', $2), 'A')`, strings.Repeat("filler ", 256)+"alpha filler beta")
	legacy(f.CreateMessage("compound-gap"), `setweight(to_tsvector('simple', $2), 'A')`, strings.Repeat("filler ", 256)+"alpha filler foo-bar")
	tied := f.CreateMessage("tied-positions")
	legacy(tied, `setweight(to_tsvector('simple', $2), 'A')`, strings.Repeat("padding ", 16384)+"cobalt xenon")
	fresh, retained, partial := f.CreateMessage("fresh"), f.CreateMessage("retained"), f.CreateMessage("partial")
	_, err = db.ExecContext(t.Context(), `UPDATE messages SET subject = 'lantern' WHERE id IN ($1, $2, $3)`, fresh, retained, partial)
	require.NoError(err)
	require.NoError(f.Store.UpsertFTS(fresh, "lantern", "", "", "", ""))
	legacy(retained, `setweight(to_tsvector('simple', 'lantern'), 'A')`)
	withBody(partial, strings.Repeat("bodypaddingone bodypaddingtwo bodypaddingthree bodypaddingfour bodypaddingfive ", 120)+"stored phrase")
	legacy(partial, `setweight(to_tsvector('simple', 'lantern unique'), 'A')`)

	_, err = f.Store.BackfillFTS(nil)
	require.NoError(err)
	var currentFields bool
	require.NoError(db.QueryRowContext(t.Context(), `SELECT search_fts_fields[1] = 'lantern' AND search_fts_fields[5] LIKE '%stored phrase' FROM messages WHERE id = $1`, partial).Scan(&currentFields))
	assert.True(currentFields, "legacy rows must retain current fields for phrase checks")
	for term, want := range map[string][]int64{
		"before foo-bar after":      {compound},
		"bar foo":                   nil,
		"lantern harbor":            {slack},
		"https://example.test/path": {url},
		"lantern sender":            {slack},
		"+15550123457":              {phone},
		"alice@example.com":         nil,
		"terminal@example.test":     nil,
		"alpha beta":                nil,
		"alpha foo":                 nil,
		"cobalt xenon":              nil,
		"xenon cobalt":              nil,
		"cobalt":                    {tied},
		"xenon":                     {tied},
		"unique":                    {partial},
		"lantern unique":            {partial},
		"stored phrase":             {partial},
	} {
		ids, _, err := f.Store.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{term}}, 10)
		require.NoError(err, term)
		assert.ElementsMatch(want, ids, term)
	}
	ids, _, err = f.Store.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{"stored phrase", "lantern unique"}}, 10)
	require.NoError(err)
	require.Equal([]int64{partial}, ids)
	bodySearcher, ok := query.NewEngine(db, true).(query.MessageBodySearcher)
	require.True(ok)
	for term, want := range map[string][]int64{"stored phrase": {partial}, "unique": nil, "the the": {slack}} {
		result, err := bodySearcher.SearchMessageBodies(t.Context(), &search.Query{TextTerms: []string{term}}, 10, 0)
		require.NoError(err, term)
		var got []int64
		for _, message := range result.Messages {
			got = append(got, message.ID)
		}
		assert.ElementsMatch(want, got, term)
	}
	rank := func(id int64) float64 {
		t.Helper()
		var value float64
		require.NoError(db.QueryRowContext(t.Context(), `SELECT ts_rank(ARRAY[0.1, 0.1, 0.4, 1.0]::real[], search_fts, to_tsquery('simple', 'lantern:*')) FROM messages WHERE id = $1`, id).Scan(&value))
		return value
	}
	for _, id := range []int64{retained, partial} {
		assert.InDelta(rank(fresh), rank(id), 0.000001)
	}
}

// TestPG_BackfillFTSKeepsMergedRecipientAlias repairs a version-2 entry after
// a participant merge: the original To alias stays searchable and recovered
// recipient text does not join To and Cc into one phrase.
func TestPG_BackfillFTSKeepsMergedRecipientAlias(t *testing.T) {
	skipUnlessPostgres(t)
	for _, snapshot := range []bool{true, false} {
		t.Run(fmt.Sprintf("snapshot=%t", snapshot), func(t *testing.T) {
			require := require.New(t)
			f := storetest.New(t)
			from := f.EnsureParticipant("sender@example.com", "", "example.com")
			alias := f.EnsureParticipant("old.alias@example.com", "", "example.com")
			kept := f.EnsureParticipant("kept@example.net", "", "example.net")
			carol := f.EnsureParticipant("carol@example.org", "", "example.org")
			var toAddresses []string
			if snapshot {
				toAddresses = []string{"old.alias@example.com"}
			}
			id, err := f.Store.PersistMessageContext(t.Context(), &store.MessagePersistData{
				Message: &store.Message{
					SourceID: f.Source.ID, ConversationID: f.ConvID, MessageType: store.MessageTypeEmail,
					SourceMessageID: "merged-alias", SenderID: sql.NullInt64{Int64: from, Valid: true},
				},
				Recipients: []store.RecipientSet{
					{Type: "from", ParticipantIDs: []int64{from}, EmailAddresses: []string{"sender@example.com"}},
					{Type: "to", ParticipantIDs: []int64{alias}, EmailAddresses: toAddresses},
					{Type: "cc", ParticipantIDs: []int64{carol}, EmailAddresses: []string{"carol@example.org"}},
				},
			})
			require.NoError(err)
			if !snapshot {
				var address sql.NullString
				require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT email_address FROM message_recipients WHERE message_id = $1 AND recipient_type = 'to'`, id).Scan(&address))
				require.False(address.Valid)
			}
			_, err = f.Store.DB().ExecContext(t.Context(), `UPDATE messages SET indexing_version = 2,
		search_fts = setweight(to_tsvector('simple', 'sender@example.com'), 'B') || setweight(to_tsvector('simple', 'old.alias@example.com carol@example.org'), 'C')
		WHERE id = $1`, id)
			require.NoError(err)
			require.NoError(f.Store.MergeParticipantsContext(t.Context(), alias, kept))
			assert.Equal(t, kept, f.GetSingleRecipientID(id, "to"))

			_, err = f.Store.BackfillFTS(nil)
			require.NoError(err)
			count, err := f.Store.BackfillFTS(nil)
			require.NoError(err)
			require.Zero(count)
			for term, want := range map[string][]int64{
				"old":                   {id},
				"old.alias@example.com": {id},
				"carol@example.org":     {id},
				"example.com carol":     nil,
				"carol example.com":     nil,
			} {
				ids, _, err := f.Store.SearchMessageIDsQueryContext(t.Context(), &search.Query{TextTerms: []string{term}}, 10)
				require.NoError(err, term)
				assert.ElementsMatch(t, want, ids, term)
			}
		})
	}
}

func TestPG_BackfillFTS_ContinuesAcrossLargeMessagePages(t *testing.T) {
	skipUnlessPostgres(t)
	f := storetest.New(t)
	body := strings.Repeat("filler ", 83000) + "alice@example.com"
	for _, id := range f.CreateMessages(8) {
		require.NoError(t, f.Store.UpsertMessageBody(id, sql.NullString{String: body, Valid: true}, sql.NullString{}))
	}
	count, err := f.Store.BackfillFTS(nil)
	require.NoError(t, err)
	assert.EqualValues(t, 8, count)
	assertFTSContains(t, f.Store, "alice@example.com", 8)
}

func TestPG_FTSSearchUsesIndex(t *testing.T) {
	skipUnlessPostgres(t)
	require := require.New(t)
	f := storetest.New(t)
	tx, err := f.Store.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = tx.ExecContext(t.Context(), `SET LOCAL enable_seqscan = off`)
	require.NoError(err)
	dialect := &store.PostgreSQLDialect{}
	_, predicate, _, _ := dialect.FTSSearchClause(false, []string{"lantern unique"})
	rows, err := tx.QueryContext(t.Context(), f.Store.Rebind(`EXPLAIN SELECT m.id FROM messages m WHERE `+predicate), dialect.BuildFTSArg([]string{"lantern unique"}))
	require.NoError(err)
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var line string
		require.NoError(rows.Scan(&line))
		plan = append(plan, line)
	}
	require.NoError(rows.Err())
	assert.Contains(t, strings.Join(plan, "\n"), "Bitmap Index Scan on messages_search_fts_idx")
	assert.Equal(t, 1, strings.Count(strings.Join(plan, "\n"), " on messages "), "search must scan messages once")
}

func TestPG_BackfillFTS_SeesConcurrentBodyUpdate(t *testing.T) {
	require := require.New(t)
	skipUnlessPostgres(t)
	f := storetest.New(t)
	id := f.CreateMessage("concurrent-backfill")
	require.NoError(f.Store.UpsertMessageBody(id, sql.NullString{String: "old body", Valid: true}, sql.NullString{}))
	tx, err := f.Store.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = tx.ExecContext(t.Context(), `UPDATE messages SET subject = 'updated', search_fts = NULL WHERE id = $1`, id)
	require.NoError(err)
	_, err = tx.ExecContext(t.Context(), `UPDATE message_bodies SET body_text = 'new@example.com' WHERE message_id = $1`, id)
	require.NoError(err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := f.Store.BackfillFTSContext(ctx, nil)
		done <- err
	}()
	require.Eventually(func() bool {
		var waiting bool
		err := f.Store.DB().QueryRowContext(t.Context(), `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname = current_database() AND query LIKE 'SELECT m.id FROM messages m WHERE m.id IN (%' AND cardinality(pg_blocking_pids(pid)) > 0)`).Scan(&waiting)
		return err == nil && waiting
	}, 30*time.Second, 20*time.Millisecond)
	require.NoError(tx.Commit())
	require.NoError(<-done)
	assertFTSContains(t, f.Store, "new@example.com", 1)
	assertFTSContains(t, f.Store, "old", 0)
}

// isPostgresTestDB reports whether the active test backend is PostgreSQL,
// inferred from MSGVAULT_TEST_DB. Used by backend-portable tests that must issue
// dialect-specific SQL to set up a scenario (e.g. punching a hole in the FTS
// index differs between SQLite's messages_fts shadow table and PG's search_fts
// column).
func isPostgresTestDB() bool {
	db := os.Getenv("MSGVAULT_TEST_DB")
	return strings.HasPrefix(db, "postgres://") || strings.HasPrefix(db, "postgresql://")
}

// TestStore_NeedsFTSBackfill_Transition (finding for P2) verifies the
// FTSNeedsBackfill contract on BOTH backends: it reports true while any message
// lacks an FTS entry and false once backfill has populated them all. On
// PostgreSQL the probe is the EXISTS(search_fts IS NULL) short-circuit; on
// SQLite it is the MAX(rowid) vs MAX(id) comparison. Both must agree.
func TestStore_NeedsFTSBackfill_Transition(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	if !f.Store.FTS5Available() {
		t.Skip("FTS5 not available")
	}

	// Seed enough rows that SQLite's 10%-slack MAX comparison is unambiguous
	// (with 20 unindexed rows it cannot round to "already backfilled").
	const total = 20
	ids := f.CreateMessages(total)
	require.Len(ids, total)

	assert.True(f.Store.NeedsFTSBackfill(),
		"NeedsFTSBackfill must be true while messages have no FTS entry")

	_, err := f.Store.BackfillFTS(nil)
	require.NoError(err, "BackfillFTS")

	assert.False(f.Store.NeedsFTSBackfill(),
		"NeedsFTSBackfill must be false after a complete backfill")
}

// TestStore_NeedsFTSBackfillQuick_Transition verifies the cheap probe's
// contract on both backends: true while the index tail is unindexed, false
// once backfill completes. (Interior holes are explicitly out of contract on
// SQLite — the full NeedsFTSBackfill anti-join is authoritative for those.)
func TestStore_NeedsFTSBackfillQuick_Transition(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	if !f.Store.FTS5Available() {
		t.Skip("FTS5 not available")
	}

	const total = 20
	ids := f.CreateMessages(total)
	require.Len(ids, total)

	assert.True(f.Store.NeedsFTSBackfillQuick(),
		"quick probe must be true while the index tail is unindexed")

	_, err := f.Store.BackfillFTS(nil)
	require.NoError(err, "BackfillFTS")

	assert.False(f.Store.NeedsFTSBackfillQuick(),
		"quick probe must be false after a complete backfill")
}

// TestStore_NeedsFTSBackfill_HoleAtLowestID (F4) verifies that a hole left at a
// LOW id while later ids are indexed is detected on BOTH backends. This is the
// case the old SQLite MAX(rowid)-vs-MAX(id) heuristic missed: the FTS MAX still
// equals the messages MAX, so it reported "no backfill needed" even though id 1
// was unindexed. Holes are reachable in practice because UpsertFTS failures
// during sync are warn-and-continue while the message row still commits.
//
// Runs on both backends; before the fix this passed on PG (EXISTS probe) and
// failed on SQLite, proving the divergence.
func TestStore_NeedsFTSBackfill_HoleAtLowestID(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	if !f.Store.FTS5Available() {
		t.Skip("FTS5 not available")
	}

	const total = 20
	ids := f.CreateMessages(total)
	require.Len(ids, total)

	_, err := f.Store.BackfillFTS(nil)
	require.NoError(err, "BackfillFTS")
	require.False(f.Store.NeedsFTSBackfill(),
		"precondition: fully backfilled index must not need backfill")

	// Remove the FTS entry for the LOWEST id only. The highest id stays indexed,
	// so any MAX-based heuristic would wrongly report "complete".
	lowest := slices.Min(ids)
	if isPostgresTestDB() {
		_, err = f.Store.DB().ExecContext(
			t.Context(),
			"UPDATE messages SET search_fts = NULL WHERE id = $1", lowest)
	} else {
		_, err = f.Store.DB().ExecContext(
			t.Context(),
			"DELETE FROM messages_fts WHERE rowid = ?", lowest)
	}
	require.NoError(err, "punch a hole at the lowest id")

	assert.True(t, f.Store.NeedsFTSBackfill(),
		"NeedsFTSBackfill must be true when a LOW id is unindexed even if later ids are indexed")
}
