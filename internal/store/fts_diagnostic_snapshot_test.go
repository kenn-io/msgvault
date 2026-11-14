package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Observe real queries, including cancellation inside the driver. The forwarding
// connector is shared with the existing Gmail snapshot tests; SQL is never faked.
type ftsDiagnosticGateConnector struct {
	driver.Connector

	gate   *gmailAuditSnapshotGate
	marker string
}

func (c *ftsDiagnosticGateConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &ftsDiagnosticGateConn{gmailAuditSnapshotConn: &gmailAuditSnapshotConn{Conn: conn}, gate: c.gate, marker: c.marker}, nil
}

type ftsDiagnosticGateConn struct {
	*gmailAuditSnapshotConn

	gate   *gmailAuditSnapshotGate
	marker string
}

func (c *ftsDiagnosticGateConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if gate, ok := ctx.Value(gmailAuditSnapshotGateKey{}).(*gmailAuditSnapshotGate); ok && gate == c.gate && strings.Contains(query, c.marker) && (gate.targetID == 0 || gate.gatesTargetQuery(args)) {
		if err := gate.hold(ctx); err != nil {
			return nil, err
		}
	}
	queryer, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return queryer.QueryContext(ctx, query, args)
}

func newFTSDiagnosticGateStore(t *testing.T, marker string) (*Store, *gmailAuditSnapshotGate) {
	t.Helper()
	gate := newGmailAuditSnapshotGate(0)
	var st *Store
	var connector driver.Connector
	if IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		st = newPGStoreInternal(t)
		cfg, err := postgresConnConfig(st.dbPath, false)
		require.NoError(t, err)
		connector = stdlib.GetConnector(*cfg)
	} else {
		st = newGmailAuditSnapshotSQLiteStore(t, newGmailAuditSnapshotGate(0))
		connector = &gmailAuditSnapshotSQLiteConnector{driver: st.db.Driver(), dsn: st.dbPath}
	}
	db := sql.OpenDB(&ftsDiagnosticGateConnector{Connector: connector, gate: gate, marker: marker})
	db.SetMaxOpenConns(4)
	replaceTestStoreDB(t, st, db)
	return st, gate
}

func seedFTSDiagnosticGateMessage(t *testing.T, st *Store) int64 {
	t.Helper()
	source, err := st.GetOrCreateSource("gmail", "fts-snapshot@example.test")
	require.NoError(t, err)
	id := seedGmailAuditSnapshotMessage(t, st, source.ID, "synthetic", "sender@example.test")
	require.NoError(t, st.IndexStoredMessageContext(t.Context(), id))
	return id
}

func TestFTSDiagnosticConcurrentWriteUsesOneSnapshot(t *testing.T) {
	a, r := assert.New(t), require.New(t)
	marker := "FROM messages_fts WHERE rowid = ?"
	if IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		marker = "search_fts_fields IS NOT DISTINCT"
	}
	st, gate := newFTSDiagnosticGateStore(t, marker)
	id := seedFTSDiagnosticGateMessage(t, st)
	ctx, cancel := context.WithTimeout(context.WithValue(t.Context(), gmailAuditSnapshotGateKey{}, gate), 15*time.Second)
	defer cancel()
	type result struct {
		report FTSDiagnosticReport
		err    error
	}
	done := make(chan result, 1)
	go func() { report, err := st.DiagnoseFTSContext(ctx, FTSDiagnosticOptions{}); done <- result{report, err} }()
	select {
	case <-gate.paused:
	case <-ctx.Done():
		r.FailNow("diagnostic did not reach document comparison")
	}
	// The expected body has already been read. Rewrite and reindex before the
	// indexed document read: without one snapshot, old and new would differ.
	r.NoError(st.UpsertMessageBodyContext(t.Context(), id, sql.NullString{String: "new orchard", Valid: true}, sql.NullString{}))
	r.NoError(st.IndexStoredMessageContext(t.Context(), id))
	gate.resume()
	select {
	case got := <-done:
		r.NoError(got.err)
		a.Equal("ok", got.report.Outcome)
	case <-ctx.Done():
		r.FailNow("diagnostic did not complete")
	}
	got, err := st.DiagnoseFTSContext(t.Context(), FTSDiagnosticOptions{})
	r.NoError(err)
	a.Equal("ok", got.Outcome, "fresh snapshots also observe the committed representation")
}

