# Packed Attachment Storage — Phase 1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Delivery steps 1–2 of `docs/internal/packed-attachments-design.md`: unify all attachment writers onto the canonical store function, add the pack index schema, and build `internal/blobstore` with the read-path switch. No packer yet — the blobstore is inert (loose fallback covers everything) until phase 2.

**Architecture:** New streaming variant of `export.StoreAttachmentFile` absorbs the three hand-rolled importer writers. Two new tables (`attachment_pack_index`, `attachment_packs`) go into both schema files. `internal/blobstore` resolves hash → pack via the index (with the design doc's two race-retry rules) and falls back to loose files; the daemon HTTP attachment endpoint switches to it.

**Tech Stack:** Go, `go.kenn.io/kit/pack` v0.3.0 (already in go.mod), SQLite + PostgreSQL via `internal/store` dialects, testify.

## Global Constraints

- All tests use testify: `require.X` halts (setup), `assert.X` continues (independent checks). Argument order is `(want, got)`. Never `t.Fatalf`/`t.Errorf`.
- Table-driven tests where there are 3+ cases of the same shape.
- After any Go change: `go fmt ./...` and `go vet ./...`; stage ALL resulting changes.
- Run `make lint-ci` before finishing (CI's testify-helper-check is not in `make lint`).
- Commit after every task; never `--amend`; imperative subject ≤72 chars.
- Test fixtures: only synthetic names/emails (`user@example.com`), never real PII.
- All DB operations route through the `Store` struct (`internal/store`).
- Work on the current branch `packed-attachments`.
- The design doc is the contract: `docs/internal/packed-attachments-design.md`.

---

### Task 1: Streaming store helper `StoreAttachmentFromPath`

**Files:**
- Modify: `internal/export/store_attachment.go`
- Test: `internal/export/store_attachment_from_path_test.go` (create)

**Interfaces:**
- Consumes: existing unexported helpers `prepareStorageDir`, `ensureSubdirSafe`, `validateExistingAttachmentFile`, and `writeAtomicFile` (refactored here into a streaming core).
- Produces: `func StoreAttachmentFromPath(attachmentsDir, srcPath string, maxSize int64) (storagePath, contentHash string, size int64, err error)` — Tasks 2 and 3 call this. Contract: on any failure before the source is hashed, `contentHash` is `""`; on failure after hashing, `contentHash` (and `size`) are returned alongside the error with `storagePath == ""`. `maxSize <= 0` means unlimited.

- [ ] **Step 1: Write the failing tests**

Create `internal/export/store_attachment_from_path_test.go`:

```go
package export

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeSrcFile(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, content, 0o600))
	return p
}

func TestStoreAttachmentFromPath(t *testing.T) {
	content := []byte("hello packed world")
	sum := sha256.Sum256(content)
	wantHash := hex.EncodeToString(sum[:])

	t.Run("stores new file at canonical path", func(t *testing.T) {
		attDir := t.TempDir()
		src := writeSrcFile(t, t.TempDir(), "a.bin", content)

		rel, hash, size, err := StoreAttachmentFromPath(attDir, src, 0)
		require.NoError(t, err)
		assert.Equal(t, wantHash[:2]+"/"+wantHash, rel)
		assert.Equal(t, wantHash, hash)
		assert.Equal(t, int64(len(content)), size)

		got, err := os.ReadFile(filepath.Join(attDir, wantHash[:2], wantHash))
		require.NoError(t, err)
		assert.Equal(t, content, got)
	})

	t.Run("dedups against valid existing file", func(t *testing.T) {
		attDir := t.TempDir()
		src := writeSrcFile(t, t.TempDir(), "a.bin", content)
		_, _, _, err := StoreAttachmentFromPath(attDir, src, 0)
		require.NoError(t, err)

		rel, hash, _, err := StoreAttachmentFromPath(attDir, src, 0)
		require.NoError(t, err)
		assert.Equal(t, wantHash[:2]+"/"+wantHash, rel)
		assert.Equal(t, wantHash, hash)
	})

	t.Run("rejects source larger than maxSize but returns no hash", func(t *testing.T) {
		attDir := t.TempDir()
		src := writeSrcFile(t, t.TempDir(), "a.bin", content)

		_, hash, _, err := StoreAttachmentFromPath(attDir, src, 4)
		require.Error(t, err)
		assert.Empty(t, hash)
	})

	t.Run("errors on missing source", func(t *testing.T) {
		_, hash, _, err := StoreAttachmentFromPath(t.TempDir(), filepath.Join(t.TempDir(), "gone"), 0)
		require.Error(t, err)
		assert.Empty(t, hash)
	})

	t.Run("errors on symlink source", func(t *testing.T) {
		srcDir := t.TempDir()
		target := writeSrcFile(t, srcDir, "target.bin", content)
		link := filepath.Join(srcDir, "link.bin")
		require.NoError(t, os.Symlink(target, link))

		_, _, _, err := StoreAttachmentFromPath(t.TempDir(), link, 0)
		require.Error(t, err)
	})

	t.Run("errors on corrupt existing file", func(t *testing.T) {
		attDir := t.TempDir()
		// Pre-plant a wrong-content file at the canonical path.
		require.NoError(t, os.MkdirAll(filepath.Join(attDir, wantHash[:2]), 0o700))
		require.NoError(t, os.WriteFile(
			filepath.Join(attDir, wantHash[:2], wantHash), []byte("XXXXXXXXXXXXXXXXXX"), 0o600))

		src := writeSrcFile(t, t.TempDir(), "a.bin", content)
		_, _, _, err := StoreAttachmentFromPath(attDir, src, 0)
		require.Error(t, err)
	})
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/export/ -run TestStoreAttachmentFromPath -v`
Expected: FAIL — `undefined: StoreAttachmentFromPath`

- [ ] **Step 3: Implement**

In `internal/export/store_attachment.go`:

3a. Refactor `writeAtomicFile` into a streaming core. Replace the current `writeAtomicFile` body so both callers share one implementation:

```go
// writeAtomicFile writes data to a temp file alongside fullPath and renames
// it into place. On rename conflict (concurrent writer), validates the
// existing file instead.
func writeAtomicFile(fullPath string, data []byte, expectedSize int64, expectedHash string) error {
	return writeAtomicFileStream(fullPath, bytes.NewReader(data), expectedSize, expectedHash)
}

// writeAtomicFileStream is writeAtomicFile for a streaming source.
func writeAtomicFileStream(fullPath string, src io.Reader, expectedSize int64, expectedHash string) error {
	dir := filepath.Dir(fullPath)
	base := filepath.Base(fullPath)

	tmp, err := os.CreateTemp(dir, base+".tmp.")
	if err != nil {
		return fmt.Errorf("create temp attachment file: %w", err)
	}
	tmpPath := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	if err := fileutil.SecureChmod(tmpPath, 0600); err != nil {
		return fmt.Errorf("chmod temp attachment file: %w", err)
	}

	if _, err := io.Copy(tmp, src); err != nil {
		return fmt.Errorf("write attachment file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close attachment file: %w", err)
	}

	if err := os.Rename(tmpPath, fullPath); err != nil {
		if _, statErr := os.Lstat(fullPath); statErr == nil {
			removeTmp = false
			_ = os.Remove(tmpPath)
			return validateExistingAttachmentFile(fullPath, expectedSize, expectedHash)
		}
		return fmt.Errorf("rename attachment file into place: %w", err)
	}
	removeTmp = false
	return nil
}
```

Add `"bytes"` to imports.

3b. Add the streaming store function:

```go
// StoreAttachmentFromPath streams the regular file at srcPath into
// content-addressed storage under attachmentsDir (hash[:2]/hash), hashing
// without loading the file into memory. maxSize > 0 rejects larger sources.
//
// Returns the storage path relative to attachmentsDir, the content hash, and
// the source size. On failures after the source was hashed, contentHash and
// size are still returned (with an empty storage path) so callers can record
// metadata for content they could not store.
func StoreAttachmentFromPath(attachmentsDir, srcPath string, maxSize int64) (string, string, int64, error) {
	if attachmentsDir == "" || srcPath == "" {
		return "", "", 0, fmt.Errorf("attachments dir and source path are required")
	}
	linfo, err := os.Lstat(srcPath)
	if err != nil {
		return "", "", 0, fmt.Errorf("lstat attachment source: %w", err)
	}
	if !linfo.Mode().IsRegular() {
		return "", "", 0, fmt.Errorf("attachment source %q is not a regular file", srcPath)
	}
	size := linfo.Size()
	if maxSize > 0 && size > maxSize {
		return "", "", 0, fmt.Errorf("attachment source %q is %d bytes (max %d)", srcPath, size, maxSize)
	}

	f, err := os.Open(srcPath)
	if err != nil {
		return "", "", 0, fmt.Errorf("open attachment source: %w", err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", "", 0, fmt.Errorf("hash attachment source: %w", err)
	}
	contentHash := hex.EncodeToString(h.Sum(nil))

	hashPrefix := contentHash[:2]
	storagePath := path.Join(hashPrefix, contentHash)

	baseDir, err := prepareStorageDir(attachmentsDir)
	if err != nil {
		return "", contentHash, size, err
	}
	if err := ensureSubdirSafe(baseDir, hashPrefix); err != nil {
		return "", contentHash, size, err
	}

	fullPath := filepath.Join(baseDir, hashPrefix, contentHash)
	if _, err := os.Lstat(fullPath); err == nil {
		if err := validateExistingAttachmentFile(fullPath, size, contentHash); err != nil {
			return "", contentHash, size, err
		}
		return storagePath, contentHash, size, nil
	} else if !os.IsNotExist(err) {
		return "", contentHash, size, fmt.Errorf("lstat attachment file: %w", err)
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", contentHash, size, fmt.Errorf("rewind attachment source: %w", err)
	}
	if err := writeAtomicFileStream(fullPath, f, size, contentHash); err != nil {
		return "", contentHash, size, err
	}
	return storagePath, contentHash, size, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/export/ -v`
Expected: PASS (all package tests, including the pre-existing `StoreAttachmentFile` tests, still green)

- [ ] **Step 5: Format, vet, commit**

```bash
go fmt ./... && go vet ./...
git add internal/export/
git commit -m "Add streaming StoreAttachmentFromPath to export package"
```

---

### Task 2: Unify WhatsApp media writes

**Files:**
- Modify: `internal/whatsapp/importer.go:594-648` (tail of `handleMediaFile`)
- Test: `internal/whatsapp/importer_media_test.go` (create)

**Interfaces:**
- Consumes: `export.StoreAttachmentFromPath(attachmentsDir, srcPath string, maxSize int64) (string, string, int64, error)` from Task 1.
- Produces: unchanged `(imp *Importer) handleMediaFile(media waMedia, opts ImportOptions) (string, string)` contract — `(relStoragePath, contentHash)`, both empty when the file is missing/oversized, hash-only when storage failed after hashing.

- [ ] **Step 1: Write the failing test**

Create `internal/whatsapp/importer_media_test.go`:

```go
package whatsapp

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandleMediaFile(t *testing.T) {
	content := []byte("whatsapp media bytes")
	sum := sha256.Sum256(content)
	wantHash := hex.EncodeToString(sum[:])

	newOpts := func(t *testing.T) ImportOptions {
		t.Helper()
		mediaDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(mediaDir, "photo.jpg"), content, 0o600))
		return ImportOptions{MediaDir: mediaDir, AttachmentsDir: t.TempDir()}
	}
	media := func(rel string) waMedia {
		return waMedia{FilePath: sql.NullString{String: rel, Valid: true}}
	}
	imp := &Importer{}

	t.Run("stores media at canonical content-addressed path", func(t *testing.T) {
		opts := newOpts(t)
		rel, hash := imp.handleMediaFile(media("photo.jpg"), opts)
		assert.Equal(t, filepath.Join(wantHash[:2], wantHash), rel)
		assert.Equal(t, wantHash, hash)

		got, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, wantHash[:2], wantHash))
		require.NoError(t, err)
		assert.Equal(t, content, got)
	})

	t.Run("returns empty for missing media file", func(t *testing.T) {
		opts := newOpts(t)
		rel, hash := imp.handleMediaFile(media("nope.jpg"), opts)
		assert.Empty(t, rel)
		assert.Empty(t, hash)
	})

	t.Run("returns empty for oversized media file", func(t *testing.T) {
		opts := newOpts(t)
		opts.MaxMediaFileSize = 4
		rel, hash := imp.handleMediaFile(media("photo.jpg"), opts)
		assert.Empty(t, rel)
		assert.Empty(t, hash)
	})
}
```

If `ImportOptions`/`waMedia` field names differ from the above, read their definitions in `internal/whatsapp/` and adjust the test — do not adjust the production types.

- [ ] **Step 2: Run test to verify current behavior baseline**

Run: `go test ./internal/whatsapp/ -run TestHandleMediaFile -v`
Expected: PASS already (the test pins the existing contract). If it fails, fix the test against current behavior first — this test must be green before and after the refactor.

- [ ] **Step 3: Replace the hand-rolled write**

In `internal/whatsapp/importer.go`, keep everything in `handleMediaFile` up to and including the max-size check (lines ~585–592), then replace the rest of the function body (streaming hash + manual copy, lines ~594–648) with:

```go
	relStoragePath, contentHash, _, err := export.StoreAttachmentFromPath(
		opts.AttachmentsDir, fullPath, maxSize)
	if err != nil {
		// contentHash is non-empty when hashing succeeded but storage failed;
		// callers use it to attach metadata to the unstored attachment row.
		return "", contentHash
	}
	return relStoragePath, contentHash
}
```

Add import `"go.kenn.io/msgvault/internal/export"`; remove now-unused imports (`crypto/sha256`, `encoding/hex` — only if unused elsewhere in the file; `goimports -w` handles it).

- [ ] **Step 4: Run tests**

Run: `go test ./internal/whatsapp/ -v`
Expected: PASS

- [ ] **Step 5: Format, vet, commit**

```bash
go fmt ./... && go vet ./...
git add internal/whatsapp/
git commit -m "Unify WhatsApp media writes onto StoreAttachmentFromPath"
```

---

### Task 3: Unify FB Messenger attachment writes

**Files:**
- Modify: `internal/fbmessenger/importer.go:837-896` (`handleAttachment`)

**Interfaces:**
- Consumes: `export.StoreAttachmentFromPath` from Task 1.
- Produces: unchanged `func handleAttachment(att Attachment, attachmentsDir string) (string, string, int)` contract — `(relPath, contentHash, size)`.

- [ ] **Step 1: Run the existing media import tests as baseline**

Run: `go test ./internal/fbmessenger/ -v`
Expected: PASS (fixtures `json_with_media`, `html_with_media`, etc. exercise `handleAttachment` through the real import path)

- [ ] **Step 2: Replace the function body**

Replace `handleAttachment` (`internal/fbmessenger/importer.go:837-896`) with:

```go
func handleAttachment(att Attachment, attachmentsDir string) (string, string, int) {
	if attachmentsDir == "" || att.AbsPath == "" {
		return "", "", 0
	}
	rel, contentHash, size, err := export.StoreAttachmentFromPath(attachmentsDir, att.AbsPath, 0)
	if err != nil {
		// contentHash is set when hashing succeeded but storage failed —
		// preserved so the caller's failed-store bookkeeping still works.
		return "", contentHash, 0
	}
	return rel, contentHash, int(size)
}
```

Add import `"go.kenn.io/msgvault/internal/export"`; drop imports that become unused.

Note one deliberate behavior change: the old code silently accepted a symlink at `att.AbsPath` if `os.Lstat` said otherwise-regular (it didn't — it rejected non-regular via `Lstat`, same as the new helper). `StoreAttachmentFromPath` preserves the `Lstat`-regular check, so behavior is identical.

- [ ] **Step 3: Run tests**

Run: `go test ./internal/fbmessenger/ -v`
Expected: PASS — identical results to the Step 1 baseline

- [ ] **Step 4: Format, vet, commit**

```bash
go fmt ./... && go vet ./...
git add internal/fbmessenger/
git commit -m "Unify FB Messenger attachment writes onto StoreAttachmentFromPath"
```

---

### Task 4: Unify SyncTech SMS attachment writes (canonical paths)

**Files:**
- Modify: `internal/synctechsms/importer.go:440-473` (`importMMSAttachments` loop body)
- Test: `internal/synctechsms/importer_test.go` (update path expectations)

**Interfaces:**
- Consumes: existing `export.StoreAttachmentFile(attachmentsDir string, att *mime.Attachment) (string, error)` (bytes are already in memory here) and `mime.Attachment{Filename, ContentType, Content, ContentHash}`.
- Produces: new SyncTech imports store at canonical `<hash[:2]>/<hash>` — the `synctech-sms/` namespace is retired for new writes. Legacy rows are untouched (phase 2's packer canonicalizes them, per the design doc).

- [ ] **Step 1: Update test expectations to canonical paths**

In `internal/synctechsms/importer_test.go`, find assertions on `storage_path` values containing `synctech-sms/` (search: `rg -n "synctech-sms" internal/synctechsms/`) and change the expected values to `<hash[:2]>/<hash>` form. Also assert the file lands at `<attachmentsDir>/<hash[:2]>/<hash>` on disk.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/synctechsms/ -v`
Expected: FAIL on the updated path expectations

- [ ] **Step 3: Replace the manual write**

In `importMMSAttachments` (`internal/synctechsms/importer.go`), replace this block:

```go
		storagePath := filepath.Join("synctech-sms", hash[:2], hash)
		fullPath := filepath.Join(i.opts.AttachmentsDir, storagePath)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o700); err != nil {
			return count, fmt.Errorf("create attachment directory: %w", err)
		}
		if err := os.WriteFile(fullPath, part.Data, 0o600); err != nil {
			return count, fmt.Errorf("write attachment: %w", err)
		}
		if err := i.store.UpsertAttachment(messageID, filename, part.ContentType, storagePath, hash, len(part.Data)); err != nil {
```

with:

```go
		att := &mime.Attachment{
			Filename:    filename,
			ContentType: part.ContentType,
			Content:     part.Data,
		}
		storagePath, err := export.StoreAttachmentFile(i.opts.AttachmentsDir, att)
		if err != nil {
			return count, fmt.Errorf("store MMS attachment: %w", err)
		}
		if err := i.store.UpsertAttachment(messageID, filename, part.ContentType, storagePath, att.ContentHash, len(part.Data)); err != nil {
```

The pre-existing `sha256.Sum256(part.Data)` hash computation above the block becomes redundant — delete it (`StoreAttachmentFile` computes and fills `att.ContentHash`) unless `hash` is used elsewhere in the loop; if it is, keep it and pass `att.ContentHash` to `UpsertAttachment` anyway. Add imports `"go.kenn.io/msgvault/internal/export"` and `"go.kenn.io/msgvault/internal/mime"`; drop unused ones. Match the file's existing import alias if `export` is already imported under another name.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/synctechsms/ -v`
Expected: PASS

- [ ] **Step 5: Format, vet, commit**

```bash
go fmt ./... && go vet ./...
git add internal/synctechsms/
git commit -m "Store SyncTech MMS attachments at canonical content-addressed paths"
```

---

### Task 5: Pack index schema and store methods

**Files:**
- Modify: `internal/store/schema.sql` (append after the `applied_migrations` table)
- Modify: `internal/store/schema_pg.sql` (same position)
- Create: `internal/store/packs.go`
- Test: `internal/store/packs_test.go` (create)
- Modify: `docs/internal/packed-attachments-design.md` (rename `offset` → `pack_offset` in the schema snippet; `OFFSET` is a reserved word in both SQLite and PostgreSQL)

**Interfaces:**
- Produces:
  - Tables `attachment_pack_index(blob_hash PK, pack_id, pack_offset, stored_len, raw_len, flags, crc32c)` and `attachment_packs(pack_id PK, entry_count, stored_bytes, created_at)`.
  - `type PackIndexEntry struct { BlobHash, PackID string; Offset, StoredLen, RawLen int64; Flags uint8; CRC32C uint32 }`
  - `type PackRecord struct { PackID string; EntryCount, StoredBytes int64; CreatedAt time.Time }`
  - `func (s *Store) RecordPackedBlobs(rec PackRecord, entries []PackIndexEntry) error` — transactional, idempotent.
  - `func (s *Store) GetAttachmentPackEntry(blobHash string) (*PackIndexEntry, error)` — `(nil, nil)` when absent. Task 6's `blobstore.PackIndex` interface is satisfied by this method.

- [ ] **Step 1: Write the failing tests**

Create `internal/store/packs_test.go`:

```go
package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestRecordAndGetPackedBlobs(t *testing.T) {
	st := testutil.NewTestStore(t)

	rec := store.PackRecord{
		PackID:      "01hzy3v7q8r9s0t1u2v3w4x5y6",
		EntryCount:  2,
		StoredBytes: 4096,
		CreatedAt:   time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}
	entries := []store.PackIndexEntry{
		{BlobHash: "aa11223344556677889900aabbccddeeff00112233445566778899aabbccddee",
			PackID: rec.PackID, Offset: 6, StoredLen: 2048, RawLen: 4000, Flags: 1, CRC32C: 4022250974},
		{BlobHash: "bb11223344556677889900aabbccddeeff00112233445566778899aabbccddee",
			PackID: rec.PackID, Offset: 2054, StoredLen: 2048, RawLen: 2048, Flags: 0, CRC32C: 1},
	}
	require.NoError(t, st.RecordPackedBlobs(rec, entries))

	got, err := st.GetAttachmentPackEntry(entries[0].BlobHash)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, entries[0], *got)

	// CRC32C above int32 max must round-trip on both backends (BIGINT column).
	assert.Equal(t, uint32(4022250974), got.CRC32C)

	missing, err := st.GetAttachmentPackEntry(
		"cc11223344556677889900aabbccddeeff00112233445566778899aabbccddee")
	require.NoError(t, err)
	assert.Nil(t, missing)

	// Idempotent re-record (crash-reconciliation re-runs adoption).
	require.NoError(t, st.RecordPackedBlobs(rec, entries))
}
```

Check `internal/testutil` for the exact `NewTestStore` signature and whether store tests live in package `store` or `store_test`; follow the existing convention in `internal/store/*_test.go`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestRecordAndGetPackedBlobs -v`
Expected: FAIL — `undefined: store.PackRecord`

- [ ] **Step 3: Add the DDL to both schema files**

Append to `internal/store/schema.sql` AND `internal/store/schema_pg.sql` (identical text; `BIGINT` has INTEGER affinity in SQLite and is 8-byte in PostgreSQL — plain `INTEGER` would truncate uint32 CRCs and >2 GiB lengths on PostgreSQL):

```sql
-- Packed attachment storage (docs/internal/packed-attachments-design.md).
-- attachment_pack_index maps content-addressed blobs (attachment content and
-- thumbnails) to sealed pack files under attachments/packs/. Rows exist only
-- for live packed blobs; loose files have no row. pack_offset et al mirror
-- the pack footer's entry so reads need no footer parse ("offset" is a
-- reserved word in SQLite and PostgreSQL, hence the prefix).
CREATE TABLE IF NOT EXISTS attachment_pack_index (
    blob_hash   TEXT PRIMARY KEY,
    pack_id     TEXT NOT NULL,
    pack_offset BIGINT NOT NULL,
    stored_len  BIGINT NOT NULL,
    raw_len     BIGINT NOT NULL,
    flags       INTEGER NOT NULL,
    crc32c      BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_attachment_pack_index_pack
    ON attachment_pack_index(pack_id);

-- Immutable per-pack totals captured at seal/adoption. GC derives dead bytes
-- as stored_bytes minus the sum of the pack's live index rows.
CREATE TABLE IF NOT EXISTS attachment_packs (
    pack_id      TEXT PRIMARY KEY,
    entry_count  BIGINT NOT NULL,
    stored_bytes BIGINT NOT NULL,
    created_at   TEXT NOT NULL
);
```

Also update the schema snippet in `docs/internal/packed-attachments-design.md` to use `pack_offset` with a one-line note about the reserved word.

- [ ] **Step 4: Implement the store methods**

Create `internal/store/packs.go`:

```go
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PackIndexEntry mirrors one kit pack.Entry for a packed attachment blob.
// See docs/internal/packed-attachments-design.md.
type PackIndexEntry struct {
	BlobHash  string
	PackID    string
	Offset    int64
	StoredLen int64
	RawLen    int64
	Flags     uint8
	CRC32C    uint32
}

// PackRecord holds a sealed pack's immutable totals, captured at seal or
// crash-reconciliation adoption.
type PackRecord struct {
	PackID      string
	EntryCount  int64
	StoredBytes int64
	CreatedAt   time.Time
}

// RecordPackedBlobs inserts a sealed pack's record and its blob index rows in
// one transaction. Idempotent: re-recording an existing pack or blob is a
// no-op, so crash reconciliation can re-run adoption safely.
func (s *Store) RecordPackedBlobs(rec PackRecord, entries []PackIndexEntry) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin record packed blobs: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(s.dialect.InsertOrIgnore(`
		INSERT OR IGNORE INTO attachment_packs (pack_id, entry_count, stored_bytes, created_at)
		VALUES (?, ?, ?, ?)`),
		rec.PackID, rec.EntryCount, rec.StoredBytes,
		rec.CreatedAt.UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("insert attachment_packs row for %s: %w", rec.PackID, err)
	}
	for _, e := range entries {
		if _, err := tx.Exec(s.dialect.InsertOrIgnore(`
			INSERT OR IGNORE INTO attachment_pack_index
			    (blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c)
			VALUES (?, ?, ?, ?, ?, ?, ?)`),
			e.BlobHash, e.PackID, e.Offset, e.StoredLen, e.RawLen,
			int64(e.Flags), int64(e.CRC32C)); err != nil {
			return fmt.Errorf("insert pack index row for %s: %w", e.BlobHash, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit record packed blobs: %w", err)
	}
	return nil
}

// GetAttachmentPackEntry returns the pack location of a blob, or (nil, nil)
// when the blob is not packed (loose or unknown).
func (s *Store) GetAttachmentPackEntry(blobHash string) (*PackIndexEntry, error) {
	var e PackIndexEntry
	var flags, crc int64
	err := s.db.QueryRow(`
		SELECT blob_hash, pack_id, pack_offset, stored_len, raw_len, flags, crc32c
		FROM attachment_pack_index WHERE blob_hash = ?`, blobHash).
		Scan(&e.BlobHash, &e.PackID, &e.Offset, &e.StoredLen, &e.RawLen, &flags, &crc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get pack index entry for %s: %w", blobHash, err)
	}
	e.Flags = uint8(flags)   //nolint:gosec // flags column stores a single byte
	e.CRC32C = uint32(crc)   //nolint:gosec // crc32c column stores a uint32
	return &e, nil
}
```

Follow the placeholder/rebind convention used by neighboring store files (e.g. `migrations.go` uses bare `?`; if PG paths require `s.Rebind`, mirror whichever pattern `messages.go` methods use). Check `s.dialect.InsertOrIgnore` handles the PG `ON CONFLICT DO NOTHING` translation — it is already used in `migrations.go:33`.

- [ ] **Step 5: Run tests**

Run: `go test ./internal/store/ -run TestRecordAndGetPackedBlobs -v`
Expected: PASS

If `MSGVAULT_TEST_DB` is set locally, also run: `make test-pg` (at minimum the store package) to confirm BIGINT round-tripping on PostgreSQL.

- [ ] **Step 6: Format, vet, commit**

```bash
go fmt ./... && go vet ./...
git add internal/store/ docs/internal/packed-attachments-design.md
git commit -m "Add attachment pack index schema and store methods"
```

---

### Task 6: `internal/blobstore` package

**Files:**
- Create: `internal/blobstore/blobstore.go`
- Test: `internal/blobstore/blobstore_test.go` (create)

**Interfaces:**
- Consumes: `store.PackIndexEntry` (Task 5), `go.kenn.io/kit/pack` (`OpenReader`, `Reader.ReadBlob`, `Entry`, `ParseBlobID`, `IsValidPackID`, `BlobFlags`), `export.ValidateContentHash`, `export.StoragePath`, `export.AttachmentOpener`.
- Produces (Task 7 consumes):
  - `func New(index PackIndex, attachmentsDir string) *Store`
  - `func (s *Store) Open(hash string) (io.ReadSeekCloser, int64, error)` — not-found satisfies `errors.Is(err, fs.ErrNotExist)`.
  - `func (s *Store) Opener() export.AttachmentOpener`
  - `func (s *Store) Close() error`
  - `type PackIndex interface { GetAttachmentPackEntry(blobHash string) (*store.PackIndexEntry, error) }`

- [ ] **Step 1: Write the failing tests**

Create `internal/blobstore/blobstore_test.go`:

```go
package blobstore

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"

	"go.kenn.io/msgvault/internal/store"
)

// mapIndex is a PackIndex over a plain map; nil values mean "not packed".
type mapIndex struct{ m map[string]*store.PackIndexEntry }

func (i *mapIndex) GetAttachmentPackEntry(h string) (*store.PackIndexEntry, error) {
	return i.m[h], nil
}

// buildPack seals content blobs into a pack under attachmentsDir/packs/ and
// returns index entries keyed by blob hash.
func buildPack(t *testing.T, attachmentsDir string, blobs ...[]byte) map[string]*store.PackIndexEntry {
	t.Helper()
	staging := t.TempDir()
	w, err := pack.NewWriter(staging, pack.WriterOptions{})
	require.NoError(t, err)

	for _, b := range blobs {
		_, err := w.Append(b)
		require.NoError(t, err)
	}
	id := w.ID()
	final := filepath.Join(attachmentsDir, "packs", id[:2], id+PackExt)
	require.NoError(t, os.MkdirAll(filepath.Dir(final), 0o700))
	entries, err := w.Seal(final)
	require.NoError(t, err)

	out := make(map[string]*store.PackIndexEntry, len(entries))
	for _, e := range entries {
		out[e.ID.String()] = &store.PackIndexEntry{
			BlobHash:  e.ID.String(),
			PackID:    id,
			Offset:    int64(e.Offset),
			StoredLen: int64(e.StoredLen),
			RawLen:    int64(e.RawLen),
			Flags:     uint8(e.Flags),
			CRC32C:    e.CRC32C,
		}
	}
	return out
}

func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func readAll(t *testing.T, s *Store, hash string) []byte {
	t.Helper()
	r, size, err := s.Open(hash)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, int64(len(data)), size)
	return data
}

func TestOpenPacked(t *testing.T) {
	dir := t.TempDir()
	content := []byte("packed blob content")
	idx := buildPack(t, dir, content)
	s := New(&mapIndex{m: idx}, dir)
	defer func() { _ = s.Close() }()

	assert.Equal(t, content, readAll(t, s, hashOf(content)))
}

func TestOpenLooseFallback(t *testing.T) {
	dir := t.TempDir()
	content := []byte("loose blob content")
	h := hashOf(content)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, h[:2]), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, h[:2], h), content, 0o600))

	s := New(&mapIndex{m: map[string]*store.PackIndexEntry{}}, dir)
	defer func() { _ = s.Close() }()
	assert.Equal(t, content, readAll(t, s, h))
}

