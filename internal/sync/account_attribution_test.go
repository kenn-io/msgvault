package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/search"
)

func TestSyncHealsPendingAccountAttribution(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 1000
	env.Mock.AddMessage("m1", []byte("From: sender@example.test\r\nX-Delivered-To: work@example.org\r\n"+
		"To: list@example.test\r\nSubject: Synthetic\r\nMessage-ID: <m1@example.test>\r\n\r\nbody\r\n"), []string{"INBOX"})
	runFullSync(t, env)
	source, err := env.Store.GetOrCreateSource("gmail", testEmail)
	require.NoError(err)
	require.NoError(env.Store.AddAccountIdentity(source.ID, "work@example.org", "manual"))

	received := func() int {
		results, _, err := env.Store.SearchMessagesQuery(search.Parse("received:work@example.org"), 0, 10)
		require.NoError(err)
		return len(results)
	}
	legacy := func(dropLedger bool) {
		_, err := env.Store.DB().Exec(`UPDATE messages SET account_address = NULL, account_path = NULL WHERE source_id = ?`, source.ID)
		require.NoError(err)
		if dropLedger {
			_, err = env.Store.DB().Exec(`DELETE FROM applied_migrations WHERE name = ?`,
				"rederive:account-attribution:gmail:"+testEmail+":v2")
			require.NoError(err)
		}
	}
	legacy(true)
	require.Zero(received())

	env.SetHistory(1000)
	runIncrementalSync(t, env)
	assert.Equal(1, received(), "the next sync heals archived mail")

	legacy(false)
	runIncrementalSync(t, env)
	assert.Zero(received(), "a recorded pass does not run again")
}

func TestSyncContinuesWhenHealFails(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	env.Mock.Profile.MessagesTotal = 1
	env.Mock.Profile.HistoryID = 1000
	env.Mock.AddMessage("m1", []byte("From: sender@example.test\r\nX-Delivered-To: work@example.org\r\n"+
		"To: list@example.test\r\nSubject: Synthetic\r\nMessage-ID: <m1@example.test>\r\n\r\nbody\r\n"), []string{"INBOX"})
	runFullSync(t, env)
	source, err := env.Store.GetOrCreateSource("gmail", testEmail)
	require.NoError(err)
	ledger := "rederive:account-attribution:gmail:" + testEmail + ":v2"
	db := env.Store.DB()
	_, err = db.Exec(`UPDATE messages SET account_address = NULL, account_path = NULL WHERE source_id = ?`, source.ID)
	require.NoError(err)
	_, err = db.Exec(`DELETE FROM applied_migrations WHERE name = ?`, ledger)
	require.NoError(err)

	// Hiding a table the pass writes makes it fail without touching sync itself.
	_, err = db.Exec(`ALTER TABLE message_delivery_addresses RENAME TO message_delivery_addresses_off`)
	require.NoError(err)
	env.SetHistory(1000)
	runIncrementalSync(t, env)
	_, err = db.Exec(`ALTER TABLE message_delivery_addresses_off RENAME TO message_delivery_addresses`)
	require.NoError(err)

	var applied int
	require.NoError(db.QueryRow(`SELECT COUNT(*) FROM applied_migrations WHERE name = ?`, ledger).Scan(&applied))
	assert.Zero(applied, "a failed pass stays owed")
	runIncrementalSync(t, env)
	require.NoError(db.QueryRow(`SELECT COUNT(*) FROM applied_migrations WHERE name = ?`, ledger).Scan(&applied))
	assert.Equal(1, applied, "the next sync retries it")
}
