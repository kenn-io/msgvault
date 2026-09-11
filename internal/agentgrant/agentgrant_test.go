package agentgrant

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGrantPermissionsDoNotImply covers proof matrix rows 12 and 13.
func TestGrantPermissionsDoNotImply(t *testing.T) {
	src := SourceRef{ID: 1, Type: "imap", Identifier: "alice@example.com"}

	t.Run("Allows false when source Type differs", func(t *testing.T) {
		g := Grant{
			ID:          "id4",
			Permissions: []Permission{PermissionDraftCreate},
			Sources:     []SourceRef{src},
		}
		different := SourceRef{ID: src.ID, Type: "gmail", Identifier: src.Identifier}
		assert.False(t, g.Allows(PermissionDraftCreate, different))
	})

	t.Run("Allows false when source Identifier differs", func(t *testing.T) {
		g := Grant{
			ID:          "id5",
			Permissions: []Permission{PermissionDraftCreate},
			Sources:     []SourceRef{src},
		}
		different := SourceRef{ID: src.ID, Type: src.Type, Identifier: "bob@example.com"}
		assert.False(t, g.Allows(PermissionDraftCreate, different))
	})

	t.Run("Allows true when source ID differs but Type and Identifier match", func(t *testing.T) {
		// After D7: ID is diagnostic only; matching uses (Type, Identifier).
		g := Grant{
			ID:          "id3",
			Permissions: []Permission{PermissionDraftCreate},
			Sources:     []SourceRef{src},
		}
		sameTypeAndIdentifier := SourceRef{ID: 99, Type: src.Type, Identifier: src.Identifier}
		assert.True(t, g.Allows(PermissionDraftCreate, sameTypeAndIdentifier),
			"same Type+Identifier with different ID must be allowed: ID is diagnostic only")
	})
}

