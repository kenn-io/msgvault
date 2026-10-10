package agentgrant

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIdentityGrantScopesRequireExactPersonAndAction(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	r := NewRegistry()
	scope := ResourceScopes{Persons: []PersonRef{{ID: 7, UID: "synthetic-person-uid"}}}
	_, secret, grant, err := r.IssueScoped("Synthetic contact grant", []Permission{PermissionIdentityRead, PermissionIdentityLink}, scope)
	requirements.NoError(err)
	assertions.True(grant.AllowsPerson(PermissionIdentityRead, scope.Persons[0]))
	assertions.True(grant.AllowsPerson(PermissionIdentityLink, scope.Persons[0]))
	assertions.False(grant.AllowsPerson(PermissionIdentityUnlink, scope.Persons[0]))
	assertions.False(grant.AllowsPerson(PermissionIdentityRead, PersonRef{ID: 7, UID: "replacement-person-uid"}))
	assertions.False(grant.AllowsPerson(PermissionIdentityRead, PersonRef{ID: 8, UID: scope.Persons[0].UID}))
	scope.Persons[0].UID = "caller-mutated-uid"
	grant.Persons[0].UID = "returned-mutated-uid"
	current, ok := r.Lookup(secret)
	requirements.True(ok)
	assertions.Equal("synthetic-person-uid", current.Persons[0].UID)
	current.Persons[0].UID = "lookup-mutated-uid"
	listed := r.List()
	requirements.Len(listed, 1)
	assertions.Equal("synthetic-person-uid", listed[0].Persons[0].UID)
	requirements.True(r.Revoke(grant.ID))
	_, ok = r.Lookup(secret)
	assertions.False(ok)
}

func TestIdentityGrantAddressBookScopeCannotFollowOwnershipChange(t *testing.T) {
	assertions := assert.New(t)

	book := AddressBookRef{AccountID: 3, BookID: 4, CanonicalURL: "https://contacts.example.test/books/synthetic/", OwnershipFingerprint: "synthetic-account-fingerprint"}
	_, _, grant, err := NewRegistry().IssueScoped("Synthetic address book grant", []Permission{PermissionIdentityRead}, ResourceScopes{AddressBooks: []AddressBookRef{book}})
	require.NoError(t, err)
	assertions.True(grant.AllowsAddressBook(PermissionIdentityRead, book))
	for _, changed := range []AddressBookRef{
		{AccountID: 5, BookID: book.BookID, CanonicalURL: book.CanonicalURL, OwnershipFingerprint: book.OwnershipFingerprint},
		{AccountID: book.AccountID, BookID: 5, CanonicalURL: book.CanonicalURL, OwnershipFingerprint: book.OwnershipFingerprint},
		{AccountID: book.AccountID, BookID: book.BookID, CanonicalURL: "https://contacts.example.test/books/other/", OwnershipFingerprint: book.OwnershipFingerprint},
		{AccountID: book.AccountID, BookID: book.BookID, CanonicalURL: book.CanonicalURL, OwnershipFingerprint: "changed-account-fingerprint"},
	} {
		assertions.False(grant.AllowsAddressBook(PermissionIdentityRead, changed))
	}
	assertions.False(grant.AllowsAddressBook(PermissionIdentityLink, book))
}

func TestIdentityGrantResourceScopesAreBoundedAndExplicit(t *testing.T) {
	persons := make([]PersonRef, 101)
	for i := range persons {
		persons[i] = PersonRef{ID: int64(i + 1), UID: "synthetic-uid"}
	}
	cases := []ResourceScopes{
		{},
		{Sources: []SourceRef{{ID: 9_007_199_254_740_992, Type: "imap", Identifier: "synthetic-owner@example.test"}}},
		{Persons: []PersonRef{{ID: 0, UID: "synthetic"}}},
		{Persons: []PersonRef{{ID: 7, UID: ""}}},
		{Persons: []PersonRef{{ID: 9_007_199_254_740_992, UID: "synthetic"}}},
		{Persons: []PersonRef{{ID: 7, UID: "synthetic"}, {ID: 7, UID: "synthetic"}}},
		{Persons: persons},
		{AddressBooks: []AddressBookRef{{AccountID: 1, BookID: 2, CanonicalURL: "", OwnershipFingerprint: "synthetic"}}},
	}
	for _, scope := range cases {
		r := NewRegistry()
		_, _, _, err := r.IssueScoped("Synthetic invalid scope", []Permission{PermissionIdentityRead}, scope)
		require.Error(t, err)
		assert.Empty(t, r.List())
	}
}

func TestIdentityGrantScopeBoundaryAndBookCopies(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	persons := make([]PersonRef, 100)
	for i := range persons {
		persons[i] = PersonRef{ID: int64(i + 1), UID: "synthetic-person-uid"}
	}
	_, _, grant, err := NewRegistry().IssueScoped("Synthetic bounded people", []Permission{PermissionIdentityRead}, ResourceScopes{Persons: persons})
	requirements.NoError(err)
	assertions.Len(grant.Persons, 100)
	book := AddressBookRef{AccountID: 3, BookID: 4, CanonicalURL: "https://contacts.example.test/books/synthetic/", OwnershipFingerprint: "synthetic-account-fingerprint"}
	books := []AddressBookRef{book}
	r := NewRegistry()
	_, secret, issued, err := r.IssueScoped("Synthetic immutable book", []Permission{PermissionIdentityRead}, ResourceScopes{AddressBooks: books})
	requirements.NoError(err)
	books[0].CanonicalURL = "https://contacts.example.test/books/changed/"
	issued.AddressBooks[0].OwnershipFingerprint = "changed-return-value"
	current, ok := r.Lookup(secret)
	requirements.True(ok)
	assertions.Equal(book, current.AddressBooks[0])
	current.AddressBooks[0].BookID = 99
	assertions.True(r.List()[0].AllowsAddressBook(PermissionIdentityRead, book))
}

func TestIdentityGrantSourcesRemainDurableWithoutImpliedPersonAuthority(t *testing.T) {
	assertions := assert.New(t)

	source := SourceRef{ID: 1, Type: "imap", Identifier: "synthetic-owner@example.test"}
	_, _, grant, err := NewRegistry().IssueScoped("Synthetic source grant", []Permission{PermissionIdentityRead}, ResourceScopes{Sources: []SourceRef{source}})
	require.NoError(t, err)
	source.ID = 99
	assertions.True(grant.Allows(PermissionIdentityRead, source))
	assertions.False(grant.AllowsPerson(PermissionIdentityRead, PersonRef{ID: 1, UID: "synthetic-person"}))
	assertions.False(grant.Allows(PermissionIdentityLink, source))
}
