package agentgrant

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/authz"
)

func TestSharedPermissionVocabulary(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	t.Parallel()
	// Values cross the package boundary without conversion. Grant issuance still
	// accepts only the narrower delegated capability vocabulary.
	permission := PermissionMessageRead
	source := SourceRef{ID: 1, Type: "test", Identifier: "reader@example.test"}
	person := PersonRef{ID: 2, UID: "synthetic-person"}
	book := AddressBookRef{AccountID: 1, BookID: 3, CanonicalURL: "https://example.test/contacts/", OwnershipFingerprint: "synthetic-owner"}
	registry := NewRegistry()
	_, secret, grant, err := registry.IssueScoped("reader", []authz.Permission{permission}, ResourceScopes{Sources: []authz.SourceRef{source}, Persons: []authz.PersonRef{person}, AddressBooks: []authz.AddressBookRef{book}}, time.Time{})
	require.NoError(err)
	assert.True(grant.HasPermission(authz.PermissionMessageRead))
	lookedUp, ok := registry.Lookup(secret)
	require.True(ok)
	assert.Equal(grant, lookedUp)
}

func FuzzPermissionVocabulary(f *testing.F) {
	// These literal sets are independent of the production definitions.
	delegated := []string{
		"search.read", "message.read", "attachment.read", "stats.read",
		"draft.create", "draft.edit", "draft.delete", "calendar.read",
		"calendar.event.read", "calendar.write", "calendar.invite",
		"person.read", "person.edit", "person.merge", "carddav.write",
	}
	roleOnly := []string{
		"system.read", "curation.write", "cli.read", "collections.write",
		"archive.export", "deletion.stage", "sql.read", "archive.admin", "grant.manage",
	}
	for _, name := range append(slices.Clone(delegated), roleOnly...) {
		f.Add(name)
	}
	for _, name := range []string{"", "*", "message.read.extra", "MESSAGE.READ", "message.read\x00", " message.read", "message.reаd", "☃"} {
		f.Add(name)
	}
	f.Fuzz(func(t *testing.T, name string) {
		wantDelegated := slices.Contains(delegated, name)
		wantKnown := wantDelegated || slices.Contains(roleOnly, name)
		permission, known := authz.KnownPermission(name)
		assert.Equal(t, wantKnown, known)
		wantPermission := Permission("")
		if wantKnown {
			wantPermission = Permission(name)
		}
		assert.Equal(t, wantPermission, permission)
		assert.Equal(t, wantDelegated, authz.Grantable(Permission(name)))
		permission, known = KnownPermission(name)
		assert.Equal(t, wantDelegated, known)
		wantPermission = ""
		if wantDelegated {
			wantPermission = Permission(name)
		}
		assert.Equal(t, wantPermission, permission)

		for _, scoped := range []bool{false, true} {
			registry := NewRegistry()
			sources := []SourceRef{{ID: 1, Type: "test", Identifier: "reader@example.test"}}
			// Invalid entries must reject the entire grant, regardless of their
			// position among otherwise valid permissions.
			for position := range 3 {
				permissions := []Permission{PermissionMessageRead, PermissionSearchRead}
				permissions = slices.Insert(permissions, position, Permission(name))
				var secret string
				var err error
				if scoped {
					_, secret, _, err = registry.IssueScoped("bounded", permissions, ResourceScopes{Sources: sources}, time.Time{})
				} else {
					_, secret, _, err = registry.Issue("bounded", permissions, sources, time.Time{})
				}
				if wantDelegated {
					require.NoError(t, err)
					grant, ok := registry.Lookup(secret)
					require.True(t, ok)
					assert.Equal(t, permissions, grant.Permissions)
				} else {
					require.Error(t, err)
					assert.Empty(t, secret)
					assert.Empty(t, registry.List())
				}
			}
		}
	})
}

func TestRoleOnlyPermissionsCannotBeGranted(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"archive.admin", "system.read", "grant.manage", "sql.read", "curation.write", "cli.read", "collections.write", "deletion.stage", "archive.export"} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			permission, known := authz.KnownPermission(name)
			require.True(known)
			assert.False(authz.Grantable(permission))
			_, grantable := KnownPermission(name)
			assert.False(grantable)
			registry := NewRegistry()
			sources := []SourceRef{{ID: 1, Type: "test", Identifier: "reader@example.test"}}
			_, secret, _, err := registry.Issue("bounded", []Permission{Permission(name)}, sources, time.Time{})
			require.ErrorContains(err, "unknown permission")
			assert.Empty(secret)
			_, secret, _, err = registry.IssueScoped("bounded", []Permission{Permission(name)}, ResourceScopes{Sources: sources}, time.Time{})
			require.ErrorContains(err, "unknown permission")
			assert.Empty(secret)
			assert.Empty(registry.List())
		})
	}
}
