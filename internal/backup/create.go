package backup

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/pack"
)

// CreateOptions parameterizes one snapshot capture.
type CreateOptions struct {
	DBPath                string
	AttachmentsDir        string
	DataDir               string
	ConfigPath            string
	IncludeConfig         bool
	IncludeTokens         bool
	AllowPlaintextSecrets bool
	Tag                   string
	ZstdLevel             int
	CacheDir              string
	MsgvaultVersion       string
	Freezer               FreezeCoordinator
	ForceUnlock           bool
	// Jobs is the number of concurrent attachment read+compress workers.
	// Zero or negative selects one per CPU. Use 1 for strictly serial file
	// reads when the live archive sits on a spinning disk or NAS share. The
	// page scan is unaffected: its disk reads are sequential at any setting.
	Jobs int
	// Progress, if non-nil, receives structured progress events as Create
	// runs. nil means fully silent. Create emits events freely and cheaply;
	// throttling for display is a rendering concern of the callback, not
	// Create's.
	Progress func(ProgressEvent)
}

// manifestExcluded names the live-archive paths a snapshot never captures.
var manifestExcluded = []string{"vectors.db", "analytics/", "logs/", "imports/", "tmp/", "locks"}

// Create captures one snapshot: freeze -> scan -> pack -> index -> manifest
// (written last). See docs/architecture/backup-format.md.
func Create(ctx context.Context, r *Repo, opts CreateOptions) (*Manifest, error) {
	start := time.Now()
	pr := newProgressEmitter(opts.Progress)
	if opts.ZstdLevel == 0 {
		opts.ZstdLevel = pack.DefaultZstdLevel
	}
	if opts.Freezer == nil {
		opts.Freezer = NoopFreezeCoordinator{}
	}

	lock, err := r.AcquireExclusiveLock("create", opts.ForceUnlock)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Release() }()
	if err := r.CleanStaging(); err != nil {
		return nil, err
	}

	known, err := r.LoadBlobIndex()
	if err != nil {
		return nil, err
	}
	fetch := func(id pack.BlobID) ([]byte, error) { return r.ReadBlob(known, id, nil) }

	parent, err := r.LatestSnapshot()
	if err != nil {
		return nil, err
	}
	parentHash, err := loadParentHashMap(r, parent, opts.CacheDir, fetch)
	if err != nil {
		return nil, err
	}

	pr.emit(ProgressEvent{Stage: ProgressStageFreeze, Total: 1})
	session, err := OpenFrozenSession(ctx, opts.DBPath, opts.Freezer)
	if err != nil {
		return nil, err
	}
	defer func() { _ = session.Close() }()
	dbBytes := int64(session.PageCount * uint64(session.PageSize)) //nolint:gosec // page-count*page-size fits int64 for real databases
	pr.emit(ProgressEvent{
		Stage: ProgressStageFreeze, Done: 1, Total: 1,
		BytesDone: dbBytes, BytesTotal: dbBytes, Final: true,
	})

	stats, err := session.Stats(ctx)
	if err != nil {
		return nil, err
	}
	refs, err := session.AttachmentRefs(ctx)
	if err != nil {
		return nil, err
	}
	nonCanonicalPaths, err := session.HasNonCanonicalAttachmentPaths(ctx)
	if err != nil {
		return nil, err
	}

	dbFile, err := os.Open(opts.DBPath)
	if err != nil {
		return nil, fmt.Errorf("backup: opening DB for scan: %w", err)
	}
	defer func() { _ = dbFile.Close() }()

	if parentHash != nil && parentHash.PageSize != session.PageSize {
		parentHash = nil // page size changed (e.g. VACUUM INTO); full re-capture
	}
	// parentMapUsable tracks whether the parent's page-map chain is safe to
	// materialize and merge against this scan: false both when there is no
	// parent and when the parent's page size no longer matches the live DB
	// (parentHash was just nulled above for that case). ParentID/lineage on
	// the manifest still records the true parent snapshot either way.
	parentMapUsable := parentHash != nil
	pageBytes := int64(session.PageSize)
	scan, err := ScanPages(ctx, dbFile, session.PageSize, session.PageCount, parentHash, func(done, total uint64) {
		pr.emit(ProgressEvent{
			Stage:      ProgressStageScan,
			Done:       int64(done),              //nolint:gosec // page counts fit int64 for real databases
			Total:      int64(total),             //nolint:gosec // page counts fit int64 for real databases
			BytesDone:  int64(done) * pageBytes,  //nolint:gosec // page counts fit int64 for real databases
			BytesTotal: int64(total) * pageBytes, //nolint:gosec // page counts fit int64 for real databases
		})
	})
	if err != nil {
		return nil, err
	}

	pr.emit(ProgressEvent{
		Stage: ProgressStageScan, Done: int64(scan.PageCount), Total: int64(scan.PageCount), //nolint:gosec // page counts fit int64
		BytesDone: dbBytes, BytesTotal: dbBytes, Final: true,
	})

	appender := NewPackAppender(r, known, opts.ZstdLevel, nil)
	ok := false
	defer func() {
		if !ok {
			appender.Abort()
		}
	}()

	delta, err := storePageBlobs(ctx, dbFile, scan, appender, opts.Jobs, pr)
	if err != nil {
		return nil, err
	}

	keyframe, chainDepth, err := decideKeyframe(r, parent, parentHash, known)
	if err != nil {
		return nil, err
	}
	mapObj, err := buildPageMapObject(r, parent, parentMapUsable, delta, keyframe, fetch)
	if err != nil {
		return nil, err
	}
	mapBlob, _, err := appender.Add(mapObj)
	if err != nil {
		return nil, err
	}
	var hashObj []byte
	if keyframe {
		hashObj = EncodeHashKeyframe(&PageHashMap{PageSize: scan.PageSize, PageCount: scan.PageCount, Hashes: scan.Hashes})
	} else {
		hashObj = EncodeHashDelta(BuildHashDelta(scan))
	}
	hashBlob, _, err := appender.Add(hashObj)
	if err != nil {
		return nil, err
	}

	parentSeen := map[string]bool{}
	if parent != nil {
		_, parentSeen, err = LoadListRefs(r, known, parent.Attachments.Lists, nil)
		if err != nil {
			return nil, err
		}
	}
	// Attachment lists are inherited append-only only while the parent union
	// stays a subset of the current ref set. If any parent-listed ref is no
	// longer present locally (e.g. after remove-account), the union would
	// exceed the current set and Verify's list-union == manifest-count check
	// would permanently fail. In that shrinkage case, write one fresh full
	// list of exactly the current refs by capturing with an empty seen set,
	// so the new snapshot's single list equals the current population.
	shrunk := parentUnionShrank(parentSeen, refs)
	captureSeen := parentSeen
	if shrunk {
		captureSeen = map[string]bool{}
	}
	capture, err := CaptureAttachments(ctx, opts.AttachmentsDir, refs, captureSeen, appender, CaptureOptions{
		Jobs: opts.Jobs,
		Progress: func(done, total int, bytesRead int64) {
			pr.emit(ProgressEvent{
				Stage: ProgressStageAttachments, Done: int64(done), Total: int64(total), BytesDone: bytesRead,
			})
		},
	})
	if err != nil {
		return nil, err
	}
	pr.emit(ProgressEvent{
		Stage: ProgressStageAttachments, Done: capture.Blobs, Total: capture.Blobs,
		BytesDone: capture.BlobBytes, BytesTotal: capture.BlobBytes, Final: true,
	})
	var lists []string
	if shrunk {
		if capture.HasNewList {
			lists = []string{capture.NewListBlob.String()}
		}
	} else {
		if parent != nil {
			lists = append(lists, parent.Attachments.Lists...)
		}
		if capture.HasNewList {
			lists = append(lists, capture.NewListBlob.String())
		}
	}

	treeBlob, hasTree, err := CaptureExtras(ExtrasOptions{
		DataDir:               opts.DataDir,
		ConfigPath:            opts.ConfigPath,
		IncludeConfig:         opts.IncludeConfig,
		IncludeTokens:         opts.IncludeTokens,
		AllowPlaintextSecrets: opts.AllowPlaintextSecrets,
		Encrypted:             false,
	}, appender)
	if err != nil {
		return nil, err
	}

	pr.emit(ProgressEvent{Stage: ProgressStageSeal, Total: 1})
	newPacks, newEntries, err := appender.Finish()
	if err != nil {
		return nil, err
	}
	ok = true
	pr.emit(ProgressEvent{Stage: ProgressStageSeal, Done: 1, Total: 1, Final: true})

	var bytesAdded int64
	for _, e := range newEntries {
		bytesAdded += int64(e.StoredLen) //nolint:gosec // stored lengths fit int64
	}
	newIndex := ""
	if len(newEntries) > 0 {
		newIndex, err = r.WriteIndex(newEntries)
		if err != nil {
			return nil, err
		}
	}
	if err := r.SetPageSize(int(session.PageSize)); err != nil {
		return nil, err
	}

	createdAt, err := nextCreatedAt(time.Now(), parent)
	if err != nil {
		return nil, err
	}

	// A snapshot whose attachment population records non-canonical storage
	// paths needs a path-aware restore: version-1 readers placed every blob
	// at "<aa>/<hash>" and would materialize a database pointing at missing
	// files, so such manifests require reader version 2 and old readers
	// refuse them explicitly instead of restoring a broken tree.
	manifestVersion := FormatVersion
	manifestMinReader := MinReaderVersion
	if nonCanonicalPaths {
		manifestVersion = dbPathManifestVersion
		manifestMinReader = dbPathManifestVersion
	}
	m := &Manifest{
		FormatVersion:    manifestVersion,
		MinReaderVersion: manifestMinReader,
		MsgvaultVersion:  opts.MsgvaultVersion,
		CreatedAt:        createdAt.Format(time.RFC3339),
		Options: ManifestOptions{
			IncludeConfig: opts.IncludeConfig,
			IncludeTokens: opts.IncludeTokens,
			ZstdLevel:     opts.ZstdLevel,
			Tag:           opts.Tag,
		},
		DB: ManifestDB{
			Engine:        "sqlite",
			PageSize:      scan.PageSize,
			PageCount:     scan.PageCount,
			PageMap:       mapBlob.String(),
			PageHashMap:   hashBlob.String(),
			MapChainDepth: chainDepth,
		},
		Attachments: ManifestAttachments{
			Layout:    []string{"loose"},
			Rows:      stats.AttachmentRows,
			Blobs:     capture.Blobs,
			BlobBytes: capture.BlobBytes,
			Recipes:   []string{},
			Lists:     lists,
		},
		Excluded:        manifestExcluded,
		Stats:           stats,
		NewPacks:        newPacks,
		NewIndex:        newIndex,
		DurationSeconds: time.Since(start).Seconds(),
		BytesAdded:      bytesAdded,
	}
	if parent != nil {
		m.ParentID = parent.SnapshotID
	}
	if hasTree {
		m.Extras.Tree = treeBlob.String()
	}
	id, err := r.WriteManifest(m)
	if err != nil {
		return nil, err
	}
	m.SnapshotID = id

	// The local cache is disposable: a save failure must not fail the backup.
	fullHash := &PageHashMap{PageSize: scan.PageSize, PageCount: scan.PageCount, Hashes: scan.Hashes}
	_ = SaveHashMapCache(opts.CacheDir, r.Config().RepoID, id, fullHash)
	return m, nil
}

