package store_test

import (
	"context"
	"fmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/filenamediscovery"
	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"strings"
	"testing"
)

// Literal edit cases catch missing deletion/insert/substitute/swap streams,
// including first-letter edits and a four-rune shorter candidate.
func TestFilenameCandidatesSeekCoverage(t *testing.T) {
	assert := assert.New(t)

	names := []string{"report.pdf", "Report.PDF", "réport.pdf", "xreport.pdf", "eport.pdf",
		"reports.pdf", "reprot.pdf", "repqrt.pdf", "repor.pdf", "reporrt.pdf", "repqst.pdf", "unrelated.pdf"}
	f, ids := filenameCandidateFixture(t, names)
	set := filenameCandidates(t, f.Store, "report.pdf")
	assert.False(set.CandidateLimitReached)
	assert.ElementsMatch(ids[:2], set.IDs[:2], "full-name hits precede all other streams")
	for _, index := range []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9} {
		assert.Contains(set.IDs, ids[index], names[index])
	}
	assert.NotContains(set.IDs, ids[11])
	short, shortIDs := filenameCandidateFixture(t, []string{"abcde.pdf", "abce.pdf", "xbcde.pdf", "abced.pdf", "abcdee.pdf"})
	assert.ElementsMatch(shortIDs, filenameCandidates(t, short.Store, "abcde.pdf").IDs)
}

func TestFilenameCandidatesNumericSeed(t *testing.T) {
	assert := assert.New(t)

	f, ids := filenameCandidateFixture(t, []string{"12345.pdf", "x12345.pdf", "123456.pdf", "12354.pdf", "12345 extra.pdf"})
	set := filenameCandidates(t, f.Store, "12345.pdf")
	assert.Equal([]int64{ids[0], ids[4]}, set.IDs, "all-number seed uses equality only")
	assert.Equal(3, set.PostingRows, "one full-name hit plus two exact-token hits")
	assert.False(set.CandidateLimitReached)
	var deletes int
	require.NoError(t, f.Store.DB().QueryRow(f.Store.Rebind(`SELECT count(*) FROM filename_discovery_deletes WHERE attachment_id=?`), ids[0]).Scan(&deletes))
	assert.Zero(deletes, "an all-number basename needs no typo postings")
}

func TestFilenameCandidatesMixedNumbers(t *testing.T) {
	for _, number := range []string{"2", "٢"} {
		t.Run(number, func(t *testing.T) {
			names := []string{"abc2de.pdf", "ab2cde.pdf", "abc3de.pdf", "abc2df.pdf", "abc2ed.pdf", "xabc2de.pdf", "ab2de.pdf", "abcde.pdf"}
			for i := range names {
				names[i] = strings.ReplaceAll(names[i], "2", number)
			}
			f, ids := filenameCandidateFixture(t, names)
			set := filenameCandidates(t, f.Store, "abc"+number+"de")
			assert.ElementsMatch(t, []int64{ids[0], ids[3], ids[4], ids[5], ids[6]}, set.IDs)
			assert.False(t, set.CandidateLimitReached)
		})
	}
}

func TestFilenameCandidatesUnicodeKeys(t *testing.T) {
	assert := assert.New(t)

	f, ids := filenameCandidateFixture(t, []string{"ab界de.pdf", "ab界d.pdf", "xb界de.pdf", "ab界dee.pdf", "ab界de_more.pdf", "unrelated.pdf"})
	set := filenameCandidates(t, f.Store, "ab界de.pdf")
	assert.False(set.CandidateLimitReached)
	assert.ElementsMatch(ids[:5], set.IDs)
	prefix := filenameCandidates(t, f.Store, "ab界")
	assert.ElementsMatch([]int64{ids[0], ids[1], ids[3], ids[4]}, prefix.IDs)
	assert.False(prefix.CandidateLimitReached)
}

