package beeper

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

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
	work := &rosterWork{}
	sum := &ImportSummary{}
	_, complete, _, err := imp.ensureConversation(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "telegram", rosterState: work, ShouldStop: func() bool { return work.Phase == "matching" && work.Next >= 2 }}, sum)
	require.ErrorIs(err, errBeeperBudgetExpired)
	assert.True(complete)
	assert.Positive(sum.Errors)
	receipts, err := st.BeeperRosterReceiptsContext(t.Context(), sourceID, rosterProvider(chat.ID))
	require.NoError(err)
	assert.Equal("failed", receipts[imp.res.identifierForUser("@telegram_first:example.test")].Status)
	assert.Equal("resolved", receipts[imp.res.identifierForUser("@telegram_second:example.test")].Status)
	assert.Equal("pending", receipts[imp.res.identifierForUser("@telegram_third:example.test")].Status)
	second := participantForBeeperUser(t, st, "@telegram_second:example.test")
	observations, err := st.ListParticipantObservationsContext(t.Context(), second, true)
	require.NoError(err)
	assert.NotEmpty(observations, "a failed member must not block later matching")
	_, complete, _, err = imp.ensureConversation(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "telegram", rosterState: work}, &ImportSummary{})
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
	_, complete, _, err = imp.ensureConversation(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "telegram", rosterState: work}, &ImportSummary{})
	require.NoError(err)
	assert.True(complete)
	observations, err = st.ListParticipantObservationsContext(t.Context(), first, true)
	require.NoError(err)
	assert.NotEmpty(observations, "the failed member must be retried")
	assert.Equal("done", work.Phase)
	receipts, err = st.BeeperRosterReceiptsContext(t.Context(), sourceID, rosterProvider(chat.ID))
	require.NoError(err)
	for _, receipt := range receipts {
		assert.Equal("resolved", receipt.Status)
	}
}

