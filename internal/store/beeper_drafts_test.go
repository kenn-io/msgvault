package store_test

import (
	"database/sql"
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
	_, err = st.BeginBeeperDraftCreateContext(t.Context(), beeperSource.ID, "signal", "!room:beeper.local", "second")
	requirements.ErrorIs(err, store.ErrBeeperDraftPending)
	assertions.NotErrorIs(err, sql.ErrNoRows)
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
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "signal")
	requirements.NoError(err)
	draft, err := st.BeginBeeperDraftCreateContext(t.Context(), source.ID, "signal", "!room:beeper.local", "hello")
	requirements.NoError(err)
	assertions.Error(st.RecordBeeperDraftOutcomeContext(t.Context(), draft.DraftID, draft.Revision, store.BeeperDraftPhaseClearConfirmed, "skipped"))
	requirements.NoError(st.RecordBeeperDraftOutcomeContext(t.Context(), draft.DraftID, draft.Revision, store.BeeperDraftPhaseSetDispatched, "dispatching"))
	assertions.Error(st.RecordBeeperDraftOutcomeContext(t.Context(), draft.DraftID, draft.Revision, store.BeeperDraftPhaseClaimed, "backward"))
}
