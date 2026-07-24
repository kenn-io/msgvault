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

	readiness, err := query.InspectCacheReadiness(dir)
	require.NoError(err, "InspectCacheReadiness after refresh")
	assert.Equal(query.CacheReady, readiness, "a refresh of a healthy cache must republish a fully consistent one")

	assertNoIdentityPublishLitter(t, dir)
}

// TestRefreshIdentityDatasetsRejectsDriftedMessageShard covers the
// integrity guard: a message shard modified outside the ETL after the last
// publication makes the cache detectably drifted (its fingerprint no longer
// matches the commit marker). An identity-only refresh must reject rather
// than recompute and commit a fingerprint over the drifted tree, which
// would stamp the damage as valid and hide it from readers.
func TestRefreshIdentityDatasetsRejectsDriftedMessageShard(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := storetest.New(t)
	st := f.Store
	a := f.EnsureParticipant("alice@example.com", "Alice", "example.com")
	b := f.EnsureParticipant("alice@personal.example", "Alice P", "personal.example")

	dir := writeCacheStatsFixture(t)
	messagesFile := filepath.Join(dir, tableMessages, "year=2024", "data.parquet")
	require.NoError(os.WriteFile(messagesFile, []byte("drifted outside the ETL"), 0o600),
		"corrupt messages shard after publication")

	before, err := query.ReadCacheSyncState(dir)
	require.NoError(err, "ReadCacheSyncState before refresh")

	_, err = st.LinkParticipants(a, b)
	require.NoError(err, "LinkParticipants")

	_, err = RefreshIdentityDatasets(context.Background(), st, dir)
	require.Error(err, "refresh over a drifted message shard must be rejected")
	require.ErrorIs(err, ErrCacheNotRefreshable)

	after, err := query.ReadCacheSyncState(dir)
	require.NoError(err, "ReadCacheSyncState after rejected refresh")
	assert.Equal(before.DatasetFingerprint, after.DatasetFingerprint,
		"rejected refresh must not rewrite the committed fingerprint")
	assert.Equal(before.IdentityRevision, after.IdentityRevision,
		"rejected refresh must not advance the committed identity revision")
	assert.Equal(before.PublishedAt, after.PublishedAt,
		"rejected refresh must not restamp PublishedAt")

	readiness, err := query.InspectCacheReadiness(dir)
	require.NoError(err, "InspectCacheReadiness after rejected refresh")
	assert.Equal(query.CacheDrifted, readiness,
		"the drift must remain detectable after the rejected refresh")
}

// TestRefreshIdentityDatasetsRejectsMissingNonIdentityDataset pins the
// narrow repair allowance: only missing identity datasets may be repaired
// by the identity refresh. A missing non-identity dataset — even alongside
// a missing identity dataset — is a non-identity interruption the refresh
// must not paper over with a fresh fingerprint.
func TestRefreshIdentityDatasetsRejectsMissingNonIdentityDataset(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := storetest.New(t)
	st := f.Store
	a := f.EnsureParticipant("alice@example.com", "Alice", "example.com")
	b := f.EnsureParticipant("alice@personal.example", "Alice P", "personal.example")

	dir := writeCacheStatsFixture(t)
	require.NoError(os.RemoveAll(filepath.Join(dir, "labels")), "remove labels dataset")
	require.NoError(os.RemoveAll(filepath.Join(dir, datasetParticipantClusters)), "remove clusters dataset")

	before, err := query.ReadCacheSyncState(dir)
	require.NoError(err, "ReadCacheSyncState before refresh")

	_, err = st.LinkParticipants(a, b)
	require.NoError(err, "LinkParticipants")

	_, err = RefreshIdentityDatasets(context.Background(), st, dir)
	require.Error(err, "refresh with a missing non-identity dataset must be rejected")
	require.ErrorIs(err, ErrCacheNotRefreshable)
	require.ErrorContains(err, "labels", "error must name the missing non-identity dataset")

	after, err := query.ReadCacheSyncState(dir)
	require.NoError(err, "ReadCacheSyncState after rejected refresh")
	assert.Equal(before.DatasetFingerprint, after.DatasetFingerprint,
		"rejected refresh must not rewrite the committed fingerprint")
	assert.Equal(before.IdentityRevision, after.IdentityRevision,
		"rejected refresh must not advance the committed identity revision")
}

