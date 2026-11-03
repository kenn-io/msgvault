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
