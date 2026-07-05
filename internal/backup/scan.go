package backup

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"runtime"
	"sync"
)

const (
	// runBlobMinPages: dirty ranges at least this long become dedicated run
	// blobs; shorter ranges are grouped into delta-group blobs (docs/architecture/backup-format.md, Page-Map Objects).
	runBlobMinPages = 256
	// blobMaxPages caps any page blob at 4 MiB of 4 KB pages.
	blobMaxPages = 1024
	// scanChunkPages is the read granularity of the full scan.
	scanChunkPages = 1024
)

// PageRange is a contiguous run of pages.
type PageRange struct {
	Start uint64
	Count uint64
}

// ScanResult carries the full page-hash pass and the dirty set vs the parent.
type ScanResult struct {
	PageSize  uint32
	PageCount uint64
	Hashes    []byte
	Dirty     []PageRange
}

// BlobPlan is one storage blob: either a single large run or a group of
// scattered small ranges concatenated in page order.
type BlobPlan struct {
	Ranges []PageRange
}

// Pages returns the plan's total page count.
func (p *BlobPlan) Pages() uint64 {
	var n uint64
	for _, r := range p.Ranges {
		n += r.Count
	}
	return n
}

// scanChunkJob carries one sequentially-read chunk to a hash worker.
type scanChunkJob struct {
	index int
	page  uint64
	n     uint64
	buf   []byte
}

// scanChunkDone reports one hashed chunk back to the in-order collector.
type scanChunkDone struct {
	index int
	page  uint64
	n     uint64
	dirty []bool
	buf   []byte
}

// ScanPages hashes every page and diffs against the parent hash map. The
// full scan is the honest cost of on-demand backup (docs/architecture/backup-format.md, Page-Map Objects); it doubles
// as live-DB bitrot detection. progress, if non-nil, is called once per
// chunk with the page count scanned so far and the total page count; it
// does not otherwise affect scan behavior.
//
// Internally the scan pipelines: one goroutine reads chunks strictly
// sequentially (the disk access pattern is identical to a serial scan, so
// this is safe on spinning disks), a worker per CPU hashes pages, and a
// collector reassembles per-chunk results in order. The output is
// byte-identical to a serial scan.
func ScanPages(
	ctx context.Context,
	r io.ReaderAt, pageSize uint32, pageCount uint64, parent *PageHashMap, progress func(done, total uint64),
) (*ScanResult, error) {
	if parent != nil && parent.PageSize != pageSize {
		return nil, fmt.Errorf("backup: parent snapshot page size %d does not match live DB %d", parent.PageSize, pageSize)
	}
	res := &ScanResult{
		PageSize:  pageSize,
		PageCount: pageCount,
		Hashes:    make([]byte, pageCount*pageHashSize),
	}

	chunkCount := (pageCount + scanChunkPages - 1) / scanChunkPages
	workers := runtime.GOMAXPROCS(0)
	if uint64(workers) > chunkCount { //nolint:gosec // GOMAXPROCS is a small positive int
		workers = int(chunkCount) //nolint:gosec // chunkCount < workers, a small int
	}
	workers = max(workers, 1)

	// Every buffer lives in the free channel, whose capacity covers them all,
	// so returning a buffer never blocks even if the reader stopped early.
	chunkBytes := uint64(pageSize) * scanChunkPages
	free := make(chan []byte, workers+2)
	for range workers + 2 {
		free <- make([]byte, chunkBytes)
	}
	chunks := make(chan scanChunkJob)
	hashed := make(chan scanChunkDone, workers+2)

	// Reader: the only stage that can fail. It publishes readErr, then closes
	// chunks; the workers' WaitGroup transitively guarantees the main
	// goroutine reads readErr after that write.
	var readErr error
	go func() {
		defer close(chunks)
		index := 0
		for page := uint64(0); page < pageCount; {
			if err := ctx.Err(); err != nil {
				readErr = err
				return
			}
			n := min(uint64(scanChunkPages), pageCount-page)
			buf := (<-free)[:n*uint64(pageSize)]
			if _, err := r.ReadAt(buf, int64(page)*int64(pageSize)); err != nil {
				readErr = fmt.Errorf("backup: reading DB pages %d..%d: %w", page, page+n-1, err)
				return
			}
			chunks <- scanChunkJob{index: index, page: page, n: n, buf: buf}
			index++
			page += n
		}
	}()

	// Hash workers write each page's hash directly into res.Hashes (chunks
	// never overlap) and report per-page dirtiness against the read-only
	// parent map.
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for job := range chunks {
				dirty := make([]bool, job.n)
				for i := range job.n {
					p := job.page + i
					ioff := i * uint64(pageSize)
					h := PageHash(job.buf[ioff : ioff+uint64(pageSize)])
					copy(res.Hashes[p*pageHashSize:], h[:])
					dirty[i] = parent == nil || p >= parent.PageCount ||
						!bytes.Equal(h[:], parent.Hashes[p*pageHashSize:(p+1)*pageHashSize])
				}
				hashed <- scanChunkDone{index: job.index, page: job.page, n: job.n, dirty: dirty, buf: job.buf}
			}
		})
	}
	go func() {
		wg.Wait()
		close(hashed)
	}()

	// Collector: reassemble chunk results in index order so dirty-range
	// merging and progress reporting behave exactly like a serial scan.
	var dirtyStart, dirtyLen uint64
	flush := func() {
		if dirtyLen > 0 {
			res.Dirty = append(res.Dirty, PageRange{Start: dirtyStart, Count: dirtyLen})
			dirtyLen = 0
		}
	}
	pending := map[int]scanChunkDone{}
	next := 0
	for done := range hashed {
		pending[done.index] = done
		for {
			c, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			next++
			for i := range c.n {
				p := c.page + i
				if c.dirty[i] {
					if dirtyLen == 0 {
						dirtyStart = p
					} else if dirtyStart+dirtyLen != p {
						flush()
						dirtyStart = p
					}
					dirtyLen++
				} else {
					flush()
				}
			}
			free <- c.buf[:cap(c.buf)]
			if progress != nil {
				progress(c.page+c.n, pageCount)
			}
		}
	}
	if readErr != nil {
		return nil, readErr
	}
	flush()
	if len(res.Hashes) == 0 {
		res.Hashes = nil
	}
	return res, nil
}