func TestOpenNotFound(t *testing.T) {
	s := New(&mapIndex{m: map[string]*store.PackIndexEntry{}}, t.TempDir())
	defer func() { _ = s.Close() }()
	_, _, err := s.Open(hashOf([]byte("nowhere")))
	assert.ErrorIs(t, err, fs.ErrNotExist)
}

func TestOpenRejectsCorruptPack(t *testing.T) {
	dir := t.TempDir()
	content := []byte("integrity checked content")
	idx := buildPack(t, dir, content)
	h := hashOf(content)

	// Flip one byte of the stored blob on disk.
	e := idx[h]
	p := filepath.Join(dir, "packs", e.PackID[:2], e.PackID+PackExt)
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	require.NoError(t, err)
	buf := []byte{0}
	_, err = f.ReadAt(buf, e.Offset)
	require.NoError(t, err)
	buf[0] ^= 0xFF
	_, err = f.WriteAt(buf, e.Offset)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	s := New(&mapIndex{m: idx}, dir)
	defer func() { _ = s.Close() }()
	_, _, err = s.Open(h)
	require.Error(t, err)
}

func TestOpenRejectsInvalidPackID(t *testing.T) {
	h := hashOf([]byte("x"))
	idx := map[string]*store.PackIndexEntry{
		h: {BlobHash: h, PackID: "../../../etc/passwd"},
	}
	s := New(&mapIndex{m: idx}, t.TempDir())
	defer func() { _ = s.Close() }()
	_, _, err := s.Open(h)
	require.Error(t, err)
	assert.NotErrorIs(t, err, fs.ErrNotExist)
}

