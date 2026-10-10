package store_test

import (
	"context"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestCardDAVPendingPublicationPersistsIntentIdentityAcrossRestart(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st, _, _, mapping := seededCardDAVConflictMapping(t)
	pending := prepareSyntheticReviewedMappedUpdate(t, st, mapping, "Synthetic Corrected")
	originalJSON, err := json.Marshal(pending)
	requirements.NoError(err)
	var identity struct{ PendingIntentID string }
	requirements.NoError(json.Unmarshal(originalJSON, &identity))
	requirements.NotEmpty(identity.PendingIntentID, "native pending intent needs a persisted identity independent of row-local revision and clock")
	reopened, err := store.OpenContext(t.Context(), store.DBPathForTest(st))
	requirements.NoError(err)
	t.Cleanup(func() { _ = reopened.Close() })
	requirements.NoError(reopened.InitSchema())
	again, err := reopened.GetCardDAVPublicationContext(t.Context(), pending.PersonID)
	requirements.NoError(err)
	againJSON, err := json.Marshal(again)
	requirements.NoError(err)
	assertions.JSONEq(string(originalJSON), string(againJSON), "a new native store retains the exact original intent identity")
}

func TestLegacyCardDAVPendingPublicationKeepsOwnerRecoveryAfterIntentMigration(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st, _, _, mapping := seededCardDAVConflictMapping(t)
	pending := prepareSyntheticReviewedMappedUpdate(t, st, mapping, "Synthetic Legacy")
	_, err := st.DB().ExecContext(t.Context(), `ALTER TABLE carddav_publications DROP COLUMN pending_intent_id`)
	requirements.NoError(err)
	reopened, err := store.OpenContext(t.Context(), store.DBPathForTest(st))
	requirements.NoError(err)
	t.Cleanup(func() { _ = reopened.Close() })
	for range 2 {
		requirements.NoError(reopened.InitSchema())
	}
	legacy, err := reopened.GetCardDAVPublicationContext(t.Context(), pending.PersonID)
	requirements.NoError(err)
	requirements.Empty(legacy.PendingIntentID, "migration must not invent identity for pre-existing ambiguous intent")
	pending.PendingIntentID = ""
	expectedJSON, err := json.Marshal(pending)
	requirements.NoError(err)
	legacyJSON, err := json.Marshal(legacy)
	requirements.NoError(err)
	assertions.JSONEq(string(expectedJSON), string(legacyJSON), "schema migration preserves all native legacy recovery evidence")
	backend, ok := any(reopened).(authorizedPublicationRefreshStore)
	requirements.True(ok)
	seen := false
	_, err = backend.RefreshCardDAVPublicationFenceAuthorizedContext(t.Context(), *legacy, func(_ context.Context, _ *store.IdentityGrantSelection) error { seen = true; return nil })
	requirements.ErrorIs(err, store.ErrCardDAVStalePlan)
	assertions.False(seen, "scoped recovery must not admit unidentified legacy intent")
	unchanged, err := reopened.GetCardDAVPublicationContext(t.Context(), pending.PersonID)
	requirements.NoError(err)
	unchangedJSON, err := json.Marshal(unchanged)
	requirements.NoError(err)
	assertions.JSONEq(string(legacyJSON), string(unchangedJSON))
	refreshed, err := reopened.RefreshCardDAVPublicationFenceContext(t.Context(), pending.PersonID)
	requirements.NoError(err)
	requirements.NoError(reopened.RollbackCardDAVPublicationContext(t.Context(), refreshed))
	settled, err := reopened.GetCardDAVPublicationContext(t.Context(), pending.PersonID)
	requirements.NoError(err)
	assertions.Empty(settled.PendingOperation, "owner can reconcile legacy pending evidence")
}