// Equal-length terms can select different posting seeds, but the whole query
// must match and the final candidate order follows rank and recency.
func TestFilenameCandidatesStableOrder(t *testing.T) {
	f, ids := filenameCandidateFixture(t, []string{
		"hello alpha.pdf", "world beta.pdf", "xello alpha.pdf", "wqrld beta.pdf",
		"hello world.pdf", "helloworld world.pdf", "xello world.pdf", "hello wqrld.pdf", "xello wqrld.pdf",
	})
	first := filenameCandidates(t, f.Store, "hello world.pdf")
	want := []int64{ids[4], ids[5], ids[7], ids[6]}
	assert.Equal(t, want, first.IDs)
	assert.Equal(t, first, filenameCandidates(t, f.Store, "hello world.pdf"))
	assert.Equal(t, want, filenameCandidates(t, f.Store, "world hello.pdf").IDs)
}

// Requests cannot register/bootstrap/repair an index, even after an otherwise
// ready generation loses its journal consumer. Cancellation is an error.
func TestFilenameCandidatesUnavailableAndCancellation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := storetest.New(t)
	q, err := filenamediscovery.ParseQuery("report.pdf")
	require.NoError(err)
	_, err = f.Store.FilenameCandidatesContext(t.Context(), q, store.FilenameSuggestionScope{})
	require.ErrorIs(err, store.ErrFilenameIndexUnavailable)
	status, err := f.Store.FilenameDiscoveryStatusContext(t.Context())
	require.NoError(err)
	assert.Zero(status.Generation)
	var consumers int
	require.NoError(f.Store.DB().QueryRow(`SELECT count(*) FROM attachment_change_consumers`).Scan(&consumers))
	assert.Zero(consumers)
	ready := filenameReady(t, f.Store)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = f.Store.FilenameCandidatesContext(ctx, q, store.FilenameSuggestionScope{})
	require.ErrorIs(err, context.Canceled)
	_, err = f.Store.DB().Exec(`DELETE FROM attachment_change_consumers WHERE consumer_key='filename-discovery/v1'`)
	require.NoError(err)
	_, err = f.Store.FilenameCandidatesContext(t.Context(), q, store.FilenameSuggestionScope{})
	require.ErrorIs(err, store.ErrFilenameIndexUnavailable)
	unchanged, err := f.Store.FilenameDiscoveryStatusContext(t.Context())
	require.NoError(err)
	assert.Equal(ready, unchanged, "requests never repair missing maintenance boundaries")
}

func TestFilenameCandidatesFullExactBeforeDensePrefix(t *testing.T) {
	assert := assert.New(t)

	names := make([]string, 601)
	for i := range 600 {
		names[i] = "reportmany.pdf"
	}
	names[600] = "report.pdf"
	f, ids := filenameCandidateFixture(t, names)
	set := filenameCandidates(t, f.Store, "report.pdf")
	require.Len(t, set.IDs, 500)
	assert.Equal(ids[600], set.IDs[0])
	assert.True(set.CandidateLimitReached)
	empty := filenameCandidates(t, f.Store, "zzz")
	assert.Empty(empty.IDs)
	assert.Zero(empty.PostingRows)
	assert.False(empty.CandidateLimitReached)
}

// The strict upper bound excludes the next hex prefix while retaining the
// exact seed and longer Unicode tokens within the range.
func TestFilenameCandidatesPrefixEndpoints(t *testing.T) {
	f, ids := filenameCandidateFixture(t, []string{"ab.pdf", "aba.pdf", "abb.pdf", "abc.pdf", "abd.pdf", "ac.pdf", "a.pdf", "ab界.pdf"})
	set := filenameCandidates(t, f.Store, "ab")
	assert.ElementsMatch(t, []int64{ids[0], ids[1], ids[2], ids[3], ids[4], ids[7]}, set.IDs)
	assert.Equal(t, 6, set.PostingRows)
	assert.False(t, set.CandidateLimitReached)
}