// flipIndex returns nothing on the first lookup, then the real entry —
// simulating the packer committing between a reader's index miss and its
// loose-file open (the loose file never existed here).
type flipIndex struct {
	first bool
	entry *store.PackIndexEntry
}

func (i *flipIndex) GetAttachmentPackEntry(string) (*store.PackIndexEntry, error) {
	if !i.first {
		i.first = true
		return nil, nil
	}
	return i.entry, nil
}

func TestOpenRetriesIndexAfterLooseMiss(t *testing.T) {
	dir := t.TempDir()
	content := []byte("packed between lookups")
	idx := buildPack(t, dir, content)
	h := hashOf(content)

	s := New(&flipIndex{entry: idx[h]}, dir)
	defer func() { _ = s.Close() }()
	assert.Equal(t, content, readAll(t, s, h))
}

// staleIndex returns a dangling pack entry first, then the live one —
// simulating a repack swapping rows and deleting the old pack mid-read.
type staleIndex struct {
	served bool
	stale  *store.PackIndexEntry
	live   *store.PackIndexEntry
}

func (i *staleIndex) GetAttachmentPackEntry(string) (*store.PackIndexEntry, error) {
	if !i.served {
		i.served = true
		return i.stale, nil
	}
	return i.live, nil
}

func TestOpenRetriesIndexAfterPackMiss(t *testing.T) {
	dir := t.TempDir()
	content := []byte("survives repack race")
	idx := buildPack(t, dir, content)
	h := hashOf(content)

	stale := *idx[h]
	stale.PackID = "01hzy3v7q8r9s0t1u2v3w4x5y6" // valid ULID, no such pack file

	s := New(&staleIndex{stale: &stale, live: idx[h]}, dir)
	defer func() { _ = s.Close() }()
	assert.Equal(t, content, readAll(t, s, h))
}
```

Adjust to the real kit API surface if names differ (`w.ID()`, `e.ID.String()`, `pack.WriterOptions{}` defaults): check `~/go/pkg/mod/go.kenn.io/kit@v0.3.0/pack/writer.go` and `blobid.go`. The stale ULID literal must satisfy `pack.IsValidPackID`; if it does not, generate one with `pack.NewPackID()` and use that.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/blobstore/ -v`
Expected: FAIL — package does not exist / `undefined: New`

