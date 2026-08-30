package store_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func newRFC822Message(
	t *testing.T, f *storetest.Fixture, sourceMessageID, rfc822ID string,
) int64 {
	t.Helper()
	id, err := f.Store.UpsertMessage(&store.Message{
		ConversationID:  f.ConvID,
		SourceID:        f.Source.ID,
		SourceMessageID: sourceMessageID,
		RFC822MessageID: sql.NullString{
			String: rfc822ID, Valid: rfc822ID != "",
		},
		MessageType:  "email",
		SizeEstimate: 1000,
	})
	require.NoError(t, err, "UpsertMessage")
	return id
}

func TestStore_FindDuplicatesByRFC822ID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	idA := newRFC822Message(t, f, "src-a", "rfc822-shared")
	idB := newRFC822Message(t, f, "src-b", "rfc822-shared")
	_ = newRFC822Message(t, f, "src-c", "rfc822-unique")

	groups, err := f.Store.FindDuplicatesByRFC822ID()
	require.NoError(err, "FindDuplicatesByRFC822ID")
	require.Len(groups, 1)
	assert.Equal("rfc822-shared", groups[0].RFC822MessageID, "key")
	assert.Equal(2, groups[0].Count, "count")

	_, err = f.Store.MergeDuplicates(idA, []int64{idB}, "batch-test")
	require.NoError(err, "MergeDuplicates")

	groups, err = f.Store.FindDuplicatesByRFC822ID()
	require.NoError(err, "FindDuplicatesByRFC822ID after merge")
	assert.Empty(groups, "groups after merge")
}

func TestStore_DuplicateGroupsCanonicalizeBracketedRFC822IDs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	bracketedID := newRFC822Message(t, f, "sync-copy", "<mixed@example.test>")
	bareID := newRFC822Message(t, f, "backfilled-copy", "mixed@example.test")

	groups, err := f.Store.FindDuplicatesByRFC822ID()
	require.NoError(err)
	require.Len(groups, 1)
	assert.Equal("mixed@example.test", groups[0].RFC822MessageID)
	assert.Equal(2, groups[0].Count)

	single, err := f.Store.GetDuplicateGroupMessages("mixed@example.test")
	require.NoError(err)
	require.Len(single, 2)
	assert.ElementsMatch([]int64{bracketedID, bareID}, []int64{single[0].ID, single[1].ID})

	batched, err := f.Store.GetDuplicateGroupMessagesBatch([]string{"mixed@example.test"})
	require.NoError(err)
	require.Contains(batched, "mixed@example.test")
	require.Len(batched["mixed@example.test"], 2)
	assert.ElementsMatch(
		[]int64{bracketedID, bareID},
		[]int64{batched["mixed@example.test"][0].ID, batched["mixed@example.test"][1].ID},
	)
}

func TestStore_DuplicateGroupsPreserveMalformedRFC822IDs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	const malformedID = "<<malformed@example.test>>"
	firstID := newRFC822Message(t, f, "malformed-first", malformedID)
	secondID := newRFC822Message(t, f, "malformed-second", malformedID)

	groups, err := f.Store.FindDuplicatesByRFC822ID()
	require.NoError(err)
	require.Len(groups, 1)
	assert.Equal(malformedID, groups[0].RFC822MessageID)
	assert.Equal(2, groups[0].Count)

	single, err := f.Store.GetDuplicateGroupMessages(malformedID)
	require.NoError(err)
	require.Len(single, 2)
	assert.ElementsMatch([]int64{firstID, secondID}, []int64{single[0].ID, single[1].ID})

	batched, err := f.Store.GetDuplicateGroupMessagesBatch([]string{malformedID})
	require.NoError(err)
	require.Contains(batched, malformedID)
	require.Len(batched[malformedID], 2)
	assert.ElementsMatch(
		[]int64{firstID, secondID},
		[]int64{batched[malformedID][0].ID, batched[malformedID][1].ID},
	)
}

func TestStore_DuplicateGroupFetchMatchesWhitespaceDiscoveryGroups(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	const (
		bracketedID = "< whitespace@example.test>"
		bareID      = " whitespace@example.test"
	)
	bracketedRows := []int64{
		newRFC822Message(t, f, "bracketed-first", bracketedID),
		newRFC822Message(t, f, "bracketed-second", bracketedID),
	}
	bareRows := []int64{
		newRFC822Message(t, f, "bare-first", bareID),
		newRFC822Message(t, f, "bare-second", bareID),
	}

	groups, err := f.Store.FindDuplicatesByRFC822ID()
	require.NoError(err)
	require.Len(groups, 2)
	groupCounts := make(map[string]int, len(groups))
	for _, group := range groups {
		groupCounts[group.RFC822MessageID] = group.Count
	}
	assert.Equal(map[string]int{bracketedID: 2, bareID: 2}, groupCounts)

	for groupID, wantIDs := range map[string][]int64{
		bracketedID: bracketedRows,
		bareID:      bareRows,
	} {
		single, err := f.Store.GetDuplicateGroupMessages(groupID)
		require.NoError(err)
		gotIDs := make([]int64, len(single))
		for i, row := range single {
			gotIDs[i] = row.ID
		}
		assert.ElementsMatch(wantIDs, gotIDs, "single fetch for %q", groupID)
	}

	batched, err := f.Store.GetDuplicateGroupMessagesBatch([]string{bracketedID, bareID})
	require.NoError(err)
	require.Len(batched, 2)
	for groupID, wantIDs := range map[string][]int64{
		bracketedID: bracketedRows,
		bareID:      bareRows,
	} {
		rows := batched[groupID]
		gotIDs := make([]int64, len(rows))
		for i, row := range rows {
			gotIDs[i] = row.ID
		}
		assert.ElementsMatch(wantIDs, gotIDs, "batch fetch for %q", groupID)
	}
}

