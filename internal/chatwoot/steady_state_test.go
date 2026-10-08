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
	recent := now().Add(-3 * time.Hour)
	messageID := int64(1000)
	for conversation := int64(1); conversation <= 60; conversation++ {
		for range 2 {
			messageID++
			image := map[string]any{"id": messageID, "file_type": "image", "content_type": "image/png", "data_url": "https://chatwoot.example.com/image.png"}
			at := recent.Add(time.Duration(messageID-1000) * time.Minute)
			api.AddMessage(conversation, messageID, at, image)
			api.ActivityAt[conversation] = at.Unix()
		}
	}
	st := testutil.NewTestStore(t)
	imp, source := contractRegister(t, st, api)
	opts := ImportOptions{InboxID: 7}
	first, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	require.False(first.Partial)
	require.Equal(120, first.MessagesProcessed)
	api.TakeRequests()

	second, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.False(second.Partial)
	assert.Zero(second.MessagesProcessed)
	assert.Equal([]string{"agents", "list " + sortByActivity}, api.TakeRequests(), "an unchanged inbox costs one activity page")
	assert.Empty(savedState(t, st, source).Conversations, "settled conversations keep no checkpoint state")

	api.AddMessage(17, 5000, now())
	api.ActivityAt[17] = now().Unix()
	third, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.False(third.Partial)
	assert.Equal(1, third.MessagesAdded)
	for _, request := range api.TakeRequests() {
		assert.True(slices.Contains([]string{"agents", "list " + sortByActivity}, request) || strings.HasPrefix(request, "messages 17 "), "unexpected request %q", request)
	}
	assert.Empty(savedState(t, st, source).Conversations)
}

func TestNewConversationsDoNotWaitForSavedHistory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	api := newContractAPI(t, 1, nil)
	at := now().Add(-30 * 24 * time.Hour)
	for id := int64(101); id <= 120; id++ {
		api.AddMessage(1, id, at)
	}
	st := testutil.NewTestStore(t)
	imp, source := contractRegister(t, st, api)
	imp.requestBudget = 8
	opts := ImportOptions{InboxID: 7}
	first, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	require.True(first.Partial)
	require.NotEmpty(savedState(t, st, source).Conversations["1"].Pending, "the first run leaves a saved tail")

	api.AddMessage(2, 500, now())
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
	second := now().Truncate(time.Second)
	api.AddMessage(1, 101, second)
	st := testutil.NewTestStore(t)
	imp, source := contractRegister(t, st, api)
	_, err := imp.Import(t.Context(), ImportOptions{InboxID: 7})
	require.NoError(err)

	// A reply in the same second leaves the conversation's activity unchanged.
	api.AddMessage(1, 102, second)
	_, err = imp.Import(t.Context(), ImportOptions{InboxID: 7})
	require.NoError(err)
	archived, err := st.MessageExistsBatch(source.ID, []string{"102"})
	require.NoError(err)
	assert.Contains(archived, "102")
}

func TestFailedOldDownloadRetriesNextSync(t *testing.T) {
	checks, must := assert.New(t), require.New(t)
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
	must.NoError(err)
	must.Contains(savedState(t, st, source).Conversations["42"].Artifacts, "901")
	api.Mu.Lock()
	attachment["data_url"] = router.url(t, media.server, "/recording-a.ogg")
	api.Mu.Unlock()
	_, err = NewImporter(st, api.client(t)).Import(t.Context(), opts)
	must.NoError(err)
	refs, payloads := readMediaRefreshBytes(t, st, contractArchivedMessageID(t, st, "901"), opts.AttachmentsDir)
	must.Len(refs, 1)
	checks.Equal([]string{"synthetic recording A bytes"}, payloads)
	checks.Empty(savedState(t, st, source).Conversations, "a stored download leaves the refresh list")
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

	api.Mu.Lock()
	recent["call"] = map[string]any{"id": 1001, "direction": "incoming", "status": "completed", "transcript": "Synthetic call words"}
	api.Mu.Unlock()
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

	api.Mu.Lock()
	forward["content_attributes"] = map[string]any{"to_emails": []string{"forward@example.com"}}
	delete(copied, "sender")
	api.Mu.Unlock()
	_, err = NewImporter(st, api.client(t)).Import(t.Context(), ImportOptions{InboxID: 7, Full: true})
	require.NoError(err)
	assert.Empty(contractRecipients(t, st, forwardID, "cc"), "a refreshed message drops copies it no longer lists")
	copiedID := contractArchivedMessageID(t, st, "303")
	assert.False(contractSender(t, st, copiedID).Valid)
	assert.Empty(contractRecipients(t, st, copiedID, "from"), "a refreshed message drops a sender it lost")
}

