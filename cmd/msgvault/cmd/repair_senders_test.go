package cmd

import (
	"bytes"
	"database/sql"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

func TestRepairSendersAlwaysProxiesThroughDaemonCLIRunner(t *testing.T) {
	server, requests := newDaemonCLIRunnerTestServer(t,
		func(req daemonCLIRunTestRequest) {
			assert.Equal(t, []string{"repair-senders", "--apply"}, req.Args)
		},
		`{"type":"stdout","data":"Repaired: 1\n"}`,
		`{"type":"complete"}`,
	)
	configureRemoteDaemonForTest(t, server.URL)
	t.Setenv(daemonCLISubprocessEnv, "")

	var stdout bytes.Buffer
	cmd := newRepairSendersCmd()
	cmd.SetArgs([]string{"--apply"})
	cmd.SetOut(&stdout)

	require.NoError(t, cmd.Execute())
	assert.Equal(t, 1, int(requests.Load()))
	assert.Contains(t, stdout.String(), "Repaired: 1")
}

func TestRunRepairSendersLocalDryRunAndApply(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dataDir := t.TempDir()
	savedCfg := cfg
	cfg = &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
	}
	t.Cleanup(func() { cfg = savedCfg })

	st, err := store.OpenForTest(cfg.DatabaseDSN())
	require.NoError(err, "OpenForTest")
	require.NoError(st.InitSchema(), "InitSchema")
	source, err := st.GetOrCreateSource("gmail", "archive@example.test")
	require.NoError(err, "GetOrCreateSource")
	conversationID, err := st.EnsureConversation(
		source.ID, "sender-repair-thread", "Sender repair",
	)
	require.NoError(err, "EnsureConversation")

	insert := func(sourceMessageID, messageType string, raw []byte) int64 {
		t.Helper()
		messageID, insertErr := st.UpsertMessage(&store.Message{
			ConversationID:  conversationID,
			SourceID:        source.ID,
			SourceMessageID: sourceMessageID,
			MessageType:     messageType,
		})
		require.NoError(insertErr, "UpsertMessage %s", sourceMessageID)
		require.NoError(st.UpsertMessageRaw(messageID, raw),
			"UpsertMessageRaw %s", sourceMessageID)
		return messageID
	}
	repairable := insert("repairable", store.MessageTypeEmail,
		[]byte("From: Fridgeco <noreply@fridgeco.example >\r\n"+
			"Subject: Repair me\r\n\r\nBody\r\n"))
	headerless := insert("headerless", store.MessageTypeEmail,
		[]byte("Subject: No sender\r\n\r\nBody\r\n"))
	uninstallable := insert("uninstallable", store.MessageTypeEmail,
		[]byte("From: Weird <x..y@example.test >\r\n"+
			"Subject: Recovered but invalid\r\n\r\nBody\r\n"))
	chat := insert("chat", store.MessageTypeGoogleChat,
		[]byte("From: chat@example.test\r\n\r\nBody\r\n"))
	require.NoError(st.Close(), "close seed store")

	var dryRunOut bytes.Buffer
	dryRunCmd := &cobra.Command{}
	dryRunCmd.SetContext(t.Context())
	dryRunCmd.SetOut(&dryRunOut)
	require.NoError(runRepairSendersLocal(dryRunCmd, false))
	assert.Contains(dryRunOut.String(), "Candidates: 3")
	assert.Contains(dryRunOut.String(), "Repairable: 1")
	assert.Contains(dryRunOut.String(), "Unresolved: 2")
	assert.Contains(dryRunOut.String(), "Dry run: no rows were modified")
	assert.False(readRepairSenderID(t, repairable).Valid,
		"dry run must not set sender_id")

	var applyOut bytes.Buffer
	applyCmd := &cobra.Command{}
	applyCmd.SetContext(t.Context())
	applyCmd.SetOut(&applyOut)
	require.NoError(runRepairSendersLocal(applyCmd, true))
	assert.Contains(applyOut.String(), "Candidates: 3")
	assert.Contains(applyOut.String(), "Repairable: 1")
	assert.Contains(applyOut.String(), "Unresolved: 2")
	assert.Contains(applyOut.String(), "Repaired: 1")
	require.True(readRepairSenderID(t, repairable).Valid,
		"apply must set sender_id")
	assert.False(readRepairSenderID(t, headerless).Valid,
		"headerless MIME must remain unresolved")
	assert.False(readRepairSenderID(t, uninstallable).Valid,
		"a recovered but invalid address must stay unresolved instead of failing apply")
	assert.False(readRepairSenderID(t, chat).Valid,
		"non-email messages must remain untouched")

	check, err := store.OpenForTest(cfg.DatabaseDSN())
	require.NoError(err, "open repaired store")
	defer func() { _ = check.Close() }()
	var fromCount int
	require.NoError(check.DB().QueryRow(check.Rebind(`
		SELECT COUNT(*)
		FROM message_recipients mr
		JOIN participants p ON p.id = mr.participant_id
		WHERE mr.message_id = ? AND mr.recipient_type = 'from'
		  AND p.email_address = 'noreply@fridgeco.example'
	`), repairable).Scan(&fromCount), "count repaired From row")
	assert.Equal(1, fromCount)
}

func readRepairSenderID(t *testing.T, messageID int64) sql.NullInt64 {
	t.Helper()
	st, err := store.OpenForTest(cfg.DatabaseDSN())
	require.NoError(t, err, "open sender check store")
	defer func() { _ = st.Close() }()
	var senderID sql.NullInt64
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT sender_id FROM messages WHERE id = ?`), messageID).Scan(&senderID),
		"read sender_id")
	return senderID
}