// TestRefreshIdentityDatasetsRejectsInterruptedMarker rejects a commit
// marker that itself records an interrupted publication (no committed
// fingerprint): the refresh cannot know what the datasets should look like,
// so stamping a fresh fingerprint would legitimize whatever is on disk.
func TestRefreshIdentityDatasetsRejectsInterruptedMarker(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := storetest.New(t)
	st := f.Store
	a := f.EnsureParticipant("alice@example.com", "Alice", "example.com")
	b := f.EnsureParticipant("alice@personal.example", "Alice P", "personal.example")

	dir := writeCacheStatsFixture(t)
	state, err := query.ReadCacheSyncState(dir)
	require.NoError(err, "ReadCacheSyncState")
	state.DatasetFingerprint = ""
	writeCacheStatsState(t, dir, state)

	_, err = st.LinkParticipants(a, b)
	require.NoError(err, "LinkParticipants")

	_, err = RefreshIdentityDatasets(context.Background(), st, dir)
	require.Error(err, "refresh over an interrupted marker must be rejected")
	require.ErrorIs(err, ErrCacheNotRefreshable)
	assert.ErrorContains(err, "interrupted publication")
}

// TestRefreshIdentityDatasetsRejectsMissingIdentityDatasetAlone pins the
// force-rebuild behavior: even when only identity dataset directories are
// missing (no non-identity dataset missing), the refresh no longer repairs
// them in place. The committed DatasetFingerprint spans the whole tree, so
// once any dataset is absent it can no longer verify the remaining datasets
// are intact, and proceeding to republish would stamp a fresh fingerprint
// over a tree whose integrity was never actually confirmed.
func TestRefreshIdentityDatasetsRejectsMissingIdentityDatasetAlone(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := storetest.New(t)
	st := f.Store
	a := f.EnsureParticipant("alice@example.com", "Alice", "example.com")
	b := f.EnsureParticipant("alice@personal.example", "Alice P", "personal.example")

	dir := writeCacheStatsFixture(t)
	require.NoError(os.RemoveAll(filepath.Join(dir, datasetOwnerParticipants)), "remove owner_participants dataset")
	require.NoError(os.RemoveAll(filepath.Join(dir, datasetParticipantClusters)), "remove participant_clusters dataset")

	before, err := query.ReadCacheSyncState(dir)
	require.NoError(err, "ReadCacheSyncState before refresh")

	_, err = st.LinkParticipants(a, b)
	require.NoError(err, "LinkParticipants")

	_, err = RefreshIdentityDatasets(context.Background(), st, dir)
	require.Error(err, "refresh over missing identity datasets alone must now be rejected")
	require.ErrorIs(err, ErrCacheNotRefreshable)

	after, err := query.ReadCacheSyncState(dir)
	require.NoError(err, "ReadCacheSyncState after rejected refresh")
	assert.Equal(before.DatasetFingerprint, after.DatasetFingerprint,
		"rejected refresh must not rewrite the committed fingerprint")
	assert.Equal(before.PublishedAt, after.PublishedAt,
		"rejected refresh must not restamp PublishedAt")
	assert.Equal(before.IdentityRevision, after.IdentityRevision,
		"rejected refresh must not advance the committed identity revision")

	_, ownerStatErr := os.Stat(filepath.Join(dir, datasetOwnerParticipants))
	assert.True(os.IsNotExist(ownerStatErr), "rejected refresh must not regenerate the missing owner_participants dataset")
}

