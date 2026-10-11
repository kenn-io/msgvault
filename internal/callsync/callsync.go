// Package callsync is the sync engine shared by the call-recording providers.
// Each incremental sync relists every call created since the previous sync's
// start less LateArtifactWindow and re-fetches it, so late recordings and
// transcripts arrive while a call is in that window; later ones need --full.
// The engine owns listing progress, the --limit budget, aging waiting rows out
// of the window, and recording fetch-to-store. A provider supplies its listing
// and the fetch-and-archive of one call.
package callsync

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/store"
)

const (
	// LateArtifactWindow is how long after a call is created late artifacts are
	// picked up: incremental listings relist calls created this far back.
	LateArtifactWindow = 7 * 24 * time.Hour
	// maxPages bounds one listing so a provider that never ends can't loop.
	maxPages = 100000
)

// Options bounds one sync run.
type Options struct {
	Identifier, AccountEmail, AttachmentsDir string
	Full                                     bool
	Limit                                    int
	CreatedAfter                             time.Time
	MediaPolicy                              attachmentpolicy.Policy
}

// Summary reports what one sync run did.
type Summary struct {
	SourceID, MeetingsProcessed, MeetingsAdded, MeetingsUpdated, AttachmentsStored, Errors int64
	// DiscoveryPending means --limit stopped the run with calls left to list or retry.
	DiscoveryPending bool
	Diagnostics      []string
}

// Traversal is resume progress through one listing. Its window is frozen when
// it starts, so a --limit run resumes the same listing.
type Traversal struct {
	// Window names the listing Cursor belongs to; StartedAt becomes the
	// watermark once that listing finishes.
	Window    string    `json:"window,omitempty"`
	StartedAt time.Time `json:"started_at,omitzero"`
	Cursor    string    `json:"cursor,omitempty"`
	// Handled lists item keys already processed on the page Cursor names, so
	// a run that --limit stopped mid-page doesn't repeat them.
	Handled []string `json:"handled,omitempty"`
}

// State holds listing progress. Incremental and full listings resume
// independently, so a scheduled run between --full batches doesn't restart
// the full traversal.
type State struct {
	Version     int       `json:"version"`
	Watermark   time.Time `json:"watermark,omitzero"`
	Incremental Traversal `json:"incremental,omitzero"`
	Full        Traversal `json:"full,omitzero"`
	// RetryIDs keeps failed calls reachable after they leave the relist window.
	RetryIDs []string `json:"retry_ids,omitempty"`
}

// Item is one call on a listing page with the provider's listing data.
type Item[L any] struct {
	ID     string
	Listed L
	// Key, when set, replaces ID as the item's identity within a run, so a
	// call listed again with new data is handled again.
	Key string
}

func (i Item[L]) key() string {
	if i.Key != "" {
		return i.Key
	}
	return i.ID
}

// Spec names a provider for messages and stored keys.
type Spec struct {
	// SourceType is the store source type; Label names the provider in
	// messages, and Command is its sync command.
	SourceType, Label, Command string
	// RowPrefix starts every recording row key the provider writes.
	RowPrefix string
}

// Provider is what a call-recording source supplies to the engine.
type Provider[L any] interface {
	// ListPage returns the calls on the listing page cursor names ("" for the
	// first), created on or after after, and the next page's cursor ("" at the
	// end).
	ListPage(ctx context.Context, after time.Time, cursor string) ([]Item[L], string, error)
	// Handle fetches and archives one call. listed is zero when retrying a
	// saved ID. callErr queues a retry on the next run; err stops the run.
	Handle(ctx context.Context, id string, listed L) (callErr, err error)
}

// Run is one sync run. Providers read its fields and use its helpers.
type Run struct {
	St       *store.Store
	SourceID int64
	Opts     Options
	Sum      *Summary
	Now      time.Time
	Spec     Spec
}

