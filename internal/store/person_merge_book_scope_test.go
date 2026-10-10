package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func seedMergeBookResource(t *testing.T, st *store.Store, book store.CardDAVAddressBook, person *store.Person) {
	t.Helper()
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO carddav_resources (
 address_book_id, href, remote_etag, remote_body, remote_semantic_hash,
 local_hash, mapping_status, governance, person_id, person_revision_at_bind
 ) VALUES (?, ?, ?, ?, ?, ?, 'mapped', 'local', ?, ?)`), book.ID, book.CanonicalURL+person.VCardUID+".vcf", `"synthetic-etag"`, []byte("synthetic card"), "synthetic-remote-hash", "synthetic-local-hash", person.ID, person.Revision)
	require.NoError(t, err)
}

func TestPersonMergeAuthorizationUsesCurrentNativeBookOwnership(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st, account, book := newCardDAVResourceStore(t)
	survivor := mustPromotedPerson(t, st, "book-survivor@example.test", "Synthetic Survivor")
	absorbed := mustPromotedPerson(t, st, "book-absorbed@example.test", "Synthetic Absorbed")
	other := mustPromotedPerson(t, st, "book-other@example.test", "Synthetic Other")
	seedMergeBookResource(t, st, book, other)
	_, err := st.AddPersonRelationshipContext(t.Context(), store.PersonRelationshipInput{SourcePersonID: absorbed.ID, TargetPersonID: other.ID, TypeSlug: "friend", Source: store.ProvenanceUser, Actor: "synthetic-owner"})
	requirements.NoError(err)
	request := store.PersonMergeRequest{SurvivorID: survivor.ID, AbsorbedID: absorbed.ID, ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision, IdempotencyKey: "synthetic-book-scope", Actor: "agent:synthetic-grant"}
	denied := errors.New("synthetic book scope denied")
	initialFingerprint := ""
	_, err = st.MergePersonsAuthorizedContext(t.Context(), request, func(_ context.Context, scope *store.IdentityGrantSelection) error {
		requirements.Len(scope.AddressBooks, 1, "include the affected third person's book")
		assertions.Equal(account.ID, scope.AddressBooks[0].AccountID)
		assertions.Equal(book.ID, scope.AddressBooks[0].BookID)
		assertions.Equal(book.CanonicalURL, scope.AddressBooks[0].CanonicalURL)
		assertions.NotEmpty(scope.AddressBooks[0].OwnershipFingerprint)
		initialFingerprint = scope.AddressBooks[0].OwnershipFingerprint
		return denied
	})
	requirements.ErrorIs(err, denied)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE carddav_accounts SET username = ? WHERE id = ?`), "synthetic-fresh-book-owner", account.ID)
	requirements.NoError(err)
	freshScopeSeen := false
	mergeTimeFingerprint := ""
	_, err = st.MergePersonsAuthorizedContext(t.Context(), request, func(_ context.Context, scope *store.IdentityGrantSelection) error {
		freshScopeSeen = true
		requirements.Len(scope.AddressBooks, 1)
		assertions.NotEqual(initialFingerprint, scope.AddressBooks[0].OwnershipFingerprint, "fresh merge must resolve current ownership")
		mergeTimeFingerprint = scope.AddressBooks[0].OwnershipFingerprint
		return denied
	})
	requirements.ErrorIs(err, denied)
	assertions.True(freshScopeSeen)
	requirements.NotEmpty(mergeTimeFingerprint)
	// Its resource becomes survivor-owned and must still require current book scope.
	seedMergeBookResource(t, st, book, absorbed)
	_, err = st.MergePersonsContext(t.Context(), request)
	requirements.NoError(err)
	// Simulate ownership drift in restored native metadata, without real credentials.
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE carddav_accounts SET username = ? WHERE id = ?`), "synthetic-reassigned-owner", account.ID)
	requirements.NoError(err)
	called := false
	_, err = st.MergePersonsAuthorizedContext(t.Context(), request, func(_ context.Context, scope *store.IdentityGrantSelection) error {
		called = true
		requirements.Len(scope.AddressBooks, 1)
		assertions.Equal(account.ID, scope.AddressBooks[0].AccountID)
		assertions.Equal(book.ID, scope.AddressBooks[0].BookID)
		assertions.NotEqual(mergeTimeFingerprint, scope.AddressBooks[0].OwnershipFingerprint, "replay must resolve current ownership rather than receipt-time ownership")
		return denied
	})
	requirements.ErrorIs(err, denied)
	assertions.True(called)
}

func TestPersonMergeAuthorizationBoundsCombinedPeopleAndBooks(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st, account, _ := newCardDAVResourceStore(t)
	survivor := mustPromotedPerson(t, st, "limit-survivor@example.test", "Synthetic Survivor")
	absorbed := mustPromotedPerson(t, st, "limit-absorbed@example.test", "Synthetic Absorbed")
	discovered := make([]store.CardDAVDiscoveredBook, 99)
	for i := range discovered {
		discovered[i] = store.CardDAVDiscoveredBook{CanonicalURL: fmt.Sprintf("%ssynthetic-%d/", account.HomeURL, i), DisplayName: "Synthetic scope limit"}
	}
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{BaseURL: account.BaseURL, Username: account.Username, PrincipalURL: account.PrincipalURL, HomeURL: account.HomeURL, Books: discovered})
	requirements.NoError(err)
	requirements.Len(books, 99)
	for _, book := range books {
		seedMergeBookResource(t, st, book, absorbed)
	}
	request := store.PersonMergeRequest{SurvivorID: survivor.ID, AbsorbedID: absorbed.ID, ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision, IdempotencyKey: "synthetic-combined-limit", Actor: "agent:synthetic-grant"}
	called := false
	authorize := func(context.Context, *store.IdentityGrantSelection) error { called = true; return nil }
	_, err = st.MergePersonsAuthorizedContext(t.Context(), request, authorize)
	requirements.ErrorIs(err, store.ErrIdentityOperationTooLarge)
	assertions.False(called, "two roots plus 99 books exceed the combined bound")
	_, err = st.GetPerson(absorbed.ID)
	requirements.NoError(err)
	merged, err := st.MergePersonsContext(t.Context(), request)
	requirements.NoError(err)
	_, err = st.MergePersonsAuthorizedContext(t.Context(), request, authorize)
	requirements.ErrorIs(err, store.ErrIdentityOperationTooLarge)
	assertions.False(called, "replay must count the historical absorbed root")
	current, err := st.GetPerson(survivor.ID)
	requirements.NoError(err)
	assertions.Equal(merged.Person, *current)
}

func TestPersonMergeAuthorizationAcceptsExactlyHundredResources(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st, account, _ := newCardDAVResourceStore(t)
	survivor := mustPromotedPerson(t, st, "boundary-survivor@example.test", "Synthetic Survivor")
	absorbed := mustPromotedPerson(t, st, "boundary-absorbed@example.test", "Synthetic Absorbed")
	discovered := make([]store.CardDAVDiscoveredBook, 98)
	for i := range discovered {
		discovered[i] = store.CardDAVDiscoveredBook{CanonicalURL: fmt.Sprintf("%sboundary-%d/", account.HomeURL, i), DisplayName: "Synthetic scope boundary"}
	}
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{BaseURL: account.BaseURL, Username: account.Username, PrincipalURL: account.PrincipalURL, HomeURL: account.HomeURL, Books: discovered})
	requirements.NoError(err)
	requirements.Len(books, 98)
	for _, book := range books {
		seedMergeBookResource(t, st, book, absorbed)
	}
	called := false
	merged, err := st.MergePersonsAuthorizedContext(t.Context(), store.PersonMergeRequest{SurvivorID: survivor.ID, AbsorbedID: absorbed.ID, ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision, IdempotencyKey: "synthetic-exact-resource-boundary", Actor: "agent:synthetic-grant"}, func(_ context.Context, scope *store.IdentityGrantSelection) error {
		called = true
		assertions.Len(scope.Persons, 2)
		assertions.Len(scope.AddressBooks, 98)
		return nil
	})
	requirements.NoError(err)
	assertions.True(called)
	assertions.Equal(survivor.ID, merged.Person.ID)
	_, err = st.GetPerson(absorbed.ID)
	requirements.ErrorIs(err, store.ErrPersonNotFound)
}
