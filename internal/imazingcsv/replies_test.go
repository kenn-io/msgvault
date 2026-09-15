package imazingcsv

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestRepliesLinkUniqueEarlierMessageAndClearLaterAmbiguity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	parent := []string{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Incoming", "+15550000002", "Alice", "", "", "", "original body", "", ""}
	child := []string{"Alice", "2024-06-01 12:01:00", "", "", "iMessage", "Outgoing", "", "", "", "Replying to:\nAlice (+15550000002)\n2024-06-01 12:00:00\noriginal body", "", "my reply", "", ""}
	exportDir := newTestExport(t, [][]string{parent, child})
	importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"})

	summary, err := importer.ImportPath(context.Background(), exportDir)
	require.NoError(err)
	assert.Equal(1, summary.RepliesLinked)
	assert.Zero(summary.RepliesUnresolved)
	parentID := messageIDByBody(t, st, "original body")
	childID := messageIDByBody(t, st, "my reply")
	var linkedParent sql.NullInt64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT reply_to_message_id FROM messages WHERE id = ?`), childID).Scan(&linkedParent))
	assert.Equal(sql.NullInt64{Int64: parentID, Valid: true}, linkedParent)

	writeTestCSV(t, filepath.Join(exportDir, "csv", "messages.csv"), [][]string{parent, child, parent})
	summary, err = importer.ImportPath(context.Background(), exportDir)
	require.NoError(err)
	assert.Zero(summary.RepliesLinked)
	assert.Equal(1, summary.RepliesUnresolved)
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT reply_to_message_id FROM messages WHERE id = ?`), childID).Scan(&linkedParent))
	assert.False(linkedParent.Valid)
}

func TestRepliesContractedExportPreservesLinkToRetainedArchivedParent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	parent := []string{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Incoming", "+15550000002", "Alice", "", "", "", "original body", "", ""}
	child := []string{"Alice", "2024-06-01 12:01:00", "", "", "iMessage", "Outgoing", "", "", "", "Replying to:\nAlice (+15550000002)\n2024-06-01 12:00:00\noriginal body", "", "my reply", "", ""}
	exportDir := newTestExport(t, [][]string{parent, child})
	importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"})

	first, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	require.Equal(1, first.RepliesLinked)
	parentID := messageIDByBody(t, st, "original body")
	childID := messageIDByBody(t, st, "my reply")

	writeTestCSV(t, filepath.Join(exportDir, "csv", "messages.csv"), [][]string{child})
	contracted, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	assert.Equal(1, contracted.RepliesLinked)
	assert.Zero(contracted.RepliesUnresolved)
	var linkedParent sql.NullInt64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT reply_to_message_id FROM messages WHERE id = ?`), childID).Scan(&linkedParent))
	assert.Equal(sql.NullInt64{Int64: parentID, Valid: true}, linkedParent)
}

func TestRepliesContractedExportDoesNotScanUnrelatedArchivedConversation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	parent := []string{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Incoming", "+15550000002", "Alice", "", "", "", "original body", "", ""}
	child := []string{"Alice", "2024-06-01 12:01:00", "", "", "iMessage", "Outgoing", "", "", "", "Replying to:\nAlice (+15550000002)\n2024-06-01 12:00:00\noriginal body", "", "my reply", "", ""}
	unrelated := []string{"Bob", "2024-06-01 11:00:00", "", "", "SMS", "Incoming", "+15550000003", "Bob", "", "", "", "unrelated body", "", ""}
	exportDir := newTestExport(t, [][]string{parent, child, unrelated})
	importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"})

	_, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	unrelatedID := messageIDByBody(t, st, "unrelated body")
	_, err = st.DB().Exec(st.Rebind(
		`UPDATE message_raw SET raw_data = ?, compression = NULL WHERE message_id = ?`),
		[]byte("not json"), unrelatedID)
	require.NoError(err)

	writeTestCSV(t, filepath.Join(exportDir, "csv", "messages.csv"), [][]string{child})
	contracted, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	assert.Equal(1, contracted.RepliesLinked)
	assert.Zero(contracted.RepliesUnresolved)
}

func TestRepliesContractedExportClearsLinkWhenEvidenceChanges(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	parent := []string{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Incoming", "+15550000002", "Alice", "", "", "", "original body", "", ""}
	child := []string{"Alice", "2024-06-01 12:01:00", "", "", "iMessage", "Outgoing", "", "", "", "Replying to:\nAlice (+15550000002)\n2024-06-01 12:00:00\noriginal body", "", "my reply", "", ""}
	exportDir := newTestExport(t, [][]string{parent, child})
	importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"})

	_, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	childID := messageIDByBody(t, st, "my reply")

	child[9] = "Replying to:\nBob (+15550000003)\n2024-06-01 11:00:00\nanother body"
	writeTestCSV(t, filepath.Join(exportDir, "csv", "messages.csv"), [][]string{child})
	contracted, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	assert.Zero(contracted.RepliesLinked)
	assert.Equal(1, contracted.RepliesUnresolved)
	var linkedParent sql.NullInt64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT reply_to_message_id FROM messages WHERE id = ?`), childID).Scan(&linkedParent))
	assert.False(linkedParent.Valid)
}

