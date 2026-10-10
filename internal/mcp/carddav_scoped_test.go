package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query/querytest"
)

func TestScopedCardDAVToolAdmission(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	backend := newIdentityReviewMCPBackend(t)
	opts := ServeOptions{Engine: &querytest.MockEngine{}, ScopedCardDAVPreview: backend, ScopedCardDAVApprove: backend, ScopedCardDAVReconcile: backend}
	reads := toolsByName(t, rawListTools(t, opts, false))
	requirements.Contains(reads, "preview_scoped_carddav_publication")
	assertions.NotContains(reads, "approve_scoped_carddav_publication")
	assertions.NotContains(reads, "reconcile_scoped_carddav_publication")
	general := toolsByName(t, rawListTools(t, opts, true))
	assertions.NotContains(general, "approve_scoped_carddav_publication")
	assertions.NotContains(general, "reconcile_scoped_carddav_publication")
	opts.AllowCardDAVWrites = true
	writes := toolsByName(t, rawListTools(t, opts, true))
	for _, name := range []string{"approve_scoped_carddav_publication", "reconcile_scoped_carddav_publication"} {
		requirements.Contains(writes, name)
		assertions.Equal(false, toolReadOnlyHint(t, writes[name]))
	}
	for _, name := range []string{ToolApproveCardDAVPublication, ToolSyncCardDAV, ToolMergePerson} {
		assertions.NotContains(writes, name)
	}
	opts.ScopedCardDAVApprove = nil
	narrow := toolsByName(t, rawListTools(t, opts, true))
	assertions.NotContains(narrow, "approve_scoped_carddav_publication")
	assertions.Contains(narrow, "reconcile_scoped_carddav_publication")
	opts.ScopedCardDAVPreview = nil
	narrow = toolsByName(t, rawListTools(t, opts, true))
	assertions.NotContains(narrow, "preview_scoped_carddav_publication")
	assertions.Contains(narrow, "reconcile_scoped_carddav_publication")
	// These caller-scoped routes remain available in a restricted CLI session.
	opts.CalendarOnly = true
	restricted := toolsByName(t, rawListTools(t, opts, true))
	assertions.Contains(restricted, "reconcile_scoped_carddav_publication")
}
