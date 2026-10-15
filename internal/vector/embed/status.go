package embed

import (
	"context"
	"errors"
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

const embeddingDiagnosticSaveInterval = time.Second

// embeddingDiagnosticHeartbeat refreshes updated_at during a long phase with
// no other change, so status can tell a stalled writer from a slow provider.
const embeddingDiagnosticHeartbeat = 15 * time.Second

// embeddingDiagnosticRun asynchronously saves live phases and useful batch timings.
type embeddingDiagnosticRun struct {
	ctx    context.Context
	writer vector.EmbeddingDiagnosticWriter
	log    *slog.Logger

	mu sync.Mutex
	// saveFailures counts consecutive failed writes.
	saveFailures int
	snapshot     vector.EmbeddingDiagnostics
	previous     *vector.EmbeddingDiagnostics
	sequence     int
	batchWorked  bool
	active       bool
	finished     bool
	// flush is the pending or running write, due at flushAt; flushDone closes
	// when it returns. dirty marks changes it has not captured, which
	// schedule the next write when it finishes.
	flush     *time.Timer
	flushAt   time.Time
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

// passDiagnostics records diagnostics for a message-embedding pass. Status
// matches a snapshot to its run by integer ID, so a pass without one records
// none.
func passDiagnostics(ctx context.Context, writer vector.EmbeddingDiagnosticWriter, log *slog.Logger, gen vector.GenerationID, backend vector.Backend, pass *operationPass) *embeddingDiagnosticRun {
	if writer == nil {
		return nil
	}
	runID, ok := pass.id.Int64()
	if !ok {
		if log != nil {
			log.Warn("embedding pass has no integer run id; batch diagnostics disabled", "generation", gen)
		}
		return nil
	}
	building, err := backend.BuildingGeneration(ctx)
	var fingerprint string
	if err == nil && building != nil && building.ID == gen {
		fingerprint = building.Fingerprint
	} else if err == nil {
		active, activeErr := backend.ActiveGeneration(ctx)
		err = activeErr
		if active.ID == gen {
			fingerprint = active.Fingerprint
		}
	}
	if err != nil || fingerprint == "" {
		if log != nil {
			log.Warn("embedding generation fingerprint unavailable; diagnostics disabled", "generation", gen, "error", err)
		}
		return nil
	}
	d := newEmbeddingDiagnosticRun(ctx, writer, log, gen, runID)
	d.snapshot.GenerationFingerprint = fingerprint
	if reader, ok := writer.(interface {
		ReadEmbeddingDiagnostics(ctx context.Context, gen int64) (*vector.EmbeddingDiagnostics, error)
	}); ok {
		previous, err := reader.ReadEmbeddingDiagnostics(ctx, int64(gen))
		if err != nil {
			d.log.Warn("read previous embedding diagnostics failed", "generation", gen, "error", err)
		} else if previous != nil && previous.GenerationFingerprint == fingerprint {
			d.previous = previous
		}
	}
	return d
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
	// While writes keep failing, wait for the heartbeat instead of competing
	// for the archive's write lock every second.
	interval := embeddingDiagnosticSaveInterval
	if d.saveFailures > 1 {
		interval = embeddingDiagnosticHeartbeat
	}
	d.scheduleLocked(max(0, interval-time.Since(d.savedAt)))
}

// scheduleLocked arranges the next write after delay, moving a later pending
// heartbeat earlier. A write already running reschedules when it returns.
func (d *embeddingDiagnosticRun) scheduleLocked(delay time.Duration) {
	at := time.Now().Add(delay)
	if d.flush != nil && (!d.flushAt.After(at) || !d.flush.Stop()) {
		return
	}
	done := make(chan struct{})
	d.flush, d.flushAt, d.flushDone = time.AfterFunc(delay, func() { d.flushPending(done) }), at, done
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
	err := d.write(snapshot)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.recordSaveLocked(snapshot.GenerationID, err)
	if d.finished {
		return
	}
	d.flush = nil
	// The first failed write retries after the save interval; repeated
	// failures back off to the heartbeat in requestSaveLocked.
	d.dirty = d.dirty || err != nil
	if d.dirty {
		d.requestSaveLocked()
		return
	}
	d.scheduleLocked(embeddingDiagnosticHeartbeat)
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

// write runs one at a time, without the lock.
func (d *embeddingDiagnosticRun) write(snapshot vector.EmbeddingDiagnostics) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(d.ctx), 250*time.Millisecond)
	defer cancel()
	return d.writer.SaveEmbeddingDiagnostics(ctx, snapshot)
}

// recordSaveLocked tracks consecutive failures. Diagnostics never change the
// pass's outcome, so a failure is logged once until a later write succeeds.
func (d *embeddingDiagnosticRun) recordSaveLocked(gen vector.GenerationID, err error) {
	if err == nil {
		d.saveFailures = 0
		return
	}
	if d.saveFailures == 0 {
		d.log.Warn("save embedding diagnostics failed; embeddings status may be stale",
			"generation", gen, "error", err)
	}
	d.saveFailures++
}

func (d *embeddingDiagnosticRun) activateLocked() {
	d.active = true
}

func (d *embeddingDiagnosticRun) markWorkLocked() {
	d.activateLocked()
	d.batchWorked = true
}

func (d *embeddingDiagnosticRun) beginBatch(attempted int, startedAt time.Time) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.completeBatchLocked()
	d.sequence++
	d.batchWorked = false
	now := time.Now().UTC()
	d.snapshot.CurrentBatch = &vector.EmbeddingBatch{Sequence: d.sequence, StartedAt: startedAt.UTC(), Attempted: attempted, Phase: "read", PhaseStartedAt: now}
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
// whose scopes were already current, so it cannot displace useful timings. Its
// sequence number is not reused; a poller may already have seen it.
func (d *embeddingDiagnosticRun) completeBatchLocked() {
	if d.snapshot.CurrentBatch == nil {
		return
	}
	b := *d.snapshot.CurrentBatch
	d.snapshot.CurrentBatch = nil
	if !d.batchWorked {
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

// finish retries the final snapshot synchronously after any write in flight.
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
	if !d.active && (d.previous == nil || d.previous.FinishedAt != nil) {
		d.mu.Unlock()
		return
	}
	now := time.Now().UTC()
	d.snapshot.FinishedAt = &now
	snapshot := d.cloneLocked()
	// Preserve useful completed history when reconciliation covers no new rows.
	if len(snapshot.RecentBatches) == 0 && snapshot.LastError == nil && d.previous != nil && d.previous.FinishedAt != nil {
		snapshot = *d.previous
	}
	d.mu.Unlock()
	for attempt := range 5 {
		saveErr := d.write(snapshot)
		d.mu.Lock()
		d.recordSaveLocked(snapshot.GenerationID, saveErr)
		d.mu.Unlock()
		if saveErr == nil {
			return
		}
		if attempt < 4 {
			timer := time.NewTimer(embeddingDiagnosticSaveInterval)
			select {
			case <-d.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}
}

func (d *embeddingDiagnosticRun) failure(err error) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failureLocked(err)
}

// failureLocked ignores what ends a pass without failing its work: yielding
// to a waiting operation, running out of scheduled runtime, cancellation such
// as daemon shutdown, and retirement of the generation.
func (d *embeddingDiagnosticRun) failureLocked(err error) {
	err = jobctx.ErrorAfterYield(d.ctx, err)
	if err == nil || errors.Is(err, context.Canceled) ||
		errors.Is(err, vector.ErrGenerationRetired) || errors.Is(err, errContextGenerationRetired) {
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
		elapsed := time.Since(start)
		chars := 0
		for _, input := range inputs[:min(max(accepted, 0), len(inputs))] {
			chars += utf8.RuneCountInString(input)
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		b := d.snapshot.CurrentBatch
		if b == nil {
			return
		}
		b.ProviderMS += float64(elapsed) / float64(time.Millisecond)
		b.Chars += chars
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
	d.activateLocked()
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

// addEmbeddingPrefix counts the role prefix applied by the client after worker truncation.
func addEmbeddingPrefix(ctx context.Context, prefix string, accepted int) {
	d := diagnosticRun(ctx)
	if d == nil || prefix == "" || accepted == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if b := d.snapshot.CurrentBatch; b != nil {
		b.Chars += accepted * utf8.RuneCountInString(prefix)
		d.requestSaveLocked()
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
		d.finishRequest(start, false)
		return response, err
	}
	if response.StatusCode == http.StatusTooManyRequests {
		d.countRateLimit()
	}
	response.Body = &embeddingMeasuredBody{ReadCloser: response.Body, done: func() {
		d.finishRequest(start, response.StatusCode == http.StatusTooManyRequests)
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
	if b.Phase == "retry_wait" {
		d.setPhaseLocked("provider")
	} else {
		d.requestSaveLocked()
	}
	return true
}

func (d *embeddingDiagnosticRun) finishRequest(start time.Time, rateLimited bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if b := d.snapshot.CurrentBatch; b != nil {
		b.RequestMS += float64(time.Since(start)) / float64(time.Millisecond)
		if rateLimited {
			d.setPhaseLocked("retry_wait")
		} else {
			d.requestSaveLocked()
		}
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
