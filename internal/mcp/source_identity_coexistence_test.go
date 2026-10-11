package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query/querytest"
)

func TestNativeSourceOperationsPreserveFourteenReviewedTools(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	backend := newIdentityReviewMCPBackend(t)
	opts := ServeOptions{Engine: &querytest.MockEngine{}, IdentityReview: backend, IdentityScoring: backend, PersonCardDAV: backend, AllowIdentityDecisions: true, AllowIdentityScoring: true, AllowPersonMerges: true, Operations: operationalTestBackend{}, OperationCapabilities: []string{"sync_source", "set_person_display_name"}, OperationWriteFamilies: []OperationFamily{OperationFamilySources}}
	tools := toolsByName(t, rawListTools(t, opts, true))
	assertions.Contains(tools, "sync_source")
	assertions.NotContains(tools, ToolSyncCardDAV)
	assertions.NotContains(tools, ToolApproveCardDAVPublication)
	assertions.NotContains(tools, "set_person_display_name")
	opts.AllowCardDAVWrites = true
	listed := rawListTools(t, opts, true)
	tools = toolsByName(t, listed)
	for _, name := range []string{ToolListIdentityMatches, ToolGetIdentityMatch, ToolAcceptIdentityMatch, ToolRejectIdentityMatch, ToolGetIdentityScoringStatus, ToolScoreIdentityMatches, ToolListIdentityJudgments, ToolGetPersonMergeContext, ToolMergePerson, ToolGetCardDAVPublication, ToolPreviewCardDAVPublication, ToolApproveCardDAVPublication, ToolSyncCardDAV, ToolGetCardDAVSyncStatus} {
		requirements.Contains(tools, name)
	}
	assertions.Len(tools, len(listed), "combined catalog has no duplicate tool names")
	opts.OperationWriteFamilies = nil
	tools = toolsByName(t, rawListTools(t, opts, true))
	assertions.Contains(tools, ToolSyncCardDAV)
	assertions.NotContains(tools, "sync_source")
	opts.AllowIdentityDecisions = false
	opts.AllowIdentityScoring = false
	opts.AllowPersonMerges = false
	opts.AllowCardDAVWrites = false
	tools = toolsByName(t, rawListTools(t, opts, true))
	for _, name := range []string{ToolAcceptIdentityMatch, ToolRejectIdentityMatch, ToolScoreIdentityMatches, ToolMergePerson, ToolApproveCardDAVPublication, ToolSyncCardDAV} {
		assertions.NotContains(tools, name)
	}
}
