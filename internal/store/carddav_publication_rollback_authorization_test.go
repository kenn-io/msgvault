package store_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/store"
)

type authorizedPublicationRollbackStore interface {
	RollbackCardDAVPublicationAuthorizedContext(ctx context.Context, pending *store.CardDAVPublication, authorize store.PersonEditAuthorizer) error
	RollbackCardDAVPublicationThrottleAuthorizedContext(ctx context.Context, pending *store.CardDAVPublication, retryAfter time.Time, authorize store.PersonEditAuthorizer) error
}

func TestPublicationRollbackAuthorizationPreservesPendingIntentAndRetryGates(t *testing.T) {
	for _, tc := range []struct {
		throttle bool
		delay    time.Duration
		field    string
	}{
		{delay: time.Minute},
		{throttle: true, delay: time.Minute},
		{throttle: true, delay: 3 * time.Hour},
		{field: "foreign_conflict"},
		{field: "conflict_owned"},
		{field: "stale_revision"},
		{throttle: true, field: "foreign_conflict"},
		{throttle: true, field: "conflict_owned"},
		{throttle: true, field: "stale_revision"},
	} {
		throttle := tc.throttle
		for _, allow := range []bool{false, true} {
			t.Run(fmt.Sprintf("throttle_%t_delay_%s_%s_allow_%t", throttle, tc.delay, tc.field, allow), func(t *testing.T) {
				requirements, assertions := require.New(t), assert.New(t)
				st, account, book, mapping := seededCardDAVConflictMapping(t)
				backend, ok := any(st).(authorizedPublicationRollbackStore)
				requirements.True(ok, "native rollback must check current authority before changing recovery evidence")
				requirements.NotNil(mapping.PersonID)
				personID := *mapping.PersonID
				source, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
				requirements.NoError(err)
				body := []byte(fmt.Sprintf("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:%s\r\nFN:Synthetic Corrected\r\nN:Example;Synthetic;;;\r\nEND:VCARD\r\n", source.Person.VCardUID))
				hash, err := carddav.SemanticHash(body)
				requirements.NoError(err)
				plan := store.CardDAVPublicationPlan{PersonID: personID, Desired: true, AddressBookID: book.ID, Href: mapping.Href, OutgoingBody: body, OutgoingSemanticHash: hash, LocalHash: source.Snapshot.Fingerprint}
				fence := store.CardDAVCurrentReviewFence(source, body, mapping.Href)
				pending, err := st.PrepareReviewedCardDAVPublicationContext(t.Context(), store.CardDAVReviewedPublicationPlan{Publication: plan, Fence: fence, ApprovalToken: store.CardDAVReviewToken(fence)})
				requirements.NoError(err)
				requirements.Equal(store.CardDAVMutationUpdate, pending.PendingOperation)
				otherAccount, otherBooks, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
					ConnectionName: "work", BaseURL: "https://dav.example.test", Username: "synthetic-other-user", PrincipalURL: "https://dav.example.test/other-principal/", HomeURL: "https://dav.example.test/books/",
					Books: []store.CardDAVDiscoveredBook{{CanonicalURL: book.CanonicalURL, DisplayName: "Synthetic Other Book", CanCreate: new(true)}},
				})
				requirements.NoError(err)
				requirements.Len(otherBooks, 1)
				ownGate := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
				foreignGate := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Microsecond)
				requirements.NoError(st.SetCardDAVRetryAfterContext(t.Context(), ownGate, account.ID))
				requirements.NoError(st.SetCardDAVRetryAfterContext(t.Context(), foreignGate, otherAccount.ID))
				before, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
				requirements.NoError(err)
				switch tc.field {
				case "foreign_conflict":
					pending.ResolutionConflictID = 42
				case "conflict_owned":
					pending.ConflictOwned = true
				case "stale_revision":
					pending.MutationRevision++
				}
				requestedGate := time.Now().Add(tc.delay).UTC().Truncate(time.Microsecond)
				denied := errors.New("synthetic rollback authority revoked")
				var seen *store.IdentityGrantSelection
				authorize := func(_ context.Context, scope *store.IdentityGrantSelection) error {
					seen = scope
					if !allow {
						return denied
					}
					return nil
				}
				if throttle {
					err = backend.RollbackCardDAVPublicationThrottleAuthorizedContext(t.Context(), pending, requestedGate, authorize)
				} else {
					err = backend.RollbackCardDAVPublicationAuthorizedContext(t.Context(), pending, authorize)
				}
				if tc.field == "stale_revision" {
					requirements.ErrorIs(err, store.ErrCardDAVStalePlan)
				} else if tc.field != "" {
					requirements.ErrorIs(err, store.ErrCardDAVInvalidPlan)
				} else if allow {
					requirements.NoError(err)
				} else {
					requirements.ErrorIs(err, denied)
				}
				if tc.field != "" {
					assertions.Nil(seen)
				} else {
					requirements.NotNil(seen)
					requirements.Len(seen.Persons, 1)
					assertions.Equal(personID, seen.Persons[0].ID)
					assertions.Equal(source.Person.VCardUID, seen.Persons[0].UID)
					requirements.Len(seen.AddressBooks, 1)
					assertions.Equal(account.ID, seen.AddressBooks[0].AccountID)
					assertions.Equal(book.ID, seen.AddressBooks[0].BookID)
				}
				after, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
				requirements.NoError(err)
				requirements.NotNil(after.Publication)
				requirements.NotNil(after.Resource)
				if !allow || tc.field != "" {
					beforeJSON, err := json.Marshal(before)
					requirements.NoError(err)
					afterJSON, err := json.Marshal(after)
					requirements.NoError(err)
					assertions.JSONEq(string(beforeJSON), string(afterJSON), "denied rollback preserves the complete original pending intent")
				} else {
					assertions.Empty(after.Publication.PendingOperation)
					assertions.Equal(mapping.MappingRevision, after.Resource.MappingRevision)
					assertions.Equal(mapping.RemoteBody, after.Resource.RemoteBody)
					assertions.Equal(mapping.RemoteETag, after.Resource.RemoteETag)
				}
				ownAfter, err := st.GetCardDAVRetryAfterContext(t.Context(), account.ID)
				requirements.NoError(err)
				requirements.NotNil(ownAfter)
				expectedGate := ownGate
				if allow && tc.field == "" && throttle && requestedGate.After(ownGate) {
					expectedGate = requestedGate
				}
				assertions.True(expectedGate.Equal(*ownAfter), "only an allowed owning-account rollback may extend its retry gate")
				foreignAfter, err := st.GetCardDAVRetryAfterContext(t.Context(), otherAccount.ID)
				requirements.NoError(err)
				requirements.NotNil(foreignAfter)
				assertions.True(foreignGate.Equal(*foreignAfter), "same-URL foreign account gate is outside the rollback effect")
			})
		}
	}
}
