package backup

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"

	"go.kenn.io/kit/pack"
)

const (
	mapKeyframeMagic = "MVMK"
	mapDeltaMagic    = "MVMD"
	pageRunSize      = 8 + 4 + 4 + 8
)

// PageRun maps a contiguous page range to consecutive bytes within a blob.
type PageRun struct {
	StartPage  uint64
	PageCount  uint32
	BlobIndex  uint32
	BlobOffset uint64
}

// PageMap is the run-length-encoded page -> (blob, offset) mapping
// (docs/architecture/backup-format.md, Page-Map Objects). A keyframe covers every page exactly once; a delta covers
// only the ranges it replaces.
type PageMap struct {
	PageSize  uint32
	PageCount uint64
	Blobs     []pack.BlobID
	Runs      []PageRun
}

// EncodePageMap serializes a page map as a keyframe or delta object.
func EncodePageMap(m *PageMap, delta bool) []byte {
	magic := mapKeyframeMagic
	if delta {
		magic = mapDeltaMagic
	}
	buf := make([]byte, 0, 6+4+8+4+len(m.Blobs)*32+4+len(m.Runs)*pageRunSize+trailerHashLen)
	buf = append(buf, magic...)
	buf = binary.LittleEndian.AppendUint16(buf, mapObjectVersion)
	buf = binary.LittleEndian.AppendUint32(buf, m.PageSize)
	buf = binary.LittleEndian.AppendUint64(buf, m.PageCount)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(m.Blobs))) //nolint:gosec // blob counts fit u32
	for _, b := range m.Blobs {
		buf = append(buf, b[:]...)
	}
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(m.Runs))) //nolint:gosec // run counts fit u32
	for _, r := range m.Runs {
		buf = binary.LittleEndian.AppendUint64(buf, r.StartPage)
		buf = binary.LittleEndian.AppendUint32(buf, r.PageCount)
		buf = binary.LittleEndian.AppendUint32(buf, r.BlobIndex)
		buf = binary.LittleEndian.AppendUint64(buf, r.BlobOffset)
	}
	sum := sha256.Sum256(buf)
	return append(buf, sum[:]...)
}

// DecodePageMap parses either page-map object form and validates structure.
func DecodePageMap(data []byte) (*PageMap, bool, error) {
	if len(data) < 4 {
		return nil, false, fmt.Errorf("backup: page map truncated (%d bytes)", len(data))
	}
	magic := string(data[:4])
	isDelta := magic == mapDeltaMagic
	if !isDelta && magic != mapKeyframeMagic {
		return nil, false, fmt.Errorf("backup: bad page-map magic %q", magic)
	}
	body, err := checkMapObject(data, magic)
	if err != nil {
		return nil, false, err
	}
	if len(body) < 6+4+8+4 {
		return nil, false, fmt.Errorf("backup: page map truncated") //nolint:perfsprint
	}
	m := &PageMap{
		PageSize:  binary.LittleEndian.Uint32(body[6:10]),
		PageCount: binary.LittleEndian.Uint64(body[10:18]),
	}
	blobCount := binary.LittleEndian.Uint32(body[18:22])
	off := 22
	if uint64(len(body)) < uint64(off)+uint64(blobCount)*32+4 {
		return nil, false, fmt.Errorf("backup: page map blob table truncated") //nolint:perfsprint
	}
	for i := uint32(0); i < blobCount; i++ { //nolint:intrange,modernize // uint32 iteration requires standard for loop
		var b pack.BlobID
		copy(b[:], body[off:off+32])
		m.Blobs = append(m.Blobs, b)
		off += 32
	}
	runCount := binary.LittleEndian.Uint32(body[off : off+4])
	off += 4
	if uint64(len(body)-off) != uint64(runCount)*pageRunSize { //nolint:gosec // no integer overflow
		return nil, false, fmt.Errorf("backup: page map run table size mismatch (runs %d)", runCount)
	}
	var prevEnd uint64
	for i := uint32(0); i < runCount; i++ { //nolint:intrange,modernize // uint32 iteration requires standard for loop
		r := PageRun{
			StartPage:  binary.LittleEndian.Uint64(body[off : off+8]),
			PageCount:  binary.LittleEndian.Uint32(body[off+8 : off+12]),
			BlobIndex:  binary.LittleEndian.Uint32(body[off+12 : off+16]),
			BlobOffset: binary.LittleEndian.Uint64(body[off+16 : off+24]),
		}
		off += pageRunSize
		if r.PageCount == 0 {
			return nil, false, fmt.Errorf("backup: page map run %d is empty", i)
		}
		if r.BlobIndex >= blobCount {
			return nil, false, fmt.Errorf("backup: page map run %d references blob %d of %d", i, r.BlobIndex, blobCount)
		}
		if i > 0 && r.StartPage < prevEnd {
			return nil, false, fmt.Errorf("backup: page map runs overlap at page %d", r.StartPage)
		}
		// r.StartPage+uint64(r.PageCount) can wrap for a near-MaxUint64
		// StartPage, which would otherwise let a huge run slip past the
		// "exceeds page count" check below. Validate via subtraction
		// instead: r.PageCount is already known non-zero (checked above), so
		// this can't itself underflow once the count bound holds.
		if uint64(r.PageCount) > m.PageCount || r.StartPage > m.PageCount-uint64(r.PageCount) {
			return nil, false, fmt.Errorf("backup: page map run %d exceeds page count %d", i, m.PageCount)
		}
		prevEnd = r.StartPage + uint64(r.PageCount)
		m.Runs = append(m.Runs, r)
	}
	return m, isDelta, nil
}

