package beeper

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestRosterVerificationRejectsChangedSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*fakeChat)
	}{
		{"member", func(c *fakeChat) { c.Participants[1]["id"] = "@changed:example.test" }},
		{"role", func(c *fakeChat) { c.Participants[1]["isAdmin"] = true }},
		{"identity", func(c *fakeChat) { c.Participants[1]["providerID"] = "changed-native-id" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			f := newFakeBeeper(t)
			chat := budgetTestChat("!verify:example.test", 2, time.Now())
			chat.Participants = append(chat.Participants, map[string]any{"id": "@signal_departed:example.test"})
			f.addChat(chat)
			imp, st := newHandlerTestImporter(t, f)
			t.Cleanup(func() { require.NoError(st.Close()) })
			sourceID := newBeeperTestSource(t, st, "signal")
			work := &rosterWork{Version: 1}
			detail, err := imp.client.GetChat(t.Context(), chat.ID)
			require.NoError(err)
			_, complete, _, err := imp.ensureRosterWork(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "signal", rosterState: work}, &ImportSummary{})
			require.NoError(err)
			require.True(complete)
			// The departure needs a removal, which a changed second read must block.
			f.mu.Lock()
			chat.Participants = chat.Participants[:2]
			f.mu.Unlock()
			detail, err = imp.client.GetChat(t.Context(), chat.ID)
			require.NoError(err)
			f.mu.Lock()
			tc.change(chat)
			f.mu.Unlock()
			sum := &ImportSummary{}
			_, complete, _, err = imp.ensureRosterWork(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "signal", rosterState: work}, sum)
			require.NoError(err)
			assert.False(complete)
			assert.Zero(sum.FetchErrors)
			assert.Len(rosterMembers(t, st, sourceID, chat.ID), 3, "the departed member stays until a read confirms the roster")
			assert.False(work.Verified)
		})
	}
}