// TestRefreshIdentityDatasetsRejectsMissingIdentityWithCorruptedNonIdentity
// is the regression test for the integrity hole the force-rebuild behavior
// closes: before this fix, a missing identity dataset short-circuited
// validation entirely (returning nil without ever checking the remaining
// datasets against the committed fingerprint), so a modified or truncated
// non-identity dataset sitting right next to it went completely unchecked —
// and the subsequent republish would recompute the whole-tree fingerprint
// over the corrupted tree and stamp it as valid, hiding the damage from
// readers that would otherwise detect it. The refresh must now reject
// before touching anything, leaving the committed marker and the corrupted
// file exactly as they were.
func TestRefreshIdentityDatasetsRejectsMissingIdentityWithCorruptedNonIdentity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	f := storetest.New(t)
	st := f.Store
	a := f.EnsureParticipant("alice@example.com", "Alice", "example.com")
	b := f.EnsureParticipant("alice@personal.example", "Alice P", "personal.example")

	dir := writeCacheStatsFixture(t)
	require.NoError(os.RemoveAll(filepath.Join(dir, datasetOwnerParticipants)),
		"remove owner_participants identity dataset")
	labelsFile := filepath.Join(dir, "labels", "data.parquet")
	require.NoError(os.WriteFile(labelsFile, []byte("corrupted outside the ETL"), 0o600),
		"corrupt the labels dataset without removing it, so it still counts as present")

	before, err := query.ReadCacheSyncState(dir)
	require.NoError(err, "ReadCacheSyncState before refresh")

	_, err = st.LinkParticipants(a, b)
	require.NoError(err, "LinkParticipants")

	_, err = RefreshIdentityDatasets(context.Background(), st, dir)
	require.Error(err, "refresh over a missing identity dataset plus a corrupted non-identity dataset must be rejected")
	require.ErrorIs(err, ErrCacheNotRefreshable)
	require.ErrorContains(err, "identity dataset", "error must explain the missing identity dataset forced the rebuild")

	after, err := query.ReadCacheSyncState(dir)
	require.NoError(err, "ReadCacheSyncState after rejected refresh")
	assert.Equal(before.DatasetFingerprint, after.DatasetFingerprint,
		"rejected refresh must not rewrite the committed fingerprint over the corrupted tree")
	assert.Equal(before.PublishedAt, after.PublishedAt,
		"rejected refresh must not restamp PublishedAt")
	assert.Equal(before.IdentityRevision, after.IdentityRevision,
		"rejected refresh must not advance the committed identity revision")

	_, ownerStatErr := os.Stat(filepath.Join(dir, datasetOwnerParticipants))
	assert.True(os.IsNotExist(ownerStatErr), "rejected refresh must not regenerate the missing identity dataset")
	corrupted, err := os.ReadFile(labelsFile)
	require.NoError(err, "read labels file after rejected refresh")
	assert.Equal([]byte("corrupted outside the ETL"), corrupted,
		"rejected refresh must not touch or re-legitimize the corrupted non-identity dataset")
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

