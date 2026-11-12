package beeper

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRosterBareSenderFollowsCurrentOwner(t *testing.T) {
	for _, omitted := range []bool{false, true} {
		t.Run(fmt.Sprintf("omitted=%t", omitted), func(t *testing.T) {
			require := require.New(t)
			f := newFakeBeeper(t)
			old := budgetTestChat("!bare:example.test", 1, time.Now().Add(-time.Hour))
			healthy := budgetTestChat("!rich:example.test", 1, time.Now().Add(-time.Hour))
			for _, chat := range []*fakeChat{old, healthy} {
				chat.Participants[0]["id"] = "@signal_shared:example.test"
				chat.Msgs[0].SenderID = "@signal_shared:example.test"
			}
			healthy.Participants[0]["phoneNumber"] = "+12025550124"
			if omitted {
				old.Participants[0], old.Participants[1] = old.Participants[1], old.Participants[0]
			}
			old.ParticipantsTruncated = true
			f.addChat(old)
			imp, st := newHandlerTestImporter(t, f)
			t.Cleanup(func() { require.NoError(st.Close()) })
			ownerA, err := newParticipantResolver(st, "signal").resolveUser(t.Context(), &User{ID: "@signal_shared:example.test", PhoneNumber: "+12025550123"})
			require.NoError(err)
			opts := ImportOptions{AccountID: "signal", NoMedia: true, Scheduled: true}
			sum, err := imp.Import(t.Context(), opts)
			require.NoError(err)
			f.addChat(healthy)
			imp = NewImporter(st, imp.client)
			_, err = imp.Import(t.Context(), opts)
			require.NoError(err)
			ownerB := participantForBeeperUser(t, st, "@signal_shared:example.test")
			require.NotEqual(ownerA, ownerB)
			run, err := st.GetLastSuccessfulSync(sum.SourceID)
			require.NoError(err)
			state, err := LoadSyncState(run.CursorAfter.String)
			require.NoError(err)
			state.Chats[old.ID].RosterWork.MemberFirst = false
			blob, err := state.Marshal()
			require.NoError(err)
			_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE sync_runs SET cursor_after = ? WHERE id = ?`), blob, run.ID)
			require.NoError(err)
			f.appendMsg(old.ID, fakeMsg{ID: "bare-new", SortKey: 100, Timestamp: time.Now(), SenderID: "@signal_shared:example.test", Text: "new"})
			imp = NewImporter(st, imp.client)
			_, err = imp.Import(t.Context(), opts)
			require.NoError(err)
			var sender int64
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT sender_id FROM messages WHERE source_id = ? AND source_message_id = ?`), sum.SourceID, "bare-new").Scan(&sender))
			require.Equal(ownerB, sender)
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT sender_id FROM messages WHERE source_id = ? AND source_message_id = ?`), sum.SourceID, old.Msgs[0].ID).Scan(&sender))
			require.Equal(ownerA, sender)
		})
	}
}

func TestImportDefersTruncatedRosterAndContinues(t *testing.T) {
	for _, incomplete := range []bool{false, true} {
		t.Run(fmt.Sprintf("incomplete=%t", incomplete), func(t *testing.T) {
			testImportDefersTruncatedRoster(t, incomplete, false, false)
		})
	}
	t.Run("complete listing verification failure", testCompleteListingVerificationFailure)
}

func testImportDefersTruncatedRoster(t *testing.T, incomplete, omitted, checkSender bool) {
	t.Helper()
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
	if omitted {
		old.Participants[0], old.Participants[1] = old.Participants[1], old.Participants[0]
	}
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
	require.Equal(newOwner, participantForBeeperUser(t, st, "@signal_user:example.test"))
	run, err := st.GetLastSuccessfulSync(sum.SourceID)
	require.NoError(err)
	state, err := LoadSyncState(run.CursorAfter.String)
	require.NoError(err)
	initialWatermark := parseWatermark(state.ListWatermark)
	require.False(initialWatermark.IsZero())
	work := state.Chats[old.ID].RosterWork
	require.True(work.Verified)
	work.MemberFirst = false
	blob, err := state.Marshal()
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE sync_runs SET cursor_after = ? WHERE id = ?`), blob, run.ID)
	require.NoError(err)
	now := time.Now()
	f.appendMsg(healthy.ID, fakeMsg{ID: "healthy-new", SortKey: 100, Timestamp: now, SenderID: "@signal_user:example.test", Text: "healthy"})
	if incomplete {
		f.mu.Lock()
		old.DetailTruncated = true
		f.mu.Unlock()
	} else {
		f.setChatGetFailure(old.ID, true)
	}
	for attempt := range 2 {
		oldMessage := fmt.Sprintf("old-new-%d", attempt)
		f.appendMsg(old.ID, fakeMsg{ID: oldMessage, SortKey: 100 + attempt, Timestamp: now.Add(time.Duration(attempt+1) * time.Second), SenderID: "@signal_user:example.test", Text: "old"})
		messageID := "healthy-new"
		if attempt > 0 {
			messageID = fmt.Sprintf("healthy-retry-%d", attempt)
			f.appendMsg(healthy.ID, fakeMsg{ID: messageID, SortKey: 100 + attempt, Timestamp: now.Add(time.Duration(attempt+2) * time.Second), SenderID: "@signal_user:example.test", Text: "healthy"})
		}
		imp = NewImporter(st, imp.client)
		sum, err = imp.Import(t.Context(), opts)
		if attempt == 0 || incomplete {
			require.NoError(err)
			require.Zero(sum.FetchErrors)
		} else {
			var partial *PartialSyncError
			require.ErrorAs(err, &partial)
			require.Positive(sum.FetchErrors)
		}
		var count int
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id = ? AND source_message_id = ?`), sum.SourceID, messageID).Scan(&count))
		require.Equal(1, count)
		if checkSender {
			var sender int64
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT sender_id FROM messages WHERE source_id = ? AND source_message_id = ?`), sum.SourceID, oldMessage).Scan(&sender))
			require.Equal(oldOwner, sender)
		}
		unresolved, err := st.ListUnresolvedConversationIDs(t.Context(), sum.SourceID)
		require.NoError(err)
		require.Contains(unresolved, old.ID)
		if attempt == 1 {
			if incomplete {
				var deferred int
				require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM sync_run_items WHERE source_message_id = ? AND error_kind = 'beeper_roster_verification_deferred'`), old.ID).Scan(&deferred))
				require.Positive(deferred)
				require.True(parseWatermark(latestBeeperState(t, imp).ListWatermark).After(initialWatermark))
			}
		}
	}
	if checkSender && !omitted {
		f.mu.Lock()
		old.Participants[0]["phoneNumber"] = "+12025550125"
		f.mu.Unlock()
		f.appendMsg(old.ID, fakeMsg{ID: "old-changed-phone", SortKey: 102, Timestamp: now.Add(5 * time.Second), SenderID: "@signal_user:example.test", Text: "changed"})
		imp = NewImporter(st, imp.client)
		_, err = imp.Import(t.Context(), opts)
		require.NoError(err)
		changedOwner, err := st.EnsureParticipantByPhone(t.Context(), "+12025550125", "", "signal")
		require.NoError(err)
		var sender int64
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT sender_id FROM messages WHERE source_id = ? AND source_message_id = ?`), sum.SourceID, "old-changed-phone").Scan(&sender))
		require.Equal(changedOwner, sender)
	}
	f.setChatGetFailure(old.ID, false)
	f.mu.Lock()
	old.DetailTruncated = false
	f.mu.Unlock()
	imp = NewImporter(st, imp.client)
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	imp = NewImporter(st, imp.client)
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	unresolved, err := st.ListUnresolvedConversationIDs(t.Context(), sum.SourceID)
	require.NoError(err)
	require.NotContains(unresolved, old.ID)
	if checkSender {
		var sender int64
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT sender_id FROM messages WHERE source_id = ? AND source_message_id = ?`), sum.SourceID, "old-new-0").Scan(&sender))
		require.Equal(oldOwner, sender)
	}
}

func TestRosterEachChatKeepsItsOwnPhone(t *testing.T) {
	for _, tc := range []struct {
		name                string
		incomplete, omitted bool
	}{
		{"failed omitted", false, true}, {"failed visible", false, false},
		{"truncated omitted", true, true}, {"truncated visible", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) { testImportDefersTruncatedRoster(t, tc.incomplete, tc.omitted, true) })
	}

	for _, tc := range []struct {
		name                            string
		messageFirst, sameRun, verified bool
	}{
		{"member first", false, false, false},
		{"message first", true, false, false},
		{"one run", false, true, false},
		{"changed owner", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testRosterEachChatKeepsItsOwnPhone(t, tc.messageFirst, tc.sameRun, tc.verified)
		})
	}
}

func testRosterEachChatKeepsItsOwnPhone(t *testing.T, messageFirst, sameRun, verified bool) {
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
	if verified {
		require.False(state.Chats[old.ID].RosterWork.Verified)
	}
}

func testCompleteListingVerificationFailure(t *testing.T) {
	require := require.New(t)
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
	require.Equal(6, countBeeperMessages(t, imp))
}
