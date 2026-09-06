package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sqliteutil"
)

// This catches a decoder that allocates the caller-controlled base64 payload
// before enforcing the Directory cursor's encoded-size boundary.
func TestDecodeDirectoryPeopleCursorRejectsOversizedInputBeforeAllocation(t *testing.T) {
	cursor := strings.Repeat("a", maxDirectoryCursorBytes*8)
	_, err := decodeDirectoryPeopleCursor(cursor)
	require.ErrorIs(t, err, ErrInvalidDirectoryCursor)

	allocations := testing.AllocsPerRun(10, func() {
		_, _ = decodeDirectoryPeopleCursor(cursor)
	})
	assert.Zero(t, allocations)
}

// This catches a regression to a whole-directory candidate projection before
// pagination. The query must retain only the requested page plus its cursor
// row, even when the durable directory has substantially more people.
func TestSelectDirectoryPeopleTxBoundsLargeSyntheticDirectory(t *testing.T) {
	require := require.New(t)
	st, err := OpenForTest(filepath.Join(t.TempDir(), "directory.db"))
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(t, st.Close()) })
	require.NoError(st.InitSchema())

	ctx := context.Background()
	for index := range 256 {
		_, err := st.DB().ExecContext(ctx,
			`INSERT INTO persons (vcard_uid, display_name) VALUES (?, ?)`,
			fmt.Sprintf("directory-%03d", index), fmt.Sprintf("Person %03d", index),
		)
		require.NoError(err)
	}
	query, err := normalizeDirectoryPeopleQuery(DirectoryPeopleQuery{Limit: 2})
	require.NoError(err)
	require.NoError(st.refreshDirectoryProjectionsContext(ctx))

	var candidates []directoryPersonCandidate
	require.NoError(st.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var selectErr error
		candidates, selectErr = st.selectDirectoryPeopleTx(ctx, tx, query, nil)
		return selectErr
	}))
	require.Len(candidates, 3)
	assert.Equal(t, []int64{1, 2, 3}, []int64{
		candidates[0].summary.ID, candidates[1].summary.ID, candidates[2].summary.ID,
	})
}

// This catches an InitSchema upgrade of an archive that predates the
// Directory projection: the tables and triggers must be installed and every
// existing person indexed by a backfill that runs exactly once.
func TestInitSchemaBackfillsDirectoryProjectionOnce(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, err := OpenForTest(filepath.Join(t.TempDir(), "directory-backfill.db"))
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(st.Close()) })
	require.NoError(st.InitSchema())
	_, err = st.DB().Exec(`INSERT INTO persons (vcard_uid, display_name) VALUES ('directory-backfill', 'Backfill Person')`)
	require.NoError(err)
	dropDirectoryProjection(t, st)
	require.NoError(st.InitSchema())

	var orderKey string
	require.NoError(st.DB().QueryRow(`SELECT order_key FROM directory_people WHERE person_id = 1`).Scan(&orderKey))
	assert.Equal(directoryKey("backfill person"), orderKey)
	var dirty int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM directory_projection_dirty`).Scan(&dirty))
	assert.Zero(dirty)

	_, err = st.DB().Exec(`UPDATE directory_people SET order_key = ? WHERE person_id = 1`, directoryKey("untouched"))
	require.NoError(err)
	require.NoError(st.InitSchema())
	require.NoError(st.DB().QueryRow(`SELECT order_key FROM directory_people WHERE person_id = 1`).Scan(&orderKey))
	assert.Equal(directoryKey("untouched"), orderKey, "a second InitSchema must not rebuild the projection")
	var applied int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM applied_migrations WHERE name LIKE 'directory_projection%'`).Scan(&applied))
	assert.Equal(1, applied)
}

