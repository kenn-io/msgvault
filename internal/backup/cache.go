package backup

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
)

// LoadHashMapCache reads the disposable local hash-map cache. Any read or
// parse failure returns empty results, never an error: the cache is rebuilt
// from the repository when unusable.
func LoadHashMapCache(cacheDir, repoID string) (string, *PageHashMap, error) {
	data, err := os.ReadFile(filepath.Join(cacheDir, repoID+".hashmap"))
	if err != nil {
		return "", nil, nil //nolint:nilerr // absent/unreadable cache is a cache miss by design
	}
	if len(data) < 4 {
		return "", nil, nil
	}
	n := binary.LittleEndian.Uint32(data[:4])
	if uint64(len(data)) < 4+uint64(n) {
		return "", nil, nil
	}
	snapshotID := string(data[4 : 4+n])
	m, err := DecodeHashKeyframe(data[4+n:])
	if err != nil {
		return "", nil, nil //nolint:nilerr // corrupt cache is a cache miss by design
	}
	return snapshotID, m, nil
}

// SaveHashMapCache atomically replaces the local hash-map cache.
func SaveHashMapCache(cacheDir, repoID, snapshotID string, m *PageHashMap) error {
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return fmt.Errorf("backup: creating cache dir: %w", err)
	}
	buf := binary.LittleEndian.AppendUint32(nil, uint32(len(snapshotID))) //nolint:gosec // snapshot ids are short
	buf = append(buf, snapshotID...)
	buf = append(buf, EncodeHashKeyframe(m)...)
	tmp, err := os.CreateTemp(cacheDir, repoID+".hashmap.*")
	if err != nil {
		return fmt.Errorf("backup: creating cache temp file: %w", err)
	}
	if _, err := tmp.Write(buf); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("backup: writing cache: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("backup: closing cache: %w", err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(cacheDir, repoID+".hashmap")); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("backup: publishing cache: %w", err)
	}
	return nil
}