func TestCappedRangesCostPagesNotHoles(t *testing.T) {
	for _, tc := range []struct {
		name              string
		cap, count, limit int
	}{
		{"cap_10", 10, 35, 0}, {"limit_20", 1000, 40, 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checks, must := assert.New(t), require.New(t)
			api := newContractAPI(t, tc.cap, nil)
			at := now().Add(-30 * 24 * time.Hour)
			for index := range int64(tc.count) {
				api.AddMessage(1, 100+3*index, at.Add(time.Duration(index)*time.Second))
			}
			st := testutil.NewTestStore(t)
			imp, source := contractRegister(t, st, api)
			opts := ImportOptions{InboxID: 7, Limit: tc.limit}
			sum, err := imp.Import(t.Context(), opts)
			must.NoError(err)
			if tc.limit > 0 {
				checks.Len(savedState(t, st, source).Conversations["1"].Pending, 1, "holes the response proved empty are not saved")
				api.TakeRequests()
				_, err = imp.Import(t.Context(), opts)
				must.NoError(err)
			} else {
				checks.False(sum.Partial)
			}
			reads := 0
			for _, request := range api.TakeRequests() {
				if strings.HasPrefix(request, "messages ") {
					reads++
				}
			}
			if tc.limit > 0 {
				checks.Equal(1+2, reads, "one tail read plus two range probes")
			} else {
				checks.Less(reads, 20, "capped history needs reads per page, not per hole")
			}
			if tc.limit > 0 {
				checks.Len(contractMessageIDs(t, st), tc.count)
			}
			checks.Empty(savedState(t, st, source).Conversations)
		})
	}
}

func TestFirstSyncBackfillsPastOneListingBatch(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		count, pageSize, budget, runs int
		idBase                        int64
	}{
		{"queue_batching", 250, 25, 0, 1, 1000}, {"scan_budget", 15, 3, 8, 20, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checks, must := assert.New(t), require.New(t)
			api := newContractAPI(t, 1000, nil)
			api.PageSize = tc.pageSize
			at := now().Add(-30 * 24 * time.Hour)
			for id := int64(1); id <= int64(tc.count); id++ {
				created := at
				if tc.budget == 0 {
					created = at.Add(time.Duration(id) * time.Minute)
				}
				api.AddMessage(id, tc.idBase+id, created)
			}
			st := testutil.NewTestStore(t)
			imp, source := contractRegister(t, st, api)
			if tc.budget > 0 {
				imp.requestBudget = tc.budget
			}
			for range tc.runs {
				sum, err := imp.Import(t.Context(), ImportOptions{InboxID: 7})
				must.NoError(err)
				if tc.budget == 0 {
					checks.False(sum.Partial)
					checks.Equal(tc.count, sum.MessagesAdded)
				}
			}
			ids := make([]string, 0, tc.count)
			for id := int64(1); id <= int64(tc.count); id++ {
				ids = append(ids, strconv.FormatInt(tc.idBase+id, 10))
			}
			archived, err := st.MessageExistsBatch(source.ID, ids)
			must.NoError(err)
			checks.Len(archived, tc.count)
			checks.Empty(savedState(t, st, source).Walk)
		})
	}
}

