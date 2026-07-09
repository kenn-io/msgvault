# Packed Attachments — Phase 2a: kit ContentSource Hook

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an optional `ContentSource` hook to kit's backup capture so an application can supply attachment bytes (hash → reader) instead of the engine opening files under `CreateOptions.ContentDir`. This unblocks msgvault's phase-2 packer: once attachments live in packs, backup capture reads them through msgvault's blobstore.

**Architecture:** New `ContentSource` interface in `backup/attachments.go`; optional `Source` field on `CaptureOptions` and `ContentSource` field on `CreateOptions` (nil = today's directory behavior, byte-for-byte). The capture pipeline (dispatcher / workers / ordered collector) is untouched; only the per-ref "turn a ref into bytes" step routes through the source, and the engine's SHA-256 verification, max-size cap, and error-ordering semantics apply identically to both paths.

**Tech Stack:** Go 1.26 in the repo at `/Users/wesm/code/kit` (NOT the msgvault repo). Branch off `main` (clean at v0.3.0, HEAD == tag): `git checkout -b backup-content-source`.

## Global Constraints

- Repo conventions are `/Users/wesm/code/kit/AGENTS.md` (CLAUDE.md symlinks to it): packages stay small and **app-neutral** — no msgvault-specific naming or behavior; narrow public APIs; stdlib first; wrap errors with context; `context.Context` for blocking work.
- Tests: testify (`require`/`assert`), no new `t.Fatal`/`t.Error`, table tests where shapes repeat, `t.TempDir()`.
- Verification: `go build ./... && go test ./backup/ && go vet ./...`; full suite `go test ./...` before finishing. CI also enforces `go mod tidy` hygiene (no new deps here, so a no-op).
- Nil `ContentSource` must be bit-identical to current behavior — every existing test passes unmodified.
- Commit after every task; imperative subject ≤72 chars; never `--amend`.
- After merge, the release is a `v0.4.0` tag push (tag-driven, no automation); tagging is the maintainer's call, noted in Task 4 but not performed by an agent.

---

### Task 1: `ContentSource` type and source-backed capture read path

**Files:**
- Modify: `/Users/wesm/code/kit/backup/attachments.go`
- Test: `/Users/wesm/code/kit/backup/attachments_source_test.go` (create)

**Interfaces:**
- Consumes: existing `ContentRef{Hash, Size, StoragePath}`, `captureResult`, `maxCaptureRawLen`, `pack.EncodeFrame`, `CaptureOptions`.
- Produces (Tasks 2–3 rely on these exact names):
  - `type ContentSource interface { Open(ctx context.Context, ref ContentRef) (io.ReadCloser, error) }`
  - `CaptureOptions.Source ContentSource` (nil = read `attachmentsDir`)
  - unexported `captureRefFromSource(ctx context.Context, source ContentSource, ref ContentRef, index int, preKnown map[pack.BlobID]struct{}, level int) captureResult`

- [ ] **Step 1: Write the failing tests**

Create `/Users/wesm/code/kit/backup/attachments_source_test.go` (package `backup`, mirroring `attachments_test.go`'s in-package style and its helpers `newTestAppender`/`writeLooseAttachment` — read that file first and reuse its fixture helpers rather than inventing new ones):

```go
package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mapSource serves content from memory, keyed by ref hash. Missing keys
// return fs-agnostic errNotInSource.
type mapSource struct {
	blobs map[string][]byte
	opens int
}

var errNotInSource = errors.New("blob not in source")

func (s *mapSource) Open(_ context.Context, ref ContentRef) (io.ReadCloser, error) {
	s.opens++
	b, ok := s.blobs[ref.Hash]
	if !ok {
		return nil, fmt.Errorf("%w: %s", errNotInSource, ref.Hash)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func sourceRef(content []byte) (ContentRef, string) {
	sum := sha256.Sum256(content)
	h := hex.EncodeToString(sum[:])
	return ContentRef{Hash: h, Size: int64(len(content))}, h
}

func TestCaptureAttachmentsFromSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	a := []byte("alpha content")
	b := []byte("bravo content")
	refA, hashA := sourceRef(a)
	refB, hashB := sourceRef(b)
	src := &mapSource{blobs: map[string][]byte{hashA: a, hashB: b}}

	appender, repo := newTestAppenderForSource(t)
	out, err := CaptureAttachments(context.Background(), "", []ContentRef{refA, refB},
		map[string]bool{}, appender, CaptureOptions{Source: src})
	require.NoError(err)
	require.NoError(appender.Finish())

	assert.Equal(int64(2), out.Blobs)
	assert.Equal(int64(len(a)+len(b)), out.BlobBytes)
	assert.Len(out.NewList, 2)
	assert.Equal(hashA, out.NewList[0].Hash)
	assert.Equal(2, src.opens)
	assertRepoHoldsBlob(t, repo, appender, hashA, a)
	assertRepoHoldsBlob(t, repo, appender, hashB, b)
}

func TestCaptureFromSourceHashMismatch(t *testing.T) {
	require := require.New(t)

	refA, hashA := sourceRef([]byte("expected content"))
	src := &mapSource{blobs: map[string][]byte{hashA: []byte("tampered content!")}}

	appender, _ := newTestAppenderForSource(t)
	_, err := CaptureAttachments(context.Background(), "", []ContentRef{refA},
		map[string]bool{}, appender, CaptureOptions{Source: src})
	require.Error(err)
	require.Contains(err.Error(), "does not match its hash")
}

func TestCaptureFromSourceMissingBlob(t *testing.T) {
	require := require.New(t)

	refA, _ := sourceRef([]byte("never stored"))
	src := &mapSource{blobs: map[string][]byte{}}

	appender, _ := newTestAppenderForSource(t)
	_, err := CaptureAttachments(context.Background(), "", []ContentRef{refA},
		map[string]bool{}, appender, CaptureOptions{Source: src})
	require.ErrorIs(err, errNotInSource)
}

func TestCaptureFromSourceOversizedBlob(t *testing.T) {
	require := require.New(t)

	old := maxCaptureRawLen
	maxCaptureRawLen = 8
	defer func() { maxCaptureRawLen = old }()

	content := []byte("longer than eight bytes")
	refA, hashA := sourceRef(content)
	src := &mapSource{blobs: map[string][]byte{hashA: content}}

	appender, _ := newTestAppenderForSource(t)
	_, err := CaptureAttachments(context.Background(), "", []ContentRef{refA},
		map[string]bool{}, appender, CaptureOptions{Source: src})
	require.Error(err)
	require.Contains(err.Error(), "maximum blob size")
}

func TestCaptureFromSourceIgnoresStoragePath(t *testing.T) {
	// A source is keyed on the ref, not the filesystem: a noncanonical
	// StoragePath (importer namespace) must not affect source capture.
	require := require.New(t)

	content := []byte("namespaced blob")
	refA, hashA := sourceRef(content)
	refA.StoragePath = "synctech-sms/" + hashA[:2] + "/" + hashA
	src := &mapSource{blobs: map[string][]byte{hashA: content}}

	appender, _ := newTestAppenderForSource(t)
	out, err := CaptureAttachments(context.Background(), "", []ContentRef{refA},
		map[string]bool{}, appender, CaptureOptions{Source: src})
	require.NoError(err)
	require.Equal(int64(1), out.Blobs)
}

func TestCaptureFromSourceParallel(t *testing.T) {
	require := require.New(t)

	blobs := map[string][]byte{}
	var refs []ContentRef
	for i := range 40 {
		content := fmt.Appendf(nil, "blob %03d content", i)
		ref, h := sourceRef(content)
		blobs[h] = content
		refs = append(refs, ref)
	}
	src := &mapSource{blobs: blobs}

	appender, _ := newTestAppenderForSource(t)
	out, err := CaptureAttachments(context.Background(), "", refs,
		map[string]bool{}, appender, CaptureOptions{Source: src, Jobs: 8})
	require.NoError(err)
	require.Equal(int64(40), out.Blobs)
	// Ordered collector: list order matches ref order regardless of Jobs.
	for i, ref := range refs {
		require.Equal(ref.Hash, out.NewList[i].Hash)
	}
}
```

Helper notes for the implementer: `newTestAppenderForSource`/`assertRepoHoldsBlob` are placeholders for whatever `attachments_test.go` already provides to build a `*PackAppender` against a temp repo and read a blob back — reuse those exact helpers (or minimally extract them) instead of duplicating fixture code. If the existing tests construct the appender inline, extract a shared unexported helper in the test file that both call.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /Users/wesm/code/kit && go test ./backup/ -run TestCaptureAttachmentsFromSource -v`
Expected: FAIL — `unknown field Source in struct literal`

- [ ] **Step 3: Implement**

In `/Users/wesm/code/kit/backup/attachments.go`:

3a. The interface, above `CaptureOptions`:

```go
// ContentSource supplies attachment content bytes during capture, replacing
// the engine's own reads of the attachments directory. Implementations
// resolve a ref however the application stores content (loose files, pack
// files, object stores); the engine still verifies every blob's SHA-256
// against ref.Hash and enforces the per-blob size cap, so a source cannot
// weaken capture integrity. Open is called from concurrent capture workers
// and must be safe for concurrent use.
type ContentSource interface {
	Open(ctx context.Context, ref ContentRef) (io.ReadCloser, error)
}
```

3b. Extend `CaptureOptions`:

```go
	// Source, when non-nil, supplies attachment bytes instead of the engine
	// reading them from the attachments directory; the directory is then
	// ignored entirely. Reads are still hash-verified and size-capped.
	Source ContentSource
```

3c. The source-backed worker read, next to `captureRef`:

```go
// captureRefFromSource is captureRef for an application-supplied source:
// same hash verification, size cap, known-blob skip, and trial compression;
// only the byte acquisition differs.
func captureRefFromSource(
	ctx context.Context, source ContentSource, ref ContentRef, index int,
	preKnown map[pack.BlobID]struct{}, level int,
) captureResult {
	content, err := readSourceBlob(ctx, source, ref)
	if err != nil {
		return captureResult{index: index, err: fmt.Errorf("backup: reading attachment %s from content source: %w", ref.Hash, err)}
	}
	sum := sha256.Sum256(content)
	if hex.EncodeToString(sum[:]) != ref.Hash {
		return captureResult{
			index: index,
			err:   fmt.Errorf("backup: attachment %s content does not match its hash (live store corruption)", ref.Hash),
		}
	}
	res := captureResult{index: index, size: int64(len(content)), id: sum}
	if _, ok := preKnown[res.id]; ok {
		res.known = true
		return res
	}
	res.frame, res.compressed = pack.EncodeFrame(content, level)
	return res
}

// readSourceBlob reads one blob from source under the same cap
// readRegularFile enforces for directory reads.
func readSourceBlob(ctx context.Context, source ContentSource, ref ContentRef) ([]byte, error) {
	rc, err := source.Open(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, maxCaptureRawLen+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxCaptureRawLen {
		return nil, fmt.Errorf("%q is larger than the maximum blob size %d", ref.Hash, maxCaptureRawLen)
	}
	return data, nil
}
```

3d. Route the pipeline in `captureContents`. Two changes, minimal and localized:

Root open becomes conditional (the doc comment stays with the directory branch):

```go
	var root *os.Root
	if opts.Source == nil {
		// (existing os.OpenRoot comment block unchanged)
		root, err = os.OpenRoot(attachmentsDir)
		if err != nil {
			return fmt.Errorf("backup: opening attachments directory: %w", err)
		}
		defer func() { _ = root.Close() }()
	}
```

Dispatcher weighting: the pre-stat only applies to directory reads; a source ref is weighted by its declared size (`Size` is -1 when unknown → weight 0, same as today's stat-failure fallback):

```go
		for i := range refs {
			if opts.Source != nil {
				weights[i] = max(refs[i].Size, 0)
			} else if rel, err := captureRelPath(refs[i]); err == nil {
				if info, err := root.Stat(rel); err == nil {
					weights[i] = info.Size()
				}
				// A stat failure dispatches at weight zero; captureRef
				// reports the real error at the right position.
			}
			...
```

Worker dispatch selects the read path:

```go
	for range workers {
		wg.Go(func() {
			for i := range work {
				if opts.Source != nil {
					results <- captureRefFromSource(ctx, opts.Source, refs[i], i, preKnown, level)
				} else {
					results <- captureRef(root, refs[i], i, preKnown, level)
				}
			}
		})
	}
```

3e. Update `CaptureAttachments`'s doc comment: note that `attachmentsDir` is ignored when `opts.Source` is non-nil.

- [ ] **Step 4: Run tests**

Run: `cd /Users/wesm/code/kit && go test ./backup/ -count=1`
Expected: PASS — all new source tests AND every pre-existing capture test (nil-source path untouched).

- [ ] **Step 5: Vet and commit**

```bash
cd /Users/wesm/code/kit && gofmt -l backup/ && go vet ./backup/
git add backup/
git commit -m "backup: add ContentSource hook for capture reads"
```

---

### Task 2: `CreateOptions.ContentSource` plumbing

**Files:**
- Modify: `/Users/wesm/code/kit/backup/create.go`
- Test: `/Users/wesm/code/kit/backup/create_source_test.go` (create)

**Interfaces:**
- Consumes: Task 1's `ContentSource` and `CaptureOptions.Source`.
- Produces: `CreateOptions.ContentSource ContentSource` — when non-nil, `Create` captures content through it and `ContentDir` may be empty.

- [ ] **Step 1: Write the failing test**

Create `/Users/wesm/code/kit/backup/create_source_test.go` (package `backup`). Reuse `seedBackupFixture`/`createOpts` from `create_test.go`, but strip the content dir: seed the fixture as usual, read every seeded content file into a `mapSource` (helper from Task 1's test file), **delete the content directory**, then run `Create` with `ContentSource` set and `ContentDir` pointing at the now-deleted path. Assert: `Create` succeeds, the manifest's attachment counts match the fixture, and a follow-up `Verify` on the repo passes. Shape:

```go
func TestCreateWithContentSource(t *testing.T) {
	require := require.New(t)

	dbPath, contentDir, dataDir, db := seedBackupFixture(t)
	src := &mapSource{blobs: readAllContentFiles(t, contentDir)}
	require.NoError(os.RemoveAll(contentDir))

	repo := initTestRepo(t)   // reuse create_test.go's repo-init helper name
	opts := createOpts(dbPath, contentDir, dataDir)
	opts.ContentSource = src

	m, err := Create(context.Background(), repo, newTestApp(), opts)
	require.NoError(err)
	require.NotNil(m)
	// Verify proves the packs hold real, hash-valid content.
	requireVerifyClean(t, repo)   // reuse/extract the verify helper used by round-trip tests
	_ = db
}
```

`readAllContentFiles` walks the seeded content dir into `map[hash][]byte` keyed by each file's SHA-256 — put it in this test file. As in Task 1, the repo/verify helper names must be taken from the actual `create_test.go`/`apptest_test.go` fixtures — read them first; do not invent parallel fixtures.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/wesm/code/kit && go test ./backup/ -run TestCreateWithContentSource -v`
Expected: FAIL — `unknown field ContentSource`

- [ ] **Step 3: Implement**

In `/Users/wesm/code/kit/backup/create.go`:

Add to `CreateOptions` (after `ContentDir`):

```go
	// ContentSource, when non-nil, supplies attachment content bytes during
	// capture instead of the engine reading ContentDir; ContentDir is then
	// ignored for content reads (it may be empty). Extras and the page scan
	// are unaffected. See ContentSource.
	ContentSource ContentSource
```

Thread it into the capture call (`create.go:220`):

```go
	capture, err := CaptureAttachments(ctx, opts.ContentDir, info.Refs, captureSeen, appender, CaptureOptions{
		Jobs:   opts.Jobs,
		Source: opts.ContentSource,
		Progress: func(done, total int, bytesRead int64) {
			...unchanged...
		},
	})
```

No other `Create` change: `opts.ContentDir` has no other content-read consumer (verified during scoping — extras use `DataDir`; `app.ContentDirName()` is name validation only).

- [ ] **Step 4: Run tests**

Run: `cd /Users/wesm/code/kit && go test ./backup/ -count=1`
Expected: PASS

- [ ] **Step 5: Vet and commit**

```bash
cd /Users/wesm/code/kit && gofmt -l backup/ && go vet ./backup/
git add backup/
git commit -m "backup: thread ContentSource through CreateOptions"
```

---

### Task 3: end-to-end round trip with no content directory

**Files:**
- Modify: `/Users/wesm/code/kit/backup/genericapp_test.go` (or a sibling `genericapp_source_test.go` in package `backup_test` if the file's fixtures don't extend cleanly)

**Interfaces:**
- Consumes: the public API only — `backup.Create` with `ContentSource`, `backup.Verify`, `backup.Restore` (this is the external-consumer proof, package `backup_test`).

- [ ] **Step 1: Write the failing-to-compile-or-pass test**

Model on `TestGenericAppRoundTrip` (`genericapp_test.go:54`). Extend `fakeApp` (or add `fakeContentApp`) so its `ContentInfo` reports two content refs, its `RestoredContentPaths` maps them to canonical `<aa>/<hash>` paths, and the test supplies the bytes via an in-memory `ContentSource` (a `backup_test`-local copy of the map-source, since Task 1's is in-package). Drive: `Create` (no content dir on disk at all) → `Verify` (clean) → `Restore` into a temp target → assert both content files exist at their canonical paths with the exact original bytes.

This is the test that proves the full promise: an application whose content never touches a loose directory can back up and restore losslessly.

- [ ] **Step 2: Run, implement any fixture gaps, re-run**

Run: `cd /Users/wesm/code/kit && go test ./backup/ -run GenericApp -v`
Expected: PASS with no production-code changes (Tasks 1–2 built everything; this task is test-only — if a production gap surfaces, fix it here and note it in the commit).

- [ ] **Step 3: Full suite, vet, commit**

```bash
cd /Users/wesm/code/kit && go build ./... && go test ./... && go vet ./...
git add backup/
git commit -m "backup: prove content-source round trip without a content dir"
```

---

### Task 4: docs and handoff

**Files:**
- Modify: `/Users/wesm/code/kit/backup/FORMAT.md` — only if it describes capture as reading the content directory; the wire format itself is unchanged (attachment lists still carry hash+size), so this is at most a one-line capture-semantics note.
- Modify: `/Users/wesm/worktrees/github.com/kenn-io/msgvault/packed-attachments/docs/internal/packed-attachments-design.md` — in "Backup coordination", note the kit hook has landed (`ContentSource` on `CreateOptions`, kit branch `backup-content-source`) and record the follow-up: msgvault's implementation must route hash → blobstore with a DB-recorded-path fallback for unindexed noncanonical blobs.

- [ ] **Step 1: Make the doc edits**

- [ ] **Step 2: Commit each repo separately**

```bash
cd /Users/wesm/code/kit && git add backup/FORMAT.md && git commit -m "backup: document ContentSource capture semantics"   # only if FORMAT.md changed
cd /Users/wesm/worktrees/github.com/kenn-io/msgvault/packed-attachments && git add docs/internal/packed-attachments-design.md && git commit -m "Record kit ContentSource hook status in design doc"
```

- [ ] **Step 3: Handoff checklist (report, do not perform)**

Report to the maintainer: kit branch `backup-content-source` ready for PR to kit `main`; after merge, tag `v0.4.0`; msgvault phase 2b then bumps `go.mod` (`go get go.kenn.io/kit@v0.4.0`) — until the tag exists, msgvault development can use `go mod edit -replace go.kenn.io/kit=/Users/wesm/code/kit` locally (never committed).

---

## Out of scope (phase 2b plan, in msgvault)

- msgvault's `ContentSource` implementation (blobstore + DB-path fallback), the go.mod bump, packer, canonicalization, `pack-attachments`/`unpack-attachments`, GC/repack, and switching the MCP-local/TUI-local loose readers to the blobstore.
