package beeper

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRosterLaterChatUsesChangedPhone(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newFakeBeeper(t)
	bare := budgetTestChat("!bare:example.test", 2, time.Now())
	rich := budgetTestChat("!rich:example.test", 2, time.Now().Add(-time.Hour))
	bare.Participants[0]["id"] = "@signal_user:example.test"
	rich.Participants[0]["id"] = "@signal_user:example.test"
	rich.Participants[0]["phoneNumber"] = "+12025550124"
	for _, chat := range []*fakeChat{bare, rich} {
		for i := range chat.Msgs {
			chat.Msgs[i].SenderID = "@signal_user:example.test"
		}
	}
	f.addChat(bare)
	f.addChat(rich)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	old, err := newParticipantResolver(st, "signal").resolveUser(t.Context(), &User{ID: "@signal_user:example.test", PhoneNumber: "+12025550123"})
	require.NoError(err)
	newPhone, err := st.EnsureParticipantByPhone(t.Context(), "+12025550124", "", "signal")
	require.NoError(err)
	_, err = imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true})
	require.NoError(err)
	for _, tc := range []struct {
		messageID string
		want      int64
	}{{bare.Msgs[0].ID, old}, {rich.Msgs[0].ID, newPhone}} {
		var sender int64
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT sender_id FROM messages WHERE source_message_id = ?`), tc.messageID).Scan(&sender))
		assert.Equal(tc.want, sender)
	}
}

func TestRosterReceiptCleanupResumes(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newFakeBeeper(t)
	chat := budgetTestChat("!cleanup:example.test", 2, time.Now().Add(-48*time.Hour))
	chat.Type = "group"
	for i := 2; i < 22; i++ {
		chat.Participants = append(chat.Participants, map[string]any{"id": fmt.Sprintf("@signal_cleanup%d:example.test", i), "username": fmt.Sprintf("cleanup%d", i)})
	}
	f.addChat(chat)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	sourceID := newBeeperTestSource(t, st, "signal")
	detail, err := imp.client.GetChat(t.Context(), chat.ID)
	require.NoError(err)
	work := &rosterWork{Version: 1}
	count := func() int {
		var n int
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM source_import_items WHERE source_id = ? AND provider = ?`), sourceID, rosterProvider(chat.ID)).Scan(&n))
		return n
	}
	_, complete, _, err := imp.ensureRosterWork(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "signal", rosterState: work}, &ImportSummary{})
	require.NoError(err)
	assert.True(complete)
	assert.Equal(22, count())
	f.mu.Lock()
	chat.Participants = chat.Participants[:4]
	f.mu.Unlock()
	detail, err = imp.client.GetChat(t.Context(), chat.ID)
	require.NoError(err)
	work = &rosterWork{Version: 1}
	_, complete, _, err = imp.ensureRosterWork(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "signal", rosterState: work, ShouldStop: func() bool {
		return work.Phase == "cleanup" && count() <= 6
	}}, &ImportSummary{})
	require.ErrorIs(err, errBeeperBudgetExpired)
	assert.True(complete)
	assert.Equal(6, count())
	_, err = st.DB().ExecContext(t.Context(), `DELETE FROM participant_contact_observations`)
	require.NoError(err)
	imp = NewImporter(st, imp.client)
	sum := &ImportSummary{}
	_, complete, _, err = imp.ensureRosterWork(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "signal", rosterState: work}, sum)
	require.NoError(err)
	assert.True(complete)
	assert.Equal(4, count())
	assert.Zero(sum.ObservationsRecorded, "cleanup must not repeat settled matching")
}

