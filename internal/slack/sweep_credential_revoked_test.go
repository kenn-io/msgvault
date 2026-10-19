package slack

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSweepPropagatesCredentialRevocation(t *testing.T) {
	require := require.New(t)
	native := nativeSlackInboxWorkspace(t)
	imp, opts := testImporter(t, native)
	enableNativeSlackInboxEvents(t, imp.store)

	summary, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	allowed, err := imp.store.NativeSourceMessagePermission(t.Context(), summary.SourceID)
	require.NoError(err)
	require.True(allowed, "the initial authenticated sync establishes Slack message authority")

	native.credentialSearch = true
	_, err = imp.Import(t.Context(), opts)
	require.ErrorIs(err, ErrCredentialRevoked, "a sweep search revocation must reach the import error handler")

	allowed, err = imp.store.NativeSourceMessagePermission(t.Context(), summary.SourceID)
	require.NoError(err)
	require.False(allowed, "credential revocation must retire the pinned source authority")
}
