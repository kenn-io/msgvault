package importer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportICSMalformedFileLeavesNoSource(t *testing.T) {
	st := testutil.NewTestStore(t)
	path := filepath.Join(t.TempDir(), "invalid.ics")
	require.NoError(t, os.WriteFile(path, []byte("invalid calendar"), 0600))
	_, err := ImportICS(t.Context(), st, path, "invalid-export")
	require.Error(t, err)
	_, err = st.GetSourceByTypeAndIdentifier(t.Context(), "ics", "invalid-export")
	assert.ErrorIs(t, err, store.ErrSourceNotFound)
}
