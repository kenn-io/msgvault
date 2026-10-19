package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// Catalog tests need only the backend's presence. Actual read calls are
// exercised through the daemon, Store and MCP SDK in the command package.
type inboxReadCatalogBackend struct{}

func (inboxReadCatalogBackend) InboxCandidates(context.Context, inboxcontrol.SourceIdentity, inboxcontrol.Scope, int, string) (*inboxcontrol.CandidatePage, error) {
	return nil, inboxcontrol.ErrUnavailable
}
func (inboxReadCatalogBackend) InboxContext(context.Context, inboxcontrol.ContextRequest) (*inboxcontrol.Context, error) {
	return nil, inboxcontrol.ErrUnavailable
}

func TestInboxReadCatalogRequiresBothContentPermissions(t *testing.T) {
	assertions := assert.New(t)
	for _, tc := range []struct {
		permissions         []string
		candidates, content bool
	}{
		{nil, false, false},
		{[]string{"inbox.read"}, true, false},
		{[]string{"inbox.content.read"}, false, false},
		{[]string{"inbox.read", "inbox.content.read"}, true, true},
	} {
		opts := ServeOptions{DelegatedOnly: true, GrantPermissions: tc.permissions, InboxCandidates: inboxReadCatalogBackend{}, InboxContext: inboxReadCatalogBackend{}}
		names := map[string]bool{}
		for _, definition := range operationCatalog(opts, nil) {
			names[definition.name] = true
		}
		assertions.Equal(tc.candidates, names["inbox_candidates"])
		assertions.Equal(tc.content, names["inbox_context"])
	}
	for _, definition := range operationCatalog(ServeOptions{}, nil) {
		assertions.NotEqual("inbox_candidates", definition.name)
		assertions.NotEqual("inbox_context", definition.name)
	}
}
