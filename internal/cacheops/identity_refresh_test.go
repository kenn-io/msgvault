package cacheops

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// TestRefreshIdentityDatasetsUpdatesClustersAndRevision pins the core
// contract: after LinkParticipants, a refresh writes the new cluster
// mapping and identity revision into the committed cache without touching
// the message datasets.
func TestRefreshIdentityDatasetsUpdatesClustersAndRevision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := storetest.New(t)
	st := f.Store
	a := f.EnsureParticipant("alice@example.com", "Alice", "example.com")
	b := f.EnsureParticipant("alice@personal.example", "Alice P", "personal.example")

	dir := writeCacheStatsFixture(t)
	messagesFile := filepath.Join(dir, tableMessages, "year=2024", "data.parquet")
	before, err := os.Stat(messagesFile)
	require.NoError(err, "stat messages fixture before refresh")

	wantRevision, err := st.LinkParticipants(a, b)
	require.NoError(err, "LinkParticipants")

	gotRevision, err := RefreshIdentityDatasets(context.Background(), st, dir)
	require.NoError(err, "RefreshIdentityDatasets")
	assert.Equal(wantRevision, gotRevision, "returned revision")

	storeRevision, err := st.IdentityRevision()
	require.NoError(err, "IdentityRevision")
	assert.Equal(storeRevision, gotRevision, "returned revision matches store")

	state, err := query.ReadCacheSyncState(dir)
	require.NoError(err, "ReadCacheSyncState")
	assert.Equal(storeRevision, state.IdentityRevision, "_last_sync.json identity_revision")
	assert.False(state.PublishedAt.IsZero(), "PublishedAt should be stamped")

	after, err := os.Stat(messagesFile)
	require.NoError(err, "stat messages fixture after refresh")
	assert.Equal(before.ModTime(), after.ModTime(), "message dataset mtime must be untouched")
	assert.Equal(before.Size(), after.Size(), "message dataset size must be untouched")

	clusters := readInt64PairsParquet(t, dir, datasetParticipantClusters, "participant_id", "canonical_id")
	want := map[int64]int64{a: min(a, b), b: min(a, b)}
	assert.Equal(want, clusters, "participant_clusters parquet")
}

// TestRefreshIdentityDatasetsPreservesAccountIdentityRevision covers Finding
// 1 of the relationships-backend review: RefreshIdentityDatasets only
// re-exports owner_participants/participant_clusters, never the message
// shards that bake in is_from_me, so it must leave the stamped
// AccountIdentityRevision untouched even while it advances IdentityRevision.
// A caller that advanced it here would make a stale is_from_me look fresh
// and permanently suppress the full rebuild account-identity drift needs.
func TestRefreshIdentityDatasetsPreservesAccountIdentityRevision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := storetest.New(t)
	st := f.Store
	a := f.EnsureParticipant("alice@example.com", "Alice", "example.com")
	b := f.EnsureParticipant("alice@personal.example", "Alice P", "personal.example")

	dir := writeCacheStatsFixture(t)
	// Simulate a prior full build that stamped a nonzero account-identity
	// revision (from an AddAccountIdentity/RemoveAccountIdentity call folded
	// into that build).
	priorState, err := query.ReadCacheSyncState(dir)
	require.NoError(err, "ReadCacheSyncState before mutating")
	priorState.AccountIdentityRevision = 7
	writeCacheStatsState(t, dir, priorState)

	wantRevision, err := st.LinkParticipants(a, b)
	require.NoError(err, "LinkParticipants")

	_, err = RefreshIdentityDatasets(context.Background(), st, dir)
	require.NoError(err, "RefreshIdentityDatasets")

	state, err := query.ReadCacheSyncState(dir)
	require.NoError(err, "ReadCacheSyncState after refresh")
	assert.Equal(wantRevision, state.IdentityRevision, "IdentityRevision must advance")
	assert.Equal(int64(7), state.AccountIdentityRevision,
		"AccountIdentityRevision must be preserved untouched by the identity-only refresh")
}

// TestRefreshIdentityDatasetsExportsOwnerParticipants verifies the
// owner_participants dataset reflects account_identities matched against
// participants, independent of the participant_clusters mapping.
func TestRefreshIdentityDatasetsExportsOwnerParticipants(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := storetest.New(t)
	st := f.Store
	owner := f.EnsureParticipant("me@example.com", "Me", "example.com")
	require.NoError(st.AddAccountIdentity(f.Source.ID, "me@example.com", "manual"), "AddAccountIdentity")

	dir := writeCacheStatsFixture(t)

	wantRevision, err := st.IdentityRevision()
	require.NoError(err, "IdentityRevision")

	revision, err := RefreshIdentityDatasets(context.Background(), st, dir)
	require.NoError(err, "RefreshIdentityDatasets")
	assert.Equal(wantRevision, revision, "returned revision matches store")

	owners := readInt64PairsParquet(t, dir, datasetOwnerParticipants, "source_id", "participant_id")
	assert.Equal(map[int64]int64{f.Source.ID: owner}, owners, "owner_participants parquet")
}

