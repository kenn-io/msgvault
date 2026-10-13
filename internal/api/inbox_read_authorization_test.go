package api

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/agentgrant"
)

func TestInboxReadGrantRequiresExactSourceLifetime(t *testing.T) {
	assertions := assert.New(t)

	source := agentgrant.SourceRef{ID: 1, Type: "gmail", Identifier: "reader@example.test"}
	grant := &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionInboxRead, agentgrant.PermissionInboxContentRead}, Sources: []agentgrant.SourceRef{source}}
	assertions.True(inboxReadAllowed(grant, agentgrant.PermissionInboxContentRead, source))
	recreated := source
	recreated.ID = 2
	assertions.False(inboxReadAllowed(grant, agentgrant.PermissionInboxContentRead, recreated))
	foreign := source
	foreign.Identifier = "foreign@example.test"
	assertions.False(inboxReadAllowed(grant, agentgrant.PermissionInboxContentRead, foreign))
	grant.Permissions = []agentgrant.Permission{agentgrant.PermissionInboxContentRead}
	assertions.False(inboxReadAllowed(grant, agentgrant.PermissionInboxContentRead, source))
	grant.Permissions = []agentgrant.Permission{agentgrant.PermissionInboxRead, agentgrant.PermissionInboxContentRead}
	grant.ExpiresAt = time.Now().Add(-time.Second)
	assertions.False(inboxReadAllowed(grant, agentgrant.PermissionInboxContentRead, source))
	assertions.False(inboxReadAllowed(nil, agentgrant.PermissionInboxContentRead, source))
}