// Import runs one sync: it lists the window, handles each listed call once,
// then ages out the rows of calls that left the window.
func Import[L any](ctx context.Context, st *store.Store, spec Spec, now time.Time, opts Options, provider func(*Run) Provider[L]) (sum *Summary, retErr error) {
	if st == nil {
		return nil, fmt.Errorf("%s importer unavailable", strings.ToLower(spec.Label))
	}
	if opts.Limit < 0 {
		return nil, errors.New("limit must be zero or positive")
	}
	if err := opts.MediaPolicy.Validate(); err != nil {
		return nil, err
	}
	source, err := st.GetSourceByTypeAndIdentifier(spec.SourceType, opts.Identifier)
	if err != nil {
		return nil, err
	}
	sum = &Summary{SourceID: source.ID}
	runID, err := st.StartSyncContext(ctx, source.ID, spec.SourceType)
	if err != nil {
		return sum, err
	}
	scoped := st.ScopedToSync(source.ID, runID)
	checkpoint := func() *store.Checkpoint {
		return &store.Checkpoint{MessagesProcessed: sum.MeetingsProcessed, MessagesAdded: sum.MeetingsAdded, MessagesUpdated: sum.MeetingsUpdated, ErrorsCount: sum.Errors}
	}
	defer func() {
		slices.Sort(sum.Diagnostics)
		sum.Diagnostics = slices.Compact(sum.Diagnostics)
		if retErr != nil {
			retErr = errors.Join(retErr, scoped.FailSyncWithCheckpoint(runID, spec.Label+" sync incomplete", checkpoint()))
		}
	}()
	current, err := scoped.GetSourceByID(source.ID)
	if err != nil {
		return sum, err
	}
	state := State{Version: 1}
	if raw := current.SyncCursor.String; raw != "" {
		if err := json.Unmarshal([]byte(raw), &state); err != nil || state.Version != 1 {
			return sum, fmt.Errorf("invalid %s sync state", spec.Label)
		}
	}
	run := &Run{St: scoped, SourceID: source.ID, Opts: opts, Sum: sum, Now: now.UTC(), Spec: spec}
	e := &engine[L]{Run: run, p: provider(run), state: state, handled: map[string]bool{}, calls: map[string]bool{}}
	steps := []func(context.Context) error{e.retry, e.discover}
	traversal := state.Incremental
	if opts.Full {
		traversal = state.Full
	}
	if opts.Limit > 0 && traversal.Window != "" {
		// Resume limited discovery before retries can consume its budget.
		slices.Reverse(steps)
	}
	for _, step := range steps {
		if err := step(ctx); err != nil {
			return sum, err
		}
	}
	if err := e.ageOut(ctx); err != nil {
		return sum, err
	}
	if len(e.failures) > 0 {
		return sum, errors.Join(e.failures...)
	}
	if err := scoped.UpdateSyncCheckpointContext(ctx, runID, checkpoint()); err != nil {
		return sum, err
	}
	return sum, scoped.CompleteSyncAndPreserveSourceCursorContext(ctx, runID, source.ID, "")
}

type engine[L any] struct {
	*Run

	p     Provider[L]
	state State
	// failures are per-call provider errors; the run finishes, then reports them.
	failures []error
	// handled holds item keys handled this run, so a call listed again with
	// the same data is fetched once.
	handled map[string]bool
	// calls holds the call IDs handled this run, which --limit counts.
	calls map[string]bool
}

// relistBound is the lower bound the next incremental listing sends for
// watermark: listings filter by UTC date, so it's the start of that day.
func relistBound(watermark time.Time) time.Time {
	if watermark.IsZero() {
		return time.Time{}
	}
	return watermark.Add(-LateArtifactWindow).UTC().Truncate(24 * time.Hour)
}

func (e *engine[L]) save() error {
	raw, err := json.Marshal(e.state, json.Deterministic(true))
	if err != nil {
		return err
	}
	return e.St.UpdateSourceSyncState(e.SourceID, string(raw))
}

// retry handles saved failures so old calls remain reachable.
func (e *engine[L]) retry(ctx context.Context) error {
	if !e.Opts.CreatedAfter.IsZero() {
		// A bounded run may exclude the recording that needs retrying.
		return nil
	}
	for _, id := range slices.Clone(e.state.RetryIDs) {
		if e.calls[id] {
			continue
		}
		if e.Opts.Limit > 0 && int(e.Sum.MeetingsProcessed) >= e.Opts.Limit {
			e.Sum.DiscoveryPending = true
			break
		}
		if err := e.handle(ctx, Item[L]{ID: id}); err != nil {
			return err
		}
	}
	return nil
}

