package cacheops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

	assertNoIdentityPublishLitter(t, dir)
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

// TestPublishIdentityDatasetsFailureKeepsCommittedStateUsable injects a
// fault at each step of the publication sequence and pins the transactional
// contract: a failed publish must leave the previous committed publication
// fully usable (marker, datasets, and matching fingerprint — never
// ErrNoCommittedCache on retry), a retry after the fault clears must
// succeed and commit the new revision, and a completed refresh must leave
// no staging or backup litter behind.
func TestPublishIdentityDatasetsFailureKeepsCommittedStateUsable(t *testing.T) {
	cases := []struct {
		name   string
		inject func(t *testing.T, dir string) (disarm func())
	}{
		{
			name: "backing up first live dataset fails",
			inject: func(t *testing.T, dir string) func() {
				t.Helper()
				return failIdentityPublishRename(t, func(oldpath, _ string) bool {
					return oldpath == filepath.Join(dir, datasetOwnerParticipants)
				})
			},
		},
		{
			name: "publishing first staged dataset fails",
			inject: func(t *testing.T, dir string) func() {
				t.Helper()
				return failIdentityPublishRename(t, func(_, newpath string) bool {
					return newpath == filepath.Join(dir, datasetOwnerParticipants)
				})
			},
		},
		{
			name: "publishing second staged dataset fails",
			inject: func(t *testing.T, dir string) func() {
				t.Helper()
				return failIdentityPublishRename(t, func(_, newpath string) bool {
					return newpath == filepath.Join(dir, datasetParticipantClusters)
				})
			},
		},
		{
			name: "fingerprinting swapped datasets fails",
			inject: func(t *testing.T, dir string) func() {
				t.Helper()
				identityPublishFingerprint = func(string) (string, error) {
					return "", errors.New("injected fingerprint failure")
				}
				disarm := func() { identityPublishFingerprint = query.CacheDatasetFingerprint }
				t.Cleanup(disarm)
				return disarm
			},
		},
		{
			name: "staging new marker fails",
			inject: func(t *testing.T, dir string) func() {
				t.Helper()
				identityPublishWriteFile = func(string, []byte, os.FileMode) error {
					return errors.New("injected marker write failure")
				}
				disarm := func() { identityPublishWriteFile = os.WriteFile }
				t.Cleanup(disarm)
				return disarm
			},
		},
		{
			name: "committing new marker fails",
			inject: func(t *testing.T, dir string) func() {
				t.Helper()
				return failIdentityPublishRename(t, func(_, newpath string) bool {
					return newpath == query.CacheStatePath(dir)
				})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)

			f := storetest.New(t)
			st := f.Store
			a := f.EnsureParticipant("alice@example.com", "Alice", "example.com")
			b := f.EnsureParticipant("alice@personal.example", "Alice P", "personal.example")

			dir := writeCacheStatsFixture(t)
			before, err := query.ReadCacheSyncState(dir)
			require.NoError(err, "ReadCacheSyncState before refresh")

			wantRevision, err := st.LinkParticipants(a, b)
			require.NoError(err, "LinkParticipants")

			disarm := tc.inject(t, dir)
			_, err = RefreshIdentityDatasets(context.Background(), st, dir)
			require.Error(err, "refresh with injected fault must fail")
			require.ErrorContains(err, "injected")
			require.NotErrorIs(err, ErrNoCommittedCache)

			state, err := query.ReadCacheSyncState(dir)
			require.NoError(err, "committed marker must survive a failed publish")
			assert.Equal(before.IdentityRevision, state.IdentityRevision,
				"failed publish must keep the pre-refresh identity revision")
			assert.Equal(before.DatasetFingerprint, state.DatasetFingerprint,
				"failed publish must keep the pre-refresh fingerprint")
			readiness, err := query.InspectCacheReadiness(dir)
			require.NoError(err, "InspectCacheReadiness after failed publish")
			assert.Equal(query.CacheReady, readiness,
				"restored publication must stay fully consistent (datasets match the marker fingerprint)")

			disarm()
			gotRevision, err := RefreshIdentityDatasets(context.Background(), st, dir)
			require.NoError(err, "retry after the fault clears must succeed without a full rebuild")
			assert.Equal(wantRevision, gotRevision, "retry must return the linked revision")

			state, err = query.ReadCacheSyncState(dir)
			require.NoError(err, "ReadCacheSyncState after successful retry")
			assert.Equal(wantRevision, state.IdentityRevision, "retry must commit the new revision")

			clusters := readInt64PairsParquet(t, dir, datasetParticipantClusters, "participant_id", "canonical_id")
			want := map[int64]int64{a: min(a, b), b: min(a, b)}
			assert.Equal(want, clusters, "retry must publish the new participant_clusters data")

			assertNoIdentityPublishLitter(t, dir)
		})
	}
}

// failIdentityPublishRename swaps identityPublishRename for one that fails
// any rename matched by match and delegates the rest to os.Rename. The
// returned disarm restores the real rename; t.Cleanup also restores it in
// case a test fails before calling disarm.
func failIdentityPublishRename(t *testing.T, match func(oldpath, newpath string) bool) func() {
	t.Helper()
	identityPublishRename = func(oldpath, newpath string) error {
		if match(oldpath, newpath) {
			return fmt.Errorf("injected rename failure: %s -> %s", oldpath, newpath)
		}
		return os.Rename(oldpath, newpath)
	}
	disarm := func() { identityPublishRename = os.Rename }
	t.Cleanup(disarm)
	return disarm
}

// assertNoIdentityPublishLitter verifies a completed refresh left no
// staging directory (which also holds the dataset backups and the staged
// marker) next to the analytics dir and no backup directory inside it.
func assertNoIdentityPublishLitter(t *testing.T, dir string) {
	t.Helper()
	parent := filepath.Dir(filepath.Clean(dir))
	entries, err := os.ReadDir(parent)
	require.NoError(t, err, "list analytics parent directory")
	prefix := "." + filepath.Base(filepath.Clean(dir)) + ".identity-"
	for _, entry := range entries {
		assert.False(t, strings.HasPrefix(entry.Name(), prefix),
			"staging/backup directory %s must not survive a completed refresh", entry.Name())
	}
	_, err = os.Stat(filepath.Join(dir, "backup"))
	assert.True(t, os.IsNotExist(err), "no backup directory may be left inside the analytics dir")
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
