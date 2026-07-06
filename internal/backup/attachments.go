package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"go.kenn.io/kit/pack"
)

const (
	attachmentListMagic   = "MVAL"
	attachmentListVersion = 1
	attachmentEntrySize   = 32 + 8
)

// ContentRef identifies one attachment (or thumbnail) content blob by its
// SHA-256 and size. Size -1 means unknown until read from disk.
//
// StoragePath is the blob's location relative to the attachments directory
// as recorded in the archive database; importers may namespace it (for
// example synctech-sms writes "synctech-sms/<aa>/<hash>"). Empty means the
// canonical loose layout "<aa>/<hash>". It is capture-time routing only and
// is not serialized into attachment list segments, which carry hash and size.
type ContentRef struct {
	Hash        string
	Size        int64
	StoragePath string
}

// EncodeAttachmentList serializes refs in their given (first-seen) order.
func EncodeAttachmentList(refs []ContentRef) ([]byte, error) {
	buf := make([]byte, 0, 4+2+4+len(refs)*attachmentEntrySize+trailerHashLen)
	buf = append(buf, attachmentListMagic...)
	buf = binary.LittleEndian.AppendUint16(buf, attachmentListVersion)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(refs))) //nolint:gosec // ref counts fit u32
	for _, ref := range refs {
		raw, err := hex.DecodeString(ref.Hash)
		if err != nil || len(raw) != 32 {
			return nil, fmt.Errorf("backup: attachment list: bad content hash %q", ref.Hash)
		}
		buf = append(buf, raw...)
		buf = binary.LittleEndian.AppendUint64(buf, uint64(ref.Size)) //nolint:gosec // sizes are non-negative at encode time
	}
	sum := sha256.Sum256(buf)
	return append(buf, sum[:]...), nil
}

// DecodeAttachmentList parses and integrity-checks a list segment.
func DecodeAttachmentList(data []byte) ([]ContentRef, error) {
	const header = 4 + 2 + 4
	if len(data) < header+trailerHashLen {
		return nil, fmt.Errorf("backup: attachment list truncated (%d bytes)", len(data))
	}
	body, trailer := data[:len(data)-trailerHashLen], data[len(data)-trailerHashLen:]
	sum := sha256.Sum256(body)
	if !bytes.Equal(sum[:], trailer) {
		return nil, errors.New("backup: attachment list integrity check failed")
	}
	if string(body[:4]) != attachmentListMagic {
		return nil, errors.New("backup: bad attachment list magic")
	}
	if v := binary.LittleEndian.Uint16(body[4:6]); v != attachmentListVersion {
		return nil, fmt.Errorf("backup: unsupported attachment list version %d", v)
	}
	count := binary.LittleEndian.Uint32(body[6:10])
	bodyLen := len(body) - header
	if bodyLen < 0 || uint64(bodyLen) != uint64(count)*attachmentEntrySize {
		return nil, fmt.Errorf("backup: attachment list body size mismatch (count %d)", count)
	}
	refs := make([]ContentRef, 0, count)
	off := header
	for range count {
		refs = append(refs, ContentRef{
			Hash: hex.EncodeToString(body[off : off+32]),
			Size: int64(binary.LittleEndian.Uint64(body[off+32 : off+40])), //nolint:gosec // sizes fit int64
		})
		off += attachmentEntrySize
	}
	return refs, nil
}

// AttachmentCapture reports one snapshot's attachment capture results.
type AttachmentCapture struct {
	NewList     []ContentRef
	NewListBlob pack.BlobID
	HasNewList  bool
	Blobs       int64
	BlobBytes   int64
}

// CaptureOptions tunes CaptureAttachments.
type CaptureOptions struct {
	// Jobs is the number of concurrent read+hash+compress workers. Zero or
	// negative selects one per CPU. Use 1 for strictly serial file reads —
	// the right choice when the live archive sits on a spinning disk or NAS
	// share that degrades under concurrent reads.
	Jobs int
	// Progress, if non-nil, is called after each file is captured with the
	// number of files done so far, the total file count, and the cumulative
	// bytes read; it does not otherwise affect capture behavior.
	Progress func(done, total int, bytesRead int64)
}

// captureResult is one worker's read+hash+compress output for refs[index].
type captureResult struct {
	index      int
	size       int64
	id         pack.BlobID
	frame      []byte
	compressed bool
	known      bool
	err        error
}

// captureMemoryBudget bounds the attachment content bytes admitted into the
// capture pipeline at once. Workers hold a whole file while hashing and
// trial-compressing it, so an unweighted worker pool over a library of large
// videos would hold many complete files in memory simultaneously. Trial
// compression transiently holds content plus candidate frame, so peak usage
// can briefly reach about twice this budget. A var so tests can shrink it.
var captureMemoryBudget int64 = 1 << 30