// nextCreatedAt returns the timestamp to record as this snapshot's
// CreatedAt. Snapshot IDs embed CreatedAt truncated to 1-second resolution
// (docs/architecture/backup-format.md), and ListSnapshots/LatestSnapshot rely on lexicographic ID
// order matching chronological order. Create holds the repo's exclusive
// lock for its entire run, so it can safely enforce that invariant here: if
// the parent's timestamp (truncated to seconds) is not strictly before
// now's, the new timestamp is bumped to one second past the parent's. This
// guarantees every new snapshot's ID sorts after its parent's even when two
// Create calls land in the same wall-clock second.
func nextCreatedAt(now time.Time, parent *Manifest) (time.Time, error) {
	now = now.UTC()
	if parent == nil {
		return now, nil
	}
	parentCreatedAt, err := time.Parse(time.RFC3339, parent.CreatedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("backup: parent snapshot %s created_at %q: %w", parent.SnapshotID, parent.CreatedAt, err)
	}
	parentSecond := parentCreatedAt.UTC().Truncate(time.Second)
	if !now.Truncate(time.Second).After(parentSecond) {
		return parentSecond.Add(time.Second), nil
	}
	return now, nil
}

// loadParentHashMap prefers the local cache when it matches the parent
// snapshot, else materializes the chain from the repo.
func loadParentHashMap(r *Repo, parent *Manifest, cacheDir string, fetch func(pack.BlobID) ([]byte, error)) (*PageHashMap, error) {
	if parent == nil {
		return nil, nil //nolint:nilnil // no parent snapshot -> no parent hash map, not an error
	}
	if cacheDir != "" {
		snapID, cached, err := LoadHashMapCache(cacheDir, r.Config().RepoID)
		if err == nil && cached != nil && snapID == parent.SnapshotID {
			return cached, nil
		}
	}
	chain, err := r.HashMapChain(parent)
	if err != nil {
		return nil, err
	}
	return MaterializeHashMap(fetch, chain)
}

