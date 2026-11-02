package agentgrant

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVerificationReadIsOnlyASourceModifier(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	source := SourceRef{ID: 1, Type: "beeper", Identifier: "synthetic-source"}
	permission, known := KnownPermission("verification.read")
	require.True(known)
	assert.Equal(PermissionVerificationRead, permission)
	registry := NewRegistry()
	_, token, grant, err := registry.Issue("reveal modifier", []Permission{PermissionVerificationRead}, []SourceRef{source}, time.Time{})
	require.NoError(err)
	_, valid := registry.Lookup(token)
	require.True(valid)
	assert.True(grant.Allows(PermissionVerificationRead, source))
	assert.False(grant.Allows(PermissionVerificationRead, SourceRef{ID: 2, Type: "beeper", Identifier: "other-source"}))
	for _, base := range []Permission{PermissionMessageRead, PermissionSearchRead, PermissionAttachmentRead, PermissionStatsRead} {
		assert.False(grant.Allows(base, source), "modifier must not grant %s", base)
	}
}
