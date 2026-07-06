package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync"

	"go.kenn.io/kit/pack"
)

// VerifyOptions parameterizes one integrity check run (docs/architecture/backup-format.md, Verification Model).
type VerifyOptions struct {
	SnapshotID  string // empty: latest
	All         bool
	Quick       bool
	ForceUnlock bool
	// Jobs is the number of concurrent content-blob read workers used in
	// full mode. Zero or negative selects one worker per CPU. Use 1 to read
	// packs strictly one at a time — the right choice when the repository
	// lives on a spinning disk or NAS share that degrades under concurrent
	// reads. Quick mode reads no content and ignores Jobs.
	Jobs int
	// Progress, if non-nil, receives structured progress events as Verify
	// runs. nil means fully silent. Verify emits events freely and cheaply;
	// throttling for display is a rendering concern of the callback, not
	// Verify's.
	Progress func(ProgressEvent)
}

// Problem names one verification failure precisely (docs/architecture/backup-format.md, Verification Model).
type Problem struct {
	SnapshotID string
	Detail     string
}

// VerifyResult reports what Verify checked and found.
type VerifyResult struct {
	Snapshots    []string
	BlobsChecked int64
	BytesRead    int64
	Problems     []Problem
}

// errBlobUnreadable marks a chain-materialization failure that was already
// recorded as a Problem by verifyState.blob, so callers unwrapping a
// MaterializeHashMap/MaterializePageMap error know not to add a second,
// less specific Problem for the same underlying blob.
var errBlobUnreadable = errors.New("backup: referenced blob failed verification")

// Verify checks a backup repository's integrity (docs/architecture/backup-format.md, Verification Model). It takes
// a SHARED repo lock (released on return): concurrent verifies and restores
// are safe, but a running create/prune (exclusive) is not.
//
// Snapshot selection: All verifies every manifest, SnapshotID verifies one
// (an error if it does not exist), and the default verifies only the latest.
//
// In Quick mode, every referenced blob is resolved through the index and its
// pack footer, but content blobs are not read. The default full mode also
// reads and hash-verifies every content blob, checks the materialized page
// map's coverage, and cross-checks attachment-list totals against the
// manifest. Per-object failures are collected as Problems naming the
// snapshot, blob, and pack; Verify keeps going so every affected snapshot is
// named, rather than stopping at the first Problem.
func Verify(ctx context.Context, r *Repo, app App, opts VerifyOptions) (*VerifyResult, error) {
	lock, err := r.AcquireSharedLock("verify", opts.ForceUnlock)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Release() }()

	manifests, err := selectManifests(r, opts)
	if err != nil {
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
	st := &verifyState{
		app:           app,
		repo:          r,
		known:         known,
		quick:         opts.Quick,
		jobs:          jobs,
		readers:       map[string]*pack.Reader{},
		readerErrs:    map[string]error{},
		checked:       map[pack.BlobID]bool{},
		readDone:      map[pack.BlobID]bool{},
		readVerdict:   map[pack.BlobID]string{},
		pendingSet:    map[pack.BlobID]bool{},
		result:        &VerifyResult{},
		progress:      newProgressEmitter(opts.Progress),
		snapshotTotal: len(manifests),
	}
	defer st.closeReaders()

	for i, m := range manifests {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		st.snapshotIndex = i
		if st.quick {
			// Quick mode reads no content, so per-snapshot ticks are the only
			// meaningful granularity.
			st.progress.emit(ProgressEvent{Stage: ProgressStageVerify, Done: int64(i), Total: int64(len(manifests))})
		}
		st.result.Snapshots = append(st.result.Snapshots, m.SnapshotID)
		st.verifySnapshot(m)
		// Full mode's bar advances per content blob inside the drain — for a
		// single-snapshot verify (the default), snapshot-level progress would
		// sit at 0% for the whole run.
		if err := st.drainContentReads(ctx); err != nil {
			return nil, err
		}
	}
	// Full mode's final tick closes out the same cumulative drain counters
	// the bar advanced with; BlobsChecked dedupes blobs shared across
	// snapshots and would not match the bar's denominator under --all.
	finalTotal := st.drainTotal
	if st.quick {
		finalTotal = int64(len(manifests))
	}
	st.progress.emit(ProgressEvent{
		Stage: ProgressStageVerify, Done: finalTotal, Total: finalTotal,
		BytesDone: st.result.BytesRead, Final: true,
	})
	return st.result, nil
}

