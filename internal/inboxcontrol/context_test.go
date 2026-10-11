package inboxcontrol

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestContextRequestExactSelectorAndBounds(t *testing.T) {
	target := Target{SourceID: 1, SourceType: "gmail", SourceIdentifier: "reader@example.test", AccountID: "reader@example.test", Scope: ScopeMessage, ItemID: 2, ProviderID: "message-2"}
	for _, limit := range []int{0, 1, 16384, 65536} {
		assert.NoError(t, (ContextRequest{Target: target, MaxBytes: limit}).Validate())
	}
	for _, limit := range []int{-1, 65537} {
		assert.ErrorIs(t, (ContextRequest{Target: target, MaxBytes: limit}).Validate(), ErrInvalid)
	}
	assert.ErrorIs(t, (ContextRequest{Target: target, MessageID: 2}).Validate(), ErrInvalid)
	target.SourceType, target.Scope = "beeper", ScopeChat
	assert.ErrorIs(t, (ContextRequest{Target: target}).Validate(), ErrInvalid)
	assert.NoError(t, (ContextRequest{Target: target, MessageID: 3}).Validate())
}
