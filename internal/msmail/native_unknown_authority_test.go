package msmail

import (
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"testing"
)

func TestNativeMicrosoftUnknownGrantPausesEvents(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	enableNativeMailEvents(t, st)
	source, err := st.GetOrCreateSource(SourceType, "me@example.com")
	require.NoError(err)
	ref, err := st.GetMCPResource(t.Context(), "source", source.ID)
	require.NoError(err)
	require.NoError(st.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{SourceID: source.ID, SourceReference: ref.Reference, SourceType: SourceType, NativeIdentity: source.Identifier, Known: true, MessageRead: true}))
	provider := newFakeGraph(t)
	_, err = provider.sync(t, st)
	require.NoError(err)
	allowed, err := st.NativeSourceMessagePermission(t.Context(), source.ID)
	require.NoError(err)
	assert.False(allowed, "a current legacy client without grant metadata must pause persisted native authority")
	current, err := st.GetMCPResource(t.Context(), "source", source.ID)
	require.NoError(err)
	assert.Equal(ref.Reference, current.Reference, "unknown evidence must preserve the account lifetime")
	provider.put("synthetic-live-without-grant", "inbox")
	_, err = provider.sync(t, st)
	require.NoError(err, "ordinary archive sync remains usable")
	assert.Zero(nativeMailEventCount(t, st), "archive success must not manufacture native Events permission")
	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
	assert.Equal(1, count)
}
