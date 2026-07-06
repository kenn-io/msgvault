package backup

import (
	"fmt"
	"math"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/pack"
)

func blobID(s string) pack.BlobID { return pack.ComputeBlobID([]byte(s)) }

func TestPageMapRoundTrip(t *testing.T) {
	require := require.New(t)
	m := &PageMap{
		PageSize:  4096,
		PageCount: 10,
		Blobs:     []pack.BlobID{blobID("b0"), blobID("b1")},
		Runs: []PageRun{
			{StartPage: 0, PageCount: 6, BlobIndex: 0, BlobOffset: 0},
			{StartPage: 6, PageCount: 4, BlobIndex: 1, BlobOffset: 0},
		},
	}
	for _, isDelta := range []bool{false, true} {
		got, gotDelta, err := DecodePageMap(EncodePageMap(m, isDelta))
		require.NoError(err)
		require.Equal(isDelta, gotDelta)
		require.Equal(m, got)
	}
}

func TestDecodePageMapRejectsInvalid(t *testing.T) {
	require := require.New(t)
	m := &PageMap{
		PageSize: 4096, PageCount: 4,
		Blobs: []pack.BlobID{blobID("b0")},
		Runs:  []PageRun{{StartPage: 0, PageCount: 4, BlobIndex: 0}},
	}
	good := EncodePageMap(m, false)
	for _, mut := range []int{0, 5, len(good) / 2, len(good) - 1} {
		bad := append([]byte{}, good...)
		bad[mut] ^= 0x01
		_, _, err := DecodePageMap(bad)
		require.Error(err, "mutated byte %d", mut)
	}
	// Overlapping runs rejected.
	overlap := &PageMap{
		PageSize: 4096, PageCount: 4,
		Blobs: []pack.BlobID{blobID("b0")},
		Runs: []PageRun{
			{StartPage: 0, PageCount: 3, BlobIndex: 0},
			{StartPage: 2, PageCount: 2, BlobIndex: 0},
		},
	}
	_, _, err := DecodePageMap(EncodePageMap(overlap, false))
	require.Error(err)
	// Blob index out of range rejected.
	badIdx := &PageMap{
		PageSize: 4096, PageCount: 1,
		Blobs: []pack.BlobID{blobID("b0")},
		Runs:  []PageRun{{StartPage: 0, PageCount: 1, BlobIndex: 7}},
	}
	_, _, err = DecodePageMap(EncodePageMap(badIdx, false))
	require.Error(err)
}

// TestDecodePageMapRejectsStartPageOverflow pins the fix validating a run's
// StartPage+PageCount via subtraction instead of addition. A near-MaxUint64
// StartPage used to wrap the addition back below m.PageCount, letting a run
// that claims to extend far past the map's page count slip through.
func TestDecodePageMapRejectsStartPageOverflow(t *testing.T) {
	require := require.New(t)
	m := &PageMap{
		PageSize: 4096, PageCount: 4,
		Blobs: []pack.BlobID{blobID("b0")},
		Runs:  []PageRun{{StartPage: math.MaxUint64 - 1, PageCount: 4, BlobIndex: 0}},
	}
	_, _, err := DecodePageMap(EncodePageMap(m, false))
	require.Error(err)
}

func TestCheckCoverageAndLookup(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	m := &PageMap{
		PageSize: 4096, PageCount: 5,
		Blobs: []pack.BlobID{blobID("b0"), blobID("b1")},
		Runs: []PageRun{
			{StartPage: 0, PageCount: 2, BlobIndex: 0, BlobOffset: 0},
			{StartPage: 2, PageCount: 3, BlobIndex: 1, BlobOffset: 8192},
		},
	}
	require.NoError(m.CheckCoverage())

	id, off, err := m.Lookup(3)
	require.NoError(err)
	assert.Equal(blobID("b1"), id)
	assert.Equal(uint64(8192+4096), off)
	_, _, err = m.Lookup(5)
	require.Error(err)

	gap := &PageMap{PageSize: 4096, PageCount: 3, Blobs: m.Blobs,
		Runs: []PageRun{{StartPage: 0, PageCount: 2, BlobIndex: 0}}}
	require.Error(gap.CheckCoverage())
	short := &PageMap{PageSize: 4096, PageCount: 3, Blobs: m.Blobs, Runs: m.Runs[:1]}
	require.Error(short.CheckCoverage())
}

