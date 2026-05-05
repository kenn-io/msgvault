package dedup_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wesm/msgvault/internal/dedup"
	"github.com/wesm/msgvault/internal/deletion"
	"github.com/wesm/msgvault/internal/store"
	"github.com/wesm/msgvault/internal/testutil"
	"github.com/wesm/msgvault/internal/testutil/storetest"
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
	testutil.MustNoErr(t, err, "EnsureConversation")
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
	testutil.MustNoErr(t, err, "UpsertMessage")
	return id
}

func assertSoftDeleted(
	t *testing.T, st *store.Store, msgID int64, wantDeleted bool,
) {
	t.Helper()
	var deletedAt sql.NullTime
	err := st.DB().QueryRow(
		"SELECT deleted_at FROM messages WHERE id = ?", msgID,
	).Scan(&deletedAt)
	testutil.MustNoErr(t, err, "query deleted_at")
	if wantDeleted && !deletedAt.Valid {
		t.Errorf("message %d: deleted_at should be set", msgID)
	}
	if !wantDeleted && deletedAt.Valid {
		t.Errorf("message %d: deleted_at should be NULL", msgID)
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
	testutil.MustNoErr(t, err, "EnsureLabel "+sourceLabelID)
	testutil.MustNoErr(t,
		st.LinkMessageLabel(msgID, lid),
		"LinkMessageLabel "+sourceLabelID,
	)
}

func TestEngine_Scan_UnionsLabelsOnSurvivor(t *testing.T) {
	f := storetest.New(t)
	st := f.Store
	gmail := f.Source

	mbox, err := st.GetOrCreateSource("mbox", "test@example.com-mbox")
	testutil.MustNoErr(t, err, "GetOrCreateSource mbox")

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
	testutil.MustNoErr(t, err, "Scan")
	if report.DuplicateGroups != 1 {
		t.Fatalf("groups = %d, want 1", report.DuplicateGroups)
	}
	if report.DuplicateMessages != 1 {
		t.Fatalf("prune count = %d, want 1", report.DuplicateMessages)
	}

	group := report.Groups[0]
	survivor := group.Messages[group.Survivor]
	if survivor.ID != idGmail {
		t.Errorf("survivor = %d, want %d (gmail)", survivor.ID, idGmail)
	}

	summary, err := eng.Execute(
		context.Background(), report, "batch-union",
	)
	testutil.MustNoErr(t, err, "Execute")
	if summary.GroupsMerged != 1 {
		t.Errorf("groupsMerged = %d, want 1", summary.GroupsMerged)
	}

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
			if err == nil {
				t.Fatal("expected error for empty AccountSourceIDs")
			}
			if !strings.Contains(err.Error(), "AccountSourceIDs must be non-empty") {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestEngine_SurvivorFavorsSentCopy(t *testing.T) {
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
	testutil.MustNoErr(t, err, "Scan")
	if report.DuplicateGroups != 1 {
		t.Fatalf("groups = %d, want 1", report.DuplicateGroups)
	}

	group := report.Groups[0]
	survivor := group.Messages[group.Survivor]
	if survivor.ID != idSent {
		t.Errorf("survivor = %d, want sent copy %d",
			survivor.ID, idSent)
	}
	if !survivor.IsSentCopy() {
		t.Errorf("survivor should be a sent copy")
	}
}

func TestEngine_DefaultConfig_NeverStagesRemote(t *testing.T) {
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
	testutil.MustNoErr(t, err, "Scan")
	summary, err := eng.Execute(
		context.Background(), report, "batch-default",
	)
	testutil.MustNoErr(t, err, "Execute")

	if summary.MessagesRemoved != 1 {
		t.Errorf("messagesRemoved = %d, want 1", summary.MessagesRemoved)
	}
	if len(summary.StagedManifests) != 0 {
		t.Errorf("stagedManifests = %d, want 0", len(summary.StagedManifests))
	}

	mgr, err := deletion.NewManager(deletionsDir)
	testutil.MustNoErr(t, err, "NewManager")
	pending, err := mgr.ListPending()
	testutil.MustNoErr(t, err, "ListPending")
	if len(pending) != 0 {
		t.Errorf("pending manifests = %d, want 0", len(pending))
	}
}

func TestEngine_OptIn_StagesOnlyWithinSameSourceID(t *testing.T) {
	f := storetest.New(t)
	st := f.Store
	gmail := f.Source

	otherGmail, err := st.GetOrCreateSource("gmail", "other@example.com")
	testutil.MustNoErr(t, err, "GetOrCreateSource otherGmail")
	mbox, err := st.GetOrCreateSource("mbox", "local.mbox")
	testutil.MustNoErr(t, err, "GetOrCreateSource mbox")

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
	testutil.MustNoErr(t, err, "Scan")
	summary, err := eng.Execute(
		context.Background(), report, "batch-opt",
	)
	testutil.MustNoErr(t, err, "Execute")

	if summary.MessagesRemoved != 3 {
		t.Errorf("messagesRemoved = %d, want 3", summary.MessagesRemoved)
	}
	assertSoftDeleted(t, st, idWinner, false)
	assertSoftDeleted(t, st, idLoser, true)
	assertSoftDeleted(t, st, idOther, true)
	assertSoftDeleted(t, st, idMbox, true)

	if len(summary.StagedManifests) != 1 {
		t.Fatalf("stagedManifests = %d, want 1", len(summary.StagedManifests))
	}
	sm := summary.StagedManifests[0]
	if sm.Account != "test@example.com" {
		t.Errorf("staged account = %q, want test@example.com", sm.Account)
	}
	if sm.MessageCount != 1 {
		t.Errorf("staged count = %d, want 1", sm.MessageCount)
	}

	mgr, err := deletion.NewManager(deletionsDir)
	testutil.MustNoErr(t, err, "NewManager")
	pending, err := mgr.ListPending()
	testutil.MustNoErr(t, err, "ListPending")
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(pending))
	}
	if len(pending[0].GmailIDs) != 1 || pending[0].GmailIDs[0] != "g-2" {
		t.Errorf("manifest GmailIDs = %v, want [g-2]", pending[0].GmailIDs)
	}

	restored, stillExec, err := eng.Undo("batch-opt")
	testutil.MustNoErr(t, err, "Undo")
	if restored != 3 {
		t.Errorf("restored = %d, want 3", restored)
	}
	if len(stillExec) != 0 {
		t.Errorf("stillExec = %v, want empty", stillExec)
	}
	pending, err = mgr.ListPending()
	testutil.MustNoErr(t, err, "ListPending after undo")
	if len(pending) != 0 {
		t.Errorf("pending after undo = %d, want 0", len(pending))
	}
}

func TestEngine_ScopedToSingleSource_IgnoresCrossAccount(t *testing.T) {
	f := storetest.New(t)
	st := f.Store
	alice := f.Source

	bob, err := st.GetOrCreateSource("gmail", "bob@example.com")
	testutil.MustNoErr(t, err, "GetOrCreateSource bob")

	addMessage(t, st, alice, "a-1", "rfc-cross", true)
	addMessage(t, st, bob, "b-1", "rfc-cross", false)

	eng := dedup.NewEngine(st, dedup.Config{
		AccountSourceIDs: []int64{alice.ID},
		Account:          "test@example.com",
	}, nil)
	report, err := eng.Scan(context.Background())
	testutil.MustNoErr(t, err, "Scan")
	if report.DuplicateGroups != 0 {
		t.Errorf("cross-account dedup happened: groups = %d",
			report.DuplicateGroups)
	}
}

func TestEngine_ContentHashFallbackFindsNormalizedDuplicates(t *testing.T) {
	f := storetest.New(t)
	st := f.Store
	gmail := f.Source

	mbox, err := st.GetOrCreateSource("mbox", "test@example.com-mbox")
	testutil.MustNoErr(t, err, "GetOrCreateSource mbox")

	id1 := addMessage(t, st, gmail, "hash-1", "", false)
	id2 := addMessage(t, st, mbox, "hash-2", "", false)

	raw1 := []byte("Received: from mx1.google.com\r\nDelivered-To: one@example.com\r\nX-Gmail-Labels: INBOX\r\nFrom: sender@example.com\r\nSubject: Meeting tomorrow\r\nDate: Mon, 1 Jan 2024 12:00:00 +0000\r\n\r\nLet's meet tomorrow at 3pm.")
	raw2 := []byte("Received: from mx2.google.com\r\nDelivered-To: two@example.com\r\nX-Gmail-Labels: SENT\r\nAuthentication-Results: spf=pass\r\nFrom: sender@example.com\r\nSubject: Meeting tomorrow\r\nDate: Mon, 1 Jan 2024 12:00:00 +0000\r\n\r\nLet's meet tomorrow at 3pm.")
	testutil.MustNoErr(t, st.UpsertMessageRaw(id1, raw1), "UpsertMessageRaw id1")
	testutil.MustNoErr(t, st.UpsertMessageRaw(id2, raw2), "UpsertMessageRaw id2")

	eng := dedup.NewEngine(st, dedup.Config{
		AccountSourceIDs:    []int64{gmail.ID, mbox.ID},
		Account:             "test@example.com",
		ContentHashFallback: true,
	}, nil)

	report, err := eng.Scan(context.Background())
	testutil.MustNoErr(t, err, "Scan")
	if report.DuplicateGroups != 1 {
		t.Fatalf("groups = %d, want 1", report.DuplicateGroups)
	}
	if report.ContentHashGroups != 1 {
		t.Fatalf("contentHashGroups = %d, want 1", report.ContentHashGroups)
	}
	if got := report.Groups[0].KeyType; got != "normalized-hash" {
		t.Fatalf("keyType = %q, want normalized-hash", got)
	}
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
	testutil.MustNoErr(t, st.UpsertMessageRaw(idA1, makeRaw("mx1.google.com", "a1@example.com", "INBOX")), "raw A1")
	testutil.MustNoErr(t, st.UpsertMessageRaw(idA2, makeRaw("mx2.google.com", "a2@example.com", "SENT")), "raw A2")
	testutil.MustNoErr(t, st.UpsertMessageRaw(idB1, makeRaw("mx3.google.com", "b1@example.com", "INBOX")), "raw B1")
	testutil.MustNoErr(t, st.UpsertMessageRaw(idB2, makeRaw("mx4.google.com", "b2@example.com", "SENT")), "raw B2")

	eng := dedup.NewEngine(st, dedup.Config{
		AccountSourceIDs:    []int64{gmail.ID},
		Account:             "test@example.com",
		ContentHashFallback: true,
	}, nil)

	report, err := eng.Scan(context.Background())
	testutil.MustNoErr(t, err, "Scan")

	// Two MID groups, no content-hash group (the group with two MID
	// survivors must be skipped, not appended).
	if report.DuplicateGroups != 2 {
		t.Errorf("DuplicateGroups = %d, want 2", report.DuplicateGroups)
	}
	if report.ContentHashGroups != 0 {
		t.Errorf("ContentHashGroups = %d, want 0 (MID-survivor group must be skipped)", report.ContentHashGroups)
	}
	// One loser per MID group; the buggy code yields 3 by demoting one
	// Message-ID survivor.
	if report.DuplicateMessages != 2 {
		t.Errorf("DuplicateMessages = %d, want 2 (one loser per MID group)", report.DuplicateMessages)
	}
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
	testutil.MustNoErr(t, st.UpsertMessageRaw(idA1, makeRaw("mx1.google.com")), "raw a1")
	testutil.MustNoErr(t, st.UpsertMessageRaw(idA2, makeRaw("mx2.google.com")), "raw a2")
	testutil.MustNoErr(t, st.UpsertMessageRaw(idOrphan, makeRaw("mx3.google.com")), "raw orphan")

	eng := dedup.NewEngine(st, dedup.Config{
		AccountSourceIDs:    []int64{gmail.ID},
		Account:             "test@example.com",
		ContentHashFallback: true,
	}, nil)

	report, err := eng.Scan(context.Background())
	testutil.MustNoErr(t, err, "Scan")

	// Expect exactly one duplicate group (the MID group). The content-hash
	// group must be skipped to preserve the sent-copy eligibility filter.
	if report.DuplicateGroups != 1 {
		t.Fatalf("DuplicateGroups = %d, want 1 (only the MID group)", report.DuplicateGroups)
	}
	if report.ContentHashGroups != 0 {
		t.Errorf("ContentHashGroups = %d, want 0 (sent-orphan collision must be skipped)", report.ContentHashGroups)
	}
	// One loser from the MID group; the sent orphan stays live.
	if report.DuplicateMessages != 1 {
		t.Errorf("DuplicateMessages = %d, want 1 (one MID loser; orphan untouched)", report.DuplicateMessages)
	}

	// Per the spec audit recommendation, pin that the orphan did not leak
	// into the surviving MID group's Messages slice. The MID group must
	// contain only the two MID-sharing rows.
	mid := report.Groups[0]
	if mid.KeyType != "message-id" {
		t.Fatalf("Groups[0].KeyType = %q, want \"message-id\"", mid.KeyType)
	}
	if len(mid.Messages) != 2 {
		t.Fatalf("MID group Messages len = %d, want 2", len(mid.Messages))
	}
	for _, m := range mid.Messages {
		if m.ID == idOrphan {
			t.Errorf("sent orphan id=%d leaked into MID group Messages — must stay out", idOrphan)
		}
	}
}

func TestEngine_ContentHashFallbackDisabledByDefault(t *testing.T) {
	f := storetest.New(t)
	st := f.Store
	gmail := f.Source

	mbox, err := st.GetOrCreateSource("mbox", "test@example.com-mbox")
	testutil.MustNoErr(t, err, "GetOrCreateSource mbox")

	id1 := addMessage(t, st, gmail, "hash-off-1", "", false)
	id2 := addMessage(t, st, mbox, "hash-off-2", "", false)
	raw := []byte("Subject: No Message-ID\r\n\r\nIdentical body")
	testutil.MustNoErr(t, st.UpsertMessageRaw(id1, raw), "UpsertMessageRaw id1")
	testutil.MustNoErr(t, st.UpsertMessageRaw(id2, raw), "UpsertMessageRaw id2")

	eng := dedup.NewEngine(st, dedup.Config{
		AccountSourceIDs: []int64{gmail.ID, mbox.ID},
		Account:          "test@example.com",
	}, nil)

	report, err := eng.Scan(context.Background())
	testutil.MustNoErr(t, err, "Scan")
	if report.DuplicateGroups != 0 {
		t.Fatalf("groups = %d, want 0", report.DuplicateGroups)
	}
}

func TestEngine_FormatMethodology_MentionsSentPolicy(t *testing.T) {
	f := storetest.New(t)
	eng := dedup.NewEngine(f.Store, dedup.Config{
		Account:          "test@example.com",
		AccountSourceIDs: []int64{f.Source.ID},
	}, nil)
	out := eng.FormatMethodology()
	if !strings.Contains(
		strings.ToLower(out),
		"never merges messages across different",
	) {
		t.Errorf("methodology missing cross-account guarantee")
	}
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
	if strings.Contains(lower, "cross-account dedup\n  is enabled") {
		t.Errorf("single-member collection should not advertise cross-account dedup; got:\n%s", out)
	}
	if strings.Contains(lower, "intentionally merges messages") {
		t.Errorf("single-member collection should not describe intentional cross-account merging; got:\n%s", out)
	}
	if !strings.Contains(lower, "never merges messages across different") {
		t.Errorf("single-member collection should fall to the same-account guarantee; got:\n%s", out)
	}
}

func TestEngine_SurvivorTiebreakers(t *testing.T) {
	t.Run("raw MIME wins over no raw MIME", func(t *testing.T) {
		f := storetest.New(t)
		st := f.Store

		idNoRaw := addMessage(t, st, f.Source, "no-raw", "rfc-raw-tie", false)
		idHasRaw := addMessage(t, st, f.Source, "has-raw", "rfc-raw-tie", false)
		testutil.MustNoErr(t,
			st.UpsertMessageRaw(idHasRaw, []byte("Subject: test\r\n\r\nBody")),
			"UpsertMessageRaw",
		)

		eng := dedup.NewEngine(st, dedup.Config{
			AccountSourceIDs: []int64{f.Source.ID},
			Account:          "test",
		}, nil)
		report, err := eng.Scan(context.Background())
		testutil.MustNoErr(t, err, "Scan")
		if report.DuplicateGroups != 1 {
			t.Fatalf("groups = %d, want 1", report.DuplicateGroups)
		}
		survivor := report.Groups[0].Messages[report.Groups[0].Survivor]
		if survivor.ID != idHasRaw {
			t.Errorf("survivor = %d, want %d (has raw)", survivor.ID, idHasRaw)
		}
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
		testutil.MustNoErr(t, err, "Scan")
		if report.DuplicateGroups != 1 {
			t.Fatalf("groups = %d, want 1", report.DuplicateGroups)
		}
		survivor := report.Groups[0].Messages[report.Groups[0].Survivor]
		if survivor.ID != idMany {
			t.Errorf("survivor = %d, want %d (more labels)", survivor.ID, idMany)
		}
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
		testutil.MustNoErr(t, err, "Scan")
		if report.DuplicateGroups != 1 {
			t.Fatalf("groups = %d, want 1", report.DuplicateGroups)
		}
		survivor := report.Groups[0].Messages[report.Groups[0].Survivor]
		if survivor.ID != idFirst {
			t.Errorf("survivor = %d, want %d (lower ID)", survivor.ID, idFirst)
		}
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
	testutil.MustNoErr(t, err, "EnsureConversation")
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
	testutil.MustNoErr(t, err, "UpsertMessage")
	if fromEmail != "" {
		pid, pErr := st.EnsureParticipant(fromEmail, "", "")
		testutil.MustNoErr(t, pErr, "EnsureParticipant")
		testutil.MustNoErr(t,
			st.ReplaceMessageRecipients(id, "from", []int64{pid}, []string{""}),
			"ReplaceMessageRecipients",
		)
	}
	return id
}

// TestEngine_PerSourceIdentity verifies that identity matching is per-source:
// an address confirmed only for source A does not count as "me" in source B.
func TestEngine_PerSourceIdentity(t *testing.T) {
	f := storetest.New(t)
	st := f.Store
	sourceA := f.Source // already created by storetest.New

	sourceB, err := st.GetOrCreateSource("mbox", "bob@example.com-mbox")
	testutil.MustNoErr(t, err, "GetOrCreateSource sourceB")

	const me = "me@personal.com"
	const rfc = "rfc-identity-perscource"

	// Add me@personal.com as confirmed identity only for source A.
	testutil.MustNoErr(t,
		st.AddAccountIdentity(sourceA.ID, me, "test"),
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
	testutil.MustNoErr(t, err, "Scan")
	if report.DuplicateGroups != 1 {
		t.Fatalf("groups = %d, want 1", report.DuplicateGroups)
	}

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

	if !msgA.MatchedIdentity {
		t.Errorf("source A copy: MatchedIdentity = false, want true")
	}
	if msgB.MatchedIdentity {
		t.Errorf("source B copy: MatchedIdentity = true, want false (identity not confirmed for source B)")
	}

	// Survivor should be the source A copy because it is the sent copy.
	survivor := group.Messages[group.Survivor]
	if survivor.ID != idA {
		t.Errorf("survivor = %d (%s), want %d (source A, matched identity)",
			survivor.ID, survivor.SourceIdentifier, idA)
	}
}
