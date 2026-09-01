package dedup_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/dedup"
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/importer"
	msgmime "go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/email"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func addMessage(
	t *testing.T,
	st *store.Store,
	source *store.Source,
	srcMsgID, rfc822ID string,
	fromMe bool,
) int64 {
	t.Helper()
	convID, err := st.EnsureConversation(
		source.ID, "thread-"+srcMsgID, "Subject",
	)
	require.NoError(t, err, "EnsureConversation")
	id, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        source.ID,
		SourceMessageID: srcMsgID,
		RFC822MessageID: sql.NullString{
			String: rfc822ID, Valid: rfc822ID != "",
		},
		MessageType:  "email",
		IsFromMe:     fromMe,
		SizeEstimate: 1000,
	})
	require.NoError(t, err, "UpsertMessage")
	return id
}

func assertSoftDeleted(
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

func linkLabel(
	t *testing.T,
	st *store.Store,
	sourceID, msgID int64,
	sourceLabelID, name, typ string,
) {
	t.Helper()
	lid, err := st.EnsureLabel(sourceID, sourceLabelID, name, typ)
	require.NoError(t, err, "EnsureLabel "+sourceLabelID)
	require.NoError(t,
		st.LinkMessageLabel(msgID, lid),
		"LinkMessageLabel "+sourceLabelID,
	)
}

func ingestRawMessage(
	t *testing.T,
	st *store.Store,
	source *store.Source,
	sourceMessageID string,
	raw []byte,
	fallbackDate time.Time,
) int64 {
	t.Helper()
	hash := sha256.Sum256(raw)
	err := importer.IngestRawMessage(
		context.Background(), st, source.ID, source.Identifier, "",
		nil, sourceMessageID, hex.EncodeToString(hash[:]), raw, fallbackDate,
		slog.New(slog.DiscardHandler),
	)
	require.NoError(t, err, "IngestRawMessage")

	var messageID int64
	err = st.DB().QueryRow(
		st.Rebind(`SELECT id FROM messages
			WHERE source_id = ? AND source_message_id = ?`),
		source.ID, sourceMessageID,
	).Scan(&messageID)
	require.NoError(t, err, "find ingested message")
	return messageID
}

func setArchivedAt(
	t *testing.T, st *store.Store, messageID int64, archivedAt time.Time,
) {
	t.Helper()
	_, err := st.DB().Exec(
		st.Rebind("UPDATE messages SET archived_at = ? WHERE id = ?"),
		archivedAt, messageID,
	)
	require.NoError(t, err, "set archived_at")
}

func setRFC822MessageID(
	t *testing.T, st *store.Store, messageID int64, rfc822MessageID string,
) {
	t.Helper()
	_, err := st.DB().Exec(
		st.Rebind("UPDATE messages SET rfc822_message_id = ? WHERE id = ?"),
		rfc822MessageID, messageID,
	)
	require.NoError(t, err, "set rfc822_message_id")
}

func TestEngine_ScanRejectsMalformedRFC822DiscoveryGroupWithoutRawMIME(t *testing.T) {
	for _, malformedID := range []string{
		"<>",
		"<<nested@example.test>>",
		"< spaced@example.test>",
	} {
		t.Run(malformedID, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := storetest.New(t)
			firstID := addMessage(t, f.Store, f.Source, "first", malformedID, false)
			secondID := addMessage(t, f.Store, f.Source, "second", malformedID, false)
			engine := dedup.NewEngine(f.Store, dedup.Config{
				AccountSourceIDs: []int64{f.Source.ID},
				Account:          f.Source.Identifier,
			}, nil)

			report, err := engine.Scan(t.Context())
			require.NoError(err)
			assert.Empty(report.Groups)

			summary, err := engine.Execute(t.Context(), report, "malformed-group")
			require.NoError(err)
			assert.Zero(summary.GroupsMerged)
			assertSoftDeleted(t, f.Store, firstID, false)
			assertSoftDeleted(t, f.Store, secondID, false)
		})
	}
}

func TestEngine_ScanRejectsRecoveredMalformedRFC822Group(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	raw := []byte("Message-ID: <<recovered@example.test>>\r\n" +
		"Content-Type: multipart mixed; boundary=outer\r\n\r\n--outer--\r\n")
	parsed, parseErr := msgmime.ParseWithRecovery(raw, "fallback")
	require.Error(parseErr)
	require.Equal("<<recovered@example.test>>", parsed.MessageID)
	firstID := addMessage(t, f.Store, f.Source, "first", parsed.MessageID, false)
	secondID := addMessage(t, f.Store, f.Source, "second", parsed.MessageID, false)
	require.NoError(f.Store.UpsertMessageRaw(firstID, raw))
	require.NoError(f.Store.UpsertMessageRaw(secondID, raw))
	engine := dedup.NewEngine(f.Store, dedup.Config{
		AccountSourceIDs: []int64{f.Source.ID},
		Account:          f.Source.Identifier,
	}, nil)

	report, err := engine.Scan(t.Context())
	require.NoError(err)
	assert.Empty(report.Groups)
	assertSoftDeleted(t, f.Store, firstID, false)
	assertSoftDeleted(t, f.Store, secondID, false)
}

func TestEngine_ScanMergesEmbeddedNULMessageIDForms(t *testing.T) {
	testutil.SkipIfPostgres(t, "PostgreSQL TEXT rejects embedded NUL bytes")
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	groupID := "engine\x00nul@example.test"
	bareID := addMessage(t, f.Store, f.Source, "nul-bare", groupID, false)
	bracketedID := addMessage(t, f.Store, f.Source, "nul-bracketed", "<"+groupID+">", false)

	engine := dedup.NewEngine(f.Store, dedup.Config{
		AccountSourceIDs: []int64{f.Source.ID},
		Account:          f.Source.Identifier,
	}, nil)
	report, err := engine.Scan(t.Context())
	require.NoError(err)
	require.Len(report.Groups, 1)
	group := report.Groups[0]
	assert.Equal(groupID, group.Key)
	require.Len(group.Messages, 2)
	assert.ElementsMatch(
		[]int64{bareID, bracketedID},
		[]int64{group.Messages[0].ID, group.Messages[1].ID},
	)

	summary, err := engine.Execute(t.Context(), report, "embedded-nul-forms")
	require.NoError(err)
	assert.Equal(1, summary.GroupsMerged)

	var hidden int
	for _, messageID := range []int64{bareID, bracketedID} {
		var deletedAt sql.NullTime
		err := f.Store.DB().QueryRow(
			f.Store.Rebind("SELECT deleted_at FROM messages WHERE id = ?"), messageID,
		).Scan(&deletedAt)
		require.NoError(err)
		if deletedAt.Valid {
			hidden++
		}
	}
	assert.Equal(1, hidden)
}

func TestEngine_Scan_UnionsLabelsOnSurvivor(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	gmail := f.Source

	mbox, err := st.GetOrCreateSource("mbox", "test@example.com-mbox")
	require.NoError(err, "GetOrCreateSource mbox")

	idGmail := addMessage(t, st, gmail, "gmail-1", "rfc-union", false)
	idMbox := addMessage(t, st, mbox, "mbox-1", "rfc-union", false)

	linkLabel(t, st, gmail.ID, idGmail, "INBOX", "Inbox", "system")
	linkLabel(t, st, mbox.ID, idMbox, "Archive", "Archive", "user")
	linkLabel(t, st, mbox.ID, idMbox, "Work", "Work", "user")

	eng := dedup.NewEngine(st, dedup.Config{
		AccountSourceIDs: []int64{gmail.ID, mbox.ID},
		Account:          "test@example.com",
	}, nil)

	report, err := eng.Scan(context.Background())
	require.NoError(err, "Scan")
	require.Equal(1, report.DuplicateGroups, "groups")
	require.Equal(1, report.DuplicateMessages, "prune count")

	group := report.Groups[0]
	survivor := group.Messages[group.Survivor]
	assert.Equal(idGmail, survivor.ID, "survivor (gmail)")

	summary, err := eng.Execute(
		context.Background(), report, "batch-union",
	)
	require.NoError(err, "Execute")
	assert.Equal(1, summary.GroupsMerged, "groupsMerged")

	f.AssertLabelCount(idGmail, 3)
	assertSoftDeleted(t, st, idMbox, true)
}

