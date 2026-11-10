package beeper

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
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
			f.addChat(chat)
			imp, st := newHandlerTestImporter(t, f)
			t.Cleanup(func() { require.NoError(st.Close()) })
			sourceID := newBeeperTestSource(t, st, "signal")
			detail, err := imp.client.GetChat(t.Context(), chat.ID)
			require.NoError(err)
			f.mu.Lock()
			tc.change(chat)
			f.mu.Unlock()
			state := &ChatState{}
			sum := &ImportSummary{}
			_, complete, _, _, err := imp.ensureConversation(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "signal"}, sum, state)
			require.NoError(err)
			assert.False(complete)
			assert.Equal(int64(1), sum.FetchErrors)
			assert.Len(rosterMembers(t, st, sourceID, chat.ID), 2)
			require.NotNil(state.RosterWork)
			assert.False(state.RosterWork.MembershipApplied)
		})
	}
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

func TestRosterWorkCheckpointClear(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	state := NewSyncState()
	state.EnsureChat("room").RosterWork = &rosterWork{Signature: "snapshot", Members: map[string]rosterMemberWork{"member": {Inputs: "inputs", Enriched: true}}, MembershipApplied: true}
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
	f.addChat(chat)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	sourceID := newBeeperTestSource(t, st, "signal")
	detail, err := imp.client.GetChat(t.Context(), chat.ID)
	require.NoError(err)
	_, _, _, _, err = imp.ensureConversation(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "signal"}, &ImportSummary{}, &ChatState{}) //nolint:dogsled // This check needs only the error or observations.
	require.NoError(err)
	before := rosterMembers(t, st, sourceID, chat.ID)
	f.setChatGetFailure(chat.ID, true)
	state := &ChatState{}
	sum := &ImportSummary{}
	_, complete, _, _, err := imp.ensureConversation(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "signal"}, sum, state) //nolint:dogsled // This check needs only completeness and error.
	require.NoError(err)
	assert.False(complete)
	assert.Equal(int64(1), sum.FetchErrors)
	assert.Equal(before, rosterMembers(t, st, sourceID, chat.ID))
	require.NotNil(state.RosterWork)
	assert.False(state.RosterWork.MembershipApplied)
	unresolved, err := st.ListUnresolvedConversationIDs(t.Context(), sourceID)
	require.NoError(err)
	assert.Contains(unresolved, chat.ID)
}

