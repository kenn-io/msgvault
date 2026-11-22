package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestGmailSkipsSpamTrashResolvesEntriesLikeSchedules(t *testing.T) {
	dir := t.TempDir()
	s := testutil.NewSQLiteTestStoreAt(t, filepath.Join(dir, "msgvault.db"))
	src, err := s.GetOrCreateSource(sourceTypeGmail, "user@example.com")
	require.NoError(t, err)
	require.NoError(t, s.UpdateSourceDisplayName(src.ID, "Work Mail"))
	_, err = s.GetOrCreateSource(sourceTypeGmail, "other@example.com")
	require.NoError(t, err)

	tests := []struct {
		name    string
		entries string
		want    bool
	}{
		{"identifier in another case", `email = "User@Example.com"` + "\ngmail_skip_spam_trash = true", true},
		{"display name", `email = "work mail"` + "\ngmail_skip_spam_trash = true", true},
		{"another account", `email = "other@example.com"` + "\ngmail_skip_spam_trash = true", false},
		{"setting off", `email = "user@example.com"` + "\nschedule = \"0 2 * * *\"", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configDir := t.TempDir()
			configPath := filepath.Join(configDir, "config.toml")
			require.NoError(t, os.WriteFile(configPath, []byte("[[accounts]]\n"+tt.entries+"\n"), 0o600))
			cfg, err := config.Load(configPath, configDir)
			require.NoError(t, err)

			got, err := gmailSkipsSpamTrash(t.Context(), cfg, s, src)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
