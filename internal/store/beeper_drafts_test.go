package store_test

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestBeeperDraftStore(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "signal")
	requirements.NoError(err)

	draft, err := st.BeginBeeperDraftCreateContext(t.Context(), source.ID, "signal", "!room:beeper.local", "hello")
	requirements.NoError(err)
	assertions.Equal(int64(1), draft.Revision)
	assertions.Equal(store.BeeperDraftOperationCreate, draft.Pending.Operation)
	requirements.NoError(st.RecordBeeperDraftOutcomeContext(t.Context(), draft.DraftID, draft.Revision, store.BeeperDraftPhaseSetDispatched, "dispatching"))
	finished, err := st.FinishBeeperDraftContext(t.Context(), draft.DraftID, draft.Revision, new("rich hello"))
	requirements.NoError(err)
	assertions.Equal(int64(2), finished.Revision)
	requirements.NotNil(finished.CommittedText)
	assertions.Equal("rich hello", *finished.CommittedText)
	assertions.Nil(finished.Pending)
	_, err = st.ClaimBeeperDraftContext(t.Context(), draft.DraftID, finished.Revision-1, store.BeeperDraftOperationDelete, "")
	requirements.ErrorIs(err, store.ErrBeeperDraftRevision)

	claimed, err := st.ClaimBeeperDraftContext(t.Context(), draft.DraftID, finished.Revision, store.BeeperDraftOperationDelete, "")
	requirements.NoError(err)
	assertions.Equal(finished.Revision+1, claimed.Revision)
	requirements.NoError(st.RecordBeeperDraftOutcomeContext(t.Context(), claimed.DraftID, claimed.Revision, store.BeeperDraftPhaseClearDispatched, "dispatching"))
	requirements.NoError(st.RecordBeeperDraftOutcomeContext(t.Context(), claimed.DraftID, claimed.Revision, store.BeeperDraftPhaseClearConfirmed, "cleared"))
	cleared, err := st.FinishBeeperDraftContext(t.Context(), claimed.DraftID, claimed.Revision, nil)
	requirements.NoError(err)
	assertions.Nil(cleared.CommittedText)

	reused, err := st.BeginBeeperDraftCreateContext(t.Context(), source.ID, "signal", "!room:beeper.local", "again")
	requirements.NoError(err)
	assertions.Equal(draft.DraftID, reused.DraftID)
	requirements.NoError(st.AbortBeeperDraftClaimContext(t.Context(), reused.DraftID, reused.Revision))
	loaded, err := st.GetBeeperDraftContext(t.Context(), reused.DraftID)
	requirements.NoError(err)
	assertions.Nil(loaded.Pending)
}

func TestBeeperDraftStoreRejectsWrongSourceAndDuplicateBinding(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	beeperSource, err := st.GetOrCreateSource("beeper", "signal")
	requirements.NoError(err)
	gmailSource, err := st.GetOrCreateSource("gmail", "signal")
	requirements.NoError(err)
	_, err = st.BeginBeeperDraftCreateContext(t.Context(), gmailSource.ID, "signal", "!room:beeper.local", "body")
	requirements.Error(err)
	_, err = st.BeginBeeperDraftCreateContext(t.Context(), beeperSource.ID, "other", "!room:beeper.local", "body")
	requirements.Error(err)

	draft, err := st.BeginBeeperDraftCreateContext(t.Context(), beeperSource.ID, "signal", "!room:beeper.local", "body")
	requirements.NoError(err)
	loaded, err := st.GetBeeperDraftForSourceChatContext(t.Context(), beeperSource.ID, "!room:beeper.local")
	requirements.NoError(err)
	assertions.Equal(draft.DraftID, loaded.DraftID)
	assertions.Equal(draft.Revision, loaded.Revision)
	_, err = st.BeginBeeperDraftCreateContext(t.Context(), beeperSource.ID, "signal", "!room:beeper.local", "second")
	requirements.ErrorIs(err, store.ErrBeeperDraftPending)
	requirements.NotErrorIs(err, sql.ErrNoRows)
	assertions.NotEmpty(draft.DraftID)
}

func TestBeeperDraftSchemaRecreatesMissingTable(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.DB().Exec(`DROP TABLE beeper_drafts`)
	requirements.NoError(err)
	requirements.NoError(st.InitSchema())
	_, err = st.DB().Exec(`SELECT draft_id FROM beeper_drafts LIMIT 1`)
	requirements.NoError(err)
}

