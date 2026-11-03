package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/agentgrant"
)

func TestFacebookCommentsSchedulerAndCLIBoundaries(t *testing.T) {
	assert := assert.New(t)
	name, ok := SchedulerJobNameForSource("facebook_comments", "app:9:page:100")
	assert.True(ok)
	assert.Equal("facebook_comments:app:9:page:100", name)
	classification := classifySourceScheduling("facebook_comments", "app:9:page:100")
	assert.Equal(sourceScheduleGeneric, classification.kind)
	for _, command := range []string{"add", "sync"} {
		assert.True(cliRunCommandAllowed([]string{"source", "facebook-comments", command, "example-page"}))
		assert.False(delegatedCLIRunAdmitted([]string{"source", "facebook-comments", command, "example-page"}, &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}}))
	}
	_, ok = SchedulerJobNameForSource("facebook_comments", "example-page")
	assert.False(ok, "unscoped display labels cannot select a Page scheduler job")
}

func TestInstagramMentionsSchedulerAndCLIBoundaries(t *testing.T) {
	assert := assert.New(t)
	name, ok := SchedulerJobNameForSource("instagram_mentions", "app:9:page:100:instagram:200")
	assert.True(ok)
	assert.Equal("instagram_mentions:app:9:page:100:instagram:200", name)
	assert.True(isSourceSyncJobName(name))
	assert.Equal(sourceScheduleGeneric, classifySourceScheduling("instagram_mentions", "app:9:page:100:instagram:200").kind)
	_, ok = SchedulerJobNameForSource("instagram_mentions", "example")
	assert.False(ok)
	for _, command := range []string{"add", "sync"} {
		assert.True(cliRunCommandAllowed([]string{"source", "instagram-mentions", command, "example"}))
		assert.False(delegatedCLIRunAdmitted([]string{"source", "instagram-mentions", command, "example"}, &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}}))
	}
}

func TestInstagramMentionsCLIRejectsUnknownGroupedAction(t *testing.T) {
	assert.False(t, cliRunCommandAllowed([]string{"source", "instagram-mentions"}))
	assert.False(t, cliRunCommandAllowed([]string{"source", "instagram-mentions", "remove"}))
	assert.False(t, cliRunCommandAllowed([]string{"source", "instagram-mentions-extra", "sync"}))
}
