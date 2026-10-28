package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A REPEATABLE READ writer takes its snapshot before it waits for the
// identity fence. Another writer's committed clock update then makes this
// writer's own clock update fail serialization. That failure happens before
// the caller's work runs, so the Store retries it in a fresh transaction
// instead of handing an unrelated write's conflict to the caller.
func TestMCPEventsFenceRetriesRepeatableReadSerializationFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := newPGStoreInternal(t, skipUnlessPostgresInternal(t))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_, err := st.ConfigureMCPEvents(ctx, MCPEventsConfig{
		Enabled: true, Principal: "owner:synthetic",
		Capabilities: []MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}}},
	})
	require.NoError(err)

	writer, err := st.DB().BeginTx(ctx, nil)
	require.NoError(err)
	defer func() { _ = writer.Rollback() }()
	var writerPID int
	require.NoError(writer.QueryRowContext(ctx, `SELECT pg_backend_pid() FROM archive_metadata WHERE key=$1 FOR UPDATE`, identityRevisionKey).Scan(&writerPID))

	calls := 0
	done := make(chan error, 1)
	go func() {
		done <- st.withTxOptionsContext(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead}, func(*loggedTx) error {
			calls++
			return nil
		})
	}()
	waitForPGBlockedQuery(ctx, t, st, writerPID, "%FOR UPDATE%")
	_, err = writer.ExecContext(ctx, `UPDATE mcp_event_clock SET head_seq=head_seq WHERE singleton=1`)
	require.NoError(err)
	require.NoError(writer.Commit())

	select {
	case err = <-done:
	case <-ctx.Done():
		require.FailNow("repeatable-read writer did not finish")
	}
	require.NoError(err)
	assert.Equal(1, calls, "the caller's work runs once, after the retried fence")
}

// A process with capture off still checks the persisted switch before each
// write. A REPEATABLE READ writer that waited behind a daemon turning capture
// on must not trust its older snapshot: it must see capture on and record a
// gap by starting a new epoch.
func TestMCPEventsDisabledRepeatableReadWriterSeesCaptureEnabled(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := newPGStoreInternal(t, skipUnlessPostgresInternal(t))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_, err := st.ConfigureMCPEvents(ctx, MCPEventsConfig{})
	require.NoError(err)

	enabler, err := st.DB().BeginTx(ctx, nil)
	require.NoError(err)
	defer func() { _ = enabler.Rollback() }()
	var enablerPID int
	require.NoError(enabler.QueryRowContext(ctx, `SELECT pg_backend_pid() FROM archive_metadata WHERE key=$1 FOR UPDATE`, identityRevisionKey).Scan(&enablerPID))

	done := make(chan error, 1)
	go func() {
		done <- st.withTxOptionsContext(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead}, func(*loggedTx) error { return nil })
	}()
	waitForPGBlockedQuery(ctx, t, st, enablerPID, "%FOR KEY SHARE%")
	var enabledEpoch int64
	require.NoError(enabler.QueryRowContext(ctx, `UPDATE mcp_event_clock SET enabled=TRUE, capture_epoch=capture_epoch+1 WHERE singleton=1 RETURNING capture_epoch`).Scan(&enabledEpoch))
	require.NoError(enabler.Commit())

	select {
	case err = <-done:
	case <-ctx.Done():
		require.FailNow("repeatable-read writer did not finish")
	}
	require.NoError(err)
	var enabled bool
	var epoch int64
	require.NoError(st.DB().QueryRowContext(ctx, `SELECT enabled, capture_epoch FROM mcp_event_clock WHERE singleton=1`).Scan(&enabled, &epoch))
	assert.False(enabled, "the uncaptured write must turn capture off")
	assert.Equal(enabledEpoch+1, epoch, "the uncaptured write must start a new epoch")
}