func TestRosterMatchingRetriesFailedMember(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newFakeBeeper(t)
	chat := observationChat("telegram", "Telegram", "@telegram_first:example.test", map[string]any{"id": "@telegram_first:example.test", "username": "first"}, map[string]any{"id": "@telegram_second:example.test", "username": "second"}, map[string]any{"id": "@telegram_third:example.test", "username": "third"})
	f.addChat(chat)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	sourceID := newBeeperTestSource(t, st, "telegram")
	imp.res.accountID = "telegram"
	first, err := imp.res.resolveUser(t.Context(), &User{ID: "@telegram_first:example.test"})
	require.NoError(err)
	if st.IsPostgreSQL() {
		_, err = st.DB().ExecContext(t.Context(), `CREATE FUNCTION fail_roster_observation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected failure'; END; $$`)
		require.NoError(err)
		_, err = st.DB().ExecContext(t.Context(), fmt.Sprintf(`CREATE TRIGGER fail_roster_observation BEFORE INSERT ON participant_contact_observations FOR EACH ROW WHEN (NEW.participant_id = %d) EXECUTE FUNCTION fail_roster_observation()`, first))
	} else {
		_, err = st.DB().ExecContext(t.Context(), fmt.Sprintf(`CREATE TRIGGER fail_roster_observation BEFORE INSERT ON participant_contact_observations WHEN NEW.participant_id = %d BEGIN SELECT RAISE(FAIL, 'injected failure'); END`, first))
	}
	require.NoError(err)
	detail, err := imp.client.GetChat(t.Context(), chat.ID)
	require.NoError(err)
	work := &rosterWork{Version: 1}
	sum := &ImportSummary{}
	_, complete, _, err := imp.ensureRosterWork(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "telegram", rosterState: work, ShouldStop: func() bool { return work.Phase == "matching" && work.Next >= 2 }}, sum)
	require.ErrorIs(err, errBeeperBudgetExpired)
	assert.True(complete)
	assert.Positive(sum.Errors)
	second := participantForBeeperUser(t, st, "@telegram_second:example.test")
	observations, err := st.ListParticipantObservationsContext(t.Context(), second, true)
	require.NoError(err)
	assert.NotEmpty(observations, "a failed member must not block later matching")
	_, complete, _, err = imp.ensureRosterWork(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "telegram", rosterState: work}, &ImportSummary{})
	require.NoError(err)
	assert.True(complete)
	assert.Equal("matching_retry", work.Phase)
	runID, err := st.StartSync(sourceID, sourceTypeBeeper)
	require.NoError(err)
	cs := &ChatState{Done: true, RosterWork: work}
	repairSummary := &ImportSummary{}
	stopped, err := imp.repairQuietRoster(t.Context(), runID, sourceID, chat.ID, ImportOptions{AccountID: "telegram"}, cs, time.Now(), repairSummary)
	require.NoError(err)
	assert.False(stopped)
	assert.NotEmpty(cs.RosterRetryAfter, "failed matching backs off without undoing membership")
	assert.EqualValues(1, repairSummary.RosterRepairErrors)
	require.NoError(st.CompleteSync(runID, ""))
	if st.IsPostgreSQL() {
		_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER fail_roster_observation ON participant_contact_observations`)
		require.NoError(err)
		_, err = st.DB().ExecContext(t.Context(), `DROP FUNCTION fail_roster_observation()`)
	} else {
		_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER fail_roster_observation`)
	}
	require.NoError(err)
	imp = NewImporter(st, imp.client)
	third := participantForBeeperUser(t, st, "@telegram_third:example.test")
	observations, err = st.ListParticipantObservationsContext(t.Context(), third, true)
	require.NoError(err)
	assert.NotEmpty(observations, "later matching advances before retrying the failure")
	_, complete, _, err = imp.ensureRosterWork(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "telegram", rosterState: work}, &ImportSummary{})
	require.NoError(err)
	assert.True(complete)
	observations, err = st.ListParticipantObservationsContext(t.Context(), first, true)
	require.NoError(err)
	assert.NotEmpty(observations, "the failed member must be retried")
	assert.Equal("done", work.Phase)
}

