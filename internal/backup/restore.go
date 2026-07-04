package backup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/pack"
)

// RestoreOptions parameterizes one restore run (docs/architecture/backup-format.md, Restore).
type RestoreOptions struct {
	SnapshotID string // empty: latest
	// TargetDir receives the restored archive: msgvault.db, attachments/,
	// and any captured extras. It must not exist, or be an empty directory,
	// unless Overwrite is set. Overwrite merges into the existing tree: the
	// database and its SQLite sidecars are removed first (a stale -wal or
	// -shm would otherwise be replayed over the restored file on its first
	// normal open), restored files replace same-named ones, and files the
	// snapshot does not carry are left in place.
	TargetDir string
	Overwrite bool
	// Jobs is the number of concurrent pack-read workers. Zero or negative
	// selects one per CPU. Use 1 to read packs strictly one at a time when
	// the repository lives on a spinning disk or NAS share.
	Jobs        int
	ForceUnlock bool
	// Progress, if non-nil, receives structured progress events as Restore
	// runs. nil means fully silent.
	Progress func(ProgressEvent)
}

// RestoreResult reports what Restore materialized and proved.
type RestoreResult struct {
	SnapshotID      string
	DBPath          string
	DBBytes         int64
	AttachmentBlobs int64
	AttachmentBytes int64
	ExtrasFiles     int
	Duration        time.Duration
}

// restoredDBFileName is the database filename inside the restore target,
// matching the live archive layout so the target is usable as a data dir.
const restoredDBFileName = "msgvault.db"

// Restore materializes one snapshot into TargetDir and then proves the
// result (docs/architecture/backup-format.md, Restore): every database page
// is hash-verified against the snapshot's page-hash map as it is written,
// every blob read re-derives its SHA-256 identity, and the restored database
// must pass PRAGMA integrity_check and reproduce the manifest's recorded
// stats exactly before Restore reports success.
//
// It takes a SHARED repository lock: concurrent restores and verifies are
// safe, a running create is not.
func Restore(ctx context.Context, r *Repo, opts RestoreOptions) (*RestoreResult, error) {
	start := time.Now()
	lock, err := r.AcquireSharedLock("restore", opts.ForceUnlock)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Release() }()

	var m *Manifest
	if opts.SnapshotID != "" {
		if m, err = r.LoadManifest(opts.SnapshotID); err != nil {
			return nil, err
		}
	} else {
		if m, err = r.LatestSnapshot(); err != nil {
			return nil, err
		}
		if m == nil {
			return nil, errors.New("backup: repository has no snapshots to restore")
		}
	}
	if err := prepareRestoreTarget(opts.TargetDir, opts.Overwrite); err != nil {
		return nil, err
	}
	known, err := r.LoadBlobIndex()
	if err != nil {
		return nil, err
	}
	jobs := opts.Jobs
	if jobs <= 0 {
		jobs = runtime.GOMAXPROCS(0)
	}
	st := &restoreState{
		repo:     r,
		known:    known,
		jobs:     jobs,
		progress: newProgressEmitter(opts.Progress),
	}

	hm, pm, err := st.materializeMaps(m)
	if err != nil {
		return nil, err
	}
	res := &RestoreResult{
		SnapshotID: m.SnapshotID,
		DBPath:     filepath.Join(opts.TargetDir, restoredDBFileName),
		DBBytes:    int64(pm.PageCount * uint64(pm.PageSize)), //nolint:gosec // geometry checked against the manifest
	}
	if err := st.restoreDB(ctx, res.DBPath, pm, hm); err != nil {
		return nil, err
	}
	res.AttachmentBlobs, res.AttachmentBytes, err = st.restoreAttachments(
		ctx, m, res.DBPath, filepath.Join(opts.TargetDir, "attachments"))
	if err != nil {
		return nil, err
	}
	if res.ExtrasFiles, err = st.restoreExtras(m, opts.TargetDir); err != nil {
		return nil, err
	}

	// The proof has two visible steps: integrity_check (which reads the
	// whole restored database inside SQLite and dominates on large
	// archives) and the manifest stats comparison.
	st.progress.emit(ProgressEvent{Stage: ProgressStageProof, Total: 2})
	if err := proveRestoredDB(ctx, res.DBPath, m, func() {
		st.progress.emit(ProgressEvent{Stage: ProgressStageProof, Done: 1, Total: 2})
	}); err != nil {
		return nil, err
	}
	st.progress.emit(ProgressEvent{Stage: ProgressStageProof, Done: 2, Total: 2, Final: true})

	if err := pack.SyncDir(opts.TargetDir); err != nil {
		return nil, fmt.Errorf("backup: syncing restore target: %w", err)
	}
	res.Duration = time.Since(start)
	return res, nil
}