- [ ] **Step 3: Implement**

Create `internal/blobstore/blobstore.go`:

```go
// Package blobstore reads attachment content by SHA-256 hash from packed CAS
// storage (sealed kit pack files under <attachmentsDir>/packs/) with a
// fallback to loose <hash[:2]>/<hash> files. It is the single read path for
// attachment bytes; see docs/internal/packed-attachments-design.md.
package blobstore

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"go.kenn.io/kit/pack"

	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/store"
)

// PackExt matches the backup engine's pack file extension so a future
// release can share packs between production and backup repos.
const PackExt = ".mvpack"

// maxOpenReaders bounds the cache of open pack readers (file handle plus
// parsed footer each).
const maxOpenReaders = 16

// PackIndex resolves a blob hash to its pack location. *store.Store
// implements it via GetAttachmentPackEntry; (nil, nil) means "not packed".
type PackIndex interface {
	GetAttachmentPackEntry(blobHash string) (*store.PackIndexEntry, error)
}

// Store reads attachment blobs from packs with a loose-file fallback.
type Store struct {
	index          PackIndex
	attachmentsDir string

	// mu guards readers/order and is held across packed reads so an evicted
	// reader is never closed while another goroutine is mid-ReadBlob.
	// Packed reads are short (one pread + optional zstd decode).
	mu      sync.Mutex
	readers map[string]*pack.Reader
	order   []string
}

// New creates a blob store over attachmentsDir backed by index.
func New(index PackIndex, attachmentsDir string) *Store {
	return &Store{
		index:          index,
		attachmentsDir: attachmentsDir,
		readers:        make(map[string]*pack.Reader),
	}
}

// Open returns the blob with the given SHA-256 content hash and its size,
// preferring packed storage. Not-found satisfies errors.Is(err, fs.ErrNotExist).
//
// Two benign races with the (future) packer and repacker are absorbed by
// retrying the index lookup once: a loose file deleted just after an index
// miss, and a pack deleted just after a stale index hit.
func (s *Store) Open(hash string) (io.ReadSeekCloser, int64, error) {
	if err := export.ValidateContentHash(hash); err != nil {
		return nil, 0, err
	}
	entry, err := s.index.GetAttachmentPackEntry(hash)
	if err != nil {
		return nil, 0, err
	}
	if entry == nil {
		r, size, looseErr := s.openLoose(hash)
		if !errors.Is(looseErr, fs.ErrNotExist) {
			return r, size, looseErr
		}
		entry, err = s.index.GetAttachmentPackEntry(hash)
		if err != nil {
			return nil, 0, err
		}
		if entry == nil {
			return nil, 0, looseErr
		}
		return s.openPacked(hash, entry)
	}
	r, size, packErr := s.openPacked(hash, entry)
	if !errors.Is(packErr, fs.ErrNotExist) {
		return r, size, packErr
	}
	entry, err = s.index.GetAttachmentPackEntry(hash)
	if err != nil {
		return nil, 0, err
	}
	if entry == nil {
		return s.openLoose(hash)
	}
	return s.openPacked(hash, entry)
}

// Opener adapts Open to the export package's opener callback.
func (s *Store) Opener() export.AttachmentOpener {
	return func(contentHash string) (io.ReadCloser, error) {
		r, _, err := s.Open(contentHash)
		return r, err
	}
}

// Close releases all cached pack readers.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstErr error
	for id, r := range s.readers {
		if err := r.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("close pack reader %s: %w", id, err)
		}
	}
	s.readers = make(map[string]*pack.Reader)
	s.order = nil
	return firstErr
}

func (s *Store) openLoose(hash string) (io.ReadSeekCloser, int64, error) {
	p, err := export.StoragePath(s.attachmentsDir, hash)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("stat loose attachment %s: %w", hash, err)
	}
	return f, st.Size(), nil
}

func (s *Store) openPacked(hash string, e *store.PackIndexEntry) (io.ReadSeekCloser, int64, error) {
	if !pack.IsValidPackID(e.PackID) {
		return nil, 0, fmt.Errorf("invalid pack id %q in index for blob %s", e.PackID, hash)
	}
	blobID, err := pack.ParseBlobID(hash)
	if err != nil {
		return nil, 0, fmt.Errorf("parse blob id %s: %w", hash, err)
	}
	pe := pack.Entry{
		ID:        blobID,
		Offset:    uint64(e.Offset),    //nolint:gosec // column mirrors a uint64
		StoredLen: uint64(e.StoredLen), //nolint:gosec // column mirrors a uint64
		RawLen:    uint64(e.RawLen),    //nolint:gosec // column mirrors a uint64
		Flags:     pack.BlobFlags(e.Flags),
		CRC32C:    e.CRC32C,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.readerLocked(e.PackID)
	if err != nil {
		return nil, 0, err
	}
	data, err := r.ReadBlob(pe)
	if err != nil {
		return nil, 0, fmt.Errorf("read blob %s from pack %s: %w", hash, e.PackID, err)
	}
	return nopSeekCloser{bytes.NewReader(data)}, int64(len(data)), nil
}

// readerLocked returns a cached reader for the pack, opening and caching it
// (with FIFO eviction) on miss. Caller holds s.mu.
func (s *Store) readerLocked(packID string) (*pack.Reader, error) {
	if r, ok := s.readers[packID]; ok {
		return r, nil
	}
	p := filepath.Join(s.attachmentsDir, "packs", packID[:2], packID+PackExt)
	r, err := pack.OpenReader(p, nil)
	if err != nil {
		return nil, err // preserves fs.ErrNotExist for the retry rule
	}
	if len(s.order) >= maxOpenReaders {
		oldest := s.order[0]
		s.order = s.order[1:]
		if old, ok := s.readers[oldest]; ok {
			_ = old.Close()
			delete(s.readers, oldest)
		}
	}
	s.readers[packID] = r
	s.order = append(s.order, packID)
	return r, nil
}

type nopSeekCloser struct{ *bytes.Reader }

func (nopSeekCloser) Close() error { return nil }
```