func TestRepliesContractedExportKeepsRetainedParentAmbiguity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	parent := []string{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Incoming", "+15550000002", "Alice", "", "", "", "same body", "", ""}
	child := []string{"Alice", "2024-06-01 12:01:00", "", "", "iMessage", "Outgoing", "", "", "", "Replying to:\nAlice (+15550000002)\n2024-06-01 12:00:00\nsame body", "", "my reply", "", ""}
	exportDir := newTestExport(t, [][]string{parent, parent, child})
	importer := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"})

	first, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	require.Equal(1, first.RepliesUnresolved)
	childID := messageIDByBody(t, st, "my reply")

	writeTestCSV(t, filepath.Join(exportDir, "csv", "messages.csv"), [][]string{parent, child})
	contracted, err := importer.ImportPath(t.Context(), exportDir)
	require.NoError(err)
	assert.Zero(contracted.RepliesLinked)
	assert.Equal(1, contracted.RepliesUnresolved,
		"the omitted indistinguishable parent must still make the reply ambiguous")
	var linkedParent sql.NullInt64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT reply_to_message_id FROM messages WHERE id = ?`), childID).Scan(&linkedParent))
	assert.False(linkedParent.Valid)
}

func TestRepliesAmbiguousSameDateParentsStayUnresolved(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	// Both parents share one rendered date, sender, and conversation, and the
	// child's evidence quotes both bodies, so exactly one parent cannot be
	// identified.
	firstParent := []string{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Incoming", "+15550000002", "Alice", "", "", "", "first body", "", ""}
	secondParent := []string{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Incoming", "+15550000002", "Alice", "", "", "", "second body", "", ""}
	child := []string{"Alice", "2024-06-01 12:01:00", "", "", "iMessage", "Outgoing", "", "", "", "Replying to:\nAlice (+15550000002)\n2024-06-01 12:00:00\nfirst body\nsecond body", "", "my reply", "", ""}
	exportDir := newTestExport(t, [][]string{firstParent, secondParent, child})

	summary, err := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"}).
		ImportPath(context.Background(), exportDir)
	require.NoError(err)
	assert.Zero(summary.RepliesLinked)
	assert.Equal(1, summary.RepliesUnresolved)
	childID := messageIDByBody(t, st, "my reply")
	var linkedParent sql.NullInt64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT reply_to_message_id FROM messages WHERE id = ?`), childID).Scan(&linkedParent))
	assert.False(linkedParent.Valid)
}

func TestRepliesLinkSparseCandidatesInLargeConversation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	// One large conversation where every message has a distinct rendered
	// date and only a sparse subset carries reply evidence, each quoting one
	// earlier message far back in the history.
	const total = 1200
	const step = 100
	rows := make([][]string, 0, total)
	type expectedLink struct {
		childBody  string
		parentBody string
	}
	var expected []expectedLink
	for index := range total {
		minute := index / 60
		second := index % 60
		date := fmt.Sprintf("2024-06-01 %02d:%02d:00", minute, second)
		body := fmt.Sprintf("message %04d", index)
		if index >= step && index%step == 0 {
			// Reference an incoming row so the quoted sender is always
			// Alice, never the owner: shift off any earlier child row.
			parentIndex := index - step
			if parentIndex >= step {
				parentIndex--
			}
			parentDate := fmt.Sprintf("2024-06-01 %02d:%02d:00", parentIndex/60, parentIndex%60)
			parentBody := fmt.Sprintf("message %04d", parentIndex)
			rows = append(rows, []string{"Alice", date, "", "", "iMessage", "Outgoing", "", "", "",
				"Replying to:\nAlice (+15550000002)\n" + parentDate + "\n" + parentBody,
				"", body, "", ""})
			expected = append(expected, expectedLink{childBody: body, parentBody: parentBody})
			continue
		}
		rows = append(rows, []string{"Alice", date, "", "", "iMessage", "Incoming", "+15550000002", "Alice", "", "", "", body, "", ""})
	}
	exportDir := newTestExport(t, rows)

	summary, err := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"}).
		ImportPath(context.Background(), exportDir)
	require.NoError(err)
	assert.Equal(len(expected), summary.RepliesLinked)
	assert.Zero(summary.RepliesUnresolved)
	for _, link := range expected {
		childID := messageIDByBody(t, st, link.childBody)
		parentID := messageIDByBody(t, st, link.parentBody)
		var linkedParent sql.NullInt64
		require.NoError(st.DB().QueryRow(st.Rebind(
			`SELECT reply_to_message_id FROM messages WHERE id = ?`), childID).Scan(&linkedParent))
		require.True(linkedParent.Valid)
		assert.Equal(parentID, linkedParent.Int64)
	}
}