func TestImportVerifiesSmallRosterBeforeReplacement(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newFakeBeeper(t)
	chat := budgetTestChat("!small-roster:example.test", 2, time.Now().Add(-time.Hour))
	f.addChat(chat)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	detailRequests := func() int {
		f.mu.Lock()
		defer f.mu.Unlock()
		n := 0
		for _, req := range f.reqs {
			if strings.HasPrefix(req, "/v1/chats/"+chat.ID+"?") {
				n++
			}
		}
		f.reqs = nil
		return n
	}
	sum, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true})
	require.NoError(err)
	assert.Equal(1, detailRequests(), "the first import verifies the roster")
	assert.Len(rosterMembers(t, st, sum.SourceID, chat.ID), 2)
	f.mu.Lock()
	chat.Participants[1]["fullName"] = "Ann Renamed"
	chat.LastActivity = time.Now()
	chat.Msgs = append(chat.Msgs, fakeMsg{ID: "renamed", SortKey: 99, Timestamp: chat.LastActivity, Text: "new", SenderID: "@signal_ann:beeper.local"})
	f.mu.Unlock()
	_, err = imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true})
	require.NoError(err)
	assert.Equal(1, detailRequests(), "a rename is verified before replacement")
	f.mu.Lock()
	chat.Participants = append(chat.Participants, map[string]any{"id": "@signal_joined:example.test", "fullName": "Joined"})
	chat.LastActivity = time.Now()
	chat.Msgs = append(chat.Msgs, fakeMsg{ID: "joined", SortKey: 100, Timestamp: chat.LastActivity, Text: "joined", SenderID: "@signal_ann:beeper.local"})
	f.mu.Unlock()
	detailRequests()
	sum, err = imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true, ShouldStop: func() bool {
		var members int
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM conversation_participants WHERE conversation_id = (SELECT id FROM conversations WHERE source_id = ? AND source_conversation_id = ?)`), sum.SourceID, chat.ID).Scan(&members))
		return members >= 3
	}})
	require.NoError(err)
	require.True(sum.Stopped)
	assert.Zero(detailRequests(), "a yield after the final member commit must not start verification")
}

func TestRosterSignatureIgnoresOrder(t *testing.T) {
	assert := assert.New(t)
	ch := &Chat{ID: "room", AccountID: "account", Type: "group", Network: "signal", Participants: ChatParticipants{Total: 2, Items: []Participant{{User{ID: "a"}, false}, {User{ID: "b"}, true}}}}
	signature := rosterSignature(ch, "account")
	ch.Participants.Items[0], ch.Participants.Items[1] = ch.Participants.Items[1], ch.Participants.Items[0]
	assert.Equal(signature, rosterSignature(ch, "account"))
	ch.Network = "other"
	assert.NotEqual(signature, rosterSignature(ch, "account"))
}

func TestRosterSettledPrefixExceedsBudget(t *testing.T) {
	require := require.New(t)
	f := newFakeBeeper(t)
	chat := budgetTestChat("!large-prefix:example.test", 2, time.Now())
	for i := range 64 {
		chat.Participants = append(chat.Participants, map[string]any{"id": fmt.Sprintf("@signal_prefix%d:example.test", i)})
	}
	f.addChat(chat)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	sourceID := newBeeperTestSource(t, st, "signal")
	detail, err := imp.client.GetChat(t.Context(), chat.ID)
	require.NoError(err)
	work := &rosterWork{Version: 1}
	_, complete, _, err := imp.ensureRosterWork(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "signal", rosterState: work}, &ImportSummary{})
	require.NoError(err)
	require.True(complete)
	for range 10 {
		polls := 0
		_, complete, _, err = imp.ensureRosterWork(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "signal", rosterState: work, ShouldStop: func() bool {
			polls++
			return polls > 20
		}}, &ImportSummary{})
		if err == nil && complete {
			return
		}
		require.ErrorIs(err, errBeeperBudgetExpired)
	}
	t.Fatal("repeated visits never finished the settled roster")
}

func TestRosterWorkCheckpointClear(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	state := NewSyncState()
	state.EnsureChat("room").RosterWork = &rosterWork{Version: 1, Signature: "snapshot", Phase: "matching", Next: 1, Verified: true}
	blob, err := state.Marshal()
	require.NoError(err)
	restored, err := LoadSyncState(blob)
	require.NoError(err)
	require.Equal(state.Chats["room"].RosterWork, restored.Chats["room"].RosterWork)
	checkpoint := NewSyncState()
	checkpoint.EnsureChat("room")
	restored.Merge(checkpoint)
	assert.Nil(restored.Chats["room"].RosterWork)
}

func TestRosterVerificationFailureKeepsExistingMembership(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newFakeBeeper(t)
	chat := budgetTestChat("!verify-failure:example.test", 2, time.Now())
	chat.Participants = append(chat.Participants, map[string]any{"id": "@signal_departed:example.test"})
	f.addChat(chat)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	sourceID := newBeeperTestSource(t, st, "signal")
	detail, err := imp.client.GetChat(t.Context(), chat.ID)
	require.NoError(err)
	_, _, _, err = imp.ensureRosterWork(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "signal"}, &ImportSummary{}) //nolint:dogsled // Only the error matters for the first visit.
	require.NoError(err)
	before := rosterMembers(t, st, sourceID, chat.ID)
	detail.Participants.Items = detail.Participants.Items[:2]
	detail.Participants.Total = 2
	f.setChatGetFailure(chat.ID, true)
	work := &rosterWork{Version: 1}
	sum := &ImportSummary{}
	_, complete, _, err := imp.ensureRosterWork(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "signal", rosterState: work}, sum)
	require.NoError(err)
	assert.False(complete)
	assert.Zero(sum.FetchErrors)
	assert.Equal(before, rosterMembers(t, st, sourceID, chat.ID))
	assert.False(work.Verified)
	unresolved, err := st.ListUnresolvedConversationIDs(t.Context(), sourceID)
	require.NoError(err)
	assert.Contains(unresolved, chat.ID)
}

func TestRosterResumeUsesMergedParticipant(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newFakeBeeper(t)
	chat := budgetTestChat("!merged-roster:example.test", 2, time.Now())
	chat.Participants[1]["phoneNumber"] = "+12025550123"
	f.addChat(chat)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	sourceID := newBeeperTestSource(t, st, "signal")
	detail, err := imp.client.GetChat(t.Context(), chat.ID)
	require.NoError(err)
	imp.res.accountID = "signal"
	first := &detail.Participants.Items[0].User
	old, err := imp.res.resolveUser(t.Context(), first)
	require.NoError(err)
	other := &detail.Participants.Items[1].User
	require.NoError(st.SetParticipantIdentifierContext(t.Context(), old, participantIdentifierType, imp.res.identifierForUser(other.ID)))
	state := &ChatState{RosterWork: &rosterWork{Version: 1, Signature: rosterSignature(detail, "signal"), Phase: "members"}}
	blob, err := (&SyncState{Chats: map[string]*ChatState{chat.ID: state}}).Marshal()
	require.NoError(err)
	restored, err := LoadSyncState(blob)
	require.NoError(err)
	imp = NewImporter(st, imp.client)
	_, _, _, err = imp.ensureRosterWork(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "signal", rosterState: restored.Chats[chat.ID].RosterWork}, &ImportSummary{}) //nolint:dogsled // Only the error matters here.
	require.NoError(err)
	winner := participantForBeeperUser(t, st, other.ID)
	assert.NotEqual(old, winner, "resolving the second member merges the first binding")
	assert.Contains(rosterMembers(t, st, sourceID, chat.ID), winner)
	assert.NotContains(rosterMembers(t, st, sourceID, chat.ID), old)
}

func TestRosterVerificationFailureContinuesMessagesAndLaterChat(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newFakeBeeper(t)
	first := budgetTestChat("!verify-deferred:example.test", 2, time.Now().Add(-48*time.Hour))
	first.Participants = append(first.Participants, map[string]any{"id": "@signal_departed:example.test"})
	later := budgetTestChat("!verify-later:example.test", 2, time.Now().Add(-72*time.Hour))
	f.addChat(first)
	f.addChat(later)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	_, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true})
	require.NoError(err)
	f.mu.Lock()
	first.Participants = first.Participants[:2]
	for _, chat := range []*fakeChat{first, later} {
		chat.LastActivity = time.Now()
		chat.Msgs = append(chat.Msgs, fakeMsg{ID: chat.ID + "-new", SortKey: 99, Timestamp: chat.LastActivity, Text: "new", SenderID: "@signal_ann:beeper.local"})
	}
	f.mu.Unlock()
	f.setChatGetFailure(first.ID, true)
	sum, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true})
	require.NoError(err)
	require.NotNil(sum)
	assert.Zero(sum.FetchErrors)
	assert.Equal(6, countBeeperMessages(t, imp))
}

func TestRosterResumeResolvesRicherIdentityAndName(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newFakeBeeper(t)
	chat := budgetTestChat("!roster-rich:example.test", 2, time.Now().Add(-48*time.Hour))
	chat.Participants[1]["fullName"] = ""
	f.addChat(chat)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	sum, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true})
	require.NoError(err)
	work := &rosterWork{Version: 1}
	saved := func() (name, email string) {
		id := participantForBeeperUser(t, st, "@signal_ann:beeper.local")
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COALESCE(display_name, ''), COALESCE(email_address, '') FROM participants WHERE id = ?`), id).Scan(&name, &email))
		return name, email
	}
	for _, update := range []struct{ key, value string }{
		{"fullName", "Updated Name"},
		{"email", "updated@example.test"},
		{"phoneNumber", "+12025550123"},
	} {
		f.mu.Lock()
		chat.Participants[1][update.key] = update.value
		f.mu.Unlock()
		detail, err := imp.client.GetChat(t.Context(), chat.ID)
		require.NoError(err)
		imp = NewImporter(st, imp.client)
		_, complete, _, err := imp.ensureRosterWork(t.Context(), 0, sum.SourceID, detail, ImportOptions{AccountID: "signal", rosterState: work}, &ImportSummary{})
		require.NoError(err)
		require.True(complete)
		name, email := saved()
		switch update.key {
		case "fullName":
			assert.Equal("Updated Name", name, "the name refresh is saved before any richer identity arrives")
		case "email":
			assert.Equal("updated@example.test", email, "the email refresh rebinds the user to its email participant")
			assert.Equal("Updated Name", name)
		}
	}
	id := participantForBeeperUser(t, st, "@signal_ann:beeper.local")
	var name, phone string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COALESCE(display_name, ''), COALESCE(phone_number, '') FROM participants WHERE id = ?`), id).Scan(&name, &phone))
	assert.Equal("Updated Name", name)
	assert.Equal("+12025550123", phone)
	assert.Contains(rosterMembers(t, st, sum.SourceID, chat.ID), id)
}

func TestRosterMatchingDuringUnfinishedHistory(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newFakeBeeper(t)
	f.pageSize = 1
	chat := budgetTestChat("!roster-backfill:example.test", 2, time.Now().Add(-48*time.Hour))
	f.addChat(chat)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	_, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true, Limit: 1})
	require.NoError(err)
	state := latestBeeperState(t, imp)
	require.NotNil(state.Chats[chat.ID])
	assert.False(state.Chats[chat.ID].Done)
	assert.Equal(1, countBeeperMessages(t, imp))
	peer := participantForBeeperUser(t, st, "@signal_ann:beeper.local")
	observations, err := st.ListParticipantObservationsContext(t.Context(), peer, true)
	require.NoError(err)
	assert.NotEmpty(observations)
}

func TestRosterWorkRejectsFutureVersion(t *testing.T) {
	for _, blob := range []string{
		`{"chats":{"room":{"roster_work":{"version":2}}}}`,
		`{"chats":{"room":{"roster_work":{"version":1,"phase":"future"}}}}`,
	} {
		_, err := LoadSyncState(blob)
		require.Error(t, err)
	}
}

func TestUnsupportedRosterVersionStopsImport(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		t.Run(fmt.Sprintf("checkpoint=%t", checkpoint), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			f := newFakeBeeper(t)
			imp, st := newHandlerTestImporter(t, f)
			t.Cleanup(func() { require.NoError(st.Close()) })
			sourceID := newBeeperTestSource(t, st, "signal")
			const blob = `{"chats":{"room":{"newest":"saved","roster_work":{"version":2}}}}`
			runID, err := st.StartSync(sourceID, sourceTypeBeeper)
			require.NoError(err)
			if checkpoint {
				require.NoError(st.PauseSyncWithCheckpoint(t.Context(), runID, &store.Checkpoint{PageToken: blob}))
			} else {
				require.NoError(st.CompleteSync(runID, blob))
			}
			_, err = imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true})
			const want = "saved Beeper progress is incompatible with this version of msgvault. Update msgvault to resume"
			require.ErrorContains(err, want)
			_, err = imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true, Full: true})
			require.ErrorContains(err, want)
			_, err = imp.BackfillMedia(t.Context(), ImportOptions{AccountID: "signal", AttachmentsDir: t.TempDir()})
			require.ErrorContains(err, want)
			var runs int
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT count(*) FROM sync_runs WHERE source_id = ?`), sourceID).Scan(&runs))
			assert.Equal(1, runs)
		})
	}
}

