package fbmessenger

import (
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
	"testing"
)

func TestImportMessengerCompletedFreshRunDoesNotReviveAbandonedAttempt(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	opts := syntheticMessengerImport(t)
	messengerSnapshotAbort(t, st, "message_bodies")
	failed, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	require.True(failed.HardErrors)
	dropMessengerAbort(t, st, "message_bodies")
	opts.NoResume = true
	fresh, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	require.False(fresh.HardErrors)
	require.NotEqual(failed.ImportRunID, fresh.ImportRunID)
	opts.NoResume = false
	next, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	require.False(next.HardErrors)
	assert.False(next.WasResumed, "a completed fresh invocation supersedes older failed attempts")
	assert.NotEqual(failed.ImportRunID, next.ImportRunID)
	assert.NotEqual(fresh.ImportRunID, next.ImportRunID)
}
