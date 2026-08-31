package cmd

import (
	"context"
	"database/sql"
	"net"
	"strconv"
	"testing"
	"time"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// listedIMAPClient returns an IMAP client that has enumerated the test
// server, so it holds observed folder states ready for persistence.
func listedIMAPClient(t *testing.T, addr string, opts ...imaplib.Option) *imaplib.Client {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)

	client := imaplib.NewClient(&imaplib.Config{
		Host:     host,
		Port:     port,
		Username: testutil.IMAPTestUsername,
	}, testutil.IMAPTestPassword, opts...)
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pageToken := ""
	for {
		resp, err := client.ListMessages(ctx, "", pageToken)
		require.NoError(t, err)
		if resp.NextPageToken == "" {
			return client
		}
		pageToken = resp.NextPageToken
	}
}

func imapMembershipRowCount(t *testing.T, st *store.Store, sourceID int64) int {
	t.Helper()
	var count int
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM imap_message_memberships WHERE source_id = ?`,
	), sourceID).Scan(&count))
	return count
}

func imapTombstonedMessageCount(t *testing.T, st *store.Store, sourceID int64) int {
	t.Helper()
	var count int
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages
		 WHERE source_id = ? AND deleted_from_source_at IS NOT NULL`,
	), sourceID).Scan(&count))
	return count
}

func seedObservedIMAPMessages(
	t *testing.T, st *store.Store, src *store.Source, client *imaplib.Client,
) {
	t.Helper()
	conversationID, err := st.EnsureConversation(src.ID, "imap-membership", "IMAP membership")
	require.NoError(t, err)
	for _, observation := range client.ObservedMemberships() {
		_, err := st.UpsertMessage(&store.Message{
			ConversationID:  conversationID,
			SourceID:        src.ID,
			SourceMessageID: observation.SourceMessageID,
			RFC822MessageID: sql.NullString{
				String: observation.RFC822MessageID,
				Valid:  observation.RFC822MessageID != "",
			},
			MessageType: "email",
		})
		require.NoError(t, err)
	}
}

func seedIMAPMessage(
	t *testing.T, st *store.Store, src *store.Source, sourceMessageID, rfc822MessageID string,
) {
	t.Helper()
	conversationID, err := st.EnsureConversation(src.ID, "imap-delta", "IMAP delta")
	require.NoError(t, err)
	_, err = st.UpsertMessage(&store.Message{
		ConversationID:  conversationID,
		SourceID:        src.ID,
		SourceMessageID: sourceMessageID,
		RFC822MessageID: sql.NullString{String: rfc822MessageID, Valid: rfc822MessageID != ""},
		MessageType:     "email",
	})
	require.NoError(t, err)
}

func completedIMAPSyncSummary(
	t *testing.T, st *store.Store, src *store.Source,
) *gmail.SyncSummary {
	t.Helper()
	syncRunID, err := st.StartSync(src.ID, "full")
	require.NoError(t, err)
	require.NoError(t, st.CompleteSync(syncRunID, "0"))
	return &gmail.SyncSummary{SyncRunID: syncRunID}
}

func TestSaveIMAPFolderStates_CleanRunPersists(t *testing.T) {
	require := require.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2, "Archive": 1})
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)

	client := listedIMAPClient(t, addr)
	seedObservedIMAPMessages(t, st, src, client)
	require.NoError(saveIMAPFolderStates(
		context.Background(), st, src, client, completedIMAPSyncSummary(t, st, src), 0))

	loaded, err := loadIMAPFolderStates(st, src.ID)
	require.NoError(err)
	assert.Equal(t, map[string]imaplib.FolderState{
		"INBOX": {
			UIDValidity: loaded["INBOX"].UIDValidity, UIDNext: 3,
			HighestModSeq: loaded["INBOX"].HighestModSeq, KnownUIDs: []uint32{1, 2},
		},
		"Archive": {
			UIDValidity: loaded["Archive"].UIDValidity, UIDNext: 2,
			HighestModSeq: loaded["Archive"].HighestModSeq, KnownUIDs: []uint32{1},
		},
	}, loaded)
}

