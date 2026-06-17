package dedup_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/dedup"
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/store"
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
	requirepkg.NoError(t, err, "EnsureConversation")
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
	requirepkg.NoError(t, err, "UpsertMessage")
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
	requirepkg.NoError(t, err, "query deleted_at")
	if wantDeleted {
		assertpkg.True(t, deletedAt.Valid, "message %d: deleted_at should be set", msgID)
	} else {
		assertpkg.False(t, deletedAt.Valid, "message %d: deleted_at should be NULL", msgID)
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
	requirepkg.NoError(t, err, "EnsureLabel "+sourceLabelID)
	requirepkg.NoError(t,
		st.LinkMessageLabel(msgID, lid),
		"LinkMessageLabel "+sourceLabelID,
	)
}

func TestEngine_Scan_UnionsLabelsOnSurvivor(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
			requirepkg.Error(t, err, "expected error for empty AccountSourceIDs")
			assertpkg.ErrorContains(t, err, "AccountSourceIDs must be non-empty")
		})
	}
}

func TestEngine_SurvivorFavorsSentCopy(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)
	st := f.Store
	gmail := f.Source

	idInbox := addMessage(t, st, gmail, "inbox-sent", "rfc-sent", false)
	idSent := addMessage(t, st, gmail, "sent-sent", "rfc-sent", true)

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
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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

	restored, stillExec, err := eng.Undo("batch-opt")
	require.NoError(err, "Undo")
	assert.Equal(int64(3), restored, "restored")
	assert.Empty(stillExec, "stillExec")
	pending, err = mgr.ListPending()
	require.NoError(err, "ListPending after undo")
	assert.Empty(pending, "pending after undo")
}

func TestEngine_ScopedToSingleSource_IgnoresCrossAccount(t *testing.T) {
	f := storetest.New(t)
	st := f.Store
	alice := f.Source

	bob, err := st.GetOrCreateSource("gmail", "bob@example.com")
	requirepkg.NoError(t, err, "GetOrCreateSource bob")

	addMessage(t, st, alice, "a-1", "rfc-cross", true)
	addMessage(t, st, bob, "b-1", "rfc-cross", false)

	eng := dedup.NewEngine(st, dedup.Config{
		AccountSourceIDs: []int64{alice.ID},
		Account:          "test@example.com",
	}, nil)
	report, err := eng.Scan(context.Background())
	requirepkg.NoError(t, err, "Scan")
	assertpkg.Equal(t, 0, report.DuplicateGroups, "cross-account dedup happened")
}

func TestEngine_ContentHashFallbackFindsNormalizedDuplicates(t *testing.T) {
	require := requirepkg.New(t)
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

// TestEngine_ContentHash_TwoMessageIDSurvivors_BothPreserved verifies the
// spec contract: "A content-hash group with two Message-ID survivors keeps
// both as winners (one per Message-ID group)."
//
// Four messages, two distinct RFC822 Message-IDs (two messages each). All
// four carry raw MIME that normalizes to the same content hash, so the
// content-hash pass would ordinarily group the two survivors together.
// The correct behaviour is to skip that content-hash group entirely —
// total losers must equal 2 (one per MID group), never 3.
func TestEngine_ContentHash_TwoMessageIDSurvivors_BothPreserved(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)
	st := f.Store
	gmail := f.Source

	// Two MID groups, two messages each.
	idA1 := addMessage(t, st, gmail, "src-a1", "mid-A", false)
	idA2 := addMessage(t, st, gmail, "src-a2", "mid-A", false)
	idB1 := addMessage(t, st, gmail, "src-b1", "mid-B", false)
	idB2 := addMessage(t, st, gmail, "src-b2", "mid-B", false)

	// All four messages share the same normalized content (stripped headers
	// differ, canonical From/Subject/Date/body are identical) so both
	// Message-ID survivors land in the same content-hash group.
	makeRaw := func(received, delivered, labels string) []byte {
		return []byte(
			"Received: " + received + "\r\n" +
				"Delivered-To: " + delivered + "\r\n" +
				"X-Gmail-Labels: " + labels + "\r\n" +
				"From: sender@example.com\r\n" +
				"Subject: Two MID survivors\r\n" +
				"Date: Mon, 1 Jan 2024 12:00:00 +0000\r\n" +
				"\r\n" +
				"Body that is identical across all four copies.",
		)
	}
	require.NoError(st.UpsertMessageRaw(idA1, makeRaw("mx1.google.com", "a1@example.com", "INBOX")), "raw A1")
	require.NoError(st.UpsertMessageRaw(idA2, makeRaw("mx2.google.com", "a2@example.com", "SENT")), "raw A2")
	require.NoError(st.UpsertMessageRaw(idB1, makeRaw("mx3.google.com", "b1@example.com", "INBOX")), "raw B1")
	require.NoError(st.UpsertMessageRaw(idB2, makeRaw("mx4.google.com", "b2@example.com", "SENT")), "raw B2")

	eng := dedup.NewEngine(st, dedup.Config{
		AccountSourceIDs:    []int64{gmail.ID},
		Account:             "test@example.com",
		ContentHashFallback: true,
	}, nil)

	report, err := eng.Scan(context.Background())
	require.NoError(err, "Scan")

	// Two MID groups, no content-hash group (the group with two MID
	// survivors must be skipped, not appended).
	assert.Equal(2, report.DuplicateGroups, "DuplicateGroups")
	assert.Equal(0, report.ContentHashGroups, "ContentHashGroups (MID-survivor group must be skipped)")
	// One loser per MID group; the buggy code yields 3 by demoting one
	// Message-ID survivor.
	assert.Equal(2, report.DuplicateMessages, "DuplicateMessages (one loser per MID group)")
}