// PlanBlobs groups dirty ranges into storage blobs.
func PlanBlobs(dirty []PageRange) []BlobPlan {
	var plans []BlobPlan
	var group BlobPlan
	flushGroup := func() {
		if len(group.Ranges) > 0 {
			plans = append(plans, group)
			group = BlobPlan{}
		}
	}
	for _, r := range dirty {
		if r.Count >= runBlobMinPages {
			flushGroup()
			for r.Count > 0 {
				n := min(r.Count, uint64(blobMaxPages))
				plans = append(plans, BlobPlan{Ranges: []PageRange{{Start: r.Start, Count: n}}})
				r.Start += n
				r.Count -= n
			}
			continue
		}
		if group.Pages()+r.Count > blobMaxPages {
			flushGroup()
		}
		group.Ranges = append(group.Ranges, r)
	}
	flushGroup()
	return plans
}

// BuildBlobContent reads the plan's pages in order into one blob.
func BuildBlobContent(r io.ReaderAt, pageSize uint32, plan BlobPlan) ([]byte, error) {
	out := make([]byte, 0, plan.Pages()*uint64(pageSize))
	for _, pr := range plan.Ranges {
		buf := make([]byte, pr.Count*uint64(pageSize))
		if _, err := r.ReadAt(buf, int64(pr.Start)*int64(pageSize)); err != nil { //nolint:gosec
			return nil, fmt.Errorf("backup: reading pages %d..%d for blob: %w", pr.Start, pr.Start+pr.Count-1, err)
		}
		out = append(out, buf...)
	}
	return out, nil
}

// RunsForPlan emits the page-map runs describing where the plan's pages live
// inside its stored blob.
func RunsForPlan(plan BlobPlan, blobIndex uint32, pageSize uint32) []PageRun {
	var runs []PageRun
	var off uint64
	for _, pr := range plan.Ranges {
		runs = append(runs, PageRun{
			StartPage:  pr.Start,
			PageCount:  uint32(pr.Count), //nolint:gosec // plans cap ranges at blobMaxPages
			BlobIndex:  blobIndex,
			BlobOffset: off,
		})
		off += pr.Count * uint64(pageSize)
	}
	return runs
}

// BuildHashDelta converts a scan's dirty set into a hash-map delta.
func BuildHashDelta(res *ScanResult) *PageHashDelta {
	d := &PageHashDelta{PageSize: res.PageSize, PageCount: res.PageCount}
	for _, r := range res.Dirty {
		for p := r.Start; p < r.Start+r.Count; p++ {
			d.Pages = append(d.Pages, p)
			d.Hashes = append(d.Hashes, res.Hashes[p*pageHashSize:(p+1)*pageHashSize]...)
		}
	}
	return d
}