func TestUnsupportedRosterSourceVersionStopsImport(t *testing.T) {
	f := newFakeBeeper(t)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	sourceID := newBeeperTestSource(t, st, "signal")
	const want = "saved Beeper progress is incompatible with this version of msgvault. Update msgvault to resume"
	require.NoError(t, st.UpdateSourceSyncState(t.Context(), sourceID, `{"chats":{"room":{"roster_work":{"version":2}}}}`))
	for _, full := range []bool{false, true} {
		_, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true, Full: full})
		require.ErrorContains(t, err, want)
	}
	_, err := imp.BackfillMedia(t.Context(), ImportOptions{AccountID: "signal", AttachmentsDir: t.TempDir()})
	require.ErrorContains(t, err, want)
	var runs int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT count(*) FROM sync_runs WHERE source_id = ?`), sourceID).Scan(&runs))
	assert.Zero(t, runs, "no media run starts over newer progress")
}

func TestRosterJoinRefreshesExistingMatchEvidence(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newFakeBeeper(t)
	chat := observationChat("slack", "Slack", "@slack_alice:beeper.local",
		map[string]any{"id": "@slack_alice:beeper.local", "username": "alice"})
	f.addChat(chat)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	imp.res.accountID = "slack"
	other, err := imp.res.resolveUser(t.Context(), &User{ID: "@slack_other:beeper.local"})
	require.NoError(err)
	otherSource := newBeeperTestSource(t, st, "slack2")
	captureAndMatch(t, newObservationRecorder(st), newIdentityMatcher(st), other,
		&User{ID: "@slack_other:beeper.local", Username: "alice"},
		captureContext{SourceID: otherSource, AccountID: "slack2", Network: "Slack"})
	sum, err := imp.Import(t.Context(), ImportOptions{AccountID: "slack", NoMedia: true})
	require.NoError(err)
	candidates, err := st.ListIdentityMatchCandidatesContext(t.Context(), nil, 100, 0)
	require.NoError(err)
	require.Len(candidates, 1)
	existing := participantForBeeperUser(t, st, "@slack_alice:beeper.local")
	captureAndMatch(t, imp.obs, imp.matcher, existing,
		&User{ID: "@slack_alice:beeper.local", Username: "alice"},
		captureContext{SourceID: sum.SourceID, AccountID: "slack", Network: "Slack"})
	f.mu.Lock()
	chat.Participants = append(chat.Participants, map[string]any{"id": "@slack_other:beeper.local"})
	f.mu.Unlock()
	detail, err := imp.client.GetChat(t.Context(), chat.ID)
	require.NoError(err)
	state, err := imp.loadResumeState(t.Context(), sum.SourceID)
	require.NoError(err)
	_, _, err = imp.syncChat(t.Context(), 0, sum.SourceID, detail, ImportOptions{AccountID: "slack", NoMedia: true}, state, time.Now(), false, false, &ImportSummary{})
	require.NoError(err)
	candidate, err := st.GetIdentityMatchCandidateContext(t.Context(), candidates[0].ID)
	require.NoError(err)
	var found bool
	for _, evidence := range candidate.Evidence {
		found = found || evidence.EvidenceKind == evidenceMembership
	}
	assert.True(found, "the existing candidate gains shared conversation evidence")
}

func TestQuietMatchingRepairInvalidatesGoneRoute(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newFakeBeeper(t)
	chat := budgetTestChat("!gone-matching:example.test", 2, time.Now().Add(-48*time.Hour))
	f.addChat(chat)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	provider := f.handler()
	imp.client.http.Transport = handlerTransport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/accounts/signal" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"accountID":"signal","bridge":{"type":"signal"},"status":"connected"}`))
			return
		}
		provider(w, r)
	}))
	sum, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true})
	require.NoError(err)
	state, err := imp.loadResumeState(t.Context(), sum.SourceID)
	require.NoError(err)
	cs := state.Chats[chat.ID]
	require.True(cs.Done)
	detail, err := imp.client.GetChat(t.Context(), chat.ID)
	require.NoError(err)
	cs.RosterWork = &rosterWork{Version: 1, Signature: rosterSignature(detail, "signal"), Phase: "matching", Verified: true}
	peer := participantForBeeperUser(t, st, "@signal_ann:beeper.local")
	person, _, err := st.CreatePersonFromParticipant(peer)
	require.NoError(err)
	query := store.PersonMessagingRouteQuery{PersonUID: person.VCardUID}
	before, err := st.GetPersonMessagingRoutesContext(t.Context(), query)
	require.NoError(err)
	require.Len(before.Routes.Items, 1)
	require.Equal("archive_verified", before.Routes.Items[0].Status)
	f.mu.Lock()
	f.goneChatGets = map[string]bool{chat.ID: true}
	f.mu.Unlock()
	runID, err := st.StartSync(sum.SourceID, sourceTypeBeeper)
	require.NoError(err)
	_, err = imp.repairQuietRosters(t.Context(), runID, sum.SourceID, ImportOptions{AccountID: "signal"}, state, nil, &ImportSummary{})
	require.NoError(err)
	require.NoError(st.CompleteSync(runID, ""))
	assert.True(cs.Gone)
	after, err := st.GetPersonMessagingRoutesContext(t.Context(), query)
	require.NoError(err)
	require.Len(after.Routes.Items, 1)
	assert.Equal("unresolved", after.Routes.Items[0].Status)
}

