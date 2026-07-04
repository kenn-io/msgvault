package backup

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/pack"
)

func hashMapOf(t *testing.T, pageSize uint32, pages ...string) *PageHashMap { //nolint:unparam
	t.Helper()
	m := &PageHashMap{PageSize: pageSize, PageCount: uint64(len(pages))}
	for _, p := range pages {
		h := sha256.Sum256([]byte(p))
		m.Hashes = append(m.Hashes, h[:pageHashSize]...)
	}
	return m
}

func TestHashKeyframeRoundTrip(t *testing.T) {
	require := require.New(t)
	m := hashMapOf(t, 4096, "a", "b", "c")
	got, err := DecodeHashKeyframe(EncodeHashKeyframe(m))
	require.NoError(err)
	require.Equal(m, got)
}

func TestHashDeltaRoundTripAndApply(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	base := hashMapOf(t, 4096, "a", "b", "c")
	// Page 1 changes, pages 3-4 appended (growth).
	d := &PageHashDelta{PageSize: 4096, PageCount: 5, Pages: []uint64{1, 3, 4}}
	for _, p := range []string{"B", "d", "e"} {
		h := sha256.Sum256([]byte(p))
		d.Hashes = append(d.Hashes, h[:pageHashSize]...)
	}
	got, err := DecodeHashDelta(EncodeHashDelta(d))
	require.NoError(err)
	require.Equal(d, got)

	merged, err := ApplyHashDelta(base, d)
	require.NoError(err)
	want := hashMapOf(t, 4096, "a", "B", "c", "d", "e")
	assert.Equal(want, merged)

	// Shrink: delta with smaller count truncates.
	shrunk, err := ApplyHashDelta(merged, &PageHashDelta{PageSize: 4096, PageCount: 2})
	require.NoError(err)
	assert.Equal(hashMapOf(t, 4096, "a", "B"), shrunk)
}

func TestApplyHashDeltaRejectsMismatch(t *testing.T) {
	require := require.New(t)
	base := hashMapOf(t, 4096, "a")
	_, err := ApplyHashDelta(base, &PageHashDelta{PageSize: 8192, PageCount: 1})
	require.Error(err)
	_, err = ApplyHashDelta(base, &PageHashDelta{
		PageSize: 4096, PageCount: 1,
		Pages: []uint64{5}, Hashes: make([]byte, pageHashSize),
	})
	require.Error(err)
	// A corrupt delta with a huge PageCount must error, not wrap the
	// allocation size and panic (or silently build an inconsistent map).
	_, err = ApplyHashDelta(base, &PageHashDelta{PageSize: 4096, PageCount: math.MaxUint64})
	require.Error(err)
	_, err = ApplyHashDelta(base, &PageHashDelta{
		PageSize:  4096,
		PageCount: math.MaxUint64/pageHashSize + 1,
	})
	require.Error(err)
}

func TestHashCodecRejectsDamage(t *testing.T) {
	require := require.New(t)
	key := EncodeHashKeyframe(hashMapOf(t, 4096, "a", "b"))
	for _, mut := range []int{0, 5, len(key) / 2, len(key) - 1} {
		bad := append([]byte{}, key...)
		bad[mut] ^= 0x01
		_, err := DecodeHashKeyframe(bad)
		require.Error(err, "mutated byte %d", mut)
	}
	_, err := DecodeHashKeyframe(key[:10])
	require.Error(err)
	// Keyframe decoder must reject a delta object and vice versa.
	delta := EncodeHashDelta(&PageHashDelta{PageSize: 4096, PageCount: 2})
	_, err = DecodeHashKeyframe(delta)
	require.Error(err)
	_, err = DecodeHashDelta(key)
	require.Error(err)
}

func TestHashCodecRejectsTruncatedButValidTrailer(t *testing.T) {
	require := require.New(t)

	// Body is only magic+version (6 bytes), but the trailer is a genuine
	// SHA-256 of that body, so checkMapObject's integrity check passes.
	// DecodeHashKeyframe must still reject the object instead of slicing
	// past the end of a 6-byte body.
	keyBody := append([]byte(hashKeyframeMagic), 0x01, 0x00)
	keySum := sha256.Sum256(keyBody)
	keyObj := append(append([]byte{}, keyBody...), keySum[:]...)
	_, err := DecodeHashKeyframe(keyObj)
	require.Error(err)

	deltaBody := append([]byte(hashDeltaMagic), 0x01, 0x00)
	deltaSum := sha256.Sum256(deltaBody)
	deltaObj := append(append([]byte{}, deltaBody...), deltaSum[:]...)
	_, err = DecodeHashDelta(deltaObj)
	require.Error(err)
}

