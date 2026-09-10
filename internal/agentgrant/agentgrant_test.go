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

	t.Run("Issue rejects duplicate source IDs", func(t *testing.T) {
		r := NewRegistry()
		sources := []SourceRef{src, {ID: src.ID, Type: "imap", Identifier: "bob@example.com"}}
		_, _, _, err := r.Issue("test", perms, sources)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "duplicate")
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
		r := NewRegistry()
		id, secret, _, err := r.Issue("test", perms, []SourceRef{src})
		require.NoError(t, err)

		// Confirm it works before revocation
		_, ok := r.Lookup(secret)
		assert.True(t, ok)

		revoked := r.Revoke(id)
		assert.True(t, revoked)

		_, ok = r.Lookup(secret)
		assert.False(t, ok)
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