func TestStore_DuplicateGroupFetchPreservesInvalidUTF8(t *testing.T) {
	testutil.SkipIfPostgres(t, "PostgreSQL TEXT rejects invalid UTF-8 before duplicate grouping")
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	invalidID := "invalid-\xff@example.test"
	bareID := newRFC822Message(t, f, "invalid-bare", invalidID)
	bracketedID := newRFC822Message(t, f, "invalid-bracketed", "<"+invalidID+">")

	groups, err := f.Store.FindDuplicatesByRFC822ID()
	require.NoError(err)
	require.Len(groups, 1)
	assert.Equal(invalidID, groups[0].RFC822MessageID)
	assert.Equal(2, groups[0].Count)

	single, err := f.Store.GetDuplicateGroupMessages(invalidID)
	require.NoError(err)
	require.Len(single, 2)
	assert.ElementsMatch([]int64{bareID, bracketedID}, []int64{single[0].ID, single[1].ID})

	batched, err := f.Store.GetDuplicateGroupMessagesBatch([]string{invalidID})
	require.NoError(err)
	require.Len(batched[invalidID], 2)
	assert.ElementsMatch(
		[]int64{bareID, bracketedID},
		[]int64{batched[invalidID][0].ID, batched[invalidID][1].ID},
	)
}

func TestStore_DuplicateGroupFetchPreservesEmptyBrackets(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	firstID := newRFC822Message(t, f, "empty-brackets-first", "<>")
	secondID := newRFC822Message(t, f, "empty-brackets-second", "<>")

	groups, err := f.Store.FindDuplicatesByRFC822ID()
	require.NoError(err)
	require.Len(groups, 1)
	assert.Equal("<>", groups[0].RFC822MessageID)
	assert.Equal(2, groups[0].Count)

	single, err := f.Store.GetDuplicateGroupMessages("<>")
	require.NoError(err)
	require.Len(single, 2)
	assert.ElementsMatch([]int64{firstID, secondID}, []int64{single[0].ID, single[1].ID})

	batched, err := f.Store.GetDuplicateGroupMessagesBatch([]string{"<>"})
	require.NoError(err)
	require.Len(batched["<>"], 2)
	assert.ElementsMatch(
		[]int64{firstID, secondID},
		[]int64{batched["<>"][0].ID, batched["<>"][1].ID},
	)
}

func TestStore_DuplicateGroupFetchPreservesNonASCIIAndControlEdges(t *testing.T) {
	tests := []struct {
		name   string
		suffix string
	}{
		{name: "tab", suffix: "\t"},
		{name: "newline", suffix: "\n"},
		{name: "non-breaking space", suffix: "\u00a0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := storetest.New(t)
			groupID := "edge@example.test" + tt.suffix
			bareID := newRFC822Message(t, f, tt.name+"-bare", groupID)
			bracketedID := newRFC822Message(t, f, tt.name+"-bracketed", "<"+groupID+">")

			groups, err := f.Store.FindDuplicatesByRFC822ID()
			require.NoError(err)
			require.Len(groups, 1)
			assert.Equal(groupID, groups[0].RFC822MessageID)
			assert.Equal(2, groups[0].Count)

			single, err := f.Store.GetDuplicateGroupMessages(groupID)
			require.NoError(err)
			require.Len(single, 2)
			assert.ElementsMatch(
				[]int64{bareID, bracketedID},
				[]int64{single[0].ID, single[1].ID},
			)

			batched, err := f.Store.GetDuplicateGroupMessagesBatch([]string{groupID})
			require.NoError(err)
			require.Len(batched[groupID], 2)
			assert.ElementsMatch(
				[]int64{bareID, bracketedID},
				[]int64{batched[groupID][0].ID, batched[groupID][1].ID},
			)
		})
	}
}

func TestStore_DuplicateGroupFetchPreservesEmbeddedNUL(t *testing.T) {
	testutil.SkipIfPostgres(t, "PostgreSQL TEXT rejects embedded NUL bytes")
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	groupID := "nul\x00id@example.test"
	bareID := newRFC822Message(t, f, "nul-bare", groupID)
	bracketedID := newRFC822Message(t, f, "nul-bracketed", "<"+groupID+">")

	groups, err := f.Store.FindDuplicatesByRFC822ID()
	require.NoError(err)
	require.Len(groups, 1)
	assert.Equal(groupID, groups[0].RFC822MessageID)
	assert.Equal(2, groups[0].Count)

	single, err := f.Store.GetDuplicateGroupMessages(groupID)
	require.NoError(err)
	require.Len(single, 2)
	assert.ElementsMatch([]int64{bareID, bracketedID}, []int64{single[0].ID, single[1].ID})

	batched, err := f.Store.GetDuplicateGroupMessagesBatch([]string{groupID})
	require.NoError(err)
	require.Len(batched[groupID], 2)
	assert.ElementsMatch(
		[]int64{bareID, bracketedID},
		[]int64{batched[groupID][0].ID, batched[groupID][1].ID},
	)
}

func TestStore_DuplicateDiscoveryDoesNotTruncateAtEmbeddedNUL(t *testing.T) {
	testutil.SkipIfPostgres(t, "PostgreSQL TEXT rejects embedded NUL bytes")
	f := storetest.New(t)
	newRFC822Message(t, f, "nul-tail-c", "<ab>\x00c>")
	newRFC822Message(t, f, "nul-tail-d", "<ab>\x00d>")

	groups, err := f.Store.FindDuplicatesByRFC822ID()
	require.NoError(t, err)
	assert.Empty(t, groups)
}

