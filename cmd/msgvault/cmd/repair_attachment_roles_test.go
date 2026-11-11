package cmd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// TestRunRepairAttachmentRolesLocalClassifiesAndResumes catches a command that
// leaves historical attachments unknown, reports no summary, or rescans an
// archive whose repair cursor already completed. The fixture stores real raw
// MIME, so the store's parser decides each role.
func TestRunRepairAttachmentRolesLocalClassifiesAndResumes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dataDir := t.TempDir()
	cfg := &config.Config{HomeDir: dataDir, Data: config.DataConfig{DataDir: dataDir}}
	testCtx := testInvocationContext(t.Context(), cfg, invocationOptions{})

	ids := newAttachmentRoleRepairArchive(t, cfg)

	var firstOut bytes.Buffer
	firstCmd := &cobra.Command{}
	firstCmd.SetContext(testCtx)
	firstCmd.SetOut(&firstOut)
	require.NoError(runRepairAttachmentRolesLocal(firstCmd))
	assert.Equal(
		"Attachment role repair complete: messages scanned=2 attachments updated=2\n",
		firstOut.String())
	assert.Equal("inline", attachmentRoleRepairArchiveRole(t, cfg, ids.inline))
	assert.Equal("standalone", attachmentRoleRepairArchiveRole(t, cfg, ids.standalone))

	var secondOut bytes.Buffer
	secondCmd := &cobra.Command{}
	secondCmd.SetContext(testCtx)
	secondCmd.SetOut(&secondOut)
	require.NoError(runRepairAttachmentRolesLocal(secondCmd))
	assert.Equal(
		"Attachment role repair complete: messages scanned=0 attachments updated=0\n",
		secondOut.String())
}

// TestRepairAttachmentRolesCommandRoutesThroughDaemonCLIRunner catches
// bypassing the daemon-owned writer path from a normal CLI parent process.
func TestRepairAttachmentRolesCommandRoutesThroughDaemonCLIRunner(t *testing.T) {
	const summary = "Attachment role repair complete: messages scanned=1 attachments updated=1\n"
	server, requests := newDaemonCLIRunnerTestServer(t,
		func(req daemonCLIRunTestRequest) {
			assert.Equal(t, []string{"repair-attachment-roles"}, req.Args)
		},
		`{"type":"stdout","data":"Attachment role repair complete: messages scanned=1 attachments updated=1\n"}`,
		`{"type":"complete"}`,
	)
	testCtx := configureRemoteDaemonForTest(t, server.URL)
	t.Setenv(daemonCLISubprocessEnv, "")

	cmd := newRepairAttachmentRolesCmd()
	cmd.SetContext(testCtx)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetArgs([]string{})
	require.NoError(t, cmd.Execute())
	assert.Equal(t, int32(1), requests.Load())
	assert.Equal(t, summary, stdout.String())
}

type attachmentRoleRepairArchiveIDs struct {
	inline     int64
	standalone int64
}

func newAttachmentRoleRepairArchive(t *testing.T, cfg *config.Config) attachmentRoleRepairArchiveIDs {
	t.Helper()
	st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
	source, err := st.GetOrCreateSource("gmail", "archive@example.test")
	require.NoError(t, err)
	conversationID, err := st.EnsureConversation(t.Context(), source.ID, "role-repair-thread", "Role repair")
	require.NoError(t, err)

	seed := func(sourceMessageID, partHeaders, content string) int64 {
		messageID, err := st.UpsertMessage(&store.Message{
			ConversationID:  conversationID,
			SourceID:        source.ID,
			SourceMessageID: sourceMessageID,
			MessageType:     "email",
		})
		require.NoError(t, err)
		raw := "From: sender@example.test\r\nTo: archive@example.test\r\n" +
			"MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
			"--b\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
			"--b\r\n" + partHeaders + "\r\n" + content + "\r\n--b--\r\n"
		require.NoError(t, st.UpsertMessageRaw(messageID, []byte(raw)))
		digest := sha256.Sum256([]byte(content))
		hash := hex.EncodeToString(digest[:])
		_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
			INSERT INTO attachments
				(message_id, filename, mime_type, storage_path, content_hash, size, created_at)
			VALUES (?, 'part.bin', 'application/octet-stream', ?, ?, ?, CURRENT_TIMESTAMP)`),
			messageID, hash[:2]+"/"+hash, hash, len(content))
		require.NoError(t, err)
		return messageID
	}

	ids := attachmentRoleRepairArchiveIDs{
		inline: seed("role-repair-inline",
			"Content-Type: image/png\r\nContent-Disposition: inline; filename=logo.png\r\n"+
				"Content-ID: <logo@example.test>\r\n",
			"logo-bytes"),
		standalone: seed("role-repair-standalone",
			"Content-Type: application/pdf\r\nContent-Disposition: attachment; filename=contract.pdf\r\n",
			"contract-bytes"),
	}
	require.NoError(t, st.Close())
	return ids
}

func attachmentRoleRepairArchiveRole(t *testing.T, cfg *config.Config, messageID int64) string {
	t.Helper()
	st, err := store.OpenForTest(t.Context(), cfg.DatabaseDSN())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	var role string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT attachment_role FROM attachments WHERE message_id = ?`), messageID).Scan(&role))
	return role
}
