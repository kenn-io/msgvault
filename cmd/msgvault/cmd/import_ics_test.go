package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/importer"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportICSCommandImportsFile(t *testing.T) {
	t.Run("malformed file leaves no source", func(t *testing.T) {
		st := testutil.NewTestStore(t)
		path := filepath.Join(t.TempDir(), "invalid.ics")
		require.NoError(t, os.WriteFile(path, []byte("invalid calendar"), 0600))
		_, err := importer.ImportICS(t.Context(), st, path, "invalid-export")
		require.Error(t, err)
		_, err = st.GetSourceByTypeAndIdentifier(t.Context(), "ics", "invalid-export")
		assert.ErrorIs(t, err, store.ErrSourceNotFound)
	})
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	cfg := lifecycleTestConfig(t.TempDir())
	ctx := withStoreResolverConfig(t, cfg)
	path := filepath.Join(t.TempDir(), "example.ics")
	raw := []byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:command@example.com\r\nDTSTART:20270302T120000Z\r\nSUMMARY:Example command event\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	requirements.NoError(os.WriteFile(path, raw, 0600))
	cmd := newImportICSCommand()
	var out bytes.Buffer
	cmd.SetContext(ctx)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{path, "--identifier", "example-export"})
	requirements.NoError(cmd.Execute())
	assertions.Equal("Events: 1 updated, 0 unchanged\n", out.String())
}
