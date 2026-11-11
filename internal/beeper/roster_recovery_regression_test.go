package beeper

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnverifiedRosterResumesVerificationBeforePausedMatching(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	f := newFakeBeeper(t)
	chat := observationChat("telegram", "Telegram", "@telegram_first:example.test", map[string]any{"id": "@telegram_first:example.test", "username": "first"}, map[string]any{"id": "@telegram_second:example.test", "username": "second"})
	f.addChat(chat)
	imp, st := newHandlerTestImporter(t, f)
	t.Cleanup(func() { require.NoError(st.Close()) })
	sourceID := newBeeperTestSource(t, st, "telegram")
	runID, err := st.StartSync(sourceID, sourceTypeBeeper)
	require.NoError(err)
	imp.res.accountID = "telegram"
	first, err := imp.res.resolveUser(t.Context(), &User{ID: "@telegram_first:example.test"})
	require.NoError(err)
	if st.IsPostgreSQL() {
		_, err = st.DB().ExecContext(t.Context(), `CREATE FUNCTION fail_recovery_observation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected failure'; END; $$`)
		require.NoError(err)
		_, err = st.DB().ExecContext(t.Context(), fmt.Sprintf(`CREATE TRIGGER fail_recovery_observation BEFORE INSERT ON participant_contact_observations FOR EACH ROW WHEN (NEW.participant_id = %d) EXECUTE FUNCTION fail_recovery_observation()`, first))
	} else {
		_, err = st.DB().ExecContext(t.Context(), fmt.Sprintf(`CREATE TRIGGER fail_recovery_observation BEFORE INSERT ON participant_contact_observations WHEN NEW.participant_id = %d BEGIN SELECT RAISE(FAIL, 'injected failure'); END`, first))
	}
	require.NoError(err)
	detail, err := imp.client.GetChat(t.Context(), chat.ID)
	require.NoError(err)
	f.setChatGetFailure(chat.ID, true)
	work := &rosterWork{}
	_, complete, _, err := imp.ensureConversation(t.Context(), runID, sourceID, detail, ImportOptions{AccountID: "telegram", rosterState: work, ShouldStop: func() bool { return work.Phase == "matching" && work.Next >= 1 }}, &ImportSummary{})
	require.ErrorIs(err, errBeeperBudgetExpired)
	require.False(complete)
	f.setChatGetFailure(chat.ID, false)
	imp = NewImporter(st, imp.client)
	cs := &ChatState{Done: true, RosterWork: work}
	stopped, err := imp.repairQuietRoster(t.Context(), runID, sourceID, chat.ID, ImportOptions{AccountID: "telegram", ShouldStop: func() bool { return work.Verified }}, cs, time.Now(), &ImportSummary{})
	require.NoError(err)
	require.True(stopped)
	assert.True(work.Verified, "a recovered roster must be verified before matching resumes")
	receipts, err := st.BeeperRosterReceiptsContext(t.Context(), sourceID, rosterProvider(chat.ID))
	require.NoError(err)
	assert.Equal("failed", receipts[imp.res.identifierForUser("@telegram_first:example.test")].Status)
	assert.Equal("matching", work.Phase)
	pending, err := st.ListUnresolvedConversationIDs(t.Context(), sourceID)
	require.NoError(err)
	assert.NotContains(pending, chat.ID)
}

func TestRosterMatchingResumesAfterOwnerChangeAndLostCheckpoint(t *testing.T) {
	for _, phase := range []string{"done", "matching", "matching_retry"} {
		t.Run("phase="+phase, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			f := newFakeBeeper(t)
			chat := observationChat("telegram", "Telegram", "@telegram_alice:example.test", map[string]any{"id": "@telegram_alice:example.test", "username": "alice"}, map[string]any{"id": "@telegram_zed:example.test", "username": "zed"})
			f.addChat(chat)
			phoneChat := observationChat("telegram", "Telegram", "@telegram_alice:example.test", map[string]any{"id": "@telegram_alice:example.test", "phoneNumber": "+12025550176"})
			phoneChat.ID = "!phone:example.test"
			f.addChat(phoneChat)
			imp, st := newHandlerTestImporter(t, f)
			t.Cleanup(func() { require.NoError(st.Close()) })
			sourceID := newBeeperTestSource(t, st, "telegram")
			detail, err := imp.client.GetChat(t.Context(), chat.ID)
			require.NoError(err)
			work := &rosterWork{}
			_, complete, _, err := imp.ensureConversation(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "telegram", rosterState: work}, &ImportSummary{})
			require.NoError(err)
			require.True(complete)
			require.Equal("done", work.Phase)
			saved := *work
			if phase != "done" {
				saved.Phase, saved.Next = phase, 1
			}
			checkpoint := NewSyncState()
			checkpoint.EnsureChat(chat.ID).RosterWork = &saved
			blob, err := checkpoint.Marshal()
			require.NoError(err)
			previous := participantForBeeperUser(t, st, "@telegram_alice:example.test")
			phoneDetail, err := imp.client.GetChat(t.Context(), phoneChat.ID)
			require.NoError(err)
			_, _, _, err = imp.ensureConversation(t.Context(), 0, sourceID, phoneDetail, ImportOptions{AccountID: "telegram", rosterState: &rosterWork{}}, &ImportSummary{})
			require.NoError(err)
			owner := participantForBeeperUser(t, st, "@telegram_alice:example.test")
			require.NotEqual(previous, owner)
			imp = NewImporter(st, imp.client)
			_, complete, _, err = imp.ensureConversation(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "telegram", rosterState: work, ShouldStop: func() bool { return work.Phase == "matching" }}, &ImportSummary{})
			require.ErrorIs(err, errBeeperBudgetExpired)
			require.True(complete)
			require.Contains(rosterMembers(t, st, sourceID, chat.ID), owner)
			receipts, err := st.BeeperRosterReceiptsContext(t.Context(), sourceID, rosterProvider(chat.ID))
			require.NoError(err)
			key := imp.res.identifierForUser("@telegram_alice:example.test")
			require.Equal("pending", receipts[key].Status)
			checkpoint, err = LoadSyncState(blob)
			require.NoError(err)
			imp = NewImporter(st, imp.client)
			_, complete, _, err = imp.ensureConversation(t.Context(), 0, sourceID, detail, ImportOptions{AccountID: "telegram", rosterState: checkpoint.Chats[chat.ID].RosterWork}, &ImportSummary{})
			require.NoError(err)
			require.True(complete)
			observations, err := st.ListParticipantObservationsContext(t.Context(), owner, true)
			require.NoError(err)
			found := false
			for _, observation := range observations {
				found = found || observation.NormalizedValue == "alice"
			}
			assert.True(found, "the new owner must receive the member's username")
			receipts, err = st.BeeperRosterReceiptsContext(t.Context(), sourceID, rosterProvider(chat.ID))
			require.NoError(err)
			assert.Equal("resolved", receipts[key].Status)
		})
	}
}
