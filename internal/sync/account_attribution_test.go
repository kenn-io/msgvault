package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/search"
)

const pendingAccountMessage = "From: sender@example.test\r\nX-Delivered-To: work@example.org\r\n" +
	"To: list@example.test\r\nSubject: Synthetic\r\nMessage-ID: <m1@example.test>\r\n\r\nbody\r\n"

func TestSyncHealsPendingAccountAttribution(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 1000
	env.Mock.AddMessage("m1", []byte(pendingAccountMessage), []string{"INBOX"})
	runFullSync(t, env)
	source, err := env.Store.GetOrCreateSource("gmail", testEmail)
	require.NoError(err)
	require.NoError(env.Store.AddAccountIdentity(source.ID, "work@example.org", "manual"))

	received := func() int {
		results, _, err := env.Store.SearchMessagesQuery(search.Parse("received:work@example.org"), 0, 10)
		require.NoError(err)
		return len(results)
	}
	pending := func() {
		_, err := env.Store.DB().Exec(
			`UPDATE messages SET account_address = NULL, account_path = NULL WHERE source_id = ?`, source.ID)
		require.NoError(err)
	}
	pending()
	require.Zero(received())

	env.SetHistory(1000)
	runIncrementalSync(t, env)
	assert.Equal(1, received(), "the next sync heals archived mail")

	pending()
	runIncrementalSync(t, env)
	assert.Equal(1, received(), "every sync derives rows that became pending since the last one")
}

func TestSyncContinuesWhenHealFails(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 1000
	env.Mock.AddMessage("m1", []byte(pendingAccountMessage), []string{"INBOX"})
	runFullSync(t, env)
	source, err := env.Store.GetOrCreateSource("gmail", testEmail)
	require.NoError(err)
	require.NoError(env.Store.AddAccountIdentity(source.ID, "work@example.org", "manual"))
	db := env.Store.DB()
	_, err = db.Exec(`UPDATE messages SET account_address = NULL, account_path = NULL WHERE source_id = ?`, source.ID)
	require.NoError(err)
	pendingRows := func() int {
		var n int
		require.NoError(db.QueryRow(
			`SELECT COUNT(*) FROM messages WHERE source_id = ? AND account_path IS NULL`, source.ID).Scan(&n))
		return n
	}

	// Hiding a table the pass writes makes it fail without touching sync itself.
	_, err = db.Exec(`ALTER TABLE message_delivery_addresses RENAME TO message_delivery_addresses_off`)
	require.NoError(err)
	env.SetHistory(1000)
	runIncrementalSync(t, env)
	_, err = db.Exec(`ALTER TABLE message_delivery_addresses_off RENAME TO message_delivery_addresses`)
	require.NoError(err)

	assert.Equal(1, pendingRows(), "a failed pass leaves the row pending")
	runIncrementalSync(t, env)
	assert.Zero(pendingRows(), "the next sync retries it")
}
