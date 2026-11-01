package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/agentgrant"
)

func TestFacebookCommentsSchedulerAndCLIBoundaries(t *testing.T) {
	name, ok := SchedulerJobNameForSource("facebook_comments", "app:9:page:100")
	assert.True(t, ok)
	assert.Equal(t, "facebook_comments:app:9:page:100", name)
	classification := classifySourceScheduling("facebook_comments", "app:9:page:100")
	assert.Equal(t, sourceScheduleGeneric, classification.kind)
	for _, command := range []string{"add-facebook-comments", "sync-facebook-comments"} {
		assert.True(t, cliRunCommandAllowed([]string{command, "example-page"}))
		assert.False(t, delegatedCLIRunAdmitted([]string{command, "example-page"}, &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}}))
	}
	_, ok = SchedulerJobNameForSource("facebook_comments", "example-page")
	assert.False(t, ok, "unscoped display labels cannot select a Page scheduler job")
}