// dropDirectoryProjection returns a SQLite archive to the state before the
// Directory projection existed: no projection tables, triggers, or ledger row.
func dropDirectoryProjection(t *testing.T, st *Store) {
	t.Helper()
	triggers := directoryTriggerNames(t, st)
	require.NotEmpty(t, triggers)
	for _, name := range triggers {
		_, err := st.DB().Exec(`DROP TRIGGER ` + name)
		require.NoError(t, err)
	}
	for _, table := range []string{
		"directory_people", "directory_person_tokens", "directory_person_token_deletes",
		"directory_person_filters", "directory_projection_dirty",
	} {
		_, err := st.DB().Exec(`DROP TABLE ` + table)
		require.NoError(t, err)
	}
	_, err := st.DB().Exec(`DELETE FROM applied_migrations WHERE name = ?`, migrationDirectoryProjectionV1)
	require.NoError(t, err)
	st.directoryProjectionReady = false
}

func directoryTriggerNames(t *testing.T, st *Store) []string {
	t.Helper()
	rows, err := st.DB().Query(`SELECT name FROM sqlite_master WHERE type = 'trigger' AND name LIKE 'directory_dirty_%'`)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	names := make([]string, 0)
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		names = append(names, name)
	}
	require.NoError(t, rows.Err())
	return names
}

// This catches a writable archive reopen that leaves the Directory projection
// disabled even though its migrations and tables are already installed.
func TestOpenExistingDirectoryProjectionRefreshesDirtyRows(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	path := filepath.Join(t.TempDir(), "directory-reopen.db")
	seed, err := OpenForTest(path)
	require.NoError(err)
	require.NoError(seed.InitSchema())
	require.NoError(seed.Close())

	reopened, err := OpenForTest(path)
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(reopened.Close()) })
	_, err = reopened.DB().Exec(`INSERT INTO persons (vcard_uid, display_name) VALUES ('directory-reopen', 'Reopened Person')`)
	require.NoError(err)

	page, err := reopened.DirectoryPeoplePageContext(t.Context(), DirectoryPeopleQuery{})
	require.NoError(err)
	require.Len(page.People, 1)
	require.NotNil(page.People[0].DisplayName)
	assert.Equal("Reopened Person", *page.People[0].DisplayName)
}

// This catches SQLite inheriting the outer contact-state UPSERT conflict
// policy inside the Directory dirty trigger. A person already in the dirty
// queue must not turn a valid contact-state insertion into a duplicate error.
func TestDirectoryDirtyContactStateTriggerIgnoresPreexistingDirtyRowDuringUpsertSQLite(t *testing.T) {
	require := require.New(t)
	st, err := OpenForTest(filepath.Join(t.TempDir(), "directory-contact-upsert.db"))
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(t, st.Close()) })
	require.NoError(st.InitSchema())
	result, err := st.DB().Exec(`INSERT INTO persons (vcard_uid, display_name) VALUES ('directory-contact-upsert', 'Contact Person')`)
	require.NoError(err)
	personID, err := result.LastInsertId()
	require.NoError(err)
	_, err = st.DB().Exec(`DELETE FROM directory_projection_dirty WHERE person_id = ?`, personID)
	require.NoError(err)
	_, err = st.DB().Exec(`INSERT INTO person_contact_state (person_id, interaction_count) VALUES (?, 1)`, personID)
	require.NoError(err)
	_, err = st.DB().Exec(`INSERT OR IGNORE INTO directory_projection_dirty(person_id) VALUES (?)`, personID)
	require.NoError(err)

	err = applyDirectoryContactAddition(t.Context(), st, personID, 2)
	require.NoError(err)
	var interactionCount int64
	require.NoError(st.DB().QueryRow(`SELECT interaction_count FROM person_contact_state WHERE person_id = ?`, personID).Scan(&interactionCount))
	assert.Equal(t, int64(2), interactionCount)
}

func applyDirectoryContactAddition(ctx context.Context, st *Store, personID, messageID int64) error {
	return st.withTxContext(ctx, func(tx *loggedTx) error {
		return st.applyContactAdditionTx(ctx, tx, personID, ActivityEvent{
			MessageID: messageID, Channel: ChannelEmail,
			OccurredAt: time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC),
			Direction:  DirectionInbound,
		}, ContactRevisions{}, true)
	})
}

