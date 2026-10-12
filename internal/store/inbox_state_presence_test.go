package store_test

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func TestInboxStatePresenceSurvivesProviderAndReceiptStorage(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	for _, values := range [][]string{nil, {}, {"Todo", "Next"}} {
		f, receipt := inboxReceiptFixture(t)
		receipt.Before.Tags, receipt.Before.Flags = values, values
		var err error
		receipt.StateHash, err = inboxcontrol.SemanticFingerprint(receipt.Before)
		require.NoError(err)
		_, err = f.Store.ObserveInboxState(t.Context(), receipt.Before)
		require.NoError(err)
		state, err := f.Store.GetInboxProviderState(t.Context(), receipt.Before.Target)
		require.NoError(err)
		require.NotNil(state)
		assert.Equal(values, state.Tags)
		assert.Equal(values, state.Flags)
		_, claimed, err := f.Store.PrepareInboxReceipt(t.Context(), receipt)
		require.NoError(err)
		require.True(claimed)
		stored, err := f.Store.GetInboxReceipt(t.Context(), receipt.ID)
		require.NoError(err)
		require.NotNil(stored)
		assert.Equal(values, stored.Before.Tags)
		assert.Equal(values, stored.Before.Flags)
	}
}