// CheckCoverage verifies a complete map covers every page exactly once.
func (m *PageMap) CheckCoverage() error {
	var next uint64
	for i, r := range m.Runs {
		if r.StartPage != next {
			return fmt.Errorf("backup: page map gap: run %d starts at %d, expected %d", i, r.StartPage, next)
		}
		next = r.StartPage + uint64(r.PageCount)
	}
	if next != m.PageCount {
		return fmt.Errorf("backup: page map covers %d of %d pages", next, m.PageCount)
	}
	return nil
}

// Lookup returns the blob and byte offset holding a page's content.
func (m *PageMap) Lookup(page uint64) (pack.BlobID, uint64, error) {
	// Equivalent to StartPage+PageCount > page but without the addition,
	// which could wrap for a near-MaxUint64 StartPage on a PageMap built
	// directly rather than through DecodePageMap's validation.
	i := sort.Search(len(m.Runs), func(i int) bool {
		r := m.Runs[i]
		return page < r.StartPage || page-r.StartPage < uint64(r.PageCount)
	})
	if i == len(m.Runs) || m.Runs[i].StartPage > page {
		return pack.BlobID{}, 0, fmt.Errorf("backup: page %d not mapped", page)
	}
	r := m.Runs[i]
	return m.Blobs[r.BlobIndex], r.BlobOffset + (page-r.StartPage)*uint64(m.PageSize), nil
}

