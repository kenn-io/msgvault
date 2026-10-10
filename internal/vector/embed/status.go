package embed

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/vector"
)

type embeddingDiagnosticKey struct{}
type embeddingAttemptKey struct{}

// embeddingDiagnosticRun records one pass's batch timings. Writes run on a
// timer, at most once a second, so the measured loop never waits on the
// archive and a long phase still shows within a second of starting. Nothing is
// written until the pass does real work: a provider call, newly covered
// messages, or an error. A pass with nothing to do keeps the previous pass's
// snapshot.
type embeddingDiagnosticRun struct {
	ctx    context.Context
	writer vector.EmbeddingDiagnosticWriter
	log    *slog.Logger

	// saveFailing belongs to the one write in flight.
	saveFailing bool

	mu          sync.Mutex
	snapshot    vector.EmbeddingDiagnostics
	sequence    int
	batchWorked bool
	active      bool
	finished    bool
	// flush is the pending or running write and flushDone closes when it
	// returns; dirty marks changes it has not captured, which schedule the
	// next write when it finishes.
	flush     *time.Timer
	flushDone chan struct{}
	dirty     bool
	savedAt   time.Time
}

func newEmbeddingDiagnosticRun(ctx context.Context, writer vector.EmbeddingDiagnosticWriter, log *slog.Logger, gen vector.GenerationID, runID int64) *embeddingDiagnosticRun {
	if writer == nil {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	now := time.Now().UTC()
	return &embeddingDiagnosticRun{ctx: ctx, writer: writer, log: log, snapshot: vector.EmbeddingDiagnostics{
		GenerationID: gen, RunID: runID, StartedAt: now, UpdatedAt: now, RecentBatches: []vector.EmbeddingBatch{},
	}}
}

func withEmbeddingDiagnostics(ctx context.Context, d *embeddingDiagnosticRun) context.Context {
	if d == nil {
		return ctx
	}
	return context.WithValue(ctx, embeddingDiagnosticKey{}, d)
}
func diagnosticRun(ctx context.Context) *embeddingDiagnosticRun {
	d, _ := ctx.Value(embeddingDiagnosticKey{}).(*embeddingDiagnosticRun)
	return d
}

func (d *embeddingDiagnosticRun) requestSaveLocked() {
	if !d.active || d.finished {
		return
	}
	d.dirty = true
	if d.flush != nil {
		return
	}
	done := make(chan struct{})
	d.flushDone = done
	d.flush = time.AfterFunc(max(0, time.Second-time.Since(d.savedAt)), func() { d.flushPending(done) })
}

func (d *embeddingDiagnosticRun) flushPending(done chan struct{}) {
	defer close(done)
	d.mu.Lock()
	if d.finished {
		d.mu.Unlock()
		return
	}
	snapshot := d.cloneLocked()
	d.dirty = false
	d.mu.Unlock()
	d.write(snapshot)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.finished {
		return
	}
	d.flush = nil
	if d.dirty {
		d.requestSaveLocked()
	}
}

func (d *embeddingDiagnosticRun) cloneLocked() vector.EmbeddingDiagnostics {
	d.savedAt = time.Now()
	d.snapshot.UpdatedAt = d.savedAt.UTC()
	snapshot := d.snapshot
	if snapshot.CurrentBatch != nil {
		batch := *snapshot.CurrentBatch
		snapshot.CurrentBatch = &batch
	}
	snapshot.RecentBatches = slices.Clone(snapshot.RecentBatches)
	return snapshot
}

// write runs one at a time. Diagnostics never change the pass's outcome, so a
// failed write is logged once until a later write succeeds.
func (d *embeddingDiagnosticRun) write(snapshot vector.EmbeddingDiagnostics) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(d.ctx), 250*time.Millisecond)
	defer cancel()
	err := d.writer.SaveEmbeddingDiagnostics(ctx, snapshot)
	if err != nil && !d.saveFailing {
		d.log.Warn("save embedding diagnostics failed; embeddings status may be stale",
			"generation", snapshot.GenerationID, "error", err)
	}
	d.saveFailing = err != nil
}