func TestRosterEnrichmentRetriesFailedMemberAfterLaterSuccess(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	testutil.SkipIfPostgres(t, "SQLite trigger injects one participant observation failure")
	f := newFakeBeeper(t)
	chat := observationChat("telegram", "Telegram", "@telegram_first:example.test",
		map[string]any{"id": "@telegram_first:example.test", "username": "first"},
		map[string]any{"id": "@telegram_second:example.test", "username": "second"})
	f.addChat(chat)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	sourceID := newBeeperTestSource(t, st, "telegram")
	detail, err := imp.client.GetChat(t.Context(), chat.ID)
	require.NoError(err)
	state := &ChatState{}
	opts := ImportOptions{AccountID: "telegram"}
	sum := &ImportSummary{}
	_, _, _, roster, err := imp.ensureConversation(t.Context(), 0, sourceID, detail, opts, sum, state) //nolint:dogsled // This check needs only the error or observations.
	require.NoError(err)
	first := roster.members[0].participantID
	second := roster.members[1].participantID
	_, err = st.DB().ExecContext(t.Context(), fmt.Sprintf(`CREATE TRIGGER fail_first_observation BEFORE INSERT ON participant_contact_observations WHEN NEW.participant_id = %d BEGIN SELECT RAISE(FAIL, 'injected failure'); END`, first))
	require.NoError(err)
	require.NoError(imp.captureRosterObservations(t.Context(), sourceID, opts, roster, sum))
	assert.Positive(sum.Errors)
	observations, err := st.ListParticipantObservationsContext(t.Context(), second, true)
	require.NoError(err)
	assert.NotEmpty(observations)
	require.NotNil(state.RosterWork)
	_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER fail_first_observation`)
	require.NoError(err)
	require.NoError(imp.captureRosterObservations(t.Context(), sourceID, opts, roster, sum))
	observations, err = st.ListParticipantObservationsContext(t.Context(), first, true)
	require.NoError(err)
	assert.NotEmpty(observations)
	assert.False(state.RosterWork.MatchingPending)
}

func TestRosterResumeUsesMergedParticipant(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newFakeBeeper(t)
	chat := budgetTestChat("!merged-roster:example.test", 2, time.Now())
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
	state := &ChatState{RosterWork: &rosterWork{Signature: rosterSignature(detail, "signal"), Members: map[string]rosterMemberWork{imp.res.identifierForUser(first.ID): {Inputs: "inputs"}}}}
	blob, err := (&SyncState{Chats: map[string]*ChatState{chat.ID: state}}).Marshal()
	require.NoError(err)
	winner, err := st.EnsureParticipantByPhone(t.Context(), "+12025550123", "Merged", "beeper")
	require.NoError(err)
	require.NoError(st.MergeParticipantsContext(t.Context(), old, winner))
	restored, err := LoadSyncState(blob)
	require.NoError(err)
	imp = NewImporter(st, imp.client)
	_, _, _, _, err = imp.ensureConversation(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "signal"}, &ImportSummary{}, restored.Chats[chat.ID]) //nolint:dogsled // This check needs only the error or observations.
	require.NoError(err)
	assert.Contains(rosterMembers(t, st, sourceID, chat.ID), winner)
	assert.NotContains(rosterMembers(t, st, sourceID, chat.ID), old)
}

func TestRosterVerificationFailureContinuesMessagesAndLaterChat(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newFakeBeeper(t)
	first := budgetTestChat("!verify-deferred:example.test", 2, time.Now().Add(-48*time.Hour))
	later := budgetTestChat("!verify-later:example.test", 2, time.Now().Add(-72*time.Hour))
	f.addChat(first)
	f.addChat(later)
	f.setChatGetFailure(first.ID, true)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	sum, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true})
	require.Error(err)
	require.NotNil(sum)
	assert.Equal(int64(1), sum.FetchErrors)
	assert.Equal(4, countBeeperMessages(t, imp))
	assert.Len(rosterMembers(t, st, sum.SourceID, first.ID), 2)
	assert.Len(rosterMembers(t, st, sum.SourceID, later.ID), 2)
	unresolved, err := st.ListUnresolvedConversationIDs(t.Context(), sum.SourceID)
	require.NoError(err)
	assert.Contains(unresolved, first.ID)
	assert.NotContains(unresolved, later.ID)
}

func TestChangedVerificationDefersPruningAndResumesFromDetail(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newFakeBeeper(t)
	chat := rosterRetryChat()
	f.addChat(chat)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	sum, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true})
	require.NoError(err)
	state := latestBeeperState(t, imp).Chats[chat.ID]
	f.mu.Lock()
	chat.Participants = chat.Participants[:24]
	f.mu.Unlock()
	listing, err := imp.client.GetChat(t.Context(), chat.ID)
	require.NoError(err)
	f.mu.Lock()
	chat.Participants = chat.Participants[:23]
	f.mu.Unlock()
	summary := &ImportSummary{}
	_, complete, _, _, err := imp.ensureConversation(t.Context(), 0, sum.SourceID, listing, ImportOptions{AccountID: "signal"}, summary, state) //nolint:dogsled // This check needs completeness and error.
	require.NoError(err)
	assert.False(complete)
	assert.Equal(int64(1), summary.FetchErrors)
	assert.Len(rosterMembers(t, st, sum.SourceID, chat.ID), 64, "a changed verification must not prune")
	unresolved, err := st.ListUnresolvedConversationIDs(t.Context(), sum.SourceID)
	require.NoError(err)
	assert.Contains(unresolved, chat.ID)
	imp = NewImporter(st, imp.client)
	_, complete, _, _, err = imp.ensureConversation(t.Context(), 0, sum.SourceID, listing, ImportOptions{AccountID: "signal"}, &ImportSummary{}, state) //nolint:dogsled // This check needs completeness and error.
	require.NoError(err)
	assert.True(complete)
	assert.Len(rosterMembers(t, st, sum.SourceID, chat.ID), 23)
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
	state := latestBeeperState(t, imp).Chats[chat.ID]
	for _, update := range []map[string]string{
		{"fullName": "Updated Name"},
		{"email": "updated@example.test"},
		{"phoneNumber": "+12025550123"},
	} {
		f.mu.Lock()
		for key, value := range update {
			chat.Participants[1][key] = value
		}
		f.mu.Unlock()
		detail, err := imp.client.GetChat(t.Context(), chat.ID)
		require.NoError(err)
		imp = NewImporter(st, imp.client)
		_, complete, _, _, err := imp.ensureConversation(t.Context(), 0, sum.SourceID, detail, ImportOptions{AccountID: "signal"}, &ImportSummary{}, state)
		require.NoError(err)
		require.True(complete)
	}
	id := participantForBeeperUser(t, st, "@signal_ann:beeper.local")
	var name, phone string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COALESCE(display_name, ''), COALESCE(phone_number, '') FROM participants WHERE id = ?`), id).Scan(&name, &phone))
	assert.Equal("Updated Name", name)
	assert.Equal("+12025550123", phone)
	assert.Contains(rosterMembers(t, st, sum.SourceID, chat.ID), id)
}

