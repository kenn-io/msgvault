package store_test

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestAppreciationDismissalFirstWriteAndProofOnly(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	key := strings.Repeat("a", 64)
	// Schema and conditional-upsert probe for a proof-only row; S2 owns its validated writer.
	_, err := f.Store.DB().ExecContext(ctx, f.Store.Rebind(`INSERT INTO person_appreciation_state (contribution_key) VALUES (?)`), key)
	require.NoError(err)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	first, err := f.Store.RecordPersonAppreciationDismissalContext(ctx, key, " person-uid ", " owner ", at)
	require.NoError(err)
	retry, err := f.Store.RecordPersonAppreciationDismissalContext(ctx, key, "other", "other", at.Add(time.Hour))
	require.NoError(err)
	assert.Equal(first, retry)
	assert.Equal(" person-uid ", *retry.DismissedPersonUID)
	assert.Equal(" owner ", *retry.DismissedBy)
	assert.True(at.Equal(*retry.DismissedAt))
	states, err := f.Store.GetPersonAppreciationStatesContext(ctx, []string{key, strings.Repeat("b", 64)})
	require.NoError(err)
	assert.Equal(map[string]store.PersonAppreciationState{key: first}, states)
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`DELETE FROM sources WHERE id = ?`), f.Source.ID)
	require.NoError(err)
	states, err = f.Store.GetPersonAppreciationStatesContext(ctx, []string{key})
	require.NoError(err)
	assert.Equal(first, states[key])
}

func TestAppreciationConcurrentDismissal(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	key := strings.Repeat("c", 64)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	var wg sync.WaitGroup
	results := make([]store.PersonAppreciationState, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Go(func() {
			results[i], errs[i] = f.Store.RecordPersonAppreciationDismissalContext(t.Context(), key, "person", []string{"one", "two"}[i], at.Add(time.Duration(i)*time.Hour))
		})
	}
	wg.Wait()
	require.NoError(errs[0])
	require.NoError(errs[1])
	assert.Equal(results[0], results[1])
	require.NotNil(results[0].DismissedBy)
	assert.Contains([]string{"one", "two"}, *results[0].DismissedBy)
}

func TestAppreciationStateValidation(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	_, err := f.Store.RecordPersonAppreciationDismissalContext(ctx, "bad", "person", "owner", time.Now())
	require.ErrorIs(err, store.ErrInvalidPersonAppreciationKey)
	_, err = f.Store.GetPersonAppreciationStatesContext(ctx, []string{strings.Repeat("a", 64), "bad"})
	require.ErrorIs(err, store.ErrInvalidPersonAppreciationKey)
	require.ErrorContains(err, "index 1")
	_, err = f.Store.GetPersonAppreciationStatesContext(ctx, make([]string, 501))
	require.ErrorIs(err, store.ErrTooManyPersonAppreciationKeys)
	require.ErrorContains(err, "500")
	_, err = f.Store.RecordPersonAppreciationDismissalContext(ctx, strings.Repeat("a", 64), "person", " ", time.Now())
	require.ErrorIs(err, store.ErrInvalidPersonAppreciationAudit)
	for _, tc := range []struct{ uid, actor string }{{"person\x00uid", "owner"}, {"person", "owner\x00actor"}} {
		_, err = f.Store.RecordPersonAppreciationDismissalContext(ctx, strings.Repeat("a", 64), tc.uid, tc.actor, time.Now())
		require.ErrorIs(err, store.ErrInvalidPersonAppreciationAudit)
	}
}

func TestAppreciationStateMigration(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	_, err := f.Store.DB().ExecContext(ctx, `DROP TABLE person_appreciation_state`)
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(ctx, `UPDATE archive_metadata SET value='40' WHERE key='schema_version'`)
	require.NoError(err)
	require.NoError(f.Store.InitSchemaContext(ctx))
	_, err = f.Store.RecordPersonAppreciationDismissalContext(ctx, strings.Repeat("d", 64), "person", "owner", time.Now())
	require.NoError(err)
}

func TestAppreciationProofDeletionPreservesDismissal(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	personID := personFactSchemaPerson(t, f.Store, "appreciation@example.com")
	generationID := insertPersonFactSchemaGeneration(t, f.Store, personID, "appreciation")
	briefID := insertPersonBriefSchemaVersion(t, f.Store, personID, generationID, 1, "current")
	key := strings.Repeat("e", 64)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	first, err := f.Store.RecordPersonAppreciationDismissalContext(ctx, key, "audit-uid", "owner", at)
	require.NoError(err)
	// This raw pointer exercises the FK only; it is not a validated thanks proof.
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`UPDATE person_appreciation_state SET thanks_brief_id = ? WHERE contribution_key = ?`), briefID, key)
	require.NoError(err)
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`DELETE FROM person_briefs WHERE id = ?`), briefID)
	require.NoError(err)
	states, err := f.Store.GetPersonAppreciationStatesContext(ctx, []string{key})
	require.NoError(err)
	assert.Equal(first, states[key])
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`DELETE FROM persons WHERE id = ?`), personID)
	require.NoError(err)
	states, err = f.Store.GetPersonAppreciationStatesContext(ctx, []string{key})
	require.NoError(err)
	assert.Equal(first, states[key])
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`INSERT INTO person_appreciation_state (contribution_key, dismissed_by) VALUES (?, ?)`), strings.Repeat("f", 64), "owner")
	require.Error(err)
}

func TestAppreciationDismissalSurvivesMergeSplit(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	p1 := f.EnsureParticipant("appreciation-one@example.com", "One", "example.com")
	p2 := f.EnsureParticipant("appreciation-two@example.com", "Two", "example.com")
	one, _, err := f.Store.CreatePersonFromParticipant(p1)
	require.NoError(err)
	two, _, err := f.Store.CreatePersonFromParticipant(p2)
	require.NoError(err)
	merged, err := f.Store.MergePersonsContext(ctx, store.PersonMergeRequest{SurvivorID: one.ID, AbsorbedID: two.ID, ExpectedSurvivorRevision: one.Revision, ExpectedAbsorbedRevision: two.Revision, IdempotencyKey: "appreciation-merge", Actor: "owner"})
	require.NoError(err)
	key := strings.Repeat("1", 64)
	first, err := f.Store.RecordPersonAppreciationDismissalContext(ctx, key, merged.Person.VCardUID, "owner", time.Now())
	require.NoError(err)
	_, err = f.Store.SplitPersonMergeContext(ctx, store.PersonSplitRequest{SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID, ParticipantIDs: []int64{p2}, ExpectedSourceRevision: merged.Person.Revision, IdempotencyKey: "appreciation-split", Actor: "owner"})
	require.NoError(err)
	states, err := f.Store.GetPersonAppreciationStatesContext(ctx, []string{key})
	require.NoError(err)
	assert.Equal(t, first, states[key])
}

func TestAppreciationDismissalSurvivesReopen(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "archive.db")
	st := testutil.NewSQLiteTestStoreAt(t, path)
	key := strings.Repeat("2", 64)
	first, err := st.RecordPersonAppreciationDismissalContext(t.Context(), key, "person", "owner", time.Now())
	require.NoError(err)
	require.NoError(st.Close())
	reopened, err := store.OpenForTest(t.Context(), path)
	require.NoError(err)
	t.Cleanup(func() { _ = reopened.Close() })
	states, err := reopened.GetPersonAppreciationStatesContext(t.Context(), []string{key})
	require.NoError(err)
	assert.Equal(t, first, states[key])
}