func TestReplyEvidenceRequiresDistinctExactBodySenderAndDate(t *testing.T) {
	parent := &plannedMessage{
		row: Row{
			MessageDate: "2024-06-01 12:00:00",
			SenderID:    "+15550000002",
			SenderName:  "Alice",
			Text:        "2024",
		},
		sender: participantIdentity{Kind: identityPhone, Value: "+15550000002", DisplayName: "Alice"},
	}

	tests := []struct {
		name     string
		rendered string
		want     bool
	}{
		{
			name:     "all distinct",
			rendered: "Alice\n2024-06-01 12:00:00\n2024",
			want:     true,
		},
		{
			name:     "body only occurs inside date",
			rendered: "Alice\n2024-06-01 12:00:00",
			want:     false,
		},
		{
			name:     "missing sender",
			rendered: "2024-06-01 12:00:00\n2024",
			want:     false,
		},
		{
			name:     "partial body",
			rendered: "Alice\n2024-06-01 12:00:00\n20",
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, replyEvidenceMatches(tt.rendered, parent))
		})
	}
}

func TestEvidenceNeedleMatcherFindsNestedAndSuffixNeedles(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	matcher := newEvidenceNeedleMatcher([]string{
		"2024-06-01 12:00:00", "12:00:00", "Alice", "lice", "body",
	})
	matches := matcher.match("Alice (+1)\n2024-06-01 12:00:00\nbody")
	require.Len(matches.matched, 5)
	for index, want := range []bool{true, true, true, true, true} {
		assert.Equal(want, matches.matched[index], "needle %d", index)
	}
	// Every matched needle is reported exactly once, so callers can use the
	// ID list directly without deduplicating.
	assert.Len(matches.matchedIDs, 5)
	assert.ElementsMatch([]int{0, 1, 2, 3, 4}, matches.matchedIDs)
	none := matcher.match("nothing relevant here")
	assert.Empty(none.matchedIDs)
	for index, got := range none.matched {
		assert.False(got, "needle %d must not match", index)
	}
	// A needle that only appears as the suffix of another needle still
	// reports both, and an empty needle set matches nothing without failing.
	assert.True(matcher.match("12:00:00").matched[1])
	assert.ElementsMatch([]int{1}, matcher.match("12:00:00").matchedIDs)
	empty := newEvidenceNeedleMatcher(nil).match("any text")
	assert.Empty(empty.matched)
	assert.Empty(empty.matchedIDs)
}

func TestReplyCandidateIndexRestrictsToEarlierMatchingParents(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	earlier := &plannedMessage{
		row: Row{MessageDate: "2024-06-01 12:00:00", SenderID: "+15550000002",
			SenderName: "Alice", Text: "original body",
			SentAt: time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)},
		sender: participantIdentity{Kind: identityPhone, Value: "+15550000002", DisplayName: "Alice"},
	}
	sameDate := &plannedMessage{
		row: Row{MessageDate: "2024-06-01 12:00:00", SenderID: "+15550000002",
			SenderName: "Alice", Text: "other body",
			SentAt: time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)},
		sender: participantIdentity{Kind: identityPhone, Value: "+15550000002", DisplayName: "Alice"},
	}
	later := &plannedMessage{
		row: Row{MessageDate: "2024-06-01 12:05:00", SenderID: "+15550000002",
			SenderName: "Alice", Text: "later body",
			SentAt: time.Date(2024, 6, 1, 12, 5, 0, 0, time.UTC)},
		sender: participantIdentity{Kind: identityPhone, Value: "+15550000002", DisplayName: "Alice"},
	}
	index := newReplyCandidateIndex([]*plannedMessage{earlier, sameDate, later})
	child := &plannedMessage{
		row: Row{MessageDate: "2024-06-01 12:01:00", SentAt: time.Date(2024, 6, 1, 12, 1, 0, 0, time.UTC),
			ReplyingTo: "Replying to:\nAlice (+15550000002)\n2024-06-01 12:00:00\noriginal body"},
	}

	// Both same-date parents are candidates regardless of order; the later
	// message is excluded because it is not earlier than the child.
	candidates := index.candidates(child)
	require.Len(candidates, 2)
	assert.Contains(candidates, earlier)
	assert.Contains(candidates, sameDate)
	assert.NotContains(candidates, later)

	// Evidence without the parent's sender or date yields no candidates.
	childWithoutEvidence := &plannedMessage{
		row: Row{MessageDate: "2024-06-01 12:01:00", SentAt: time.Date(2024, 6, 1, 12, 1, 0, 0, time.UTC),
			ReplyingTo: "Someone else\n2024-06-01 11:00:00\nunrelated"},
	}
	assert.Empty(index.candidates(childWithoutEvidence))
}