// ApplyPageMapDelta merges a delta into a base map: delta ranges win, base
// runs are split around them, and the result is resized to the delta's
// page count.
func ApplyPageMapDelta(base, delta *PageMap) (*PageMap, error) {
	if base.PageSize != delta.PageSize {
		return nil, fmt.Errorf("backup: page map delta page size %d does not match base %d", delta.PageSize, base.PageSize)
	}
	out := &PageMap{PageSize: base.PageSize, PageCount: delta.PageCount}
	blobIdx := map[pack.BlobID]uint32{}
	addBlob := func(id pack.BlobID) uint32 {
		if i, ok := blobIdx[id]; ok {
			return i
		}
		i := uint32(len(out.Blobs)) //nolint:gosec // blob counts fit u32
		out.Blobs = append(out.Blobs, id)
		blobIdx[id] = i
		return i
	}
	emit := func(br PageRun, fStart, fEnd uint64) {
		f := PageRun{
			StartPage:  fStart,
			PageCount:  uint32(fEnd - fStart), //nolint:gosec // bounded by run length
			BlobIndex:  addBlob(base.Blobs[br.BlobIndex]),
			BlobOffset: br.BlobOffset + (fStart-br.StartPage)*uint64(base.PageSize),
		}
		if f.StartPage >= out.PageCount {
			return
		}
		if fEnd > out.PageCount {
			f.PageCount = uint32(out.PageCount - f.StartPage) //nolint:gosec // bounded by run length
		}
		out.Runs = append(out.Runs, f)
	}

	// Base runs, minus delta-covered intervals, truncated to the new count.
	// Both run lists are sorted by StartPage and internally non-overlapping,
	// so a single two-pointer sweep subtracts the delta intervals in O(n+m)
	// rather than the earlier O(base * delta) nested scan. di advances only
	// past delta runs that end before the current base run begins; a delta
	// run spanning several base runs is revisited by the inner loop but never
	// re-scanned from the start.
	di := 0
	for _, br := range base.Runs {
		brEnd := br.StartPage + uint64(br.PageCount)
		for di < len(delta.Runs) && delta.Runs[di].StartPage+uint64(delta.Runs[di].PageCount) <= br.StartPage {
			di++
		}
		curStart := br.StartPage
		for j := di; j < len(delta.Runs) && delta.Runs[j].StartPage < brEnd; j++ {
			dStart := delta.Runs[j].StartPage
			dEnd := dStart + uint64(delta.Runs[j].PageCount)
			if dStart > curStart {
				emit(br, curStart, min(dStart, brEnd))
			}
			if dEnd > curStart {
				curStart = dEnd
			}
			if curStart >= brEnd {
				break
			}
		}
		if curStart < brEnd {
			emit(br, curStart, brEnd)
		}
	}
	for _, dr := range delta.Runs {
		dr.BlobIndex = addBlob(delta.Blobs[dr.BlobIndex])
		// Same overflow hazard as DecodePageMap's bounds check: guard via
		// subtraction rather than dr.StartPage+uint64(dr.PageCount), which
		// can wrap for a near-MaxUint64 StartPage.
		if uint64(dr.PageCount) > out.PageCount || dr.StartPage > out.PageCount-uint64(dr.PageCount) {
			return nil, fmt.Errorf("backup: page map delta run at %d exceeds new page count %d", dr.StartPage, out.PageCount)
		}
		out.Runs = append(out.Runs, dr)
	}
	sort.Slice(out.Runs, func(i, j int) bool { return out.Runs[i].StartPage < out.Runs[j].StartPage })
	return out, nil
}

// MaterializePageMap walks a newest-to-oldest blob chain to a keyframe and
// replays the deltas oldest-first.
func MaterializePageMap(fetch func(pack.BlobID) ([]byte, error), chain []pack.BlobID) (*PageMap, error) {
	var deltas []*PageMap
	for i, id := range chain {
		data, err := fetch(id)
		if err != nil {
			return nil, fmt.Errorf("backup: fetching page-map chain blob %s: %w", id, err)
		}
		m, isDelta, err := DecodePageMap(data)
		if err != nil {
			return nil, fmt.Errorf("backup: page-map chain blob %d (%s): %w", i, id, err)
		}
		if !isDelta {
			for j := len(deltas) - 1; j >= 0; j-- { //nolint:modernize // backward loop required for delta application order
				m, err = ApplyPageMapDelta(m, deltas[j])
				if err != nil {
					return nil, err
				}
			}
			return m, nil
		}
		deltas = append(deltas, m)
	}
	return nil, fmt.Errorf("backup: page-map chain of %d blobs has no keyframe", len(chain))
}