func TestSaveIMAPFolderStates_SupersededGenerationCannotOverwriteNewerRun(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 1})
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://generation@example.com")
	require.NoError(err)
	require.NoError(st.UpsertIMAPFolderStates(src.ID, []store.IMAPFolderState{{
		Mailbox: "Prior", UIDValidity: 7, UIDNext: 9, HighestModSeq: 11,
	}}))

	client := listedIMAPClient(t, addr)
	seedObservedIMAPMessages(t, st, src, client)
	oldRunID, err := st.StartSync(src.ID, "full")
	require.NoError(err)
	require.NoError(st.CompleteSync(oldRunID, "0"))
	newRunID, err := st.StartSync(src.ID, "full")
	require.NoError(err)

	err = saveIMAPFolderStates(
		context.Background(), st, src, client, &gmail.SyncSummary{SyncRunID: oldRunID}, 0)
	require.ErrorIs(err, store.ErrSyncRunSuperseded)

	loaded, err := loadIMAPFolderStates(st, src.ID)
	require.NoError(err)
	assert.Equal(map[string]imaplib.FolderState{
		"Prior": {
			UIDValidity: 7, UIDNext: 9, HighestModSeq: 11, KnownUIDs: []uint32{},
		},
	}, loaded)
	active, err := st.GetActiveSync(src.ID)
	require.NoError(err)
	assert.Equal(newRunID, active.ID)
}

func TestSaveIMAPFolderStates_ErrorsBlockPersistence(t *testing.T) {
	require := require.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2})
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)

	client := listedIMAPClient(t, addr)
	require.NoError(st.UpsertIMAPFolderStates(src.ID, []store.IMAPFolderState{{
		Mailbox: "prior", UIDValidity: 7, UIDNext: 9, HighestModSeq: 11,
	}}))
	require.NoError(saveIMAPFolderStates(
		context.Background(), st, src, client, &gmail.SyncSummary{Errors: 1}, 0))

	loaded, err := loadIMAPFolderStates(st, src.ID)
	require.NoError(err)
	assert.Equal(t, map[string]imaplib.FolderState{
		"prior": {UIDValidity: 7, UIDNext: 9, HighestModSeq: 11, KnownUIDs: []uint32{}},
	}, loaded, "a run with fetch errors must not advance folder high water marks")
}

func TestSaveIMAPFolderStates_InterruptionBlocksPersistence(t *testing.T) {
	require := require.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 1})
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)

	client := listedIMAPClient(t, addr)
	seedObservedIMAPMessages(t, st, src, client)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(saveIMAPFolderStates(ctx, st, src, client, &gmail.SyncSummary{}, 0))

	loaded, err := loadIMAPFolderStates(st, src.ID)
	require.NoError(err)
	assert.Empty(t, loaded, "an interrupted run must leave durable incremental state untouched")
}

func TestSaveIMAPFolderStates_ResumedRunBlocksPersistence(t *testing.T) {
	require := require.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 1})
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)

	client := listedIMAPClient(t, addr)
	seedObservedIMAPMessages(t, st, src, client)
	require.NoError(saveIMAPFolderStates(
		context.Background(), st, src, client, &gmail.SyncSummary{WasResumed: true}, 0))

	loaded, err := loadIMAPFolderStates(st, src.ID)
	require.NoError(err)
	assert.Empty(t, loaded, "a resumed run must not publish a partial membership snapshot")
}

func TestSaveIMAPFolderStates_LimitTruncationBlocksPersistence(t *testing.T) {
	require := require.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 5})
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)

	client := listedIMAPClient(t, addr)
	require.NoError(saveIMAPFolderStates(
		context.Background(), st, src, client, &gmail.SyncSummary{MessagesFound: 3}, 3))

	loaded, err := loadIMAPFolderStates(st, src.ID)
	require.NoError(err)
	assert.Empty(t, loaded, "a --limit-truncated run must not advance folder high water marks")
}