// TestReplyCandidateIndexEnumeratesOnlyMatchedDateBuckets shows that
// candidate enumeration is driven by the matched needle IDs, not by the
// number of distinct dates in the conversation: with one date bucket per
// message, a child quoting a single parent only ever touches that parent's
// bucket.
func TestReplyCandidateIndexEnumeratesOnlyMatchedDateBuckets(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	const total = 40
	const quoted = 10
	const quotedDate = "2024-06-01 00:10:00"
	plans := make([]*plannedMessage, 0, total)
	for index := range total {
		plans = append(plans, &plannedMessage{
			row: Row{
				MessageDate: fmt.Sprintf("2024-06-01 00:%02d:00", index),
				SenderID:    "+15550000002",
				SenderName:  "Alice",
				Text:        fmt.Sprintf("body %02d", index),
				SentAt:      time.Date(2024, 6, 1, 0, index, 0, 0, time.UTC),
			},
			sender: participantIdentity{Kind: identityPhone, Value: "+15550000002", DisplayName: "Alice"},
		})
	}
	index := newReplyCandidateIndex(plans)
	// Every message renders a distinct date, so the index holds one non-empty
	// date bucket per conversation message.
	buckets := 0
	for _, parents := range index.dateParents {
		if parents != nil {
			buckets++
		}
	}
	require.Equal(total, buckets)

	child := &plannedMessage{
		row: Row{
			MessageDate: "2024-06-01 00:39:30",
			SentAt:      time.Date(2024, 6, 1, 0, 39, 30, 0, time.UTC),
			ReplyingTo:  "Replying to:\nAlice (+15550000002)\n" + quotedDate + "\nbody 10",
		},
	}
	matches := index.matcher.match(normalizeRenderedEvidence(child.row.ReplyingTo))
	// Only the quoted date and the two sender spellings hit, out of every
	// date and sender needle the index knows.
	require.Len(matches.matchedIDs, 3)
	var matchedBuckets [][]indexedParent
	for _, id := range matches.matchedIDs {
		if id < len(index.dateParents) && index.dateParents[id] != nil {
			matchedBuckets = append(matchedBuckets, index.dateParents[id])
		}
	}
	// Exactly one date bucket is reachable from the matched IDs, and it
	// holds only the quoted parent.
	require.Len(matchedBuckets, 1)
	require.Len(matchedBuckets[0], 1)
	assert.Equal(plans[quoted], matchedBuckets[0][0].plan)
	assert.Equal([]*plannedMessage{plans[quoted]}, index.candidates(child))

	// Evidence quoting none of the indexed dates matches no needle with a
	// date bucket, so no bucket is visited at all.
	elsewhere := &plannedMessage{
		row: Row{
			MessageDate: "2024-06-01 00:40:00",
			SentAt:      time.Date(2024, 6, 1, 0, 40, 0, 0, time.UTC),
			ReplyingTo:  "Replying to:\nAlice (+15550000002)\n2024-05-31 23:59:00\nbody 10",
		},
	}
	elsewhereMatches := index.matcher.match(normalizeRenderedEvidence(elsewhere.row.ReplyingTo))
	for _, id := range elsewhereMatches.matchedIDs {
		assert.True(id >= len(index.dateParents) || index.dateParents[id] == nil,
			"needle %d must not own a date bucket", id)
	}
	assert.Empty(index.candidates(elsewhere))
}

func messageIDByBody(t *testing.T, st *store.Store, body string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT message_id FROM message_bodies WHERE body_text = ?`), body).Scan(&id))
	return id
}
