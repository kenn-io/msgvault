package backup

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"sort"

	"go.kenn.io/msgvault/internal/pack"
)

const (
	hashKeyframeMagic = "MVHK"
	hashDeltaMagic    = "MVHD"
	mapObjectVersion  = 1
	pageHashSize      = 16
	// keyframeChainMax bounds the delta chain depth before a new keyframe.
	keyframeChainMax = 30
)

// PageHashMap holds the truncated SHA-256 of every DB page. It is the input
// to incremental change detection (docs/architecture/backup-format.md, Page-Map Objects).
type PageHashMap struct {
	PageSize  uint32
	PageCount uint64
	Hashes    []byte
}

// PageHashDelta patches a parent PageHashMap: PageCount is the new total,
// Pages lists changed page numbers ascending, Hashes their new hashes.
type PageHashDelta struct {
	PageSize  uint32
	PageCount uint64
	Pages     []uint64
	Hashes    []byte
}

// PageHash returns the truncated SHA-256 dedup hash of one page.
func PageHash(page []byte) [pageHashSize]byte {
	full := sha256.Sum256(page)
	var h [pageHashSize]byte
	copy(h[:], full[:pageHashSize])
	return h
}

// EncodeHashKeyframe serializes a full hash map.
func EncodeHashKeyframe(m *PageHashMap) []byte {
	buf := make([]byte, 0, 4+2+4+8+len(m.Hashes)+trailerHashLen)
	buf = append(buf, hashKeyframeMagic...)
	buf = binary.LittleEndian.AppendUint16(buf, mapObjectVersion)
	buf = binary.LittleEndian.AppendUint32(buf, m.PageSize)
	buf = binary.LittleEndian.AppendUint64(buf, m.PageCount)
	buf = append(buf, m.Hashes...)
	sum := sha256.Sum256(buf)
	return append(buf, sum[:]...)
}

// DecodeHashKeyframe parses and integrity-checks a keyframe object.
func DecodeHashKeyframe(data []byte) (*PageHashMap, error) {
	body, err := checkMapObject(data, hashKeyframeMagic)
	if err != nil {
		return nil, err
	}
	if len(body) < 18 {
		return nil, fmt.Errorf("backup: hash keyframe truncated (%d bytes)", len(body))
	}
	m := &PageHashMap{
		PageSize:  binary.LittleEndian.Uint32(body[6:10]),
		PageCount: binary.LittleEndian.Uint64(body[10:18]),
	}
	rest := body[18:]
	// Compare via division/modulo rather than m.PageCount*pageHashSize: that
	// multiplication is on an untrusted, attacker-controlled uint64 and can
	// wrap for a huge PageCount, making a malformed (too-short) body appear
	// to match.
	if len(rest)%pageHashSize != 0 || uint64(len(rest)/pageHashSize) != m.PageCount {
		return nil, fmt.Errorf("backup: hash keyframe body size mismatch (pages %d)", m.PageCount)
	}
	if len(rest) > 0 {
		m.Hashes = append([]byte{}, rest...)
	}
	return m, nil
}

// EncodeHashDelta serializes a hash-map delta.
func EncodeHashDelta(d *PageHashDelta) []byte {
	buf := make(
		[]byte,
		0,
		4+2+4+8+4+len(d.Pages)*8+len(d.Hashes)+trailerHashLen,
	)
	buf = append(buf, hashDeltaMagic...)
	buf = binary.LittleEndian.AppendUint16(buf, mapObjectVersion)
	buf = binary.LittleEndian.AppendUint32(buf, d.PageSize)
	buf = binary.LittleEndian.AppendUint64(buf, d.PageCount)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(d.Pages))) //nolint:gosec // page counts fit u32 far below overflow
	for _, p := range d.Pages {
		buf = binary.LittleEndian.AppendUint64(buf, p)
	}
	buf = append(buf, d.Hashes...)
	sum := sha256.Sum256(buf)
	return append(buf, sum[:]...)
}

// DecodeHashDelta parses and integrity-checks a delta object.
func DecodeHashDelta(data []byte) (*PageHashDelta, error) {
	body, err := checkMapObject(data, hashDeltaMagic)
	if err != nil {
		return nil, err
	}
	if len(body) < 22 {
		return nil, fmt.Errorf("backup: hash delta truncated") //nolint:perfsprint
	}
	d := &PageHashDelta{
		PageSize:  binary.LittleEndian.Uint32(body[6:10]),
		PageCount: binary.LittleEndian.Uint64(body[10:18]),
	}
	count := binary.LittleEndian.Uint32(body[18:22])
	rest := body[22:]
	if uint64(len(rest)) != uint64(count)*(8+pageHashSize) {
		return nil, fmt.Errorf("backup: hash delta body size mismatch (entries %d)", count)
	}
	var prev uint64
	for i := uint32(0); i < count; i++ { //nolint:intrange,modernize // uint32 iteration requires standard for loop
		p := binary.LittleEndian.Uint64(rest[i*8 : i*8+8])
		if i > 0 && p <= prev {
			return nil, fmt.Errorf("backup: hash delta pages not strictly sorted") //nolint:perfsprint
		}
		prev = p
		d.Pages = append(d.Pages, p)
	}
	if count > 0 {
		d.Hashes = append([]byte{}, rest[count*8:]...)
	}
	return d, nil
}