// TestEngine_ContentHash_MIDSurvivorAndSentOrphan_SkipsGroup verifies that the
// content-hash pass does not demote a sent-copy orphan to a loser by forcing
// a non-sent Message-ID survivor to win the content-hash group. Per spec
// § Survivor selection: "When any message in a duplicate group looks like a
// sent copy, only sent copies are eligible to survive."
//
// Three messages: two share rfc822_message_id "mid-A" (neither is_from_me),
// one is a sent orphan (no Message-ID, is_from_me=true). All three carry raw
// MIME that normalizes to the same content hash. The MID-pass survivor would
// otherwise be forced in over the sent orphan; the new skip rule prevents
// that.
func TestEngine_ContentHash_MIDSurvivorAndSentOrphan_SkipsGroup(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)
	st := f.Store
	gmail := f.Source

	// MID group: two messages sharing mid-A, neither is_from_me.
	idA1 := addMessage(t, st, gmail, "src-a1", "mid-A", false)
	idA2 := addMessage(t, st, gmail, "src-a2", "mid-A", false)

	// Sent orphan: no MID, is_from_me=true. Content matches the MID group.
	idOrphan := addMessage(t, st, gmail, "src-orphan", "", true)

	makeRaw := func(received string) []byte {
		return []byte(
			"Received: " + received + "\r\n" +
				"From: sender@example.com\r\n" +
				"Subject: MID/sent-orphan collision\r\n" +
				"Date: Mon, 1 Jan 2024 12:00:00 +0000\r\n" +
				"\r\n" +
				"Identical body across all three copies.",
		)
	}
	require.NoError(st.UpsertMessageRaw(idA1, makeRaw("mx1.google.com")), "raw a1")
	require.NoError(st.UpsertMessageRaw(idA2, makeRaw("mx2.google.com")), "raw a2")
	require.NoError(st.UpsertMessageRaw(idOrphan, makeRaw("mx3.google.com")), "raw orphan")

	eng := dedup.NewEngine(st, dedup.Config{
		AccountSourceIDs:    []int64{gmail.ID},
		Account:             "test@example.com",
		ContentHashFallback: true,
	}, nil)

	report, err := eng.Scan(context.Background())
	require.NoError(err, "Scan")

	// Expect exactly one duplicate group (the MID group). The content-hash
	// group must be skipped to preserve the sent-copy eligibility filter.
	require.Equal(1, report.DuplicateGroups, "DuplicateGroups (only the MID group)")
	assert.Equal(0, report.ContentHashGroups, "ContentHashGroups (sent-orphan collision must be skipped)")
	// One loser from the MID group; the sent orphan stays live.
	assert.Equal(1, report.DuplicateMessages, "DuplicateMessages (one MID loser; orphan untouched)")

	// Per the spec audit recommendation, pin that the orphan did not leak
	// into the surviving MID group's Messages slice. The MID group must
	// contain only the two MID-sharing rows.
	mid := report.Groups[0]
	require.Equal("message-id", mid.KeyType, "Groups[0].KeyType")
	require.Len(mid.Messages, 2, "MID group Messages len")
	for _, m := range mid.Messages {
		assert.NotEqual(idOrphan, m.ID, "sent orphan id=%d leaked into MID group Messages — must stay out", idOrphan)
	}
}

