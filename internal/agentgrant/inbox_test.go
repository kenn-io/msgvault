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
func TestInboxPermissionGrantKeepsPermissionsIsolated(t *testing.T) {
	for _, permission := range []Permission{
		"inbox.read", "inbox.tag", "inbox.archive", "inbox.read-state",
		"inbox.move", "inbox.folder-create",
	} {
		t.Run(string(permission), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			source := SourceRef{ID: 1, Type: "gmail", Identifier: "reader@example.test"}
			registry := NewRegistry()
			expires := time.Now().Add(time.Hour)
			_, secret, grant, err := registry.Issue("inbox", []Permission{permission}, []SourceRef{source}, expires)
			requirements.NoError(err)
			assertions.True(grant.Allows(permission, source))
			assertions.False(grant.Allows(permission, SourceRef{ID: 2, Type: "gmail", Identifier: "other@example.test"}))
			for _, other := range []Permission{
				"inbox.read", "inbox.tag", "inbox.archive", "inbox.read-state",
				"inbox.move", "inbox.folder-create",
			} {
				if other != permission {
					assertions.False(grant.Allows(other, source), "permission %s must not imply %s", permission, other)
				}
			}
			assertions.False(grant.HasPermission(PermissionDraftCreate))
			assertions.False(grant.HasPermission(PermissionMessageRead))
			_, allowed := registry.Lookup(secret)
			requirements.True(allowed)
		})
	}
}

func TestInboxPermissionGrantExpiresAtBoundary(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		source := SourceRef{ID: 1, Type: "gmail", Identifier: "reader@example.test"}
		registry := NewRegistry()
		expires := time.Now().Add(time.Hour)
		_, secret, grant, err := registry.Issue("inbox", []Permission{"inbox.read"}, []SourceRef{source}, expires)
		require.NoError(t, err)
		assert.Equal(t, expires, grant.ExpiresAt)

		synctest.Sleep(time.Hour)
		_, allowed := registry.Lookup(secret)
		assert.False(t, allowed, "expiry denies Inbox authority at its boundary")
	})
}
