package sync

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

// pausedProfileAPI holds an incremental sync in GetProfile, after it has read
// its pending retries and before it checks which messages exist.
type pausedProfileAPI struct {
	*gmail.MockAPI

	entered chan struct{}
	resume  chan struct{}
}

func (a *pausedProfileAPI) GetProfile(ctx context.Context) (*gmail.Profile, error) {
	close(a.entered)
	<-a.resume
	return a.MockAPI.GetProfile(ctx)
}

// A purge that lands after an incremental sync has read its pending retries
// still stands: the sync downloads the message but cannot save it, because the
// purge settled the retry. Later syncs leave the message deleted.
func TestDedupPurgeDuringIncrementalSyncCannotRestoreMessage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	raw := testemail.NewMessage().Header("Message-ID", "<shared@example.invalid>").Body("Same message.").Bytes()
	env.Mock.AddMessage("keep", raw, []string{"INBOX"})
	env.Mock.AddMessage("dup", raw, []string{"INBOX"})
	env.Mock.Profile.MessagesTotal = 2
	runFullSync(t, env)
	source, err := env.Store.GetOrCreateSource("gmail", testEmail)
	require.NoError(err)

	// A failed raw refill leaves a retry pending for the duplicate.
	_, err = env.Store.DB().Exec(`DELETE FROM message_raw WHERE message_id = (
		SELECT id FROM messages WHERE source_message_id = 'dup')`)
	require.NoError(err)
	env.Mock.GetMessageError["dup"] = errors.New("temporary fetch failure")
	runFullSync(t, env)
	delete(env.Mock.GetMessageError, "dup")
	ids, err := env.Store.MessageExistsBatch(source.ID, []string{"keep", "dup"})
	require.NoError(err)
	_, err = env.Store.MergeDuplicates(ids["keep"], []int64{ids["dup"]}, "batch-race")
	require.NoError(err)

	env.Mock.GetMessageCalls = nil
	api := &pausedProfileAPI{MockAPI: env.Mock, entered: make(chan struct{}), resume: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := New(api, env.Store, nil).Incremental(env.Context, source)
		done <- err
	}()
	<-api.entered
	deleted, err := env.Store.DeleteDedupedBatch("batch-race")
	close(api.resume)
	require.NoError(err)
	assert.Equal(int64(1), deleted)
	require.NoError(<-done)
	assert.Contains(env.Mock.GetMessageCalls, "dup", "the paused sync retried the duplicate")
	ids, err = env.Store.MessageExistsBatch(source.ID, []string{"dup"})
	require.NoError(err)
	assert.NotContains(ids, "dup", "the paused sync did not restore the purged duplicate")

	env.Mock.GetMessageCalls = nil
	runIncrementalSync(t, env)
	assert.Empty(env.Mock.GetMessageCalls, "the purged duplicate is not downloaded again")
	ids, err = env.Store.MessageExistsBatch(source.ID, []string{"dup"})
	require.NoError(err)
	assert.NotContains(ids, "dup")
}