// parentUnionShrank reports whether any hash the parent's attachment lists
// enumerate is missing from the current frozen ref set. When true, the
// append-only list inheritance would leave the manifest's list union a strict
// superset of the current population, so Create writes a fresh full list
// instead.
func parentUnionShrank(parentSeen map[string]bool, refs []ContentRef) bool {
	if len(parentSeen) == 0 {
		return false
	}
	current := make(map[string]bool, len(refs))
	for _, ref := range refs {
		current[ref.Hash] = true
	}
	for hash := range parentSeen {
		if !current[hash] {
			return true
		}
	}
	return false
}

// pageBlobResult is one worker's read+hash+compress output for plans[index].
type pageBlobResult struct {
	index      int
	id         pack.BlobID
	frame      []byte
	rawLen     uint64
	compressed bool
	known      bool
	err        error
}

// buildPageBlob reads, hashes, and (for blobs the repository does not
// already hold) trial-compresses one planned dirty-page blob. Runs on a
// worker; dbFile reads use ReadAt, which is concurrent-safe.
func buildPageBlob(
	dbFile *os.File, pageSize uint32, plan BlobPlan, index int,
	preKnown map[pack.BlobID]struct{}, level int,
) pageBlobResult {
	content, err := BuildBlobContent(dbFile, pageSize, plan)
	if err != nil {
		return pageBlobResult{index: index, err: err}
	}
	res := pageBlobResult{index: index, id: pack.ComputeBlobID(content), rawLen: uint64(len(content))}
	if _, ok := preKnown[res.id]; ok {
		res.known = true
		return res
	}
	res.frame, res.compressed = pack.EncodeFrame(content, level)
	return res
}

