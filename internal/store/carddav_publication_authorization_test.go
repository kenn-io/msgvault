package store_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vcard"
)

type authorizedReviewedPublicationStore interface {
	PrepareReviewedCardDAVPublicationAuthorizedContext(ctx context.Context, plan store.CardDAVReviewedPublicationPlan, authorize store.PersonEditAuthorizer) (*store.CardDAVPublication, error)
}

func TestReviewedPublicationAuthorizesBeforeAnyNativePreparationWrite(t *testing.T) {
	for _, change := range []bool{false, true} {
		for _, allow := range []bool{false, true} {
			name := "unchanged"
			if change {
				name = "changed"
			}
			if allow {
				name += "-allowed"
			} else {
				name += "-denied"
			}
			t.Run(name, func(t *testing.T) {
				requirements := require.New(t)
				assertions := assert.New(t)
				st := testutil.NewTestStore(t)
				backend, ok := any(st).(authorizedReviewedPublicationStore)
				requirements.True(ok, "native reviewed publication must support in-transaction authorization")
				account, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
					ConnectionName: "default", BaseURL: "https://dav.example.test", Username: "synthetic-user",
					PrincipalURL: "https://dav.example.test/principal/", HomeURL: "https://dav.example.test/books/",
					Books: []store.CardDAVDiscoveredBook{{CanonicalURL: "https://dav.example.test/books/scoped/", DisplayName: "Synthetic Scoped Book", CanCreate: new(true), CanUpdate: new(true)}},
				})
				requirements.NoError(err)
				requirements.Len(books, 1)
				book := books[0]
				body := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:synthetic-scoped-publication\r\nFN:Synthetic Original\r\nEMAIL:scoped-publication@example.test\r\nEND:VCARD\r\n")
				semanticHash, err := carddav.SemanticHash(body)
				requirements.NoError(err)
				remote := store.CardDAVRemoteResource{Href: book.CanonicalURL + "person.vcf", RemoteUID: "synthetic-scoped-publication", RemoteETag: `"base"`, RemoteBody: body, SemanticHash: semanticHash, DisplayName: "Synthetic Original", Emails: []string{"scoped-publication@example.test"}}
				_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{AddressBookID: book.ID, ConnectionGeneration: account.ConnectionGeneration, SyncRevision: book.SyncRevision, Upserts: []store.CardDAVRemoteResource{remote}})
				requirements.NoError(err)
				mapping, err := st.GetCardDAVResourceContext(t.Context(), book.ID, remote.Href)
				requirements.NoError(err)
				requirements.NotNil(mapping.PersonID)
				personID := *mapping.PersonID
				_, err = st.AppendPersonNoteContext(t.Context(), store.PersonNoteAppendInput{PersonID: personID, Text: "Synthetic inferred detail", Source: store.ProvenanceExtraction})
				requirements.NoError(err)
				person, err := st.GetPersonContext(t.Context(), personID)
				requirements.NoError(err)
				// Another connection can map the same person at an identical URL.
				// Publishing one mapped resource must not require its unrelated book.
				otherAccount, otherBooks, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{
					ConnectionName: "work", BaseURL: "https://dav.example.test", Username: "synthetic-other-user",
					PrincipalURL: "https://dav.example.test/other-principal/", HomeURL: "https://dav.example.test/books/",
					Books: []store.CardDAVDiscoveredBook{{CanonicalURL: "https://dav.example.test/books/scoped/", DisplayName: "Synthetic Other Book"}},
				})
				requirements.NoError(err)
				requirements.Len(otherBooks, 1)
				otherBook := otherBooks[0]
				otherBody := []byte(fmt.Sprintf("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:%s\r\nFN:Synthetic Other\r\nEND:VCARD\r\n", person.VCardUID))
				otherHash, err := carddav.SemanticHash(otherBody)
				requirements.NoError(err)
				otherRemote := store.CardDAVRemoteResource{Href: otherBook.CanonicalURL + "other.vcf", RemoteUID: person.VCardUID, RemoteETag: `"other"`, RemoteBody: otherBody, SemanticHash: otherHash, DisplayName: "Synthetic Other"}
				_, err = st.ApplyCardDAVSyncPlanContext(t.Context(), store.CardDAVSyncPlan{AddressBookID: otherBook.ID, ConnectionGeneration: otherAccount.ConnectionGeneration, SyncRevision: otherBook.SyncRevision, Upserts: []store.CardDAVRemoteResource{otherRemote}})
				requirements.NoError(err)
				otherMapping, err := st.GetCardDAVResourceContext(t.Context(), otherBook.ID, otherRemote.Href)
				requirements.NoError(err)
				requirements.NotNil(otherMapping.PersonID)
				requirements.Equal(personID, *otherMapping.PersonID)
				source, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
				requirements.NoError(err)
				requirements.Positive(source.Inference.InferenceRevision)
				if change {
					body = []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:synthetic-scoped-publication\r\nFN:Synthetic Corrected\r\nEMAIL:scoped-publication@example.test\r\nEND:VCARD\r\n")
					semanticHash, err = carddav.SemanticHash(body)
					requirements.NoError(err)
				}
				plan := store.CardDAVPublicationPlan{PersonID: personID, Desired: true, AddressBookID: book.ID, Href: remote.Href, OutgoingBody: body, OutgoingSemanticHash: semanticHash, LocalHash: source.Snapshot.Fingerprint}
				if !change {
					requirements.NotNil(source.Envelope)
					plan.OutgoingEnvelopeMetadata, err = vcard.MarshalResourceMetadata(source.Envelope.ResourceEnvelope)
					requirements.NoError(err)
					requirements.NotEmpty(plan.OutgoingEnvelopeMetadata)
				}
				fence := store.CardDAVCurrentReviewFence(source, body, remote.Href)
				reviewed := store.CardDAVReviewedPublicationPlan{Publication: plan, Fence: fence, ApprovalToken: store.CardDAVReviewToken(fence)}
				denied := errors.New("synthetic grant revoked")
				var seen *store.IdentityGrantSelection
				result, err := backend.PrepareReviewedCardDAVPublicationAuthorizedContext(t.Context(), reviewed, func(_ context.Context, scope *store.IdentityGrantSelection) error {
					seen = scope
					if !allow {
						return denied
					}
					return nil
				})
				if allow {
					requirements.NoError(err)
					requirements.NotNil(result)
				} else {
					requirements.ErrorIs(err, denied)
					assertions.Nil(result)
				}
				requirements.NotNil(seen)
				requirements.Len(seen.Persons, 1)
				assertions.Equal(personID, seen.Persons[0].ID)
				assertions.Equal(source.Person.VCardUID, seen.Persons[0].UID)
				requirements.Len(seen.AddressBooks, 1)
				assertions.Equal(account.ID, seen.AddressBooks[0].AccountID)
				assertions.Equal(book.ID, seen.AddressBooks[0].BookID)
				assertions.Equal("https://dav.example.test/books/scoped/", seen.AddressBooks[0].CanonicalURL)
				assertions.NotEmpty(seen.AddressBooks[0].OwnershipFingerprint)
				after, err := st.LoadCardDAVPublicationReviewSourceContext(t.Context(), personID)
				requirements.NoError(err)
				otherAfter, err := st.GetCardDAVResourceContext(t.Context(), otherBook.ID, otherRemote.Href)
				requirements.NoError(err)
				otherBeforeJSON, err := json.Marshal(otherMapping)
				requirements.NoError(err)
				otherAfterJSON, err := json.Marshal(otherAfter)
				requirements.NoError(err)
				assertions.JSONEq(string(otherBeforeJSON), string(otherAfterJSON), "preparation leaves the other connection's mapped resource intact")
				if !allow {
					beforeJSON, err := json.Marshal(source)
					requirements.NoError(err)
					afterJSON, err := json.Marshal(after)
					requirements.NoError(err)
					assertions.JSONEq(string(beforeJSON), string(afterJSON), "denied preparation preserves mapping, envelope, inference approval, and publication ledger")
				} else {
					assertions.Equal(source.Inference.InferenceRevision, after.Inference.ApprovedRevision)
					assertions.Equal(!change, result.Noop)
					if change {
						assertions.Equal(store.CardDAVMutationUpdate, result.PendingOperation)
					} else {
						assertions.Empty(result.PendingOperation)
					}
				}
			})
		}
	}
}