func TestEngine_ContentHashFallbackDisabledByDefault(t *testing.T) {
	require := requirepkg.New(t)
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
	assertpkg.Contains(t,
		strings.ToLower(out),
		"never merges messages across different",
		"methodology missing cross-account guarantee",
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
	assertpkg.NotContains(t, lower, "cross-account dedup\n  is enabled",
		"single-member collection should not advertise cross-account dedup; got:\n%s", out)
	assertpkg.NotContains(t, lower, "intentionally merges messages",
		"single-member collection should not describe intentional cross-account merging; got:\n%s", out)
	assertpkg.Contains(t, lower, "never merges messages across different",
		"single-member collection should fall to the same-account guarantee; got:\n%s", out)
}

func TestEngine_SurvivorTiebreakers(t *testing.T) {
	t.Run("raw MIME wins over no raw MIME", func(t *testing.T) {
		require := requirepkg.New(t)
		f := storetest.New(t)
		st := f.Store

		idNoRaw := addMessage(t, st, f.Source, "no-raw", "rfc-raw-tie", false)
		idHasRaw := addMessage(t, st, f.Source, "has-raw", "rfc-raw-tie", false)
		require.NoError(st.UpsertMessageRaw(idHasRaw, []byte("Subject: test\r\n\r\nBody")),
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
		assertpkg.Equal(t, idHasRaw, survivor.ID, "survivor (has raw)")
		_ = idNoRaw
	})

	t.Run("more labels wins when raw MIME is equal", func(t *testing.T) {
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
		requirepkg.NoError(t, err, "Scan")
		requirepkg.Equal(t, 1, report.DuplicateGroups, "groups")
		survivor := report.Groups[0].Messages[report.Groups[0].Survivor]
		assertpkg.Equal(t, idMany, survivor.ID, "survivor (more labels)")
	})

	t.Run("lower ID wins as final tiebreaker", func(t *testing.T) {
		f := storetest.New(t)
		st := f.Store

		idFirst := addMessage(t, st, f.Source, "first", "rfc-id-tie", false)
		_ = addMessage(t, st, f.Source, "second", "rfc-id-tie", false)

		eng := dedup.NewEngine(st, dedup.Config{
			AccountSourceIDs: []int64{f.Source.ID},
			Account:          "test",
		}, nil)
		report, err := eng.Scan(context.Background())
		requirepkg.NoError(t, err, "Scan")
		requirepkg.Equal(t, 1, report.DuplicateGroups, "groups")
		survivor := report.Groups[0].Messages[report.Groups[0].Survivor]
		assertpkg.Equal(t, idFirst, survivor.ID, "survivor (lower ID)")
	})
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
	convID, err := st.EnsureConversation(
		source.ID, "thread-"+srcMsgID, "Subject",
	)
	requirepkg.NoError(t, err, "EnsureConversation")
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
	requirepkg.NoError(t, err, "UpsertMessage")
	if fromEmail != "" {
		pid, pErr := st.EnsureParticipant(fromEmail, "", "")
		requirepkg.NoError(t, pErr, "EnsureParticipant")
		requirepkg.NoError(t,
			st.ReplaceMessageRecipients(id, "from", []int64{pid}, []string{""}),
			"ReplaceMessageRecipients",
		)
	}
	return id
}

// TestEngine_PerSourceIdentity verifies that identity matching is per-source:
// an address confirmed only for source A does not count as "me" in source B.
func TestEngine_PerSourceIdentity(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