func slackRosterChat(id, account, userID string, activity time.Time, member map[string]any) *fakeChat {
	member["id"] = userID
	chat := budgetTestChat(id, 1, activity)
	chat.AccountID, chat.Network, chat.Type = account, "Slack", "group"
	chat.Participants = []map[string]any{{"id": "@me:beeper.local", "fullName": "Test User", "isSelf": true}, member}
	chat.Msgs[0].SenderID = userID
	return chat
}

// A shared email that reaches one side of an existing suggestion through a
// separate chat must still explain the suggestion after matching completed.
func TestLateSharedEmailExplainsExistingSuggestion(t *testing.T) {
	t.Parallel()
	assert, require := assert.New(t), require.New(t)
	f := newFakeBeeper(t)
	base := time.Now().Add(-48 * time.Hour)
	first := slackRosterChat("!sam-first:example.test", "slack", "@slack_sam:beeper.local", base, map[string]any{
		"fullName": "Sam Example", "username": "sam", "phoneNumber": "+12025550141",
	})
	f.addChat(first)
	f.addChat(slackRosterChat("!sam-second:example.test", "slack2", "@slack2_sam:beeper.local", base, map[string]any{
		"fullName": "Sam Example", "username": "sam",
		"phoneNumber": "+12025550142", "email": "sam@example.test",
	}))
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	for _, account := range []string{"slack", "slack2"} {
		_, err := imp.Import(t.Context(), ImportOptions{AccountID: account, NoMedia: true})
		require.NoError(err)
	}
	candidates, err := st.ListIdentityMatchCandidatesContext(t.Context(), nil, 10, 0)
	require.NoError(err)
	require.Len(candidates, 1)
	require.Equal(store.IdentityMatchServiceScopeUsername, candidates[0].Basis)
	samSecond := participantForBeeperUser(t, st, "@slack2_sam:beeper.local")
	require.Equal("sam@example.test", observationValues(t, st, samSecond)[store.ContactAddressEmail])
	sharedEmail := func() *store.IdentityMatchEvidence {
		candidate, err := st.GetIdentityMatchCandidateContext(t.Context(), candidates[0].ID)
		require.NoError(err)
		for i, evidence := range candidate.Evidence {
			if evidence.EvidenceKind == evidenceEmail && evidence.Detail != nil && *evidence.Detail == "sam@example.test" {
				return &candidate.Evidence[i]
			}
		}
		return nil
	}
	require.Nil(sharedEmail(), "the first account has not reported the email yet")

	// The email arrives in a separate chat without a username.
	f.addChat(slackRosterChat("!sam-email:example.test", "slack", "@slack_sam:beeper.local", time.Now(), map[string]any{
		"fullName":    "Sam Example",
		"phoneNumber": "+12025550141", "email": "sam@example.test",
	}))
	_, err = imp.Import(t.Context(), ImportOptions{AccountID: "slack", NoMedia: true})
	require.NoError(err)
	require.Equal("sam@example.test", observationValues(t, st, participantForBeeperUser(t, st, "@slack_sam:beeper.local"))[store.ContactAddressEmail],
		"the separate chat recorded the email")
	assert.NotNil(sharedEmail(), "the newly shared email must explain the existing suggestion")
}