// byteGate admits work under a byte budget. A request larger than the whole
// budget is admitted once nothing else is in flight, so oversized files
// serialize instead of deadlocking.
type byteGate struct {
	mu      sync.Mutex
	cond    *sync.Cond
	held    int64
	budget  int64
	stopped bool
}

func newByteGate(budget int64) *byteGate {
	g := &byteGate{budget: budget}
	g.cond = sync.NewCond(&g.mu)
	return g
}

// acquire blocks until n bytes fit under the budget or the gate is stopped.
func (g *byteGate) acquire(n int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for !g.stopped && g.held > 0 && g.held+n > g.budget {
		g.cond.Wait()
	}
	g.held += n
}

func (g *byteGate) release(n int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.held -= n
	g.cond.Broadcast()
}

// stop unblocks every current and future acquire; called when capture fails
// so a dispatcher waiting on budget can observe the stop channel and exit.
func (g *byteGate) stop() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stopped = true
	g.cond.Broadcast()
}

// CaptureAttachments stores every referenced attachment content blob,
// re-hashing each file as it goes (docs/architecture/backup-format.md, Attachment Lists: backup verifies the live store).
// Refs not present in parentSeen become the snapshot's new list segment.
//
// Reading, hashing, and trial compression fan out to opts.Jobs workers;
// results are recorded in ref order by a single collector feeding the
// appender, so pack contents, list order, accounting, and progress reporting
// match a serial capture exactly. Blobs already stored in the repository are
// detected before compression and skip it entirely, keeping the no-change
// incremental case cheap.
func CaptureAttachments(
	ctx context.Context,
	attachmentsDir string, refs []ContentRef, parentSeen map[string]bool, appender *PackAppender,
	opts CaptureOptions,
) (*AttachmentCapture, error) {
	out := &AttachmentCapture{}
	if err := captureContents(ctx, attachmentsDir, refs, parentSeen, appender, opts, out); err != nil {
		return nil, err
	}
	if len(out.NewList) > 0 {
		data, err := EncodeAttachmentList(out.NewList)
		if err != nil {
			return nil, err
		}
		id, _, err := appender.Add(data)
		if err != nil {
			return nil, err
		}
		out.NewListBlob = id
		out.HasNewList = true
	}
	return out, nil
}

// captureContents runs the capture pipeline: a dispatcher hands ref indexes
// to workers under a bounded in-flight window, workers read+hash+compress,
// and the collector (this goroutine) records results strictly in ref order.
// The first error — by ref order, matching what a serial capture would have
// hit first — stops dispatch and is returned after the pipeline drains.
func captureContents(
	ctx context.Context,
	attachmentsDir string, refs []ContentRef, parentSeen map[string]bool, appender *PackAppender,
	opts CaptureOptions, out *AttachmentCapture,
) error {
	if len(refs) == 0 {
		return nil
	}
	workers := opts.Jobs
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	workers = min(workers, len(refs))

	// Workers must not read the appender's live known map (the collector
	// mutates it); they consult a snapshot instead. Refs are unique within a
	// run, so the snapshot's answer is exact for every queued blob.
	preKnown := appender.knownSnapshot()
	level := appender.zstdLevel

	// inflight bounds dispatched-but-unrecorded refs; the byte gate below
	// additionally bounds their cumulative size, since a count bound alone
	// lets a worker-per-CPU pool hold that many complete video files at
	// once. results has the same capacity as tokens, so workers never block
	// on a stalled collector.
	inflight := workers + 2
	stop := make(chan struct{})
	work := make(chan int)
	results := make(chan captureResult, inflight)
	tokens := make(chan struct{}, inflight)
	gate := newByteGate(captureMemoryBudget)
	// weights[i] is written by the dispatcher before index i is dispatched
	// and read by the collector only after i's result arrives; the channel
	// sends order those accesses.
	weights := make([]int64, len(refs))

	go func() {
		defer close(work)
		for i := range refs {
			if rel, err := captureRelPath(refs[i]); err == nil {
				if info, err := os.Stat(filepath.Join(attachmentsDir, rel)); err == nil {
					weights[i] = info.Size()
				}
				// A stat failure dispatches at weight zero; captureRef
				// reports the real error at the right position.
			}
			gate.acquire(weights[i])
			select {
			case <-stop:
				gate.release(weights[i])
				return
			case <-ctx.Done():
				gate.release(weights[i])
				return
			case tokens <- struct{}{}:
			}
			select {
			case <-stop:
				gate.release(weights[i])
				return
			case <-ctx.Done():
				gate.release(weights[i])
				return
			case work <- i:
			}
		}
	}()
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for i := range work {
				results <- captureRef(attachmentsDir, refs[i], i, preKnown, level)
			}
		})
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	pending := map[int]captureResult{}
	next := 0
	var firstErr error
	for res := range results {
		if firstErr != nil {
			gate.release(weights[res.index])
			continue // draining after failure
		}
		if err := ctx.Err(); err != nil {
			firstErr = err
			close(stop)
			gate.stop()
			gate.release(weights[res.index])
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
				firstErr = recordCapture(c, refs, parentSeen, appender, opts, out)
			}
			gate.release(weights[c.index])
			if firstErr != nil {
				close(stop)
				gate.stop()
			}
		}
	}
	if firstErr == nil {
		// The dispatcher exits silently on ctx.Done; without this check an
		// early cancellation could report a partial capture as success.
		firstErr = ctx.Err()
	}
	return firstErr
}

