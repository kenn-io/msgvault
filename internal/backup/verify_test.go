package backup

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/pack"
)

func newTestVerifyState(t *testing.T, r *Repo, quick bool) *verifyState {
	t.Helper()
	known, err := r.LoadBlobIndex()
	require.NoError(t, err)
	st := &verifyState{
		repo:        r,
		known:       known,
		quick:       quick,
		jobs:        2,
		readers:     map[string]*pack.Reader{},
		readerErrs:  map[string]error{},
		checked:     map[pack.BlobID]bool{},
		readDone:    map[pack.BlobID]bool{},
		readVerdict: map[pack.BlobID]string{},
		pendingSet:  map[pack.BlobID]bool{},
		result:      &VerifyResult{},
	}
	t.Cleanup(st.closeReaders)
	return st
}

func distinctPackIDs(t *testing.T, r *Repo) []string {
	t.Helper()
	known, err := r.LoadBlobIndex()
	require.NoError(t, err)
	var ids []string
	seen := map[string]bool{}
	for _, ie := range known {
		if !seen[ie.PackID] {
			seen[ie.PackID] = true
			ids = append(ids, ie.PackID)
		}
	}
	return ids
}

func buildVerifyFixture(t *testing.T) (*Repo, *Manifest) {
	t.Helper()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	m, err := Create(context.Background(), r, createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(t, err)
	return r, m
}

func TestVerifyCleanRepo(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r, m := buildVerifyFixture(t)

	for _, quick := range []bool{true, false} {
		res, err := Verify(context.Background(), r, VerifyOptions{Quick: quick})
		require.NoError(err)
		assert.Empty(res.Problems)
		assert.Equal([]string{m.SnapshotID}, res.Snapshots)
		assert.Positive(res.BlobsChecked)
	}
}

func TestVerifySelection(t *testing.T) {
	require := require.New(t)
	r, m := buildVerifyFixture(t)

	res, err := Verify(context.Background(), r, VerifyOptions{SnapshotID: m.SnapshotID, Quick: true})
	require.NoError(err)
	require.Equal([]string{m.SnapshotID}, res.Snapshots)

	_, err = Verify(context.Background(), r, VerifyOptions{SnapshotID: "20990101T000000Z-deadbeef"})
	require.Error(err)

	res, err = Verify(context.Background(), r, VerifyOptions{All: true, Quick: true})
	require.NoError(err)
	require.Equal([]string{m.SnapshotID}, res.Snapshots)
}

func TestVerifyNamesCorruptBlob(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r, m := buildVerifyFixture(t)

	// Corrupt one byte in the middle of a pack's blob region.
	packID := m.NewPacks[0]
	path := r.Path("packs", packID[:2], packID+".mvpack")
	data, err := os.ReadFile(path)
	require.NoError(err)
	data[len(data)/3] ^= 0x01
	require.NoError(os.WriteFile(path, data, 0o600))

	res, err := Verify(context.Background(), r, VerifyOptions{})
	require.NoError(err)
	require.NotEmpty(res.Problems)
	found := false
	for _, p := range res.Problems {
		assert.Equal(m.SnapshotID, p.SnapshotID)
		if assert.NotEmpty(p.Detail) && containsAll(p.Detail, packID) {
			found = true
		}
	}
	assert.True(found, "a problem must name the damaged pack")
}

// TestVerifyJobsSerialMatchesParallel pins the --jobs contract: Jobs=1 (the
// spinning-disk/NAS mode, packs read strictly one at a time) must produce
// the same verdicts, counts, and bytes as the default parallel drain — on a
// clean repository and on one with a corrupted blob.
func TestVerifyJobsSerialMatchesParallel(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r, m := buildVerifyFixture(t)

	run := func(jobs int) *VerifyResult {
		res, err := Verify(context.Background(), r, VerifyOptions{Jobs: jobs})
		require.NoError(err)
		return res
	}
	serial, parallel := run(1), run(8)
	assert.Empty(serial.Problems)
	assert.Empty(parallel.Problems)
	assert.Equal(serial.BlobsChecked, parallel.BlobsChecked)
	assert.Equal(serial.BytesRead, parallel.BytesRead)

	packID := m.NewPacks[0]
	path := r.Path("packs", packID[:2], packID+".mvpack")
	data, err := os.ReadFile(path)
	require.NoError(err)
	data[len(data)/3] ^= 0x01
	require.NoError(os.WriteFile(path, data, 0o600))

	serial, parallel = run(1), run(8)
	require.NotEmpty(serial.Problems)
	require.NotEmpty(parallel.Problems)
	assert.ElementsMatch(serial.Problems, parallel.Problems,
		"serial and parallel drains must report the same problem set")
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// TestVerifyFlagsPageMapGeometryMismatch pins that verify checks both axes
// of the materialized page map's geometry against the manifest. Restore
// refuses a page-size mismatch, so a verify that missed it would pass a
// snapshot that cannot be restored.
func TestVerifyFlagsPageMapGeometryMismatch(t *testing.T) {
	require := require.New(t)
	r, _ := buildVerifyFixture(t)
	st := newTestVerifyState(t, r, true)

	pm := &PageMap{
		PageSize:  8192,
		PageCount: 4,
		Blobs:     []pack.BlobID{blobID("b")},
		Runs:      []PageRun{{StartPage: 0, PageCount: 4, BlobIndex: 0, BlobOffset: 0}},
	}
	m := &Manifest{SnapshotID: "s", DB: ManifestDB{PageSize: 4096, PageCount: 4}}
	st.checkPageMapCoverage(m, pm)
	require.Len(st.result.Problems, 1)
	require.Contains(st.result.Problems[0].Detail, "page size 8192 disagrees with manifest page_size 4096")
}

// TestVerifyAllProgressIsMonotonic pins full-mode progress across a
// multi-snapshot --all run: each snapshot's drain grows the denominator
// cumulatively, Done never moves backward, and the final event closes out
// the same counters the bar advanced with.
func TestVerifyAllProgressIsMonotonic(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, writer := seedBackupFixture(t)
	cacheDir := t.TempDir()
	_, err := Create(ctx, r, createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)

	// The second snapshot brings new content blobs, so its drain queues work
	// beyond the first snapshot's.
	_, err = writer.ExecContext(ctx, `INSERT INTO messages (sent_at) VALUES ('2026-03-01T00:00:00Z')`)
	require.NoError(err)
	ref := writeLooseAttachment(t, attachmentsDir, []byte("content new to snapshot 2"))
	_, err = writer.ExecContext(ctx,
		`INSERT INTO attachments (content_hash, storage_path, size, thumbnail_hash, thumbnail_path)
		 VALUES (?, ?, ?, '', '')`,
		ref.Hash, ref.Hash[:2]+"/"+ref.Hash, ref.Size)
	require.NoError(err)
	_, err = Create(ctx, r, createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)

	var events []ProgressEvent
	res, err := Verify(ctx, r, VerifyOptions{All: true, Progress: func(ev ProgressEvent) {
		if ev.Stage == ProgressStageVerify {
			events = append(events, ev)
		}
	}})
	require.NoError(err)
	require.Empty(res.Problems)
	require.Len(res.Snapshots, 2)

	require.NotEmpty(events)
	prev := ProgressEvent{}
	for _, ev := range events {
		assert.GreaterOrEqual(ev.Done, prev.Done, "Done must never move backward across snapshot drains")
		assert.GreaterOrEqual(ev.Total, prev.Total, "Total only grows as later drains queue work")
		prev = ev
	}
	final := events[len(events)-1]
	require.True(final.Final)
	assert.Equal(final.Total, final.Done, "the final event reports the run complete")
	assert.Greater(final.Total, events[0].Total,
		"the second snapshot's drain must grow the cumulative denominator")
}

func TestVerifyQuickCatchesMissingPack(t *testing.T) {
	require := require.New(t)
	r, m := buildVerifyFixture(t)

	packID := m.NewPacks[0]
	require.NoError(os.Remove(r.Path("packs", packID[:2], packID+".mvpack")))

	res, err := Verify(context.Background(), r, VerifyOptions{Quick: true})
	require.NoError(err)
	require.NotEmpty(res.Problems)
}

func TestVerifyRefusesUnderExclusiveLock(t *testing.T) {
	require := require.New(t)
	r, _ := buildVerifyFixture(t)

	l, err := r.AcquireExclusiveLock("prune", false)
	require.NoError(err)
	defer func() { require.NoError(l.Release()) }()

	_, err = Verify(context.Background(), r, VerifyOptions{Quick: true})
	require.ErrorIs(err, ErrRepoLocked)
}

func TestVerifyEmptyRepo(t *testing.T) {
	r := initTestRepo(t)
	_, err := Verify(context.Background(), r, VerifyOptions{})
	require.Error(t, err)
}

// buildMultiPackRepo creates several snapshots, each adding a new attachment,
// so the repository holds at least the given number of distinct packs.
func buildMultiPackRepo(t *testing.T, minPacks int) *Repo {
	t.Helper()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, db := seedBackupFixture(t)
	cacheDir := t.TempDir()
	_, err := Create(context.Background(), r, createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(t, err)
	for i := 0; len(distinctPackIDs(t, r)) < minPacks; i++ {
		require.Less(t, i, 20, "expected distinct packs to accumulate")
		content := []byte(strings.Repeat("x", i+1) + "-attachment")
		ref := writeLooseAttachment(t, attachmentsDir, content)
		_, err = db.Exec(
			`INSERT INTO attachments (content_hash, storage_path, size, thumbnail_hash, thumbnail_path)
			 VALUES (?, ?, ?, '', '')`,
			ref.Hash, ref.Hash[:2]+"/"+ref.Hash, ref.Size)
		require.NoError(t, err)
		_, err = Create(context.Background(), r, createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
		require.NoError(t, err)
	}
	return r
}

// TestVerifyReaderCacheEvictsAndReopens pins Important 5: the pack-reader
// cache is a bounded LRU that closes the least-recently-used reader on
// overflow and reopens a fresh one when the pack is requested again.
func TestVerifyReaderCacheEvictsAndReopens(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	old := maxOpenPackReaders
	maxOpenPackReaders = 2
	t.Cleanup(func() { maxOpenPackReaders = old })

	r := buildMultiPackRepo(t, 3)
	packs := distinctPackIDs(t, r)
	require.GreaterOrEqual(len(packs), 3)

	st := newTestVerifyState(t, r, false)
	p0, err := st.reader(packs[0])
	require.NoError(err)
	_, err = st.reader(packs[1])
	require.NoError(err)
	require.Len(st.readers, 2)

	_, err = st.reader(packs[2]) // over the cap: evicts packs[0]
	require.NoError(err)
	assert.Len(st.readers, 2)
	_, present := st.readers[packs[0]]
	assert.False(present, "LRU evicted the least-recently-used reader")

	// The evicted reader was closed: reading through the stale handle fails.
	entries := p0.Entries()
	require.NotEmpty(entries)
	_, err = p0.ReadBlob(entries[0])
	require.Error(err, "evicted reader must be closed")

	// Re-requesting the evicted pack reopens a fresh reader.
	p0b, err := st.reader(packs[0])
	require.NoError(err)
	assert.NotSame(p0, p0b, "re-request reopened a new reader instance")
	assert.Len(st.readers, 2)
	_, present = st.readers[packs[1]]
	assert.False(present, "reopening evicted the next least-recently-used reader")
}

// TestVerifyMemoizesSharedContentReads pins Important 6: a content blob shared
// by several snapshots is fully read once; later snapshots reuse the verdict.
func TestVerifyMemoizesSharedContentReads(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	cacheDir := t.TempDir()

	m1, err := Create(context.Background(), r, createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)
	// No source changes: snapshot 2 shares every content blob with snapshot 1.
	m2, err := Create(context.Background(), r, createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)

	ctx := context.Background()
	st1 := newTestVerifyState(t, r, false)
	st1.verifySnapshot(m1)
	require.NoError(st1.drainContentReads(ctx))
	require.Empty(st1.result.Problems)
	assert.Positive(st1.contentReads)

	st2 := newTestVerifyState(t, r, false)
	st2.verifySnapshot(m1)
	require.NoError(st2.drainContentReads(ctx))
	st2.verifySnapshot(m2)
	require.NoError(st2.drainContentReads(ctx))
	require.Empty(st2.result.Problems)
	assert.Equal(st1.contentReads, st2.contentReads,
		"snapshot 2 shares all content, so memoization adds no new full reads")
}

// TestVerifyDetectsHashMapGeometryMismatch pins Minor 11: full verify compares
// the materialized page-hash-map's geometry against the manifest. A simple
// byte-tamper of the manifest is now rejected earlier by LoadManifest's
// content-derived ID check, so this models the remaining reachable case — a
// buggy writer that recorded the wrong geometry and computed a self-consistent
// snapshot ID over it.
func TestVerifyDetectsHashMapGeometryMismatch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r, m := buildVerifyFixture(t)

	path := r.Path(snapshotsDirName, m.SnapshotID+manifestExt)
	data, err := os.ReadFile(path)
	require.NoError(err)
	var doctored Manifest
	require.NoError(json.Unmarshal(data, &doctored))
	doctored.DB.PageCount++
	createdAt, err := time.Parse(time.RFC3339, doctored.CreatedAt)
	require.NoError(err)
	forgedID, err := ComputeSnapshotID(createdAt, &doctored)
	require.NoError(err)
	doctored.SnapshotID = forgedID
	out, err := json.MarshalIndent(&doctored, "", "  ")
	require.NoError(err)
	require.NoError(os.Remove(path))
	require.NoError(os.WriteFile(r.Path(snapshotsDirName, forgedID+manifestExt), out, 0o600))

	res, err := Verify(context.Background(), r, VerifyOptions{})
	require.NoError(err)
	found := false
	for _, p := range res.Problems {
		if strings.Contains(p.Detail, "page hash map covers") {
			found = true
		}
	}
	assert.True(found, "full verify must flag the hash-map geometry mismatch")
}