// TestRefreshIdentityDatasetsOwnerParticipantsMatchesNonEmailIdentifiersVerbatim
// pins ownerParticipantsSQL's non-email branch against the same contract
// cmd/msgvault/cmd/build_cache.go enforces for the full-build derivation
// (TestBuildCache_OwnerParticipantsMatchesNonEmailIdentifiersVerbatim): a
// confirmed non-email identifier (phone, chat handle) matches
// participant_identifiers verbatim, not case-insensitively. The two queries
// are documented as byte-equivalent aside from the ATTACH prefix; this test
// guards against the lightweight refresh path silently diverging from the
// full-build derivation it must match.
func TestRefreshIdentityDatasetsOwnerParticipantsMatchesNonEmailIdentifiersVerbatim(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := storetest.New(t)
	st := f.Store
	require.NoError(st.AddAccountIdentity(f.Source.ID, "+15550100001", "manual"), "confirm phone identity")
	require.NoError(st.AddAccountIdentity(f.Source.ID, "@user:matrix.org", "manual"), "confirm chat-handle identity")

	phoneOwnerID, err := st.EnsureParticipantByPhone("+15550100001", "Phone Owner", "phone")
	require.NoError(err)
	caseVariantID, err := st.EnsureParticipantByIdentifier("matrix", "@User:matrix.org", "Case Variant")
	require.NoError(err)

	dir := writeCacheStatsFixture(t)

	_, err = RefreshIdentityDatasets(context.Background(), st, dir)
	require.NoError(err, "RefreshIdentityDatasets")

	db, err := sql.Open("duckdb", "")
	require.NoError(err)
	defer func() { _ = db.Close() }()
	pattern := strings.ReplaceAll(filepath.Join(dir, datasetOwnerParticipants, "*.parquet"), "'", "''")

	var phoneRows int
	require.NoError(db.QueryRow(
		"SELECT COUNT(*) FROM read_parquet('"+pattern+"') WHERE participant_id = ?", phoneOwnerID,
	).Scan(&phoneRows))
	assert.Equal(1, phoneRows, "phone-identified owner must match verbatim")

	var caseVariantRows int
	require.NoError(db.QueryRow(
		"SELECT COUNT(*) FROM read_parquet('"+pattern+"') WHERE participant_id = ?", caseVariantID,
	).Scan(&caseVariantRows))
	assert.Equal(0, caseVariantRows, "a case-differing non-email identifier must not match verbatim")
}

// TestRefreshIdentityDatasetsSecondCallIsNoOpWhenRevisionUnchanged covers the
// no-op short-circuit: the identity link/unlink API always re-runs this
// refresh, even when the call changed nothing (a no-op link, or a retry
// after another writer already refreshed). Republishing unconditionally
// would rewrite the datasets and advance PublishedAt/the fingerprint for no
// reason, needlessly 409-ing active pagination cursors. A second call with
// no identity mutation in between must return the same revision and leave
// the committed datasets and PublishedAt untouched.
func TestRefreshIdentityDatasetsSecondCallIsNoOpWhenRevisionUnchanged(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := storetest.New(t)
	st := f.Store
	a := f.EnsureParticipant("alice@example.com", "Alice", "example.com")
	b := f.EnsureParticipant("alice@personal.example", "Alice P", "personal.example")

	dir := writeCacheStatsFixture(t)

	wantRevision, err := st.LinkParticipants(a, b)
	require.NoError(err, "LinkParticipants")

	firstRevision, err := RefreshIdentityDatasets(context.Background(), st, dir)
	require.NoError(err, "first RefreshIdentityDatasets")
	assert.Equal(wantRevision, firstRevision)

	stateAfterFirst, err := query.ReadCacheSyncState(dir)
	require.NoError(err, "ReadCacheSyncState after first refresh")

	clustersPath := filepath.Join(dir, datasetParticipantClusters, "participant_clusters.parquet")
	before, err := os.Stat(clustersPath)
	require.NoError(err, "stat clusters dataset after first refresh")

	secondRevision, err := RefreshIdentityDatasets(context.Background(), st, dir)
	require.NoError(err, "second RefreshIdentityDatasets")
	assert.Equal(wantRevision, secondRevision, "second call returns the same, unchanged revision")

	stateAfterSecond, err := query.ReadCacheSyncState(dir)
	require.NoError(err, "ReadCacheSyncState after second refresh")
	assert.Equal(stateAfterFirst.PublishedAt, stateAfterSecond.PublishedAt,
		"a no-op second refresh must not advance PublishedAt")
	assert.Equal(stateAfterFirst.DatasetFingerprint, stateAfterSecond.DatasetFingerprint,
		"a no-op second refresh must not change the dataset fingerprint")

	after, err := os.Stat(clustersPath)
	require.NoError(err, "stat clusters dataset after second refresh")
	assert.Equal(before.ModTime(), after.ModTime(), "a no-op second refresh must not rewrite the clusters dataset")
}

// TestRefreshIdentityDatasetsNoCommittedCache verifies the typed error path:
// refreshing against an analytics directory with no prior publication must
// fail clearly instead of fabricating a cache.
func TestRefreshIdentityDatasetsNoCommittedCache(t *testing.T) {
	f := storetest.New(t)
	dir := t.TempDir()

	_, err := RefreshIdentityDatasets(context.Background(), f.Store, dir)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoCommittedCache)
}

// readInt64PairsParquet reads a two-BIGINT-column dataset (any *.parquet
// file(s) under dir/dataset) into a map, for asserting against small
// identity dataset fixtures.
func readInt64PairsParquet(t *testing.T, dir, dataset, keyCol, valueCol string) map[int64]int64 {
	t.Helper()
	db, err := sql.Open("duckdb", "")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	pattern := strings.ReplaceAll(filepath.Join(dir, dataset, "*.parquet"), "'", "''")
	rows, err := db.Query("SELECT " + keyCol + ", " + valueCol + " FROM read_parquet('" + pattern + "')")
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	out := map[int64]int64{}
	for rows.Next() {
		var k, v int64
		require.NoError(t, rows.Scan(&k, &v))
		out[k] = v
	}
	require.NoError(t, rows.Err())
	return out
}