func TestEngine_Scan_RejectsEmptyAccountSourceIDs(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	cases := []struct {
		name string
		ids  []int64
	}{
		{"nil", nil},
		{"empty slice", []int64{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := dedup.NewEngine(st, dedup.Config{
				AccountSourceIDs: tc.ids,
			}, nil)
			_, err := eng.Scan(context.Background())
			require.Error(t, err, "expected error for empty AccountSourceIDs")
			assert.ErrorContains(t, err, "AccountSourceIDs must be non-empty")
		})
	}
}

func TestEngine_SurvivorFavorsSentCopy(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	gmail := f.Source

	idInbox := addMessage(t, st, gmail, "inbox-sent", "rfc-sent", false)
	idSent := addMessage(t, st, gmail, "sent-sent", "rfc-sent", true)
	_, err := st.DB().Exec(
		st.Rebind(`UPDATE messages
			SET size_estimate = ?, attachment_count = ? WHERE id = ?`),
		int64(10000), 3, idInbox,
	)
	require.NoError(err, "make received copy payload-richer")
	_, err = st.DB().Exec(
		st.Rebind(`UPDATE messages
			SET size_estimate = ?, attachment_count = ? WHERE id = ?`),
		int64(100), 0, idSent,
	)
	require.NoError(err, "make sent copy payload-poorer")

	linkLabel(t, st, gmail.ID, idInbox, "INBOX", "Inbox", "system")
	linkLabel(t, st, gmail.ID, idSent, "SENT", "Sent", "system")

	eng := dedup.NewEngine(st, dedup.Config{
		AccountSourceIDs: []int64{gmail.ID},
		Account:          "test@example.com",
	}, nil)

	report, err := eng.Scan(context.Background())
	require.NoError(err, "Scan")
	require.Equal(1, report.DuplicateGroups, "groups")

	group := report.Groups[0]
	survivor := group.Messages[group.Survivor]
	assert.Equal(idSent, survivor.ID, "survivor (sent copy)")
	assert.True(survivor.IsSentCopy(), "survivor should be a sent copy")
}

func TestEngine_DefaultConfig_NeverStagesRemote(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	gmail := f.Source

	_ = addMessage(t, st, gmail, "g-1", "rfc-default", false)
	_ = addMessage(t, st, gmail, "g-2", "rfc-default", false)

	deletionsDir := filepath.Join(t.TempDir(), "deletions")
	eng := dedup.NewEngine(st, dedup.Config{
		AccountSourceIDs: []int64{gmail.ID},
		Account:          "test@example.com",
		DeletionsDir:     deletionsDir,
	}, nil)

	report, err := eng.Scan(context.Background())
	require.NoError(err, "Scan")
	summary, err := eng.Execute(
		context.Background(), report, "batch-default",
	)
	require.NoError(err, "Execute")

	assert.Equal(1, summary.MessagesRemoved, "messagesRemoved")
	assert.Empty(summary.StagedManifests, "stagedManifests")

	mgr, err := deletion.NewManager(deletionsDir)
	require.NoError(err, "NewManager")
	pending, err := mgr.ListPending()
	require.NoError(err, "ListPending")
	assert.Empty(pending, "pending manifests")
}

func TestEngine_OptIn_StagesOnlyWithinSameSourceID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	gmail := f.Source

	otherGmail, err := st.GetOrCreateSource("gmail", "other@example.com")
	require.NoError(err, "GetOrCreateSource otherGmail")
	mbox, err := st.GetOrCreateSource("mbox", "local.mbox")
	require.NoError(err, "GetOrCreateSource mbox")

	idWinner := addMessage(t, st, gmail, "g-1", "rfc-opt", false)
	idLoser := addMessage(t, st, gmail, "g-2", "rfc-opt", false)
	idOther := addMessage(t, st, otherGmail, "g-3", "rfc-opt", false)
	idMbox := addMessage(t, st, mbox, "m-1", "rfc-opt", false)
	raw := []byte("Message-ID: <rfc-opt>\r\nSubject: Same message\r\n\r\nBody")
	for _, id := range []int64{idWinner, idLoser, idOther, idMbox} {
		require.NoError(st.UpsertMessageRaw(id, raw), "store equivalent raw MIME")
	}

	deletionsDir := filepath.Join(t.TempDir(), "deletions")
	eng := dedup.NewEngine(st, dedup.Config{
		AccountSourceIDs:           []int64{gmail.ID, otherGmail.ID, mbox.ID},
		Account:                    "pile",
		DeleteDupsFromSourceServer: true,
		DeletionsDir:               deletionsDir,
	}, nil)

	report, err := eng.Scan(context.Background())
	require.NoError(err, "Scan")
	summary, err := eng.Execute(
		context.Background(), report, "batch-opt",
	)
	require.NoError(err, "Execute")

	assert.Equal(3, summary.MessagesRemoved, "messagesRemoved")
	assertSoftDeleted(t, st, idWinner, false)
	assertSoftDeleted(t, st, idLoser, true)
	assertSoftDeleted(t, st, idOther, true)
	assertSoftDeleted(t, st, idMbox, true)

	require.Len(summary.StagedManifests, 1, "stagedManifests")
	sm := summary.StagedManifests[0]
	assert.Equal("test@example.com", sm.Account, "staged account")
	assert.Equal(1, sm.MessageCount, "staged count")

	mgr, err := deletion.NewManager(deletionsDir)
	require.NoError(err, "NewManager")
	pending, err := mgr.ListPending()
	require.NoError(err, "ListPending")
	require.Len(pending, 1, "pending")
	assert.Equal([]string{"g-2"}, pending[0].GmailIDs, "manifest GmailIDs")
	assert.Equal(2, pending[0].Version)
	require.NotNil(pending[0].Source)
	assert.Equal(deletion.SourceReference{ID: gmail.ID, Type: "gmail", Identifier: "test@example.com"}, *pending[0].Source)

	restored, stillExec, err := eng.Undo("batch-opt")
	require.NoError(err, "Undo")
	assert.Equal(int64(3), restored, "restored")
	assert.Empty(stillExec, "stillExec")
	pending, err = mgr.ListPending()
	require.NoError(err, "ListPending after undo")
	assert.Empty(pending, "pending after undo")
}

func TestEngine_OptIn_RejectsMissingSourceIdentifierBeforeMerge(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store
	source := f.Source

	winnerID := addMessage(t, st, source, "g-1", "rfc-missing-source", false)
	loserID := addMessage(t, st, source, "g-2", "rfc-missing-source", false)
	raw := []byte("Message-ID: <rfc-missing-source>\r\nSubject: Same message\r\n\r\nBody")
	require.NoError(st.UpsertMessageRaw(winnerID, raw), "store winner raw MIME")
	require.NoError(st.UpsertMessageRaw(loserID, raw), "store loser raw MIME")
	deletionsDir := filepath.Join(t.TempDir(), "deletions")
	eng := dedup.NewEngine(st, dedup.Config{
		AccountSourceIDs:           []int64{source.ID},
		Account:                    "Work",
		DeleteDupsFromSourceServer: true,
		DeletionsDir:               deletionsDir,
	}, nil)

	report, err := eng.Scan(context.Background())
	require.NoError(err, "Scan")
	require.Len(report.Groups, 1)
	group := &report.Groups[0]
	for i := range group.Messages {
		if i != group.Survivor {
			group.Messages[i].SourceIdentifier = ""
		}
	}

	_, err = eng.Execute(context.Background(), report, "batch-missing-source")
	require.ErrorContains(err, "has no source identifier")
	assertSoftDeleted(t, st, winnerID, false)
	assertSoftDeleted(t, st, loserID, false)

	mgr, err := deletion.NewManager(deletionsDir)
	require.NoError(err, "NewManager")
	pending, err := mgr.ListPending()
	require.NoError(err, "ListPending")
	assert.Empty(t, pending)
}