// Source removal runs in a maintenance transaction outside the write fences.
// When it will lock the Events clock it must first take the identity fence,
// or it can deadlock with a capture-off writer that holds a key-share lock on
// identity and a share lock on the clock, then asks for identity again. The
// writer here issues the same statements a capture-off writer does; a Store
// keeps one Events runtime, so a second handle cannot be opened in-process.
func TestMCPEventsSourceRemovalDoesNotDeadlockCaptureOffWriter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := newPGStoreInternal(t, skipUnlessPostgresInternal(t))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_, err := st.ConfigureMCPEvents(ctx, MCPEventsConfig{
		Enabled: true, Principal: "owner:synthetic",
		Capabilities: []MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}}},
	})
	require.NoError(err)
	// Another process turned persisted capture off.
	_, err = st.DB().ExecContext(ctx, `UPDATE mcp_event_clock SET enabled=FALSE WHERE singleton=1`)
	require.NoError(err)
	source, err := st.GetOrCreateSource("gmail", "removed@example.test")
	require.NoError(err)

	writer, err := st.DB().BeginTx(ctx, nil)
	require.NoError(err)
	defer func() { _ = writer.Rollback() }()
	var writerPID int
	require.NoError(writer.QueryRowContext(ctx, `SELECT pg_backend_pid() FROM archive_metadata WHERE key=$1 FOR KEY SHARE`, identityRevisionKey).Scan(&writerPID))
	var enabled bool
	require.NoError(writer.QueryRowContext(ctx, `SELECT enabled FROM mcp_event_clock WHERE singleton=1 FOR SHARE`).Scan(&enabled))
	require.False(enabled)

	removed := make(chan error, 1)
	go func() { removed <- st.RemoveSource(source.ID) }()
	waitForPGBlockedQuery(ctx, t, st, writerPID, "%")
	_, err = writer.ExecContext(ctx, `SELECT value FROM archive_metadata WHERE key=$1 FOR SHARE`, identityRevisionKey)
	require.NoError(err, "the writer must not deadlock with source removal")
	require.NoError(writer.Commit())
	select {
	case err = <-removed:
	case <-ctx.Done():
		require.FailNow("source removal did not finish")
	}
	assert.NoError(err)
}

// CardDAV discovery replacement opens its own transaction, so it must follow
// the lock order of the other CardDAV writers on its handle. On a configured
// handle, fenced writers such as a book-role change take identity before the
// discovery lock; a capturing writer in another process does the same. On an
// unconfigured handle, writers are unfenced and take the discovery lock
// first. Each case pairs a replacement that changes the account identity with
// a writer that issues the matching statements in that order.
func TestMCPEventsCardDAVDiscoveryFollowsHandleLockOrder(t *testing.T) {
	enabled := MCPEventsConfig{
		Enabled: true, Principal: "owner:synthetic",
		Capabilities: []MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}}},
	}
	identityFirst := []string{
		`SELECT value FROM archive_metadata WHERE key='identity_revision' FOR UPDATE`,
		`SELECT singleton FROM carddav_discovery_lock WHERE singleton = 1 FOR UPDATE`,
	}
	discoveryFirst := []string{
		`SELECT singleton FROM carddav_discovery_lock WHERE singleton = 1 FOR UPDATE`,
		`UPDATE archive_metadata SET value = value WHERE key = 'identity_revision'`,
	}
	for _, tc := range []struct {
		name   string
		config *MCPEventsConfig
		writer []string
	}{
		{"capture on", &enabled, identityFirst},
		{"capture off", &MCPEventsConfig{}, identityFirst},
		{"unconfigured", nil, discoveryFirst},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := newPGStoreInternal(t, skipUnlessPostgresInternal(t))
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			if tc.config != nil {
				_, err := st.ConfigureMCPEvents(ctx, *tc.config)
				require.NoError(err)
			}
			discovery := func(username string) CardDAVDiscoveryInput {
				return CardDAVDiscoveryInput{
					BaseURL: "https://contacts.example/dav", Username: username,
					PrincipalURL: "https://contacts.example/principal/" + username + "/",
					HomeURL:      "https://contacts.example/books/" + username + "/",
					Books: []CardDAVDiscoveredBook{{
						CanonicalURL: "https://contacts.example/books/" + username + "/personal/",
						DisplayName:  "Personal", SupportsSyncCollection: true, SupportsMultiget: true,
					}},
				}
			}
			_, _, err := st.ReplaceCardDAVDiscoveryContext(ctx, discovery("user-a"))
			require.NoError(err)

			writer, err := st.DB().BeginTx(ctx, nil)
			require.NoError(err)
			defer func() { _ = writer.Rollback() }()
			var writerPID int
			require.NoError(writer.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&writerPID))
			_, err = writer.ExecContext(ctx, tc.writer[0])
			require.NoError(err)

			replaced := make(chan error, 1)
			go func() {
				_, _, err := st.ReplaceCardDAVDiscoveryContext(ctx, discovery("user-b"))
				replaced <- err
			}()
			waitForPGBlockedQuery(ctx, t, st, writerPID, "%")
			_, err = writer.ExecContext(ctx, tc.writer[1])
			require.NoError(err, "the writer must not deadlock with discovery replacement")
			require.NoError(writer.Commit())
			select {
			case err = <-replaced:
			case <-ctx.Done():
				require.FailNow("discovery replacement did not finish")
			}
			assert.NoError(err)
		})
	}
}
