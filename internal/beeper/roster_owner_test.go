package beeper

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestImportDefersTruncatedRosterAndContinues(t *testing.T) {
	for _, incomplete := range []bool{false, true} {
		name := "failed detail"
		if incomplete {
			name = "truncated detail"
		}
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			f := newFakeBeeper(t)
			old := budgetTestChat("!old:example.test", 1, time.Now().Add(-time.Hour))
			healthy := budgetTestChat("!healthy:example.test", 1, time.Now().Add(-time.Hour))
			for _, chat := range []*fakeChat{old, healthy} {
				chat.Participants[0]["id"] = "@signal_user:example.test"
				chat.Msgs[0].SenderID = "@signal_user:example.test"
				f.addChat(chat)
			}
			old.Participants[0]["phoneNumber"] = "+12025550123"
			healthy.Participants[0]["phoneNumber"] = "+12025550124"
			old.Participants[0], old.Participants[1] = old.Participants[1], old.Participants[0]
			old.ParticipantsTruncated = true
			imp, st := newHandlerTestImporter(t, f)
			t.Cleanup(func() { require.NoError(st.Close()) })
			opts := ImportOptions{AccountID: "signal", NoMedia: true, Scheduled: true}
			sum, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			oldOwner, err := st.EnsureParticipantByPhone(t.Context(), "+12025550123", "", "signal")
			require.NoError(err)
			newOwner, err := st.EnsureParticipantByPhone(t.Context(), "+12025550124", "", "signal")
			require.NoError(err)
			require.NotEqual(oldOwner, newOwner)
			run, err := st.GetLastSuccessfulSync(sum.SourceID)
			require.NoError(err)
			state, err := LoadSyncState(run.CursorAfter.String)
			require.NoError(err)
			work := state.Chats[old.ID].RosterWork
			require.True(work.Verified)
			work.MemberFirst = false
			receipts, err := st.BeeperRosterReceiptsContext(t.Context(), sum.SourceID, rosterProvider(old.ID))
			require.NoError(err)
			signature, listingSignature := work.Signature, work.ListingSignature
			before := *state.Chats[old.ID]
			blob, err := state.Marshal()
			require.NoError(err)
			_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE sync_runs SET cursor_after = ? WHERE id = ?`), blob, run.ID)
			require.NoError(err)
			now := time.Now()
			f.appendMsg(old.ID, fakeMsg{ID: "old-new", SortKey: 100, Timestamp: now.Add(time.Second), SenderID: "@signal_user:example.test", Text: "old"})
			f.appendMsg(healthy.ID, fakeMsg{ID: "healthy-new", SortKey: 100, Timestamp: now, SenderID: "@signal_user:example.test", Text: "healthy"})
			if incomplete {
				f.mu.Lock()
				old.DetailTruncated = true
				f.mu.Unlock()
			} else {
				f.setChatGetFailure(old.ID, true)
			}
			for attempt := range 2 {
				messageID := "healthy-new"
				if attempt > 0 {
					messageID = fmt.Sprintf("healthy-retry-%d", attempt)
					f.appendMsg(healthy.ID, fakeMsg{ID: messageID, SortKey: 100 + attempt, Timestamp: now.Add(time.Duration(attempt+2) * time.Second), SenderID: "@signal_user:example.test", Text: "healthy"})
				}
				imp = NewImporter(st, imp.client)
				sum, err = imp.Import(t.Context(), opts)
				var partial *PartialSyncError
				require.ErrorAs(err, &partial)
				require.Positive(sum.FetchErrors)
				require.Positive(sum.Errors)
				var count int
				require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND source_message_id = ?`), sum.SourceID, messageID).Scan(&count))
				require.Equal(1, count)
				require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND source_message_id = ?`), sum.SourceID, "old-new").Scan(&count))
				require.Zero(count)
				state, err = imp.loadResumeState(t.Context(), sum.SourceID, false)
				require.NoError(err)
				require.Equal(before.Newest, state.Chats[old.ID].Newest)
				require.Equal(before.Oldest, state.Chats[old.ID].Oldest)
				require.False(state.Chats[old.ID].RosterWork.MemberFirst)
				require.Equal(signature, state.Chats[old.ID].RosterWork.Signature)
				require.Equal(listingSignature, state.Chats[old.ID].RosterWork.ListingSignature)
				afterReceipts, err := st.BeeperRosterReceiptsContext(t.Context(), sum.SourceID, rosterProvider(old.ID))
				require.NoError(err)
				require.Equal(receipts, afterReceipts)
			}
			f.setChatGetFailure(old.ID, false)
			f.mu.Lock()
			old.DetailTruncated = false
			f.mu.Unlock()
			imp = NewImporter(st, imp.client)
			_, err = imp.Import(t.Context(), opts)
			require.NoError(err)
			var sender int64
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT sender_id FROM messages WHERE source_id = ? AND source_message_id = ?`), sum.SourceID, "old-new").Scan(&sender))
			require.Equal(oldOwner, sender)
		})
	}
}

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
	opts := ImportOptions{AccountID: "signal", NoMedia: true, ShouldStop: func() bool { return false }}
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
		sum := &ImportSummary{}
		_, complete, err := imp.syncChat(t.Context(), 0, sourceID, detail, opts, state, time.Now().Add(-time.Hour), false, false, sum)
		require.NoError(err)
		require.False(complete)
		require.Positive(sum.FetchErrors)
		require.False(state.Chats[old.ID].RosterWork.MemberFirst)
		var count int
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND source_message_id = ?`), sourceID, "new-old-phone-message").Scan(&count))
		require.Zero(count)
		f.setChatGetFailure(old.ID, false)
		f.mu.Lock()
		old.DetailTruncated = true
		f.mu.Unlock()
		state.Chats[old.ID].RosterWork.MemberFirst = false
		sum = &ImportSummary{}
		_, complete, err = imp.syncChat(t.Context(), 0, sourceID, detail, opts, state, time.Now().Add(-time.Hour), false, false, sum)
		require.NoError(err)
		require.False(complete)
		require.Positive(sum.FetchErrors)
		require.False(state.Chats[old.ID].RosterWork.MemberFirst)
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