// checkMapObject validates trailer, magic, and version, returning the body.
func checkMapObject(data []byte, magic string) ([]byte, error) {
	if len(data) < 6+trailerHashLen {
		return nil, fmt.Errorf("backup: %s object truncated (%d bytes)", magic, len(data))
	}
	body, trailer := data[:len(data)-trailerHashLen], data[len(data)-trailerHashLen:]
	sum := sha256.Sum256(body)
	if !bytes.Equal(sum[:], trailer) {
		return nil, fmt.Errorf("backup: %s object integrity check failed", magic)
	}
	if string(body[:4]) != magic {
		return nil, fmt.Errorf("backup: expected %s object, got magic %q", magic, string(body[:4]))
	}
	if v := binary.LittleEndian.Uint16(body[4:6]); v != mapObjectVersion {
		return nil, fmt.Errorf("backup: unsupported %s object version %d", magic, v)
	}
	return body, nil
}

// ApplyHashDelta produces the child hash map from a parent and a delta.
func ApplyHashDelta(base *PageHashMap, d *PageHashDelta) (*PageHashMap, error) {
	if len(d.Hashes) != len(d.Pages)*pageHashSize {
		return nil, fmt.Errorf(
			"backup: hash delta has %d pages but %d hash bytes",
			len(d.Pages),
			len(d.Hashes),
		)
	}
	if base.PageSize != d.PageSize {
		return nil, fmt.Errorf(
			"backup: hash delta page size %d does not match base %d",
			d.PageSize,
			base.PageSize,
		)
	}
	// PageCount is untrusted (it arrives from decoded delta objects). A
	// growing delta must explicitly carry a hash for every appended page:
	// BuildHashDelta always does (appended pages are dirty by definition), so
	// a delta claiming growth beyond its own entries is corrupt. Pages is
	// strictly sorted and each entry is bounds-checked below, so exactly
	// (PageCount - base.PageCount) entries at or past base.PageCount means
	// the appended range is fully covered. This also bounds the allocation
	// below by the delta's own decoded size, so a tiny corrupt delta cannot
	// declare a huge PageCount and force a massive zero-filled allocation.
	if d.PageCount > base.PageCount {
		grown := d.PageCount - base.PageCount
		firstAppended := sort.Search(len(d.Pages), func(i int) bool {
			return d.Pages[i] >= base.PageCount
		})
		appended := uint64(len(d.Pages) - firstAppended) //nolint:gosec // Search result is within [0, len]
		if appended != grown {
			return nil, fmt.Errorf(
				"backup: hash delta grows the map to %d pages but carries hashes for %d of %d appended pages",
				d.PageCount, appended, grown)
		}
	}
	// Belt and braces: the growth check above already bounds PageCount, but
	// the multiplication below must never be able to wrap regardless.
	if d.PageCount > uint64(math.MaxInt/pageHashSize) {
		return nil, fmt.Errorf("backup: hash delta page count %d too large", d.PageCount)
	}
	out := &PageHashMap{PageSize: base.PageSize, PageCount: d.PageCount}
	out.Hashes = make([]byte, d.PageCount*pageHashSize)
	copy(out.Hashes, base.Hashes)
	for i, p := range d.Pages {
		if p >= d.PageCount {
			return nil, fmt.Errorf(
				"backup: hash delta page %d out of range (count %d)",
				p,
				d.PageCount,
			)
		}
		copy(
			out.Hashes[p*pageHashSize:],
			d.Hashes[i*pageHashSize:(i+1)*pageHashSize],
		)
	}
	if len(out.Hashes) == 0 {
		out.Hashes = nil
	}
	return out, nil
}

// MaterializeHashMap walks a newest-to-oldest blob chain until it finds a
// keyframe, then replays the deltas oldest-first.
func MaterializeHashMap(
	fetch func(pack.BlobID) ([]byte, error),
	chain []pack.BlobID,
) (*PageHashMap, error) {
	var deltas []*PageHashDelta
	for i, id := range chain {
		data, err := fetch(id)
		if err != nil {
			return nil, fmt.Errorf(
				"backup: fetching hash-map chain blob %s: %w",
				id,
				err,
			)
		}
		if len(data) >= 4 && string(data[:4]) == hashKeyframeMagic {
			m, err := DecodeHashKeyframe(data)
			if err != nil {
				return nil, err
			}
			for j := len(deltas) - 1; j >= 0; j-- { //nolint:modernize
				m, err = ApplyHashDelta(m, deltas[j])
				if err != nil {
					return nil, err
				}
			}
			return m, nil
		}
		d, err := DecodeHashDelta(data)
		if err != nil {
			return nil, fmt.Errorf(
				"backup: hash-map chain blob %d (%s): %w",
				i,
				id,
				err,
			)
		}
		deltas = append(deltas, d)
	}
	return nil, fmt.Errorf("backup: hash-map chain of %d blobs has no keyframe", len(chain))
}
