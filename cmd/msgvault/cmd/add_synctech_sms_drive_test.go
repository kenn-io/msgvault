package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestAddSynctechSMSDriveWritesConfigWithoutSecrets(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	require.NoError(cmd.Execute(), "Execute")
	data, err := os.ReadFile(filepath.Join(home, "config.toml"))
	require.NoError(err, "read config")
	text := string(data)
	for _, want := range []string{`[[synctech_sms.sources]]`, `name = "pixel"`, `backend = "drive"`, `folder_id = "drive-folder-id"`, `google_account = "user@example.com"`, `owner_phone = "+15550000001"`} {
		require.Contains(text, want, "config missing %q", want)
	}
	lower := strings.ToLower(text)
	refreshTokenKey := "refresh" + "_token"
	clientSecretKey := "client" + "_secret\""
	assert.NotContains(lower, refreshTokenKey, "config contains secret material:\n%s", text)
	assert.NotContains(lower, clientSecretKey, "config contains secret material:\n%s", text)
}
