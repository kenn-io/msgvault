package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cancelDuringTriggersDialect cancels the initialisation the moment trigger
// replacement begins, then delegates. It wraps whatever dialect the store was
// built with, so it changes when the cancellation lands and nothing else.
type cancelDuringTriggersDialect struct {
	Dialect

	cancel func()
}

func (d cancelDuringTriggersDialect) EnsureTriggers(q querier) error {
	d.cancel()
	return d.Dialect.EnsureTriggers(q)
}

// TestInitSchema_TriggerReplacementStopsWhenTheContextIsCancelled is the second
// half of the operator's exit from a long upgrade.
//
// Trigger replacement runs under runMaintenance, which disables the pool-wide
// statement_timeout first, and it DROPs and CREATEs triggers on `messages`. On
// PostgreSQL those statements queue behind any conflicting lock on the table —
// an import's, say — with no timeout left to cut them off. Handed the raw
// transaction, whose Exec and QueryRow bottom out in context.Background(), they
// ignore SIGINT and SIGTERM for as long as that lock is held, and the operator's
// only remaining move is SIGKILL on a process in the middle of writing.
//
// The existing backfill cancellation test cannot catch this: it cancels at a
// batch boundary, which is a later step and a different querier.
//
// The wiring under test is dialect-independent — it is which querier the call
// site hands the dialect — so this runs on SQLite, where a store can be opened
// without the package's external test helpers.
func TestInitSchema_TriggerReplacementStopsWhenTheContextIsCancelled(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	st, err := Open(filepath.Join(t.TempDir(), "triggers.db"))
	require.NoError(err, "open store")
	t.Cleanup(func() { _ = st.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st.dialect = cancelDuringTriggersDialect{Dialect: st.dialect, cancel: cancel}

	err = st.InitSchemaContext(ctx)

	require.Error(err, "a cancelled initialisation must report failure, not a silent partial upgrade")
	require.ErrorIs(err, context.Canceled,
		"and report it as cancellation, so the daemon exits on the signal rather than "+
			"treating an operator's Ctrl-C as a corrupt archive")
	assert.Contains(err.Error(), "ensure message watermark triggers",
		"the cancellation has to stop trigger replacement itself: an error raised by a "+
			"LATER step means the DROP/CREATE ran to completion with the context "+
			"already cancelled, which on PostgreSQL is an unbounded wait on a table lock")
}
