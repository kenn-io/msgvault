package store_test

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"testing"
)

// These Store unit fixtures isolate lifetimes and fanout with explicit positive
// native authority. Native producer/SDK and denial tests prove actual grants.
func grantSyntheticSourceFixture(t *testing.T, f *storetest.Fixture) {
	t.Helper()
	grantSyntheticSourceAuthority(t, f.Store, f.Source.ID)
}
func grantSyntheticSourceAuthority(t *testing.T, st *store.Store, sourceID int64) {
	t.Helper()
	resource, err := st.GetMCPResource(t.Context(), "source", sourceID)
	require.NoError(t, err)
	require.NoError(t, st.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{SourceID: sourceID, SourceReference: resource.Reference, SourceType: resource.SourceType, NativeIdentity: resource.ProviderID, Known: true, MessageRead: true}))
}
