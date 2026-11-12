package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
	syncer "go.kenn.io/msgvault/internal/sync"
	"go.kenn.io/msgvault/internal/testutil"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

func TestRepairDerivedAttachmentMessage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	testutil.SkipIfPostgres(t, "command fixture opens its own local SQLite archive")
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	st, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	require.NoError(st.InitSchema())
	mock := gmail.NewMockAPI()
	mock.Profile = &gmail.Profile{EmailAddress: "synthetic@example.com", HistoryID: 1000}
	raw := testemail.NewMessage().WithAttachment("restore.bin", "application/octet-stream", []byte("local bytes")).Bytes()
	mock.AddMessage("local-attachment", raw, []string{"INBOX"})
	_, err = syncer.New(mock, st, &syncer.Options{}).Full(t.Context(), "synthetic@example.com")
	require.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier(t.Context(), "gmail", "synthetic@example.com")
	require.NoError(err)
	ids, err := st.MessageExistsBatch(source.ID, []string{"local-attachment"})
	require.NoError(err)
	require.NoError(st.Close())
	for range 2 {
		root := newTestRootCmd()
		root.AddCommand(newRepairDerivedCmd())
		root.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
		root.SetArgs([]string{"repair-derived", "--attachment-message", "local-attachment", "--source-id", strconv.FormatInt(source.ID, 10)})
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		require.NoError(root.Execute())
		assert.Contains(out.String(), "Attachments restored")
	}
	st, err = store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	defer func() { require.NoError(st.Close()) }()
	refs, err := st.MessageMIMEAttachmentsContext(t.Context(), ids["local-attachment"])
	require.NoError(err)
	require.Len(refs, 1)
	got, err := os.ReadFile(filepath.Join(cfg.AttachmentsDir(), refs[0].StoragePath))
	require.NoError(err)
	assert.Equal([]byte("local bytes"), got)
	got, err = st.GetMessageRaw(ids["local-attachment"])
	require.NoError(err)
	assert.Equal(raw, got)
}