func TestBeeperDraftStoreRejectsSkippedPhase(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "signal")
	requirements.NoError(err)
	draft, err := st.BeginBeeperDraftCreateContext(t.Context(), source.ID, "signal", "!room:beeper.local", "hello")
	requirements.NoError(err)
	requirements.Error(st.RecordBeeperDraftOutcomeContext(t.Context(), draft.DraftID, draft.Revision, store.BeeperDraftPhaseClearConfirmed, "skipped"))
	requirements.NoError(st.RecordBeeperDraftOutcomeContext(t.Context(), draft.DraftID, draft.Revision, store.BeeperDraftPhaseSetDispatched, "dispatching"))
	requirements.Error(st.RecordBeeperDraftOutcomeContext(t.Context(), draft.DraftID, draft.Revision, store.BeeperDraftPhaseClaimed, "backward"))
}

func TestBeeperDraftRetirePendingPhasesAfterEmptyObservation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "signal")
	requirements.NoError(err)
	cases := []struct {
		operation string
		phase     string
	}{
		{store.BeeperDraftOperationCreate, store.BeeperDraftPhaseClaimed},
		{store.BeeperDraftOperationCreate, store.BeeperDraftPhaseSetDispatched},
		{store.BeeperDraftOperationCreate, store.BeeperDraftPhaseRejected},
		{store.BeeperDraftOperationCreate, store.BeeperDraftPhaseRemoteUnknown},
		{store.BeeperDraftOperationCreate, store.BeeperDraftPhaseAcceptedLocalFailed},
		{store.BeeperDraftOperationEdit, store.BeeperDraftPhaseClaimed},
		{store.BeeperDraftOperationEdit, store.BeeperDraftPhaseClearDispatched},
		{store.BeeperDraftOperationEdit, store.BeeperDraftPhaseClearConfirmed},
		{store.BeeperDraftOperationEdit, store.BeeperDraftPhaseSetDispatched},
		{store.BeeperDraftOperationEdit, store.BeeperDraftPhaseRejected},
		{store.BeeperDraftOperationEdit, store.BeeperDraftPhaseRemoteUnknown},
		{store.BeeperDraftOperationEdit, store.BeeperDraftPhaseAcceptedLocalFailed},
		{store.BeeperDraftOperationDelete, store.BeeperDraftPhaseClaimed},
		{store.BeeperDraftOperationDelete, store.BeeperDraftPhaseClearDispatched},
		{store.BeeperDraftOperationDelete, store.BeeperDraftPhaseClearConfirmed},
		{store.BeeperDraftOperationDelete, store.BeeperDraftPhaseRejected},
		{store.BeeperDraftOperationDelete, store.BeeperDraftPhaseRemoteUnknown},
		{store.BeeperDraftOperationDelete, store.BeeperDraftPhaseAcceptedLocalFailed},
	}
	for i, tc := range cases {
		t.Run(fmt.Sprintf("%s/%s", tc.operation, tc.phase), func(t *testing.T) {
			requirements := require.New(t)
			chatID := fmt.Sprintf("!room:%d", i)
			draft, err := st.BeginBeeperDraftCreateContext(t.Context(), source.ID, "signal", chatID, "hello")
			requirements.NoError(err)
			if tc.operation == store.BeeperDraftOperationCreate {
				advanceBeeperDraftPhase(t, st, draft, tc.phase)
			} else {
				requirements.NoError(st.RecordBeeperDraftOutcomeContext(t.Context(), draft.DraftID, draft.Revision, store.BeeperDraftPhaseSetDispatched, "dispatching"))
				finished, finishErr := st.FinishBeeperDraftContext(t.Context(), draft.DraftID, draft.Revision, new("rich hello"))
				requirements.NoError(finishErr)
				candidate := "updated"
				if tc.operation == store.BeeperDraftOperationDelete {
					candidate = ""
				}
				claimed, claimErr := st.ClaimBeeperDraftContext(t.Context(), draft.DraftID, finished.Revision, tc.operation, candidate)
				requirements.NoError(claimErr)
				advanceBeeperDraftEditOrDeletePhase(t, st, claimed, tc.phase)
			}
			before, loadErr := st.GetBeeperDraftContext(t.Context(), draft.DraftID)
			requirements.NoError(loadErr)
			requirements.NotNil(before.Pending)
			retired, retireErr := st.RetireBeeperDraftAfterEmptyObservationContext(t.Context(), draft.DraftID, before.Revision)
			requirements.NoError(retireErr)
			assertions.Nil(retired.Pending)
			assertions.Nil(retired.CommittedText)
			assertions.Equal(before.Revision+1, retired.Revision)
		})
	}
}