// Hidden messages never occupy the candidate pool, though their postings
// still consume physical work. Exhausted streams prove the result complete.
func TestFilenameCandidatesSkipHiddenMessages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	names := make([]string, 600)
	for i := range names {
		names[i] = "rep.pdf"
	}
	f, ids := filenameCandidateFixture(t, names)
	_, err := f.Store.DB().Exec(`UPDATE messages SET deleted_at=CURRENT_TIMESTAMP`)
	require.NoError(err)
	set := filenameCandidates(t, f.Store, "rep.pdf")
	assert.Empty(set.IDs)
	assert.Equal(1200, set.PostingRows, "full-name and seed-exact postings are charged")
	assert.False(set.CandidateLimitReached)
	visible := f.CreateMessage("visible")
	var id int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`INSERT INTO attachments(message_id,filename,storage_path) VALUES(?,'rep.pdf','') RETURNING id`), visible).Scan(&id))
	// Hiding 600 messages journaled 600 events ahead of the insert.
	drained := false
	for range 10 {
		batch, err := f.Store.FilenameDiscoveryBatchContext(t.Context())
		require.NoError(err)
		if drained = !batch.More; drained {
			break
		}
	}
	require.True(drained)
	assert.Equal([]int64{id}, filenameCandidates(t, f.Store, "rep.pdf").IDs)
	assert.NotContains(ids, id)
}

// A person's older file stays reachable behind more than a candidate pool of
// newer matches from other people, because scope filters apply at admission.
func TestFilenameCandidatesScopedAdmission(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f, _, members := filenamePersonFixture(t, 1)
	var mine int64
	tx, err := f.Store.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	var message int64
	require.NoError(tx.QueryRow(f.Store.Rebind(`INSERT INTO messages(source_id,conversation_id,source_message_id,message_type) VALUES(?,?,'mine','email') RETURNING id`), f.Source.ID, f.ConvID).Scan(&message))
	_, err = tx.Exec(f.Store.Rebind(`INSERT INTO message_recipients(message_id,participant_id,recipient_type,email_address) VALUES(?,?,'to','member@example.test')`), message, members[0])
	require.NoError(err)
	require.NoError(tx.QueryRow(f.Store.Rebind(`INSERT INTO attachments(message_id,filename,storage_path) VALUES(?,'report final.pdf','') RETURNING id`), message).Scan(&mine))
	require.NoError(tx.Commit())
	other := f.CreateMessage("other")
	tx, err = f.Store.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	for i := range 600 {
		_, err := tx.Exec(f.Store.Rebind(`INSERT INTO attachments(message_id,filename,storage_path) VALUES(?,?,'')`), other, fmt.Sprintf("report %d.pdf", i))
		require.NoError(err)
	}
	require.NoError(tx.Commit())
	filenameReady(t, f.Store)
	q, err := filenamediscovery.ParseQuery("report")
	require.NoError(err)
	unscoped, err := f.Store.FilenameCandidatesContext(t.Context(), q, store.FilenameSuggestionScope{})
	require.NoError(err)
	assert.True(unscoped.CandidateLimitReached)
	assert.NotContains(unscoped.IDs, mine, "the unscoped pool holds the 500 newest matches")
	scope := personscope.Scope{ParticipantIDs: members, Directions: []personscope.Direction{personscope.ToPerson}}
	scoped, err := f.Store.FilenameCandidatesContext(t.Context(), q, store.FilenameSuggestionScope{Person: &scope})
	require.NoError(err)
	assert.Equal([]int64{mine}, scoped.IDs)
	assert.False(scoped.CandidateLimitReached)
	assert.Equal(4207, scoped.PostingRows, "601 seed-exact rows plus six deletion-key streams over the same 601 tokens")
}

func TestFilenameCandidatesInvalidBeforeSQL(t *testing.T) {
	f := storetest.New(t)
	require.NoError(t, f.Store.DB().Close())
	for _, query := range []filenamediscovery.Query{
		{}, {Normalized: strings.Repeat("a", 129), Tokens: []string{strings.Repeat("a", 129)}},
		{Normalized: "report.pdf", Tokens: []string{"spoof"}},
		{Normalized: "a a a a a a a a a", Tokens: strings.Fields("a a a a a a a a a")},
	} {
		_, err := f.Store.FilenameCandidatesContext(t.Context(), query, store.FilenameSuggestionScope{})
		assert.ErrorIs(t, err, filenamediscovery.ErrInvalidQuery)
	}
}
