package cmd

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestImportMboxCmd_GoogleGroupsTakeout(t *testing.T) {
	markDaemonCLISubprocessForTest(t)

	require := require.New(t)
	assert := assert.New(t)
	tmp := t.TempDir()

	// Save/restore global state for cmd package.
	prevNoDefault := noDefaultIdentityImportMbox
	prevCfg := cfg
	prevLogger := logger
	prevSourceType := importMboxSourceType
	prevLabel := importMboxLabels
	prevNoResume := importMboxNoResume
	prevCheckpointInterval := importMboxCheckpointInterval
	prevNoAttachments := importMboxNoAttachments
	prevCfgFile := cfgFile
	prevHomeDir := homeDir
	prevVerbose := verbose
	prevOut := rootCmd.OutOrStdout()
	prevErr := rootCmd.ErrOrStderr()
	t.Cleanup(func() {
		noDefaultIdentityImportMbox = prevNoDefault
		cfg = prevCfg
		logger = prevLogger
		importMboxSourceType = prevSourceType
		importMboxLabels = prevLabel
		importMboxNoResume = prevNoResume
		importMboxCheckpointInterval = prevCheckpointInterval
		importMboxNoAttachments = prevNoAttachments
		cfgFile = prevCfgFile
		homeDir = prevHomeDir
		verbose = prevVerbose
		rootCmd.SetOut(prevOut)
		rootCmd.SetErr(prevErr)
		rootCmd.SetArgs(nil)
	})

	zipPath := filepath.Join(tmp, "takeout.zip")
	raw := `From synthetic@example.invalid Mon Jan 1 12:00:00 +0000 2024
From: Test Group <test-group@googlegroups.com>
To: alice@example.com
Message-ID: <one@example.com>
X-Google-Groups: test-group
X-GM-THRID: 123
X-Gmail-Labels: Starred, Starred
Subject: Group announcement
Content-Type: multipart/mixed; boundary=parts

--parts
Content-Type: text/plain

Synthetic announcement.
--parts
Content-Type: text/plain
Content-Disposition: attachment; filename=test.txt

Synthetic attachment.
--parts--
`
	writeZipFile(t, zipPath, map[string]string{
		"Takeout/Groups/googlegroups.com/test-group@googlegroups.com/topics.mbox":  raw,
		"Takeout/Groups/googlegroups.com/other-group@googlegroups.com/topics.mbox": strings.ReplaceAll(strings.ReplaceAll(raw, "test-group", "other-group"), "one@example.com", "two@example.com"),
		"Takeout/Groups/googlegroups.com/test-group@googlegroups.com/members.csv":  "Email\nalice@example.com\n",
	})
	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs([]string{"--home", tmp, "import-mbox", "test-group@googlegroups.com", zipPath, "--source-type", "google-groups", "--label", "test-group", "--no-resume"})
	require.NoError(rootCmd.ExecuteContext(context.Background()))
	st, err := store.Open(filepath.Join(tmp, "msgvault.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	var messages, identities, fromMe, attachments, labels int
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM messages").Scan(&messages))
	assert.Equal(2, messages)
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM account_identities").Scan(&identities))
	assert.Zero(identities, "the group address must not become the user's identity")
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM messages WHERE is_from_me = 1").Scan(&fromMe))
	assert.Zero(fromMe)
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM attachments").Scan(&attachments))
	assert.Equal(2, attachments)
	require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM message_labels").Scan(&labels))
	assert.Equal(5, labels, "repeated header and explicit labels must not duplicate links")
}