// prepareRestoreTarget creates TargetDir, refusing a non-empty existing
// directory unless overwrite is set (docs/architecture/backup-format.md, Restore).
func prepareRestoreTarget(target string, overwrite bool) error {
	if target == "" {
		return errors.New("backup: restore target directory is required")
	}
	entries, err := os.ReadDir(target)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(target, 0o700); err != nil {
			return fmt.Errorf("backup: creating restore target: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("backup: reading restore target: %w", err)
	case len(entries) > 0 && !overwrite:
		return fmt.Errorf("backup: restore target %s is not empty (use --overwrite to restore into it anyway)", target)
	}
	// Overwrite merges rather than clearing the tree, but the database and
	// its SQLite sidecars must not survive: restoreDB rewrites msgvault.db,
	// and a stale -wal/-shm pair next to it would be replayed over the
	// proven bytes on the file's first normal (non-immutable) open.
	for _, name := range []string{restoredDBFileName, restoredDBFileName + "-wal", restoredDBFileName + "-shm"} {
		if err := os.Remove(filepath.Join(target, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("backup: removing stale %s from restore target: %w", name, err)
		}
	}
	return nil
}

// restoreState carries the shared read machinery for one Restore run. mu
// guards progress counters and the first-error slot while pack workers run.
type restoreState struct {
	repo     *Repo
	known    map[pack.BlobID]IndexEntry
	jobs     int
	progress *progressEmitter

	mu       sync.Mutex
	firstErr error
	done     int64 // stage-local progress counter (pages, then files)
	doneByte int64
}

// fail records the run's first error; workers check failed() to stop early.
func (s *restoreState) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.firstErr == nil {
		s.firstErr = err
	}
}

func (s *restoreState) failed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.firstErr != nil
}

// fetch reads one metadata blob through the repository index.
func (s *restoreState) fetch(id pack.BlobID) ([]byte, error) {
	return s.repo.ReadBlob(s.known, id, nil)
}

// materializeMaps walks and materializes the snapshot's hash-map and
// page-map chains, cross-checking both against the manifest's recorded
// geometry and the page map's coverage before any byte is written.
func (s *restoreState) materializeMaps(m *Manifest) (*PageHashMap, *PageMap, error) {
	hashChain, err := s.repo.HashMapChain(m)
	if err != nil {
		return nil, nil, err
	}
	hm, err := MaterializeHashMap(s.fetch, hashChain)
	if err != nil {
		return nil, nil, err
	}
	pageChain, err := s.repo.PageMapChain(m)
	if err != nil {
		return nil, nil, err
	}
	pm, err := MaterializePageMap(s.fetch, pageChain)
	if err != nil {
		return nil, nil, err
	}
	if err := pm.CheckCoverage(); err != nil {
		return nil, nil, err
	}
	if pm.PageCount != m.DB.PageCount || pm.PageSize != m.DB.PageSize {
		return nil, nil, fmt.Errorf(
			"backup: page map geometry (%d pages of %d bytes) disagrees with manifest (%d pages of %d bytes)",
			pm.PageCount, pm.PageSize, m.DB.PageCount, m.DB.PageSize)
	}
	if hm.PageCount != m.DB.PageCount || hm.PageSize != m.DB.PageSize {
		return nil, nil, fmt.Errorf(
			"backup: page hash map geometry (%d pages of %d bytes) disagrees with manifest (%d pages of %d bytes)",
			hm.PageCount, hm.PageSize, m.DB.PageCount, m.DB.PageSize)
	}
	return hm, pm, nil
}