func TestStore_DuplicateGroupsPreserveNULTailAsOwnKey(t *testing.T) {
	testutil.SkipIfPostgres(t, "PostgreSQL TEXT rejects embedded NUL bytes")
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	const cleanID = "tail@example.test"
	nulTailID := "<" + cleanID + ">\x00"
	cleanRows := []int64{
		newRFC822Message(t, f, "clean-bare", cleanID),
		newRFC822Message(t, f, "clean-bracketed", "<"+cleanID+">"),
	}
	nulRows := []int64{
		newRFC822Message(t, f, "nul-tail-first", nulTailID),
		newRFC822Message(t, f, "nul-tail-second", nulTailID),
	}

	groups, err := f.Store.FindDuplicatesByRFC822ID()
	require.NoError(err)
	require.Len(groups, 2)
	counts := make(map[string]int, len(groups))
	for _, group := range groups {
		counts[group.RFC822MessageID] = group.Count
	}
	assert.Equal(map[string]int{cleanID: 2, nulTailID: 2}, counts)

	batched, err := f.Store.GetDuplicateGroupMessagesBatch([]string{cleanID, nulTailID})
	require.NoError(err)
	require.Len(batched, 2)
	for groupID, wantIDs := range map[string][]int64{cleanID: cleanRows, nulTailID: nulRows} {
		rows := batched[groupID]
		gotIDs := make([]int64, len(rows))
		for i, row := range rows {
			gotIDs[i] = row.ID
		}
		assert.ElementsMatch(wantIDs, gotIDs, "batch fetch for %q", groupID)
	}
}

func TestStore_DuplicateGroupFetchPreservesMultibyteID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	const groupID = "héllo@exämple.test"
	bareID := newRFC822Message(t, f, "multibyte-bare", groupID)
	bracketedID := newRFC822Message(t, f, "multibyte-bracketed", "<"+groupID+">")

	groups, err := f.Store.FindDuplicatesByRFC822ID()
	require.NoError(err)
	require.Len(groups, 1)
	assert.Equal(groupID, groups[0].RFC822MessageID)
	assert.Equal(2, groups[0].Count)

	batched, err := f.Store.GetDuplicateGroupMessagesBatch([]string{groupID})
	require.NoError(err)
	require.Len(batched[groupID], 2)
	assert.ElementsMatch(
		[]int64{bareID, bracketedID},
		[]int64{batched[groupID][0].ID, batched[groupID][1].ID},
	)
}

func TestStore_GetDuplicateGroupMessages_SentLabel(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	idInbox := newRFC822Message(t, f, "inbox-copy", "rfc822-sent")
	idSent := newRFC822Message(t, f, "sent-copy", "rfc822-sent")

	labels := f.EnsureLabels(
		map[string]string{"SENT": "Sent", "INBOX": "Inbox"}, "system",
	)
	require.NoError(f.Store.LinkMessageLabel(idInbox, labels["INBOX"]), "link INBOX")
	require.NoError(f.Store.LinkMessageLabel(idSent, labels["SENT"]), "link SENT")

	rows, err := f.Store.GetDuplicateGroupMessages("rfc822-sent")
	require.NoError(err, "GetDuplicateGroupMessages")
	require.Len(rows, 2)

	var sentRow, inboxRow *store.DuplicateMessageRow
	for i := range rows {
		switch rows[i].ID {
		case idSent:
			sentRow = &rows[i]
		case idInbox:
			inboxRow = &rows[i]
		}
	}
	require.NotNil(sentRow, "sent row missing")
	require.NotNil(inboxRow, "inbox row missing")
	assert.True(sentRow.HasSentLabel, "sent row: HasSentLabel")
	assert.False(inboxRow.HasSentLabel, "inbox row: HasSentLabel")
}

func TestStore_MergeDuplicates_UnionsLabels(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	idKeep := newRFC822Message(t, f, "keep", "rfc822-merge")
	idDrop := newRFC822Message(t, f, "drop", "rfc822-merge")

	labels := f.EnsureLabels(
		map[string]string{"INBOX": "Inbox", "IMPORTANT": "Important", "WORK": "Work"}, "user",
	)
	require.NoError(f.Store.LinkMessageLabel(idKeep, labels["INBOX"]), "link INBOX to keep")
	require.NoError(f.Store.LinkMessageLabel(idDrop, labels["IMPORTANT"]), "link IMPORTANT to drop")
	require.NoError(f.Store.LinkMessageLabel(idDrop, labels["WORK"]), "link WORK to drop")

	result, err := f.Store.MergeDuplicates(idKeep, []int64{idDrop}, "batch-labels")
	require.NoError(err, "MergeDuplicates")
	assert.Equal(2, result.LabelsTransferred, "labelsTransferred")

	f.AssertLabelCount(idKeep, 3)
	assertDedupDeleted(t, f.Store, idDrop, true)

	restored, err := f.Store.UndoDedup("batch-labels")
	require.NoError(err, "UndoDedup")
	assert.Equal(int64(1), restored, "restored")
	assertDedupDeleted(t, f.Store, idDrop, false)
}

func assertDedupDeleted(
	t *testing.T, st *store.Store, msgID int64, wantDeleted bool,
) {
	t.Helper()
	var deletedAt sql.NullTime
	err := st.DB().QueryRow(
		st.Rebind("SELECT deleted_at FROM messages WHERE id = ?"), msgID,
	).Scan(&deletedAt)
	require.NoError(t, err, "query deleted_at")
	if wantDeleted {
		assert.True(t, deletedAt.Valid, "message %d: deleted_at should be set", msgID)
	} else {
		assert.False(t, deletedAt.Valid, "message %d: deleted_at should be NULL", msgID)
	}
}