func TestIncompleteVerificationLeavesMembershipPending(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newFakeBeeper(t)
	chat := budgetTestChat("!verification-incomplete:example.test", 2, time.Now())
	f.addChat(chat)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	sourceID := newBeeperTestSource(t, st, "signal")
	detail, err := imp.client.GetChat(t.Context(), chat.ID)
	require.NoError(err)
	handler := f.handler()
	imp.client.http.Transport = handlerTransport(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chats/"+chat.ID {
			payload := f.chatJSON(chat, false)
			payload["participants"] = map[string]any{"items": chat.Participants, "hasMore": true, "total": len(chat.Participants)}
			writeJSON(t, w, payload)
			return
		}
		handler(w, r)
	}))
	state := &ChatState{}
	sum := &ImportSummary{}
	_, complete, _, _, err := imp.ensureConversation(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "signal"}, sum, state) //nolint:dogsled // This check needs completeness and error.
	require.NoError(err)
	assert.False(complete)
	assert.Equal(int64(1), sum.FetchErrors)
	assert.False(state.RosterWork.MembershipApplied)
	unresolved, err := st.ListUnresolvedConversationIDs(t.Context(), sourceID)
	require.NoError(err)
	assert.Contains(unresolved, chat.ID)
}

func TestRosterMatchingDuringUnfinishedHistory(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newFakeBeeper(t)
	f.pageSize = 1
	chat := budgetTestChat("!roster-backfill:example.test", 2, time.Now().Add(-48*time.Hour))
	chat.Participants[1]["providerID"] = "synthetic-native"
	f.addChat(chat)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	_, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true, Limit: 1})
	require.NoError(err)
	state := latestBeeperState(t, imp)
	require.NotNil(state.Chats[chat.ID])
	assert.False(state.Chats[chat.ID].Done)
	assert.Equal(1, countBeeperMessages(t, imp))
	var count int
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT message_count FROM conversations WHERE source_conversation_id = ?`), chat.ID).Scan(&count))
	assert.Equal(1, count, "conversation statistics include messages imported during this visit")
	peer := participantForBeeperUser(t, st, "@signal_ann:beeper.local")
	observations, err := st.ListParticipantObservationsContext(t.Context(), peer, true)
	require.NoError(err)
	assert.NotEmpty(observations)
}

func TestRosterJoinRetainsUnchangedMatchingProgress(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newFakeBeeper(t)
	chat := budgetTestChat("!roster-join:example.test", 2, time.Now().Add(-48*time.Hour))
	chat.Participants[1]["providerID"] = "native-original"
	f.addChat(chat)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	sum, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true})
	require.NoError(err)
	state := latestBeeperState(t, imp).Chats[chat.ID]
	require.NotNil(state.RosterWork)
	imp.res.accountID = "signal"
	original := imp.res.identifierForUser("@signal_ann:beeper.local")
	require.True(state.RosterWork.Members[original].Enriched)
	f.mu.Lock()
	chat.Participants = append(chat.Participants, map[string]any{"id": "@signal_joined:example.test", "providerID": "native-joined"})
	f.mu.Unlock()
	detail, err := imp.client.GetChat(t.Context(), chat.ID)
	require.NoError(err)
	imp = NewImporter(st, imp.client)
	_, complete, _, roster, err := imp.ensureConversation(t.Context(), 0, sum.SourceID, detail, ImportOptions{AccountID: "signal"}, &ImportSummary{}, state)
	require.NoError(err)
	require.True(complete)
	assert.True(state.RosterWork.Members[original].Enriched)
	assert.True(state.RosterWork.MatchingPending)
	require.NoError(imp.captureRosterObservations(t.Context(), sum.SourceID, ImportOptions{AccountID: "signal"}, roster, &ImportSummary{}))
	joined := participantForBeeperUser(t, st, "@signal_joined:example.test")
	observed, err := st.ListParticipantObservationsContext(t.Context(), joined, true)
	require.NoError(err)
	assert.NotEmpty(observed)
	assert.False(state.RosterWork.MatchingPending)
}