// selectManifests resolves VerifyOptions' snapshot selection to a manifest
// list, erroring on an empty repository or a missing explicit SnapshotID.
func selectManifests(r *Repo, opts VerifyOptions) ([]*Manifest, error) {
	if opts.All {
		list, err := r.ListSnapshots()
		if err != nil {
			return nil, err
		}
		if len(list) == 0 {
			return nil, errors.New("backup: repository has no snapshots to verify")
		}
		return list, nil
	}
	if opts.SnapshotID != "" {
		m, err := r.LoadManifest(opts.SnapshotID)
		if err != nil {
			return nil, err
		}
		return []*Manifest{m}, nil
	}
	latest, err := r.LatestSnapshot()
	if err != nil {
		return nil, err
	}
	if latest == nil {
		return nil, errors.New("backup: repository has no snapshots to verify")
	}
	return []*Manifest{latest}, nil
}

// maxOpenPackReaders bounds how many pack.Readers Verify keeps open at once.
// A repository can reference thousands of packs; without a bound the reader
// cache would exhaust the process file-descriptor limit (macOS defaults to
// 256). It is a var, not a const, only so tests can lower it. Reader errors
// stay cached forever regardless of eviction.
var maxOpenPackReaders = 64

// verifyState holds the cross-manifest resources a Verify run shares: the
// index map, a bounded LRU open-pack.Reader cache (closed before Verify
// returns), the set of already-checked blobs, and per-blob full-read verdicts
// so a content blob shared by several snapshots is read and hashed only once.
//
// Concurrency model: each snapshot's structural walk (chains, lists, extras,
// metadata reads) runs serially and enqueues its content blobs into
// pendingReads instead of reading them inline; drainContentReads then reads
// them with a pack-grouped worker pool before the next snapshot starts. mu
// guards every field the drain workers touch (result, checked, readDone,
// readVerdict, readerErrs, contentReads, progress emission); the serial
// phases run alone and need no locking.
type verifyState struct {
	app          App
	repo         *Repo
	known        map[pack.BlobID]IndexEntry
	quick        bool
	jobs         int
	readers      map[string]*pack.Reader
	readerOrder  []string // LRU order, least-recently-used first
	readerErrs   map[string]error
	checked      map[pack.BlobID]bool
	readDone     map[pack.BlobID]bool
	readVerdict  map[pack.BlobID]string // "" ok, else the cached problem detail
	contentReads int
	result       *VerifyResult
	progress     *progressEmitter
	mu           sync.Mutex
	// pendingReads/pendingSet queue the current snapshot's content blobs for
	// the drain; pendingSet dedupes repeat references within one snapshot.
	pendingReads []pendingRead
	pendingSet   map[pack.BlobID]bool
	// drainDone/drainTotal drive full-mode progress: blobs processed and
	// blobs queued, cumulative across every drain in the run so a multi-
	// snapshot verify never moves backward. Guarded by mu while workers run.
	drainDone  int64
	drainTotal int64
	// snapshotIndex/snapshotTotal are the current snapshot's 0-based position
	// and the total snapshot count being verified, for quick mode's
	// per-snapshot progress ticks.
	snapshotIndex int
	snapshotTotal int
}

// pendingRead is one queued content-blob read attributed to a snapshot.
type pendingRead struct {
	id         pack.BlobID
	snapshotID string
}

func (s *verifyState) closeReaders() {
	for _, pr := range s.readers {
		_ = pr.Close()
	}
}

func (s *verifyState) problem(snapshotID, detail string) {
	s.result.Problems = append(s.result.Problems, Problem{SnapshotID: snapshotID, Detail: detail})
}

// reader opens (or reuses) a cached pack.Reader for packID. OpenReader
// validates the footer checksum, so a damaged footer is caught here; failures
// are cached too, so a pack broken beyond repair is only opened once.
func (s *verifyState) reader(packID string) (*pack.Reader, error) {
	if pr, ok := s.readers[packID]; ok {
		s.touchReader(packID)
		return pr, nil
	}
	if err, ok := s.readerErrs[packID]; ok {
		return nil, err
	}
	pr, err := pack.OpenReader(s.repo.Path(packsDirName, packID[:2], packID+".mvpack"), nil)
	if err != nil {
		s.readerErrs[packID] = err
		return nil, err
	}
	if len(s.readers) >= maxOpenPackReaders {
		s.evictOldestReader()
	}
	s.readers[packID] = pr
	s.readerOrder = append(s.readerOrder, packID)
	return pr, nil
}