func TestEngine_ScopedToSingleSource_IgnoresCrossAccount(t *testing.T) {
	f := storetest.New(t)
	st := f.Store
	alice := f.Source

	bob, err := st.GetOrCreateSource("gmail", "bob@example.com")
	require.NoError(t, err, "GetOrCreateSource bob")

	addMessage(t, st, alice, "a-1", "rfc-cross", true)
	addMessage(t, st, bob, "b-1", "rfc-cross", false)

	eng := dedup.NewEngine(st, dedup.Config{
		AccountSourceIDs: []int64{alice.ID},
		Account:          "test@example.com",
	}, nil)
	report, err := eng.Scan(context.Background())
	require.NoError(t, err, "Scan")
	assert.Equal(t, 0, report.DuplicateGroups, "cross-account dedup happened")
}

func TestEngine_ContentHashFallbackFindsNormalizedDuplicates(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store
	gmail := f.Source

	mbox, err := st.GetOrCreateSource("mbox", "test@example.com-mbox")
	require.NoError(err, "GetOrCreateSource mbox")

	id1 := addMessage(t, st, gmail, "hash-1", "", false)
	id2 := addMessage(t, st, mbox, "hash-2", "", false)

	raw1 := []byte("Received: from mx1.google.com\r\nDelivered-To: one@example.com\r\nX-Gmail-Labels: INBOX\r\nFrom: sender@example.com\r\nSubject: Meeting tomorrow\r\nDate: Mon, 1 Jan 2024 12:00:00 +0000\r\n\r\nLet's meet tomorrow at 3pm.")
	raw2 := []byte("Received: from mx2.google.com\r\nDelivered-To: two@example.com\r\nX-Gmail-Labels: SENT\r\nAuthentication-Results: spf=pass\r\nFrom: sender@example.com\r\nSubject: Meeting tomorrow\r\nDate: Mon, 1 Jan 2024 12:00:00 +0000\r\n\r\nLet's meet tomorrow at 3pm.")
	require.NoError(st.UpsertMessageRaw(id1, raw1), "UpsertMessageRaw id1")
	require.NoError(st.UpsertMessageRaw(id2, raw2), "UpsertMessageRaw id2")

	eng := dedup.NewEngine(st, dedup.Config{
		AccountSourceIDs:    []int64{gmail.ID, mbox.ID},
		Account:             "test@example.com",
		ContentHashFallback: true,
	}, nil)

	report, err := eng.Scan(context.Background())
	require.NoError(err, "Scan")
	require.Equal(1, report.DuplicateGroups, "groups")
	require.Equal(1, report.ContentHashGroups, "contentHashGroups")
	require.Equal("normalized-hash", report.Groups[0].KeyType, "keyType")
}

func TestEngine_ContentHashPrefersAttachmentCompleteCopy(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store
	source := f.Source

	partialID := addMessage(t, st, source, "hash-partial", "", false)
	fullID := addMessage(t, st, source, "hash-full", "", false)
	raw := []byte("From: sender@example.test\r\nSubject: Same content\r\n\r\nBody")
	require.NoError(st.UpsertMessageRaw(partialID, raw), "UpsertMessageRaw partial")
	require.NoError(st.UpsertMessageRaw(fullID, raw), "UpsertMessageRaw full")

	_, err := st.DB().Exec(
		st.Rebind(`UPDATE messages
			SET size_estimate = ?, attachment_count = ?, archived_at = ?
			WHERE id = ?`),
		int64(len(raw)), 0,
		time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC), partialID,
	)
	require.NoError(err, "set partial completeness")
	_, err = st.DB().Exec(
		st.Rebind(`UPDATE messages
			SET size_estimate = ?, attachment_count = ?, archived_at = ?
			WHERE id = ?`),
		int64(len(raw)+100), 1,
		time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC), fullID,
	)
	require.NoError(err, "set full completeness")

	eng := dedup.NewEngine(st, dedup.Config{
		AccountSourceIDs:    []int64{source.ID},
		Account:             source.Identifier,
		ContentHashFallback: true,
	}, nil)
	report, err := eng.Scan(context.Background())
	require.NoError(err, "Scan")
	require.Len(report.Groups, 1, "duplicate groups")
	require.Equal("normalized-hash", report.Groups[0].KeyType, "key type")

	group := report.Groups[0]
	survivor := group.Messages[group.Survivor]
	require.Equal(fullID, survivor.ID, "attachment-complete content-hash survivor")
}

func TestEngine_ContentHashFallbackDisabledByDefault(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store
	gmail := f.Source

	mbox, err := st.GetOrCreateSource("mbox", "test@example.com-mbox")
	require.NoError(err, "GetOrCreateSource mbox")

	id1 := addMessage(t, st, gmail, "hash-off-1", "", false)
	id2 := addMessage(t, st, mbox, "hash-off-2", "", false)
	raw := []byte("Subject: No Message-ID\r\n\r\nIdentical body")
	require.NoError(st.UpsertMessageRaw(id1, raw), "UpsertMessageRaw id1")
	require.NoError(st.UpsertMessageRaw(id2, raw), "UpsertMessageRaw id2")

	eng := dedup.NewEngine(st, dedup.Config{
		AccountSourceIDs: []int64{gmail.ID, mbox.ID},
		Account:          "test@example.com",
	}, nil)

	report, err := eng.Scan(context.Background())
	require.NoError(err, "Scan")
	require.Equal(0, report.DuplicateGroups, "groups")
}

func TestEngine_FormatMethodology_MentionsSentPolicy(t *testing.T) {
	f := storetest.New(t)
	eng := dedup.NewEngine(f.Store, dedup.Config{
		Account:          "test@example.com",
		AccountSourceIDs: []int64{f.Source.ID},
	}, nil)
	out := eng.FormatMethodology()
	assert.Contains(t,
		strings.ToLower(out),
		"never merges messages across different",
		"methodology missing cross-account guarantee",
	)
	assert.Contains(t,
		out,
		"Tiebreakers: has raw MIME > when all eligible copies have matching normalized MIME, more attachments > attachment signal > larger payload > more labels > earlier archived_at > lower id.",
		"methodology missing payload completeness order",
	)
}

// TestEngine_FormatMethodology_SingleMemberCollection asserts that a
// `--collection` invocation with only one resolved source does NOT
// describe itself as cross-account. Regression test for iter14
// claude Low: ScopeIsCollection alone gated the cross-account
// wording, even when len(AccountSourceIDs) == 1 made cross-account
// merging impossible.
func TestEngine_FormatMethodology_SingleMemberCollection(t *testing.T) {
	f := storetest.New(t)
	eng := dedup.NewEngine(f.Store, dedup.Config{
		Account:           "myCollection",
		AccountSourceIDs:  []int64{f.Source.ID},
		ScopeIsCollection: true,
	}, nil)
	out := eng.FormatMethodology()
	lower := strings.ToLower(out)
	assert.NotContains(t, lower, "cross-account dedup\n  is enabled",
		"single-member collection should not advertise cross-account dedup; got:\n%s", out)
	assert.NotContains(t, lower, "intentionally merges messages",
		"single-member collection should not describe intentional cross-account merging; got:\n%s", out)
	assert.Contains(t, lower, "never merges messages across different",
		"single-member collection should fall to the same-account guarantee; got:\n%s", out)
}

