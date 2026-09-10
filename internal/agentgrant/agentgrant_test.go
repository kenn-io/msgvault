package agentgrant

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGrantPermissionsDoNotImply covers proof matrix rows 12 and 13.
func TestGrantPermissionsDoNotImply(t *testing.T) {
	src := SourceRef{ID: 1, Type: "imap", Identifier: "alice@example.com"}

	t.Run("draft.create does not imply message.read", func(t *testing.T) {
		g := Grant{
			ID:          "id1",
			Permissions: []Permission{PermissionDraftCreate},
			Sources:     []SourceRef{src},
		}
		assert.True(t, g.Allows(PermissionDraftCreate, src))
		assert.False(t, g.Allows(PermissionMessageRead, src))
	})

	t.Run("message.read does not imply draft.create", func(t *testing.T) {
		g := Grant{
			ID:          "id2",
			Permissions: []Permission{PermissionMessageRead},
			Sources:     []SourceRef{src},
		}
		assert.True(t, g.Allows(PermissionMessageRead, src))
		assert.False(t, g.Allows(PermissionDraftCreate, src))
	})

	t.Run("Allows false when source ID differs", func(t *testing.T) {
		g := Grant{
			ID:          "id3",
			Permissions: []Permission{PermissionDraftCreate},
			Sources:     []SourceRef{src},
		}
		different := SourceRef{ID: 99, Type: src.Type, Identifier: src.Identifier}
		assert.False(t, g.Allows(PermissionDraftCreate, different))
	})

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
}

// TestRegistryLifecycle covers proof matrix rows 12 and 13.
func TestRegistryLifecycle(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	src := SourceRef{ID: 1, Type: "imap", Identifier: "alice@example.com"}
	perms := []Permission{PermissionDraftCreate}

	t.Run("new registry has zero grants", func(t *testing.T) {
		r := NewRegistry(clock)
		assert.Empty(t, r.List())
	})

	t.Run("Issue rejects empty label", func(t *testing.T) {
		r := NewRegistry(clock)
		_, _, _, err := r.Issue("", perms, []SourceRef{src}, DefaultLifetime)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "label")
	})

	t.Run("Issue rejects empty permissions", func(t *testing.T) {
		r := NewRegistry(clock)
		_, _, _, err := r.Issue("test", []Permission{}, []SourceRef{src}, DefaultLifetime)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "permission")
	})

	t.Run("Issue rejects empty sources", func(t *testing.T) {
		r := NewRegistry(clock)
		_, _, _, err := r.Issue("test", perms, []SourceRef{}, DefaultLifetime)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "source")
	})

	t.Run("Issue rejects nonpositive source ID", func(t *testing.T) {
		r := NewRegistry(clock)
		_, _, _, err := r.Issue("test", perms, []SourceRef{{ID: 0, Type: "imap", Identifier: "x"}}, DefaultLifetime)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "positive")
	})

	t.Run("Issue rejects negative source ID", func(t *testing.T) {
		r := NewRegistry(clock)
		_, _, _, err := r.Issue("test", perms, []SourceRef{{ID: -1, Type: "imap", Identifier: "x"}}, DefaultLifetime)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "positive")
	})

	t.Run("Issue rejects duplicate source IDs", func(t *testing.T) {
		r := NewRegistry(clock)
		sources := []SourceRef{src, {ID: src.ID, Type: "imap", Identifier: "bob@example.com"}}
		_, _, _, err := r.Issue("test", perms, sources, DefaultLifetime)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "duplicate")
	})

	t.Run("Issue rejects unknown permission", func(t *testing.T) {
		r := NewRegistry(clock)
		_, _, _, err := r.Issue("test", []Permission{"unknown.perm"}, []SourceRef{src}, DefaultLifetime)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown")
	})

	t.Run("Issue rejects wildcard permission", func(t *testing.T) {
		r := NewRegistry(clock)
		_, _, _, err := r.Issue("test", []Permission{"*"}, []SourceRef{src}, DefaultLifetime)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid")
	})

	t.Run("unspecified lifetime defaults to 24h", func(t *testing.T) {
		r := NewRegistry(clock)
		_, _, g, err := r.Issue("test", perms, []SourceRef{src}, 0)
		require.NoError(t, err)
		assert.Equal(t, now.Add(DefaultLifetime), g.ExpiresAt)
	})

	t.Run("secret has expected prefix", func(t *testing.T) {
		r := NewRegistry(clock)
		_, secret, _, err := r.Issue("test", perms, []SourceRef{src}, DefaultLifetime)
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(secret, secretPrefix), "secret should start with %s", secretPrefix)
	})

	t.Run("Lookup after ExpiresAt fails and removes entry", func(t *testing.T) {
		r := NewRegistry(clock)
		_, secret, g, err := r.Issue("test", perms, []SourceRef{src}, DefaultLifetime)
		require.NoError(t, err)

		// Advance clock past expiry
		now = g.ExpiresAt.Add(time.Second)

		found, ok := r.Lookup(secret)
		assert.False(t, ok)
		assert.Empty(t, found.ID)

		// Entry should have been cleaned up
		assert.Empty(t, r.List())
	})

	t.Run("revoked grant fails next Lookup", func(t *testing.T) {
		now = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		r := NewRegistry(clock)
		id, secret, _, err := r.Issue("test", perms, []SourceRef{src}, DefaultLifetime)
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
		r := NewRegistry(clock)
		revoked := r.Revoke("nonexistent-id")
		assert.False(t, revoked)
	})

	t.Run("Issue nonpositive lifetime defaults to 24h", func(t *testing.T) {
		now = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		r := NewRegistry(clock)
		_, _, g, err := r.Issue("test", perms, []SourceRef{src}, -time.Hour)
		require.NoError(t, err)
		assert.Equal(t, now.Add(DefaultLifetime), g.ExpiresAt)
	})

	t.Run("Lookup with wrong secret returns false", func(t *testing.T) {
		now = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		r := NewRegistry(clock)
		_, _, _, err := r.Issue("test", perms, []SourceRef{src}, DefaultLifetime)
		require.NoError(t, err)
		_, ok := r.Lookup("wrongsecret")
		assert.False(t, ok)
	})

	t.Run("Close empties the registry", func(t *testing.T) {
		now = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
		r := NewRegistry(clock)
		_, _, _, err := r.Issue("test", perms, []SourceRef{src}, DefaultLifetime)
		require.NoError(t, err)
		require.Len(t, r.List(), 1)
		r.Close()
		assert.Empty(t, r.List())
	})
}
