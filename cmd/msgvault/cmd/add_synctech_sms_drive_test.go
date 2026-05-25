package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.kenn.io/msgvault/internal/config"
)

func TestAddSynctechSMSDriveWritesConfigWithoutSecrets(t *testing.T) {
	home := t.TempDir()
	cfg = config.NewDefaultConfig()
	cfg.HomeDir = home
	cfg.Data.DataDir = home
	cmd := newTestRootCmd()
	cmd.AddCommand(newAddSynctechSMSDriveCmd())
	cmd.SetArgs([]string{
		"add-synctech-sms-drive", "pixel",
		"--owner-phone", "+15550000001",
		"--folder-id", "drive-folder-id",
		"--google-account", "user@example.com",
		"--schedule", "30 4 * * *",
		"--oauth-app", "personal",
		"--skip-auth-for-test",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	text := string(data)
	for _, want := range []string{`[[synctech_sms.sources]]`, `name = "pixel"`, `backend = "drive"`, `folder_id = "drive-folder-id"`, `google_account = "user@example.com"`, `owner_phone = "+15550000001"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("config missing %q:\n%s", want, text)
		}
	}
	lower := strings.ToLower(text)
	refreshTokenKey := "refresh" + "_token"
	clientSecretKey := "client" + "_secret\""
	if strings.Contains(lower, refreshTokenKey) || strings.Contains(lower, clientSecretKey) {
		t.Fatalf("config contains secret material:\n%s", text)
	}
}
