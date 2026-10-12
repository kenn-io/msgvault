package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func TestInboxStatePresenceSurvivesProviderAndReceiptStorage(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	for _, values := range [][]string{nil, {}, {"Todo", "Next"}} {
		f, receipt := inboxReceiptFixture(t)
		receipt.Before.Tags, receipt.Before.Flags = values, values
		var err error
		receipt.StateHash, err = inboxcontrol.SemanticFingerprint(receipt.Before)
		requirements.NoError(err)
		_, err = f.Store.ObserveInboxState(t.Context(), receipt.Before)
		requirements.NoError(err)
		state, err := f.Store.GetInboxProviderState(t.Context(), receipt.Before.Target)
		requirements.NoError(err)
		requirements.NotNil(state)
		assertions.Equal(values, state.Tags)
		assertions.Equal(values, state.Flags)
		_, claimed, err := f.Store.PrepareInboxReceipt(t.Context(), receipt)
		requirements.NoError(err)
		requirements.True(claimed)
		stored, err := f.Store.GetInboxReceipt(t.Context(), receipt.ID)
		requirements.NoError(err)
		requirements.NotNil(stored)
		assertions.Equal(values, stored.Before.Tags)
		assertions.Equal(values, stored.Before.Flags)
	}
}
