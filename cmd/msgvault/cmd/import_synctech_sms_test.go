package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestImportSynctechSMSRequiresOwnerPhone(t *testing.T) {
	dir := t.TempDir()
	cfg = config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	cmd := newTestRootCmd()
	cmd.AddCommand(newImportSynctechSMSCmd())
	cmd.SetArgs([]string{"import-synctech-sms", dir})
	err := cmd.Execute()
	require.Error(t, err)
	require.ErrorContains(t, err, "--owner-phone is required")
}

func TestImportSynctechSMSCommandRuns(t *testing.T) {
	home := t.TempDir()
	input := filepath.Join(t.TempDir(), "sms.xml")
	err := os.WriteFile(input, []byte(`<smses count="1"><sms address="+15551234567" date="1717214400000" type="1" body="hello" read="1" status="-1"/></smses>`), 0o600)
	require.NoError(t, err, "write fixture")
	cfg = config.NewDefaultConfig()
	cfg.HomeDir = home
	cfg.Data.DataDir = home
	cmd := newTestRootCmd()
	cmd.AddCommand(newImportSynctechSMSCmd())
	cmd.SetArgs([]string{"import-synctech-sms", "--owner-phone", "+15550000001", input})
	require.NoError(t, cmd.Execute(), "Execute")
}