// captureRelPath resolves ref's location relative to the attachments
// directory: the database-recorded storage path when one is set (rejecting
// traversal and absolute paths — they come from DB rows and must never read
// outside the attachments directory), the canonical loose "<aa>/<hash>"
// derivation otherwise.
func captureRelPath(ref ContentRef) (string, error) {
	if ref.StoragePath == "" {
		// The loose layout keys on the first two hash characters, so a
		// too-short hash from a corrupt DB row would otherwise panic on the
		// slice below.
		if len(ref.Hash) < 2 {
			return "", fmt.Errorf("backup: attachment content hash %q is too short for the loose store layout", ref.Hash)
		}
		return filepath.Join(ref.Hash[:2], ref.Hash), nil
	}
	rel := filepath.FromSlash(ref.StoragePath)
	if !filepath.IsLocal(rel) {
		return "", fmt.Errorf("backup: attachment %s storage path %q escapes the attachments directory", ref.Hash, ref.StoragePath)
	}
	return rel, nil
}

// captureRef reads, hash-verifies, and (for blobs the repository does not
// already hold) trial-compresses one attachment file. Runs on a worker.
func captureRef(
	attachmentsDir string, ref ContentRef, index int, preKnown map[pack.BlobID]struct{}, level int,
) captureResult {
	// Failing validation here (not in an upfront sweep) keeps error reporting
	// in strict ref order: the collector surfaces whichever failure a serial
	// capture would have hit first, whatever its kind.
	rel, err := captureRelPath(ref)
	if err != nil {
		return captureResult{index: index, err: err}
	}
	content, err := os.ReadFile(filepath.Join(attachmentsDir, rel))
	if err != nil {
		return captureResult{index: index, err: fmt.Errorf("backup: reading attachment %s: %w", rel, err)}
	}
	sum := sha256.Sum256(content)
	if hex.EncodeToString(sum[:]) != ref.Hash {
		return captureResult{
			index: index,
			err:   fmt.Errorf("backup: attachment %s content does not match its hash (live store corruption)", rel),
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

// recordCapture applies one worker result in ref order: append the frame
// (unless the blob was already stored), fill the ref's size, and update
// accounting, the new-list segment, and progress. Runs on the collector.
func recordCapture(
	c captureResult, refs []ContentRef, parentSeen map[string]bool, appender *PackAppender,
	opts CaptureOptions, out *AttachmentCapture,
) error {
	ref := &refs[c.index]
	ref.Size = c.size
	if !c.known {
		if _, err := appender.AddEncoded(c.id, c.frame, uint64(c.size), c.compressed); err != nil { //nolint:gosec // sizes are non-negative
			return err
		}
	}
	out.Blobs++
	out.BlobBytes += c.size
	if !parentSeen[ref.Hash] {
		out.NewList = append(out.NewList, *ref)
	}
	if opts.Progress != nil {
		opts.Progress(c.index+1, len(refs), out.BlobBytes)
	}
	return nil
}

// LoadListRefs fetches and decodes a manifest's attachment list blobs.
func LoadListRefs(r *Repo, known map[pack.BlobID]IndexEntry, listBlobIDs []string, crypter *pack.Crypter) ([]ContentRef, map[string]bool, error) {
	var refs []ContentRef
	seen := map[string]bool{}
	for _, s := range listBlobIDs {
		id, err := pack.ParseBlobID(s)
		if err != nil {
			return nil, nil, fmt.Errorf("backup: attachment list blob id %q: %w", s, err)
		}
		data, err := r.ReadBlob(known, id, crypter)
		if err != nil {
			return nil, nil, err
		}
		segment, err := DecodeAttachmentList(data)
		if err != nil {
			return nil, nil, fmt.Errorf("backup: attachment list %s: %w", s, err)
		}
		for _, ref := range segment {
			refs = append(refs, ref)
			seen[ref.Hash] = true
		}
	}
	return refs, seen, nil
}