func TestEngine_NonEquivalentPayloadCannotSteerSurvivorOrRemoteDeletion(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store

	const messageID = "shared-message-id@example.test"
	legitimateID := addMessage(
		t, st, f.Source, "legitimate-source-id", messageID, false,
	)
	forgedID := addMessage(
		t, st, f.Source, "forged-source-id", messageID, false,
	)
	require.NoError(st.UpsertMessageRaw(
		legitimateID,
		[]byte("Message-ID: <shared-message-id@example.test>\r\n"+
			"From: sender@example.test\r\nSubject: Expected message\r\n\r\nExpected body"),
	), "store legitimate raw MIME")
	require.NoError(st.UpsertMessageRaw(
		forgedID,
		[]byte("Message-ID: <shared-message-id@example.test>\r\n"+
			"From: attacker@example.test\r\nSubject: Different message\r\n\r\nDifferent body"),
	), "store forged raw MIME")

	_, err := st.DB().Exec(
		st.Rebind(`UPDATE messages
			SET size_estimate = ?, has_attachments = ?, attachment_count = ?
			WHERE id = ?`),
		int64(100), false, 0, legitimateID,
	)
	require.NoError(err, "set legitimate completeness")
	_, err = st.DB().Exec(
		st.Rebind(`UPDATE messages
			SET size_estimate = ?, has_attachments = ?, attachment_count = ?
			WHERE id = ?`),
		int64(10000), true, 3, forgedID,
	)
	require.NoError(err, "set forged completeness")

	deletionsDir := filepath.Join(t.TempDir(), "deletions")
	eng := dedup.NewEngine(st, dedup.Config{
		AccountSourceIDs:           []int64{f.Source.ID},
		Account:                    f.Source.Identifier,
		DeleteDupsFromSourceServer: true,
		DeletionsDir:               deletionsDir,
	}, nil)
	report, err := eng.Scan(context.Background())
	require.NoError(err, "Scan")
	require.Len(report.Groups, 1, "duplicate groups")

	group := report.Groups[0]
	survivor := group.Messages[group.Survivor]
	assert.Equal(legitimateID, survivor.ID,
		"non-equivalent completeness metadata must not steer survivor selection")

	summary, err := eng.Execute(context.Background(), report, "non-equivalent")
	require.NoError(err, "Execute")
	assert.Empty(summary.StagedManifests,
		"non-equivalent Message-ID matches must not stage remote deletion")
	assertSoftDeleted(t, st, legitimateID, false)
	assertSoftDeleted(t, st, forgedID, true)

	mgr, err := deletion.NewManager(deletionsDir)
	require.NoError(err, "NewManager")
	pending, err := mgr.ListPending()
	require.NoError(err, "ListPending")
	assert.Empty(pending, "pending remote deletions")
}

func TestEngine_MixedNormalizedHashesIgnorePayloadCompleteness(t *testing.T) {
	type copySpec struct {
		raw             []byte
		payloadBytes    int64
		attachmentCount int
		hasAttachments  bool
		labels          []string
	}

	const messageID = "mixed-normalized-hashes@example.test"
	equivalentRaw := []byte("Message-ID: <mixed-normalized-hashes@example.test>\r\n" +
		"Subject: Equivalent message\r\n\r\nEquivalent body")
	specs := map[string]copySpec{
		"payload-rich": {
			raw:             equivalentRaw,
			payloadBytes:    10000,
			attachmentCount: 3,
			hasAttachments:  true,
		},
		"label-rich": {
			raw:          equivalentRaw,
			payloadBytes: 100,
			labels:       []string{"label-one", "label-two"},
		},
		"different-content": {
			raw: []byte("Message-ID: <mixed-normalized-hashes@example.test>\r\n" +
				"Subject: Different message\r\n\r\nDifferent body"),
			payloadBytes: 100,
			labels:       []string{"label-three"},
		},
	}
	orders := [][]string{
		{"payload-rich", "label-rich", "different-content"},
		{"label-rich", "different-content", "payload-rich"},
		{"different-content", "payload-rich", "label-rich"},
	}

	for _, order := range orders {
		t.Run(strings.Join(order, "_then_"), func(t *testing.T) {
			require := require.New(t)
			f := storetest.New(t)
			ids := make(map[string]int64, len(order))
			for _, name := range order {
				spec := specs[name]
				id := addMessage(t, f.Store, f.Source, name, messageID, false)
				ids[name] = id
				require.NoError(f.Store.UpsertMessageRaw(id, spec.raw), "store "+name+" raw MIME")
				_, err := f.Store.DB().Exec(
					f.Store.Rebind(`UPDATE messages
						SET size_estimate = ?, attachment_count = ?, has_attachments = ?
						WHERE id = ?`),
					spec.payloadBytes, spec.attachmentCount, spec.hasAttachments, id,
				)
				require.NoError(err, "set "+name+" metadata")
				for _, label := range spec.labels {
					linkLabel(t, f.Store, f.Source.ID, id, label, label, "user")
				}
			}

			eng := dedup.NewEngine(f.Store, dedup.Config{
				AccountSourceIDs: []int64{f.Source.ID},
				Account:          f.Source.Identifier,
			}, nil)
			report, err := eng.Scan(t.Context())
			require.NoError(err, "Scan")
			require.Len(report.Groups, 1, "duplicate groups")

			group := report.Groups[0]
			survivor := group.Messages[group.Survivor]
			require.Equal(ids["label-rich"], survivor.ID,
				"mixed content hashes must fall back to the group-wide label ordering")
		})
	}
}

func TestEngine_PartialFirstFullLaterKeepsAttachmentCompleteCopy(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store

	source, err := st.GetOrCreateSource("apple-mail", "mailbox@example.test")
	require.NoError(err, "GetOrCreateSource")

	const messageID = "<partial-full@example.test>"
	raw := email.NewMessage().
		From("sender@example.test").
		To("recipient@example.test").
		Header("Message-ID", messageID).
		Body("Complete body, attachment not cached.").
		WithAttachment(
			"report.txt", "text/plain", []byte("downloaded attachment"),
		).
		Bytes()

	partialID := ingestRawMessage(
		t, st, source, "partial-copy", raw,
		time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
	)
	fullID := ingestRawMessage(
		t, st, source, "full-copy", raw,
		time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC),
	)
	setRFC822MessageID(t, st, partialID, strings.Trim(messageID, "<>"))
	setRFC822MessageID(t, st, fullID, strings.Trim(messageID, "<>"))
	_, err = st.DB().Exec(
		st.Rebind("DELETE FROM attachments WHERE message_id = ?"), partialID,
	)
	require.NoError(err, "remove partial extracted attachment")
	_, err = st.DB().Exec(
		st.Rebind(`UPDATE messages
			SET has_attachments = FALSE, attachment_count = 0 WHERE id = ?`),
		partialID,
	)
	require.NoError(err, "mark attachment extraction incomplete")
	setArchivedAt(t, st, partialID, time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	setArchivedAt(t, st, fullID, time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC))

	eng := dedup.NewEngine(st, dedup.Config{
		AccountSourceIDs: []int64{source.ID},
		Account:          source.Identifier,
	}, nil)
	report, err := eng.Scan(context.Background())
	require.NoError(err, "Scan")
	require.Len(report.Groups, 1, "duplicate groups")

	group := report.Groups[0]
	survivor := group.Messages[group.Survivor]
	require.Equal(fullID, survivor.ID, "attachment-complete survivor")

	_, err = eng.Execute(context.Background(), report, "partial-full")
	require.NoError(err, "Execute")
	assertSoftDeleted(t, st, partialID, true)
	assertSoftDeleted(t, st, fullID, false)
}

func TestEngine_CrossMailboxPartialFullKeepsAttachmentCompleteCopy(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store

	partialSource, err := st.GetOrCreateSource(
		"apple-mail", "mailbox-a@example.test",
	)
	require.NoError(err, "GetOrCreateSource partial")
	fullSource, err := st.GetOrCreateSource(
		"apple-mail", "mailbox-b@example.test",
	)
	require.NoError(err, "GetOrCreateSource full")

	const messageID = "<cross-mailbox-partial-full@example.test>"
	raw := email.NewMessage().
		Header("Message-ID", messageID).
		Body("Message body").
		WithAttachment(
			"archive.bin", "application/octet-stream", []byte("complete payload"),
		).
		Bytes()

	partialID := ingestRawMessage(
		t, st, partialSource, "mailbox-a-partial", raw,
		time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC),
	)
	fullID := ingestRawMessage(
		t, st, fullSource, "mailbox-b-full", raw,
		time.Date(2026, 2, 2, 12, 0, 0, 0, time.UTC),
	)
	setRFC822MessageID(t, st, partialID, strings.Trim(messageID, "<>"))
	setRFC822MessageID(t, st, fullID, strings.Trim(messageID, "<>"))
	_, err = st.DB().Exec(
		st.Rebind("DELETE FROM attachments WHERE message_id = ?"), partialID,
	)
	require.NoError(err, "remove partial extracted attachment")
	_, err = st.DB().Exec(
		st.Rebind(`UPDATE messages
			SET has_attachments = FALSE, attachment_count = 0 WHERE id = ?`),
		partialID,
	)
	require.NoError(err, "mark attachment extraction incomplete")
	setArchivedAt(t, st, partialID, time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC))
	setArchivedAt(t, st, fullID, time.Date(2026, 2, 2, 12, 0, 0, 0, time.UTC))

	eng := dedup.NewEngine(st, dedup.Config{
		AccountSourceIDs:  []int64{partialSource.ID, fullSource.ID},
		Account:           "Apple Mail",
		ScopeIsCollection: true,
	}, nil)
	report, err := eng.Scan(context.Background())
	require.NoError(err, "Scan")
	require.Len(report.Groups, 1, "duplicate groups")

	group := report.Groups[0]
	survivor := group.Messages[group.Survivor]
	require.Equal(fullID, survivor.ID, "attachment-complete cross-mailbox survivor")
	assert.Equal(fullSource.ID, survivor.SourceID, "survivor source")
}