// TestPublishIdentityDatasetsRollbackRemovesNewlyCreatedIdentityDataset is
// the end-to-end proof for the rollback finding, exercised directly against
// publishIdentityDatasets: RefreshIdentityDatasets's
// validateCommittedPublication now rejects any missing dataset (see
// TestRefreshIdentityDatasetsRejectsMissingIdentityDatasetAlone) before ever
// reaching publish, so a missing identity dataset can no longer reach this
// code through that public entry point. The underlying rollback primitive
// still needs to behave correctly, though — a live dataset with no prior
// version to restore is exactly the state a failed rollback or an external
// deletion can leave behind — so this test calls publishIdentityDatasets
// directly with that state prepared.
//
// It starts from one identity dataset directory absent while the committed
// marker is retained, then faults the marker commit (the last publication
// step) so publish rolls back after both datasets have been swapped into
// place. Rollback must REMOVE the identity dataset it just created (the one
// with no prior live version to restore), not strand it: a stranded dataset
// the retained old marker's fingerprint never accounted for would read as
// unaccounted-for drift on the next refresh. The dataset that did have a
// prior version must be restored from its backup, the old marker must be
// unchanged, and a subsequent publish must repair the still-missing dataset
// and succeed.
func TestPublishIdentityDatasetsRollbackRemovesNewlyCreatedIdentityDataset(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	dir := writeCacheStatsFixture(t)
	ownerDir := filepath.Join(dir, datasetOwnerParticipants)
	clustersDir := filepath.Join(dir, datasetParticipantClusters)
	require.NoError(os.RemoveAll(ownerDir),
		"remove owner_participants to mirror a prior interrupted refresh")

	before, err := query.ReadCacheSyncState(dir)
	require.NoError(err, "ReadCacheSyncState before publish")
	nextRevision := before.IdentityRevision + 1

	staging := stageIdentityDatasets(t, dir)
	disarm := failIdentityPublishRename(t, func(_, newpath string) bool {
		return newpath == query.CacheStatePath(dir)
	})

	err = publishIdentityDatasets(staging, dir, before, nextRevision)
	require.Error(err, "publish with the marker commit faulted must fail")
	require.ErrorContains(err, "injected")
	require.NoError(staging.cleanup(), "clean up first staging attempt, mirroring RefreshIdentityDatasets's deferred cleanup")

	_, ownerStatErr := os.Stat(ownerDir)
	assert.True(os.IsNotExist(ownerStatErr),
		"rollback must remove the newly created owner_participants dataset (no prior version to restore)")
	_, clustersStatErr := os.Stat(clustersDir)
	require.NoError(clustersStatErr,
		"rollback must restore the previously present participant_clusters dataset from its backup")

	after, err := query.ReadCacheSyncState(dir)
	require.NoError(err, "committed marker must survive the failed publish")
	assert.Equal(before.IdentityRevision, after.IdentityRevision,
		"failed publish must keep the pre-refresh identity revision")
	assert.Equal(before.DatasetFingerprint, after.DatasetFingerprint,
		"failed publish must keep the pre-refresh fingerprint")

	disarm()
	retryStaging := stageIdentityDatasets(t, dir)
	require.NoError(publishIdentityDatasets(retryStaging, dir, before, nextRevision),
		"retry after the fault clears must repair the missing identity dataset and succeed")
	require.NoError(retryStaging.cleanup(), "clean up retry staging, mirroring RefreshIdentityDatasets's deferred cleanup")

	state, err := query.ReadCacheSyncState(dir)
	require.NoError(err, "ReadCacheSyncState after repair")
	assert.Equal(nextRevision, state.IdentityRevision, "repaired marker must carry the new identity revision")

	readiness, err := query.InspectCacheReadiness(dir)
	require.NoError(err, "InspectCacheReadiness after repair")
	assert.Equal(query.CacheReady, readiness, "repaired publication must be fully consistent")

	assertNoIdentityPublishLitter(t, dir)
}

// stageIdentityDatasets creates a fresh identity staging directory next to
// dir and writes a placeholder Parquet file into each staged identity
// dataset, mirroring what exportOwnerParticipants/exportParticipantClusters
// leave behind before publishIdentityDatasets runs. The staging content
// itself is not under test here (only the swap/rollback mechanics are), so
// the file contents are arbitrary; CacheDatasetFingerprint hashes file
// metadata, not Parquet contents, so a placeholder is sufficient. The
// caller must call staging.cleanup() itself once done with it — mirroring
// RefreshIdentityDatasets's own deferred cleanup, which runs before its
// caller can observe a litter-free analytics directory — instead of relying
// on this helper's t.Cleanup, which only runs after the test function
// returns and would leave staging/backup directories visible to a
// same-test litter check like assertNoIdentityPublishLitter. t.Cleanup here
// is only a safety net for a test that fails before reaching its own
// explicit cleanup call.
func stageIdentityDatasets(t *testing.T, dir string) *identityStaging {
	t.Helper()
	staging, err := newIdentityStaging(dir)
	require.NoError(t, err, "newIdentityStaging")
	t.Cleanup(func() { _ = staging.cleanup() })
	for _, dataset := range identityDatasets {
		path := filepath.Join(staging.datasetDir(dataset), dataset+".parquet")
		require.NoError(t, os.WriteFile(path, []byte("staged"), 0o600), "write staged %s placeholder", dataset)
	}
	return staging
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
