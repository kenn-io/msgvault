package agentgrant

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Inbox actions must be issuable independently while retaining the registry's
// source scope, expiry and revocation behavior.
func TestInboxPermissionGrantLifecycle(t *testing.T) {
	for _, permission := range []Permission{
		"inbox.read", "inbox.tag", "inbox.archive", "inbox.read-state",
		"inbox.move", "inbox.folder-create",
	} {
		t.Run(string(permission), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				source := SourceRef{ID: 1, Type: "gmail", Identifier: "reader@example.test"}
				registry := NewRegistry()
				expires := time.Now().Add(time.Hour)
				_, secret, grant, err := registry.Issue("inbox", []Permission{permission}, []SourceRef{source}, expires)
				require.NoError(t, err)
				assert.Equal(t, expires, grant.ExpiresAt)
				assert.True(t, grant.Allows(permission, source))
				assert.False(t, grant.Allows(permission, SourceRef{ID: 2, Type: "gmail", Identifier: "other@example.test"}))
				assert.False(t, grant.HasPermission(PermissionDraftCreate))
				assert.False(t, grant.HasPermission(PermissionMessageRead))
				_, allowed := registry.Lookup(secret)
				require.True(t, allowed)
			})
		})
	}
}

func TestInboxPermissionGrantExpiresAtBoundary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source := SourceRef{ID: 1, Type: "gmail", Identifier: "reader@example.test"}
		registry := NewRegistry()
		expires := time.Now().Add(time.Hour)
		_, secret, grant, err := registry.Issue("expiring inbox", []Permission{"inbox.read"}, []SourceRef{source}, expires)
		require.NoError(t, err)
		assert.True(t, grant.Allows("inbox.read", source))
		time.Sleep(time.Hour)
		_, allowed := registry.Lookup(secret)
		assert.False(t, allowed, "expiry denies Inbox authority at its boundary")
	})
}