func TestStore_BackfillRFC822IDs_EmptyTable(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	count, err := f.Store.CountMessagesWithoutRFC822ID()
	require.NoError(err, "CountMessagesWithoutRFC822ID")
	assert.Equal(int64(0), count, "empty-table count")

	updated, _, err := f.Store.BackfillRFC822IDs(nil, nil)
	require.NoError(err, "BackfillRFC822IDs")
	assert.Equal(int64(0), updated, "updated")
}

func TestStore_PlanRFC822IDBackfillDoesNotWriteAndSeparatesFailures(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	validID := newRFC822Message(t, f, "plan-valid", "")
	malformedID := newRFC822Message(t, f, "plan-malformed", "")
	missingHeaderID := newRFC822Message(t, f, "plan-no-message-id", "")
	noRawID := newRFC822Message(t, f, "plan-no-raw", "")

	require.NoError(f.Store.UpsertMessageRaw(validID, []byte(
		"From: alice@example.com\r\nMessage-ID: <planned@example.com>\r\n\r\nBody")))
	require.NoError(f.Store.UpsertMessageRaw(missingHeaderID, []byte(
		"From: bob@example.com\r\nSubject: no identifier\r\n\r\nBody")))
	_, err := f.Store.DB().Exec(f.Store.Rebind(
		`INSERT INTO message_raw (message_id, raw_data, raw_format, compression)
		 VALUES (?, ?, 'mime', 'zlib')`), malformedID, []byte("not-zlib"))
	require.NoError(err)

	plan, err := f.Store.PlanRFC822IDBackfill(t.Context(), []int64{f.Source.ID})
	require.NoError(err)
	assert.Equal(int64(3), plan.Candidates)
	require.Len(plan.Items, 1)
	assert.Equal(validID, plan.Items[0].MessageID)
	assert.Equal(f.Source.ID, plan.Items[0].SourceID)
	assert.Equal("planned@example.com", plan.Items[0].RFC822MessageID)
	assert.Equal(int64(2), plan.Failed)

	for _, messageID := range []int64{validID, malformedID, missingHeaderID, noRawID} {
		var stored sql.NullString
		require.NoError(f.Store.DB().QueryRow(
			f.Store.Rebind(`SELECT rfc822_message_id FROM messages WHERE id = ?`), messageID,
		).Scan(&stored))
		assert.False(stored.Valid, "message %d must remain unchanged", messageID)
	}
}

func TestStore_PlanRFC822IDBackfillSanitizesInvalidUTF8(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	messageID := newRFC822Message(t, f, "plan-invalid-utf8", "")
	require.NoError(f.Store.UpsertMessageRaw(messageID, []byte(
		"From: alice@example.com\r\n"+
			"Message-ID: <m-\xfd\xfe@example.com>\r\n"+
			"Content-Type: text/plain\r\n\r\nBody")))

	plan, err := f.Store.PlanRFC822IDBackfill(t.Context(), []int64{f.Source.ID})
	require.NoError(err)
	require.Len(plan.Items, 1)
	assert.True(utf8.ValidString(plan.Items[0].RFC822MessageID))
	assert.Equal("m-\uFFFD\uFFFD@example.com", plan.Items[0].RFC822MessageID)
	assert.Empty(storedRFC822ID(t, f.Store, messageID))
}

func TestStore_ApplyRFC822IDBackfillCommitsSanitizedInvalidUTF8ID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	cleanID := newRFC822Message(t, f, "apply-clean", "")
	dirtyID := newRFC822Message(t, f, "apply-invalid-utf8", "")
	require.NoError(f.Store.UpsertMessageRaw(cleanID, []byte(
		"Message-ID: <apply-clean@example.com>\r\n\r\nFirst")))
	require.NoError(f.Store.UpsertMessageRaw(dirtyID, []byte(
		"From: bob@example.com\r\n"+
			"Message-ID: <d-\x80@example.com>\r\n"+
			"Content-Type: text/plain\r\n\r\nSecond")))

	plan, err := f.Store.PlanRFC822IDBackfill(t.Context(), []int64{f.Source.ID})
	require.NoError(err)
	require.Len(plan.Items, 2)

	progressCalls := 0
	updated, err := f.Store.ApplyRFC822IDBackfill(
		t.Context(), []int64{f.Source.ID}, plan,
		func(_, _ int64) { progressCalls++ },
	)
	require.NoError(err)
	assert.Equal(int64(2), updated)
	assert.Equal(1, progressCalls)
	assert.Equal("apply-clean@example.com", storedRFC822ID(t, f.Store, cleanID))
	storedDirtyID := storedRFC822ID(t, f.Store, dirtyID)
	assert.True(utf8.ValidString(storedDirtyID))
	assert.Equal("d-\uFFFD@example.com", storedDirtyID)
}