func TestEngine_SourcePriorityOutranksPayloadCompleteness(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store
	gmail := f.Source
	appleMail, err := st.GetOrCreateSource(
		"apple-mail", "archive@example.test",
	)
	require.NoError(err, "GetOrCreateSource apple-mail")

	gmailID := addMessage(
		t, st, gmail, "gmail-copy", "source-priority-completeness", false,
	)
	appleMailID := addMessage(
		t, st, appleMail, "apple-mail-copy", "source-priority-completeness", false,
	)
	require.NoError(
		st.UpsertMessageRaw(gmailID, []byte(
			"Message-ID: <source-priority-completeness>\r\n"+
				"From: sender@example.test\r\n\r\nBody")),
		"UpsertMessageRaw gmail",
	)
	require.NoError(
		st.UpsertMessageRaw(
			appleMailID,
			[]byte("Message-ID: <source-priority-completeness>\r\n"+
				"From: sender@example.test\r\n\r\nBody with complete attachments"),
		),
		"UpsertMessageRaw apple-mail",
	)
	_, err = st.DB().Exec(
		st.Rebind(`UPDATE messages
			SET size_estimate = ?, attachment_count = ? WHERE id = ?`),
		int64(100), 0, gmailID,
	)
	require.NoError(err, "set gmail completeness")
	_, err = st.DB().Exec(
		st.Rebind(`UPDATE messages
			SET size_estimate = ?, attachment_count = ? WHERE id = ?`),
		int64(10000), 3, appleMailID,
	)
	require.NoError(err, "set apple-mail completeness")

	eng := dedup.NewEngine(st, dedup.Config{
		AccountSourceIDs:  []int64{gmail.ID, appleMail.ID},
		Account:           "collection",
		ScopeIsCollection: true,
	}, nil)
	report, err := eng.Scan(context.Background())
	require.NoError(err, "Scan")
	require.Len(report.Groups, 1, "duplicate groups")

	group := report.Groups[0]
	survivor := group.Messages[group.Survivor]
	require.Equal(gmailID, survivor.ID, "source-priority survivor")
	require.Equal(0, survivor.AttachmentCount, "higher-priority source can be less complete")
}

func TestEngine_SurvivorTiebreakerHasAttachments(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store

	idWithoutFlag := addMessage(
		t, st, f.Source, "without-attachment-flag", "rfc-attachment-flag", false,
	)
	idWithFlag := addMessage(
		t, st, f.Source, "with-attachment-flag", "rfc-attachment-flag", false,
	)
	raw := []byte("Message-ID: <rfc-attachment-flag>\r\nSubject: Same\r\n\r\nBody")
	require.NoError(st.UpsertMessageRaw(idWithoutFlag, raw), "store first raw MIME")
	require.NoError(st.UpsertMessageRaw(idWithFlag, raw), "store second raw MIME")

	_, err := st.DB().Exec(
		st.Rebind(`UPDATE messages
				SET size_estimate = ?, has_attachments = ?, attachment_count = ?
				WHERE id = ?`),
		int64(len(raw)), false, 0, idWithoutFlag,
	)
	require.NoError(err, "clear attachment flag")
	_, err = st.DB().Exec(
		st.Rebind(`UPDATE messages
				SET size_estimate = ?, has_attachments = ?, attachment_count = ?
				WHERE id = ?`),
		int64(len(raw)), true, 0, idWithFlag,
	)
	require.NoError(err, "set attachment flag")

	eng := dedup.NewEngine(st, dedup.Config{
		AccountSourceIDs: []int64{f.Source.ID},
		Account:          "test",
	}, nil)
	report, err := eng.Scan(context.Background())
	require.NoError(err, "Scan")
	require.Len(report.Groups, 1, "duplicate groups")
	survivor := report.Groups[0].Messages[report.Groups[0].Survivor]
	assert.Equal(t, idWithFlag, survivor.ID, "survivor (has attachments flag)")
}

func TestEngine_SurvivorTiebreakerLargerPayload(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store

	idSmaller := addMessage(t, st, f.Source, "smaller", "rfc-payload-tie", false)
	idLarger := addMessage(t, st, f.Source, "larger", "rfc-payload-tie", false)
	raw := []byte("Message-ID: <rfc-payload-tie>\r\nSubject: Same\r\n\r\nBody")
	require.NoError(st.UpsertMessageRaw(idSmaller, raw), "store smaller raw MIME")
	require.NoError(st.UpsertMessageRaw(idLarger, raw), "store larger raw MIME")
	_, err := st.DB().Exec(
		st.Rebind(`UPDATE messages
				SET size_estimate = ?, attachment_count = ? WHERE id = ?`),
		int64(1000), 1, idSmaller,
	)
	require.NoError(err, "set smaller payload completeness")
	_, err = st.DB().Exec(
		st.Rebind(`UPDATE messages
				SET size_estimate = ?, attachment_count = ? WHERE id = ?`),
		int64(2000), 1, idLarger,
	)
	require.NoError(err, "set larger payload completeness")

	eng := dedup.NewEngine(st, dedup.Config{
		AccountSourceIDs: []int64{f.Source.ID},
		Account:          "test",
	}, nil)
	report, err := eng.Scan(context.Background())
	require.NoError(err, "Scan")
	require.Equal(1, report.DuplicateGroups, "groups")
	survivor := report.Groups[0].Messages[report.Groups[0].Survivor]
	assert.Equal(t, idLarger, survivor.ID, "survivor (larger payload)")
}

func TestEngine_SurvivorTiebreakerRawMIME(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	st := f.Store

	idNoRaw := addMessage(t, st, f.Source, "no-raw", "rfc-raw-tie", false)
	idHasRaw := addMessage(t, st, f.Source, "has-raw", "rfc-raw-tie", false)
	require.NoError(st.UpsertMessageRaw(idHasRaw,
		[]byte("Message-ID: <rfc-raw-tie>\r\nSubject: test\r\n\r\nBody")),
		"UpsertMessageRaw",
	)

	eng := dedup.NewEngine(st, dedup.Config{
		AccountSourceIDs: []int64{f.Source.ID},
		Account:          "test",
	}, nil)
	report, err := eng.Scan(context.Background())
	require.NoError(err, "Scan")
	require.Equal(1, report.DuplicateGroups, "groups")
	survivor := report.Groups[0].Messages[report.Groups[0].Survivor]
	assert.Equal(t, idHasRaw, survivor.ID, "survivor (has raw)")
	_ = idNoRaw
}

func TestEngine_SurvivorTiebreakerMoreLabels(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	idFew := addMessage(t, st, f.Source, "few", "rfc-label-tie", false)
	idMany := addMessage(t, st, f.Source, "many", "rfc-label-tie", false)

	lid1, _ := st.EnsureLabel(f.Source.ID, "L1", "Label1", "user")
	lid2, _ := st.EnsureLabel(f.Source.ID, "L2", "Label2", "user")
	lid3, _ := st.EnsureLabel(f.Source.ID, "L3", "Label3", "user")
	_ = st.LinkMessageLabel(idFew, lid1)
	_ = st.LinkMessageLabel(idMany, lid1)
	_ = st.LinkMessageLabel(idMany, lid2)
	_ = st.LinkMessageLabel(idMany, lid3)

	eng := dedup.NewEngine(st, dedup.Config{
		AccountSourceIDs: []int64{f.Source.ID},
		Account:          "test",
	}, nil)
	report, err := eng.Scan(context.Background())
	require.NoError(t, err, "Scan")
	require.Equal(t, 1, report.DuplicateGroups, "groups")
	survivor := report.Groups[0].Messages[report.Groups[0].Survivor]
	assert.Equal(t, idMany, survivor.ID, "survivor (more labels)")
}

