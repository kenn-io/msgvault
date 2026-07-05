package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDB builds pageCount pages of pageSize bytes with deterministic content.
func fakeDB(t *testing.T, pageSize uint32, pageCount int) []byte {
	t.Helper()
	db := make([]byte, int(pageSize)*pageCount)
	_, err := rand.Read(db)
	require.NoError(t, err)
	return db
}

func TestScanPagesFullAndIncremental(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	const pageSize = 512
	db := fakeDB(t, pageSize, 10)
	res, err := ScanPages(context.Background(), bytes.NewReader(db), pageSize, 10, nil, nil)
	require.NoError(err)
	assert.Equal(uint64(10), res.PageCount)
	assert.Len(res.Hashes, 10*pageHashSize)
	require.Len(res.Dirty, 1)
	assert.Equal(PageRange{Start: 0, Count: 10}, res.Dirty[0])

	parent := &PageHashMap{PageSize: pageSize, PageCount: 10, Hashes: res.Hashes}

	// No changes: clean incremental scan.
	res2, err := ScanPages(context.Background(), bytes.NewReader(db), pageSize, 10, parent, nil)
	require.NoError(err)
	assert.Empty(res2.Dirty)

	// Mutate pages 2 and 3 (adjacent -> one range) and page 7.
	db2 := append([]byte{}, db...)
	db2[2*pageSize] ^= 0xff
	db2[3*pageSize] ^= 0xff
	db2[7*pageSize] ^= 0xff
	res3, err := ScanPages(context.Background(), bytes.NewReader(db2), pageSize, 10, parent, nil)
	require.NoError(err)
	assert.Equal([]PageRange{{Start: 2, Count: 2}, {Start: 7, Count: 1}}, res3.Dirty)

	// Growth: pages beyond the parent count are dirty.
	db3 := append(append([]byte{}, db...), fakeDB(t, pageSize, 2)...)
	res4, err := ScanPages(context.Background(), bytes.NewReader(db3), pageSize, 12, parent, nil)
	require.NoError(err)
	assert.Equal([]PageRange{{Start: 10, Count: 2}}, res4.Dirty)

	// Page size mismatch errors.
	_, err = ScanPages(context.Background(), bytes.NewReader(db), 1024, 5, parent, nil)
	require.Error(err)
}

// TestScanPagesMultiChunkMatchesBruteForce exercises the concurrent scan
// pipeline across many chunks (pageCount >> scanChunkPages) and checks its
// output against a brute-force per-page reference: hashes for every page,
// and dirty ranges exactly covering the mutated pages.
func TestScanPagesMultiChunkMatchesBruteForce(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	const pageSize = 512
	const pageCount = 4*scanChunkPages + 37 // 5 chunks, last one partial
	db := fakeDB(t, pageSize, pageCount)

	base, err := ScanPages(context.Background(), bytes.NewReader(db), pageSize, pageCount, nil, nil)
	require.NoError(err)
	require.Len(base.Hashes, pageCount*pageHashSize)
	require.Equal([]PageRange{{Start: 0, Count: pageCount}}, base.Dirty)
	for p := range pageCount {
		h := PageHash(db[p*pageSize : (p+1)*pageSize])
		assert.Equal(h[:], base.Hashes[p*pageHashSize:(p+1)*pageHashSize], "page %d hash", p)
	}

	// Mutate a scattered set of pages, including chunk-boundary straddles
	// (1023..1025) so range merging across chunk seams is covered.
	parent := &PageHashMap{PageSize: pageSize, PageCount: pageCount, Hashes: base.Hashes}
	mutated := []int{0, 511, 1023, 1024, 1025, 2048, 3000, 3001, pageCount - 1}
	db2 := append([]byte{}, db...)
	for _, p := range mutated {
		db2[p*pageSize] ^= 0xff
	}
	var progressCalls []uint64
	res, err := ScanPages(context.Background(), bytes.NewReader(db2), pageSize, pageCount, parent, func(done, total uint64) {
		require.Equal(uint64(pageCount), total)
		progressCalls = append(progressCalls, done)
	})
	require.NoError(err)
	assert.Equal([]PageRange{
		{Start: 0, Count: 1}, {Start: 511, Count: 1}, {Start: 1023, Count: 3},
		{Start: 2048, Count: 1}, {Start: 3000, Count: 2}, {Start: pageCount - 1, Count: 1},
	}, res.Dirty)

	// Progress arrives strictly in chunk order despite out-of-order hashing.
	require.Len(progressCalls, 5)
	assert.Equal([]uint64{scanChunkPages, 2 * scanChunkPages, 3 * scanChunkPages, 4 * scanChunkPages, pageCount}, progressCalls)
}

