package store_test

import (
	"context"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestAuthorizedPublicationRejectsRecreatedRowRevisionCollision(t *testing.T) {
	for _, operation := range []string{"settlement", "rollback", "rollback_throttle", "refresh", "retry"} {
		for _, evidence := range []string{"original_artifact", "old_nonce", "empty_nonce"} {
			t.Run(operation+"/"+evidence, func(t *testing.T) {
				requirements, assertions := require.New(t), assert.New(t)
				st, account, book, mapping := seededCardDAVConflictMapping(t)
				original := prepareSyntheticReviewedMappedUpdate(t, st, mapping, "Synthetic Original")
				requirements.NoError(st.RollbackCardDAVPublicationContext(t.Context(), original))
				deletion, err := st.PrepareCardDAVPublicationContext(t.Context(), store.CardDAVPublicationPlan{PersonID: original.PersonID, Desired: false, AddressBookID: book.ID, Href: mapping.Href, LocalHash: original.LocalHash})
				requirements.NoError(err)
				requirements.Equal(store.CardDAVMutationDelete, deletion.PendingOperation)
				requirements.NoError(st.CommitCardDAVPublicationContext(t.Context(), store.CardDAVCanonicalMutation{Publication: *deletion, Tombstone: true}))
				_, err = st.GetCardDAVPublicationContext(t.Context(), original.PersonID)
				requirements.ErrorIs(err, store.ErrCardDAVPublicationNotFound)
				person, err := st.GetPersonProfileContext(t.Context(), original.PersonID)
				requirements.NoError(err)
				books, err := st.ListCardDAVAddressBooksContext(t.Context(), account.ID)
				requirements.NoError(err)
				requirements.Len(books, 1)
				remote := remoteResource(mapping.Href, person.Person.VCardUID, "Synthetic Remapped", "remapped@example.test", `"remapped-base"`)
				_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: books[0].SyncRevision, Upserts: []store.CardDAVRemoteResource{remote}})
				requirements.NoError(err)
				remapped, err := st.GetCardDAVResourceContext(t.Context(), book.ID, mapping.Href)
				requirements.NoError(err)
				requirements.NotNil(remapped.PersonID)
				requirements.Equal(original.PersonID, *remapped.PersonID)
				replacement := prepareSyntheticReviewedMappedUpdate(t, st, remapped, "Synthetic Replacement")
				requirements.Equal(original.MutationRevision, replacement.MutationRevision, "native row recreation reuses the original revision")
				requirements.Equal(original.PendingOperation, replacement.PendingOperation)
				requirements.NotEmpty(original.PendingIntentID)
				requirements.NotEmpty(replacement.PendingIntentID)
				assertions.NotEqual(original.PendingIntentID, replacement.PendingIntentID, "native row recreation generates a new persisted intent identity")
				if operation == "refresh" {
					books, err = st.ListCardDAVAddressBooksContext(t.Context(), account.ID)
					requirements.NoError(err)
					requirements.Len(books, 1)
					_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: books[0].SyncRevision, NextSyncToken: "synthetic-recovery-token"})
					requirements.NoError(err)
				}
				originalGate := time.Now().UTC().Truncate(time.Microsecond).Add(time.Hour)
				requirements.NoError(st.SetCardDAVRetryAfterContext(t.Context(), originalGate, account.ID))
				before, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), original.PersonID)
				requirements.NoError(err)
				expected := *original
				if evidence != "original_artifact" {
					expected = *replacement
					expected.PendingIntentID = original.PendingIntentID
					if evidence == "empty_nonce" {
						expected.PendingIntentID = ""
					}
				}
				seen := false
				authorize := func(_ context.Context, _ *store.IdentityGrantSelection) error { seen = true; return nil }
				switch operation {
				case "settlement":
					backend, ok := any(st).(authorizedPublicationSettlementStore)
					requirements.True(ok)
					canonical := store.CardDAVRemoteResource{Href: replacement.Href, RemoteUID: person.Person.VCardUID, RemoteETag: `"replacement-canonical"`, RemoteBody: replacement.OutgoingBody, SemanticHash: replacement.OutgoingSemanticHash}
					err = backend.CommitCardDAVPublicationAuthorizedContext(t.Context(), store.CardDAVCanonicalMutation{Publication: expected, Remote: canonical}, authorize)
				case "rollback", "rollback_throttle":
					backend, ok := any(st).(authorizedPublicationRollbackStore)
					requirements.True(ok)
					if operation == "rollback_throttle" {
						err = backend.RollbackCardDAVPublicationThrottleAuthorizedContext(t.Context(), &expected, originalGate.Add(time.Hour), authorize)
					} else {
						err = backend.RollbackCardDAVPublicationAuthorizedContext(t.Context(), &expected, authorize)
					}
				case "retry":
					backend, ok := any(st).(authorizedPublicationRetryStore)
					requirements.True(ok)
					err = backend.SetCardDAVPublicationRetryAfterAuthorizedContext(t.Context(), expected, originalGate.Add(time.Hour), authorize)
				case "refresh":
					backend, ok := any(st).(authorizedPublicationRefreshStore)
					requirements.True(ok)
					_, err = backend.RefreshCardDAVPublicationFenceAuthorizedContext(t.Context(), expected, authorize)
				}
				requirements.ErrorIs(err, store.ErrCardDAVStalePlan)
				assertions.False(seen, "old operation evidence must not admit a replacement intent")
				gateAfter, err := st.GetCardDAVRetryAfterContext(t.Context(), account.ID)
				requirements.NoError(err)
				requirements.NotNil(gateAfter)
				assertions.True(originalGate.Equal(*gateAfter), "obsolete intent cannot extend the account retry gate")
				after, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), original.PersonID)
				requirements.NoError(err)
				beforeJSON, err := json.Marshal(before)
				requirements.NoError(err)
				afterJSON, err := json.Marshal(after)
				requirements.NoError(err)
				assertions.JSONEq(string(beforeJSON), string(afterJSON), "replacement pending intent must remain untouched")
			})
		}
	}
}
