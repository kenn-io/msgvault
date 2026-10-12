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
		"inbox.read", "inbox.content.read", "inbox.tag", "inbox.archive", "inbox.read-state",
		"inbox.move", "inbox.folder-create",
	} {
		t.Run(string(permission), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				assertions := assert.New(t)
				requirements := require.New(t)
				source := SourceRef{ID: 1, Type: "gmail", Identifier: "reader@example.test"}
				registry := NewRegistry()
				expires := time.Now().Add(time.Hour)
				id, secret, grant, err := registry.Issue("inbox", []Permission{permission}, []SourceRef{source}, expires)
				requirements.NoError(err)
				assertions.Equal(expires, grant.ExpiresAt)
				assertions.True(grant.Allows(permission, source))
				assertions.False(grant.Allows(permission, SourceRef{ID: 2, Type: "gmail", Identifier: "other@example.test"}))
				assertions.False(grant.HasPermission(PermissionDraftCreate))
				assertions.False(grant.HasPermission(PermissionMessageRead))
				_, allowed := registry.Lookup(secret)
				requirements.True(allowed)
				requirements.True(registry.Revoke(id))
				_, allowed = registry.Lookup(secret)
				assertions.False(allowed, "revocation denies active Inbox authority")
				id, secret, _, err = registry.Issue("expiring inbox", []Permission{permission}, []SourceRef{source}, expires)
				requirements.NoError(err)
				time.Sleep(time.Hour)
				_, allowed = registry.Lookup(secret)
				assertions.False(allowed, "expiry denies Inbox authority at its boundary")
				requirements.True(registry.Revoke(id))
				assertions.Empty(registry.List())
			})
		})
	}
}