// discover pages the provider's listing and handles every listed call, saving
// the cursor after each page so a limited or interrupted run resumes there.
func (e *engine[L]) discover(ctx context.Context) error {
	after := e.Opts.CreatedAfter
	if !e.Opts.Full && !e.state.Watermark.IsZero() {
		if overlap := relistBound(e.state.Watermark); overlap.After(after) {
			after = overlap
		}
	}
	t := &e.state.Incremental
	if e.Opts.Full {
		t = &e.state.Full
	}
	if window := after.Format(time.RFC3339); t.Window != window {
		*t = Traversal{Window: window, StartedAt: e.Now}
	}
	cursor := t.Cursor
	seenCursors := map[string]bool{}
	for {
		if seenCursors[cursor] || len(seenCursors) >= maxPages {
			return fmt.Errorf("%s: call pagination did not terminate", strings.ToLower(e.Spec.Label))
		}
		seenCursors[cursor] = true
		items, next, err := e.p.ListPage(ctx, after, cursor)
		if err != nil {
			e.Sum.Errors++
			return err
		}
		for _, item := range items {
			key := item.key()
			if e.handled[key] || slices.Contains(t.Handled, key) {
				continue
			}
			if e.Opts.Limit > 0 && int(e.Sum.MeetingsProcessed) >= e.Opts.Limit && !e.calls[item.ID] {
				e.Sum.DiscoveryPending = true
				return e.save()
			}
			e.handled[key] = true
			t.Handled = append(t.Handled, key)
			if err := e.handle(ctx, item); err != nil {
				return err
			}
		}
		if next == "" {
			break
		}
		cursor = next
		t.Cursor, t.Handled = next, nil
		if err := e.save(); err != nil {
			return err
		}
	}
	// A bounded full sync must cover the old incremental window before it
	// can replace that window's watermark.
	coversWindow := !e.Opts.Full || e.state.Watermark.IsZero() || !after.After(relistBound(e.state.Watermark))
	if coversWindow && t.StartedAt.After(e.state.Watermark) {
		e.state.Watermark = t.StartedAt
	}
	*t = Traversal{}
	return e.save()
}

// handle archives one call. A provider failure is reported and the run goes
// on. Store errors and cancellation stop the run.
func (e *engine[L]) handle(ctx context.Context, item Item[L]) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	callErr, err := e.p.Handle(ctx, item.ID, item.Listed)
	if err == nil && callErr != nil && ctx.Err() != nil {
		err = callErr
	}
	if err != nil {
		e.Sum.Errors++
		return err
	}
	if !e.calls[item.ID] {
		e.calls[item.ID] = true
		e.Sum.MeetingsProcessed++
	}
	if callErr == nil && !e.Opts.CreatedAfter.IsZero() {
		// A successful bounded refresh may have skipped older failed media.
		// Leave any saved retry for a run without --after.
		return nil
	}
	wasRetry := slices.Contains(e.state.RetryIDs, item.ID)
	e.state.RetryIDs = slices.DeleteFunc(e.state.RetryIDs, func(id string) bool { return id == item.ID })
	if callErr != nil {
		// Put persistent failures last so limited runs reach other retries.
		e.state.RetryIDs = append(e.state.RetryIDs, item.ID)
		e.Sum.Errors++
		e.failures = append(e.failures, fmt.Errorf("%s call %s: %w", strings.ToLower(e.Spec.Label), item.ID, callErr))
	}
	if wasRetry || callErr != nil {
		// Keep retries durable even if a later call or listing stops the run.
		return e.save()
	}
	return nil
}