// touchReader moves packID to the most-recently-used end of the LRU order.
func (s *verifyState) touchReader(packID string) {
	for i, id := range s.readerOrder {
		if id == packID {
			s.readerOrder = append(s.readerOrder[:i], s.readerOrder[i+1:]...)
			break
		}
	}
	s.readerOrder = append(s.readerOrder, packID)
}

// evictOldestReader closes and drops the least-recently-used open reader.
func (s *verifyState) evictOldestReader() {
	if len(s.readerOrder) == 0 {
		return
	}
	oldest := s.readerOrder[0]
	s.readerOrder = s.readerOrder[1:]
	if pr, ok := s.readers[oldest]; ok {
		_ = pr.Close()
		delete(s.readers, oldest)
	}
}

// blob resolves one referenced blob and reports whether it checked out.
// readContent must be true for metadata blobs (maps, lists, the extras
// tree): their bytes are how further references are enumerated. Content
// blobs pass readContent=!quick. On success it returns the blob's raw bytes
// when readContent is true. On failure it records a Problem naming the
// snapshot, blob, and pack, and returns ok=false so the caller can skip any
// references that blob would otherwise have named.
func (s *verifyState) blob(id pack.BlobID, snapshotID string, readContent bool) ([]byte, bool) {
	ie, ok := s.known[id]
	if !ok {
		s.problem(snapshotID, fmt.Sprintf("blob %s not present in any index", id))
		return nil, false
	}
	pr, err := s.reader(ie.PackID)
	if err != nil {
		s.problem(snapshotID, fmt.Sprintf("opening pack %s for blob %s: %v", ie.PackID, id, err))
		return nil, false
	}
	entries := pr.Entries()
	var entry *pack.Entry
	for i := range entries {
		if entries[i].ID == id {
			entry = &entries[i]
			break
		}
	}
	if entry == nil {
		s.problem(snapshotID, fmt.Sprintf("blob %s missing from pack %s footer", id, ie.PackID))
		return nil, false
	}
	if entry.Offset != ie.Offset || entry.StoredLen != ie.StoredLen {
		s.problem(snapshotID, fmt.Sprintf(
			"blob %s index entry (offset %d, len %d) disagrees with pack %s footer (offset %d, len %d)",
			id, ie.Offset, ie.StoredLen, ie.PackID, entry.Offset, entry.StoredLen))
		return nil, false
	}
	if !s.checked[id] {
		s.checked[id] = true
		s.result.BlobsChecked++
	}
	if !readContent {
		return nil, true
	}
	raw, err := pr.ReadBlob(*entry)
	if err != nil {
		s.problem(snapshotID, fmt.Sprintf("reading blob %s from pack %s: %v", id, ie.PackID, err))
		return nil, false
	}
	if !s.readDone[id] {
		s.readDone[id] = true
		s.result.BytesRead += int64(entry.RawLen) //nolint:gosec // raw lengths fit int64
	}
	return raw, true
}

// verifyContentBlob checks a content blob (page data, attachment, or extras
// file) whose bytes the caller discards. In full mode it memoizes the
// read-and-hash verdict across snapshots: the first snapshot that references a
// blob reads and hashes it, and later snapshots reuse the verdict. A cached
// failure is still re-reported as a Problem naming the referencing snapshot,
// so per-snapshot attribution is preserved. Quick mode does the per-snapshot
// structural check only and never memoizes.
//
// In full mode the read itself is deferred: the blob is queued for
// drainContentReads, which runs after the snapshot's structural walk with a
// pack-grouped worker pool.
func (s *verifyState) verifyContentBlob(id pack.BlobID, snapshotID string) {
	if s.quick {
		s.blob(id, snapshotID, false)
		return
	}
	if detail, seen := s.readVerdict[id]; seen {
		if detail != "" {
			s.problem(snapshotID, detail)
		}
		return
	}
	if s.pendingSet[id] {
		return
	}
	s.pendingSet[id] = true
	s.pendingReads = append(s.pendingReads, pendingRead{id: id, snapshotID: snapshotID})
}