func TestRosterMatchingFailureRetriesAcrossChats(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newFakeBeeper(t)
	first := observationChat("slack", "Slack", "@slack_alice:beeper.local", map[string]any{"id": "@slack_alice:beeper.local", "username": "alice"})
	second := observationChat("slack", "Slack", "@slack_alice:beeper.local", map[string]any{"id": "@slack_alice:beeper.local", "username": "alice"})
	second.ID = "!second:example.test"
	f.addChat(first)
	f.addChat(second)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	sourceID := newBeeperTestSource(t, st, "slack")
	works := []*rosterWork{{}, {}}
	for i, chat := range []*fakeChat{first, second} {
		detail, err := imp.client.GetChat(t.Context(), chat.ID)
		require.NoError(err)
		_, complete, _, err := imp.ensureConversation(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "slack", rosterState: works[i]}, &ImportSummary{})
		require.NoError(err)
		require.True(complete)
		_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE source_import_items SET status = 'pending' WHERE source_id = ? AND provider = ?`), sourceID, rosterProvider(chat.ID))
		require.NoError(err)
		works[i].Phase = "matching"
		works[i].Next = 0
	}
	otherSource := newBeeperTestSource(t, st, "slack2")
	other, err := newParticipantResolver(st, "slack2").resolveUser(t.Context(), &User{ID: "@slack_other:beeper.local"})
	require.NoError(err)
	_, err = newObservationRecorder(st).capture(t.Context(), other, &User{ID: "@slack_other:beeper.local", Username: "alice"}, captureContext{SourceID: otherSource, AccountID: "slack2", Network: "Slack"})
	require.NoError(err)
	if st.IsPostgreSQL() {
		_, err = st.DB().ExecContext(t.Context(), `CREATE FUNCTION fail_roster_match() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected failure'; END; $$`)
		require.NoError(err)
		_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_roster_match BEFORE INSERT ON identity_match_candidates FOR EACH ROW EXECUTE FUNCTION fail_roster_match()`)
	} else {
		_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_roster_match BEFORE INSERT ON identity_match_candidates BEGIN SELECT RAISE(FAIL, 'injected failure'); END`)
	}
	require.NoError(err)
	imp = NewImporter(st, imp.client)
	for range 2 {
		for i, chat := range []*fakeChat{first, second} {
			detail, err := imp.client.GetChat(t.Context(), chat.ID)
			require.NoError(err)
			_, _, _, err = imp.ensureConversation(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "slack", rosterState: works[i]}, &ImportSummary{}) //nolint:dogsled // Receipt status is the observable.
			require.NoError(err)
			receipts, err := st.BeeperRosterReceiptsContext(t.Context(), sourceID, rosterProvider(chat.ID))
			require.NoError(err)
			assert.Equal("failed", receipts[imp.res.identifierForUser("@slack_alice:beeper.local")].Status)
		}
	}
	if st.IsPostgreSQL() {
		_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER fail_roster_match ON identity_match_candidates`)
		require.NoError(err)
		_, err = st.DB().ExecContext(t.Context(), `DROP FUNCTION fail_roster_match()`)
	} else {
		_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER fail_roster_match`)
	}
	require.NoError(err)
	imp = NewImporter(st, imp.client)
	for i, chat := range []*fakeChat{first, second} {
		detail, err := imp.client.GetChat(t.Context(), chat.ID)
		require.NoError(err)
		_, _, _, err = imp.ensureConversation(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "slack", rosterState: works[i]}, &ImportSummary{}) //nolint:dogsled // Receipt status is the observable.
		require.NoError(err)
		receipts, err := st.BeeperRosterReceiptsContext(t.Context(), sourceID, rosterProvider(chat.ID))
		require.NoError(err)
		assert.Equal("resolved", receipts[imp.res.identifierForUser("@slack_alice:beeper.local")].Status)
	}
	candidates, err := st.ListIdentityMatchCandidatesContext(t.Context(), nil, 100, 0)
	require.NoError(err)
	require.Len(candidates, 1)
	candidate, err := st.GetIdentityMatchCandidateContext(t.Context(), candidates[0].ID)
	require.NoError(err)
	assert.NotEmpty(candidate.Evidence)
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
	work := &rosterWork{}
	//nolint:dogsled // Only the stop matters here.
	_, _, _, err = imp.ensureConversation(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "signal", rosterState: work, ShouldStop: func() bool {
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
	_, complete, _, err := imp.ensureConversation(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "signal", rosterState: work}, &ImportSummary{})
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
	work := &rosterWork{}
	//nolint:dogsled // Only the stop matters here.
	_, _, _, err = imp.ensureConversation(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "slack", rosterState: work, ShouldStop: func() bool {
		return work.Phase == "matching" && work.Next >= 1
	}}, &ImportSummary{})
	require.ErrorIs(err, errBeeperBudgetExpired)
	candidates, err := st.ListIdentityMatchCandidatesContext(t.Context(), nil, 100, 0)
	require.NoError(err)
	require.Len(candidates, 1)
	require.Equal(store.IdentityMatchStateCandidate, candidates[0].State)
	before, err := st.GetIdentityMatchCandidateContext(t.Context(), candidates[0].ID)
	require.NoError(err)
	for _, evidence := range before.Evidence {
		assert.NotEqual(evidenceMembership, evidence.EvidenceKind)
	}
	f.mu.Lock()
	chat.Participants = append(chat.Participants, map[string]any{"id": "@slack_other:beeper.local"})
	f.mu.Unlock()
	detail, err = imp.client.GetChat(t.Context(), chat.ID)
	require.NoError(err)
	imp = NewImporter(st, imp.client)
	_, _, _, err = imp.ensureConversation(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "slack", rosterState: work}, &ImportSummary{}) //nolint:dogsled // Only matching evidence matters here.
	require.NoError(err)
	candidate, err := st.GetIdentityMatchCandidateContext(t.Context(), candidates[0].ID)
	require.NoError(err)
	var found bool
	for _, evidence := range candidate.Evidence {
		found = found || evidence.EvidenceKind == evidenceMembership
	}
	assert.True(found, "a settled candidate gains evidence when a member joins")
}