Verify against the real kit API: `pack.OpenReader(path, crypter)` error wrapping must preserve the underlying `fs.ErrNotExist` (check `reader.go:24-40`; if it wraps with `%w` it does). If it does not, detect via `os.IsNotExist` on the unwrapped chain and normalize.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/blobstore/ -v`
Expected: PASS

- [ ] **Step 5: Format, vet, commit**

```bash
go fmt ./... && go vet ./...
git add internal/blobstore/
git commit -m "Add blobstore package for packed attachment reads"
```

---

### Task 7: Switch the daemon HTTP attachment endpoint to the blobstore

**Files:**
- Modify: `internal/api/server.go` (ServerOptions + Server field)
- Modify: `internal/api/cli_handlers.go:1982-2025` (`handleCLIAttachment`)
- Modify: `cmd/msgvault/cmd/serve.go` (construct and pass the blobstore where `apiOpts` is built, near line 286)
- Test: extend the existing `handleCLIAttachment` test file in `internal/api/` (find it: `rg -ln "handleCLIAttachment|cli/attachment" internal/api/*_test.go`)

**Interfaces:**
- Consumes: `blobstore.New(index, attachmentsDir) *blobstore.Store` and `(*blobstore.Store).Open` from Task 6; `store.Store` implements `blobstore.PackIndex` (Task 5).
- Produces: `api.ServerOptions.BlobStore AttachmentBlobStore` — the daemon serves packed and loose attachments over `/api/v1/cli/attachment`, which is the production read path for TUI, MCP, and `export attachment` (all daemon-routed).

- [ ] **Step 1: Write the failing test**

`internal/api/handlers_test.go:2583` already tests the attachment endpoint against a loose file: it writes `dataDir/attachments/<hash[:2]>/<hash>`, builds the server with the file's existing fixture, and issues `httptest.NewRequest(http.MethodGet, "/api/v1/cli/attachment?content_hash="+contentHash, nil)`. Clone that test into `TestCLIAttachmentServesPackedBlob` in the same file, with these differences:

1. Instead of writing a loose file, build a real pack under `dataDir/attachments/packs/<id[:2]>/<id>.mvpack` (same recipe as the `buildPack` helper in `internal/blobstore/blobstore_test.go`: `pack.NewWriter(stagingDir, pack.WriterOptions{})` → `Append(content)` → `MkdirAll` → `Seal(finalPath)`).
2. Back the index with a `store.Store`: `st := testutil.NewTestStore(t)`, then `st.RecordPackedBlobs(rec, entries)` built from the sealed entries (fields as in Task 5's test).
3. Set `BlobStore: blobstore.New(st, filepath.Join(dataDir, "attachments"))` in the `ServerOptions` the fixture builds (extend the fixture with an optional field if it doesn't take raw options).
4. Assert: 200 with body equal to the packed content; and a second sub-case with an unknown (valid-format) hash returns 404.
5. Add a third sub-case: a loose file (exactly like the existing test's fixture) served through the same `BlobStore`-configured server returns 200 — proving the fallback.

Imports: `go.kenn.io/kit/pack`, `go.kenn.io/msgvault/internal/blobstore`, `go.kenn.io/msgvault/internal/store`, `go.kenn.io/msgvault/internal/testutil`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/ -run TestCLIAttachmentServesPackedBlob -v`
Expected: FAIL — `unknown field BlobStore` (compile error)

- [ ] **Step 3: Implement**

3a. In `internal/api/server.go`, add to the imports `"io"`, then near the other small interfaces:

```go
// AttachmentBlobStore serves attachment bytes by content hash from packed or
// loose storage. Implemented by *blobstore.Store. Not-found errors satisfy
// errors.Is(err, fs.ErrNotExist).
type AttachmentBlobStore interface {
	Open(hash string) (io.ReadSeekCloser, int64, error)
}
```

Add `BlobStore AttachmentBlobStore` to `ServerOptions`, `blobStore AttachmentBlobStore` to `Server`, and `blobStore: opts.BlobStore,` to the `NewServerWithOptions` literal.

3b. In `handleCLIAttachment` (`internal/api/cli_handlers.go`), after the `ValidateContentHash` check, insert the blobstore path before the legacy loose-open code:

```go
	if s.blobStore != nil {
		rc, size, err := s.blobStore.Open(contentHash)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				writeError(w, http.StatusNotFound, "not_found", "Attachment not found")
				return
			}
			s.logger.Error("failed to open CLI attachment", "content_hash", contentHash, "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to retrieve attachment")
			return
		}
		defer func() { _ = rc.Close() }()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.Header().Set("X-Msgvault-Content-Hash", contentHash)
		w.WriteHeader(http.StatusOK)
		if _, err := io.Copy(w, rc); err != nil {
			s.logger.Error("failed to write CLI attachment", "content_hash", contentHash, "error", err)
		}
		return
	}
```

Keep the existing loose-file code below it as the nil-BlobStore fallback (tests and any embedded callers that construct a Server without options keep working). Add `"io/fs"` import.

3c. In `cmd/msgvault/cmd/serve.go`, where `apiOpts` is assembled (the `api.NewServerWithOptions(apiOpts)` call is at ~line 288; the store `s` from `store.Open(dbPath)` at line 144 and `cfg` are both in scope):

```go
	apiOpts.BlobStore = blobstore.New(s, cfg.AttachmentsDir())
```

Add import `"go.kenn.io/msgvault/internal/blobstore"`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/api/ -v && go build ./...`
Expected: PASS, clean build

- [ ] **Step 5: Format, vet, commit**

```bash
go fmt ./... && go vet ./...
git add internal/api/ cmd/msgvault/cmd/serve.go
git commit -m "Serve daemon attachment endpoint through blobstore"
```

---

### Task 8: Opener-based export helpers and dead-code sweep

**Files:**
- Modify: `internal/export/attachments.go` (`AttachmentsToDir` → opener-based core)
- Modify: `cmd/msgvault/cmd/export_attachment.go` (delete unreachable loose-file helpers)
- Test: `internal/export/attachments_test.go` (extend)

**Interfaces:**
- Consumes: `export.AttachmentOpener` (existing).
- Produces: `func AttachmentsToDirWithOpener(outputDir string, attachments []query.AttachmentInfo, open AttachmentOpener) DirExportResult`. Phase 2's packer work wires blobstore-backed openers into any remaining local callers; after this task every export helper accepts one.

- [ ] **Step 1: Write the failing test**

In `internal/export/attachments_test.go` add:

```go
func TestAttachmentsToDirWithOpener(t *testing.T) {
	content := []byte("opener-served content")
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])

	outDir := t.TempDir()
	atts := []query.AttachmentInfo{{Filename: "doc.txt", ContentHash: hash}}
	res := AttachmentsToDirWithOpener(outDir, atts, func(contentHash string) (io.ReadCloser, error) {
		require.Equal(t, hash, contentHash)
		return io.NopCloser(bytes.NewReader(content)), nil
	})

	require.Empty(t, res.Errors)
	require.Len(t, res.Files, 1)
	got, err := os.ReadFile(res.Files[0].Path)
	require.NoError(t, err)
	assert.Equal(t, content, got)
}
```

Match the file's existing imports/fixtures for `query.AttachmentInfo`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/export/ -run TestAttachmentsToDirWithOpener -v`
Expected: FAIL — `undefined: AttachmentsToDirWithOpener`

- [ ] **Step 3: Implement**

In `internal/export/attachments.go`:

3a. Add the opener-based entry point and make `AttachmentsToDir` delegate:

```go
// AttachmentsToDirWithOpener exports attachments as individual files into
// outputDir using the supplied opener for attachment bytes.
func AttachmentsToDirWithOpener(outputDir string, attachments []query.AttachmentInfo, open AttachmentOpener) DirExportResult {
	var result DirExportResult
	usedNames := make(map[string]int)

	for _, att := range attachments {
		if att.URL != "" {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: URL-backed attachment is available at %s", att.Filename, att.URL))
			continue
		}
		if err := ValidateContentHash(att.ContentHash); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", att.Filename, err))
			continue
		}

		filename := resolveUniqueFilename(att.Filename, att.ContentHash, usedNames)
		exported, err := exportAttachmentToFile(outputDir, open, att.ContentHash, filename)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", att.Filename, err))
			continue
		}
		result.Files = append(result.Files, exported)
	}
	return result
}

// AttachmentsToDir exports attachments as individual files into outputDir,
// reading content from attachmentsDir's loose content-addressed files.
func AttachmentsToDir(outputDir, attachmentsDir string, attachments []query.AttachmentInfo) DirExportResult {
	return AttachmentsToDirWithOpener(outputDir, attachments, looseOpener(attachmentsDir))
}

// looseOpener opens loose <hash[:2]>/<hash> files under attachmentsDir.
func looseOpener(attachmentsDir string) AttachmentOpener {
	return func(contentHash string) (io.ReadCloser, error) {
		p, err := StoragePath(attachmentsDir, contentHash)
		if err != nil {
			return nil, err
		}
		return os.Open(p)
	}
}
```

3b. Change `exportAttachmentToFile` to take the opener instead of a directory:

```go
func exportAttachmentToFile(outputDir string, open AttachmentOpener, contentHash, filename string) (ExportedFile, error) {
	src, err := open(contentHash)
	if err != nil {
		if os.IsNotExist(err) {
			return ExportedFile{}, fmt.Errorf("attachment file not found for hash %s", contentHash)
		}
		return ExportedFile{}, fmt.Errorf("open source: %w", err)
	}
	defer func() { _ = src.Close() }()
	// ... rest of the existing body unchanged ...
```

3c. Simplify `Attachments` to reuse `looseOpener`:

```go
func Attachments(zipFilename, attachmentsDir string, attachments []query.AttachmentInfo) ExportStats {
	return AttachmentsWithOpener(zipFilename, attachments, looseOpener(attachmentsDir))
}
```

3d. Dead-code sweep in `cmd/msgvault/cmd/export_attachment.go`: `runExportAttachment` unconditionally routes to `runExportAttachmentHTTP` (line 68), so verify with `rg -n "exportAttachmentAsJSON|exportAttachmentAsBase64|exportAttachmentBinary\(|openAttachmentFile|readAttachmentFile" cmd/ internal/` that `exportAttachmentAsJSON`, `exportAttachmentAsBase64`, `exportAttachmentBinary`, `openAttachmentFile`, and `readAttachmentFile` have no callers outside that file's dead chain — then delete them (and their tests if any assert on them directly). If a live caller turns up, leave that function and note it in the commit message.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/export/ ./cmd/... -count=1`
Expected: PASS

- [ ] **Step 5: Format, vet, commit**

```bash
go fmt ./... && go vet ./...
git add internal/export/ cmd/msgvault/cmd/export_attachment.go
git commit -m "Add opener-based directory export and remove dead attachment helpers"
```

---

### Task 9: Full verification sweep

**Files:**
- Modify: `docs/internal/packed-attachments-design.md` (status note)
- Modify: `CLAUDE.md` (add `internal/blobstore` to the Core key-files list, one line)

- [ ] **Step 1: Run the full suite**

```bash
go fmt ./... && go vet ./...
make lint-ci
make test
```

Expected: all green, zero warnings. Fix anything that fails before proceeding (never skip; never `--no-verify`).

- [ ] **Step 2: PostgreSQL pass if available**

If `MSGVAULT_TEST_DB` is configured locally: `make test-pg`. Expected: green, specifically `internal/store` (BIGINT round-trips). If no PG instance is available, note that in the final report — CI covers it.

- [ ] **Step 3: Update docs**

In `docs/internal/packed-attachments-design.md`, change the status line to:

```
implemented (this branch); phase 2 (kit content-reader hook, packer, GC)
not yet started.
```
under a note that delivery steps 1–2 are done. In `CLAUDE.md` under "Core (`internal/`)", add:

```
- `blobstore/blobstore.go` - Attachment blob reads: packed CAS with loose-file fallback
```

- [ ] **Step 4: Commit**

```bash
git add docs/internal/packed-attachments-design.md CLAUDE.md
git commit -m "Mark packed-attachments phase 1 delivery steps complete"
```

---

## Out of scope (phase 2 plan, after this lands)

- kit change: `backup.App` content-reader hook (release-blocking before any packer ships) — separate plan in `~/code/kit`.
- Packer (`pack-attachments`), canonicalization of legacy noncanonical rows, `unpack-attachments`, GC/repack, auto-run hooks.
- Windows note for phase 2: deleting a pack while a cached reader holds it open fails on Windows without `FILE_SHARE_DELETE`; the repack step must close cached readers first (the blobstore's `Close`/eviction already provides the hook).