// blobRuns is one page blob and every page-map run it backs.
type blobRuns struct {
	id   pack.BlobID
	runs []PageRun
}

// restoreDB materializes the database file: every page-map run is written at
// page*page_size, and every page is hash-verified against the page-hash map
// while its blob is still in memory. Work is grouped by pack, s.jobs packs
// in flight; the file writes are disjoint pwrite calls, safe concurrently.
func (s *restoreState) restoreDB(ctx context.Context, dbPath string, pm *PageMap, hm *PageHashMap) error {
	f, err := os.OpenFile(dbPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("backup: creating restored database: %w", err)
	}
	defer func() { _ = f.Close() }()
	size := int64(pm.PageCount * uint64(pm.PageSize)) //nolint:gosec // geometry checked against the manifest
	if err := f.Truncate(size); err != nil {
		return fmt.Errorf("backup: sizing restored database: %w", err)
	}

	runsByBlob := make(map[uint32][]PageRun)
	for _, r := range pm.Runs {
		runsByBlob[r.BlobIndex] = append(runsByBlob[r.BlobIndex], r)
	}
	groups := map[string][]blobRuns{}
	var order []string
	for i, id := range pm.Blobs {
		ie, ok := s.known[id]
		if !ok {
			return fmt.Errorf("backup: page blob %s not present in any index", id)
		}
		if _, seen := groups[ie.PackID]; !seen {
			order = append(order, ie.PackID)
		}
		groups[ie.PackID] = append(groups[ie.PackID], blobRuns{id: id, runs: runsByBlob[uint32(i)]})
	}

	s.done, s.doneByte = 0, 0
	s.progress.emit(ProgressEvent{
		Stage: ProgressStageRestoreDB, Total: int64(pm.PageCount), BytesTotal: size, //nolint:gosec // page counts fit int64
	})
	err = s.runPackGroups(ctx, order, func(packID string) {
		s.restorePackPages(f, packID, groups[packID], pm.PageSize, hm)
	})
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("backup: syncing restored database: %w", err)
	}
	s.progress.emit(ProgressEvent{
		Stage: ProgressStageRestoreDB, Done: int64(pm.PageCount), Total: int64(pm.PageCount), //nolint:gosec // page counts fit int64
		BytesDone: size, BytesTotal: size, Final: true,
	})
	return nil
}

// runPackGroups fans packIDs out to s.jobs workers, stopping dispatch on
// context cancellation or the first recorded failure, and returns the run's
// first error.
func (s *restoreState) runPackGroups(ctx context.Context, packIDs []string, work func(packID string)) error {
	packs := make(chan string)
	var wg sync.WaitGroup
	for range max(min(s.jobs, len(packIDs)), 1) {
		wg.Go(func() {
			for packID := range packs {
				if s.failed() {
					continue
				}
				work(packID)
			}
		})
	}
	for _, packID := range packIDs {
		if err := ctx.Err(); err != nil {
			s.fail(err)
			break
		}
		if s.failed() {
			break
		}
		packs <- packID
	}
	close(packs)
	wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.firstErr
}