// storePageBlobs stores every dirty-page blob and returns the page-map delta
// describing the new blobs. Reading and compression fan out to jobs workers
// (zero or negative: one per CPU; one: strictly serial reads, for archives
// on spinning disks or NAS shares); a single in-order collector feeds the
// appender, so pack contents and the delta are identical to a serial run.
// Progress is reported per plan under ProgressStagePack — on a first backup
// this phase, not the scan, is most of the wall clock.
func storePageBlobs(
	ctx context.Context,
	dbFile *os.File, scan *ScanResult, appender *PackAppender, jobs int, pr *progressEmitter,
) (*PageMap, error) {
	delta := &PageMap{PageSize: scan.PageSize, PageCount: scan.PageCount}
	plans := PlanBlobs(scan.Dirty)
	if len(plans) == 0 {
		return delta, nil
	}
	var totalPages int64
	for i := range plans {
		totalPages += int64(plans[i].Pages()) //nolint:gosec // page counts fit int64
	}
	pageBytes := int64(scan.PageSize)
	pr.emit(ProgressEvent{Stage: ProgressStagePack, Total: totalPages, BytesTotal: totalPages * pageBytes})

	workers := jobs
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	workers = min(workers, len(plans))
	preKnown := appender.knownSnapshot()
	level := appender.zstdLevel

	// inflight bounds dispatched-but-unrecorded plans; each holds at most
	// one blob (4 MiB max), so memory stays small without a byte gate.
	inflight := workers + 2
	stop := make(chan struct{})
	work := make(chan int)
	results := make(chan pageBlobResult, inflight)
	tokens := make(chan struct{}, inflight)

	go func() {
		defer close(work)
		for i := range plans {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case tokens <- struct{}{}:
			}
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case work <- i:
			}
		}
	}()
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for i := range work {
				results <- buildPageBlob(dbFile, scan.PageSize, plans[i], i, preKnown, level)
			}
		})
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	blobIdx := map[pack.BlobID]uint32{}
	pending := map[int]pageBlobResult{}
	next := 0
	var donePages int64
	var firstErr error
	for res := range results {
		if firstErr != nil {
			continue // draining after failure
		}
		if err := ctx.Err(); err != nil {
			firstErr = err
			close(stop)
			continue
		}
		pending[res.index] = res
		for firstErr == nil {
			c, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			next++
			<-tokens
			if c.err != nil {
				firstErr = c.err
			} else {
				firstErr = recordPageBlob(c, plans[c.index], scan.PageSize, appender, delta, blobIdx)
			}
			if firstErr != nil {
				close(stop)
				continue
			}
			donePages += int64(plans[c.index].Pages()) //nolint:gosec // page counts fit int64
			pr.emit(ProgressEvent{
				Stage: ProgressStagePack, Done: donePages, Total: totalPages,
				BytesDone: donePages * pageBytes, BytesTotal: totalPages * pageBytes,
			})
		}
	}
	if firstErr == nil {
		// The dispatcher exits silently on ctx.Done; without this check an
		// early cancellation could return a partial delta as success.
		firstErr = ctx.Err()
	}
	if firstErr != nil {
		return nil, firstErr
	}
	sort.Slice(delta.Runs, func(i, j int) bool { return delta.Runs[i].StartPage < delta.Runs[j].StartPage })
	pr.emit(ProgressEvent{
		Stage: ProgressStagePack, Done: totalPages, Total: totalPages,
		BytesDone: totalPages * pageBytes, BytesTotal: totalPages * pageBytes, Final: true,
	})
	return delta, nil
}