// drainContentReads reads and hash-verifies every content blob the current
// snapshot queued, grouped by pack so each worker reads one pack file at a
// time (sequential within the file), with s.jobs packs in flight at once.
// jobs=1 therefore reads packs strictly one after another — the safe mode
// for repositories on spinning disks. It returns an error only when ctx is
// canceled; per-blob failures become Problems, as in the serial path.
func (s *verifyState) drainContentReads(ctx context.Context) error {
	if len(s.pendingReads) == 0 {
		return ctx.Err()
	}
	groups := map[string][]pendingRead{}
	var order []string
	for _, pr := range s.pendingReads {
		ie, ok := s.known[pr.id]
		if !ok {
			detail := fmt.Sprintf("blob %s not present in any index", pr.id)
			s.problem(pr.snapshotID, detail)
			s.readVerdict[pr.id] = detail
			continue
		}
		if _, seen := groups[ie.PackID]; !seen {
			order = append(order, ie.PackID)
		}
		groups[ie.PackID] = append(groups[ie.PackID], pr)
	}
	s.pendingReads = nil
	clear(s.pendingSet)

	// The full-mode bar tracks content blobs cumulatively across the run's
	// drains: a single-snapshot verify moves smoothly instead of parking at
	// 0/1 snapshots, and an --all run grows the denominator per snapshot
	// instead of jumping back to 0/queued at each drain.
	var queued int64
	for _, g := range groups {
		queued += int64(len(g))
	}
	s.drainTotal += queued
	s.progress.emit(ProgressEvent{
		Stage: ProgressStageVerify, Done: s.drainDone, Total: s.drainTotal, BytesDone: s.result.BytesRead,
	})

	workers := min(s.jobs, len(order))
	packs := make(chan string)
	var wg sync.WaitGroup
	for range max(workers, 1) {
		wg.Go(func() {
			for packID := range packs {
				s.verifyPackGroup(packID, groups[packID])
			}
		})
	}
	var ctxErr error
	for _, packID := range order {
		if ctxErr = ctx.Err(); ctxErr != nil {
			break
		}
		packs <- packID
	}
	close(packs)
	wg.Wait()
	return ctxErr
}

// verifyPackGroup reads one pack's queued content blobs, reproducing the
// structural checks, accounting, and verdict caching of the serial
// blob()+verifyContentBlob path. ReadBlob calls run outside the state lock;
// everything recorded runs under it.
func (s *verifyState) verifyPackGroup(packID string, reads []pendingRead) {
	pr, err := s.openGroupReader(packID)
	if err != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, rd := range reads {
			detail := fmt.Sprintf("opening pack %s for blob %s: %v", packID, rd.id, err)
			s.problem(rd.snapshotID, detail)
			s.readVerdict[rd.id] = detail
			s.drainDone++
		}
		s.emitDrainProgressLocked()
		return
	}
	defer func() { _ = pr.Close() }()
	entries := pr.Entries()
	entryByID := make(map[pack.BlobID]*pack.Entry, len(entries))
	for i := range entries {
		entryByID[entries[i].ID] = &entries[i]
	}
	for _, rd := range reads {
		s.verifyGroupBlob(pr, packID, entryByID, rd)
	}
}