// This instruments the actual indexed token relation used by candidate
// selection; a full current-profile projection cannot satisfy this plan.
func TestDirectoryProjectionTokenLookupUsesIndex(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, err := OpenForTest(filepath.Join(t.TempDir(), "directory-plan.db"))
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(st.Close()) })
	require.NoError(st.InitSchema())
	for index := range 128 {
		_, err := st.DB().Exec(`INSERT INTO persons (vcard_uid, display_name) VALUES (?, ?)`, fmt.Sprintf("plan-%03d", index), fmt.Sprintf("Plan Person %03d", index))
		require.NoError(err)
	}
	require.NoError(st.refreshDirectoryProjectionsContext(context.Background()))
	rows, err := st.DB().Query(`EXPLAIN QUERY PLAN SELECT person_id FROM directory_person_tokens WHERE token_key = ?`, directoryKey("plan"))
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	var details []string
	for rows.Next() {
		var selectID, order, from int
		var detail string
		require.NoError(rows.Scan(&selectID, &order, &from, &detail))
		details = append(details, detail)
	}
	require.NoError(rows.Err())
	assert.Contains(strings.Join(details, "\n"), "idx_directory_person_tokens_lookup")
	rows, err = st.DB().Query(`EXPLAIN QUERY PLAN SELECT person_id FROM directory_person_tokens WHERE token_key >= ? AND token_key < ?`, directoryKey("plan"), directoryPrefixEnd(directoryKey("plan")))
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	details = nil
	for rows.Next() {
		var selectID, order, from int
		var detail string
		require.NoError(rows.Scan(&selectID, &order, &from, &detail))
		details = append(details, detail)
	}
	require.NoError(rows.Err())
	assert.Contains(strings.Join(details, "\n"), "idx_directory_person_tokens_lookup")
}

func TestReadOnlyDirectoryRejectsDirtyProjectionUntilWriterRefreshes(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "directory-reader.db")
	writer, err := OpenForTest(path)
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(t, writer.Close()) })
	require.NoError(writer.InitSchema())
	_, err = writer.DB().Exec(`INSERT INTO persons (vcard_uid, display_name) VALUES ('reader-person', 'Before Refresh')`)
	require.NoError(err)
	require.NoError(writer.RefreshDirectoryProjectionContext(context.Background()))
	_, err = writer.DB().Exec(`UPDATE persons SET display_name = 'After Refresh' WHERE id = 1`)
	require.NoError(err)
	reader, err := OpenReadOnly(path)
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(t, reader.Close()) })
	_, err = reader.DirectoryPeoplePageContext(context.Background(), DirectoryPeopleQuery{})
	require.ErrorIs(err, ErrDirectoryProjectionStale)
	require.NoError(writer.RefreshDirectoryProjectionContext(context.Background()))
	page, err := reader.DirectoryPeoplePageContext(context.Background(), DirectoryPeopleQuery{Query: "after"})
	require.NoError(err)
	require.Len(page.People, 1)
}