func TestUnverifiedTurnoverDoesNotAddSharedEvidence(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newFakeBeeper(t)
	chat := observationChat("slack", "Slack", "@slack_alice:beeper.local", map[string]any{"id": "@slack_alice:beeper.local", "username": "alice"}, map[string]any{"id": "@slack_extra:beeper.local"})
	later := observationChat("slack", "Slack", "@slack_alice:beeper.local", map[string]any{"id": "@slack_alice:beeper.local", "username": "alice"}, map[string]any{"id": "@slack_later:beeper.local"})
	later.ID = "!later:example.test"
	f.addChat(chat)
	f.addChat(later)
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
	work := &rosterWork{}
	_, complete, _, err := imp.ensureConversation(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "slack", rosterState: work}, &ImportSummary{})
	require.NoError(err)
	require.True(complete)
	candidates, err := st.ListIdentityMatchCandidatesContext(t.Context(), nil, 100, 0)
	require.NoError(err)
	require.Len(candidates, 1)
	f.mu.Lock()
	chat.Participants[0] = map[string]any{"id": "@slack_other:beeper.local"}
	f.mu.Unlock()
	detail, err = imp.client.GetChat(t.Context(), chat.ID)
	require.NoError(err)
	f.setChatGetFailure(chat.ID, true)
	imp = NewImporter(st, imp.client)
	_, complete, _, err = imp.ensureConversation(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "slack", rosterState: work}, &ImportSummary{})
	require.NoError(err)
	require.False(complete)
	check := func() {
		candidate, err := st.GetIdentityMatchCandidateContext(t.Context(), candidates[0].ID)
		require.NoError(err)
		for _, evidence := range candidate.Evidence {
			assert.NotEqual(evidenceMembership, evidence.EvidenceKind)
		}
	}
	check()
	laterDetail, err := imp.client.GetChat(t.Context(), later.ID)
	require.NoError(err)
	_, _, _, err = imp.ensureConversation(t.Context(), 0, sourceID, laterDetail, ImportOptions{AccountID: "slack", rosterState: &rosterWork{}}, &ImportSummary{}) //nolint:dogsled // The later chat exercises matching.
	require.NoError(err)
	check()
	f.setChatGetFailure(chat.ID, false)
	imp = NewImporter(st, imp.client)
	_, complete, _, err = imp.ensureConversation(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "slack", rosterState: work}, &ImportSummary{})
	require.NoError(err)
	assert.True(complete)
	assert.NotContains(rosterMembers(t, st, sourceID, chat.ID), participantForBeeperUser(t, st, "@slack_alice:beeper.local"))
}

func TestRosterUnchangedVisitSkipsMatching(t *testing.T) {
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
	f.appendMsg(chat.ID, fakeMsg{ID: "unchanged-visit", SortKey: 99, Timestamp: time.Now(), Text: "new", SenderID: "@telegram_alice:example.test"})
	imp = NewImporter(st, imp.client)
	sum, err := imp.Import(t.Context(), ImportOptions{AccountID: "telegram", NoMedia: true})
	require.NoError(err)
	require.Positive(sum.MessagesAdded, "the unchanged roster was revisited")
	require.Zero(sum.ObservationsRecorded, "an unchanged roster skips matching")
}