func TestStore_PlanRFC822IDBackfillExcludesNonMIMEStoredRows(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	messageID := newRFC822Message(t, f, "plan-non-mime", "")
	require.NoError(f.Store.UpsertMessageRaw(messageID, []byte(
		"Message-ID: <must-not-derive@example.com>\r\n\r\nNon-MIME source payload")))
	_, err := f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE message_raw SET raw_format = ? WHERE message_id = ?`),
		"imessage_archive", messageID)
	require.NoError(err)

	plan, err := f.Store.PlanRFC822IDBackfill(t.Context(), []int64{f.Source.ID})
	require.NoError(err)
	assert.Equal(int64(0), plan.Candidates)
	assert.Empty(plan.Items)
	assert.Equal(int64(0), plan.Failed)
	assert.Empty(storedRFC822ID(t, f.Store, messageID))
}

func TestStore_RFC822IDBackfillPlanDigestBindsRowsAndRawInput(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	firstID := newRFC822Message(t, f, "digest-first", "")
	require.NoError(f.Store.UpsertMessageRaw(firstID, []byte(
		"Message-ID: <first@example.com>\r\n\r\nFirst body")))
	firstPlan, err := f.Store.PlanRFC822IDBackfill(t.Context(), nil)
	require.NoError(err)
	require.Len(firstPlan.Items, 1)

	require.NoError(f.Store.UpsertMessageRaw(firstID, []byte(
		"Message-ID: <replacement@example.com>\r\n\r\nReplacement body")))
	replacedRawPlan, err := f.Store.PlanRFC822IDBackfill(t.Context(), nil)
	require.NoError(err)
	require.Len(replacedRawPlan.Items, 1)
	assert.NotEqual(firstPlan.Digest(), replacedRawPlan.Digest())

	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET rfc822_message_id = ? WHERE id = ?`), "already-set@example.com", firstID)
	require.NoError(err)
	secondID := newRFC822Message(t, f, "digest-second", "")
	require.NoError(f.Store.UpsertMessageRaw(secondID, []byte(
		"Message-ID: <replacement@example.com>\r\n\r\nReplacement body")))
	replacedRowPlan, err := f.Store.PlanRFC822IDBackfill(t.Context(), nil)
	require.NoError(err)
	require.Len(replacedRowPlan.Items, 1)
	assert.Equal(replacedRawPlan.Candidates, replacedRowPlan.Candidates)
	assert.NotEqual(replacedRawPlan.Digest(), replacedRowPlan.Digest())
}

func TestStore_RFC822IDBackfillPlanDigestStableAcrossItemOrder(t *testing.T) {
	firstFingerprint := sha256.Sum256([]byte("first"))
	secondFingerprint := sha256.Sum256([]byte("second"))
	items := []store.RFC822IDBackfillItem{
		{MessageID: 20, SourceID: 2, RFC822MessageID: "second@example.com", RawInputSHA256: secondFingerprint},
		{MessageID: 10, SourceID: 1, RFC822MessageID: "first@example.com", RawInputSHA256: firstFingerprint},
	}
	forward := store.RFC822IDBackfillPlan{Candidates: 2, Items: items, Failed: 0}
	reversed := store.RFC822IDBackfillPlan{Candidates: 2, Items: []store.RFC822IDBackfillItem{items[1], items[0]}, Failed: 0}

	assert.Equal(t, forward.Digest(), reversed.Digest())
}

func TestStore_RFC822IDBackfillPlanDigestIgnoresNonExecutableCounts(t *testing.T) {
	fingerprint := sha256.Sum256([]byte("executable-raw-input"))
	items := []store.RFC822IDBackfillItem{{
		MessageID:       10,
		SourceID:        1,
		RFC822MessageID: "executable@example.com",
		RawInputSHA256:  fingerprint,
	}}
	base := store.RFC822IDBackfillPlan{Candidates: 3, Items: items, Failed: 2}

	tests := []struct {
		name string
		plan store.RFC822IDBackfillPlan
	}{
		{
			name: "candidate-only substitution",
			plan: store.RFC822IDBackfillPlan{Candidates: 4, Items: items, Failed: 2},
		},
		{
			name: "failed-only substitution",
			plan: store.RFC822IDBackfillPlan{Candidates: 3, Items: items, Failed: 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, base.Digest(), tt.plan.Digest())
		})
	}
}

func TestStore_RFC822IDBackfillRawFingerprintDistinguishesNullCompression(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	messageID := newRFC822Message(t, f, "compression-null-empty", "")
	raw := []byte("Message-ID: <compression@example.com>\r\n\r\nBody")
	_, err := f.Store.DB().Exec(f.Store.Rebind(
		`INSERT INTO message_raw (message_id, raw_data, raw_format, compression)
		 VALUES (?, ?, 'mime', NULL)`), messageID, raw)
	require.NoError(err)
	nullPlan, err := f.Store.PlanRFC822IDBackfill(t.Context(), nil)
	require.NoError(err)
	require.Len(nullPlan.Items, 1)

	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE message_raw SET compression = '' WHERE message_id = ?`), messageID)
	require.NoError(err)
	emptyPlan, err := f.Store.PlanRFC822IDBackfill(t.Context(), nil)
	require.NoError(err)
	require.Len(emptyPlan.Items, 1)

	assert.NotEqual(nullPlan.Items[0].RawInputSHA256, emptyPlan.Items[0].RawInputSHA256)
}

func TestStore_ApplyRFC822IDBackfillCommitsExactPlan(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	firstID := newRFC822Message(t, f, "apply-first", "")
	secondID := newRFC822Message(t, f, "apply-second", "")
	require.NoError(f.Store.UpsertMessageRaw(firstID, []byte(
		"Message-ID: <apply-first@example.com>\r\n\r\nFirst")))
	require.NoError(f.Store.UpsertMessageRaw(secondID, []byte(
		"Message-ID: <apply-second@example.com>\r\n\r\nSecond")))
	plan, err := f.Store.PlanRFC822IDBackfill(t.Context(), []int64{f.Source.ID})
	require.NoError(err)
	require.Len(plan.Items, 2)

	var progressCalls int
	updated, err := f.Store.ApplyRFC822IDBackfill(
		t.Context(), []int64{f.Source.ID}, plan,
		func(done, total int64) {
			progressCalls++
			assert.Equal(plan.Candidates, done)
			assert.Equal(plan.Candidates, total)
			assert.Equal("apply-first@example.com", storedRFC822ID(t, f.Store, firstID))
			assert.Equal("apply-second@example.com", storedRFC822ID(t, f.Store, secondID))
		},
	)
	require.NoError(err)
	assert.Equal(int64(2), updated)
	assert.Equal(1, progressCalls)
	assert.Equal("apply-first@example.com", storedRFC822ID(t, f.Store, firstID))
	assert.Equal("apply-second@example.com", storedRFC822ID(t, f.Store, secondID))
}

func TestStore_ApplyRFC822IDBackfillRollsBackWhenLaterMessageIsStale(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	firstID, secondID, plan := twoMessageRFC822IDBackfillPlan(t, f, "stale")
	_, err := f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET rfc822_message_id = ? WHERE id = ?`), "claimed@example.com", secondID)
	require.NoError(err)

	updated, err := f.Store.ApplyRFC822IDBackfill(t.Context(), []int64{f.Source.ID}, plan, nil)
	require.NoError(err)
	assert.Equal(int64(0), updated)
	assert.Empty(storedRFC822ID(t, f.Store, firstID))
	assert.Equal("claimed@example.com", storedRFC822ID(t, f.Store, secondID))
}