// This catches the freshness-check/use race at the actual database-driver
// boundary. The second connection commits after the serving transaction has
// qualified projection freshness but before it reads Directory rows. A
// correct implementation serves one internally consistent older snapshot;
// the next call observes or refreshes the dirty row.
func TestDirectoryFreshnessDecisionSharesServingSnapshot(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("read_only_%t", readOnly), func(t *testing.T) {
			require := require.New(t)
			path := filepath.Join(t.TempDir(), "directory-snapshot.db")
			writer, err := OpenForTest(path)
			require.NoError(err)
			t.Cleanup(func() { assert.NoError(t, writer.Close()) })
			require.NoError(writer.InitSchema())
			_, err = writer.DB().Exec(`INSERT INTO persons (vcard_uid, display_name) VALUES ('snapshot-person', 'Before Refresh')`)
			require.NoError(err)
			require.NoError(writer.RefreshDirectoryProjectionContext(t.Context()))

			gate := newDirectorySnapshotGate("SELECT EXISTS (SELECT 1 FROM directory_projection_dirty)")
			reader := newDirectorySnapshotGateStore(t, path, gate, readOnly)
			ctx, cancel := context.WithTimeout(context.WithValue(t.Context(), directorySnapshotGateKey{}, gate), 5*time.Second)
			defer cancel()

			type result struct {
				page *DirectoryPeoplePage
				err  error
			}
			results := make(chan result, 1)
			go func() {
				page, err := reader.DirectoryPeoplePageContext(ctx, DirectoryPeopleQuery{})
				results <- result{page: page, err: err}
			}()
			waitDirectorySnapshotSignal(t, gate.paused, "Directory freshness check did not pause")
			_, err = writer.DB().Exec(`UPDATE persons SET display_name = 'After Refresh' WHERE id = 1`)
			require.NoError(err)
			gate.release()

			got := waitDirectorySnapshotResult(t, results)
			require.NoError(got.err)
			require.Len(got.page.People, 1)
			require.NotNil(got.page.People[0].DisplayName)
			assert.Equal(t, "Before Refresh", *got.page.People[0].DisplayName)

			if readOnly {
				_, err = reader.DirectoryPeoplePageContext(t.Context(), DirectoryPeopleQuery{})
				require.ErrorIs(err, ErrDirectoryProjectionStale)
			} else {
				page, pageErr := reader.DirectoryPeoplePageContext(t.Context(), DirectoryPeopleQuery{Query: "after"})
				require.NoError(pageErr)
				require.Len(page.People, 1)
			}
		})
	}
}

func TestWritableDirectoryRefreshRetriesDirtySnapshotContention(t *testing.T) {
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "directory-dirty-contention.db")
	writer, err := OpenForTest(path)
	requirements.NoError(err)
	t.Cleanup(func() { assert.NoError(t, writer.Close()) })
	requirements.NoError(writer.InitSchema())
	_, err = writer.DB().Exec(`INSERT INTO persons (vcard_uid, display_name) VALUES ('dirty-contention', 'Initial Name')`)
	requirements.NoError(err)
	requirements.NoError(writer.RefreshDirectoryProjectionContext(t.Context()))
	_, err = writer.DB().Exec(`UPDATE persons SET display_name = 'First Change' WHERE id = 1`)
	requirements.NoError(err)

	gate := newDirectorySnapshotGate("SELECT EXISTS (SELECT 1 FROM directory_projection_dirty)")
	reader := newDirectorySnapshotGateStore(t, path, gate, false)
	ctx, cancel := context.WithTimeout(context.WithValue(t.Context(), directorySnapshotGateKey{}, gate), 5*time.Second)
	defer cancel()

	type result struct {
		page *DirectoryPeoplePage
		err  error
	}
	results := make(chan result, 1)
	go func() {
		page, err := reader.DirectoryPeoplePageContext(ctx, DirectoryPeopleQuery{})
		results <- result{page: page, err: err}
	}()
	waitDirectorySnapshotSignal(t, gate.paused, "Directory dirty check did not pause")
	_, err = writer.DB().Exec(`UPDATE persons SET display_name = 'Second Change' WHERE id = 1`)
	requirements.NoError(err)
	gate.release()

	got := waitDirectorySnapshotResult(t, results)
	requirements.NoError(got.err)
	requirements.Len(got.page.People, 1)
	requirements.NotNil(got.page.People[0].DisplayName)
	assert.Equal(t, "Second Change", *got.page.People[0].DisplayName)
}

type directorySnapshotGateKey struct{}

type directorySnapshotGate struct {
	query       string
	paused      chan struct{}
	releaseRead chan struct{}
	pauseOnce   sync.Once
	releaseOnce sync.Once
}

func (g *directorySnapshotGate) pause(ctx context.Context) {
	g.pauseOnce.Do(func() {
		close(g.paused)
		select {
		case <-g.releaseRead:
		case <-ctx.Done():
		}
	})
}

func newDirectorySnapshotGate(query string) *directorySnapshotGate {
	return &directorySnapshotGate{
		query: query, paused: make(chan struct{}), releaseRead: make(chan struct{}),
	}
}

