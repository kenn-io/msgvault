package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
)

func TestShowVirtualChildren(t *testing.T) {
	inbox := store.VirtualAccount{Key: "identity:7:inbox", SourceID: 7, AccountAddress: "inbox@example.net", MessageCount: 3}
	work := store.VirtualAccount{Key: "identity:7:work", SourceID: 7, AccountAddress: "work@example.org", MessageCount: 1}
	empty := store.VirtualAccount{Key: "unattributed:7", SourceID: 7, Unattributed: true}
	for name, tc := range map[string]struct {
		identifier string
		children   []store.VirtualAccount
		want       bool
	}{
		"inbox only":                   {"inbox@example.net", []store.VirtualAccount{inbox, empty}, false},
		"alias divides the source":     {"inbox@example.net", []store.VirtualAccount{inbox, work}, true},
		"unattributed mail":            {"inbox@example.net", []store.VirtualAccount{inbox, {Key: "unattributed:7", Unattributed: true, MessageCount: 2}}, true},
		"pending repair":               {"inbox@example.net", []store.VirtualAccount{inbox, {Key: "unattributed:7", Unattributed: true, PendingCount: 1}}, true},
		"imap identifier is the inbox": {"imaps://inbox%40example.net@imap.example.net:993", []store.VirtualAccount{inbox}, false},
	} {
		assert.Equal(t, tc.want, showVirtualChildren(query.AccountInfo{Identifier: tc.identifier, VirtualAccounts: tc.children}), name)
	}
}

func TestVirtualSourceScopeAppliesAccountScope(t *testing.T) {
	assert := assert.New(t)
	source := int64(7)
	var filter query.MessageFilter
	virtualSourceScope(store.VirtualAccount{Key: "unattributed:7", SourceID: 7, Unattributed: true, PendingCount: 2}).apply(&filter)
	assert.Equal(&source, filter.SourceID)
	assert.Equal([]search.AccountScope{{SourceID: &source, Unattributed: true}}, filter.AccountScopes)

	accountSourceScope(&source).apply(&filter)
	assert.Nil(filter.AccountScopes, "switching back to the whole source clears the account scope")

	scope := virtualSourceScope(store.VirtualAccount{Key: "unattributed:7", SourceID: 7, Unattributed: true, PendingCount: 2})
	assert.Equal("Unattributed (2 awaiting repair)", scope.title(nil))
	assert.True(scope.matches(scopeOption{kind: scopeOptionVirtual, virtualAccount: &store.VirtualAccount{Key: "unattributed:7"}}))
}