func TestCappedArtifactReadStillReachesSkippedIDs(t *testing.T) {
	for _, rotation := range []bool{false, true} {
		name := "skipped_id_ordering"
		if rotation {
			name = "budgeted_rotation"
		}
		t.Run(name, func(t *testing.T) {
			checks, must := assert.New(t), require.New(t)
			api := newContractAPI(t, 2, nil)
			offsets := map[int64]time.Duration{901: -time.Hour + 5*time.Second, 902: -time.Hour + time.Second, 903: -time.Hour + 2*time.Second}
			if rotation {
				offsets = map[int64]time.Duration{}
				for id := int64(100); id <= 112; id++ {
					offsets[id] = time.Duration(112-id) * time.Second
				}
			}
			attachments := map[int64]map[string]any{}
			for id, offset := range offsets {
				attachment := map[string]any{"id": id, "file_type": "audio"}
				attachments[id] = attachment
				api.AddMessage(1, id, now().Add(offset), attachment)
			}
			st := testutil.NewTestStore(t)
			imp, source := contractRegister(t, st, api)
			_, err := imp.Import(t.Context(), ImportOptions{InboxID: 7})
			must.NoError(err)
			targets, transcript, runs := []int64{901, 902, 903}, "late words", 1
			if rotation {
				targets, transcript, runs = []int64{111}, "cappedquartz transcript", 8
			}
			api.Mu.Lock()
			for _, id := range targets {
				attachments[id]["transcribed_text"] = transcript
			}
			api.Mu.Unlock()
			for range runs {
				imp = NewImporter(st, api.client(t))
				if rotation {
					imp.requestBudget = 6
				}
				_, err = imp.Import(t.Context(), ImportOptions{InboxID: 7})
				must.NoError(err)
			}
			for _, id := range targets {
				body, err := st.GetMessageBodyText(contractArchivedMessageID(t, st, strconv.FormatInt(id, 10)))
				must.NoError(err)
				checks.Contains(body, transcript)
			}
			if rotation {
				checks.Contains(savedState(t, st, source).Conversations["1"].Artifacts, "100")
			}
		})
	}
}

func TestReconcileRereadsMessagesCommittedOutOfOrder(t *testing.T) {
	for _, evidence := range []string{"commit_time", "missing", "invalid"} {
		t.Run(evidence, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			fixed := now
			clock := fixed()
			now = func() time.Time { return clock }
			t.Cleanup(func() { now = fixed })
			api := newContractAPI(t, 1000, nil)
			api.AddMessage(1, 101, clock.Add(-time.Minute))
			st := testutil.NewTestStore(t)
			imp, source := contractRegister(t, st, api)
			opts := ImportOptions{InboxID: 7}
			_, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			clock = clock.Add(20 * time.Hour)
			api.AddMessage(1, 100, clock.Add(-20*time.Hour-20*time.Minute))
			api.ActivityAt[1] = clock.Add(-20*time.Hour - 20*time.Minute).Unix()
			switch evidence {
			case "missing":
				delete(api.UpdatedAt, 1)
			case "invalid":
				api.UpdatedAt[1] = -1
			}
			_, err = imp.Import(t.Context(), opts)
			require.NoError(err)
			archived, err := st.MessageExistsBatch(source.ID, []string{"100"})
			require.NoError(err)
			assert.Empty(archived)
			clock = clock.Add(5 * time.Hour)
			opts.Limit = 1
			partial, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			require.True(partial.Partial)
			start := savedState(t, st, source).WalkStartedAt
			clock = clock.Add(time.Hour)
			api.AddMessage(1, 99, clock.Add(-26*time.Hour-30*time.Minute))
			opts.Limit = 0
			_, err = imp.Import(t.Context(), opts)
			require.NoError(err)
			assert.Equal(start, savedState(t, st, source).ReconciledAt)
			clock = clock.Add(24 * time.Hour)
			_, err = imp.Import(t.Context(), opts)
			require.NoError(err)
			archived, err = st.MessageExistsBatch(source.ID, []string{"99", "100"})
			require.NoError(err)
			assert.Len(archived, 2, "commit-time updates cover late lower IDs and resumed walks")
		})
	}
}