func TestRosterResumeRevisitsChangedMember(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newFakeBeeper(t)
	chat := budgetTestChat("!member-change:example.test", 2, time.Now().Add(-48*time.Hour))
	f.addChat(chat)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	sourceID := newBeeperTestSource(t, st, "signal")
	detail, err := imp.client.GetChat(t.Context(), chat.ID)
	require.NoError(err)
	work := &rosterWork{Version: 1}
	//nolint:dogsled // Only the stop matters here.
	_, _, _, err = imp.ensureRosterWork(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "signal", rosterState: work, ShouldStop: func() bool {
		return len(rosterMembers(t, st, sourceID, chat.ID)) == 1
	}}, &ImportSummary{})
	require.ErrorIs(err, errBeeperBudgetExpired)
	old := rosterMembers(t, st, sourceID, chat.ID)[0]
	winner, err := st.EnsureParticipantByPhone(t.Context(), "+12025550130", "Native", "signal")
	require.NoError(err)
	f.mu.Lock()
	chat.Participants[0]["phoneNumber"] = "+12025550130"
	f.mu.Unlock()
	detail, err = imp.client.GetChat(t.Context(), chat.ID)
	require.NoError(err)
	imp = NewImporter(st, imp.client)
	_, complete, _, err := imp.ensureRosterWork(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "signal", rosterState: work}, &ImportSummary{})
	require.NoError(err)
	assert.True(complete)
	assert.Contains(rosterMembers(t, st, sourceID, chat.ID), winner)
	assert.NotContains(rosterMembers(t, st, sourceID, chat.ID), old)
}
func TestRosterJoinRefreshesSettledMatchAfterStop(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newFakeBeeper(t)
	chat := observationChat("slack", "Slack", "@slack_alice:beeper.local", map[string]any{"id": "@slack_alice:beeper.local", "username": "alice"}, map[string]any{"id": "@slack_extra:beeper.local"})
	f.addChat(chat)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	imp.res.accountID = "slack"
	other, err := imp.res.resolveUser(t.Context(), &User{ID: "@slack_other:beeper.local"})
	require.NoError(err)
	otherSource := newBeeperTestSource(t, st, "slack2")
	captureAndMatch(t, newObservationRecorder(st), newIdentityMatcher(st), other, &User{ID: "@slack_other:beeper.local", Username: "alice"}, captureContext{SourceID: otherSource, AccountID: "slack2", Network: "Slack"})
	sourceID := newBeeperTestSource(t, st, "slack")
	detail, err := imp.client.GetChat(t.Context(), chat.ID)
	require.NoError(err)
	work := &rosterWork{Version: 1}
	//nolint:dogsled // Only the stop matters here.
	_, _, _, err = imp.ensureRosterWork(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "slack", rosterState: work, ShouldStop: func() bool {
		return (work.Phase == "matching" || work.Phase == "cleanup") && work.Next >= 1
	}}, &ImportSummary{})
	require.ErrorIs(err, errBeeperBudgetExpired)
	candidates, err := st.ListIdentityMatchCandidatesContext(t.Context(), nil, 100, 0)
	require.NoError(err)
	require.Len(candidates, 1)
	f.mu.Lock()
	chat.Participants = append(chat.Participants, map[string]any{"id": "@slack_other:beeper.local"})
	f.mu.Unlock()
	detail, err = imp.client.GetChat(t.Context(), chat.ID)
	require.NoError(err)
	imp = NewImporter(st, imp.client)
	_, _, _, err = imp.ensureRosterWork(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "slack", rosterState: work}, &ImportSummary{}) //nolint:dogsled // Only matching evidence matters here.
	require.NoError(err)
	candidate, err := st.GetIdentityMatchCandidateContext(t.Context(), candidates[0].ID)
	require.NoError(err)
	var found bool
	for _, evidence := range candidate.Evidence {
		found = found || evidence.EvidenceKind == evidenceMembership
	}
	assert.True(found, "a settled candidate gains evidence when a member joins")
}