func TestSaveIMAPFolderStates_BelowLimitStillBlocksPersistence(t *testing.T) {
	require := require.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 1})
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)

	client := listedIMAPClient(t, addr)
	seedObservedIMAPMessages(t, st, src, client)
	require.NoError(saveIMAPFolderStates(
		context.Background(), st, src, client, &gmail.SyncSummary{MessagesFound: 1}, 3))

	loaded, err := loadIMAPFolderStates(st, src.ID)
	require.NoError(err)
	assert.Empty(t, loaded,
		"any nonzero --limit must leave durable incremental state untouched")
}

func TestSaveIMAPFolderStates_NonIMAPClientIsNoOp(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(err)

	var notIMAP gmail.API
	require.NoError(saveIMAPFolderStates(
		context.Background(), st, src, notIMAP, &gmail.SyncSummary{}, 0))

	loaded, err := loadIMAPFolderStates(st, src.ID)
	require.NoError(err)
	assert.Empty(t, loaded)
}

func TestIMAPFolderStateOptions_RoundTripSkipsUnchangedFolders(t *testing.T) {
	require := require.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2, "Archive": 3})
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)

	first := listedIMAPClient(t, addr)
	seedObservedIMAPMessages(t, st, src, first)
	require.NoError(saveIMAPFolderStates(
		context.Background(), st, src, first, completedIMAPSyncSummary(t, st, src), 0))
	require.NoError(first.Close())

	beforeMemberships := imapMembershipRowCount(t, st, src.ID)
	require.NotZero(beforeMemberships)

	opts := imapFolderStateOptions(st, src, false)
	require.NotEmpty(opts, "saved states must produce a client option")

	second := listedIMAPClient(t, addr, opts...)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := second.ListMessages(ctx, "", "")
	require.NoError(err)
	assert.Empty(t, resp.Messages,
		"a stored baseline plus a matching message count proves nothing changed")

	// The whole point of the skip is that it costs nothing durable: applying
	// the resulting deltas must leave every membership row and every message
	// exactly as the first sync left them.
	require.NoError(saveIMAPFolderStates(
		context.Background(), st, src, second, completedIMAPSyncSummary(t, st, src), 0))
	assert.Equal(t, beforeMemberships, imapMembershipRowCount(t, st, src.ID),
		"skipping a mailbox must not delete its memberships")
	assert.Zero(t, imapTombstonedMessageCount(t, st, src.ID),
		"skipping a mailbox must not tombstone its messages")
}

func TestIMAPFolderStateOptions_ForceRescanRetainsStatesAndEnumerates(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	addr, _ := testutil.StartIMAPMemServer(
		t, map[string]int{"INBOX": 1})
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource(
		"imap", "imap://alice@example.com")
	require.NoError(err)

	first := listedIMAPClient(t, addr)
	seedObservedIMAPMessages(t, st, src, first)
	require.NoError(saveIMAPFolderStates(
		context.Background(), st, src, first, completedIMAPSyncSummary(t, st, src), 0))
	require.NoError(first.Close())

	opts := imapFolderStateOptions(st, src, true)
	require.Len(opts, 3,
		"--noresume needs saved identity and alias state plus forced enumeration")

	second := listedIMAPClient(t, addr, opts...)
	ctx, cancel := context.WithTimeout(
		context.Background(), 30*time.Second)
	defer cancel()
	resp, err := second.ListMessages(ctx, "", "")
	require.NoError(err)
	assert.Len(resp.Messages, 1,
		"forced enumeration must not skip an unchanged mailbox")
}

func TestSaveIMAPFolderStates_UnmappableObservationRollsBackCursor(t *testing.T) {
	require := require.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 1})
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)

	client := listedIMAPClient(t, addr)
	err = saveIMAPFolderStates(
		context.Background(), st, src, client, completedIMAPSyncSummary(t, st, src), 0)
	require.Error(err)
	assert.Contains(t, err.Error(), "resolve IMAP membership")
	loaded, loadErr := loadIMAPFolderStates(st, src.ID)
	require.NoError(loadErr)
	assert.Empty(t, loaded)
}