// TestRegistryLifecycle covers proof matrix rows 12 and 13.
func TestRegistryLifecycle(t *testing.T) {
	src := SourceRef{ID: 1, Type: "imap", Identifier: "alice@example.com"}
	perms := []Permission{PermissionDraftCreate}

	t.Run("new registry has zero grants", func(t *testing.T) {
		r := NewRegistry()
		assert.Empty(t, r.List())
	})

	t.Run("Issue rejects empty label", func(t *testing.T) {
		r := NewRegistry()
		_, _, _, err := r.Issue("", perms, []SourceRef{src})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "label")
	})

	t.Run("Issue rejects empty permissions", func(t *testing.T) {
		r := NewRegistry()
		_, _, _, err := r.Issue("test", []Permission{}, []SourceRef{src})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "permission")
	})

	t.Run("Issue rejects empty sources", func(t *testing.T) {
		r := NewRegistry()
		_, _, _, err := r.Issue("test", perms, []SourceRef{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "source")
	})

	t.Run("Issue rejects nonpositive source ID", func(t *testing.T) {
		r := NewRegistry()
		_, _, _, err := r.Issue("test", perms, []SourceRef{{ID: 0, Type: "imap", Identifier: "x"}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "positive")
	})

	t.Run("Issue rejects negative source ID", func(t *testing.T) {
		r := NewRegistry()
		_, _, _, err := r.Issue("test", perms, []SourceRef{{ID: -1, Type: "imap", Identifier: "x"}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "positive")
	})

	t.Run("Issue rejects empty source Type", func(t *testing.T) {
		r := NewRegistry()
		_, _, _, err := r.Issue("test", perms, []SourceRef{{ID: 1, Type: "", Identifier: "x"}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Type")
	})

	t.Run("Issue rejects empty source Identifier", func(t *testing.T) {
		r := NewRegistry()
		_, _, _, err := r.Issue("test", perms, []SourceRef{{ID: 1, Type: "imap", Identifier: ""}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Identifier")
	})

	t.Run("Issue rejects duplicate source Type+Identifier", func(t *testing.T) {
		r := NewRegistry()
		sources := []SourceRef{src, {ID: src.ID + 1, Type: src.Type, Identifier: src.Identifier}}
		_, _, _, err := r.Issue("test", perms, sources)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "duplicate")
	})

	t.Run("Issue allows same ID with different Type+Identifier", func(t *testing.T) {
		r := NewRegistry()
		sources := []SourceRef{src, {ID: src.ID, Type: "imap", Identifier: "bob@example.com"}}
		_, _, _, err := r.Issue("test", perms, sources)
		require.NoError(t, err, "same ID with different (Type, Identifier) must be accepted")
	})

	t.Run("Issue rejects unknown permission", func(t *testing.T) {
		r := NewRegistry()
		_, _, _, err := r.Issue("test", []Permission{"unknown.perm"}, []SourceRef{src})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown")
	})

	t.Run("Issue rejects wildcard permission", func(t *testing.T) {
		r := NewRegistry()
		_, _, _, err := r.Issue("test", []Permission{"*"}, []SourceRef{src})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid")
	})

	t.Run("secret has expected prefix", func(t *testing.T) {
		r := NewRegistry()
		_, secret, _, err := r.Issue("test", perms, []SourceRef{src})
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(secret, secretPrefix), "secret should start with %s", secretPrefix)
	})

	t.Run("revoked grant fails next Lookup", func(t *testing.T) {
		assert := assert.New(t)
		r := NewRegistry()
		id, secret, _, err := r.Issue("test", perms, []SourceRef{src})
		require.NoError(t, err)

		// Confirm it works before revocation
		_, ok := r.Lookup(secret)
		assert.True(ok)

		revoked := r.Revoke(id)
		assert.True(revoked)

		_, ok = r.Lookup(secret)
		assert.False(ok)
	})

	t.Run("Revoke nonexistent ID returns false", func(t *testing.T) {
		r := NewRegistry()
		revoked := r.Revoke("nonexistent-id")
		assert.False(t, revoked)
	})

	t.Run("Lookup with wrong secret returns false", func(t *testing.T) {
		r := NewRegistry()
		_, _, _, err := r.Issue("test", perms, []SourceRef{src})
		require.NoError(t, err)
		_, ok := r.Lookup("wrongsecret")
		assert.False(t, ok)
	})

	t.Run("Close empties the registry", func(t *testing.T) {
		r := NewRegistry()
		_, _, _, err := r.Issue("test", perms, []SourceRef{src})
		require.NoError(t, err)
		require.Len(t, r.List(), 1)
		r.Close()
		assert.Empty(t, r.List())
	})
}

// TestGrantAllowsExactOriginalTriple covers proof matrix row 6 (novel assertion).
// The exact (ID, Type, Identifier) triple that was issued must be allowed; this
// complements TestGrantPermissionsDoNotImply which covers partial-match cases.
func TestGrantAllowsExactOriginalTriple(t *testing.T) {
	original := SourceRef{ID: 5, Type: "imap", Identifier: "imap://alice@example.com"}
	g := Grant{
		ID:          "g-original",
		Permissions: []Permission{PermissionDraftCreate},
		Sources:     []SourceRef{original},
	}
	assert.True(t, g.Allows(PermissionDraftCreate, original),
		"exact original (ID, Type, Identifier) triple must be allowed")
}

func TestIssuePermissionValidation(t *testing.T) {
	src := SourceRef{ID: 1, Type: "imap", Identifier: "alice@example.com"}

	t.Run("all four draft permissions issue independently", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		for _, p := range []Permission{PermissionDraftCreate, PermissionDraftRead, PermissionDraftEdit, PermissionDraftDelete} {
			r := NewRegistry()
			_, _, g, err := r.Issue("test", []Permission{p}, []SourceRef{src})
			require.NoError(err, "issuing %s", p)
			assert.Equal([]Permission{p}, g.Permissions, "grant carries exactly %s", p)
		}
	})

	t.Run("issue all four together", func(t *testing.T) {
		require := require.New(t)
		r := NewRegistry()
		all := AllPermissions()
		_, _, g, err := r.Issue("test", all, []SourceRef{src})
		require.NoError(err)
		assert.Equal(t, all, g.Permissions)
	})

	t.Run("empty permission rejected", func(t *testing.T) {
		r := NewRegistry()
		_, _, _, err := r.Issue("test", []Permission{""}, []SourceRef{src})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid")
	})

	t.Run("wildcard rejected", func(t *testing.T) {
		r := NewRegistry()
		_, _, _, err := r.Issue("test", []Permission{"*"}, []SourceRef{src})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid")
	})

	t.Run("draft.send rejected", func(t *testing.T) {
		r := NewRegistry()
		_, _, _, err := r.Issue("test", []Permission{"draft.send"}, []SourceRef{src})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown")
	})

	t.Run("unknown permission rejected", func(t *testing.T) {
		r := NewRegistry()
		_, _, _, err := r.Issue("test", []Permission{"archive.read"}, []SourceRef{src})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown")
	})

	t.Run("KnownPermission resolves all four", func(t *testing.T) {
		assert := assert.New(t)
		for _, p := range AllPermissions() {
			got, ok := KnownPermission(string(p))
			assert.True(ok, "KnownPermission(%q)", p)
			assert.Equal(p, got)
		}
		_, ok := KnownPermission("draft.send")
		assert.False(ok, "draft.send must not be known")
	})
}

func TestDraftPermissionIsolation(t *testing.T) {
	src := SourceRef{ID: 1, Type: "imap", Identifier: "alice@example.com"}
	allPerms := AllPermissions()

	t.Run("each permission allows only itself", func(t *testing.T) {
		assert := assert.New(t)
		for _, granted := range allPerms {
			g := Grant{
				ID:          "iso",
				Permissions: []Permission{granted},
				Sources:     []SourceRef{src},
			}
			for _, checked := range allPerms {
				if checked == granted {
					assert.True(g.Allows(checked, src), "grant(%s).Allows(%s) must be true", granted, checked)
				} else {
					assert.False(g.Allows(checked, src), "grant(%s).Allows(%s) must be false", granted, checked)
				}
			}
		}
	})

	t.Run("source type mismatch rejects all permissions", func(t *testing.T) {
		assert := assert.New(t)
		g := Grant{
			ID:          "type-mismatch",
			Permissions: allPerms,
			Sources:     []SourceRef{src},
		}
		wrong := SourceRef{ID: src.ID, Type: "gmail", Identifier: src.Identifier}
		for _, p := range allPerms {
			assert.False(g.Allows(p, wrong), "Allows(%s) with wrong Type must be false", p)
		}
	})

	t.Run("source identifier mismatch rejects all permissions", func(t *testing.T) {
		assert := assert.New(t)
		g := Grant{
			ID:          "id-mismatch",
			Permissions: allPerms,
			Sources:     []SourceRef{src},
		}
		wrong := SourceRef{ID: src.ID, Type: src.Type, Identifier: "bob@example.com"}
		for _, p := range allPerms {
			assert.False(g.Allows(p, wrong), "Allows(%s) with wrong Identifier must be false", p)
		}
	})

	t.Run("changed diagnostic ID still matches", func(t *testing.T) {
		assert := assert.New(t)
		g := Grant{
			ID:          "diag-id",
			Permissions: allPerms,
			Sources:     []SourceRef{src},
		}
		different := SourceRef{ID: 999, Type: src.Type, Identifier: src.Identifier}
		for _, p := range allPerms {
			assert.True(g.Allows(p, different), "Allows(%s) with different ID must be true", p)
		}
	})
}
