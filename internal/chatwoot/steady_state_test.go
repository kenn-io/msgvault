package chatwoot

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func savedState(t *testing.T, st *store.Store, source *store.Source) *syncState {
	t.Helper()
	last, err := st.GetLastSuccessfulSyncByType(source.ID, SourceType)
	require.NoError(t, err)
	require.NotNil(t, last)
	state, err := parseSyncState(last.CursorAfter.String, source.Identifier)
	require.NoError(t, err)
	return state
}

func TestSteadyStateSyncRequestsOnlyChangedWork(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	api := newContractAPI(t, 1000, nil)
	recent := time.Now().Add(-3 * time.Hour)
	messageID := int64(1000)
	for conversation := int64(1); conversation <= 60; conversation++ {
		for range 2 {
			messageID++
			image := map[string]any{"id": messageID, "file_type": "image", "content_type": "image/png", "data_url": "https://chatwoot.example.com/image.png"}
			api.addMessage(conversation, messageID, recent.Add(time.Duration(messageID-1000)*time.Minute), image)
		}
	}
	st := testutil.NewTestStore(t)
	imp, source := contractRegister(t, st, api)
	opts := ImportOptions{InboxID: 7}
	first, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	require.False(first.Partial)
	require.Equal(120, first.MessagesProcessed)
	api.takeRequests()

	second, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.False(second.Partial)
	assert.Zero(second.MessagesProcessed)
	assert.Equal([]string{"agents", "list " + sortByActivity}, api.takeRequests(), "an unchanged inbox costs one activity page")
	assert.Empty(savedState(t, st, source).Conversations, "settled conversations keep no checkpoint state")

	api.addMessage(17, 5000, time.Now())
	third, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.False(third.Partial)
	assert.Equal(1, third.MessagesAdded)
	for _, request := range api.takeRequests() {
		assert.True(slices.Contains([]string{"agents", "list " + sortByActivity}, request) || strings.HasPrefix(request, "messages 17 "), "unexpected request %q", request)
	}
	assert.Empty(savedState(t, st, source).Conversations)
}

func TestNewConversationsDoNotWaitForSavedHistory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	api := newContractAPI(t, 1, nil)
	at := time.Now().Add(-30 * 24 * time.Hour)
	for id := int64(101); id <= 120; id++ {
		api.addMessage(1, id, at)
	}
	st := testutil.NewTestStore(t)
	imp, source := contractRegister(t, st, api)
	imp.requestBudget = 8
	opts := ImportOptions{InboxID: 7}
	first, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	require.True(first.Partial)
	require.NotEmpty(savedState(t, st, source).Conversations["1"].Pending, "the first run leaves a saved tail")

	api.addMessage(2, 500, time.Now())
	second, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.True(second.Partial)
	archived, err := st.MessageExistsBatch(source.ID, []string{"500"})
	require.NoError(err)
	assert.Contains(archived, "500", "a new conversation is archived while older history is still saved")
}

func TestSameSecondMessageIsNotSkipped(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	api := newContractAPI(t, 1000, nil)
	second := time.Now().Truncate(time.Second)
	api.addMessage(1, 101, second)
	st := testutil.NewTestStore(t)
	imp, source := contractRegister(t, st, api)
	_, err := imp.Import(t.Context(), ImportOptions{InboxID: 7})
	require.NoError(err)

	// A reply in the same second leaves the conversation's activity unchanged.
	api.addMessage(1, 102, second)
	_, err = imp.Import(t.Context(), ImportOptions{InboxID: 7})
	require.NoError(err)
	archived, err := st.MessageExistsBatch(source.ID, []string{"102"})
	require.NoError(err)
	assert.Contains(archived, "102")
}

func TestFailedOldDownloadRetriesNextSync(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	media := newMediaRefreshServer(t)
	router := newChatwootMediaRouter(t, media.server)
	media.failures["/recording-a.ogg"] = true
	// Far older than the refresh window, as in a first import of old history.
	message := contractMessage(901, now().Add(-30*24*time.Hour).Unix(), nil)
	message["attachments"] = []any{map[string]any{"id": 2001, "message_id": 901, "file_type": "file", "data_url": router.url(t, media.server, "/recording-a.ogg")}}
	api := newContractAPI(t, 1000, []map[string]any{message})
	api.mediaRouter = router
	st := testutil.NewTestStore(t)
	importer, source := contractRegister(t, st, api)
	opts := mediaRefreshOptions(t)
	first, err := importer.Import(t.Context(), opts)
	require.NoError(err)
	require.Equal(1, first.MediaFailures)
	assert.Contains(savedState(t, st, source).Conversations["42"].Artifacts, "901")

	media.mu.Lock()
	media.failures["/recording-a.ogg"] = false
	media.mu.Unlock()
	_, err = NewImporter(st, api.client(t)).Import(t.Context(), opts)
	require.NoError(err)
	refs, payloads := readMediaRefreshBytes(t, st, contractArchivedMessageID(t, st, "901"), opts.AttachmentsDir)
	require.Len(refs, 1)
	assert.Equal([]string{"synthetic recording A bytes"}, payloads)
	assert.Empty(savedState(t, st, source).Conversations, "a stored download leaves the refresh list")
}

