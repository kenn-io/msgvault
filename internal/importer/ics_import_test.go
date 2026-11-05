package importer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImportICSInvalidLeavesNoSource(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st, dir := openTestStore(t)
	path := filepath.Join(dir, "invalid.ics")
	requirements.NoError(os.WriteFile(path, []byte("invalid calendar"), 0o600))
	_, err := ImportICS(t.Context(), st, path, "invalid-calendar")
	requirements.Error(err)
	sources, err := st.ListSources("ics")
	requirements.NoError(err)
	assertions.Empty(sources)
}