func advanceBeeperDraftPhase(t *testing.T, st *store.Store, draft store.BeeperDraft, phase string) {
	t.Helper()
	record := func(next, code string) {
		require.NoError(t, st.RecordBeeperDraftOutcomeContext(t.Context(), draft.DraftID, draft.Revision, next, code))
	}
	switch phase {
	case store.BeeperDraftPhaseClaimed:
	case store.BeeperDraftPhaseSetDispatched:
		record(store.BeeperDraftPhaseSetDispatched, "dispatching")
	case store.BeeperDraftPhaseRejected:
		record(store.BeeperDraftPhaseRejected, "provider_rejected")
	case store.BeeperDraftPhaseRemoteUnknown:
		record(store.BeeperDraftPhaseSetDispatched, "dispatching")
		record(store.BeeperDraftPhaseRemoteUnknown, "remote_unknown")
	case store.BeeperDraftPhaseAcceptedLocalFailed:
		record(store.BeeperDraftPhaseSetDispatched, "dispatching")
		record(store.BeeperDraftPhaseAcceptedLocalFailed, "remote_accepted_local_failed")
	default:
		require.FailNow(t, "unsupported create phase", phase)
	}
}

func advanceBeeperDraftEditOrDeletePhase(t *testing.T, st *store.Store, draft store.BeeperDraft, phase string) {
	t.Helper()
	record := func(next, code string) {
		require.NoError(t, st.RecordBeeperDraftOutcomeContext(t.Context(), draft.DraftID, draft.Revision, next, code))
	}
	if phase == store.BeeperDraftPhaseClaimed {
		return
	}
	if phase == store.BeeperDraftPhaseRejected || phase == store.BeeperDraftPhaseRemoteUnknown {
		record(phase, phase)
		return
	}
	record(store.BeeperDraftPhaseClearDispatched, "dispatching")
	if phase == store.BeeperDraftPhaseClearDispatched {
		return
	}
	record(store.BeeperDraftPhaseClearConfirmed, "cleared")
	if phase == store.BeeperDraftPhaseClearConfirmed {
		return
	}
	if draft.Pending.Operation == store.BeeperDraftOperationDelete && phase == store.BeeperDraftPhaseAcceptedLocalFailed {
		record(store.BeeperDraftPhaseAcceptedLocalFailed, "remote_accepted_local_failed")
		return
	}
	if phase == store.BeeperDraftPhaseSetDispatched {
		record(store.BeeperDraftPhaseSetDispatched, "dispatching")
		return
	}
	if phase == store.BeeperDraftPhaseAcceptedLocalFailed {
		record(store.BeeperDraftPhaseSetDispatched, "dispatching")
		record(store.BeeperDraftPhaseAcceptedLocalFailed, "remote_accepted_local_failed")
		return
	}
	require.FailNow(t, "unsupported edit/delete phase", phase)
}

func TestBeeperDraftRetirePendingRevisionAndRollback(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	testutil.SkipIfPostgres(t, "rollback injection uses a SQLite trigger")
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "signal")
	requirements.NoError(err)
	draft, err := st.BeginBeeperDraftCreateContext(t.Context(), source.ID, "signal", "!rollback:beeper.local", "candidate")
	requirements.NoError(err)
	_, retireErr := st.RetireBeeperDraftAfterEmptyObservationContext(t.Context(), draft.DraftID, draft.Revision-1)
	requirements.ErrorIs(retireErr, store.ErrBeeperDraftRevision)
	unchanged, err := st.GetBeeperDraftContext(t.Context(), draft.DraftID)
	requirements.NoError(err)
	assertions.Equal(draft.Revision, unchanged.Revision)
	assertions.Equal("candidate", unchanged.Pending.Candidate)
	_, err = st.DB().Exec(`
CREATE TRIGGER beeper_drafts_retire_store_failure
BEFORE UPDATE OF committed_text ON beeper_drafts
BEGIN
  SELECT RAISE(FAIL, 'injected retire store failure');
END`)
	requirements.NoError(err)
	_, err = st.RetireBeeperDraftAfterEmptyObservationContext(t.Context(), draft.DraftID, draft.Revision)
	requirements.Error(err)
	unchanged, err = st.GetBeeperDraftContext(t.Context(), draft.DraftID)
	requirements.NoError(err)
	assertions.Equal(draft.Revision, unchanged.Revision)
	assertions.Equal("candidate", unchanged.Pending.Candidate)
}