// A service registered for the account after its roster settled must reach
// the unchanged members on the next visit.
func TestServiceRegistrationRematchesSettledRoster(t *testing.T) {
	t.Parallel()
	assert, require := assert.New(t), require.New(t)
	f := newFakeBeeper(t)
	chat := budgetTestChat("!mystery-room:example.test", 1, time.Now().Add(-48*time.Hour))
	chat.AccountID, chat.Network, chat.Type = "acct-custom", "Mystery Bridge", "group"
	chat.Participants = []map[string]any{
		{"id": "@me:beeper.local", "fullName": "Test User", "isSelf": true},
		{"id": "@bridge-a_alice:beeper.local", "fullName": "Alice Example", "username": "alice"},
	}
	chat.Msgs[0].SenderID = "@bridge-a_alice:beeper.local"
	f.addChat(chat)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	opts := ImportOptions{AccountID: "acct-custom", NoMedia: true}
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	alice := participantForBeeperUser(t, st, "@bridge-a_alice:beeper.local")
	usernameServices := func() []string {
		observations, err := st.ListParticipantObservationsContext(t.Context(), alice, true)
		require.NoError(err)
		var slugs []string
		for _, observation := range observations {
			if observation.AddressKind == store.ContactAddressUsername && observation.ServiceSlug != nil {
				slugs = append(slugs, *observation.ServiceSlug)
			}
		}
		return slugs
	}
	require.Equal([]string{"bridge-a"}, usernameServices())

	configured, created, err := st.EnsureCommunicationServiceContext(t.Context(), store.CommunicationServiceInput{
		Slug: "acct-custom", DisplayLabel: "Configured Account Service",
		ScopePolicy: store.ScopePolicyOptional, Normalization: store.NormalizationLower, NormalizationVersion: 1,
	})
	require.NoError(err)
	require.True(created)
	f.appendMsg(chat.ID, fakeMsg{
		ID: chat.ID + "-new", SortKey: 1, Timestamp: time.Now(), Text: "new", SenderID: "@bridge-a_alice:beeper.local",
	})
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)

	assert.Contains(usernameServices(), "acct-custom", "the registered service must reach the unchanged member")
	var serviceID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT COALESCE(service_id, 0) FROM participant_identifiers WHERE participant_id = ? AND identifier_type = ?`),
		alice, participantIdentifierType).Scan(&serviceID))
	assert.Equal(configured.ID, serviceID, "the member's Beeper identifier moves to the registered service")
}

func TestReboundMemberRematchesSettledRoster(t *testing.T) {
	t.Parallel()
	assert, require := assert.New(t), require.New(t)
	f := newFakeBeeper(t)
	base := time.Now().Add(-48 * time.Hour)
	// The phone chat is listed first, so each import rebinds ann before the
	// username chat is visited.
	phoneChat := budgetTestChat("!rebind-phone:example.test", 1, base)
	phoneChat.Participants[1]["phoneNumber"] = "+12025550123"
	usernameChat := budgetTestChat("!rebind-username:example.test", 1, base)
	usernameChat.Participants[1]["username"] = "annhandle"
	f.addChat(phoneChat)
	f.addChat(usernameChat)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	_, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true})
	require.NoError(err)
	previous := participantForBeeperUser(t, st, "@signal_ann:beeper.local")
	require.Equal("annhandle", observationValues(t, st, previous)[store.ContactAddressUsername])

	f.mu.Lock()
	phoneChat.Participants[1]["phoneNumber"] = "+12025550199"
	f.mu.Unlock()
	for _, chat := range []*fakeChat{phoneChat, usernameChat} {
		f.appendMsg(chat.ID, fakeMsg{
			ID: chat.ID + "-new", SortKey: 1, Timestamp: time.Now(), Text: "new", SenderID: "@signal_ann:beeper.local",
		})
	}
	_, err = imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true})
	require.NoError(err)

	current := participantForBeeperUser(t, st, "@signal_ann:beeper.local")
	require.NotEqual(previous, current, "the changed phone rebinds ann")
	assert.Equal("annhandle", observationValues(t, st, current)[store.ContactAddressUsername],
		"the username chat must record ann's username for the new participant")
}