func TestRosterVerificationRechecksChangedSnapshot(t *testing.T) {
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
	receiptsBefore, err := st.BeeperRosterReceiptsContext(t.Context(), sum.SourceID, rosterProvider(chat.ID))
	require.NoError(err)
	require.Len(receiptsBefore, 4)
	work := &rosterWork{}

	detail, err := imp.client.GetChat(t.Context(), chat.ID)
	require.NoError(err)
	detail.Participants.Items = detail.Participants.Items[:2]
	detail.Participants.Total = 2
	f.mu.Lock()
	chat.Participants = all[:1]
	f.mu.Unlock()
	_, complete, _, err := imp.ensureConversation(t.Context(), 0, sum.SourceID, detail, ImportOptions{AccountID: "signal", rosterState: work}, &ImportSummary{})
	require.NoError(err)
	assert.False(complete)
	assert.Equal(before, rosterMembers(t, st, sum.SourceID, chat.ID))
	receipts, err := st.BeeperRosterReceiptsContext(t.Context(), sum.SourceID, rosterProvider(chat.ID))
	require.NoError(err)
	assert.Len(receipts, len(receiptsBefore))
	for key := range receiptsBefore {
		assert.Contains(receipts, key)
	}
	_, complete, _, err = imp.ensureConversation(t.Context(), 0, sum.SourceID, detail, ImportOptions{AccountID: "signal", rosterState: work}, &ImportSummary{})
	require.NoError(err)
	assert.True(complete)
	assert.Len(rosterMembers(t, st, sum.SourceID, chat.ID), 1)
	finalReceipts, err := st.BeeperRosterReceiptsContext(t.Context(), sum.SourceID, rosterProvider(chat.ID))
	require.NoError(err)
	require.Len(finalReceipts, 1)
	id, ok := all[0]["id"].(string)
	require.True(ok)
	assert.Contains(finalReceipts, imp.res.identifierForUser(id))
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

func TestLargeRosterCheckpointStaysCompact(t *testing.T) {
	size := 10000
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

	var runBytes int
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT LENGTH(cursor_after) FROM sync_runs WHERE source_id = ? ORDER BY id DESC LIMIT 1`), sum.SourceID).Scan(&runBytes))
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
}

func TestRosterBudgetAlternatesMessagesUntilComplete(t *testing.T) {
	size := 64
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
		if countBeeperMessages(t, imp) == 2 && members == size && state != nil && state.RosterWork != nil && state.RosterWork.Phase == "done" {
			break
		}
	}
	assert.True(messagesBeforeRosterComplete, "messages arrive before the roster completes")
	assert.Equal(2, countBeeperMessages(t, imp))
	assert.Len(rosterMembers(t, st, sum.SourceID, chat.ID), size)
	state := latestBeeperState(t, imp).Chats[chat.ID]
	assert.True(state.Done)
	assert.Equal("done", state.RosterWork.Phase, "matching must finish")
}

func TestCompletedRosterProofStaysCompact(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newFakeBeeper(t)
	for room := range 3 {
		chat := budgetTestChat(fmt.Sprintf("!compact-%d:example.test", room), 2, time.Now())
		for member := 2; member < 64; member++ {
			chat.Participants = append(chat.Participants, map[string]any{"id": fmt.Sprintf("@member-%d:example.test", member)})
		}
		f.addChat(chat)
	}
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	sum, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true})
	require.NoError(err)
	state := latestBeeperState(t, imp)
	for _, chat := range state.Chats {
		require.NotNil(chat.RosterWork)
		assert.Equal("done", chat.RosterWork.Phase)
	}
	var runBytes int
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT LENGTH(cursor_after) FROM sync_runs WHERE source_id = ? ORDER BY id DESC LIMIT 1`), sum.SourceID).Scan(&runBytes))
	assert.Less(runBytes, 4096)
}