func TestRosterUnchangedVisitRestoresObservationsWithoutReceiptWrites(t *testing.T) {
	require := require.New(t)
	f := newFakeBeeper(t)
	chat := observationChat("telegram", "Telegram", "@telegram_alice:example.test", map[string]any{"id": "@self:example.test", "isSelf": true}, map[string]any{"id": "@telegram_alice:example.test", "username": "alice", "phoneNumber": "+12025550123"})
	f.addChat(chat)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	_, err := imp.Import(t.Context(), ImportOptions{AccountID: "telegram", NoMedia: true})
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `DELETE FROM participant_contact_observations`)
	require.NoError(err)
	if st.IsPostgreSQL() {
		_, err = st.DB().ExecContext(t.Context(), `CREATE FUNCTION reject_roster_receipt_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'unchanged receipt write'; END; $$`)
		require.NoError(err)
		_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER reject_roster_receipt_write BEFORE INSERT OR UPDATE OR DELETE ON source_import_items FOR EACH ROW EXECUTE FUNCTION reject_roster_receipt_write()`)
	} else {
		for _, op := range []string{"INSERT", "UPDATE", "DELETE"} {
			_, err = st.DB().ExecContext(t.Context(), fmt.Sprintf(`CREATE TRIGGER reject_roster_receipt_%s BEFORE %s ON source_import_items BEGIN SELECT RAISE(FAIL, 'unchanged receipt write'); END`, op, op))
			require.NoError(err)
		}
	}
	require.NoError(err)
	imp = NewImporter(st, imp.client)
	sum, err := imp.Import(t.Context(), ImportOptions{AccountID: "telegram", NoMedia: true})
	require.NoError(err)
	require.Positive(sum.ObservationsRecorded, "a later visit restores deleted observations")
}

func TestRosterVerificationRechecksChangedSnapshot(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(fmt.Sprintf("interrupted=%t", interrupted), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			f := newFakeBeeper(t)
			chat := budgetTestChat("!changed-roster:example.test", 2, time.Now().Add(-48*time.Hour))
			chat.Participants = append(chat.Participants, map[string]any{"id": "@departed1:example.test"}, map[string]any{"id": "@departed2:example.test"})
			f.addChat(chat)
			imp, st := newHandlerTestImporter(t, f)
			t.Cleanup(func() { require.NoError(st.Close()) })
			sum, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true})
			require.NoError(err)
			all := append([]map[string]any(nil), chat.Participants...)
			before := rosterMembers(t, st, sum.SourceID, chat.ID)
			require.Len(before, 4)
			work := &rosterWork{Version: 1}
			if interrupted {
				f.mu.Lock()
				chat.Participants = all[:1]
				f.mu.Unlock()
				detail, err := imp.client.GetChat(t.Context(), chat.ID)
				require.NoError(err)
				_, _, _, err = imp.ensureRosterWork(t.Context(), 0, sum.SourceID, detail, ImportOptions{AccountID: "signal", rosterState: work, ShouldStop: func() bool { return len(rosterMembers(t, st, sum.SourceID, chat.ID)) == 3 }}, &ImportSummary{})
				require.ErrorIs(err, errBeeperBudgetExpired)
				remaining := rosterMembers(t, st, sum.SourceID, chat.ID)
				f.mu.Lock()
				chat.Participants = all[:2]
				f.mu.Unlock()
				_, complete, _, err := imp.ensureRosterWork(t.Context(), 0, sum.SourceID, detail, ImportOptions{AccountID: "signal", rosterState: work}, &ImportSummary{})
				require.NoError(err)
				assert.False(complete)
				assert.Equal(remaining, rosterMembers(t, st, sum.SourceID, chat.ID))
			} else {
				detail, err := imp.client.GetChat(t.Context(), chat.ID)
				require.NoError(err)
				detail.Participants.Items = detail.Participants.Items[:2]
				detail.Participants.Total = 2
				f.mu.Lock()
				chat.Participants = all[:1]
				f.mu.Unlock()
				_, complete, _, err := imp.ensureRosterWork(t.Context(), 0, sum.SourceID, detail, ImportOptions{AccountID: "signal", rosterState: work}, &ImportSummary{})
				require.NoError(err)
				assert.False(complete)
				assert.Equal(before, rosterMembers(t, st, sum.SourceID, chat.ID))
				_, complete, _, err = imp.ensureRosterWork(t.Context(), 0, sum.SourceID, detail, ImportOptions{AccountID: "signal", rosterState: work}, &ImportSummary{})
				require.NoError(err)
				assert.True(complete)
				assert.Len(rosterMembers(t, st, sum.SourceID, chat.ID), 1)
			}
		})
	}
}

func TestRosterRetainedReceiptFollowsIdentifier(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing=%t", missing), func(t *testing.T) {
			require := require.New(t)
			f := newFakeBeeper(t)
			chat := budgetTestChat("!owner-change:example.test", 1, time.Now())
			chat.Participants[0]["id"] = "@signal_owner:example.test"
			f.addChat(chat)
			imp, st := newHandlerTestImporter(t, f)
			t.Cleanup(func() { require.NoError(st.Close()) })
			if !missing {
				_, err := newParticipantResolver(st, "signal").resolveUser(t.Context(), &User{ID: "@signal_owner:example.test", PhoneNumber: "+12025550123"})
				require.NoError(err)
			}
			first, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true})
			require.NoError(err)
			key := imp.res.identifierForUser("@signal_owner:example.test")
			var owner int64
			if missing {
				_, err = st.DB().ExecContext(t.Context(), st.Rebind(`DELETE FROM participant_identifiers WHERE identifier_type = 'beeper' AND identifier_value = ?`), key)
				require.NoError(err)
			} else {
				owner, err = st.EnsureParticipantByPhone(t.Context(), "+12025550124", "", "signal")
				require.NoError(err)
				_, err = newParticipantResolver(st, "signal").resolveUser(t.Context(), &User{ID: "@signal_owner:example.test", PhoneNumber: "+12025550124"})
				require.NoError(err)
			}
			_, err = imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true})
			require.NoError(err)
			if missing {
				owner, _, err = st.ParticipantByIdentifierContext(t.Context(), "beeper", key)
				require.NoError(err)
				require.Positive(owner)
			}
			require.Contains(rosterMembers(t, st, first.SourceID, chat.ID), owner)
		})
	}
}

func TestRosterBudgetAlternatesMessagesAndKeepsCheckpointSmall(t *testing.T) {
	for _, size := range []int{64, 10000} {
		t.Run(fmt.Sprintf("members=%d", size), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			f := newFakeBeeper(t)
			chat := budgetTestChat("!roster-budget:example.test", 2, time.Now().Add(-48*time.Hour))
			chat.Type = "group"
			for i := 2; i < size; i++ {
				chat.Participants = append(chat.Participants, map[string]any{"id": fmt.Sprintf("@signal_member%05d:example.test", i)})
			}
			f.addChat(chat)
			imp, st := newHandlerTestImporter(t, f)
			t.Cleanup(func() { require.NoError(st.Close()) })
			polls := 0
			sum, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true, Scheduled: true, ShouldStop: func() bool { polls++; return polls >= 25 }})
			require.NoError(err)
			require.True(sum.Stopped)
			var receipts int
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM source_import_items WHERE source_id = ? AND provider = ?`), sum.SourceID, rosterProvider(chat.ID)).Scan(&receipts))
			assert.Positive(receipts)
			assert.Less(receipts, 25)
			assert.Less(len(rosterMembers(t, st, sum.SourceID, chat.ID)), size)
			if size == 10000 {
				var sourceBytes, runBytes int
				require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT LENGTH(sync_cursor) FROM sources WHERE id = ?`), sum.SourceID).Scan(&sourceBytes))
				require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT LENGTH(cursor_after) FROM sync_runs WHERE source_id = ? ORDER BY id DESC LIMIT 1`), sum.SourceID).Scan(&runBytes))
				assert.Less(sourceBytes, 4096)
				assert.Less(runBytes, 4096)
				imp = NewImporter(st, imp.client)
				_, err = imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true, Scheduled: true})
				require.NoError(err)
				imp = NewImporter(st, imp.client)
				_, err = imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true, Scheduled: true, ShouldStop: func() bool {
					var current int
					require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM source_import_items WHERE source_id = ? AND provider = ?`), sum.SourceID, rosterProvider(chat.ID)).Scan(&current))
					return current > receipts
				}})
				require.NoError(err)
				var after int
				require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM source_import_items WHERE source_id = ? AND provider = ?`), sum.SourceID, rosterProvider(chat.ID)).Scan(&after))
				assert.Equal(receipts+1, after)
				assert.Equal(2, countBeeperMessages(t, imp))
				assert.Less(len(rosterMembers(t, st, sum.SourceID, chat.ID)), size, "messages arrive before the roster completes")
				return
			}
			var messagesBeforeRosterComplete bool
			for range 30 {
				polls = 0
				imp = NewImporter(st, imp.client)
				next, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true, Scheduled: true, ShouldStop: func() bool { polls++; return polls >= 25 }})
				require.NoError(err)
				require.Zero(next.Errors)
				state := latestBeeperState(t, imp).Chats[chat.ID]
				members := len(rosterMembers(t, st, sum.SourceID, chat.ID))
				if countBeeperMessages(t, imp) == 2 && members < size {
					messagesBeforeRosterComplete = true
				}
				if countBeeperMessages(t, imp) == 2 && members == size && state != nil && state.RosterWork == nil {
					break
				}
			}
			assert.True(messagesBeforeRosterComplete, "messages arrive before the roster completes")
			assert.Equal(2, countBeeperMessages(t, imp))
			assert.Len(rosterMembers(t, st, sum.SourceID, chat.ID), size)
			state := latestBeeperState(t, imp).Chats[chat.ID]
			assert.True(state.Done)
			assert.Nil(state.RosterWork, "matching and receipt cleanup must finish")
		})
	}
}