func TestStore_ApplyRFC822IDBackfillRollsBackWhenRawMIMEChanged(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	firstID, secondID, plan := twoMessageRFC822IDBackfillPlan(t, f, "raw-drift")
	require.NoError(f.Store.UpsertMessageRaw(secondID, []byte(
		"Message-ID: <raw-drift-second@example.com>\r\n\r\nChanged after planning")))

	updated, err := f.Store.ApplyRFC822IDBackfill(t.Context(), []int64{f.Source.ID}, plan, nil)
	require.NoError(err)
	assert.Equal(int64(0), updated)
	assert.Empty(storedRFC822ID(t, f.Store, firstID))
	assert.Empty(storedRFC822ID(t, f.Store, secondID))
}

func TestStore_ApplyRFC822IDBackfillProgressOnlyAfterCommit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	firstID, secondID, plan := twoMessageRFC822IDBackfillPlan(t, f, "progress")
	_, err := f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET rfc822_message_id = ? WHERE id = ?`), "stale@example.com", secondID)
	require.NoError(err)

	progressCalls := 0
	updated, err := f.Store.ApplyRFC822IDBackfill(
		t.Context(), []int64{f.Source.ID}, plan,
		func(_, _ int64) { progressCalls++ },
	)
	require.NoError(err)
	assert.Equal(int64(0), updated)
	assert.Equal(0, progressCalls)
	assert.Empty(storedRFC822ID(t, f.Store, firstID))
}

func twoMessageRFC822IDBackfillPlan(
	t *testing.T, f *storetest.Fixture, prefix string,
) (int64, int64, store.RFC822IDBackfillPlan) {
	t.Helper()
	firstID := newRFC822Message(t, f, prefix+"-first", "")
	secondID := newRFC822Message(t, f, prefix+"-second", "")
	require.NoError(t, f.Store.UpsertMessageRaw(firstID, []byte(
		"Message-ID: <"+prefix+"-first@example.com>\r\n\r\nFirst")))
	require.NoError(t, f.Store.UpsertMessageRaw(secondID, []byte(
		"Message-ID: <"+prefix+"-second@example.com>\r\n\r\nSecond")))
	plan, err := f.Store.PlanRFC822IDBackfill(t.Context(), []int64{f.Source.ID})
	require.NoError(t, err)
	require.Len(t, plan.Items, 2)
	return firstID, secondID, plan
}

func storedRFC822ID(t *testing.T, st *store.Store, messageID int64) string {
	t.Helper()
	var value sql.NullString
	require.NoError(t, st.DB().QueryRow(
		st.Rebind(`SELECT rfc822_message_id FROM messages WHERE id = ?`), messageID,
	).Scan(&value))
	return value.String
}

func TestStore_CountActiveMessages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	_ = newRFC822Message(t, f, "a", "id-a")
	idB := newRFC822Message(t, f, "b", "id-b")

	total, err := f.Store.CountActiveMessages()
	require.NoError(err, "CountActiveMessages")
	assert.Equal(int64(2), total, "active")

	_, err = f.Store.MergeDuplicates(
		newRFC822Message(t, f, "c", "id-c"),
		[]int64{idB},
		"batch-count",
	)
	require.NoError(err, "MergeDuplicates")

	total, err = f.Store.CountActiveMessages()
	require.NoError(err, "CountActiveMessages after merge")
	assert.Equal(int64(2), total, "active after merge")
}

func TestStore_BackfillRFC822IDs_ParsesFromRawMIME(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	id := newRFC822Message(t, f, "needs-backfill", "")

	rawMIME := []byte("From: alice@example.com\r\nTo: bob@example.com\r\nMessage-ID: <unique-123@example.com>\r\nSubject: Backfill test\r\n\r\nBody text")
	require.NoError(f.Store.UpsertMessageRaw(id, rawMIME),
		"UpsertMessageRaw",
	)

	count, err := f.Store.CountMessagesWithoutRFC822ID()
	require.NoError(err, "CountMessagesWithoutRFC822ID")
	require.Equal(int64(1), count, "count without rfc822")

	updated, _, err := f.Store.BackfillRFC822IDs(nil, nil)
	require.NoError(err, "BackfillRFC822IDs")
	require.Equal(int64(1), updated, "updated")

	var rfc822ID string
	err = f.Store.DB().QueryRow(
		f.Store.Rebind("SELECT rfc822_message_id FROM messages WHERE id = ?"), id,
	).Scan(&rfc822ID)
	require.NoError(err, "scan rfc822_message_id")
	assert.Equal("unique-123@example.com", rfc822ID, "rfc822_message_id")

	count, err = f.Store.CountMessagesWithoutRFC822ID()
	require.NoError(err, "CountMessagesWithoutRFC822ID after backfill")
	assert.Equal(int64(0), count, "count after backfill")
}

func TestStore_BackfillRFC822IDs_DoesNotOvercountRolledBackBatch(t *testing.T) {
	require := require.New(t)
	testutil.SkipIfPostgres(t, "uses SQLite-specific CREATE TRIGGER ... NEW.* / RAISE(FAIL,...) syntax to force a mid-batch rollback")
	f := storetest.New(t)

	idA := newRFC822Message(t, f, "needs-backfill-a", "")
	idB := newRFC822Message(t, f, "needs-backfill-b", "")

	rawA := []byte("From: alice@example.com\r\nMessage-ID: <unique-a@example.com>\r\n\r\nBody")
	rawB := []byte("From: bob@example.com\r\nMessage-ID: <unique-b@example.com>\r\n\r\nBody")
	require.NoError(f.Store.UpsertMessageRaw(idA, rawA), "UpsertMessageRaw A")
	require.NoError(f.Store.UpsertMessageRaw(idB, rawB), "UpsertMessageRaw B")

	_, err := f.Store.DB().Exec(fmt.Sprintf(`
		CREATE TRIGGER fail_backfill_second_message
		BEFORE UPDATE OF rfc822_message_id ON messages
		WHEN NEW.id = %d
		BEGIN
			SELECT RAISE(FAIL, 'forced backfill failure');
		END
	`, idB))
	require.NoError(err, "create trigger")

	updated, failed, err := f.Store.BackfillRFC822IDs(nil, nil)
	require.Error(err, "expected backfill error")
	require.Equal(int64(0), updated, "updated after rollback")
	require.Equal(int64(0), failed, "failed")

	var count int64
	err = f.Store.DB().QueryRow(`
		SELECT COUNT(*) FROM messages
		WHERE rfc822_message_id IS NOT NULL AND rfc822_message_id != ''
	`).Scan(&count)
	require.NoError(err, "count backfilled rows")
	require.Equal(int64(0), count, "backfilled rows after rollback")
}

func TestStore_MergeDuplicates_BackfillsRawMIME(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	idSurvivor := newRFC822Message(t, f, "survivor", "rfc822-mime-backfill")
	idDuplicate := newRFC822Message(t, f, "duplicate", "rfc822-mime-backfill")

	rawData := []byte("From: alice@example.com\r\nSubject: Test\r\n\r\nBody")
	require.NoError(f.Store.UpsertMessageRaw(idDuplicate, rawData),
		"UpsertMessageRaw on duplicate",
	)

	_, err := f.Store.GetMessageRaw(idSurvivor)
	require.Error(err, "survivor should not have raw MIME before merge")

	result, err := f.Store.MergeDuplicates(
		idSurvivor, []int64{idDuplicate}, "batch-mime",
	)
	require.NoError(err, "MergeDuplicates")
	assert.Equal(1, result.RawMIMEBackfilled, "RawMIMEBackfilled")

	got, err := f.Store.GetMessageRaw(idSurvivor)
	require.NoError(err, "GetMessageRaw survivor after merge")
	assert.NotEmpty(got, "survivor raw MIME should not be empty after backfill")
}

// TestStore_GetDuplicateGroupMessages_PreservesFromCase verifies that the
// FromEmail field returned by GetDuplicateGroupMessages preserves the
// original case of the sender's address. The query layer must NOT
// blanket-lowercase the address — synthetic identifiers like Matrix
// MXIDs (`@Alice:matrix.org`) and chat handles are case-sensitive in
// the rest of the identity subsystem (NormalizeIdentifierForCompare
// preserves case for non-email shapes), so any pre-lowering in SQL
// would prevent dedup's per-source identity match from finding a
// stored case-mixed identity. Regression test for iter12 codex Medium.
func TestStore_GetDuplicateGroupMessages_PreservesFromCase(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)

	mxid := "@Alice:matrix.org"
	pid := f.EnsureParticipant(mxid, "", "")

	id := newRFC822Message(t, f, "msg-mxid", "rfc822-mxid")

	_, err := f.Store.DB().Exec(
		f.Store.Rebind(`INSERT INTO message_recipients
			(message_id, participant_id, recipient_type)
			VALUES (?, ?, 'from')`),
		id, pid,
	)
	require.NoError(err, "insert from recipient")

	rows, err := f.Store.GetDuplicateGroupMessages("rfc822-mxid")
	require.NoError(err, "GetDuplicateGroupMessages")
	require.Len(rows, 1)
	assert.Equal(t, mxid, rows[0].FromEmail, "FromEmail (case must be preserved)")
}

// TestStore_GetAllRawMIMECandidates_PreservesFromCase mirrors
// TestStore_GetDuplicateGroupMessages_PreservesFromCase but covers the
// content-hash candidate path. Both queries had the same SQL `LOWER()`
// problem before iter12; both fixes need regression coverage so a
// future refactor that reintroduces lowercasing in either query is
// caught. Iter13 claude follow-up.
func TestStore_GetAllRawMIMECandidates_PreservesFromCase(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	mxid := "@Bob:matrix.org"
	pid := f.EnsureParticipant(mxid, "", "")

	id := newRFC822Message(t, f, "msg-mxid-raw", "rfc822-mxid-raw")
	_, err := f.Store.DB().Exec(
		f.Store.Rebind(`UPDATE messages
			SET size_estimate = ?, has_attachments = ?, attachment_count = ?
			WHERE id = ?`),
		int64(2048), true, 2, id,
	)
	require.NoError(err, "set completeness metadata")

	_, err = f.Store.DB().Exec(
		f.Store.Rebind(`INSERT INTO message_recipients
			(message_id, participant_id, recipient_type)
			VALUES (?, ?, 'from')`),
		id, pid,
	)
	require.NoError(err, "insert from recipient")

	// GetAllRawMIMECandidates only returns messages that have a raw
	// MIME row, so synthesize one.
	require.NoError(f.Store.UpsertMessageRaw(id, []byte("From: "+mxid+"\r\n\r\nbody")),
		"UpsertMessageRaw",
	)

	cands, err := f.Store.GetAllRawMIMECandidates()
	require.NoError(err, "GetAllRawMIMECandidates")
	var got *store.ContentHashCandidate
	for i := range cands {
		if cands[i].ID == id {
			got = &cands[i]
			break
		}
	}
	require.NotNil(got, "test message %d not in candidates: %+v", id, cands)
	assert.Equal(mxid, got.FromEmail, "FromEmail (case must be preserved)")
	assert.Equal(int64(2048), got.PayloadBytes, "PayloadBytes")
	assert.Equal(2, got.AttachmentCount, "AttachmentCount")
	assert.True(got.HasAttachments, "HasAttachments")
}

func TestStore_GetDuplicateGroupMessagesBatch_MatchesPerGroupQuery(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	// 600 groups exceeds queryInChunks's chunk size of 500, so this
	// exercises at least two chunk rounds and would catch a bug where the
	// result map is reset per chunk instead of accumulated across chunks.
	const numGroups = 600
	rfc822IDs := make([]string, numGroups)
	for i := range numGroups {
		rfc822ID := fmt.Sprintf("rfc822-batch-%d", i)
		rfc822IDs[i] = rfc822ID
		newRFC822Message(t, f, fmt.Sprintf("src-%d-a", i), rfc822ID)
		newRFC822Message(t, f, fmt.Sprintf("src-%d-b", i), rfc822ID)
	}

	batched, err := f.Store.GetDuplicateGroupMessagesBatch(rfc822IDs, f.Source.ID)
	require.NoError(err, "GetDuplicateGroupMessagesBatch")
	require.Len(batched, numGroups, "batched group count")

	for _, rfc822ID := range rfc822IDs {
		want, err := f.Store.GetDuplicateGroupMessages(rfc822ID, f.Source.ID)
		require.NoError(err, "GetDuplicateGroupMessages reference for %s", rfc822ID)
		assert.Equal(want, batched[rfc822ID], "mismatch for group %s", rfc822ID)
	}
}

func TestStore_GetDuplicateGroupMessagesBatch_EmptyInput(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)

	batched, err := f.Store.GetDuplicateGroupMessagesBatch(nil)
	require.NoError(err, "GetDuplicateGroupMessagesBatch with nil input")
	assert.Empty(batched, "no groups requested, no groups returned")
}

func TestStore_GetDuplicateGroupMessagesBatchRejectsOverlappingRequestedForms(t *testing.T) {
	f := storetest.New(t)
	_, err := f.Store.GetDuplicateGroupMessagesBatch([]string{
		"overlap@example.test",
		"<overlap@example.test>",
	})
	require.ErrorIs(t, err, store.ErrRFC822StorageFormCollision)
	require.ErrorContains(t, err, "maps to multiple duplicate groups")

	_, err = f.Store.GetDuplicateGroupMessagesBatch([]string{
		"repeated@example.test",
		"repeated@example.test",
	})
	require.NoError(t, err)
}

func TestStore_GetDuplicateGroupMessagesBatchPreservesOrderAcrossChunks(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	const targetID = "split@example.test"
	bracketedRow := newRFC822Message(t, f, "split-bracketed", "<"+targetID+">")
	bareRow := newRFC822Message(t, f, "split-bare", targetID)

	// 249 clean keys contribute 498 storage forms. The literal-only "<>"
	// occupies slot 499, so targetID's bare and bracketed forms straddle the
	// 500-value query chunk boundary.
	groupIDs := make([]string, 0, 251)
	for i := range 249 {
		groupIDs = append(groupIDs, fmt.Sprintf("padding-%03d@example.test", i))
	}
	groupIDs = append(groupIDs, "<>", targetID)

	single, err := f.Store.GetDuplicateGroupMessages(targetID)
	require.NoError(err)
	require.Len(single, 2)
	assert.Equal([]int64{bracketedRow, bareRow}, []int64{single[0].ID, single[1].ID})

	batched, err := f.Store.GetDuplicateGroupMessagesBatch(groupIDs)
	require.NoError(err)
	require.Len(batched[targetID], 2)
	assert.Equal(
		[]int64{bracketedRow, bareRow},
		[]int64{batched[targetID][0].ID, batched[targetID][1].ID},
	)
}

func TestStore_GetDuplicateGroupMessagesBatchContext_Canceled(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	newRFC822Message(t, f, "src-a", "rfc822-canceled")
	newRFC822Message(t, f, "src-b", "rfc822-canceled")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := f.Store.GetDuplicateGroupMessagesBatchContext(
		ctx, []string{"rfc822-canceled"}, f.Source.ID,
	)
	require.ErrorIs(err, context.Canceled)
}

func TestStore_GetDuplicateGroupMessagesBatch_FiltersBySourceID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	idA := newRFC822Message(t, f, "src-a", "rfc822-scoped")
	idB := newRFC822Message(t, f, "src-b", "rfc822-scoped")

	scoped, err := f.Store.GetDuplicateGroupMessagesBatch(
		[]string{"rfc822-scoped"}, f.Source.ID,
	)
	require.NoError(err, "GetDuplicateGroupMessagesBatch scoped to source")
	require.Len(scoped["rfc822-scoped"], 2)
	assert.Equal(idA, scoped["rfc822-scoped"][0].ID)
	assert.Equal(idB, scoped["rfc822-scoped"][1].ID)
}