func (d *embeddingDiagnosticRun) markWorkLocked() {
	d.batchWorked = true
	d.active = true
}

func (d *embeddingDiagnosticRun) beginBatch(attempted int) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.completeBatchLocked()
	d.sequence++
	d.batchWorked = false
	now := time.Now().UTC()
	d.snapshot.CurrentBatch = &vector.EmbeddingBatch{Sequence: d.sequence, StartedAt: now, Attempted: attempted, Phase: "read", PhaseStartedAt: now}
	d.requestSaveLocked()
}

func (d *embeddingDiagnosticRun) setAttempted(attempted int) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.snapshot.CurrentBatch != nil {
		d.snapshot.CurrentBatch.Attempted = attempted
	}
}

func (d *embeddingDiagnosticRun) completeBatch() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.completeBatchLocked()
}

// completeBatchLocked drops a batch that did no work, such as a reconcile page
// whose scopes were already current, so it cannot displace useful timings.
func (d *embeddingDiagnosticRun) completeBatchLocked() {
	if d.snapshot.CurrentBatch == nil {
		return
	}
	b := *d.snapshot.CurrentBatch
	d.snapshot.CurrentBatch = nil
	if !d.batchWorked {
		d.sequence--
		return
	}
	b.FinishedAt = time.Now().UTC()
	b.ElapsedMS = float64(b.FinishedAt.Sub(b.StartedAt)) / float64(time.Millisecond)
	b.Phase = "complete"
	if b.Completed > 0 {
		d.snapshot.LastSuccessfulBatchAt = &b.FinishedAt
	}
	d.snapshot.RecentBatches = append(d.snapshot.RecentBatches, b)
	if len(d.snapshot.RecentBatches) > vector.EmbeddingRecentBatchLimit {
		d.snapshot.RecentBatches = d.snapshot.RecentBatches[1:]
	}
	d.requestSaveLocked()
}

// finish writes the final snapshot synchronously, after any write in flight.
func (d *embeddingDiagnosticRun) finish(err error) {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.failureLocked(err)
	d.completeBatchLocked()
	d.finished = true
	flush, done := d.flush, d.flushDone
	d.flush = nil
	d.mu.Unlock()
	if flush != nil && !flush.Stop() {
		<-done
	}
	d.mu.Lock()
	if !d.active {
		d.mu.Unlock()
		return
	}
	now := time.Now().UTC()
	d.snapshot.FinishedAt = &now
	snapshot := d.cloneLocked()
	d.mu.Unlock()
	d.write(snapshot)
}

func (d *embeddingDiagnosticRun) failure(err error) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failureLocked(err)
}

// failureLocked ignores cancellation from yielding to a waiting operation or
// running out of scheduled runtime; those end a pass without failing it.
func (d *embeddingDiagnosticRun) failureLocked(err error) {
	err = jobctx.ErrorAfterYield(d.ctx, err)
	if err == nil {
		return
	}
	public := operationPublicError(context.Background(), err)
	d.snapshot.LastError = &operations.OperationPublicError{Code: public.Code, Message: public.Message}
	if d.snapshot.CurrentBatch != nil {
		d.snapshot.CurrentBatch.Error = d.snapshot.LastError
	}
	d.markWorkLocked()
	d.requestSaveLocked()
}

// completed counts messages that were newly covered by this batch.
func (d *embeddingDiagnosticRun) completed(n int) {
	if d == nil || n <= 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.snapshot.CurrentBatch == nil {
		return
	}
	d.snapshot.CurrentBatch.Completed += n
	d.markWorkLocked()
	d.requestSaveLocked()
}

