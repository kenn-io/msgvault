package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "--owner-phone is required") {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestImportSynctechSMSCommandRuns(t *testing.T) {
	home := t.TempDir()
	input := filepath.Join(t.TempDir(), "sms.xml")
	if err := os.WriteFile(input, []byte(`<smses count="1"><sms address="+15551234567" date="1717214400000" type="1" body="hello" read="1" status="-1"/></smses>`), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	cfg = config.NewDefaultConfig()
	cfg.HomeDir = home
	cfg.Data.DataDir = home
	cmd := newTestRootCmd()
	cmd.AddCommand(newImportSynctechSMSCmd())
	cmd.SetArgs([]string{"import-synctech-sms", "--owner-phone", "+15550000001", input})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}