func TestCallsAreRecheckedOnlyInsideTheWindow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	recent := contractMessage(901, now().Add(-time.Hour).Unix(), nil)
	recent["content_type"] = "voice_call"
	recent["call"] = map[string]any{"id": 1001, "direction": "incoming", "status": "in-progress"}
	old := contractMessage(902, now().Add(-8*24*time.Hour).Unix(), nil)
	old["content_type"] = "voice_call"
	old["call"] = map[string]any{"id": 1002, "direction": "incoming", "status": "in-progress"}
	api := newContractAPI(t, 1000, []map[string]any{old, recent})
	st := testutil.NewTestStore(t)
	importer, source := contractRegister(t, st, api)
	_, err := importer.Import(t.Context(), ImportOptions{InboxID: 7})
	require.NoError(err)
	artifacts := savedState(t, st, source).Conversations["42"].Artifacts
	assert.Contains(artifacts, "901", "a recent call is rechecked")
	assert.NotContains(artifacts, "902", "a call older than the window is not")

	api.mu.Lock()
	recent["call"] = map[string]any{"id": 1001, "direction": "incoming", "status": "completed", "transcript": "Synthetic call words"}
	api.mu.Unlock()
	_, err = NewImporter(st, api.client(t)).Import(t.Context(), ImportOptions{InboxID: 7})
	require.NoError(err)
	assert.Contains(savedState(t, st, source).Conversations["42"].Artifacts, "901", "a later recording can still replace this one")
}

func TestEmailReplyUsesItsAddressedRecipients(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	agent := map[string]any{"id": int64(7), "type": "user", "name": "Example Agent"}
	forward := contractMessage(301, 1767225601, agent)
	forward["message_type"] = 1
	forward["content_attributes"] = map[string]any{"to_emails": []string{"Forward@Example.com"}, "cc_emails": []string{"copy@example.com"}, "bcc_emails": []string{}}
	reply := contractMessage(302, 1767225602, agent)
	reply["message_type"] = 1
	copied := contractMessage(303, 1767225603, agent)
	copied["message_type"] = 1
	copied["content_attributes"] = map[string]any{"cc_emails": []string{"copy@example.com"}}
	api := newContractAPI(t, 1000, []map[string]any{forward, reply, copied})
	st := testutil.NewTestStore(t)
	importer, _ := contractRegister(t, st, api)
	_, err := importer.Import(t.Context(), ImportOptions{InboxID: 7})
	require.NoError(err)
	forwardID := contractArchivedMessageID(t, st, "301")
	to := contractRecipients(t, st, forwardID, "to")
	require.Len(to, 1, "a forward reaches its named recipient, not the conversation contact")
	assert.Equal("forward@example.com", to[0].EmailAddress)
	cc := contractRecipients(t, st, forwardID, "cc")
	require.Len(cc, 1)
	assert.Equal("copy@example.com", cc[0].EmailAddress)
	replyTo := contractRecipients(t, st, contractArchivedMessageID(t, st, "302"), "to")
	require.Len(replyTo, 1)
	assert.NotEqual(to[0].ParticipantID, replyTo[0].ParticipantID, "an ordinary reply still reaches the contact")
	copiedTo := contractRecipients(t, st, contractArchivedMessageID(t, st, "303"), "to")
	require.Len(copiedTo, 1)
	assert.Equal(replyTo[0].ParticipantID, copiedTo[0].ParticipantID, "a reply with only copies still reaches the contact")

	api.mu.Lock()
	forward["content_attributes"] = map[string]any{"to_emails": []string{"forward@example.com"}}
	delete(copied, "sender")
	api.mu.Unlock()
	_, err = NewImporter(st, api.client(t)).Import(t.Context(), ImportOptions{InboxID: 7, Full: true})
	require.NoError(err)
	assert.Empty(contractRecipients(t, st, forwardID, "cc"), "a refreshed message drops copies it no longer lists")
	copiedID := contractArchivedMessageID(t, st, "303")
	assert.False(contractSender(t, st, copiedID).Valid)
	assert.Empty(contractRecipients(t, st, copiedID, "from"), "a refreshed message drops a sender it lost")
}