// restorePackPages writes one pack's page blobs into the database file.
func (s *restoreState) restorePackPages(f *os.File, packID string, blobs []blobRuns, pageSize uint32, hm *PageHashMap) {
	pr, err := pack.OpenReader(s.repo.Path(packsDirName, packID[:2], packID+".mvpack"), nil)
	if err != nil {
		s.fail(fmt.Errorf("backup: opening pack %s: %w", packID, err))
		return
	}
	defer func() { _ = pr.Close() }()
	entries := pr.Entries()
	entryByID := make(map[pack.BlobID]*pack.Entry, len(entries))
	for i := range entries {
		entryByID[entries[i].ID] = &entries[i]
	}
	for _, br := range blobs {
		if s.failed() {
			return
		}
		entry, ok := entryByID[br.id]
		if !ok {
			s.fail(fmt.Errorf("backup: page blob %s missing from pack %s footer", br.id, packID))
			return
		}
		raw, err := pr.ReadBlob(*entry)
		if err != nil {
			s.fail(fmt.Errorf("backup: reading page blob %s from pack %s: %w", br.id, packID, err))
			return
		}
		for _, run := range br.runs {
			if err := s.writeRun(f, raw, br.id, run, pageSize, hm); err != nil {
				s.fail(err)
				return
			}
		}
	}
}

// writeRun hash-verifies and writes one page-map run from its blob's bytes.
func (s *restoreState) writeRun(f *os.File, raw []byte, id pack.BlobID, run PageRun, pageSize uint32, hm *PageHashMap) error {
	length := uint64(run.PageCount) * uint64(pageSize)
	// Subtraction-based bounds check: BlobOffset is untrusted (decoded from
	// a page-map object), and BlobOffset+length can wrap uint64 for a huge
	// offset, slipping past an addition-based comparison into a slice panic.
	if run.BlobOffset > uint64(len(raw)) || length > uint64(len(raw))-run.BlobOffset {
		return fmt.Errorf(
			"backup: page map run (pages %d..%d) overruns blob %s (%d bytes at offset %d)",
			run.StartPage, run.StartPage+uint64(run.PageCount)-1, id, len(raw), run.BlobOffset)
	}
	segment := raw[run.BlobOffset : run.BlobOffset+length]
	for i := range uint64(run.PageCount) {
		p := run.StartPage + i
		h := PageHash(segment[i*uint64(pageSize) : (i+1)*uint64(pageSize)])
		if !bytes.Equal(h[:], hm.Hashes[p*pageHashSize:(p+1)*pageHashSize]) {
			return fmt.Errorf("backup: restored page %d does not match the snapshot's page hash map", p)
		}
	}
	if _, err := f.WriteAt(segment, int64(run.StartPage)*int64(pageSize)); err != nil { //nolint:gosec // page*pageSize fits int64
		return fmt.Errorf("backup: writing pages %d..%d: %w", run.StartPage, run.StartPage+uint64(run.PageCount)-1, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done += int64(run.PageCount)
	s.doneByte += int64(length) //nolint:gosec // run lengths fit int64
	s.progress.emit(ProgressEvent{
		Stage: ProgressStageRestoreDB, Done: s.done, Total: int64(hm.PageCount), //nolint:gosec // page counts fit int64
		BytesDone: s.doneByte, BytesTotal: int64(hm.PageCount) * int64(pageSize), //nolint:gosec // page counts fit int64
	})
	return nil
}

// restoreAttachments lays the snapshot's attachment population out at the
// storage paths the restored database records for each hash (importers may
// namespace paths beyond the plain <hash[:2]>/<hash> layout), reading blobs
// grouped by pack. Every blob read re-derives its SHA-256 identity before
// any file is written.
func (s *restoreState) restoreAttachments(ctx context.Context, m *Manifest, dbPath, dir string) (int64, int64, error) {
	refs, _, err := LoadListRefs(s.repo, s.known, m.Attachments.Lists, nil)
	if err != nil {
		return 0, 0, err
	}
	if int64(len(refs)) != m.Attachments.Blobs {
		return 0, 0, fmt.Errorf(
			"backup: attachment lists name %d blobs but manifest reports %d", len(refs), m.Attachments.Blobs)
	}
	paths, err := loadRestoredAttachmentPaths(ctx, dbPath)
	if err != nil {
		return 0, 0, err
	}
	groups := map[string][]ContentRef{}
	var order []string
	for _, ref := range refs {
		id, err := pack.ParseBlobID(ref.Hash)
		if err != nil {
			return 0, 0, fmt.Errorf("backup: attachment content hash %q: %w", ref.Hash, err)
		}
		ie, ok := s.known[id]
		if !ok {
			return 0, 0, fmt.Errorf("backup: attachment blob %s not present in any index", ref.Hash)
		}
		if _, seen := groups[ie.PackID]; !seen {
			order = append(order, ie.PackID)
		}
		groups[ie.PackID] = append(groups[ie.PackID], ref)
	}

	s.done, s.doneByte = 0, 0
	s.progress.emit(ProgressEvent{
		Stage: ProgressStageAttachments, Total: int64(len(refs)), BytesTotal: m.Attachments.BlobBytes,
	})
	err = s.runPackGroups(ctx, order, func(packID string) {
		s.restorePackAttachments(dir, packID, groups[packID], paths, int64(len(refs)), m.Attachments.BlobBytes)
	})
	if err != nil {
		return 0, 0, err
	}
	var totalBytes int64
	for _, ref := range refs {
		totalBytes += ref.Size
	}
	s.progress.emit(ProgressEvent{
		Stage: ProgressStageAttachments, Done: int64(len(refs)), Total: int64(len(refs)),
		BytesDone: totalBytes, BytesTotal: totalBytes, Final: true,
	})
	return int64(len(refs)), totalBytes, nil
}

// loadRestoredAttachmentPaths maps each content and thumbnail hash in the
// restored database to every relative storage path it is recorded at. Paths
// come from DB rows, so each is validated as local before restore writes it.
// restoredDBDSN builds the immutable read-only SQLite URI for the restored
// database. immutable=1 matters: the file has no writers and no WAL sidecar,
// and an immutable open never creates -wal/-shm next to it. Built via
// url.URL so a target path containing '?' or '#' cannot be misparsed as URI
// syntax. The path is made absolute first — a relative restore target must
// not become slash-rooted — then converted to the slash-separated,
// slash-rooted form SQLite's URI parser requires
// ("file:///C:/dir/msgvault.db"): a raw drive-letter path would otherwise
// be read as a URI authority.
func restoredDBDSN(dbPath string) string {
	if abs, err := filepath.Abs(dbPath); err == nil {
		dbPath = abs
	}
	p := filepath.ToSlash(dbPath)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p, RawQuery: "immutable=1&mode=ro"}).String()
}