// A quiet-room repair that stops during matching keeps the verified roster
// resolved and keeps its matching progress for the next repair.
func TestQuietRosterBudgetStopKeepsVerifiedMembershipResolved(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newFakeBeeper(t)
	chat := budgetTestChat("!quiet-verified:example.test", 1, time.Now().Add(-48*time.Hour))
	chat.Type = "group"
	chat.Participants = append(chat.Participants,
		map[string]any{"id": "@signal_q1:beeper.local", "fullName": "Quinn One"},
		map[string]any{"id": "@signal_q2:beeper.local", "fullName": "Quinn Two"})
	f.addChat(chat)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	sum, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true})
	require.NoError(err)
	f.mu.Lock()
	chat.Participants[0]["username"] = "firstchanged"
	chat.Participants[1]["username"] = "secondchanged"
	f.mu.Unlock()
	unresolved, err := st.ListUnresolvedConversationIDs(t.Context(), sum.SourceID)
	require.NoError(err)
	require.NotContains(unresolved, chat.ID)
	runID, err := st.StartSync(sum.SourceID, sourceTypeBeeper)
	require.NoError(err)
	cs := &ChatState{Done: true}
	stopped, err := imp.repairQuietRoster(t.Context(), runID, sum.SourceID, chat.ID, ImportOptions{
		AccountID: "signal", NoMedia: true,
		ShouldStop: func() bool {
			return cs.RosterWork != nil && cs.RosterWork.Phase == "matching" && cs.RosterWork.Next >= 1
		},
	}, cs, time.Now(), &ImportSummary{})
	require.NoError(err)
	require.True(stopped)
	require.NotNil(cs.RosterWork)
	assert.True(cs.RosterWork.Verified, "membership was verified before the stop")
	assert.Equal("matching", cs.RosterWork.Phase)
	assert.Equal(1, cs.RosterWork.Next, "matching progress is kept for the next repair")
	unresolved, err = st.ListUnresolvedConversationIDs(t.Context(), sum.SourceID)
	require.NoError(err)
	assert.NotContains(unresolved, chat.ID, "verified membership stays resolved while quiet matching is deferred")
	require.NoError(st.CompleteSync(runID, ""))
}

func TestManualImportRefreshesRosterAfterScheduledStop(t *testing.T) {
	require := require.New(t)
	f := newFakeBeeper(t)
	chat := budgetTestChat("!manual-refresh:example.test", 2, time.Now().Add(-time.Hour))
	f.addChat(chat)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	initial, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true})
	require.NoError(err)
	departed := participantForBeeperUser(t, st, "@signal_ann:beeper.local")
	require.Contains(rosterMembers(t, st, initial.SourceID, chat.ID), departed)
	f.mu.Lock()
	chat.Participants = append(chat.Participants, map[string]any{"id": "@temp:example.test"})
	chat.LastActivity = time.Now().Add(time.Hour)
	f.mu.Unlock()
	imp = NewImporter(st, imp.client)
	sum, err := imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true, Scheduled: true, ShouldStop: func() bool {
		var count int
		require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM conversation_participants`).Scan(&count))
		return count >= 3
	}})
	require.NoError(err)
	require.True(sum.Stopped)
	state := latestBeeperState(t, imp).Chats[chat.ID]
	require.NotNil(state)
	require.NotNil(state.RosterWork)
	require.False(state.RosterWork.MemberFirst)
	f.mu.Lock()
	chat.Participants = []map[string]any{{"id": "@new:example.test", "isAdmin": true}, {"id": "@me:beeper.local", "isSelf": true}}
	chat.LastActivity = time.Now().Add(time.Hour)
	f.mu.Unlock()
	imp = NewImporter(st, imp.client)
	_, err = imp.Import(t.Context(), ImportOptions{AccountID: "signal", NoMedia: true})
	require.NoError(err)
	newID := participantForBeeperUser(t, st, "@new:example.test")
	selfID := participantForBeeperUser(t, st, "@me:beeper.local")
	require.ElementsMatch([]int64{newID, selfID}, rosterMembers(t, st, sum.SourceID, chat.ID))
	require.NotContains(rosterMembers(t, st, sum.SourceID, chat.ID), departed)
	var role string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT role FROM conversation_participants WHERE participant_id = ? AND conversation_id = (SELECT id FROM conversations WHERE source_id = ? AND source_conversation_id = ?)`), newID, sum.SourceID, chat.ID).Scan(&role))
	require.Equal("admin", role)
	state = latestBeeperState(t, imp).Chats[chat.ID]
	require.NotNil(state.RosterWork)
	require.True(state.RosterWork.Verified)
}
