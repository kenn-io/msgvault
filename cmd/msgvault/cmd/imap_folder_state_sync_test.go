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
// mailbox cursor in the source — the outcome the fix exists to prevent.
func TestSaveIMAPFolderStates_MessageGoneMidRunStillPersists(t *testing.T) {
	require := require.New(t)
	addr, _ := testutil.StartIMAPMemServer(t, map[string]int{"INBOX": 2})
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("imap", "imap://alice@example.com")
	require.NoError(err)

	client := listedIMAPClient(t, addr)
	testutil.ExpungeIMAPMessage(t, addr, "INBOX", imapapi.UID(2))
	results, err := client.GetMessageLabelsBatch(
		context.Background(), []string{"INBOX|1", "INBOX|2"})
	require.NoError(err)
	require.Len(results, 2)
	require.NoError(results[0].Err)
	require.ErrorIs(results[1].Err, gmail.ErrMessageGone)

	// Only the survivor was ingested, which is what the run itself would have
	// stored: the expunged message was acknowledged, never fetched.
	seedIMAPMessage(t, st, src, "INBOX|1", "")

	require.NoError(saveIMAPFolderStates(
		context.Background(), st, src, client, completedIMAPSyncSummary(t, st, src), 0))

	loaded, err := loadIMAPFolderStates(st, src.ID)
	require.NoError(err)
	require.Contains(loaded, "INBOX")
	assert.Equal(t, []uint32{1}, loaded["INBOX"].KnownUIDs)
}