// recordPageBlob applies one worker result in plan order: append the frame
// (unless the blob is already stored — AddEncoded also dedupes repeats
// within this run) and record the plan's runs against the blob's index.
func recordPageBlob(
	c pageBlobResult, plan BlobPlan, pageSize uint32,
	appender *PackAppender, delta *PageMap, blobIdx map[pack.BlobID]uint32,
) error {
	if !c.known {
		if _, err := appender.AddEncoded(c.id, c.frame, c.rawLen, c.compressed); err != nil {
			return err
		}
	}
	idx, seen := blobIdx[c.id]
	if !seen {
		idx = uint32(len(delta.Blobs)) //nolint:gosec // blob counts fit u32
		delta.Blobs = append(delta.Blobs, c.id)
		blobIdx[c.id] = idx
	}
	delta.Runs = append(delta.Runs, RunsForPlan(plan, idx, pageSize)...)
	return nil
}

// decideKeyframe applies the keyframe cadence (docs/architecture/backup-format.md, Page-Map Objects): keyframe when
// there is no usable parent, the chain would reach keyframeChainMax, or the
// chain's stored delta bytes exceed the keyframe's stored bytes.
func decideKeyframe(r *Repo, parent *Manifest, parentHash *PageHashMap, known map[pack.BlobID]IndexEntry) (bool, int, error) {
	if parent == nil || parentHash == nil {
		return true, 0, nil
	}
	depth := parent.DB.MapChainDepth + 1
	if depth >= keyframeChainMax {
		return true, 0, nil
	}
	chain, err := r.PageMapChain(parent)
	if err != nil {
		return false, 0, err
	}
	var deltaBytes, keyframeBytes uint64
	for i, id := range chain {
		e, ok := known[id]
		if !ok {
			return false, 0, fmt.Errorf("backup: page-map chain blob %s missing from indexes", id)
		}
		if i == len(chain)-1 {
			keyframeBytes = e.StoredLen
		} else {
			deltaBytes += e.StoredLen
		}
	}
	if deltaBytes > keyframeBytes {
		return true, 0, nil
	}
	return false, depth, nil
}

// buildPageMapObject encodes this snapshot's page-map object: the delta
// itself, or a merged keyframe when the cadence calls for one. parentMapUsable
// is false when there is no parent, or when the parent's page-map chain was
// built at a page size that no longer matches this scan (e.g. VACUUM changed
// page_size); in that case the parent chain is never walked and the delta —
// which already covers every page, since a page-size change forces a full
// rescan — becomes the keyframe as-is.
func buildPageMapObject(r *Repo, parent *Manifest, parentMapUsable bool, delta *PageMap, keyframe bool, fetch func(pack.BlobID) ([]byte, error)) ([]byte, error) {
	if !keyframe {
		return EncodePageMap(delta, true), nil
	}
	full := delta
	if parentMapUsable && parent != nil {
		chain, err := r.PageMapChain(parent)
		if err != nil {
			return nil, fmt.Errorf("backup: loading parent page-map chain for keyframe: %w", err)
		}
		parentMap, err := MaterializePageMap(fetch, chain)
		if err != nil {
			return nil, err
		}
		full, err = ApplyPageMapDelta(parentMap, delta)
		if err != nil {
			return nil, err
		}
	}
	if err := full.CheckCoverage(); err != nil {
		return nil, fmt.Errorf("backup: keyframe page map incomplete: %w", err)
	}
	return EncodePageMap(full, false), nil
}