func (g *directorySnapshotGate) release() {
	g.releaseOnce.Do(func() { close(g.releaseRead) })
}

type directorySnapshotConnector struct {
	driver driver.Driver
	dsn    string
	gate   *directorySnapshotGate
}

func (c *directorySnapshotConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.driver.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &directorySnapshotConn{Conn: conn, gate: c.gate}, nil
}

func (c *directorySnapshotConnector) Driver() driver.Driver { return c.driver }

type directorySnapshotConn struct {
	driver.Conn

	gate *directorySnapshotGate
}

func (c *directorySnapshotConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	queryer, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	rows, err := queryer.QueryContext(ctx, query, args)
	if err != nil || ctx.Value(directorySnapshotGateKey{}) != c.gate ||
		!strings.Contains(strings.Join(strings.Fields(query), " "), c.gate.query) {
		return rows, err
	}
	return &directorySnapshotRows{Rows: rows, ctx: ctx, gate: c.gate}, nil
}

func (c *directorySnapshotConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if execer, ok := c.Conn.(driver.ExecerContext); ok {
		result, err := execer.ExecContext(ctx, query, args)
		if err == nil && ctx.Value(directorySnapshotGateKey{}) == c.gate &&
			strings.Contains(strings.Join(strings.Fields(query), " "), c.gate.query) {
			c.gate.pause(ctx)
		}
		return result, err
	}
	return nil, driver.ErrSkip
}

func (c *directorySnapshotConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if preparer, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return preparer.PrepareContext(ctx, query)
	}
	return c.Prepare(query)
}

func (c *directorySnapshotConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if beginner, ok := c.Conn.(driver.ConnBeginTx); ok {
		return beginner.BeginTx(ctx, opts)
	}
	return nil, driver.ErrSkip
}

func (c *directorySnapshotConn) Ping(ctx context.Context) error {
	if pinger, ok := c.Conn.(driver.Pinger); ok {
		return pinger.Ping(ctx)
	}
	return nil
}

func (c *directorySnapshotConn) ResetSession(ctx context.Context) error {
	if resetter, ok := c.Conn.(driver.SessionResetter); ok {
		return resetter.ResetSession(ctx)
	}
	return nil
}

func (c *directorySnapshotConn) IsValid() bool {
	if validator, ok := c.Conn.(driver.Validator); ok {
		return validator.IsValid()
	}
	return true
}

func (c *directorySnapshotConn) CheckNamedValue(value *driver.NamedValue) error {
	if checker, ok := c.Conn.(driver.NamedValueChecker); ok {
		return checker.CheckNamedValue(value)
	}
	return driver.ErrSkip
}

type directorySnapshotRows struct {
	driver.Rows

	ctx  context.Context
	gate *directorySnapshotGate
}

func (r *directorySnapshotRows) Close() error {
	err := r.Rows.Close()
	r.gate.pause(r.ctx)
	return err
}

func newDirectorySnapshotGateStore(t *testing.T, path string, gate *directorySnapshotGate, readOnly bool) *Store {
	t.Helper()
	sqliteDriver := &sqlite3.SQLiteDriver{ConnectHook: func(conn *sqlite3.SQLiteConn) error {
		return conn.RegisterFunc(sqliteutil.UnicodeLowerFunction, strings.ToLower, true)
	}}
	connector := &directorySnapshotConnector{
		driver: sqliteDriver, dsn: path + testSQLiteParams, gate: gate,
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(4)
	dialect := &SQLiteDialect{}
	st := &Store{
		db: newLoggedDB(db, dialect.Rebind), dbPath: path, dialect: dialect,
		readOnly: readOnly, directoryProjectionReady: true,
	}
	t.Cleanup(func() { assert.NoError(t, st.Close()) })
	return st
}

func waitDirectorySnapshotSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		require.FailNow(t, message)
	}
}

func waitDirectorySnapshotResult[T any](t *testing.T, results <-chan T) T {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(5 * time.Second):
		require.FailNow(t, "Directory query did not finish")
		var zero T
		return zero
	}
}