func TestApplyPageMapDelta(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	base := &PageMap{
		PageSize: 4096, PageCount: 8,
		Blobs: []pack.BlobID{blobID("orig")},
		Runs:  []PageRun{{StartPage: 0, PageCount: 8, BlobIndex: 0, BlobOffset: 0}},
	}
	// Delta: pages 2-3 rewritten, page 8 appended (growth to 9).
	delta := &PageMap{
		PageSize: 4096, PageCount: 9,
		Blobs: []pack.BlobID{blobID("new")},
		Runs: []PageRun{
			{StartPage: 2, PageCount: 2, BlobIndex: 0, BlobOffset: 0},
			{StartPage: 8, PageCount: 1, BlobIndex: 0, BlobOffset: 8192},
		},
	}
	merged, err := ApplyPageMapDelta(base, delta)
	require.NoError(err)
	require.NoError(merged.CheckCoverage())
	assert.Equal(uint64(9), merged.PageCount)

	expect := map[uint64]struct {
		blob pack.BlobID
		off  uint64
	}{
		0: {blobID("orig"), 0},
		1: {blobID("orig"), 4096},
		2: {blobID("new"), 0},
		3: {blobID("new"), 4096},
		4: {blobID("orig"), 4 * 4096},
		7: {blobID("orig"), 7 * 4096},
		8: {blobID("new"), 8192},
	}
	for page, want := range expect {
		id, off, err := merged.Lookup(page)
		require.NoError(err)
		assert.Equal(want.blob, id, "page %d blob", page)
		assert.Equal(want.off, off, "page %d offset", page)
	}

	// Shrink: new count 4 truncates the tail run.
	shrinkDelta := &PageMap{
		PageSize: 4096, PageCount: 4,
		Blobs: []pack.BlobID{blobID("new2")},
		Runs:  []PageRun{{StartPage: 3, PageCount: 1, BlobIndex: 0, BlobOffset: 0}},
	}
	shrunk, err := ApplyPageMapDelta(merged, shrinkDelta)
	require.NoError(err)
	require.NoError(shrunk.CheckCoverage())
	assert.Equal(uint64(4), shrunk.PageCount)
}

// pageOwner records, per page, which blob and byte offset should hold it.
type pageOwner struct {
	blob pack.BlobID
	off  uint64
}

// buildRandomKeyframe partitions [0,count) into runs, one blob each, and
// returns the map plus a per-page ownership reference.
func buildRandomKeyframe(rng *rand.Rand, tag string, count uint64, pageSize uint32) (*PageMap, map[uint64]pageOwner) {
	m := &PageMap{PageSize: pageSize, PageCount: count}
	owners := make(map[uint64]pageOwner, count)
	for p := uint64(0); p < count; {
		runLen := uint64(1 + rng.Intn(10))
		if p+runLen > count {
			runLen = count - p
		}
		blob := blobID(fmt.Sprintf("%s-%d", tag, len(m.Blobs)))
		idx := uint32(len(m.Blobs))
		m.Blobs = append(m.Blobs, blob)
		m.Runs = append(m.Runs, PageRun{StartPage: p, PageCount: uint32(runLen), BlobIndex: idx})
		for k := range runLen {
			owners[p+k] = pageOwner{blob: blob, off: k * uint64(pageSize)}
		}
		p += runLen
	}
	return m, owners
}

