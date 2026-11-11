package beeper

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestRosterVerificationRejectsChangedSnapshot(t *testing.T) {
	t.Run("addition changes again", func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		f := newFakeBeeper(t)
		chat := budgetTestChat("!verify-addition:example.test", 2, time.Now())
		f.addChat(chat)
		imp, st := newHandlerTestImporter(t, f)
		t.Cleanup(func() { require.NoError(st.Close()) })
		sourceID := newBeeperTestSource(t, st, "signal")
		work := &rosterWork{}
		detail, err := imp.client.GetChat(t.Context(), chat.ID)
		require.NoError(err)
		_, complete, _, err := imp.ensureConversation(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "signal", rosterState: work}, &ImportSummary{})
		require.NoError(err)
		require.True(complete)
		f.mu.Lock()
		chat.Participants = append(chat.Participants, map[string]any{"id": "@signal_joined:example.test"})
		f.mu.Unlock()
		detail, err = imp.client.GetChat(t.Context(), chat.ID)
		require.NoError(err)
		f.mu.Lock()
		chat.Participants = append(chat.Participants, map[string]any{"id": "@signal_later:example.test"})
		f.mu.Unlock()
		_, complete, _, err = imp.ensureConversation(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "signal", rosterState: work}, &ImportSummary{})
		require.NoError(err)
		assert.False(complete)
		assert.False(work.Verified)
		assert.Len(rosterMembers(t, st, sourceID, chat.ID), 3)
		receipts, err := st.BeeperRosterReceiptsContext(t.Context(), sourceID, rosterProvider(chat.ID))
		require.NoError(err)
		assert.Len(receipts, 3)
		pending, err := st.ListUnresolvedConversationIDs(t.Context(), sourceID)
		require.NoError(err)
		assert.Contains(pending, chat.ID)
	})
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
			work := &rosterWork{}
			detail, err := imp.client.GetChat(t.Context(), chat.ID)
			require.NoError(err)
			_, complete, _, err := imp.ensureConversation(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "signal", rosterState: work}, &ImportSummary{})
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
			_, complete, _, err = imp.ensureConversation(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "signal", rosterState: work}, sum)
			require.NoError(err)
			assert.False(complete)
			assert.Zero(sum.FetchErrors)
			assert.Len(rosterMembers(t, st, sourceID, chat.ID), 3, "the departed member stays until a read confirms the roster")
			assert.False(work.Verified)
			unresolved, err := st.ListUnresolvedConversationIDs(t.Context(), sourceID)
			require.NoError(err)
			assert.Contains(unresolved, chat.ID)
		})
	}
}

func TestTruncatedChangedListingDefersRoster(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newFakeBeeper(t)
	chat := budgetTestChat("!changed-listing:example.test", 2, time.Now())
	chat.ParticipantsTruncated = true
	f.addChat(chat)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	sum, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true})
	require.NoError(err)
	state := latestBeeperState(t, imp).Chats[chat.ID]
	require.True(state.RosterWork.Verified)
	f.mu.Lock()
	chat.Participants[0]["fullName"] = "Changed"
	f.mu.Unlock()
	listing, err := imp.client.SearchChats(t.Context(), SearchChatsParams{AccountID: "signal"})
	require.NoError(err)
	require.Len(listing.Items, 1)
	_, complete, _, err := imp.ensureConversation(t.Context(), 0, sum.SourceID, &listing.Items[0], ImportOptions{AccountID: "signal", skipRoster: true, rosterState: state.RosterWork, routeAccount: &Account{AccountID: "signal", Bridge: &AccountBridge{Type: "signal"}, Status: "connected"}}, &ImportSummary{})
	require.NoError(err)
	assert.False(complete)
	unresolved, err := st.ListUnresolvedConversationIDs(t.Context(), sum.SourceID)
	require.NoError(err)
	assert.Contains(unresolved, chat.ID)
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
	chat.LastActivity = time.Now()
	chat.Msgs = append(chat.Msgs, fakeMsg{ID: "unchanged", SortKey: 98, Timestamp: chat.LastActivity, Text: "new", SenderID: "@signal_ann:beeper.local"})
	f.mu.Unlock()
	_, err = imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true})
	require.NoError(err)
	assert.Zero(detailRequests(), "an unchanged roster needs no verification read")
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

func TestRosterWorkCheckpointClear(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	state := NewSyncState()
	state.EnsureChat("room").RosterWork = &rosterWork{Signature: "snapshot", Phase: "matching", Next: 1, Verified: true}
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
	_, _, _, err = imp.ensureConversation(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "signal"}, &ImportSummary{}) //nolint:dogsled // Only the error matters for the first visit.
	require.NoError(err)
	before := rosterMembers(t, st, sourceID, chat.ID)
	detail.Participants.Items = detail.Participants.Items[:2]
	detail.Participants.Total = 2
	f.setChatGetFailure(chat.ID, true)
	work := &rosterWork{}
	sum := &ImportSummary{}
	_, complete, _, err := imp.ensureConversation(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "signal", rosterState: work}, sum)
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
	state := &ChatState{RosterWork: &rosterWork{Signature: rosterSignature(detail, "signal"), Phase: "members"}}
	blob, err := (&SyncState{Chats: map[string]*ChatState{chat.ID: state}}).Marshal()
	require.NoError(err)
	restored, err := LoadSyncState(blob)
	require.NoError(err)
	imp = NewImporter(st, imp.client)
	_, _, _, err = imp.ensureConversation(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "signal", rosterState: restored.Chats[chat.ID].RosterWork}, &ImportSummary{}) //nolint:dogsled // Only the error matters here.
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
	work := &rosterWork{}
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
		_, complete, _, err := imp.ensureConversation(t.Context(), 0, sum.SourceID, detail, ImportOptions{AccountID: "signal", rosterState: work}, &ImportSummary{})
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
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE participants SET display_name = NULL WHERE id = ?`), id)
	require.NoError(err)
	imp = NewImporter(st, imp.client)
	_, err = imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true, Full: true})
	require.NoError(err)
	name, _ = saved()
	assert.Equal("Updated Name", name)
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
	state, err := imp.loadResumeState(t.Context(), sum.SourceID, false)
	require.NoError(err)
	cs := state.Chats[chat.ID]
	require.True(cs.Done)
	detail, err := imp.client.GetChat(t.Context(), chat.ID)
	require.NoError(err)
	cs.RosterWork = &rosterWork{Signature: rosterSignature(detail, "signal"), Phase: "matching", Verified: true}
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