func TestFTSDiagnosticCancellationBoundaries(t *testing.T) {
	for _, boundary := range []string{"ORDER BY id LIMIT", "indexing_version FROM messages", "FROM message_recipients mr LEFT", "FROM message_bodies WHERE", "COALESCE(m.subject", "FROM messages_fts WHERE rowid = ?"} {
		t.Run(boundary, func(t *testing.T) {
			a, r := assert.New(t), require.New(t)
			marker := boundary
			pg := IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB"))
			if boundary == "indexing_version FROM messages" && !pg {
				marker = "NULL FROM messages m WHERE"
			}
			if boundary == "FROM messages_fts WHERE rowid = ?" && pg {
				marker = "search_fts_fields IS NOT DISTINCT"
			}
			st, gate := newFTSDiagnosticGateStore(t, marker)
			id := seedFTSDiagnosticGateMessage(t, st)
			ctx, cancel := context.WithCancel(context.WithValue(t.Context(), gmailAuditSnapshotGateKey{}, gate))
			defer cancel()
			type result struct {
				report FTSDiagnosticReport
				err    error
			}
			done := make(chan result, 1)
			go func() { report, err := st.DiagnoseFTSContext(ctx, FTSDiagnosticOptions{}); done <- result{report, err} }()
			select {
			case <-gate.paused:
			case <-time.After(10 * time.Second):
				r.FailNow("boundary was not reached")
			}
			cancel()
			select {
			case got := <-done:
				r.ErrorIs(got.err, context.Canceled)
				a.False(got.report.Complete)
				a.Equal("incomplete", got.report.Outcome)
				a.Zero(got.report.Checked)
				r.NotEmpty(got.report.NextCursor)
				retry, err := st.DiagnoseFTSContext(t.Context(), FTSDiagnosticOptions{Cursor: got.report.NextCursor})
				r.NoError(err)
				r.Len(retry.Entries, 1)
				a.Equal(id, retry.Entries[0].MessageID, "interrupted row remains eligible")
			case <-time.After(10 * time.Second):
				r.FailNow("cancellation did not stop the diagnostic")
			}
		})
	}
}

func TestFTSDiagnosticTimeout(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, gate := newFTSDiagnosticGateStore(t, "FROM message_bodies WHERE")
	seedFTSDiagnosticGateMessage(t, st)
	ctx := context.WithValue(t.Context(), gmailAuditSnapshotGateKey{}, gate)
	got, err := st.DiagnoseFTSContext(ctx, FTSDiagnosticOptions{Timeout: time.Second})
	require.ErrorIs(err, context.DeadlineExceeded)
	assert.Equal("timeout", got.Reason)
	assert.False(got.Complete)
	assert.NotEmpty(got.NextCursor)
}