func TestCappedRangesCostPagesNotHoles(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	api := newContractAPI(t, 10, nil)
	at := time.Now().Add(-30 * 24 * time.Hour)
	// Account-wide IDs leave a hole between every message of one conversation.
	for index := range int64(35) {
		api.addMessage(1, 100+3*index, at.Add(time.Duration(index)*time.Second))
	}
	st := testutil.NewTestStore(t)
	imp, source := contractRegister(t, st, api)
	sum, err := imp.Import(t.Context(), ImportOptions{InboxID: 7})
	require.NoError(err)
	assert.False(sum.Partial)
	assert.Len(contractMessageIDs(t, st), 35)
	var reads int
	for _, request := range api.takeRequests() {
		if strings.HasPrefix(request, "messages ") {
			reads++
		}
	}
	assert.Less(reads, 20, "35 messages behind a 10-message cap need a few reads per page, not one per hole")
	assert.Empty(savedState(t, st, source).Conversations)
}

func TestStoredRecordingSurvivesWhenProviderDropsIt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	media := newMediaRefreshServer(t)
	router := newChatwootMediaRouter(t, media.server)
	message := mediaRefreshCall(router.url(t, media.server, "/recording-a.ogg"), "")
	call, ok := message["call"].(map[string]any)
	require.True(ok)
	api := newContractAPI(t, 1000, []map[string]any{message})
	api.mediaRouter = router
	st := testutil.NewTestStore(t)
	importer, _ := contractRegister(t, st, api)
	opts := mediaRefreshOptions(t)
	_, err := importer.Import(t.Context(), opts)
	require.NoError(err)
	meetingID := contractArchivedMessageID(t, st, "call:901")

	api.mu.Lock()
	delete(call, "recording_url")
	api.mu.Unlock()
	_, err = NewImporter(st, api.client(t)).Import(t.Context(), opts)
	require.NoError(err)
	refs, payloads := readMediaRefreshBytes(t, st, meetingID, opts.AttachmentsDir)
	assert.Len(refs, 1)
	assert.Equal([]string{"synthetic recording A bytes"}, payloads, "the archive keeps a recording the provider stops listing")
}

func TestFirstSyncBackfillsPastOneListingBatch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	api := newContractAPI(t, 1000, nil)
	at := time.Now().Add(-30 * 24 * time.Hour)
	for conversation := int64(1); conversation <= 250; conversation++ {
		api.addMessage(conversation, 1000+conversation, at.Add(time.Duration(conversation)*time.Minute))
	}
	st := testutil.NewTestStore(t)
	imp, source := contractRegister(t, st, api)
	sum, err := imp.Import(t.Context(), ImportOptions{InboxID: 7})
	require.NoError(err)
	assert.False(sum.Partial)
	assert.Equal(250, sum.MessagesAdded, "one run backfills as far as its request budget allows")
	assert.Empty(savedState(t, st, source).Walk)
}

func TestFileWithoutURLWaitsOnRefreshList(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	media := newMediaRefreshServer(t)
	router := newChatwootMediaRouter(t, media.server)
	message := contractMessage(901, now().Add(-time.Hour).Unix(), nil)
	attachment := map[string]any{"id": 2001, "message_id": 901, "file_type": "file"}
	message["attachments"] = []any{attachment}
	api := newContractAPI(t, 1000, []map[string]any{message})
	api.mediaRouter = router
	st := testutil.NewTestStore(t)
	importer, source := contractRegister(t, st, api)
	opts := mediaRefreshOptions(t)
	_, err := importer.Import(t.Context(), opts)
	require.NoError(err)
	assert.Contains(savedState(t, st, source).Conversations["42"].Artifacts, "901", "Chatwoot attaches the file after creating the message")

	api.mu.Lock()
	attachment["data_url"] = router.url(t, media.server, "/recording-a.ogg")
	api.mu.Unlock()
	_, err = NewImporter(st, api.client(t)).Import(t.Context(), opts)
	require.NoError(err)
	refs, payloads := readMediaRefreshBytes(t, st, contractArchivedMessageID(t, st, "901"), opts.AttachmentsDir)
	require.Len(refs, 1)
	assert.Equal([]string{"synthetic recording A bytes"}, payloads)
	assert.Empty(savedState(t, st, source).Conversations)
}

