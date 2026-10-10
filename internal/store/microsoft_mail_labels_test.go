package store_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMicrosoftMailFolderLookupPreservesDatabaseError(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("msmail", "owner@example.test")
	require.NoError(t, err)
	// Removing the fixture table makes the real lookup fail before any rename
	// or catalog update; its error must retain the operation that failed.
	ddl := "DROP TABLE labels"
	if store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		ddl += " CASCADE"
	}
	_, err = st.DB().Exec(ddl)
	require.NoError(t, err)
	_, err = st.EnsureMicrosoftMailFoldersContext(t.Context(), source.ID, map[string]store.LabelInfo{"folder-1": {Name: "Todo"}})
	require.ErrorContains(t, err, "check Microsoft folder name")
}
