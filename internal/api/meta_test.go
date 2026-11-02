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
	for _, command := range []string{"add-facebook-comments", "sync-facebook-comments"} {
		assert.True(cliRunCommandAllowed([]string{command, "example-page"}))
		assert.False(delegatedCLIRunAdmitted([]string{command, "example-page"}, &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}}))
	}
	_, ok = SchedulerJobNameForSource("facebook_comments", "example-page")
	assert.False(ok, "unscoped display labels cannot select a Page scheduler job")
}

func TestMetaWebhookCommandsAllowedForOwnerOnly(t *testing.T) {
	for _, command := range []string{"subscribe-meta-webhooks", "meta-webhooks-status", "replay-meta-webhooks"} {
		assert.True(t, cliRunCommandAllowed([]string{command}))
		assert.False(t, delegatedCLIRunAdmitted([]string{command}, &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}}))
	}
}
