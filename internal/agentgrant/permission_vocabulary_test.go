package agentgrant

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/authz"
)

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