// buildRandomDelta rewrites a random subset of pages and always covers the
// grown tail [baseCount,newCount), so the merged map has full coverage.
func buildRandomDelta(rng *rand.Rand, tag string, baseCount, newCount uint64, pageSize uint32) (*PageMap, map[uint64]pageOwner) {
	dirty := make([]bool, newCount)
	for q := range newCount {
		if q >= baseCount || rng.Float64() < 0.35 {
			dirty[q] = true
		}
	}
	m := &PageMap{PageSize: pageSize, PageCount: newCount}
	owners := make(map[uint64]pageOwner, newCount)
	for q := uint64(0); q < newCount; {
		if !dirty[q] {
			q++
			continue
		}
		start := q
		for q < newCount && dirty[q] {
			q++
		}
		runLen := q - start
		blob := blobID(fmt.Sprintf("%s-%d", tag, len(m.Blobs)))
		idx := uint32(len(m.Blobs))
		m.Blobs = append(m.Blobs, blob)
		m.Runs = append(m.Runs, PageRun{StartPage: start, PageCount: uint32(runLen), BlobIndex: idx})
		for k := range runLen {
			owners[start+k] = pageOwner{blob: blob, off: k * uint64(pageSize)}
		}
	}
	return m, owners
}

// TestApplyPageMapDeltaMatchesReference pins Important 7: the linear
// two-pointer subtraction must produce the same per-page mapping as a
// brute-force per-page model over randomized (seeded, deterministic) runs.
func TestApplyPageMapDeltaMatchesReference(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	const pageSize = 4096

	for iter := range 60 {
		rng := rand.New(rand.NewSource(int64(iter) + 1)) //nolint:gosec // deterministic test RNG, not security
		baseCount := uint64(50 + rng.Intn(250))
		base, baseOwners := buildRandomKeyframe(rng, fmt.Sprintf("base%d", iter), baseCount, pageSize)
		require.NoError(base.CheckCoverage(), "iter %d base", iter)

		newCount := uint64(1 + rng.Intn(int(baseCount)+60))
		delta, deltaOwners := buildRandomDelta(rng, fmt.Sprintf("delta%d", iter), baseCount, newCount, pageSize)

		merged, err := ApplyPageMapDelta(base, delta)
		require.NoError(err, "iter %d", iter)
		require.NoError(merged.CheckCoverage(), "iter %d merged coverage", iter)

		for page := range newCount {
			want, ok := deltaOwners[page]
			if !ok {
				want = baseOwners[page] // non-dirty pages are always < baseCount
			}
			gotBlob, gotOff, err := merged.Lookup(page)
			require.NoError(err, "iter %d page %d", iter, page)
			assert.Equal(want.blob, gotBlob, "iter %d page %d blob", iter, page)
			assert.Equal(want.off, gotOff, "iter %d page %d offset", iter, page)
		}
	}
}

func TestMaterializePageMap(t *testing.T) {
	require := require.New(t)

	key := &PageMap{
		PageSize: 4096, PageCount: 4,
		Blobs: []pack.BlobID{blobID("k")},
		Runs:  []PageRun{{StartPage: 0, PageCount: 4, BlobIndex: 0}},
	}
	d := &PageMap{
		PageSize: 4096, PageCount: 4,
		Blobs: []pack.BlobID{blobID("d")},
		Runs:  []PageRun{{StartPage: 1, PageCount: 1, BlobIndex: 0}},
	}
	blobs := map[pack.BlobID][]byte{}
	put := func(data []byte) pack.BlobID {
		id := pack.ComputeBlobID(data)
		blobs[id] = data
		return id
	}
	keyID := put(EncodePageMap(key, false))
	dID := put(EncodePageMap(d, true))
	fetch := func(id pack.BlobID) ([]byte, error) { return blobs[id], nil }

	got, err := MaterializePageMap(fetch, []pack.BlobID{dID, keyID})
	require.NoError(err)
	require.NoError(got.CheckCoverage())
	id, _, err := got.Lookup(1)
	require.NoError(err)
	require.Equal(blobID("d"), id)

	_, err = MaterializePageMap(fetch, []pack.BlobID{dID})
	require.Error(err)
}
