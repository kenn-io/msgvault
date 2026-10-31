package sync

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

func TestIncrementalSyncPreservesRetryWhenErrorRecordingFails(t *testing.T) {
	for _, tc := range []struct {
		name            string
		failErrorRecord bool
	}{
		{name: "error_record_persists"},
		{name: "error_record_fails", failErrorRecord: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			env := newTestEnv(t)
			source := env.CreateSourceWithHistory(t, "999")
			messages := []struct {
				id  string
				raw []byte
			}{
				{"good", testemail.NewMessage().Header("Message-ID", "<good@example.invalid>").Body("Healthy message.").Bytes()},
				{"lost", testemail.NewMessage().Header("Message-ID", "<lost@example.invalid>").Body("Message to retry.").Bytes()},
			}
			for _, message := range messages {
				env.Mock.AddMessage(message.id, message.raw, []string{"INBOX"})
			}
			env.Mock.Profile.MessagesTotal = 2
			env.SetHistory(1000, historyAdded("lost"), historyAdded("good"))
			env.Mock.GetMessageError["lost"] = errors.New("temporary fetch failure")
			if tc.failErrorRecord {
				// Reject only this error record. Other messages and cursor writes
				// remain writable; this does not simulate lock contention or a crash.
				_, err := env.Store.DB().Exec(`CREATE TRIGGER fail_retry_error_record
					BEFORE INSERT ON sync_run_items
					WHEN NEW.source_message_id = 'lost' AND NEW.status = 'error'
					BEGIN SELECT RAISE(ABORT, 'synthetic error-record failure'); END`)
				require.NoError(err)
			}
			summary, runErr := env.Syncer.Incremental(env.Context, source)
			t.Logf("first sync: summary=%+v error=%v", summary, runErr)
			require.Contains(env.Mock.GetMessageCalls, "lost", "exercise the recoverable fetch failure")
			if tc.failErrorRecord {
				_, err := env.Store.DB().Exec("DROP TRIGGER fail_retry_error_record")
				require.NoError(err)
			}

			var sequence int
			var database, dbPath string
			require.NoError(env.Store.DB().QueryRow("PRAGMA database_list").Scan(&sequence, &database, &dbPath))
			reopen := func() {
				// A failed reopen must not leave cleanup pointing at a closed Store.
				current := env.Store
				env.Store = nil
				require.NoError(current.Close())
				reopened, err := store.Open(dbPath)
				require.NoError(err)
				env.Store = reopened
				env.Syncer = New(env.Mock, env.Store, nil)
			}
			t.Cleanup(func() {
				if env.Store != nil {
					assert.NoError(env.Store.Close())
				}
			})
			reopen()
			delete(env.Mock.GetMessageError, "lost")
			env.Mock.GetMessageCalls = nil
			// No new History event. Retain the original page so a conservative
			// cursor is as acceptable as durable debt or another recovery route.
			runIncrementalSync(t, env)
			t.Logf("raw fetches after restart: %v", env.Mock.GetMessageCalls)
			reopen()
			ids, err := env.Store.MessageExistsBatch(source.ID, []string{"good", "lost"})
			require.NoError(err)
			for _, message := range messages {
				require.NotZero(ids[message.id], "G2/G3: acquisition %q must remain automatically recoverable after restart without a new History event", message.id)
				raw, err := env.Store.GetMessageRaw(ids[message.id])
				require.NoError(err)
				assert.Equal(message.raw, raw, "MIME bytes after reopening: %s", message.id)
			}
			assert.Contains(env.Mock.GetMessageCalls, "lost", "restart retries the failed acquisition without a new History event")
		})
	}
}

// A full sync completes with item errors and leaves them for incremental
// replay. If it cannot record one, it must fail so a rerun fetches the message.
func TestFullSyncFailsWhenRetryRecordingFails(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	seedMessages(env, 2, 1000, "good", "lost")
	env.Mock.GetMessageError["lost"] = errors.New("temporary fetch failure")
	_, err := env.Store.DB().Exec(`CREATE TRIGGER fail_retry_error_record
		BEFORE INSERT ON sync_run_items
		WHEN NEW.source_message_id = 'lost' AND NEW.status = 'error'
		BEGIN SELECT RAISE(ABORT, 'synthetic error-record failure'); END`)
	require.NoError(err)

	_, err = env.Syncer.Full(env.Context, testEmail)
	require.ErrorContains(err, `record retry for Gmail message "lost"`)
	run, err := env.Store.GetLatestSync(source.ID)
	require.NoError(err)
	assert.Equal(store.SyncStatusFailed, run.Status)

	_, err = env.Store.DB().Exec("DROP TRIGGER fail_retry_error_record")
	require.NoError(err)
	delete(env.Mock.GetMessageError, "lost")
	runFullSync(t, env)
	ids, err := env.Store.MessageExistsBatch(source.ID, []string{"good", "lost"})
	require.NoError(err)
	assert.NotZero(ids["good"])
	assert.NotZero(ids["lost"], "rerun archives the message whose retry was not recorded")
}
