package store_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestOperationInvocationConcurrentIdenticalFinishIsIdempotent(t *testing.T) {
	st := testutil.NewTestStore(t)
	created, err := st.BeginOperationInvocation(t.Context(), operations.InvocationSpec{
		Kind: operations.KindMessageEmbedding, Key: "manual:concurrent-finish",
		Trigger: operations.TriggerManual, StartedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	counters := operations.InvocationCounters{Attempted: 1, Failed: 1}
	publicError := operations.FixedPublicError(operations.PublicErrorInvocationUpstreamFailed)

	const writers = 24
	ready := make(chan struct{}, writers)
	start := make(chan struct{})
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for range writers {
		wg.Go(func() {
			ready <- struct{}{}
			<-start
			errs <- st.FinishOperationInvocation(t.Context(), created.ID, counters, operations.StateFailed, publicError)
		})
	}
	for range writers {
		<-ready
	}
	close(start)
	wg.Wait()
	close(errs)
	for finishErr := range errs {
		assert.NoError(t, finishErr)
	}
}

func TestOperationInvocationSchemasRejectErrorsOutsideDurableAllowlist(t *testing.T) {
	st := testutil.NewTestStore(t)
	tables := []string{
		"message_embedding_runs", "person_embedding_runs", "document_extraction_runs",
		"document_embedding_runs", "visual_embedding_runs",
	}
	for _, table := range tables {
		t.Run(table, func(t *testing.T) {
			insert := st.Rebind(fmt.Sprintf(`INSERT INTO %s
				(invocation_key, trigger, state, started_at, finished_at, error_code)
				VALUES (?, 'manual', 'failed', ?, ?, ?)`, table))
			instant := time.Now().UTC()
			for _, code := range []string{"upstream_failed", "not_registered"} {
				_, err := st.DB().ExecContext(t.Context(), insert, "invalid:"+code, instant, instant, code)
				require.Error(t, err)
			}
			_, err := st.DB().ExecContext(t.Context(), insert, "valid", instant, instant,
				string(operations.PublicErrorInvocationUpstreamFailed))
			require.NoError(t, err)
		})
	}
}

func TestOperationInvocationLifecycleForEveryLedger(t *testing.T) {
	st := testutil.NewTestStore(t)
	started := time.Date(2026, 8, 30, 12, 0, 0, 123000000, time.UTC)
	ledgers := []struct {
		kind  operations.Kind
		table string
	}{
		{operations.KindMessageEmbedding, "message_embedding_runs"},
		{operations.KindPersonEmbedding, "person_embedding_runs"},
		{operations.KindDocumentExtraction, "document_extraction_runs"},
		{operations.KindDocumentEmbedding, "document_embedding_runs"},
		{operations.KindVisualEmbedding, "visual_embedding_runs"},
	}

	for index, ledger := range ledgers {
		t.Run(string(ledger.kind), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			spec := operations.InvocationSpec{
				Kind: ledger.kind, Key: fmt.Sprintf("manual:%d", index),
				Trigger: operations.TriggerManual, StartedAt: started.Add(time.Duration(index) * time.Second),
			}
			created, err := st.BeginOperationInvocation(t.Context(), spec)
			require.NoError(err)
			assert.Equal(operations.BeginCreated, created.Disposition)
			assert.Nil(created.Terminal)

			active, err := st.BeginOperationInvocation(t.Context(), spec)
			require.NoError(err)
			assert.Equal(operations.BeginActive, active.Disposition)
			assert.Equal(created.ID, active.ID)

			checkpoint := finalInvocationCounters(ledger.kind)
			require.NoError(st.CheckpointOperationInvocation(t.Context(), created.ID, checkpoint))
			decreased := checkpoint
			decreased.Attempted--
			require.Error(st.CheckpointOperationInvocation(t.Context(), created.ID, decreased))

			publicError := operations.FixedPublicError(operations.PublicErrorInvocationUpstreamFailed)
			require.Error(st.FinishOperationInvocation(
				t.Context(), created.ID, checkpoint, operations.StatePartial,
				operations.FixedPublicError(operations.PublicErrorInvocationCancelled),
			), "explicit cancellation must win over useful outcomes")
			require.NoError(st.FinishOperationInvocation(
				t.Context(), created.ID, checkpoint, operations.StatePartial, publicError,
			))
			require.NoError(st.FinishOperationInvocation(
				t.Context(), created.ID, checkpoint, operations.StatePartial, publicError,
			), "an identical finish retry must be idempotent")
			require.Error(st.FinishOperationInvocation(
				t.Context(), created.ID, checkpoint, operations.StateFailed, publicError,
			), "a terminal result cannot be rewritten")

			terminal, err := st.BeginOperationInvocation(t.Context(), spec)
			require.NoError(err)
			assert.Equal(operations.BeginTerminal, terminal.Disposition)
			require.NotNil(terminal.Terminal)
			assert.Equal(operations.StatePartial, terminal.Terminal.State)
			assert.Equal(checkpoint.PublicCounters(ledger.kind), terminal.Terminal.Counters)
			_, err = st.DB().ExecContext(t.Context(), st.Rebind(fmt.Sprintf(
				"UPDATE %s SET succeeded = succeeded + 1 WHERE id = ?", ledger.table)),
				mustStoreOperationID(t, created.ID))
			require.Error(err, "the database must reject terminal counter rewrites")

			next := spec
			next.Key += ":retry"
			different, err := st.BeginOperationInvocation(t.Context(), next)
			require.NoError(err)
			assert.NotEqual(created.ID, different.ID)
		})
	}
}

func mustStoreOperationID(t *testing.T, id operations.StableID) int64 {
	t.Helper()
	value, ok := id.Int64()
	require.True(t, ok)
	return value
}

func TestOperationInvocationRecoveryUsesDurableOutcomes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	started := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	useful, err := st.BeginOperationInvocation(t.Context(), operations.InvocationSpec{
		Kind: operations.KindMessageEmbedding, Key: "scheduler:useful",
		Trigger: operations.TriggerScheduled, StartedAt: started,
	})
	require.NoError(err)
	require.NoError(st.CheckpointOperationInvocation(t.Context(), useful.ID,
		operations.InvocationCounters{Attempted: 1, Succeeded: 1}))
	empty, err := st.BeginOperationInvocation(t.Context(), operations.InvocationSpec{
		Kind: operations.KindDocumentExtraction, Key: "scheduler:empty",
		Trigger: operations.TriggerScheduled, StartedAt: started.Add(time.Second),
	})
	require.NoError(err)

	recoveredAt := started.Add(time.Minute)
	require.NoError(st.RecoverOperationInvocations(t.Context(), recoveredAt))
	require.NoError(st.RecoverOperationInvocations(t.Context(), recoveredAt), "recovery must be idempotent")

	usefulReplay, err := st.BeginOperationInvocation(t.Context(), operations.InvocationSpec{
		Kind: operations.KindMessageEmbedding, Key: "scheduler:useful",
		Trigger: operations.TriggerScheduled, StartedAt: started,
	})
	require.NoError(err)
	require.NotNil(usefulReplay.Terminal)
	assert.Equal(operations.StatePartial, usefulReplay.Terminal.State)
	assert.Equal(operations.PublicErrorInvocationDaemonRestarted, usefulReplay.Terminal.Error.Code)
	emptyReplay, err := st.BeginOperationInvocation(t.Context(), operations.InvocationSpec{
		Kind: operations.KindDocumentExtraction, Key: "scheduler:empty",
		Trigger: operations.TriggerScheduled, StartedAt: started.Add(time.Second),
	})
	require.NoError(err)
	require.NotNil(emptyReplay.Terminal)
	assert.Equal(operations.StateFailed, emptyReplay.Terminal.State)
	assert.Equal(empty.ID, emptyReplay.ID)
}

func finalInvocationCounters(kind operations.Kind) operations.InvocationCounters {
	switch kind {
	case operations.KindVisualEmbedding:
		return operations.InvocationCounters{Attempted: 3, Succeeded: 1, Failed: 1, Skipped: 1}
	default:
		return operations.InvocationCounters{Attempted: 2, Succeeded: 1, Failed: 1}
	}
}
