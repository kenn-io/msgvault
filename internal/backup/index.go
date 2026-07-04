package backup

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/oklog/ulid/v2"

	"go.kenn.io/msgvault/internal/pack"
)

const (
	indexMagic     = "MVIX"
	indexVersion   = 1
	indexEntrySize = 32 + 16 + 8 + 8 + 1
	indexExt       = ".mvidx"
	trailerHashLen = 32
)

// IndexEntry maps one blob to its location inside a sealed pack.
type IndexEntry struct {
	Blob      pack.BlobID
	PackID    string
	Offset    uint64
	StoredLen uint64
	Flags     pack.BlobFlags
}

// EncodeIndex serializes entries sorted ascending by blob ID with a SHA-256
// integrity trailer.
func EncodeIndex(entries []IndexEntry) ([]byte, error) {
	sorted := append([]IndexEntry{}, entries...)
	sort.Slice(sorted, func(i, j int) bool {
		return bytes.Compare(sorted[i].Blob[:], sorted[j].Blob[:]) < 0
	})
	for i := 1; i < len(sorted); i++ {
		if sorted[i-1].Blob == sorted[i].Blob {
			return nil, fmt.Errorf(
				"backup: encoding index: duplicate blob id %s",
				sorted[i].Blob,
			)
		}
	}
	buf := make(
		[]byte,
		0,
		4+2+4+len(sorted)*indexEntrySize+trailerHashLen,
	)
	buf = append(buf, indexMagic...)
	buf = binary.LittleEndian.AppendUint16(buf, indexVersion)
	//nolint:gosec // entry counts are far below u32 range
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(sorted)))
	for _, e := range sorted {
		u, err := ulid.Parse(strings.ToUpper(e.PackID))
		if err != nil {
			return nil, fmt.Errorf(
				"backup: encoding index: bad pack id %q: %w",
				e.PackID,
				err,
			)
		}
		buf = append(buf, e.Blob[:]...)
		buf = append(buf, u[:]...)
		buf = binary.LittleEndian.AppendUint64(buf, e.Offset)
		buf = binary.LittleEndian.AppendUint64(buf, e.StoredLen)
		buf = append(buf, byte(e.Flags))
	}
	sum := sha256.Sum256(buf)
	return append(buf, sum[:]...), nil
}

// DecodeIndex parses and integrity-checks an index object.
func DecodeIndex(data []byte) ([]IndexEntry, error) {
	const header = 4 + 2 + 4
	if len(data) < header+trailerHashLen {
		return nil, fmt.Errorf("backup: index truncated (%d bytes)", len(data))
	}
	body, trailer := data[:len(data)-trailerHashLen],
		data[len(data)-trailerHashLen:]
	sum := sha256.Sum256(body)
	if !bytes.Equal(sum[:], trailer) {
		return nil, errors.New("backup: index integrity check failed")
	}
	if string(body[:4]) != indexMagic {
		return nil, errors.New("backup: bad index magic")
	}
	if v := binary.LittleEndian.Uint16(body[4:6]); v != indexVersion {
		return nil, fmt.Errorf("backup: unsupported index version %d", v)
	}
	count := binary.LittleEndian.Uint32(body[6:10])
	//nolint:gosec // count is constrained by body size
	if uint64(len(body)-header) != uint64(count)*indexEntrySize {
		return nil, fmt.Errorf(
			"backup: index body size mismatch (count %d)",
			count,
		)
	}
	entries := make([]IndexEntry, 0, count)
	off := header
	var prev []byte
	for range count {
		var e IndexEntry
		copy(e.Blob[:], body[off:off+32])
		if prev != nil && bytes.Compare(prev, e.Blob[:]) >= 0 {
			return nil, errors.New(
				"backup: index entries not strictly sorted",
			)
		}
		prev = body[off : off+32]
		var u ulid.ULID
		copy(u[:], body[off+32:off+48])
		e.PackID = strings.ToLower(u.String())
		e.Offset = binary.LittleEndian.Uint64(body[off+48 : off+56])
		e.StoredLen = binary.LittleEndian.Uint64(body[off+56 : off+64])
		e.Flags = pack.BlobFlags(body[off+64])
		entries = append(entries, e)
		off += indexEntrySize
	}
	return entries, nil
}

// WriteIndex publishes a new immutable index object and returns its ULID.
func (r *Repo) WriteIndex(entries []IndexEntry) (string, error) {
	data, err := EncodeIndex(entries)
	if err != nil {
		return "", err
	}
	id := pack.NewPackID()
	if err := writeFileAtomic(
		r,
		filepath.Join(indexesDirName, id+indexExt),
		data,
	); err != nil {
		return "", err
	}
	return id, nil
}

// LoadBlobIndex reads every index object and returns the union blob map,
// trusting every *.mvidx file in the indexes directory without checking
// whether a manifest actually references it.
//
// An index can be orphaned if Create fails after WriteIndex succeeds but
// before the manifest is written (e.g. SetPageSize or WriteManifest fails
// afterward). That orphan is still safe to dedupe against: WriteIndex is
// only ever called after appender.Finish() has sealed its packs durably
// (create.go), so every entry in a published index — orphaned or not —
// points at a real, durable, sealed blob. A later run that re-derives the
// same content will see it already indexed here and skip re-storing it,
// reusing the orphaned blob instead of duplicating it. There is no unsafe
// window: the failure that can orphan an index happens strictly after the
// data it describes is already durable.
func (r *Repo) LoadBlobIndex() (map[pack.BlobID]IndexEntry, error) {
	entries, err := os.ReadDir(r.Path(indexesDirName))
	if err != nil {
		return nil, fmt.Errorf("backup: reading indexes dir: %w", err)
	}
	m := map[pack.BlobID]IndexEntry{}
	for _, de := range entries {
		if !strings.HasSuffix(de.Name(), indexExt) {
			continue
		}
		data, err := os.ReadFile(r.Path(indexesDirName, de.Name()))
		if err != nil {
			return nil, fmt.Errorf(
				"backup: reading index %s: %w",
				de.Name(),
				err,
			)
		}
		idx, err := DecodeIndex(data)
		if err != nil {
			return nil, fmt.Errorf(
				"backup: index %s: %w",
				de.Name(),
				err,
			)
		}
		for _, e := range idx {
			m[e.Blob] = e
		}
	}
	return m, nil
}
