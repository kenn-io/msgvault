package backup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testManifest(created string, parent string, depth int) *Manifest {
	return &Manifest{
		FormatVersion:    FormatVersion,
		MinReaderVersion: MinReaderVersion,
		AppVersion:       "test",
		ParentID:         parent,
		CreatedAt:        created,
		DB: ManifestDB{
			Engine: "sqlite", PageSize: 4096, PageCount: 10,
			PageMap:       blobID("map-" + created).String(),
			PageHashMap:   blobID("hash-" + created).String(),
			MapChainDepth: depth,
		},
		Attachments: ManifestAttachments{Layout: []string{"loose"}},
		Excluded:    []string{"vectors.db", "analytics/", "logs/", "imports/", "tmp/", "locks"},
	}
}

func TestSnapshotIDDeterministicAndContentDerived(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	created := time.Date(2026, 7, 3, 12, 30, 15, 0, time.UTC)
	m := testManifest("2026-07-03T12:30:15Z", "", 0)
	id1, err := ComputeSnapshotID(created, m)
	require.NoError(err)
	id2, err := ComputeSnapshotID(created, m)
	require.NoError(err)
	assert.Equal(id1, id2)
	assert.Regexp(`^20260703T123015Z-[0-9a-f]{32}$`, id1)

	changed := testManifest("2026-07-03T12:30:15Z", "", 0)
	changed.Stats = json.RawMessage(`{"messages":42}`)
	id3, err := ComputeSnapshotID(created, changed)
	require.NoError(err)
	assert.NotEqual(id1, id3)

	// A pre-set SnapshotID must not influence the hash.
	preset := testManifest("2026-07-03T12:30:15Z", "", 0)
	preset.SnapshotID = "bogus"
	id4, err := ComputeSnapshotID(created, preset)
	require.NoError(err)
	assert.Equal(id1, id4)
}

func TestWriteListLoadLatest(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)

	latest, err := r.LatestSnapshot()
	require.NoError(err)
	assert.Nil(latest)

	id1, err := r.WriteManifest(testManifest("2026-07-01T00:00:00Z", "", 0))
	require.NoError(err)
	id2, err := r.WriteManifest(testManifest("2026-07-02T00:00:00Z", id1, 1))
	require.NoError(err)

	list, err := r.ListSnapshots()
	require.NoError(err)
	require.Len(list, 2)
	assert.Equal(id1, list[0].SnapshotID)
	assert.Equal(id2, list[1].SnapshotID)

	got, err := r.LoadManifest(id1)
	require.NoError(err)
	assert.Equal("2026-07-01T00:00:00Z", got.CreatedAt)

	latest, err = r.LatestSnapshot()
	require.NoError(err)
	require.NotNil(latest)
	assert.Equal(id2, latest.SnapshotID)

	_, statErr := filepath.Glob(r.Path("snapshots", "*.mvmanifest"))
	require.NoError(statErr)
}

func TestChains(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)

	keyID, err := r.WriteManifest(testManifest("2026-07-01T00:00:00Z", "", 0))
	require.NoError(err)
	d1ID, err := r.WriteManifest(testManifest("2026-07-02T00:00:00Z", keyID, 1))
	require.NoError(err)
	head, err := r.LoadManifest(d1ID)
	require.NoError(err)

	chain, err := r.HashMapChain(head)
	require.NoError(err)
	require.Len(chain, 2)
	assert.Equal(head.DB.PageHashMap, chain[0].String())

	mapChain, err := r.PageMapChain(head)
	require.NoError(err)
	require.Len(mapChain, 2)

	// Broken chain: parent manifest missing.
	orphan := testManifest("2026-07-03T00:00:00Z", "20990101T000000Z-deadbeef", 1)
	orphanID, err := r.WriteManifest(orphan)
	require.NoError(err)
	loaded, err := r.LoadManifest(orphanID)
	require.NoError(err)
	_, err = r.HashMapChain(loaded)
	require.Error(err)
}

// TestMapChainCycleRejectedAtLoad replaces the old self-cycle walk test:
// LoadManifest now recomputes the content-derived snapshot ID, so a forged
// self-referencing manifest (whose filename cannot be a SHA-256 fixed point
// over its own content) is rejected at load, before mapChain ever walks it.
func TestMapChainCycleRejectedAtLoad(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)

	m := testManifest("2026-07-01T00:00:00Z", "", 0)
	m.SnapshotID = "self-cycle-20260701-abcd1234"
	m.ParentID = "self-cycle-20260701-abcd1234"
	m.DB.MapChainDepth = 1

	data, err := json.MarshalIndent(m, "", "  ")
	require.NoError(err)

	err = writeFileAtomic(r, filepath.Join(snapshotsDirName, m.SnapshotID+manifestExt), data)
	require.NoError(err)

	_, err = r.LoadManifest(m.SnapshotID)
	require.ErrorContains(err, "content-derived ID check")
}