func loadRestoredAttachmentPaths(ctx context.Context, dbPath string) (map[string][]string, error) {
	db, err := sql.Open("sqlite3", restoredDBDSN(dbPath))
	if err != nil {
		return nil, fmt.Errorf("backup: opening restored database for attachment paths: %w", err)
	}
	defer func() { _ = db.Close() }()
	// UNION deduplicates repeated (hash, path) rows across attachments.
	rows, err := db.QueryContext(ctx,
		"SELECT content_hash, storage_path FROM attachments WHERE "+contentBearing+
			" UNION SELECT thumbnail_hash, thumbnail_path FROM attachments WHERE "+thumbBearing)
	if err != nil {
		return nil, fmt.Errorf("backup: attachment path query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	paths := map[string][]string{}
	for rows.Next() {
		var hash, p string
		if err := rows.Scan(&hash, &p); err != nil {
			return nil, fmt.Errorf("backup: scanning attachment path: %w", err)
		}
		rel := filepath.FromSlash(p)
		if !filepath.IsLocal(rel) {
			return nil, fmt.Errorf(
				"backup: attachment %s storage path %q escapes the attachments directory", hash, p)
		}
		paths[hash] = append(paths[hash], rel)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("backup: attachment path rows: %w", err)
	}
	return paths, nil
}

// restorePackAttachments writes one pack's attachment blobs to their
// recorded storage paths under dir.
func (s *restoreState) restorePackAttachments(
	dir, packID string, refs []ContentRef, paths map[string][]string, total, totalBytes int64,
) {
	pr, err := pack.OpenReader(s.repo.Path(packsDirName, packID[:2], packID+".mvpack"), nil)
	if err != nil {
		s.fail(fmt.Errorf("backup: opening pack %s: %w", packID, err))
		return
	}
	defer func() { _ = pr.Close() }()
	entries := pr.Entries()
	entryByID := make(map[pack.BlobID]*pack.Entry, len(entries))
	for i := range entries {
		entryByID[entries[i].ID] = &entries[i]
	}
	for _, ref := range refs {
		if s.failed() {
			return
		}
		id, _ := pack.ParseBlobID(ref.Hash) // validated during grouping
		entry, ok := entryByID[id]
		if !ok {
			s.fail(fmt.Errorf("backup: attachment blob %s missing from pack %s footer", ref.Hash, packID))
			return
		}
		content, err := pr.ReadBlob(*entry)
		if err != nil {
			s.fail(fmt.Errorf("backup: reading attachment %s from pack %s: %w", ref.Hash, packID, err))
			return
		}
		if int64(len(content)) != ref.Size {
			s.fail(fmt.Errorf(
				"backup: attachment %s is %d bytes but its list records %d", ref.Hash, len(content), ref.Size))
			return
		}
		rels := paths[ref.Hash]
		if len(rels) == 0 {
			// The manifest's stats proof pins list count == DB blob count, so
			// a listed hash with no DB path means the two sets diverge.
			s.fail(fmt.Errorf(
				"backup: attachment blob %s is in the snapshot's lists but the restored database records no path for it",
				ref.Hash))
			return
		}
		for _, rel := range rels {
			if err := writeRestoredFile(filepath.Join(dir, rel), content, 0o600); err != nil {
				s.fail(err)
				return
			}
		}
		s.mu.Lock()
		s.done++
		s.doneByte += ref.Size
		s.progress.emit(ProgressEvent{
			Stage: ProgressStageAttachments, Done: s.done, Total: total,
			BytesDone: s.doneByte, BytesTotal: totalBytes,
		})
		s.mu.Unlock()
	}
}

// restoreExtras lays out the snapshot's captured extras files (deletions,
// config, tokens) under the target, preserving their recorded modes.
func (s *restoreState) restoreExtras(m *Manifest, target string) (int, error) {
	if m.Extras.Tree == "" {
		return 0, nil
	}
	id, err := pack.ParseBlobID(m.Extras.Tree)
	if err != nil {
		return 0, fmt.Errorf("backup: extras tree blob id %q: %w", m.Extras.Tree, err)
	}
	raw, err := s.fetch(id)
	if err != nil {
		return 0, err
	}
	var tree ExtrasTree
	if err := json.Unmarshal(raw, &tree); err != nil {
		return 0, fmt.Errorf("backup: extras tree %s: %w", id, err)
	}
	s.progress.emit(ProgressEvent{Stage: ProgressStageExtras, Total: int64(len(tree.Entries))})
	for i, entry := range tree.Entries {
		if err := s.restoreExtrasEntry(entry, target); err != nil {
			return 0, err
		}
		s.progress.emit(ProgressEvent{
			Stage: ProgressStageExtras, Done: int64(i + 1), Total: int64(len(tree.Entries)),
			Final: i == len(tree.Entries)-1,
		})
	}
	return len(tree.Entries), nil
}

// restoreExtrasEntry validates and writes one extras file. Entry paths come
// from a decoded tree blob, so they are re-validated here: only local,
// relative, traversal-free paths may be written under the target, and never
// paths that overlap the database or attachments restore already produced.
func (s *restoreState) restoreExtrasEntry(entry ExtrasEntry, target string) error {
	// Clean before validating: the final filepath.Join cleans the path
	// anyway, so validating the raw form would let "safe/../msgvault.db"
	// pass the reserved-name check below yet still land on a reserved path.
	rel := filepath.Clean(filepath.FromSlash(entry.Path))
	if entry.Path == "" || rel == "." || filepath.IsAbs(rel) || !filepath.IsLocal(rel) {
		return fmt.Errorf("backup: extras entry path %q escapes the restore target", entry.Path)
	}
	// Capture never records archive content as an extra, so an entry naming
	// the restored database, its SQLite sidecars, or the attachments tree
	// can only come from a tampered tree blob trying to overwrite already-
	// proven outputs. Folded comparison: the default macOS filesystem is
	// case-insensitive.
	first, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
	for _, reserved := range []string{
		"attachments", restoredDBFileName, restoredDBFileName + "-wal", restoredDBFileName + "-shm",
	} {
		if strings.EqualFold(first, reserved) {
			return fmt.Errorf("backup: extras entry path %q overlaps restored archive content", entry.Path)
		}
	}
	id, err := pack.ParseBlobID(entry.Blob)
	if err != nil {
		return fmt.Errorf("backup: extras entry %s blob id %q: %w", entry.Path, entry.Blob, err)
	}
	content, err := s.fetch(id)
	if err != nil {
		return err
	}
	if int64(len(content)) != entry.Size {
		return fmt.Errorf("backup: extras entry %s is %d bytes but its tree records %d",
			entry.Path, len(content), entry.Size)
	}
	mode := os.FileMode(entry.Mode).Perm()
	if mode == 0 {
		mode = 0o600
	}
	return writeRestoredFile(filepath.Join(target, rel), content, mode)
}

// writeRestoredFile writes one restored file durably: parents created, the
// file written and fsynced before close.
func writeRestoredFile(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("backup: creating restore directory for %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("backup: creating restored file %s: %w", path, err)
	}
	// O_CREATE's mode is filtered through the umask; restore must reproduce
	// the captured mode exactly.
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return fmt.Errorf("backup: setting mode on restored file %s: %w", path, err)
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return fmt.Errorf("backup: writing restored file %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("backup: syncing restored file %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("backup: closing restored file %s: %w", path, err)
	}
	return nil
}

// proveRestoredDB is the restore proof (docs/architecture/backup-format.md,
// Restore): the restored database must pass PRAGMA integrity_check and
// reproduce the manifest's recorded stats through exactly the queries
// capture ran inside the freeze. Page-level identity was already proven
// against the page-hash map during materialization.
// checked is called after integrity_check passes, before the stats
// comparison, so callers can report sub-step progress.
func proveRestoredDB(ctx context.Context, dbPath string, m *Manifest, checked func()) error {
	db, err := sql.Open("sqlite3", restoredDBDSN(dbPath))
	if err != nil {
		return fmt.Errorf("backup: opening restored database for proof: %w", err)
	}
	defer func() { _ = db.Close() }()

	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return fmt.Errorf("backup: restored database integrity_check: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var findings []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return fmt.Errorf("backup: reading integrity_check result: %w", err)
		}
		findings = append(findings, line)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("backup: integrity_check rows: %w", err)
	}
	if len(findings) != 1 || findings[0] != "ok" {
		return fmt.Errorf("backup: restored database failed integrity_check: %v", findings)
	}
	if checked != nil {
		checked()
	}

	stats, err := computeManifestStats(ctx, db)
	if err != nil {
		return err
	}
	if stats != m.Stats {
		return fmt.Errorf(
			"backup: restored database stats %+v do not match the manifest's recorded stats %+v",
			stats, m.Stats)
	}
	return nil
}
