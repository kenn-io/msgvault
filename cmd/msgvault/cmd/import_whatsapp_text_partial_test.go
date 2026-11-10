package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

func TestWhatsAppTextPartialCoverage(t *testing.T) {
	markDaemonCLISubprocessForTest(t)
	cfg := config.NewDefaultConfig()
	home := t.TempDir()
	cfg.HomeDir = home
	cfg.Data.DataDir = home
	st, err := store.Open(cfg.DatabaseDSN())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, st.Close()) })
	require.NoError(t, st.InitSchemaContext(t.Context()))
	_, err = st.DB().ExecContext(t.Context(), "CREATE TRIGGER reject_text_media BEFORE INSERT ON attachments BEGIN SELECT RAISE(FAIL, 'fictional attachment failure'); END")
	require.NoError(t, err)
	fixture := filepath.Join(t.TempDir(), "chat.txt")
	require.NoError(t, os.WriteFile(fixture, []byte("13/6/26, 10:00 - Mira: <attached: note.txt>\n"), 0600))
	media := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(media, "note.txt"), []byte("fictional seed list"), 0600))
	cmd := &cobra.Command{Use: "test"}
	cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	cmd.AddCommand(newImportWhatsAppCmd())
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"import-whatsapp", fixture, "--format", "text", "--phone", "+12025550123", "--chat-id", "garden", "--chat-type", "group", "--date-order", "dmy", "--timezone", "UTC", "--media-dir", media, "--no-default-identity"})
	require.ErrorContains(t, cmd.Execute(), "fictional attachment failure")
	assert.Contains(t, out.String(), "Import incomplete: 1 records")
	assert.Contains(t, out.String(), "1 added, 0 unchanged")
	assert.Contains(t, out.String(), "Media: 0 stored")
	var processed, added int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT messages_processed,messages_added FROM sync_runs ORDER BY id DESC LIMIT 1").Scan(&processed, &added))
	assert.Equal(t, 1, processed)
	assert.Equal(t, 1, added)
}