func TestEngine_SurvivorTiebreakerLowerID(t *testing.T) {
	f := storetest.New(t)
	st := f.Store

	idFirst := addMessage(t, st, f.Source, "first", "rfc-id-tie", false)
	_ = addMessage(t, st, f.Source, "second", "rfc-id-tie", false)

	eng := dedup.NewEngine(st, dedup.Config{
		AccountSourceIDs: []int64{f.Source.ID},
		Account:          "test",
	}, nil)
	report, err := eng.Scan(context.Background())
	require.NoError(t, err, "Scan")
	require.Equal(t, 1, report.DuplicateGroups, "groups")
	survivor := report.Groups[0].Messages[report.Groups[0].Survivor]
	assert.Equal(t, idFirst, survivor.ID, "survivor (lower ID)")
}

// addMessageWithFrom is like addMessage but also sets FromEmail via the
// message_recipients table so the dedup query can read it.
func addMessageWithFrom(
	t *testing.T,
	st *store.Store,
	source *store.Source,
	srcMsgID, rfc822ID, fromEmail string,
) int64 {
	t.Helper()
	require := require.New(t)
	convID, err := st.EnsureConversation(
		source.ID, "thread-"+srcMsgID, "Subject",
	)
	require.NoError(err, "EnsureConversation")
	id, err := st.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        source.ID,
		SourceMessageID: srcMsgID,
		RFC822MessageID: sql.NullString{
			String: rfc822ID, Valid: rfc822ID != "",
		},
		MessageType:  "email",
		IsFromMe:     false, // no is_from_me so MatchedIdentity is the deciding signal
		SizeEstimate: 1000,
	})
	require.NoError(err, "UpsertMessage")
	if fromEmail != "" {
		pid, pErr := st.EnsureParticipant(fromEmail, "", "")
		require.NoError(pErr, "EnsureParticipant")
		require.NoError(st.ReplaceMessageRecipients(id, "from", []int64{pid}, []string{""}),
			"ReplaceMessageRecipients",
		)
	}
	return id
}

// TestEngine_PerSourceIdentity verifies that identity matching is per-source:
// an address confirmed only for source A does not count as "me" in source B.
func TestEngine_PerSourceIdentity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	sourceA := f.Source // already created by storetest.New

	sourceB, err := st.GetOrCreateSource("mbox", "bob@example.com-mbox")
	require.NoError(err, "GetOrCreateSource sourceB")

	const me = "me@personal.com"
	const rfc = "rfc-identity-perscource"

	// Add me@personal.com as confirmed identity only for source A.
	require.NoError(st.AddAccountIdentity(sourceA.ID, me, "test"),
		"AddAccountIdentity sourceA",
	)

	// Two messages with same RFC822 ID, both From: me@personal.com,
	// one in each source. Neither has HasSentLabel or IsFromMe.
	idA := addMessageWithFrom(t, st, sourceA, "a-identity", rfc, me)
	idB := addMessageWithFrom(t, st, sourceB, "b-identity", rfc, me)

	identities := map[int64]map[string]struct{}{
		sourceA.ID: {me: {}},
		// sourceB intentionally omitted
	}

	eng := dedup.NewEngine(st, dedup.Config{
		AccountSourceIDs:          []int64{sourceA.ID, sourceB.ID},
		Account:                   "test",
		IdentityAddressesBySource: identities,
	}, nil)

	report, err := eng.Scan(context.Background())
	require.NoError(err, "Scan")
	require.Equal(1, report.DuplicateGroups, "groups")

	group := report.Groups[0]
	// Find the message structs for each source.
	var msgA, msgB dedup.DuplicateMessage
	for _, m := range group.Messages {
		switch m.ID {
		case idA:
			msgA = m
		case idB:
			msgB = m
		}
	}

	assert.True(msgA.MatchedIdentity, "source A copy: MatchedIdentity")
	assert.False(msgB.MatchedIdentity, "source B copy: MatchedIdentity (identity not confirmed for source B)")

	// Survivor should be the source A copy because it is the sent copy.
	survivor := group.Messages[group.Survivor]
	assert.Equal(idA, survivor.ID,
		"survivor (%s), want source A, matched identity",
		survivor.SourceIdentifier)
}

func TestEngine_AliasOnlyAfterDiscoveryConfirmation(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	source := f.Source

	const alias = "masked-shop@example.test"
	const rfc822ID = "rfc-alias-confirmation"
	aliasMessageID := addMessageWithFrom(
		t,
		st,
		source,
		"alias-copy",
		rfc822ID,
		alias,
	)
	_ = addMessageWithFrom(
		t,
		st,
		source,
		"other-copy",
		rfc822ID,
		"other-sender@example.test",
	)

	plan := func(identityAddresses map[int64]map[string]struct{}) dedup.DuplicateMessage {
		t.Helper()
		engine := dedup.NewEngine(st, dedup.Config{
			AccountSourceIDs:          []int64{source.ID},
			Account:                   source.Identifier,
			IdentityAddressesBySource: identityAddresses,
		}, nil)
		report, err := engine.Scan(context.Background())
		require.NoError(err, "Scan")
		require.Len(report.Groups, 1, "duplicate groups")
		for _, message := range report.Groups[0].Messages {
			if message.ID == aliasMessageID {
				return message
			}
		}
		require.Fail("alias message missing from duplicate plan")
		return dedup.DuplicateMessage{}
	}

	before := plan(nil)
	assert.False(before.MatchedIdentity, "unconfirmed alias must not count as sender identity")
	require.NoError(st.AddAccountIdentity(source.ID, "MASKED-SHOP@EXAMPLE.TEST", "sent-folder"),
		"confirm discovered alias")
	confirmed, err := st.ListAccountIdentities(source.ID)
	require.NoError(err, "ListAccountIdentities")
	identityAddresses := map[int64]map[string]struct{}{source.ID: {}}
	for _, identity := range confirmed {
		identityAddresses[source.ID][store.NormalizeIdentifierForCompare(identity.Address)] = struct{}{}
	}

	after := plan(identityAddresses)
	assert.True(after.MatchedIdentity, "confirmed alias counts as sender identity")
}

func TestEngine_ScanPlansRFC822BackfillWithoutWriting(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	messageID := addMessage(t, f.Store, f.Source, "missing-rfc822", "", false)
	require.NoError(f.Store.UpsertMessageRaw(messageID,
		[]byte("Message-ID: <derived@example.test>\r\nSubject: Planned\r\n\r\nBody")))

	engine := dedup.NewEngine(f.Store, dedup.Config{
		AccountSourceIDs: []int64{f.Source.ID},
		Account:          f.Source.Identifier,
	}, nil)
	report, err := engine.Scan(t.Context())
	require.NoError(err)

	assert.Equal(int64(1), report.BackfillCandidates)
	assert.Equal(int64(0), report.BackfillFailed)
	assert.Equal(int64(1), report.RFC822IDsReady)
	assert.Equal(int64(1), report.PendingRFC822IDBackfill())
	assert.NotEmpty(report.BackfillPlanDigest)
	assert.Equal(0, report.DuplicateGroups)

	var storedID sql.NullString
	err = f.Store.DB().QueryRow(
		f.Store.Rebind("SELECT rfc822_message_id FROM messages WHERE id = ?"),
		messageID,
	).Scan(&storedID)
	require.NoError(err)
	assert.False(storedID.Valid, "Scan must not persist the planned derivation")
}

func TestEngine_ScanMalformedOnlyBackfillDoesNotExposePendingWork(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	messageID := addMessage(t, f.Store, f.Source, "malformed-rfc822", "", false)
	require.NoError(f.Store.UpsertMessageRaw(messageID,
		[]byte("Subject: Missing Message-ID\r\n\r\nBody")))

	engine := dedup.NewEngine(f.Store, dedup.Config{
		AccountSourceIDs: []int64{f.Source.ID},
		Account:          f.Source.Identifier,
	}, nil)
	report, err := engine.Scan(t.Context())
	require.NoError(err)

	assert.Equal(int64(1), report.BackfillCandidates)
	assert.Equal(int64(1), report.BackfillFailed)
	assert.Equal(int64(0), report.RFC822IDsReady)
	assert.Equal(int64(0), report.PendingRFC822IDBackfill())
	assert.Equal(0, report.DuplicateGroups)
}

