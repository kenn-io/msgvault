package beeper

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
)

func TestRosterEachChatKeepsItsOwnPhone(t *testing.T) {
	for _, tc := range []struct {
		name                                       string
		messageFirst, sameRun, verified, truncated bool
	}{
		{"member first", false, false, false, false},
		{"message first", true, false, false, false},
		{"one run", false, true, false, false},
		{"changed owner", true, false, true, false},
		{"truncated message first", true, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testRosterEachChatKeepsItsOwnPhone(t, tc.messageFirst, tc.sameRun, tc.verified, tc.truncated)
		})
	}
}

func testRosterEachChatKeepsItsOwnPhone(t *testing.T, messageFirst, sameRun, verified, truncated bool) {
	t.Helper()
	require := require.New(t)
	f := newFakeBeeper(t)
	old := budgetTestChat("!old-phone:example.test", 1, time.Now().Add(-time.Hour))
	newer := budgetTestChat("!new-phone:example.test", 1, time.Now().Add(-time.Hour))
	for _, chat := range []*fakeChat{old, newer} {
		chat.Participants[0]["id"] = "@signal_user:example.test"
		chat.Msgs[0].SenderID = "@signal_user:example.test"
		f.addChat(chat)
	}
	old.Participants[0]["phoneNumber"] = "+12025550123"
	if truncated {
		old.Participants[0], old.Participants[1] = old.Participants[1], old.Participants[0]
		old.ParticipantsTruncated = true
	}
	newer.Participants[0]["phoneNumber"] = "+12025550124"
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	sourceID := newBeeperTestSource(t, st, "signal")
	oldOwner, err := st.EnsureParticipantByPhone(t.Context(), "+12025550123", "", "signal")
	require.NoError(err)
	newOwner, err := st.EnsureParticipantByPhone(t.Context(), "+12025550124", "", "signal")
	require.NoError(err)
	state := NewSyncState()
	opts := ImportOptions{AccountID: "signal", NoMedia: true}
	for _, chat := range []*fakeChat{old, newer} {
		if !sameRun {
			imp = NewImporter(st, imp.client)
		}
		detail, err := imp.client.GetChat(t.Context(), chat.ID)
		require.NoError(err)
		if truncated && chat == old {
			listing, err := imp.client.SearchChats(t.Context(), SearchChatsParams{AccountID: "signal"})
			require.NoError(err)
			for i := range listing.Items {
				if listing.Items[i].ID == old.ID {
					detail = &listing.Items[i]
				}
			}
			require.True(detail.Participants.HasMore)
		}
		_, _, err = imp.syncChat(t.Context(), 0, sourceID, detail, opts, state, time.Now().Add(-time.Hour), false, false, &ImportSummary{})
		require.NoError(err)
	}
	var newerSender int64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT sender_id FROM messages WHERE source_id = ? AND source_message_id = ?`), sourceID, newer.Msgs[0].ID).Scan(&newerSender))
	require.Equal(newOwner, newerSender)
	require.NotEqual(oldOwner, newOwner)
	f.appendMsg(old.ID, fakeMsg{ID: "new-old-phone-message", SortKey: 100, Timestamp: time.Now(), SenderID: "@signal_user:example.test", Text: "Hello"})
	if !sameRun {
		imp = NewImporter(st, imp.client)
	}
	detail, err := imp.client.GetChat(t.Context(), old.ID)
	require.NoError(err)
	if truncated {
		listing, err := imp.client.SearchChats(t.Context(), SearchChatsParams{AccountID: "signal"})
		require.NoError(err)
		for i := range listing.Items {
			if listing.Items[i].ID == old.ID {
				detail = &listing.Items[i]
			}
		}
		require.True(detail.Participants.HasMore)
		f.setChatGetFailure(old.ID, true)
		state.Chats[old.ID].RosterWork.MemberFirst = false
		_, _, err = imp.syncChat(t.Context(), 0, sourceID, detail, opts, state, time.Now().Add(-time.Hour), false, false, &ImportSummary{})
		require.Error(err)
		var count int
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND source_message_id = ?`), sourceID, "new-old-phone-message").Scan(&count))
		require.Zero(count)
		f.setChatGetFailure(old.ID, false)
		f.mu.Lock()
		old.DetailTruncated = true
		f.mu.Unlock()
		state.Chats[old.ID].RosterWork.MemberFirst = false
		_, _, err = imp.syncChat(t.Context(), 0, sourceID, detail, opts, state, time.Now().Add(-time.Hour), false, false, &ImportSummary{})
		require.ErrorContains(err, "incomplete roster")
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND source_message_id = ?`), sourceID, "new-old-phone-message").Scan(&count))
		require.Zero(count)
		f.mu.Lock()
		old.DetailTruncated = false
		f.mu.Unlock()
	}
	if verified {
		work := state.Chats[old.ID].RosterWork
		require.NotNil(work)
		require.True(work.Verified)
		require.Equal("done", work.Phase)
		require.NotEmpty(work.Signature)
		require.NotEmpty(work.ListingSignature)
		require.Equal(newOwner, participantForBeeperUser(t, st, "@signal_user:example.test"))
		work.MemberFirst = false
	} else if messageFirst {
		state.Chats[old.ID].RosterWork = &rosterWork{MemberFirst: false, Phase: "matching", Signature: rosterSignature(detail, "signal")}
	}
	_, _, err = imp.syncChat(t.Context(), 0, sourceID, detail, opts, state, time.Now().Add(-time.Hour), false, false, &ImportSummary{})
	require.NoError(err)
	var sender int64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT sender_id FROM messages WHERE source_id = ? AND source_message_id = ?`), sourceID, "new-old-phone-message").Scan(&sender))
	require.Equal(oldOwner, sender)
	require.Contains(rosterMembers(t, st, sourceID, old.ID), oldOwner)
	if verified && !truncated {
		require.False(state.Chats[old.ID].RosterWork.Verified)
	}
}

