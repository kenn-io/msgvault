package store_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPersistMessageWithParticipantsBumpsDisplayNameRevisionOnce(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store

	source, err := st.GetOrCreateSource("email", "persist-revision@example.com")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(
		source.ID, "persist-revision-conversation", "email_thread", "Revision Test",
	)
	require.NoError(err)
	before := participantDisplayNameRevision(t, st)
	_, err = st.PersistMessageWithParticipantsContext(t.Context(), []store.ParticipantPersistData{
		{EmailAddress: "persist-one@example.com", DisplayName: "Persist One", Domain: "example.com"},
		{EmailAddress: "persist-two@example.com", DisplayName: "Persist Two", Domain: "example.com"},
	}, func([]int64) *store.MessagePersistData {
		return &store.MessagePersistData{Message: &store.Message{
			SourceID: source.ID, SourceMessageID: "persist-revision-message",
			ConversationID: conversationID, MessageType: "email",
		}}
	})
	require.NoError(err)
	assert.Equal(before+1, participantDisplayNameRevision(t, st),
		"one message transaction must invalidate derived participant data once")
}

func TestPostgreSQLParticipantBatchSerializesWithMerge(t *testing.T) {
	require := require.New(t)
	st := storetest.New(t).Store
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL lock-order regression")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	absorbedID, err := st.EnsureParticipant(
		"z-batch-merge@example.com", "Absorbed", "example.com",
	)
	require.NoError(err)
	survivorID, err := st.EnsureParticipant(
		"a-batch-merge@example.com", "Survivor", "example.com",
	)
	require.NoError(err)
	require.Less(absorbedID, survivorID,
		"fixture requires merge row order to oppose sorted email order")

	const advisoryKey int64 = 88442212
	barrier, err := st.DB().Conn(ctx)
	require.NoError(err)
	t.Cleanup(func() {
		_, _ = barrier.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", advisoryKey)
		_ = barrier.Close()
	})
	_, err = barrier.ExecContext(ctx, "SELECT pg_advisory_lock($1)", advisoryKey)
	require.NoError(err)
	_, err = st.DB().ExecContext(ctx, fmt.Sprintf(`
		CREATE FUNCTION delay_participant_merge_update_fn() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF OLD.id = %d THEN
				PERFORM pg_advisory_xact_lock(%d);
			END IF;
			RETURN NEW;
		END
		$$;
		CREATE TRIGGER delay_participant_merge_update
		BEFORE UPDATE ON participants
		FOR EACH ROW EXECUTE FUNCTION delay_participant_merge_update_fn()`, absorbedID, advisoryKey))
	require.NoError(err)

	mergeDone := make(chan error, 1)
	go func() { mergeDone <- st.MergeParticipants(absorbedID, survivorID) }()
	require.Eventually(func() bool {
		return postgreSQLWaitingLockCount(t, st) >= 1
	}, 5*time.Second, 10*time.Millisecond, "merge did not reach its update barrier")

	batchDone := make(chan error, 1)
	go func() {
		_, batchErr := st.EnsureParticipantsBatch([]mime.Address{
			{Name: "Absorbed", Email: "z-batch-merge@example.com", Domain: "example.com"},
			{Name: "Survivor", Email: "a-batch-merge@example.com", Domain: "example.com"},
		})
		batchDone <- batchErr
	}()
	require.Eventually(func() bool {
		return postgreSQLWaitingLockCount(t, st) >= 2
	}, 5*time.Second, 10*time.Millisecond, "batch did not reach the opposing lock order")

	_, err = barrier.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", advisoryKey)
	require.NoError(err)
	require.NoError(<-mergeDone)
	require.NoError(<-batchDone)
}