func TestEngine_ScanRejectsLegacyNormalizedMalformedMessageID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	const storedID = "legacy-normalized@example.test"
	validID := addMessage(t, f.Store, f.Source, "valid-header", storedID, false)
	legacyID := addMessage(t, f.Store, f.Source, "legacy-header", storedID, false)
	require.NoError(f.Store.UpsertMessageRaw(validID,
		[]byte("Message-ID: <"+storedID+">\r\n\r\nValid")))
	require.NoError(f.Store.UpsertMessageRaw(legacyID,
		[]byte("Message-ID: <<"+storedID+">>\r\n\r\nMalformed")))

	engine := dedup.NewEngine(f.Store, dedup.Config{
		AccountSourceIDs: []int64{f.Source.ID},
		Account:          f.Source.Identifier,
	}, nil)
	report, err := engine.Scan(t.Context())
	require.NoError(err)

	assert.Zero(report.DuplicateGroups)
	assert.Zero(report.DuplicateMessages)
}

func TestEngine_ScanRequiresAvailableRawMIMEToConfirmStoredMessageID(t *testing.T) {
	tests := []struct {
		name       string
		raw        []byte
		parseFails bool
		wantGroups int
	}{
		{
			name: "missing Message-ID header",
			raw:  []byte("Subject: no message ID\r\n\r\nBody"),
		},
		{
			name: "recovered Message-ID differs",
			raw: []byte("Message-ID: <different@example.test>\r\n" +
				"Content-Type: multipart mixed; boundary=outer\r\n\r\n--outer--\r\n"),
			parseFails: true,
		},
		{
			name: "recovered Message-ID matches",
			raw: []byte("Message-ID: <stored@example.test>\r\n" +
				"Content-Type: multipart mixed; boundary=outer\r\n\r\n--outer--\r\n"),
			parseFails: true,
			wantGroups: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := storetest.New(t)
			const storedID = "stored@example.test"
			firstID := addMessage(t, f.Store, f.Source, "first", storedID, false)
			secondID := addMessage(t, f.Store, f.Source, "second", storedID, false)
			require.NoError(f.Store.UpsertMessageRaw(firstID,
				[]byte("Message-ID: <"+storedID+">\r\n\r\nValid")))
			require.NoError(f.Store.UpsertMessageRaw(secondID, tt.raw))
			_, parseErr := msgmime.Parse(tt.raw)
			if tt.parseFails {
				require.Error(parseErr)
			} else {
				require.NoError(parseErr)
			}

			engine := dedup.NewEngine(f.Store, dedup.Config{
				AccountSourceIDs: []int64{f.Source.ID},
				Account:          f.Source.Identifier,
			}, nil)
			report, err := engine.Scan(t.Context())
			require.NoError(err)

			assert.Len(report.Groups, tt.wantGroups)
		})
	}
}

func TestEngine_ExecuteBackfillsThenMergesWhenActionablePlanIsUnchanged(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	winnerID := addMessage(t, f.Store, f.Source, "winner", "stable@example.test", false)
	loserID := addMessage(t, f.Store, f.Source, "loser", "stable@example.test", false)
	derivedID := addMessage(t, f.Store, f.Source, "derived", "", false)
	require.NoError(f.Store.UpsertMessageRaw(derivedID,
		[]byte("Message-ID: <unrelated@example.test>\r\nSubject: Unrelated\r\n\r\nBody")))

	engine := dedup.NewEngine(f.Store, dedup.Config{
		AccountSourceIDs: []int64{f.Source.ID},
		Account:          f.Source.Identifier,
	}, nil)
	report, err := engine.Scan(t.Context())
	require.NoError(err)
	require.Equal(int64(1), report.PendingRFC822IDBackfill())
	require.Len(report.Groups, 1)

	summary, err := engine.Execute(t.Context(), report, "unchanged-plan")
	require.NoError(err)
	assert.Equal(int64(1), summary.RFC822IDsBackfilled)
	assert.Equal(1, summary.GroupsMerged)
	assertSoftDeleted(t, f.Store, winnerID, false)
	assertSoftDeleted(t, f.Store, loserID, true)

	var storedID sql.NullString
	err = f.Store.DB().QueryRow(
		f.Store.Rebind("SELECT rfc822_message_id FROM messages WHERE id = ?"),
		derivedID,
	).Scan(&storedID)
	require.NoError(err)
	assert.Equal(sql.NullString{String: "unrelated@example.test", Valid: true}, storedID)
}

