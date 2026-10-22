package fbmessenger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportMessengerFailureKeepsLogicalRunResumableAcrossLaterThreads(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	opts := syntheticMessengerImport(t)
	firstDir := filepath.Join(opts.RootDir, "your_activity_across_facebook/messages/inbox/synthetic_THREAD")
	laterDir := filepath.Join(opts.RootDir, "your_activity_across_facebook/messages/inbox/zlater_THREAD")
	require.NoError(os.MkdirAll(laterDir, 0700))
	raw, err := os.ReadFile(filepath.Join(firstDir, "message_1.json"))
	require.NoError(err)
	require.NoError(os.WriteFile(filepath.Join(laterDir, "message_1.json"), []byte(strings.ReplaceAll(string(raw), "Synthetic atomic Messenger message", "Synthetic later message")), 0600))
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`CREATE FUNCTION synthetic_messenger_recovery_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.body_text LIKE 'Synthetic atomic Messenger message%' THEN RAISE EXCEPTION 'synthetic first thread failure'; END IF; RETURN NEW; END $$`)
		require.NoError(err)
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_messenger_recovery_abort BEFORE INSERT ON message_bodies FOR EACH ROW EXECUTE FUNCTION synthetic_messenger_recovery_abort()`)
	} else {
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_messenger_recovery_abort BEFORE INSERT ON message_bodies WHEN NEW.body_text LIKE 'Synthetic atomic Messenger message%' BEGIN SELECT RAISE(ABORT,'synthetic first thread failure'); END`)
	}
	require.NoError(err)
	first, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	require.NotEmpty(first.ImportRunID)
	assert.Equal(int64(1), first.MessagesAdded, "continue into the unaffected later thread")
	assert.True(first.HardErrors)
	var completed bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT completed FROM import_runs WHERE id=?`), first.ImportRunID).Scan(&completed))
	assert.False(completed, "required projection failure must not finalize logical policy")
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`DROP TRIGGER synthetic_messenger_recovery_abort ON message_bodies`)
	} else {
		_, err = st.DB().Exec(`DROP TRIGGER synthetic_messenger_recovery_abort`)
	}
	require.NoError(err)
	second, err := ImportDYI(t.Context(), st, opts)
	require.NoError(err)
	assert.True(second.WasResumed)
	assert.Equal(first.ImportRunID, second.ImportRunID)
	assert.False(second.HardErrors)
	var messages, decisions int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
	assert.Equal(2, messages)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM import_message_decisions`).Scan(&decisions))
	assert.Equal(2, decisions)
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT completed FROM import_runs WHERE id=?`), second.ImportRunID).Scan(&completed))
	assert.True(completed)
}
