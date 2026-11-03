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

func TestMetaWebhookCommandsAllowedForOwnerOnly(t *testing.T) {
	for _, action := range []string{"subscribe", "status", "replay"} {
		command := []string{"meta-webhooks", action}
		assert.True(t, cliRunCommandAllowed(command))
		assert.False(t, delegatedCLIRunAdmitted(command, &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}}))
	}
}

func TestCLIRunAllowsMessagesCommands(t *testing.T) {
	assert := assert.New(t)

	for _, provider := range []string{"facebook-messages", "instagram-messages"} {
		for _, operation := range []string{"add", "sync"} {
			assert.True(cliRunCommandAllowed([]string{"source", provider, operation, "example"}))
		}
		assert.False(cliRunCommandAllowed([]string{"source", provider}))
		assert.False(cliRunCommandAllowed([]string{"source", provider, "delete"}))
	}
}
func TestDelegatedGrantCannotRunMessagesSync(t *testing.T) {
	assert := assert.New(t)

	for _, provider := range []string{"facebook-messages", "instagram-messages"} {
		for _, operation := range []string{"add", "sync"} {
			assert.False(delegatedCLIRunAdmitted([]string{"source", provider, operation, "example"}, &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}}))
		}
	}
}
func TestSchedulerJobNameForMessagesSources(t *testing.T) {
	assert := assert.New(t)

	for _, tc := range []struct{ kind, id string }{{"facebook_messages", "app:9:page:100"}, {"instagram_messages", "app:9:page:100:instagram:1784"}} {
		name, ok := SchedulerJobNameForSource(tc.kind, tc.id)
		assert.True(ok)
		assert.Equal(tc.kind+":"+tc.id, name)
		assert.True(isSourceSyncJobName(name))
		classification := classifySourceScheduling(tc.kind, tc.id)
		assert.Equal(sourceScheduleGeneric, classification.kind)
		_, ok = SchedulerJobNameForSource(tc.kind, "example")
		assert.False(ok)
	}
}
