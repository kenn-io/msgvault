package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query/querytest"
)

func TestNativeCardDAVOperationalToolsPreserveFourteenReviewedTools(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	backend := newIdentityReviewMCPBackend(t)
	opts := ServeOptions{Engine: &querytest.MockEngine{}, IdentityReview: backend, IdentityScoring: backend, PersonCardDAV: backend, AllowIdentityDecisions: true, AllowIdentityScoring: true, AllowPersonMerges: true, Operations: operationalTestBackend{}, OperationCapabilities: []string{"list_carddav_books", "sync_carddav_connections", "unpublish_carddav_person"}, OperationWriteFamilies: []OperationFamily{OperationFamilyCardDAV}}
	tools := toolsByName(t, rawListTools(t, opts, true))
	assertions.Contains(tools, "sync_carddav_connections")
	assertions.NotContains(tools, ToolSyncCardDAV)
	assertions.NotContains(tools, ToolApproveCardDAVPublication)
	opts.AllowCardDAVWrites = true
	listed := rawListTools(t, opts, true)
	tools = toolsByName(t, listed)
	for _, name := range []string{ToolListIdentityMatches, ToolGetIdentityMatch, ToolAcceptIdentityMatch, ToolRejectIdentityMatch, ToolGetIdentityScoringStatus, ToolScoreIdentityMatches, ToolListIdentityJudgments, ToolGetPersonMergeContext, ToolMergePerson, ToolGetCardDAVPublication, ToolPreviewCardDAVPublication, ToolApproveCardDAVPublication, ToolSyncCardDAV, ToolGetCardDAVSyncStatus} {
		assertions.Contains(tools, name)
	}
	assertions.Len(tools, len(listed), "the combined catalog must have no duplicate tool names")
	opts.OperationWriteFamilies = nil
	tools = toolsByName(t, rawListTools(t, opts, true))
	assertions.Contains(tools, ToolSyncCardDAV)
	assertions.NotContains(tools, "sync_carddav_connections")
	assertions.NotContains(tools, "unpublish_carddav_person")
	approval, ok := tools[ToolApproveCardDAVPublication]["inputSchema"].(map[string]any)
	requirements.True(ok)
	assertions.Contains(approval["required"], "approval_token")
}