func (d *embeddingDiagnosticRun) setPhaseLocked(phase string) {
	d.snapshot.CurrentBatch.Phase = phase
	d.snapshot.CurrentBatch.PhaseStartedAt = time.Now().UTC()
	d.requestSaveLocked()
}

// measureEmbeddingProvider times one provider call, including retry waits;
// request time excludes them. The returned function takes how many leading
// inputs the provider accepted. Chars counts only those, so a split or
// truncated retry does not count the same text twice.
func measureEmbeddingProvider(ctx context.Context, inputs []string) func(accepted int, err error) {
	d := diagnosticRun(ctx)
	if d == nil || len(inputs) == 0 {
		return func(int, error) {}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.snapshot.CurrentBatch == nil {
		return func(int, error) {}
	}
	d.markWorkLocked()
	d.setPhaseLocked("provider")
	start := time.Now()
	return func(accepted int, err error) {
		d.mu.Lock()
		defer d.mu.Unlock()
		b := d.snapshot.CurrentBatch
		if b == nil {
			return
		}
		b.ProviderMS += float64(time.Since(start)) / float64(time.Millisecond)
		for _, input := range inputs[:min(max(accepted, 0), len(inputs))] {
			b.Chars += utf8.RuneCountInString(input)
		}
		d.failureLocked(err)
		d.setPhaseLocked("read")
	}
}

func measureEmbeddingWrite(ctx context.Context) func(error) {
	d := diagnosticRun(ctx)
	if d == nil {
		return func(error) {}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.snapshot.CurrentBatch == nil {
		return func(error) {}
	}
	d.setPhaseLocked("write")
	start := time.Now()
	return func(err error) {
		d.mu.Lock()
		defer d.mu.Unlock()
		b := d.snapshot.CurrentBatch
		if b == nil {
			return
		}
		b.DBWriteMS += float64(time.Since(start)) / float64(time.Millisecond)
		d.failureLocked(err)
		d.setPhaseLocked("read")
	}
}

// Each retry loop owns one counter. Request packing/downshift calls start
// separate loops and therefore cannot be mistaken for retries.
func withEmbeddingAttemptGroup(ctx context.Context) context.Context {
	return context.WithValue(ctx, embeddingAttemptKey{}, new(int))
}

type embeddingMeasuredTransport struct{ base http.RoundTripper }

func (t embeddingMeasuredTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	d := diagnosticRun(r.Context())
	if d == nil || !d.countRequest(r.Context()) {
		return t.base.RoundTrip(r)
	}
	start := time.Now()
	response, err := t.base.RoundTrip(r)
	if err != nil {
		d.finishRequest(start)
		return response, err
	}
	if response.StatusCode == http.StatusTooManyRequests {
		d.countRateLimit()
	}
	response.Body = &embeddingMeasuredBody{ReadCloser: response.Body, done: func() {
		d.finishRequest(start)
	}}
	return response, nil
}

func (d *embeddingDiagnosticRun) countRequest(ctx context.Context) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	b := d.snapshot.CurrentBatch
	if b == nil {
		return false
	}
	b.Requests++
	if count, ok := ctx.Value(embeddingAttemptKey{}).(*int); ok {
		if *count > 0 {
			b.Retries++
		}
		*count++
	}
	d.requestSaveLocked()
	return true
}

func (d *embeddingDiagnosticRun) finishRequest(start time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if b := d.snapshot.CurrentBatch; b != nil {
		b.RequestMS += float64(time.Since(start)) / float64(time.Millisecond)
		d.requestSaveLocked()
	}
}

func (d *embeddingDiagnosticRun) countRateLimit() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if b := d.snapshot.CurrentBatch; b != nil {
		b.RateLimits++
		d.requestSaveLocked()
	}
}

type embeddingMeasuredBody struct {
	io.ReadCloser

	done func()
}

func (b *embeddingMeasuredBody) Close() error {
	err := b.ReadCloser.Close()
	if b.done != nil {
		b.done()
		b.done = nil
	}
	return err
}
