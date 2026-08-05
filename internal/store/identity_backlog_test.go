package store_test

import (
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type backlogMarker struct {
	LastError string `json:"last_error"`
	FailedAt  string `json:"failed_at"`
	Attempts  int    `json:"attempts"`
}

// readBacklogMarker reads the raw archive_metadata row so the tests pin the
// durable on-disk contract, not just what the accessor chooses to return.
// The key is interpolated rather than bound because *sql.DB skips the store's
// dialect rebinding.
func readBacklogMarker(t *testing.T, st *store.Store, sourceID int64) backlogMarker {
	t.Helper()
	key := "identity_discovery_backlog:" + strconv.FormatInt(sourceID, 10)
	var value string
	require.NoError(t,
		st.DB().QueryRow(`SELECT value FROM archive_metadata WHERE key = '`+key+`'`).Scan(&value),
		"read backlog metadata row")
	var marker backlogMarker
	require.NoError(t, json.Unmarshal([]byte(value), &marker), "decode backlog marker JSON")
	return marker
}

// writeRawBacklogValue plants a marker value the production writer would never
// produce, so the tests can exercise what happens when the row has been
// hand-edited or written by an older format. The value is interpolated rather
// than bound because *sql.DB skips the store's dialect rebinding.
func writeRawBacklogValue(t *testing.T, st *store.Store, sourceID int64, value string) {
	t.Helper()
	key := "identity_discovery_backlog:" + strconv.FormatInt(sourceID, 10)
	_, err := st.DB().Exec(
		`INSERT INTO archive_metadata (key, value) VALUES ('` + key + `', '` + value + `')`)
	require.NoError(t, err, "seed raw backlog metadata row")
}

func newBacklogTestSource(t *testing.T) (*store.Store, int64) {
	t.Helper()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "alice@example.test")
	require.NoError(t, err, "GetOrCreateSource")
	return st, source.ID
}

func TestIdentityDiscoveryBacklogSetRecordsCause(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, sourceID := newBacklogTestSource(t)
	ctx := t.Context()

	require.NoError(
		st.SetIdentityDiscoveryBacklogContext(ctx, sourceID, errors.New("forced discovery failure")),
		"SetIdentityDiscoveryBacklogContext")

	found, lastError, err := st.IdentityDiscoveryBacklogContext(ctx, sourceID)
	require.NoError(err, "IdentityDiscoveryBacklogContext")
	assert.True(found, "marker is found after being set")
	assert.Equal("forced discovery failure", lastError)

	marker := readBacklogMarker(t, st, sourceID)
	assert.Equal("forced discovery failure", marker.LastError)
	assert.Equal(1, marker.Attempts, "first failure records one attempt")
	_, parseErr := time.Parse(time.RFC3339, marker.FailedAt)
	assert.NoError(parseErr, "failed_at is RFC3339")
}

func TestIdentityDiscoveryBacklogSetTwiceIncrementsAttempts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, sourceID := newBacklogTestSource(t)
	ctx := t.Context()

	require.NoError(
		st.SetIdentityDiscoveryBacklogContext(ctx, sourceID, errors.New("first failure")),
		"first SetIdentityDiscoveryBacklogContext")
	require.NoError(
		st.SetIdentityDiscoveryBacklogContext(ctx, sourceID, errors.New("second failure")),
		"second SetIdentityDiscoveryBacklogContext")

	marker := readBacklogMarker(t, st, sourceID)
	assert.Equal(2, marker.Attempts, "repeated failure increments attempts")
	assert.Equal("second failure", marker.LastError, "the most recent cause replaces the previous one")

	found, lastError, err := st.IdentityDiscoveryBacklogContext(ctx, sourceID)
	require.NoError(err, "IdentityDiscoveryBacklogContext")
	assert.True(found, "marker stays set")
	assert.Equal("second failure", lastError)
}