func TestEngine_ExecuteStopsWhenRFC822BackfillPlanChanged(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	winnerID := addMessage(t, f.Store, f.Source, "winner-stale", "stable-stale@example.test", false)
	loserID := addMessage(t, f.Store, f.Source, "loser-stale", "stable-stale@example.test", false)
	derivedID := addMessage(t, f.Store, f.Source, "derived-stale", "", false)
	require.NoError(f.Store.UpsertMessageRaw(derivedID,
		[]byte("Message-ID: <derived-stale@example.test>\r\n\r\nBody")))

	engine := dedup.NewEngine(f.Store, dedup.Config{
		AccountSourceIDs: []int64{f.Source.ID},
		Account:          f.Source.Identifier,
	}, nil)
	report, err := engine.Scan(t.Context())
	require.NoError(err)
	require.Equal(int64(1), report.PendingRFC822IDBackfill())
	require.Len(report.Groups, 1)

	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET rfc822_message_id = ? WHERE id = ?`),
		"claimed@example.test", derivedID)
	require.NoError(err)

	summary, err := engine.Execute(t.Context(), report, "stale-backfill-plan")
	require.ErrorIs(err, store.ErrRFC822IDBackfillPlanChanged)
	assert.Zero(summary.GroupsMerged)
	assertSoftDeleted(t, f.Store, winnerID, false)
	assertSoftDeleted(t, f.Store, loserID, false)
}

func TestEngine_ExecuteAllowsGroupProvenanceChangeWhenDestructivePlanIsUnchanged(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	const stableRFC822ID = "z-stable@example.test"
	stableWinnerID := addMessage(t, f.Store, f.Source, "stable-winner", stableRFC822ID, false)
	stableLoserID := addMessage(t, f.Store, f.Source, "stable-loser", stableRFC822ID, false)
	const rfc822ID = "reclassified@example.test"
	raw := []byte("Message-ID: <" + rfc822ID + ">\r\nSubject: Same\r\n\r\nBody")
	winnerID := addMessage(t, f.Store, f.Source, "stored-id", rfc822ID, false)
	loserID := addMessage(t, f.Store, f.Source, "derivable-id", "", false)
	require.NoError(f.Store.UpsertMessageRaw(winnerID, raw))
	require.NoError(f.Store.UpsertMessageRaw(loserID, raw))

	engine := dedup.NewEngine(f.Store, dedup.Config{
		AccountSourceIDs:    []int64{f.Source.ID},
		Account:             f.Source.Identifier,
		ContentHashFallback: true,
	}, nil)
	report, err := engine.Scan(t.Context())
	require.NoError(err)
	require.Equal(int64(1), report.PendingRFC822IDBackfill())
	require.Len(report.Groups, 2)
	assert.Equal("message-id", report.Groups[0].KeyType)
	assert.Equal(stableRFC822ID, report.Groups[0].Key)
	initialGroup := report.Groups[1]
	assert.Equal("normalized-hash", initialGroup.KeyType)
	assert.Equal(winnerID, initialGroup.Messages[initialGroup.Survivor].ID)

	summary, err := engine.Execute(t.Context(), report, "reclassified-plan")
	require.NoError(err)
	assert.Equal(int64(1), summary.RFC822IDsBackfilled)
	assert.Equal(2, summary.GroupsMerged)
	assertSoftDeleted(t, f.Store, stableWinnerID, false)
	assertSoftDeleted(t, f.Store, stableLoserID, true)
	assertSoftDeleted(t, f.Store, winnerID, false)
	assertSoftDeleted(t, f.Store, loserID, true)
}

func TestEngine_ExecuteStopsBeforeMergeWhenBackfillRevealsDuplicate(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	const rfc822ID = "revealed@example.test"
	raw := []byte("Message-ID: <" + rfc822ID + ">\r\nSubject: Same\r\n\r\nBody")
	winnerID := addMessage(t, f.Store, f.Source, "persisted", "<"+rfc822ID+">", false)
	loserID := addMessage(t, f.Store, f.Source, "derivable", "", false)
	require.NoError(f.Store.UpsertMessageRaw(winnerID, raw))
	require.NoError(f.Store.UpsertMessageRaw(loserID, raw))

	deletionsDir := filepath.Join(t.TempDir(), "deletions")
	engine := dedup.NewEngine(f.Store, dedup.Config{
		AccountSourceIDs:           []int64{f.Source.ID},
		Account:                    f.Source.Identifier,
		DeleteDupsFromSourceServer: true,
		DeletionsDir:               deletionsDir,
	}, nil)
	report, err := engine.Scan(t.Context())
	require.NoError(err)
	require.Equal(int64(1), report.PendingRFC822IDBackfill())
	require.Empty(report.Groups)

	summary, err := engine.Execute(t.Context(), report, "changed-plan")
	require.Error(err)
	require.ErrorIs(err, dedup.ErrPlanChangedAfterRFC822Backfill)
	assert.Equal(int64(1), summary.RFC822IDsBackfilled)
	assert.Equal(0, summary.GroupsMerged)
	assertSoftDeleted(t, f.Store, winnerID, false)
	assertSoftDeleted(t, f.Store, loserID, false)

	var batched int
	err = f.Store.DB().QueryRow(
		"SELECT COUNT(*) FROM messages WHERE delete_batch_id IS NOT NULL",
	).Scan(&batched)
	require.NoError(err)
	assert.Equal(0, batched)

	mgr, err := deletion.NewManager(deletionsDir)
	require.NoError(err)
	pending, err := mgr.ListPending()
	require.NoError(err)
	assert.Empty(pending)

	refreshed, err := engine.Scan(t.Context())
	require.NoError(err)
	require.Len(refreshed.Groups, 1)
	group := refreshed.Groups[0]
	assert.Equal("message-id", group.KeyType)
	assert.Equal(rfc822ID, group.Key)
	assert.Equal(winnerID, group.Messages[group.Survivor].ID)
	var loserIDs []int64
	for i, message := range group.Messages {
		if i != group.Survivor {
			loserIDs = append(loserIDs, message.ID)
		}
	}
	assert.Equal([]int64{loserID}, loserIDs)
}

// scanStartCounter is a concurrency-safe slog.Handler that counts how many
// times the engine logged a scan start. Scan logs exactly one "dedup scan
// start" per invocation, so the count observes how many full scans ran
// without the test having to wrap the store.
type scanStartCounter struct {
	mu    sync.Mutex
	count int
}

func (h *scanStartCounter) Enabled(context.Context, slog.Level) bool { return true }

func (h *scanStartCounter) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "dedup scan start" {
		h.mu.Lock()
		h.count++
		h.mu.Unlock()
	}
	return nil
}

func (h *scanStartCounter) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *scanStartCounter) WithGroup(string) slog.Handler { return h }

func (h *scanStartCounter) scans() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.count
}

// TestEngine_ExecuteSkipsRescanWhenNoBackfillWasPlanned pins the fix for the
// review finding that Execute always ran a second full Scan after
// ApplyRFC822IDBackfill, even when the confirmed plan contained no RFC822
// Message-ID derivation items. Even when failed candidates were inspected,
// no items means the backfill commits nothing, so the confirmed report still
// describes the database and the rescan is pure overhead. The stale-plan
// safety check itself stays exercised, on the derivation path, by
// TestEngine_ExecuteBackfillsThenMergesWhenActionablePlanIsUnchanged and
// TestEngine_ExecuteStopsBeforeMergeWhenBackfillRevealsDuplicate.
func TestEngine_ExecuteSkipsRescanWhenNoBackfillWasPlanned(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	winnerID := addMessage(t, f.Store, f.Source, "winner", "skip-rescan@example.test", false)
	loserID := addMessage(t, f.Store, f.Source, "loser", "skip-rescan@example.test", false)
	malformedID := addMessage(t, f.Store, f.Source, "malformed-no-message-id", "", false)
	require.NoError(f.Store.UpsertMessageRaw(malformedID,
		[]byte("Subject: Missing Message-ID\r\n\r\nBody")))

	scans := &scanStartCounter{}
	engine := dedup.NewEngine(f.Store, dedup.Config{
		AccountSourceIDs: []int64{f.Source.ID},
		Account:          f.Source.Identifier,
	}, slog.New(scans))

	report, err := engine.Scan(t.Context())
	require.NoError(err, "Scan")
	require.Len(report.Groups, 1, "duplicate groups")
	require.Equal(int64(1), report.BackfillCandidates,
		"fixture must include a failed candidate but no applicable item")
	require.Equal(int64(1), report.BackfillFailed,
		"fixture must include a failed candidate but no applicable item")
	require.Equal(int64(0), report.PendingRFC822IDBackfill(),
		"fixture must plan no RFC822 derivation")

	require.Equal(1, scans.scans(), "the explicit Scan must be the only initial scan")
	summary, err := engine.Execute(t.Context(), report, "skip-rescan")
	require.NoError(err, "Execute")

	assert.Equal(1, scans.scans(),
		"Execute must not run a second full scan when no derivation was planned")
	assert.Equal(int64(0), summary.RFC822IDsBackfilled, "RFC822IDsBackfilled")
	assert.Equal(1, summary.GroupsMerged, "GroupsMerged")
	assertSoftDeleted(t, f.Store, winnerID, false)
	assertSoftDeleted(t, f.Store, loserID, true)
}

func TestEngine_FormatReportShowsBackfillCandidatesReadyAndFailures(t *testing.T) {
	assert := assert.New(t)
	f := storetest.New(t)
	engine := dedup.NewEngine(f.Store, dedup.Config{
		AccountSourceIDs: []int64{f.Source.ID},
	}, nil)

	report := &dedup.Report{
		RFC822IDsReady:     2,
		BackfillCandidates: 3,
		BackfillFailed:     1,
	}
	formatted := engine.FormatReport(report)
	assert.Contains(formatted,
		"3 messages with missing RFC822 Message-ID were inspected.")
	assert.Contains(formatted,
		"2 RFC822 Message-ID values are ready to be derived from stored MIME after confirmation.")
	assert.Contains(formatted,
		"1 message could not provide a usable Message-ID and will be skipped.")
	assert.NotContains(formatted, "Backfilled")

	malformedOnly := engine.FormatReport(&dedup.Report{
		BackfillCandidates: 1,
		BackfillFailed:     1,
	})
	assert.Contains(malformedOnly,
		"1 message with missing RFC822 Message-ID was inspected.")
	assert.Contains(malformedOnly,
		"1 message could not provide a usable Message-ID and will be skipped.")
	assert.NotContains(malformedOnly, "ready to be derived")
}

func TestEngine_PlanChangedErrorReportsCommittedDerivationAndNoBatch(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	const rfc822ID = "error-detail@example.test"
	raw := []byte("Message-ID: <" + rfc822ID + ">\r\nSubject: Same\r\n\r\nBody")
	persistedID := addMessage(t, f.Store, f.Source, "persisted-error", rfc822ID, false)
	derivedID := addMessage(t, f.Store, f.Source, "derived-error", "", false)
	require.NoError(f.Store.UpsertMessageRaw(persistedID, raw))
	require.NoError(f.Store.UpsertMessageRaw(derivedID, raw))

	engine := dedup.NewEngine(f.Store, dedup.Config{
		AccountSourceIDs:           []int64{f.Source.ID},
		Account:                    f.Source.Identifier,
		DeleteDupsFromSourceServer: true,
		DeletionsDir:               filepath.Join(t.TempDir(), "deletions"),
	}, nil)
	report, err := engine.Scan(t.Context())
	require.NoError(err)

	_, err = engine.Execute(t.Context(), report, "error-detail")
	require.Error(err)
	require.ErrorIs(err, dedup.ErrPlanChangedAfterRFC822Backfill)
	require.ErrorContains(err, "1 RFC822 Message-ID derivation was committed")
	require.ErrorContains(err,
		"no duplicate messages were hidden and no dedup batch was created; rerun deduplicate to review the updated plan")
}
