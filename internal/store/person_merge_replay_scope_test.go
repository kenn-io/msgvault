package store_test

import (
	"context"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// Historical numeric IDs cannot authorize a different or split live lineage.
func TestPersonMergeAuthorizedReplayRejectsChangedLineage(t *testing.T) {
	for _, kind := range []string{"split", "moved survivor", "changed survivor UID", "reused absorbed ID"} {
		t.Run(kind, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := storetest.New(t).Store
			survivor := mustPromotedPerson(t, st, "replay-survivor@example.test", "Synthetic Survivor")
			absorbed := mustPromotedPerson(t, st, "replay-absorbed@example.test", "Synthetic Absorbed")
			request := store.PersonMergeRequest{SurvivorID: survivor.ID, AbsorbedID: absorbed.ID, ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision, IdempotencyKey: "synthetic-replay-lineage", Actor: "agent:synthetic-grant"}
			merged, err := st.MergePersonsAuthorizedContext(t.Context(), request, func(context.Context, *store.IdentityGrantSelection) error { return nil })
			requirements.NoError(err)
			switch kind {
			case "split":
				_, err = st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{SourcePersonID: survivor.ID, MergeID: merged.Merge.ID, ParticipantIDs: absorbed.ParticipantIDs, ExpectedSourceRevision: merged.Person.Revision, IdempotencyKey: "synthetic-split-lineage", Actor: "synthetic-owner"})
				requirements.NoError(err)
			case "moved survivor":
				other := mustPromotedPerson(t, st, "replay-other@example.test", "Synthetic Other")
				_, err = st.MergePersonsContext(t.Context(), store.PersonMergeRequest{SurvivorID: other.ID, AbsorbedID: survivor.ID, ExpectedSurvivorRevision: other.Revision, ExpectedAbsorbedRevision: merged.Person.Revision, IdempotencyKey: "synthetic-move-lineage", Actor: "synthetic-owner"})
				requirements.NoError(err)
			case "changed survivor UID":
				// Simulate replacement archive identity; ordinary edits never change UIDs.
				_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE persons SET vcard_uid = ? WHERE id = ?`), "synthetic-replaced-survivor-uid", survivor.ID)
				requirements.NoError(err)
			case "reused absorbed ID":
				// Simulate restored numeric IDs with a different immutable identity.
				insert := `INSERT INTO persons (id, vcard_uid) `
				if st.IsPostgreSQL() {
					insert += `OVERRIDING SYSTEM VALUE `
				}
				_, err = st.DB().ExecContext(t.Context(), st.Rebind(insert+`VALUES (?, ?)`), absorbed.ID, "synthetic-reused-absorbed-uid")
				requirements.NoError(err)
			}
			called := false
			replay, err := st.MergePersonsAuthorizedContext(t.Context(), request, func(context.Context, *store.IdentityGrantSelection) error { called = true; return nil })
			requirements.ErrorIs(err, store.ErrPersonMergeLineageConflict)
			assertions.Nil(replay)
			assertions.False(called, "changed lineage must not be admitted as the original roots")
			ownerReplay, err := st.MergePersonsContext(t.Context(), request)
			requirements.NoError(err)
			wantReceipt, err := json.Marshal(merged)
			requirements.NoError(err)
			gotReceipt, err := json.Marshal(ownerReplay)
			requirements.NoError(err)
			assertions.JSONEq(string(wantReceipt), string(gotReceipt), "native owner historical replay remains available")
		})
	}
}