func TestRosterMessageFirstUsesListedMediaCount(t *testing.T) {
	require := require.New(t)
	f := newFakeBeeper(t)
	chat := mediaChat()
	f.addChat(chat)
	f.setAsset("mxc://beeper.local/photo1", []byte("photo"))
	imp, st, done := newTestImporter(t, f)
	defer done()
	opts := mediaImportOptions(t)
	opts.MediaPolicy = attachmentpolicy.Policy{MaxParticipants: 5}
	opts.ShouldStop = func() bool {
		var n int
		require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM conversation_participants`).Scan(&n))
		return n == 1
	}
	first, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	require.True(first.Stopped)
	opts.ShouldStop = nil
	imp = NewImporter(st, imp.client)
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	require.NotEmpty(assetRequests(f))
}

func TestRosterResumeRechecksOwnerOfSettledPrefix(t *testing.T) {
	require := require.New(t)
	f := newFakeBeeper(t)
	chat := budgetTestChat("!prefix-owner:example.test", 1, time.Now())
	chat.Participants[0]["id"] = "@signal_a:example.test"
	chat.Participants[0]["phoneNumber"] = "+12025550123"
	chat.Participants[1]["id"] = "@signal_b:example.test"
	f.addChat(chat)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	sourceID := newBeeperTestSource(t, st, "signal")
	oldOwner, err := st.EnsureParticipantByPhone(t.Context(), "+12025550123", "", "signal")
	require.NoError(err)
	_, err = st.EnsureParticipantByPhone(t.Context(), "+12025550124", "", "signal")
	require.NoError(err)
	detail, err := imp.client.GetChat(t.Context(), chat.ID)
	require.NoError(err)
	work := &rosterWork{}
	//nolint:dogsled // Only the interruption matters.
	_, _, _, err = imp.ensureConversation(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "signal", rosterState: work, ShouldStop: func() bool {
		var count int
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM source_import_items WHERE source_id = ? AND provider = ?`), sourceID, rosterProvider(chat.ID)).Scan(&count))
		return count >= 1
	}}, &ImportSummary{})
	require.ErrorIs(err, errBeeperBudgetExpired)
	_, err = newParticipantResolver(st, "signal").resolveUser(t.Context(), &User{ID: "@signal_a:example.test", PhoneNumber: "+12025550124"})
	require.NoError(err)
	imp = NewImporter(st, imp.client)
	_, complete, _, err := imp.ensureConversation(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "signal", rosterState: work}, &ImportSummary{})
	require.NoError(err)
	require.True(complete)
	require.Contains(rosterMembers(t, st, sourceID, chat.ID), oldOwner)
}