func TestEnsureParticipantsBatchConcurrentOppositeOrderConverges(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	forward := []mime.Address{
		{Name: "Concurrent One", Email: "concurrent-one@example.com", Domain: "example.com"},
		{Name: "Concurrent Two", Email: "concurrent-two@example.com", Domain: "example.com"},
	}
	reverse := []mime.Address{forward[1], forward[0]}

	start := make(chan struct{})
	results := make(chan map[string]int64, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, addresses := range [][]mime.Address{forward, reverse} {
		wg.Add(1)
		go func(batch []mime.Address) {
			defer wg.Done()
			<-start
			result, err := st.EnsureParticipantsBatch(batch)
			results <- result
			errs <- err
		}(addresses)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		require.NoError(err)
	}
	var first map[string]int64
	for result := range results {
		require.Len(result, 2)
		if first == nil {
			first = result
			continue
		}
		assert.Equal(first, result)
	}
}

func TestEnsureParticipantBumpsDisplayNameRevisionOnlyWhenCreatingParticipant(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store

	before := participantDisplayNameRevision(t, st)
	participantID, err := st.EnsureParticipant(
		"directory-revision@example.com", "Directory User", "example.com",
	)
	require.NoError(err)
	afterCreate := participantDisplayNameRevision(t, st)
	assert.Equal(before+1, afterCreate,
		"new participant must invalidate derived participant data")

	againID, err := st.EnsureParticipant(
		"directory-revision@example.com", "Ignored Name", "ignored.example",
	)
	require.NoError(err)
	assert.Equal(participantID, againID)
	assert.Equal(afterCreate, participantDisplayNameRevision(t, st),
		"idempotent participant ensure must not advance the revision")
}

func TestEnsureParticipantsBatchBumpsDisplayNameRevisionForActualInserts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	addresses := []mime.Address{
		{Name: "Batch One", Email: "batch-one@example.com", Domain: "example.com"},
		{Name: "Batch Two", Email: "batch-two@example.com", Domain: "example.com"},
	}

	before := participantDisplayNameRevision(t, st)
	created, err := st.EnsureParticipantsBatch(addresses)
	require.NoError(err)
	require.Len(created, 2)
	afterCreate := participantDisplayNameRevision(t, st)
	assert.Equal(before+1, afterCreate,
		"one successful batch must invalidate derived participant data once")

	again, err := st.EnsureParticipantsBatch(addresses)
	require.NoError(err)
	assert.Equal(created, again)
	assert.Equal(afterCreate, participantDisplayNameRevision(t, st),
		"idempotent batch ensure must not advance the revision")
}

func participantDisplayNameRevision(t *testing.T, st *store.Store) int64 {
	t.Helper()
	revision, err := st.ParticipantDisplayNameRevision()
	require.NoError(t, err, "ParticipantDisplayNameRevision")
	return revision
}

func TestEnsureParticipantByIdentifierBackfillBumpsDisplayNameRevisionOnce(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store

	beforeCreate := participantDisplayNameRevision(t, st)
	participantID, err := st.EnsureParticipantByIdentifier(
		"example", "display-name-backfill", "",
	)
	require.NoError(err, "seed blank participant")
	assert.Equal(beforeCreate+1, participantDisplayNameRevision(t, st),
		"new identifier participant must invalidate derived participant data")
	before := participantDisplayNameRevision(t, st)

	backfilledID, err := st.EnsureParticipantByIdentifier(
		"example", "display-name-backfill", "Test User",
	)
	require.NoError(err, "backfill participant display name")
	assert.Equal(participantID, backfilledID)
	afterBackfill := participantDisplayNameRevision(t, st)
	assert.Equal(before+1, afterBackfill,
		"display-name backfill must invalidate derived participant data")

	retryID, err := st.EnsureParticipantByIdentifier(
		"example", "display-name-backfill", "Retry User",
	)
	require.NoError(err, "retry participant display-name backfill")
	assert.Equal(participantID, retryID)
	afterRetry := participantDisplayNameRevision(t, st)
	assert.Equal(afterBackfill, afterRetry,
		"idempotent display-name ensure must not advance the revision")
}

func TestEnsureParticipantByPhoneBackfillBumpsDisplayNameRevisionOnceWhenIdentifierNoop(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	const phone = "+1555010200"

	beforeCreate := participantDisplayNameRevision(t, st)
	participantID, err := st.EnsureParticipantByPhone(phone, "", "whatsapp")
	require.NoError(err, "seed blank phone participant")
	assert.Equal(beforeCreate+1, participantDisplayNameRevision(t, st),
		"new phone participant must invalidate derived participant data")
	beforeDisplayName := participantDisplayNameRevision(t, st)
	beforeIdentifier, err := st.ParticipantIdentifierRevision()
	require.NoError(err, "read participant identifier revision before backfill")

	backfilledID, err := st.EnsureParticipantByPhone(phone, "Phone User", "whatsapp")
	require.NoError(err, "backfill phone participant display name")
	assert.Equal(participantID, backfilledID)
	afterDisplayName := participantDisplayNameRevision(t, st)
	assert.Equal(beforeDisplayName+1, afterDisplayName,
		"display-name backfill must invalidate derived participant data")
	afterIdentifier, err := st.ParticipantIdentifierRevision()
	require.NoError(err, "read participant identifier revision after backfill")
	assert.Equal(beforeIdentifier, afterIdentifier,
		"backfill must not bump an already-complete identifier write")

	retryID, err := st.EnsureParticipantByPhone(phone, "Retry User", "whatsapp")
	require.NoError(err, "retry phone participant display-name backfill")
	assert.Equal(participantID, retryID)
	assert.Equal(afterDisplayName, participantDisplayNameRevision(t, st),
		"idempotent display-name ensure must not advance the revision")
}