// openGroupReader opens a dedicated pack.Reader for one drain group. It
// shares the readerErrs cache with the serial path (a pack whose footer is
// broken is reported once per open attempt, not re-parsed per blob) but not
// the LRU reader cache, whose evictions could close a reader another worker
// is using.
func (s *verifyState) openGroupReader(packID string) (*pack.Reader, error) {
	s.mu.Lock()
	if err, ok := s.readerErrs[packID]; ok {
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Unlock()
	pr, err := pack.OpenReader(s.repo.Path(packsDirName, packID[:2], packID+".mvpack"), nil)
	if err != nil {
		s.mu.Lock()
		s.readerErrs[packID] = err
		s.mu.Unlock()
		return nil, err
	}
	return pr, nil
}

// verifyGroupBlob checks one queued blob against its pack: index/footer
// consistency, then a full read whose decode path verifies CRC and the
// content hash against the blob ID.
func (s *verifyState) verifyGroupBlob(
	pr *pack.Reader, packID string, entryByID map[pack.BlobID]*pack.Entry, rd pendingRead,
) {
	fail := func(detail string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.problem(rd.snapshotID, detail)
		s.readVerdict[rd.id] = detail
		s.drainDone++
		s.emitDrainProgressLocked()
	}
	ie := s.known[rd.id]
	entry, ok := entryByID[rd.id]
	if !ok {
		fail(fmt.Sprintf("blob %s missing from pack %s footer", rd.id, packID))
		return
	}
	if entry.Offset != ie.Offset || entry.StoredLen != ie.StoredLen {
		fail(fmt.Sprintf(
			"blob %s index entry (offset %d, len %d) disagrees with pack %s footer (offset %d, len %d)",
			rd.id, ie.Offset, ie.StoredLen, packID, entry.Offset, entry.StoredLen))
		return
	}
	s.mu.Lock()
	if !s.checked[rd.id] {
		s.checked[rd.id] = true
		s.result.BlobsChecked++
	}
	s.mu.Unlock()
	if _, err := pr.ReadBlob(*entry); err != nil {
		fail(fmt.Sprintf("reading blob %s from pack %s: %v", rd.id, packID, err))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.readDone[rd.id] {
		s.readDone[rd.id] = true
		s.result.BytesRead += int64(entry.RawLen) //nolint:gosec // raw lengths fit int64
	}
	s.readVerdict[rd.id] = ""
	s.contentReads++
	s.drainDone++
	s.emitDrainProgressLocked()
}

// emitDrainProgressLocked reports the current drain's blob progress and
// cumulative bytes read. Callers must hold s.mu.
func (s *verifyState) emitDrainProgressLocked() {
	s.progress.emit(ProgressEvent{
		Stage: ProgressStageVerify, Done: s.drainDone, Total: s.drainTotal,
		BytesDone: s.result.BytesRead,
	})
}

// fetcher adapts blob to the fetch signature MaterializeHashMap and
// MaterializePageMap expect, so their own chain-walk and decode logic
// enumerates and verifies every chain blob. A blob-level failure (already
// recorded as a Problem by blob) is surfaced as errBlobUnreadable so the
// caller does not add a second, less specific Problem for it.
func (s *verifyState) fetcher(snapshotID string) func(pack.BlobID) ([]byte, error) {
	return func(id pack.BlobID) ([]byte, error) {
		raw, ok := s.blob(id, snapshotID, true)
		if !ok {
			return nil, errBlobUnreadable
		}
		return raw, nil
	}
}

// verifySnapshot enumerates and checks every blob one manifest references
// (docs/architecture/backup-format.md, Verification Model): the page-hash-map and page-map chains, the materialized page
// map's blob table, attachment lists and the content blobs they name, and
// the extras tree and the blobs it names.
func (s *verifyState) verifySnapshot(m *Manifest) {
	s.checkHashMapChain(m)

	pageMap := s.checkPageMapChain(m)
	if pageMap != nil {
		s.checkPageMapBlobs(m, pageMap)
		if !s.quick {
			s.checkPageMapCoverage(m, pageMap)
		}
	}

	refs := s.checkAttachmentLists(m)
	if !s.quick {
		s.checkAttachmentConsistency(m, refs)
	}

	s.checkExtrasTree(m)
}

func (s *verifyState) checkHashMapChain(m *Manifest) {
	chain, err := s.repo.HashMapChain(m)
	if err != nil {
		s.problem(m.SnapshotID, fmt.Sprintf("page-hash-map chain: %v", err))
		return
	}
	hm, err := MaterializeHashMap(s.fetcher(m.SnapshotID), chain)
	if err != nil {
		if !errors.Is(err, errBlobUnreadable) {
			s.problem(m.SnapshotID, fmt.Sprintf("page-hash-map chain: %v", err))
		}
		return
	}
	if s.quick {
		return
	}
	if hm.PageCount != m.DB.PageCount {
		s.problem(m.SnapshotID, fmt.Sprintf(
			"page hash map covers %d pages but manifest reports page_count %d", hm.PageCount, m.DB.PageCount))
	}
	if hm.PageSize != m.DB.PageSize {
		s.problem(m.SnapshotID, fmt.Sprintf(
			"page hash map page_size %d disagrees with manifest page_size %d", hm.PageSize, m.DB.PageSize))
	}
}

// checkPageMapChain enumerates and decodes the page-map chain, returning the
// materialized map so the caller can check its blob table (and, in full
// mode, its coverage). It returns nil when the chain or any of its blobs
// failed, since downstream checks have nothing usable to work from.
func (s *verifyState) checkPageMapChain(m *Manifest) *PageMap {
	chain, err := s.repo.PageMapChain(m)
	if err != nil {
		s.problem(m.SnapshotID, fmt.Sprintf("page-map chain: %v", err))
		return nil
	}
	pageMap, err := MaterializePageMap(s.fetcher(m.SnapshotID), chain)
	if err != nil {
		if !errors.Is(err, errBlobUnreadable) {
			s.problem(m.SnapshotID, fmt.Sprintf("page-map chain: %v", err))
		}
		return nil
	}
	return pageMap
}

func (s *verifyState) checkPageMapBlobs(m *Manifest, pm *PageMap) {
	for _, id := range pm.Blobs {
		s.verifyContentBlob(id, m.SnapshotID)
	}
}

func (s *verifyState) checkPageMapCoverage(m *Manifest, pm *PageMap) {
	if err := pm.CheckCoverage(); err != nil {
		s.problem(m.SnapshotID, fmt.Sprintf("page map: %v", err))
	}
	if pm.PageCount != m.DB.PageCount {
		s.problem(m.SnapshotID, fmt.Sprintf(
			"page map covers %d pages but manifest reports page_count %d", pm.PageCount, m.DB.PageCount))
	}
	// Restore refuses this geometry mismatch; verify must flag it too, or a
	// snapshot could verify cleanly and still be unrestorable.
	if pm.PageSize != m.DB.PageSize {
		s.problem(m.SnapshotID, fmt.Sprintf(
			"page map page size %d disagrees with manifest page_size %d", pm.PageSize, m.DB.PageSize))
	}
}

// checkAttachmentLists decodes every attachment list blob the manifest names
// and checks every content blob those lists reference, returning the union
// of decoded refs for the full-mode consistency check.
func (s *verifyState) checkAttachmentLists(m *Manifest) []ContentRef {
	var refs []ContentRef
	for _, listBlob := range m.Attachments.Lists {
		id, err := pack.ParseBlobID(listBlob)
		if err != nil {
			s.problem(m.SnapshotID, fmt.Sprintf("attachment list blob id %q: %v", listBlob, err))
			continue
		}
		raw, ok := s.blob(id, m.SnapshotID, true)
		if !ok {
			continue
		}
		segment, err := DecodeAttachmentList(raw)
		if err != nil {
			s.problem(m.SnapshotID, fmt.Sprintf("attachment list %s: %v", id, err))
			continue
		}
		for _, ref := range segment {
			refs = append(refs, ref)
			contentID, err := pack.ParseBlobID(ref.Hash)
			if err != nil {
				s.problem(m.SnapshotID, fmt.Sprintf("attachment content hash %q: %v", ref.Hash, err))
				continue
			}
			s.verifyContentBlob(contentID, m.SnapshotID)
		}
	}
	return refs
}

// checkAttachmentConsistency cross-checks the union of decoded attachment
// list refs against the manifest's own attachment totals (docs/architecture/backup-format.md, Verification Model).
func (s *verifyState) checkAttachmentConsistency(m *Manifest, refs []ContentRef) {
	var sizeSum int64
	for _, ref := range refs {
		sizeSum += ref.Size
	}
	if int64(len(refs)) != m.Attachments.Blobs {
		s.problem(m.SnapshotID, fmt.Sprintf(
			"attachment list union has %d blobs but manifest reports attachments.blobs %d", len(refs), m.Attachments.Blobs))
	}
	if sizeSum != m.Attachments.BlobBytes {
		s.problem(m.SnapshotID, fmt.Sprintf(
			"attachment list union sums %d bytes but manifest reports attachments.blob_bytes %d", sizeSum, m.Attachments.BlobBytes))
	}
	// App-level manifest consistency (e.g. stats payload vs attachment totals)
	// is opaque to the engine; the app reports any problems it finds.
	for _, detail := range s.app.CheckManifest(m) {
		s.problem(m.SnapshotID, detail)
	}
}

// checkExtrasTree decodes the extras tree blob (if any) and checks every
// blob it names.
func (s *verifyState) checkExtrasTree(m *Manifest) {
	if m.Extras.Tree == "" {
		return
	}
	id, err := pack.ParseBlobID(m.Extras.Tree)
	if err != nil {
		s.problem(m.SnapshotID, fmt.Sprintf("extras tree blob id %q: %v", m.Extras.Tree, err))
		return
	}
	raw, ok := s.blob(id, m.SnapshotID, true)
	if !ok {
		return
	}
	var tree ExtrasTree
	if err := json.Unmarshal(raw, &tree); err != nil {
		s.problem(m.SnapshotID, fmt.Sprintf("extras tree %s: %v", id, err))
		return
	}
	for _, entry := range tree.Entries {
		blobID, err := pack.ParseBlobID(entry.Blob)
		if err != nil {
			s.problem(m.SnapshotID, fmt.Sprintf("extras entry %s blob id %q: %v", entry.Path, entry.Blob, err))
			continue
		}
		s.verifyContentBlob(blobID, m.SnapshotID)
	}
}
