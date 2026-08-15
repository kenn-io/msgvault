package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type dailyNoteListGateKey struct{}

type dailyNoteListGate struct {
	mu          sync.Mutex
	queryCount  int
	firstClosed chan struct{}
	allowSecond chan struct{}
	closeFirst  sync.Once
	release     sync.Once
}

func newDailyNoteListGate() *dailyNoteListGate {
	return &dailyNoteListGate{
		firstClosed: make(chan struct{}),
		allowSecond: make(chan struct{}),
	}
}

func (g *dailyNoteListGate) nextQuery() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.queryCount++
	return g.queryCount
}

func (g *dailyNoteListGate) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.queryCount
}

func (g *dailyNoteListGate) releaseSecond() {
	g.release.Do(func() { close(g.allowSecond) })
}

type dailyNoteGateConnector struct {
	driver.Connector

	gate *dailyNoteListGate
}

func (c *dailyNoteGateConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &dailyNoteGateConn{Conn: conn, gate: c.gate}, nil
}

type dailyNoteGateConn struct {
	driver.Conn

	gate *dailyNoteListGate
}

func (c *dailyNoteGateConn) QueryContext(
	ctx context.Context, query string, args []driver.NamedValue,
) (driver.Rows, error) {
	queryer, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	gate, armed := ctx.Value(dailyNoteListGateKey{}).(*dailyNoteListGate)
	if !armed || gate != c.gate {
		return queryer.QueryContext(ctx, query, args)
	}
	queryNumber := gate.nextQuery()
	if queryNumber == 2 {
		select {
		case <-gate.allowSecond:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	rows, err := queryer.QueryContext(ctx, query, args)
	if err != nil || queryNumber != 1 {
		return rows, err
	}
	return &dailyNoteGateRows{Rows: rows, gate: gate}, nil
}

func (c *dailyNoteGateConn) ExecContext(
	ctx context.Context, query string, args []driver.NamedValue,
) (driver.Result, error) {
	if execer, ok := c.Conn.(driver.ExecerContext); ok {
		return execer.ExecContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

func (c *dailyNoteGateConn) PrepareContext(
	ctx context.Context, query string,
) (driver.Stmt, error) {
	if preparer, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return preparer.PrepareContext(ctx, query)
	}
	return c.Prepare(query)
}

func (c *dailyNoteGateConn) BeginTx(
	ctx context.Context, opts driver.TxOptions,
) (driver.Tx, error) {
	if beginner, ok := c.Conn.(driver.ConnBeginTx); ok {
		return beginner.BeginTx(ctx, opts)
	}
	return nil, errors.New("wrapped PostgreSQL connection does not implement ConnBeginTx")
}

func (c *dailyNoteGateConn) Ping(ctx context.Context) error {
	if pinger, ok := c.Conn.(driver.Pinger); ok {
		return pinger.Ping(ctx)
	}
	return nil
}

func (c *dailyNoteGateConn) ResetSession(ctx context.Context) error {
	if resetter, ok := c.Conn.(driver.SessionResetter); ok {
		return resetter.ResetSession(ctx)
	}
	return nil
}

func (c *dailyNoteGateConn) IsValid() bool {
	if validator, ok := c.Conn.(driver.Validator); ok {
		return validator.IsValid()
	}
	return true
}

func (c *dailyNoteGateConn) CheckNamedValue(value *driver.NamedValue) error {
	if checker, ok := c.Conn.(driver.NamedValueChecker); ok {
		return checker.CheckNamedValue(value)
	}
	return driver.ErrSkip
}

type dailyNoteGateRows struct {
	driver.Rows

	gate *dailyNoteListGate
}

func (r *dailyNoteGateRows) Close() error {
	err := r.Rows.Close()
	r.gate.closeFirst.Do(func() { close(r.gate.firstClosed) })
	return err
}

func newGatedPostgresStore(
	t *testing.T, dbURL string, gate *dailyNoteListGate,
) *Store {
	t.Helper()
	admin, err := sql.Open("pgx", dbURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })

	random := make([]byte, 8)
	_, err = rand.Read(random)
	require.NoError(t, err)
	schema := "msgvault_test_" + hex.EncodeToString(random)
	_, err = admin.ExecContext(t.Context(), "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	})

	separator := "?"
	if strings.Contains(dbURL, "?") {
		separator = "&"
	}
	testURL := dbURL + separator + "search_path=" + schema
	config, err := postgresConnConfig(testURL, false)
	require.NoError(t, err)
	baseConnector := stdlib.GetConnector(*config)
	db := sql.OpenDB(&dailyNoteGateConnector{Connector: baseConnector, gate: gate})
	db.SetMaxOpenConns(8)

	dialect := &PostgreSQLDialect{}
	st := &Store{
		db:      newLoggedDB(db, dialect.Rebind),
		dbPath:  testURL,
		dialect: dialect,
	}
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(t, st.InitSchema())
	return st
}

// TestDailyNotePersonListingUsesOneSnapshot observes production statements at
// the database driver boundary. If listing regresses to "page, then targets",
// the harness pauses the second statement until deletion has cascaded the
// target row; the statement count and returned target set both fail.
func TestDailyNotePersonListingUsesOneSnapshot(t *testing.T) {
	dbURL := os.Getenv("MSGVAULT_TEST_DB")
	if !IsPostgresURL(dbURL) {
		t.Skip("PostgreSQL DB-boundary interleaving test")
	}
	assert := assert.New(t)
	require := require.New(t)
	gate := newDailyNoteListGate()
	defer gate.releaseSecond()
	st := newGatedPostgresStore(t, dbURL, gate)

	firstParticipant, err := st.EnsureParticipant(
		"snapshot-first@example.invalid", "Test First", "example.invalid",
	)
	require.NoError(err)
	first, _, err := st.CreatePersonFromParticipant(firstParticipant)
	require.NoError(err)
	secondParticipant, err := st.EnsureParticipant(
		"snapshot-second@example.invalid", "Test Second", "example.invalid",
	)
	require.NoError(err)
	second, _, err := st.CreatePersonFromParticipant(secondParticipant)
	require.NoError(err)
	entry, err := st.CreateDailyNoteEntry(DailyNoteEntryInput{
		LocalDate: "2026-08-07",
		Body:      "snapshot entry",
		PersonIDs: []int64{first.ID, second.ID},
	})
	require.NoError(err)

	type listResult struct {
		entries []DailyNoteEntry
		err     error
	}
	result := make(chan listResult, 1)
	ctx, cancel := context.WithTimeout(
		context.WithValue(t.Context(), dailyNoteListGateKey{}, gate),
		5*time.Second,
	)
	defer cancel()
	go func() {
		entries, err := st.ListDailyNoteEntriesForPersonContext(
			ctx, first.ID, "2026-08-07", 10, 0,
		)
		result <- listResult{entries: entries, err: err}
	}()

	select {
	case <-gate.firstClosed:
	case <-time.After(5 * time.Second):
		require.FailNow("first listing statement did not finish")
	}
	require.NoError(st.DeletePerson(first.ID, first.Revision))
	gate.releaseSecond()

	select {
	case listed := <-result:
		require.NoError(listed.err)
		require.Len(listed.entries, 1)
		assert.Equal(entry.ID, listed.entries[0].ID)
		assert.Equal([]int64{first.ID, second.ID}, listed.entries[0].PersonIDs)
	case <-time.After(5 * time.Second):
		require.FailNow("listing did not finish")
	}
	assert.Equal(1, gate.count(),
		"listing issued %d database statements", gate.count())
}