func TestMapChainExceedsDepthLimit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)

	// Build a chain that exceeds keyframeChainMax + 1 without hitting a keyframe.
	// keyframeChainMax = 30, so we need a chain of 31+ to exceed.
	// Create 32 manifests: manifest 31 is the keyframe (depth 0),
	// manifest 30 has depth 1 pointing to manifest 31,
	// ..., manifest 0 has depth 31 pointing to manifest 1.
	// Walking from manifest 0 should exceed the depth limit.

	// Create from oldest (manifest 31) to newest (manifest 0) via
	// WriteManifest, so every manifest carries a genuine content-derived ID
	// and passes LoadManifest's integrity check; the chain itself is still
	// bogus (each depth claims one more hop than keyframeChainMax allows).
	ids := make([]string, 32)
	for i := 31; i >= 0; i-- {
		var parent string
		var depth int
		if i < 31 {
			parent = ids[i+1] //nolint:gosec // i+1 is always < 32 due to loop bounds
			depth = 31 - i
		}
		id, err := r.WriteManifest(testManifest("2026-07-01T00:00:00Z", parent, depth))
		require.NoError(err)
		ids[i] = id
	}

	// Load manifest 0 (the head) and try to walk the chain.
	loaded, err := r.LoadManifest(ids[0])
	require.NoError(err)

	_, err = r.HashMapChain(loaded)
	require.Error(err)
	assert.Contains(err.Error(), "exceeds")
}

func TestLoadManifestRejectsNewerMinReaderVersion(t *testing.T) {
	require := require.New(t)
	repo := initTestRepo(t)

	m := testManifest("2026-07-03T12:30:15Z", "", 0)
	m.MinReaderVersion = SupportedReaderVersion + 1
	id, err := repo.WriteManifest(m)
	require.NoError(err)

	_, err = repo.LoadManifest(id)
	require.ErrorContains(err, "requires reader version")
	require.ErrorContains(err, "upgrade msgvault")
}

// TestLoadManifestRejectsCorruptedOrRenamedManifest pins LoadManifest's
// content-derived ID recomputation: a manifest whose body was edited after
// writing, or a valid manifest served under a different snapshot ID, must be
// rejected rather than silently accepted by list, latest, and verify.
func TestLoadManifestRejectsCorruptedOrRenamedManifest(t *testing.T) {
	require := require.New(t)
	repo := initTestRepo(t)

	id, err := repo.WriteManifest(testManifest("2026-07-03T12:30:15Z", "", 0))
	require.NoError(err)
	path := repo.Path(snapshotsDirName, id+manifestExt)
	original, err := os.ReadFile(path)
	require.NoError(err)

	// The untampered manifest round-trips.
	_, err = repo.LoadManifest(id)
	require.NoError(err)

	// Tamper: flip one stats field without recomputing the ID.
	var doctored Manifest
	require.NoError(json.Unmarshal(original, &doctored))
	doctored.Stats = json.RawMessage(`{"messages":1}`)
	data, err := json.MarshalIndent(&doctored, "", "  ")
	require.NoError(err)
	require.NoError(os.WriteFile(path, data, 0o600))
	_, err = repo.LoadManifest(id)
	require.ErrorContains(err, "content-derived ID check")

	// Rename: the pristine manifest body under a different snapshot ID.
	require.NoError(os.WriteFile(path, original, 0o600))
	renamed := "20260703T123015Z-deadbeef"
	require.NoError(os.Rename(path, repo.Path(snapshotsDirName, renamed+manifestExt)))
	_, err = repo.LoadManifest(renamed)
	require.ErrorContains(err, "content-derived ID check")

	// A garbled created_at is also corruption, reported as such.
	var badTime Manifest
	require.NoError(json.Unmarshal(original, &badTime))
	badTime.CreatedAt = "not-a-timestamp"
	data, err = json.MarshalIndent(&badTime, "", "  ")
	require.NoError(err)
	require.NoError(os.WriteFile(path, data, 0o600))
	_, err = repo.LoadManifest(id)
	require.ErrorContains(err, "created_at")
}

// TestLoadManifestRecomputesIDWithRawStats guards the json.RawMessage
// transition: manifests are stored indented, RawMessage preserves captured
// formatting, and the snapshot-ID recompute in LoadManifest must still
// match. encoding/json compacts RawMessage during Marshal, which is what
// makes this hold — this test is the proof.
func TestLoadManifestRecomputesIDWithRawStats(t *testing.T) {
	r, err := Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	m := &Manifest{
		FormatVersion:    FormatVersion,
		MinReaderVersion: MinReaderVersion,
		AppVersion:       "raw-stats-test",
		CreatedAt:        "2026-01-02T03:04:05Z",
		DB:               ManifestDB{Engine: "sqlite", PageSize: 4096, PageCount: 1},
		Attachments:      ManifestAttachments{Layout: []string{"loose"}, Recipes: []string{}},
		Excluded:         []string{},
		Stats: json.RawMessage(`{"messages":1,"conversations":0,"sources":0,` +
			`"accounts":0,"attachment_rows":0,"attachment_blobs":0,"labels":0,` +
			`"date_range":["",""]}`),
	}
	id, err := r.WriteManifest(m)
	require.NoError(t, err)
	loaded, err := r.LoadManifest(id) // internal ID recompute IS the assertion
	require.NoError(t, err)
	assert.Equal(t, id, loaded.SnapshotID)
}
