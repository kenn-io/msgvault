package store_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

type authorizedPublicationRetryStore interface {
	SetCardDAVPublicationRetryAfterAuthorizedContext(ctx context.Context, expected store.CardDAVPublication, retryAfter time.Time, authorize store.PersonEditAuthorizer) error
}

func TestPublicationRetryGateAuthorizationPreservesRevokedPendingIntent(t *testing.T) {
	for _, field := range []string{"allowed", "denied", "stale_revision", "foreign_conflict", "conflict_owned"} {
		t.Run(field, func(t *testing.T) {
			requirements, assertions := require.New(t), assert.New(t)
			st, account, book, mapping := seededCardDAVConflictMapping(t)
			backend, ok := any(st).(authorizedPublicationRetryStore)
			requirements.True(ok, "a provider pause must authorize local retry-gate writes for the pending publication")
			pending := prepareSyntheticReviewedMappedUpdate(t, st, mapping, "Synthetic Corrected")
			otherAccount, otherBooks, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
				ConnectionName: "work", BaseURL: "https://dav.example.test", Username: "synthetic-other-user", PrincipalURL: "https://dav.example.test/other-principal/", HomeURL: "https://dav.example.test/books/",
				Books: []store.CardDAVDiscoveredBook{{CanonicalURL: book.CanonicalURL, DisplayName: "Synthetic Other Book", CanCreate: new(true)}},
			})
			requirements.NoError(err)
			requirements.Len(otherBooks, 1)
			clock := time.Now().UTC().Truncate(time.Microsecond)
			ownGate, foreignGate, requestedGate := clock.Add(time.Hour), clock.Add(2*time.Hour), clock.Add(3*time.Hour)
			requirements.NoError(st.SetCardDAVRetryAfterContext(t.Context(), ownGate, account.ID))
			requirements.NoError(st.SetCardDAVRetryAfterContext(t.Context(), foreignGate, otherAccount.ID))
			before, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), pending.PersonID)
			requirements.NoError(err)
			expected := *pending
			switch field {
			case "stale_revision":
				expected.MutationRevision++
			case "foreign_conflict":
				expected.ResolutionConflictID = 42
			case "conflict_owned":
				expected.ConflictOwned = true
			}
			denied := errors.New("synthetic provider pause authority revoked")
			var seen *store.IdentityGrantSelection
			err = backend.SetCardDAVPublicationRetryAfterAuthorizedContext(t.Context(), expected, requestedGate, func(_ context.Context, scope *store.IdentityGrantSelection) error {
				seen = scope
				if field == "denied" {
					return denied
				}
				return nil
			})
			switch field {
			case "allowed":
				requirements.NoError(err)
			case "denied":
				requirements.ErrorIs(err, denied)
			case "stale_revision":
				requirements.ErrorIs(err, store.ErrCardDAVStalePlan)
			default:
				requirements.ErrorIs(err, store.ErrCardDAVInvalidPlan)
			}
			if field == "allowed" || field == "denied" {
				requirements.NotNil(seen)
				requirements.Len(seen.Persons, 1)
				assertions.Equal(pending.PersonID, seen.Persons[0].ID)
				assertions.Equal(before.Person.VCardUID, seen.Persons[0].UID)
				requirements.Len(seen.AddressBooks, 1)
				assertions.Equal(account.ID, seen.AddressBooks[0].AccountID)
				assertions.Equal(book.ID, seen.AddressBooks[0].BookID)
			} else {
				assertions.Nil(seen)
			}
			after, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), pending.PersonID)
			requirements.NoError(err)
			beforeJSON, err := json.Marshal(before)
			requirements.NoError(err)
			afterJSON, err := json.Marshal(after)
			requirements.NoError(err)
			assertions.JSONEq(string(beforeJSON), string(afterJSON), "a provider pause retains the complete exact pending publication")
			ownAfter, err := st.GetCardDAVRetryAfterContext(t.Context(), account.ID)
			requirements.NoError(err)
			requirements.NotNil(ownAfter)
			expectedGate := ownGate
			if field == "allowed" {
				expectedGate = requestedGate
			}
			assertions.True(expectedGate.Equal(*ownAfter), "only current publication authority may extend the native owning-account retry deadline")
			foreignAfter, err := st.GetCardDAVRetryAfterContext(t.Context(), otherAccount.ID)
			requirements.NoError(err)
			requirements.NotNil(foreignAfter)
			assertions.True(foreignGate.Equal(*foreignAfter), "same-URL foreign account retry state is preserved")
		})
	}
}