func TestQuietOverlapSettlesAndEmptyInboxDiscoversImmediately(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(strconv.FormatBool(empty), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			fixed := now
			clock := fixed()
			now = func() time.Time { return clock }
			t.Cleanup(func() { now = fixed })
			api := newContractAPI(t, 1000, nil)
			if !empty {
				for id := int64(1); id <= 60; id++ {
					api.AddMessage(id, id+100, clock)
				}
			}
			st := testutil.NewTestStore(t)
			imp, source := contractRegister(t, st, api)
			_, err := imp.Import(t.Context(), ImportOptions{InboxID: 7})
			require.NoError(err)
			if empty {
				api.AddMessage(1, 101, clock)
				sum, err := NewImporter(st, api.client(t)).Import(t.Context(), ImportOptions{InboxID: 7})
				require.NoError(err)
				assert.Equal(1, sum.MessagesAdded)
				return
			}
			clock = clock.Add(11 * time.Minute)
			imp.requestBudget = 4
			_, err = imp.Import(t.Context(), ImportOptions{InboxID: 7})
			require.NoError(err)
			assert.Zero(savedState(t, st, source).ActivitySettled, "incomplete scans keep the overlap open")
			imp.requestBudget = 100
			_, err = imp.Import(t.Context(), ImportOptions{InboxID: 7})
			require.NoError(err)
			api.TakeRequests()
			_, err = NewImporter(st, api.client(t)).Import(t.Context(), ImportOptions{InboxID: 7})
			require.NoError(err)
			assert.Equal([]string{"agents", "list " + sortByActivity}, api.TakeRequests())
		})
	}
}

func TestFutureActivityDoesNotHideNormalDiscovery(t *testing.T) {
	for _, poisoned := range []bool{false, true} {
		t.Run(strconv.FormatBool(poisoned), func(t *testing.T) {
			checks, must := assert.New(t), require.New(t)
			previousNow := now
			clock := previousNow().UTC().Truncate(time.Second)
			now = func() time.Time { return clock }
			t.Cleanup(func() { now = previousNow })
			api := newContractAPI(t, 20, nil)
			api.PageSize = 1
			api.AddMessage(1, 101, clock.Add(-time.Hour))
			st := testutil.NewTestStore(t)
			imp, source := contractRegister(t, st, api)
			opts := ImportOptions{InboxID: 7, ReconcileInterval: 24 * time.Hour}
			_, err := imp.Import(t.Context(), opts)
			must.NoError(err)
			reconciledAt := savedState(t, st, source).ReconciledAt
			api.AddMessage(2, 201, clock.Add(72*time.Hour))
			if poisoned {
				state := savedState(t, st, source)
				state.ActivityWatermark = clock.Add(72 * time.Hour).Unix()
				state.ActivitySettled = state.ActivityWatermark
				state.ActivitySeenAt = clock.Add(-time.Hour)
				blob, err := state.marshal()
				must.NoError(err)
				syncID, err := st.StartSyncContext(t.Context(), source.ID, SourceType)
				must.NoError(err)
				must.NoError(st.CompleteSyncAndUpdateSourceCursorContext(t.Context(), syncID, source.ID, blob))
				api.AddMessage(3, 301, clock.Add(-2*time.Hour))
			} else {
				_, err = imp.Import(t.Context(), opts)
				must.NoError(err)
				checks.LessOrEqual(savedState(t, st, source).ActivityWatermark, clock.Unix())
				clock = clock.Add(11 * time.Minute)
				_, err = imp.Import(t.Context(), opts)
				must.NoError(err)
				state := savedState(t, st, source)
				checks.Equal(state.ActivityWatermark, state.ActivitySettled)
				api.AddMessage(3, 301, clock)
			}
			api.TakeRequests()
			sum, err := NewImporter(st, api.client(t)).Import(t.Context(), opts)
			must.NoError(err)
			checks.False(sum.Partial)
			checks.Contains(contractMessageIDs(t, st), int64(301))
			checks.Contains(contractMessageIDs(t, st), int64(201), "future conversations still archive")
			state := savedState(t, st, source)
			checks.LessOrEqual(state.ActivityWatermark, clock.Unix())
			checks.Equal(reconciledAt, state.ReconciledAt, "discovery succeeds before reconciliation is due")
			listingPages := 0
			for _, request := range api.TakeRequests() {
				if request == "list "+sortByActivity {
					listingPages++
				}
				checks.NotEqual("list "+sortByCreated, request)
			}
			checks.GreaterOrEqual(listingPages, 2, "traverse past the future-dated first page")
		})
	}
}