func TestFTSDiagnosticCancellationPreservesCompletedPrefix(t *testing.T) {
	a, r := assert.New(t), require.New(t)
	st, gate := newFTSDiagnosticGateStore(t, "FROM message_bodies WHERE")
	first := seedFTSDiagnosticGateMessage(t, st)
	var sourceID, conversationID int64
	r.NoError(st.db.QueryRowContext(t.Context(), `SELECT source_id, conversation_id FROM messages WHERE id = ?`, first).Scan(&sourceID, &conversationID))
	second, err := st.UpsertMessageContext(t.Context(), &Message{SourceID: sourceID, ConversationID: conversationID, SourceMessageID: "second", MessageType: "email"})
	r.NoError(err)
	r.NoError(st.IndexStoredMessageContext(t.Context(), second))
	gate.targetID = second
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), gmailAuditSnapshotGateKey{}, gate))
	defer cancel()
	type result struct {
		report FTSDiagnosticReport
		err    error
	}
	done := make(chan result, 1)
	go func() { report, err := st.DiagnoseFTSContext(ctx, FTSDiagnosticOptions{}); done <- result{report, err} }()
	select {
	case <-gate.paused:
	case <-time.After(10 * time.Second):
		r.FailNow("second message read not reached")
	}
	cancel()
	select {
	case got := <-done:
		r.ErrorIs(got.err, context.Canceled)
		r.Len(got.report.Entries, 1)
		a.Equal(first, got.report.Entries[0].MessageID)
		retry, err := st.DiagnoseFTSContext(t.Context(), FTSDiagnosticOptions{Cursor: got.report.NextCursor})
		r.NoError(err)
		r.Len(retry.Entries, 1)
		a.Equal(second, retry.Entries[0].MessageID)
	case <-time.After(10 * time.Second):
		r.FailNow("diagnostic failed to cancel")
	}
}

func TestFTSDiagnosticSQLFailureAndUnavailable(t *testing.T) {
	a, r := assert.New(t), require.New(t)
	st, _ := newFTSDiagnosticGateStore(t, "unused")
	id := seedFTSDiagnosticGateMessage(t, st)
	// Real unavailable representation: the existing Store still remembers its
	// startup capability, so a subsequent point read fails instead of matching.
	statement := `DROP TABLE messages_fts`
	if st.IsPostgreSQL() {
		statement = `ALTER TABLE messages DROP COLUMN search_fts_fields`
	}
	_, err := st.db.ExecContext(t.Context(), statement)
	r.NoError(err)
	got, err := st.DiagnoseFTSContext(t.Context(), FTSDiagnosticOptions{})
	r.ErrorContains(err, "check failed; retry with --cursor")
	a.Equal("incomplete", got.Outcome)
	a.Zero(got.Matched)
	a.Zero(got.Checked)
	r.NotEmpty(got.NextCursor)
	st.fts5Available = false
	unavailable, err := st.DiagnoseFTSContext(t.Context(), FTSDiagnosticOptions{Cursor: got.NextCursor})
	r.NoError(err)
	a.False(unavailable.Complete)
	a.Equal("fts_unavailable", unavailable.Reason)
	a.Equal(id, unavailable.ThroughID)
}

func TestFTSDiagnosticDeletionAfterSelection(t *testing.T) {
	a, r := assert.New(t), require.New(t)
	marker := "NULL FROM messages m WHERE"
	if IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		marker = "indexing_version FROM messages"
	}
	st, gate := newFTSDiagnosticGateStore(t, marker)
	id := seedFTSDiagnosticGateMessage(t, st)
	ctx, cancel := context.WithTimeout(context.WithValue(t.Context(), gmailAuditSnapshotGateKey{}, gate), 15*time.Second)
	defer cancel()
	type result struct {
		report FTSDiagnosticReport
		err    error
	}
	done := make(chan result, 1)
	go func() { report, err := st.DiagnoseFTSContext(ctx, FTSDiagnosticOptions{}); done <- result{report, err} }()
	select {
	case <-gate.paused:
	case <-ctx.Done():
		r.FailNow("metadata read not reached")
	}
	_, err := st.db.ExecContext(t.Context(), `DELETE FROM messages WHERE id = ?`, id)
	r.NoError(err)
	gate.resume()
	select {
	case got := <-done:
		r.NoError(got.err)
		a.True(got.report.Complete)
		a.Equal("incomplete", got.report.Outcome)
		r.Len(got.report.Entries, 1)
		a.Equal("message_disappeared", got.report.Entries[0].Reason)
	case <-ctx.Done():
		r.FailNow("diagnostic did not complete")
	}
}
