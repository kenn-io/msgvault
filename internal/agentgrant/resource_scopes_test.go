package agentgrant

import (
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScopedGrantIdentityAndImmutableSelection(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Parallel()
	r := NewRegistry()
	person := PersonRef{ID: 1, UID: "synthetic-person"}
	book := AddressBookRef{AccountID: 2, BookID: 3, CanonicalURL: "https://contacts.example.test/book/", OwnershipFingerprint: "synthetic-owner"}
	scope := ResourceScopes{Persons: []PersonRef{person}, AddressBooks: []AddressBookRef{book}, Sources: []SourceRef{{ID: 4, Type: "imap", Identifier: "agent@example.test", SenderKeys: []string{"agent@example.test"}}}}
	perms := []Permission{PermissionPersonRead, PermissionPersonEdit, PermissionCardDAVWrite}
	id, secret, returned, err := r.IssueScoped("synthetic editor", perms, scope, time.Time{})
	requirements.NoError(err)
	perms[0] = PermissionStatsRead
	scope.Persons[0].UID = "changed"
	scope.AddressBooks[0].OwnershipFingerprint = "changed"
	scope.Sources[0].SenderKeys[0] = "changed@example.test"
	returned.Persons[0].UID = "changed"
	returned.AddressBooks[0].CanonicalURL = "https://other.example.test/"
	returned.Sources[0].SenderKeys[0] = "changed@example.test"
	got, ok := r.Lookup(secret)
	requirements.True(ok)
	assertions.True(got.AllowsPerson(PermissionPersonEdit, person))
	assertions.True(got.AllowsAddressBook(PermissionCardDAVWrite, book))
	assertions.Equal([]string{"agent@example.test"}, got.Sources[0].SenderKeys)
	assertions.False(got.AllowsPerson(PermissionPersonEdit, PersonRef{ID: 1, UID: "recreated"}))
	assertions.False(got.AllowsPerson(PermissionPersonEdit, PersonRef{ID: 5, UID: person.UID}))
	assertions.False(got.AllowsPerson(PermissionPersonMerge, person))
	for _, changed := range []AddressBookRef{
		{AccountID: 9, BookID: book.BookID, CanonicalURL: book.CanonicalURL, OwnershipFingerprint: book.OwnershipFingerprint},
		{AccountID: book.AccountID, BookID: 9, CanonicalURL: book.CanonicalURL, OwnershipFingerprint: book.OwnershipFingerprint},
		{AccountID: book.AccountID, BookID: book.BookID, CanonicalURL: "https://other.example.test/", OwnershipFingerprint: book.OwnershipFingerprint},
		{AccountID: book.AccountID, BookID: book.BookID, CanonicalURL: book.CanonicalURL, OwnershipFingerprint: "recreated"},
	} {
		assertions.False(got.AllowsAddressBook(PermissionCardDAVWrite, changed))
	}
	got.Persons[0].UID = "changed"
	listed := r.List()
	requirements.Len(listed, 1)
	listed[0].Persons[0].UID = "changed"
	listed[0].AddressBooks[0].OwnershipFingerprint = "changed"
	current, ok := r.Lookup(secret)
	requirements.True(ok)
	assertions.True(current.AllowsPerson(PermissionPersonRead, person))
	assertions.True(current.AllowsAddressBook(PermissionCardDAVWrite, book))
	requirements.True(r.Revoke(id))
	_, ok = r.Lookup(secret)
	assertions.False(ok)
}

func TestScopedGrantBoundsAndLegacySources(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	for count := range 102 {
		t.Run(fmt.Sprintf("resources_%d", count), func(t *testing.T) {
			scope := ResourceScopes{}
			for i := range count {
				scope.Persons = append(scope.Persons, PersonRef{ID: int64(i + 1), UID: fmt.Sprint("synthetic-", i)})
			}
			_, _, _, err := r.IssueScoped("synthetic", []Permission{PermissionPersonRead}, scope, time.Time{})
			if count == 0 || count > 100 {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
	for i, scope := range []ResourceScopes{
		{Persons: []PersonRef{{ID: 1, UID: ""}}},
		{Persons: []PersonRef{{ID: 1, UID: "one"}, {ID: 1, UID: "two"}}},
		{Sources: []SourceRef{{ID: 1, Type: "imap", Identifier: "one@example.test"}, {ID: 1, Type: "imap", Identifier: "one@example.test"}}},
		{Sources: []SourceRef{{ID: 1, Type: "imap", Identifier: "one@example.test"}, {ID: 1, Type: "imap", Identifier: "two@example.test"}}},
		{AddressBooks: []AddressBookRef{{AccountID: 1, BookID: 1, CanonicalURL: "https://user:password@contacts.example.test/", OwnershipFingerprint: "owner"}}},
		{AddressBooks: []AddressBookRef{{AccountID: 1, BookID: 1, CanonicalURL: "file:///contacts", OwnershipFingerprint: "owner"}}},
	} {
		t.Run(fmt.Sprintf("invalid_%d", i), func(t *testing.T) {
			_, _, _, err := r.IssueScoped("synthetic", []Permission{PermissionPersonRead}, scope, time.Time{})
			require.Error(t, err)
		})
	}
	legacyID, _, _, err := r.Issue("legacy", []Permission{PermissionSearchRead}, nil, time.Time{})
	require.Error(t, err, "legacy source issuance still requires sources")
	assert.Empty(t, legacyID)
}

func TestScopedGrantExpiry(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)

		r := NewRegistry()
		scope := ResourceScopes{Persons: []PersonRef{{ID: 1, UID: "synthetic-person"}}}
		_, _, _, err := r.IssueScoped("expired", []Permission{PermissionPersonRead}, scope, time.Now())
		requirements.Error(err)
		id, secret, grant, err := r.IssueScoped("expires", []Permission{PermissionPersonRead}, scope, time.Now().Add(time.Hour))
		requirements.NoError(err)
		assertions.Equal(time.Now().Add(time.Hour), grant.ExpiresAt)
		time.Sleep(time.Hour)
		_, ok := r.Lookup(secret)
		assertions.False(ok, "expiry is exclusive")
		requirements.Len(r.List(), 1)
		assertions.True(r.Revoke(id))
	})
}

// The native JSON contract accepts every exact positive integer through 2^53-1
// and rejects every other int64, in each resource-ID position.
func FuzzScopedGrantExactJSONIDs(f *testing.F) {
	for _, id := range []int64{-1, 0, 1, 9_007_199_254_740_991, 9_007_199_254_740_992, 1<<63 - 1} {
		f.Add(id)
	}
	f.Fuzz(func(t *testing.T, id int64) {
		for _, scope := range []ResourceScopes{
			{Persons: []PersonRef{{ID: id, UID: "synthetic-person"}}},
			{Sources: []SourceRef{{ID: id, Type: "imap", Identifier: "synthetic@example.test"}}},
			{AddressBooks: []AddressBookRef{{AccountID: id, BookID: 1, CanonicalURL: "https://contacts.example.test/book/", OwnershipFingerprint: "synthetic-owner"}}},
			{AddressBooks: []AddressBookRef{{AccountID: 1, BookID: id, CanonicalURL: "https://contacts.example.test/book/", OwnershipFingerprint: "synthetic-owner"}}},
		} {
			_, _, _, err := NewRegistry().IssueScoped("synthetic", []Permission{PermissionPersonRead}, scope, time.Time{})
			assert.Equal(t, id >= 1 && id < 1<<53, err == nil)
		}
	})
}