// failAfterReader fails any ReadAt touching offsets at or beyond failAt.
type failAfterReader struct {
	data   []byte
	failAt int64
}

func (r *failAfterReader) ReadAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > r.failAt {
		return 0, assert.AnError
	}
	n, err := bytes.NewReader(r.data).ReadAt(p, off)
	if err != nil {
		return n, fmt.Errorf("failAfterReader: %w", err)
	}
	return n, nil
}

func TestScanPagesPropagatesMidScanReadError(t *testing.T) {
	require := require.New(t)

	const pageSize = 512
	const pageCount = 3 * scanChunkPages
	db := fakeDB(t, pageSize, pageCount)

	// The second chunk's read fails; the error must surface, not hang or be
	// swallowed by the concurrent pipeline.
	r := &failAfterReader{data: db, failAt: int64(scanChunkPages * pageSize)}
	_, err := ScanPages(context.Background(), r, pageSize, pageCount, nil, nil)
	require.ErrorIs(err, assert.AnError)
	require.ErrorContains(err, "reading DB pages")
}

func TestPlanBlobs(t *testing.T) {
	assert := assert.New(t)

	// A large range becomes dedicated run plans split at 1024 pages.
	plans := PlanBlobs([]PageRange{{Start: 0, Count: 2500}})
	assert.Len(plans, 3)
	assert.Equal(uint64(1024), plans[0].Pages())
	assert.Equal(uint64(1024), plans[1].Pages())
	assert.Equal(uint64(452), plans[2].Pages())

	// Small scattered ranges group into one delta-group plan.
	small := []PageRange{{Start: 0, Count: 1}, {Start: 5, Count: 2}, {Start: 90, Count: 3}}
	plans = PlanBlobs(small)
	assert.Len(plans, 1)
	assert.Equal(small, plans[0].Ranges)

	// Groups flush before exceeding 1024 pages.
	var many []PageRange
	for i := range 10 {
		many = append(many, PageRange{Start: uint64(i) * 1000, Count: 200})
	}
	plans = PlanBlobs(many)
	for _, p := range plans {
		assert.LessOrEqual(p.Pages(), uint64(1024))
	}
	var total uint64
	for _, p := range plans {
		total += p.Pages()
	}
	assert.Equal(uint64(2000), total)

	// A mixed input keeps big ranges dedicated and groups the rest.
	mixed := []PageRange{{Start: 0, Count: 300}, {Start: 400, Count: 2}}
	plans = PlanBlobs(mixed)
	assert.Len(plans, 2)
	assert.Equal([]PageRange{{Start: 0, Count: 300}}, plans[0].Ranges)
	assert.Equal([]PageRange{{Start: 400, Count: 2}}, plans[1].Ranges)
}

func TestBuildBlobContentAndRuns(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	const pageSize = 256
	db := fakeDB(t, pageSize, 20)
	plan := BlobPlan{Ranges: []PageRange{{Start: 2, Count: 2}, {Start: 10, Count: 1}}}

	content, err := BuildBlobContent(bytes.NewReader(db), pageSize, plan)
	require.NoError(err)
	require.Len(content, 3*pageSize)
	assert.Equal(db[2*pageSize:4*pageSize], content[:2*pageSize])
	assert.Equal(db[10*pageSize:11*pageSize], content[2*pageSize:])

	runs := RunsForPlan(plan, 4, pageSize)
	assert.Equal([]PageRun{
		{StartPage: 2, PageCount: 2, BlobIndex: 4, BlobOffset: 0},
		{StartPage: 10, PageCount: 1, BlobIndex: 4, BlobOffset: 2 * pageSize},
	}, runs)
}

func TestBuildHashDelta(t *testing.T) {
	require := require.New(t)
	const pageSize = 512
	db := fakeDB(t, pageSize, 6)
	res, err := ScanPages(context.Background(), bytes.NewReader(db), pageSize, 6, nil, nil)
	require.NoError(err)

	d := BuildHashDelta(res)
	require.Equal(uint64(6), d.PageCount)
	require.Len(d.Pages, 6)
	require.Equal(res.Hashes, d.Hashes)

	res.Dirty = []PageRange{{Start: 1, Count: 2}, {Start: 5, Count: 1}}
	d = BuildHashDelta(res)
	require.Equal([]uint64{1, 2, 5}, d.Pages)
	require.Equal(
		append(append([]byte{}, res.Hashes[1*pageHashSize:3*pageHashSize]...), res.Hashes[5*pageHashSize:6*pageHashSize]...),
		d.Hashes)
}