// ageOut ends the waiting rows of calls the next incremental listing no
// longer reaches, since nothing fetches them again before --full. A call lasts
// at most a day, so one that started a day before the bound has every
// recording older than it.
func (e *engine[L]) ageOut(ctx context.Context) error {
	bound := relistBound(e.state.Watermark)
	if bound.IsZero() {
		return nil
	}
	ended, err := e.St.EndWaitingProviderAttachments(ctx, e.SourceID, e.Spec.RowPrefix, bound.Add(-24*time.Hour), e.state.RetryIDs)
	if err != nil {
		return err
	}
	if ended > 0 {
		// Ended rows change their meetings, so they count as updates.
		e.Sum.MeetingsUpdated += ended
		e.Sum.Diagnostics = append(e.Sum.Diagnostics, fmt.Sprintf("%d recording(s) no longer retried: the call is older than the %d-day relist window; run %s --full to retry", ended, int(LateArtifactWindow.Hours()/24), e.Spec.Command))
	}
	return nil
}

// NotReady marks a pending write as waiting for a recording the provider
// hasn't produced yet, so aging it out ends it unavailable, not failed.
func NotReady(write *store.AttachmentWrite) {
	write.State, write.SkipReason = attachmentpolicy.StatePending, attachmentpolicy.SkipSourceUnavailable
}

// Waiting reports whether a recording row still waits for a fetch.
func Waiting(row store.AttachmentRef) bool {
	return row.ContentHash == "" && attachmentpolicy.RetryEligible(row.State)
}

// Retryable is implemented by provider API errors; the provider's own retry
// decision is the one Transient uses.
type Retryable interface{ Retryable() bool }

// ErrTransport marks a request that failed before the provider answered; a
// provider wraps it when it scrubs the underlying error.
var ErrTransport = errors.New("transport error")

// Transient reports a provider failure a later run may not repeat: a transport
// failure or timeout, or an API error the provider retries. Any other failure
// is permanent for the call it hit.
func Transient(err error) bool {
	var retryable Retryable
	if errors.As(err, &retryable) {
		return retryable.Retryable()
	}
	var netErr net.Error
	return errors.Is(err, ErrTransport) || errors.As(err, &netErr) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, context.DeadlineExceeded)
}

// Fetch stores one recording's audio from open into write. A failure that
// isn't Transient becomes a failed row, reported once when it first fails;
// providerErr is worth retrying later, and storeErr is a local write failure
// that must stop the run.
func (r *Run) Fetch(ctx context.Context, name string, open func(maxBytes int64) (io.ReadCloser, error), alreadyFailed bool, write *store.AttachmentWrite) (providerErr, storeErr error) {
	capBytes := r.Opts.MediaPolicy.MaxBytes
	if capBytes <= 0 {
		capBytes = attachmentpolicy.DefaultChatMaxBytes
	}
	reader, err := open(capBytes)
	if err == nil {
		body := &export.SourceReader{R: reader}
		var storagePath, hash string
		var size int64
		storagePath, hash, size, err = export.StoreAttachmentStream(ctx, r.Opts.AttachmentsDir, body, capBytes)
		_ = reader.Close()
		switch {
		case err == nil:
			write.State, write.StoragePath, write.ContentHash, write.Size = attachmentpolicy.StateStored, storagePath, hash, size
			r.Sum.AttachmentsStored++
			return nil, nil
		case !errors.Is(err, export.ErrAttachmentTooLarge) && body.Err == nil && ctx.Err() == nil:
			return nil, fmt.Errorf("store recording %s: %w", name, err)
		}
	}
	switch {
	case errors.Is(err, export.ErrAttachmentTooLarge):
		write.State, write.SkipReason = attachmentpolicy.StateSkipped, attachmentpolicy.SkipSizeCap
		r.Sum.Diagnostics = append(r.Sum.Diagnostics, fmt.Sprintf("recording %s skipped: larger than the %d MiB cap; raise max_media_mb and run %s --full", name, capBytes>>20, r.Spec.Command))
		return nil, nil
	case ctx.Err() != nil || Transient(err):
		return err, nil
	default:
		write.State, write.SkipReason = attachmentpolicy.StateFailed, attachmentpolicy.SkipFetchFailure
		if !alreadyFailed {
			r.Sum.Errors++
			r.Sum.Diagnostics = append(r.Sum.Diagnostics, "recording_fetch_failed: "+err.Error())
		}
		return nil, nil
	}
}