func TestIdentityDiscoveryBacklogClearRemovesMarker(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, sourceID := newBacklogTestSource(t)
	ctx := t.Context()
	require.NoError(
		st.SetIdentityDiscoveryBacklogContext(ctx, sourceID, errors.New("forced discovery failure")),
		"SetIdentityDiscoveryBacklogContext")

	require.NoError(st.ClearIdentityDiscoveryBacklogContext(ctx, sourceID),
		"ClearIdentityDiscoveryBacklogContext")

	found, lastError, err := st.IdentityDiscoveryBacklogContext(ctx, sourceID)
	require.NoError(err, "IdentityDiscoveryBacklogContext after clear")
	assert.False(found, "cleared marker is not found")
	assert.Empty(lastError, "cleared marker reports no error text")

	var rows int
	key := "identity_discovery_backlog:" + strconv.FormatInt(sourceID, 10)
	require.NoError(
		st.DB().QueryRow(`SELECT COUNT(*) FROM archive_metadata WHERE key = '`+key+`'`).Scan(&rows),
		"count backlog metadata rows")
	assert.Equal(0, rows, "clear deletes the metadata row")
}

func TestIdentityDiscoveryBacklogReportsNotFoundWhenNeverSet(t *testing.T) {
	st, sourceID := newBacklogTestSource(t)

	found, lastError, err := st.IdentityDiscoveryBacklogContext(t.Context(), sourceID)

	require.NoError(t, err, "reading an absent marker is not an error")
	assert.False(t, found, "never-set marker is not found")
	assert.Empty(t, lastError)
}

// TestIdentityDiscoveryBacklogSurfacesUndecodableMarker pins the deliberate
// fallback in the getter: the debt is real whether or not its payload parses,
// so refusing to read it would strand the very repair the marker exists for.
func TestIdentityDiscoveryBacklogSurfacesUndecodableMarker(t *testing.T) {
	st, sourceID := newBacklogTestSource(t)
	writeRawBacklogValue(t, st, sourceID, "not-a-json-marker")

	found, lastError, err := st.IdentityDiscoveryBacklogContext(t.Context(), sourceID)

	require.NoError(t, err, "an undecodable marker is a debt to drain, not a read failure")
	assert.True(t, found, "the drain must still run")
	assert.Equal(t, "not-a-json-marker", lastError,
		"the raw value is surfaced so the operator sees what is actually stored")
}

// TestIdentityDiscoveryBacklogRestartsAttemptsFromUnusablePriorValue pins the
// matching fallback in the writer: a prior value that yields no usable count
// restarts at one rather than failing the sync that is trying to record a
// failure.
func TestIdentityDiscoveryBacklogRestartsAttemptsFromUnusablePriorValue(t *testing.T) {
	tests := []struct {
		name  string
		prior string
	}{
		{name: "undecodable", prior: "not-a-json-marker"},
		{
			// A non-positive count must not be incremented into a still-wrong
			// one; zero would be indistinguishable from restarting, so the
			// case that actually pins the guard is a negative count.
			name:  "negative attempts",
			prior: `{"last_error":"old","failed_at":"2026-01-01T00:00:00Z","attempts":-5}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, sourceID := newBacklogTestSource(t)
			writeRawBacklogValue(t, st, sourceID, tt.prior)

			require.NoError(t,
				st.SetIdentityDiscoveryBacklogContext(t.Context(), sourceID, errors.New("fresh failure")),
				"SetIdentityDiscoveryBacklogContext")

			marker := readBacklogMarker(t, st, sourceID)
			assert.Equal(t, 1, marker.Attempts, "an unusable prior count restarts at one")
			assert.Equal(t, "fresh failure", marker.LastError, "the new cause replaces the unusable value")
		})
	}
}

func TestIdentityDiscoveryBacklogIsPerSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, sourceID := newBacklogTestSource(t)
	other, err := st.GetOrCreateSource("gmail", "bob@example.test")
	require.NoError(err, "GetOrCreateSource")
	ctx := t.Context()

	require.NoError(
		st.SetIdentityDiscoveryBacklogContext(ctx, sourceID, errors.New("alice failure")),
		"SetIdentityDiscoveryBacklogContext")

	found, _, err := st.IdentityDiscoveryBacklogContext(ctx, other.ID)
	require.NoError(err, "IdentityDiscoveryBacklogContext for the other source")
	assert.False(found, "one source's backlog must not shadow another's")

	require.NoError(st.ClearIdentityDiscoveryBacklogContext(ctx, other.ID),
		"clearing an absent marker is a no-op")
	found, _, err = st.IdentityDiscoveryBacklogContext(ctx, sourceID)
	require.NoError(err, "IdentityDiscoveryBacklogContext after unrelated clear")
	assert.True(found, "clearing another source leaves this marker intact")
}
