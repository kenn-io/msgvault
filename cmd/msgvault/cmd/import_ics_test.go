package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestImportICSCommandImportsFile(t *testing.T) {
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
	assertions.Equal("Events: 1 updated, 0 not updated\n", out.String())
	raw = bytes.Replace(raw, []byte("Example command event"), []byte("Changed title"), 1)
	requirements.NoError(os.WriteFile(path, raw, 0600))
	out.Reset()
	cmd = newImportICSCommand()
	cmd.SetContext(ctx)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{path, "--identifier", "example-export"})
	requirements.NoError(cmd.Execute())
	assertions.Equal("Events: 0 updated, 1 not updated\n", out.String())
	st, err := store.Open(cfg.DatabaseDSN())
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(st.Close()) })
	_, err = st.GetSourceByTypeAndIdentifier(t.Context(), "ics", "example-export")
	requirements.NoError(err)
}