func TestCappedArtifactReadStillReachesSkippedIDs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var messages, attachments []map[string]any
	// 901 was created last, so a capped read in creation order never returns it.
	for id, offset := range map[int64]int64{901: 5, 902: 1, 903: 2} {
		m := contractMessage(id, now().Add(-time.Hour).Unix()+offset, nil)
		attachment := map[string]any{"id": id, "file_type": "audio"}
		m["attachments"] = []any{attachment}
		messages, attachments = append(messages, m), append(attachments, attachment)
	}
	api := newContractAPI(t, 2, messages)
	st := testutil.NewTestStore(t)
	importer, _ := contractRegister(t, st, api)
	_, err := importer.Import(t.Context(), ImportOptions{InboxID: 7})
	require.NoError(err)
	api.mu.Lock()
	for _, attachment := range attachments {
		attachment["transcribed_text"] = "late words"
	}
	api.mu.Unlock()
	_, err = NewImporter(st, api.client(t)).Import(t.Context(), ImportOptions{InboxID: 7})
	require.NoError(err)
	for _, id := range []string{"901", "902", "903"} {
		body, err := st.GetMessageBodyText(contractArchivedMessageID(t, st, id))
		require.NoError(err)
		assert.Contains(body, "late words", "message %s", id)
	}
}

func TestReconcileRereadsMessagesCommittedOutOfOrder(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	api := newContractAPI(t, 1000, nil)
	// Written after the first sync's reconcile, so the next one covers them.
	at := now().Add(time.Minute)
	api.addMessage(1, 200, at.Add(2*time.Second))
	// A backdated higher ID, archived first, must not hide a recent lower one.
	api.addMessage(1, 300, at.Add(-48*time.Hour))
	st := testutil.NewTestStore(t)
	imp, source := contractRegister(t, st, api)
	opts := ImportOptions{InboxID: 7}
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)

	// Concurrent writes commit late: a lower ID, and a higher ID created earlier.
	// Neither changes the listing's newest message or activity time.
	api.addMessage(1, 150, at.Add(2*time.Second))
	api.addMessage(1, 201, at.Add(time.Second))
	api.addMessage(1, 160, at.Add(3*time.Second))
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	archived, err := st.MessageExistsBatch(source.ID, []string{"150", "160", "201"})
	require.NoError(err)
	assert.Empty(archived, "an incremental sync trusts the listing")

	fixed := now
	t.Cleanup(func() { now = fixed })
	now = func() time.Time { return fixed().Add(48 * time.Hour) }
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	archived, err = st.MessageExistsBatch(source.ID, []string{"150", "160", "201", "300"})
	require.NoError(err)
	assert.Len(archived, 4, "reconcile rereads each conversation active since the last one")
}

func TestLimitedRunSavesOnlyTheUnreadTail(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	api := newContractAPI(t, 1000, nil)
	at := time.Now().Add(-30 * 24 * time.Hour)
	for index := range int64(40) {
		api.addMessage(1, 100+3*index, at.Add(time.Duration(index)*time.Second))
	}
	st := testutil.NewTestStore(t)
	imp, source := contractRegister(t, st, api)
	opts := ImportOptions{InboxID: 7, Limit: 20}
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.Len(savedState(t, st, source).Conversations["1"].Pending, 1, "holes the response proved empty are not saved")
	api.takeRequests()
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	var reads int
	for _, request := range api.takeRequests() {
		if strings.HasPrefix(request, "messages ") {
			reads++
		}
	}
	assert.Equal(1+2, reads, "one read of the tail plus the import's two range probes")
	assert.Len(contractMessageIDs(t, st), 40)
}

func TestListingProgressesWhenTheActivityScanRunsOutOfBudget(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	api := newContractAPI(t, 1000, nil)
	api.pageSize = 3
	// Every conversation sits inside the overlap, so the scan never reaches the
	// watermark within its half of a small budget.
	at := time.Now().Add(-30 * 24 * time.Hour)
	for conversation := int64(1); conversation <= 15; conversation++ {
		api.addMessage(conversation, 100+conversation, at)
	}
	st := testutil.NewTestStore(t)
	imp, source := contractRegister(t, st, api)
	imp.requestBudget = 8
	for range 20 {
		_, err := imp.Import(t.Context(), ImportOptions{InboxID: 7})
		require.NoError(err)
	}
	ids := make([]string, 0, 15)
	for id := int64(101); id <= 115; id++ {
		ids = append(ids, strconv.FormatInt(id, 10))
	}
	archived, err := st.MessageExistsBatch(source.ID, ids)
	require.NoError(err)
	assert.Len(archived, 15)
	assert.Empty(savedState(t, st, source).Walk, "the listing finishes even while the scan can't")
}
