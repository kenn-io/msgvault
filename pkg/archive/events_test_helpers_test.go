package archive_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func grantArchiveTestMessageRead(t *testing.T, st *store.Store, source *store.Source) {
	t.Helper()
	resource, err := st.GetMCPResource(t.Context(), "source", source.ID)
	require.NoError(t, err)
	require.NoError(t, st.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{
		SourceID: source.ID, SourceReference: resource.Reference,
		SourceType: source.SourceType, NativeIdentity: source.Identifier,
		Known: true, MessageRead: true,
	}))
}