func TestSaveIMAPFolderStates_StatusFailureBlocksDurableApply(t *testing.T) {
	require := require.New(t)
	addr, user := testutil.StartIMAPMemServerWithStatusError(
		t,
		map[string]int{
			"INBOX":            0,
			"[Gmail]/All Mail": 0,
		},
		map[string][]imapapi.MailboxAttr{
			"[Gmail]/All Mail": {imapapi.MailboxAttrAll},
		},
		"INBOX",
	)
	const messageID = "status-failure-store@example.com"
	testutil.AppendIMAPMessageWithMessageID(t, user, "INBOX", messageID)
	testutil.AppendIMAPMessageWithMessageID(t, user, "[Gmail]/All Mail", messageID)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)

	client := listedIMAPClient(t, addr)
	seedObservedIMAPMessages(t, st, src, client)
	require.NoError(saveIMAPFolderStates(
		context.Background(), st, src, client, &gmail.SyncSummary{}, 0))

	states, err := st.GetIMAPFolderStates(src.ID)
	require.NoError(err)
	assert.Empty(t, states)
	known, err := st.GetIMAPKnownUIDs(src.ID)
	require.NoError(err)
	assert.Empty(t, known)
	var membershipCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM imap_message_memberships WHERE source_id = ?
	`), src.ID).Scan(&membershipCount))
	assert.Zero(t, membershipCount)
}

func TestSaveIMAPFolderStates_StoreRoundTripValues(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)

	seedIMAPMessage(t, st, src, "INBOX|7", "round-trip@example.com")
	require.NoError(st.ApplyIMAPMailboxDeltas(src.ID, []store.IMAPMailboxDelta{{
		Mailbox: "INBOX",
		State: store.IMAPFolderState{
			Mailbox: "INBOX", UIDValidity: 42, UIDNext: 100, HighestModSeq: 123456789,
		},
		Memberships: []store.IMAPMembershipObservation{{
			Mailbox: "INBOX", UIDValidity: 42, UID: 7, SourceMessageID: "INBOX|7",
		}},
	}}))
	loaded, err := loadIMAPFolderStates(st, src.ID)
	require.NoError(err)
	assert.Equal(t, map[string]imaplib.FolderState{
		"INBOX": {
			UIDValidity: 42, UIDNext: 100, HighestModSeq: 123456789, KnownUIDs: []uint32{7},
		},
	}, loaded)
}

func TestApplyIMAPMailboxDeltas_ConvertsObservationsAndVanishedUIDs(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)
	seedIMAPMessage(t, st, src, "INBOX|1", "removed@example.com")
	seedIMAPMessage(t, st, src, "INBOX|2", "retained@example.com")
	seedIMAPMessage(t, st, src, "INBOX|3", "added@example.com")
	require.NoError(st.ApplyIMAPMailboxDeltas(src.ID, []store.IMAPMailboxDelta{{
		Mailbox: "INBOX",
		State: store.IMAPFolderState{
			Mailbox: "INBOX", UIDValidity: 17, UIDNext: 3, HighestModSeq: 100,
		},
		Memberships: []store.IMAPMembershipObservation{
			{UID: 1, SourceMessageID: "INBOX|1"},
			{UID: 2, SourceMessageID: "INBOX|2"},
		},
	}}))

	summary := completedIMAPSyncSummary(t, st, src)
	err = applyIMAPMailboxDeltas(context.Background(), st, src, summary.SyncRunID, []imaplib.MailboxDelta{{
		Mailbox: "INBOX",
		State: imaplib.FolderState{
			UIDValidity: 17, UIDNext: 4, HighestModSeq: 101, KnownUIDs: []uint32{2, 3},
		},
		ChangedUIDs:  []imapapi.UID{3},
		VanishedUIDs: []imapapi.UID{1},
		Incremental:  true,
	}}, []imaplib.MembershipObservation{{
		Mailbox: "INBOX", UIDValidity: 17, UID: 3,
		SourceMessageID: "INBOX|3", RFC822MessageID: "added@example.com",
		Flags: []string{"\\Flagged"},
	}})
	require.NoError(err)

	known, err := st.GetIMAPKnownUIDs(src.ID)
	require.NoError(err)
	assert.Equal(t, map[string][]uint32{"INBOX": {2, 3}}, known)
	states, err := st.GetIMAPFolderStates(src.ID)
	require.NoError(err)
	assert.Equal(t, []store.IMAPFolderState{{
		Mailbox: "INBOX", UIDValidity: 17, UIDNext: 4, HighestModSeq: 101,
	}}, states)
}

// TestSaveIMAPFolderStates_MessageGoneMidRunStillPersists is the durable half
// of the expunge race. The sync treats a message that left the mailbox between
// enumeration and fetch as handled, so the run finishes without errors and
// reaches this commit. Nothing ingested that message, so its enumeration
// observation would find no message row to resolve and would roll back every
// mailbox cursor in the source.
func TestSaveIMAPFolderStates_MessageGoneMidRunStillPersists(t *testing.T) {
	require := require.New(t)
	addr, _, hideUID := testutil.StartIMAPMemServerWithMissingUID(
		t, map[string]int{"INBOX": 2}, "INBOX", imapapi.UID(2))
	hideUID(true)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)

	client := listedIMAPClient(t, addr)
	results, err := client.GetMessageLabelsBatch(
		context.Background(), []string{"INBOX|1", "INBOX|2"})
	require.NoError(err)
	require.Len(results, 2)
	require.NoError(results[0].Err)
	require.ErrorIs(results[1].Err, gmail.ErrMessageGone)

	// Only the survivor was ingested, which is what the run itself would have
	// stored: the message that left the mailbox was acknowledged, never fetched.
	seedIMAPMessage(t, st, src, "INBOX|1", "")

	require.NoError(saveIMAPFolderStates(
		context.Background(), st, src, client, completedIMAPSyncSummary(t, st, src), 0))

	loaded, err := loadIMAPFolderStates(st, src.ID)
	require.NoError(err)
	require.Contains(loaded, "INBOX")
	assert.Equal(t, []uint32{1}, loaded["INBOX"].KnownUIDs)
}

// TestSaveIMAPFolderStates_LiveMessageWithoutHeadersBlocksPersistence is the
// other side of that line. The server returned the UID, so the message is
// still in the mailbox and only its headers are missing. Acknowledging it
// would drop a live message from the authoritative snapshot, so it stays a
// fetch error and the commit does not run.
func TestSaveIMAPFolderStates_LiveMessageWithoutHeadersBlocksPersistence(t *testing.T) {
	require := require.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2})
	client := listedIMAPClient(t, addr)
	// The in-memory server answers a FETCH of a UID expunged by another
	// session with an empty body rather than leaving it out of the response.
	testutil.ExpungeIMAPMessage(t, addr, "INBOX", imapapi.UID(2))
	results, err := client.GetMessageLabelsBatch(
		context.Background(), []string{"INBOX|1", "INBOX|2"})
	require.NoError(err)
	require.Len(results, 2)
	require.Error(results[1].Err)
	assert.NotErrorIs(t, results[1].Err, gmail.ErrMessageGone)
}

// syncedIMAPClient enumerates the server the way a run does and refreshes the
// labels of every message it listed, so the client ends holding the membership
// observations the commit will publish.
func syncedIMAPClient(
	t *testing.T, addr string, opts ...imaplib.Option,
) (*imaplib.Client, []gmail.MessageLabelsBatchResult) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client := listedIMAPClient(t, addr, opts...)
	var ids []string
	pageToken := ""
	for {
		resp, err := client.ListMessages(ctx, "", pageToken)
		require.NoError(t, err)
		for _, msg := range resp.Messages {
			ids = append(ids, msg.ID)
		}
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	if len(ids) == 0 {
		return client, nil
	}
	results, err := client.GetMessageLabelsBatch(ctx, ids)
	require.NoError(t, err)
	return client, results
}

// TestSaveIMAPFolderStates_RepublishMissingUIDRecoversNextRun measures what
// treating an omitted UID as gone costs on a republish, rather than arguing
// about it.
//
// A republish keeps only what the run read: ApplyIMAPMailboxDeltas answers a
// Reset delta by deleting every membership row for the mailbox and
// re-inserting the observations. A live UID the server failed to return is
// therefore deleted, and tombstoned when that was its last mailbox.
//
// This test does not claim the reading is right. It pins the bound: the cost
// is one cycle, the message row is never deleted, and the next run restores
// both the membership and the message. If run three ever fails, the reading is
// not reversible and the sentinel needs a revalidating fetch behind it.
func TestSaveIMAPFolderStates_RepublishMissingUIDRecoversNextRun(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	addr, _, hideUID := testutil.StartIMAPMemServerWithMissingUID(
		t, map[string]int{"INBOX": 2}, "INBOX", imapapi.UID(2))
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)

	// Run one: the healthy baseline. No saved state, so the mailbox is
	// republished and both messages are stored.
	first, firstLabels := syncedIMAPClient(t, addr)
	require.Len(firstLabels, 2)
	require.NoError(firstLabels[0].Err)
	require.NoError(firstLabels[1].Err)
	seedObservedIMAPMessages(t, st, src, first)
	require.NoError(saveIMAPFolderStates(
		ctx, st, src, first, completedIMAPSyncSummary(t, st, src), 0))
	require.NoError(first.Close())
	require.Equal(2, imapMembershipRowCount(t, st, src.ID))
	require.Zero(imapTombstonedMessageCount(t, st, src.ID))

	// Run two: another republish, with the server leaving UID 2 out of every
	// FETCH response. The run treats it as gone and finishes without errors,
	// so the commit proceeds -- which on main it never did.
	hideUID(true)
	second, secondLabels := syncedIMAPClient(t, addr)
	require.Len(secondLabels, 2)
	require.ErrorIs(secondLabels[1].Err, gmail.ErrMessageGone)
	seedObservedIMAPMessages(t, st, src, second)
	require.NoError(saveIMAPFolderStates(
		ctx, st, src, second, completedIMAPSyncSummary(t, st, src), 0))
	require.NoError(second.Close())

	assert.Equal(1, imapMembershipRowCount(t, st, src.ID),
		"a republish deletes the membership of a UID it did not read back")
	assert.Equal(1, imapTombstonedMessageCount(t, st, src.ID),
		"losing its last mailbox tombstones the message -- this is the cost")

	// Run three: the server stops hiding the UID. The saved baseline is one
	// row short of the server's message count, so the mailbox cannot be
	// skipped, and reading it again restores both the membership and the
	// message.
	hideUID(false)
	third, thirdLabels := syncedIMAPClient(t, addr, imapFolderStateOptions(st, src, false)...)
	// The recovery is the count check, not another republish. The saved
	// baseline holds one UID and the server reports two, so the mailbox is
	// read again and the one missing UID is diffed back in.
	thirdDeltas := third.ObservedMailboxDeltas()
	require.Len(thirdDeltas, 1)
	assert.False(thirdDeltas[0].Reset,
		"the mailbox recovers by diff, not by republishing it again")
	assert.Equal([]imapapi.UID{2}, thirdDeltas[0].ChangedUIDs)
	seedObservedIMAPMessages(t, st, src, third)
	require.NoError(saveIMAPFolderStates(
		ctx, st, src, third, completedIMAPSyncSummary(t, st, src), 0))
	require.NoError(third.Close())
	for _, result := range thirdLabels {
		require.NoError(result.Err)
	}

	assert.Equal(2, imapMembershipRowCount(t, st, src.ID),
		"the next run must read the mailbox again and restore the membership")
	assert.Zero(imapTombstonedMessageCount(t, st, src.ID),
		"a message that regains a mailbox must lose its tombstone")
}