func TestEnsureParticipantByPhoneSameWhitespaceNameDoesNotBumpDisplayNameRevision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	const phone = "+1555010203"

	participantID, err := st.EnsureParticipantByPhone(phone, "   ", "whatsapp")
	require.NoError(err)
	before := participantDisplayNameRevision(t, st)

	againID, err := st.EnsureParticipantByPhone(phone, "   ", "whatsapp")
	require.NoError(err)
	assert.Equal(participantID, againID)
	assert.Equal(before, participantDisplayNameRevision(t, st),
		"assigning the same whitespace value must not advance the revision")
}

func TestUpdateParticipantDisplayNameByPhoneBumpsDisplayNameRevisionOnlyOnActualUpdate(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	const phone = "+1555010201"

	_, err := st.EnsureParticipantByPhone(phone, "", "whatsapp")
	require.NoError(err, "seed blank phone participant")
	before := participantDisplayNameRevision(t, st)

	updated, err := st.UpdateParticipantDisplayNameByPhone(phone, "Phone User")
	require.NoError(err, "update phone participant display name")
	assert.True(updated, "expected phone display-name update")
	afterUpdate := participantDisplayNameRevision(t, st)
	assert.Equal(before+1, afterUpdate,
		"actual phone display-name update must invalidate derived participant data")

	updated, err = st.UpdateParticipantDisplayNameByPhone(phone, "Replacement User")
	require.NoError(err, "retry phone participant display-name update")
	assert.False(updated, "non-empty phone display name must not be overwritten")
	assert.Equal(afterUpdate, participantDisplayNameRevision(t, st),
		"no-op phone display-name update must not advance the revision")
}

func TestUpdateImessageParticipantDisplayNameByPhoneBumpsDisplayNameRevisionOnlyOnActualUpdate(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	const phone = "+1555010202"

	_, err := st.EnsureParticipantByPhone(phone, phone, "imessage")
	require.NoError(err, "seed legacy iMessage phone participant")
	before := participantDisplayNameRevision(t, st)

	updated, err := st.UpdateImessageParticipantDisplayNameByPhone(phone, "iMessage User")
	require.NoError(err, "update iMessage participant display name")
	assert.True(updated, "expected iMessage display-name update")
	afterUpdate := participantDisplayNameRevision(t, st)
	assert.Equal(before+1, afterUpdate,
		"actual iMessage display-name update must invalidate derived participant data")

	updated, err = st.UpdateImessageParticipantDisplayNameByPhone(phone, "Replacement User")
	require.NoError(err, "retry iMessage participant display-name update")
	assert.False(updated, "non-empty iMessage display name must not be overwritten")
	assert.Equal(afterUpdate, participantDisplayNameRevision(t, st),
		"no-op iMessage display-name update must not advance the revision")
}

func TestUpdateImessageParticipantDisplayNameByPhoneSameValueIsNoop(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store
	const phone = "+1555010204"

	_, err := st.EnsureParticipantByPhone(phone, phone, "imessage")
	require.NoError(err)
	before := participantDisplayNameRevision(t, st)

	updated, err := st.UpdateImessageParticipantDisplayNameByPhone(phone, phone)
	require.NoError(err)
	assert.False(updated)
	assert.Equal(before, participantDisplayNameRevision(t, st),
		"assigning the existing phone placeholder must not advance the revision")
}

func TestUpdateParticipantDisplayNameByEmailBumpsDisplayNameRevisionOnlyOnActualUpdate(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	st := f.Store

	_, err := st.EnsureParticipant("display-name@example.com", "", "example.com")
	require.NoError(err, "seed blank email participant")
	before := participantDisplayNameRevision(t, st)

	updated, err := st.UpdateParticipantDisplayNameByEmail("display-name@example.com", "Email User")
	require.NoError(err, "update email participant display name")
	assert.True(updated, "expected email display-name update")
	afterUpdate := participantDisplayNameRevision(t, st)
	assert.Equal(before+1, afterUpdate,
		"actual email display-name update must invalidate derived participant data")

	updated, err = st.UpdateParticipantDisplayNameByEmail("display-name@example.com", "Replacement User")
	require.NoError(err, "retry email participant display-name update")
	assert.False(updated, "non-empty email display name must not be overwritten")
	assert.Equal(afterUpdate, participantDisplayNameRevision(t, st),
		"no-op email display-name update must not advance the revision")
}
