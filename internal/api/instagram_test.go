package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/agentgrant"
)

func TestInstagramCommentsSchedulerAndCLIBoundaries(t *testing.T) {
	assert := assert.New(t)
	identifier := "app:9:page:100:instagram:200"
	name, ok := SchedulerJobNameForSource("instagram_comments", identifier)
	assert.True(ok)
	assert.Equal("instagram_comments:"+identifier, name)
	assert.Equal(sourceScheduleGeneric, classifySourceScheduling("instagram_comments", identifier).kind)
	assert.Equal("sync", embeddingSlotKind(name, true))
	for _, command := range []string{"add", "sync"} {
		assert.True(cliRunCommandAllowed([]string{"source", "instagram-comments", command, "example-account"}))
		assert.False(delegatedCLIRunAdmitted([]string{"source", "instagram-comments", command, "example-account"}, &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}}))
	}
	for _, invalid := range []string{"example-account", "app:9:page:100", "app:9:page:100:instagram:200/other"} {
		_, ok := SchedulerJobNameForSource("instagram_comments", invalid)
		assert.False(ok)
		assert.Equal(sourceScheduleNonSchedulable, classifySourceScheduling("instagram_comments", invalid).kind)
	}
}