// TestHashKeyframeRejectsPageCountOverflow pins the fix for
// DecodeHashKeyframe's body-size check, which used to compute
// m.PageCount*pageHashSize directly. wrapPageCount*pageHashSize (1<<60 * 16)
// wraps to exactly 0 mod 2^64, so it used to match a body carrying zero hash
// bytes and pass validation despite claiming an enormous page count. The
// division/modulo check must still reject it.
func TestHashKeyframeRejectsPageCountOverflow(t *testing.T) {
	require := require.New(t)

	const wrapPageCount = uint64(1) << 60
	body := make([]byte, 0, 18)
	body = append(body, hashKeyframeMagic...)
	body = binary.LittleEndian.AppendUint16(body, mapObjectVersion)
	body = binary.LittleEndian.AppendUint32(body, 4096)
	body = binary.LittleEndian.AppendUint64(body, wrapPageCount)
	sum := sha256.Sum256(body)
	obj := append(append([]byte{}, body...), sum[:]...)

	_, err := DecodeHashKeyframe(obj)
	require.Error(err)
}

// TestApplyHashDeltaRejectsUncoveredGrowth pins the growth-coverage check: a
// delta that claims a larger PageCount must carry a hash for every appended
// page. A sparse growing delta would otherwise materialize zero-filled hashes
// for the uncovered appended pages, which verify and restore would then trust.
func TestApplyHashDeltaRejectsUncoveredGrowth(t *testing.T) {
	require := require.New(t)
	base := hashMapOf(t, 4096, "a")

	// Grows 1 -> 10 but only covers appended page 5: pages 1-4 and 6-9
	// would come back as zero hashes.
	_, err := ApplyHashDelta(base, &PageHashDelta{
		PageSize: 4096, PageCount: 10,
		Pages: []uint64{5}, Hashes: make([]byte, pageHashSize),
	})
	require.ErrorContains(err, "carries hashes for 1 of 9 appended pages")

	// A tiny corrupt delta declaring huge-but-allocatable growth must be
	// rejected by the coverage check before the zero-filled allocation.
	_, err = ApplyHashDelta(base, &PageHashDelta{
		PageSize: 4096, PageCount: 1 << 40,
		Pages: []uint64{1}, Hashes: make([]byte, pageHashSize),
	})
	require.ErrorContains(err, "appended pages")

	// Modifying existing pages while growing is fine as long as every
	// appended page is covered.
	grown, err := ApplyHashDelta(hashMapOf(t, 4096, "a", "b"), &PageHashDelta{
		PageSize: 4096, PageCount: 3,
		Pages: []uint64{0, 2}, Hashes: make([]byte, 2*pageHashSize),
	})
	require.NoError(err)
	require.Equal(uint64(3), grown.PageCount)
}

func TestApplyHashDeltaRejectsHashLengthMismatch(t *testing.T) {
	require := require.New(t)
	base := hashMapOf(t, 4096, "a")
	_, err := ApplyHashDelta(base, &PageHashDelta{
		PageSize: 4096, PageCount: 1,
		Pages: []uint64{0}, Hashes: nil,
	})
	require.Error(err)
}

func TestMaterializeHashMap(t *testing.T) {
	require := require.New(t)

	key := hashMapOf(t, 4096, "a", "b", "c")
	d1 := &PageHashDelta{PageSize: 4096, PageCount: 3, Pages: []uint64{0}}
	h := sha256.Sum256([]byte("A"))
	d1.Hashes = append([]byte{}, h[:pageHashSize]...)
	d2 := &PageHashDelta{PageSize: 4096, PageCount: 4, Pages: []uint64{3}}
	h = sha256.Sum256([]byte("d"))
	d2.Hashes = append([]byte{}, h[:pageHashSize]...)

	blobs := map[pack.BlobID][]byte{}
	put := func(data []byte) pack.BlobID {
		id := pack.ComputeBlobID(data)
		blobs[id] = data
		return id
	}
	keyID := put(EncodeHashKeyframe(key))
	d1ID := put(EncodeHashDelta(d1))
	d2ID := put(EncodeHashDelta(d2))
	fetch := func(id pack.BlobID) ([]byte, error) { return blobs[id], nil }

	got, err := MaterializeHashMap(fetch, []pack.BlobID{d2ID, d1ID, keyID})
	require.NoError(err)
	require.Equal(hashMapOf(t, 4096, "A", "b", "c", "d"), got)

	// Chain without a keyframe fails.
	_, err = MaterializeHashMap(fetch, []pack.BlobID{d2ID, d1ID})
	require.Error(err)
}

func TestHashMapCacheRoundTrip(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dir := t.TempDir()

	m := hashMapOf(t, 4096, "a", "b")
	require.NoError(SaveHashMapCache(dir, "repo-1", "snap-1", m))
	snap, got, err := LoadHashMapCache(dir, "repo-1")
	require.NoError(err)
	assert.Equal("snap-1", snap)
	assert.Equal(m, got)

	// Absent and corrupt caches are non-errors: disposable.
	snap, got, err = LoadHashMapCache(dir, "missing")
	require.NoError(err)
	assert.Empty(snap)
	assert.Nil(got)
}
