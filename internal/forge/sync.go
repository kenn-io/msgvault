package forge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/messageimport"
	"go.kenn.io/msgvault/internal/store"
)

const (
	// checkpointEvery bounds how long a run archives threads between saves
	// of its resume state. Discovery saves after every page.
	checkpointEvery = 10 * time.Second
	// maxStoredErrorChars bounds the error a sync-run item keeps.
	maxStoredErrorChars = 500
)

// ErrEveryThreadFailed means no thread a sync tried could be archived: the
// source is failing, not one thread.
var ErrEveryThreadFailed = errors.New("every thread failed")

// errPause stops a run at a safe boundary so it can pause: a rate limit
// was reached, or the scheduler asked it to yield.
var errPause = errors.New("sync paused")

// Options configures one sync.
type Options struct {
	Owners OwnerFilter
	// Full forgets how far discovery came, so every thread is found and
	// fetched again; posts archived unchanged are not written twice.
	Full bool
}

// Summary is what one sync did.
type Summary struct {
	SourceID int64
	Login    string
	// Found counts threads this run queued that were not queued before.
	Found int
	// Archived counts threads whose every post this run wrote or matched.
	Archived int
	// Added and Updated count messages archived for the first time and
	// messages whose stored copy changed.
	Added   int
	Updated int
	// Unavailable counts threads that are gone or that the token may not
	// read; they are dropped.
	Unavailable int
	// Excluded counts queued threads whose repository now belongs to an
	// excluded owner; they are dropped unfetched.
	Excluded int
	// Failed counts threads that failed in this run; they stay queued.
	Failed int
	// Missed counts what discovery found but couldn't turn into a thread,
	// such as a notified discussion whose lookup failed; the next sync
	// looks again.
	Missed int
	// Remaining counts the threads still queued for a later sync.
	Remaining int
	// RateLimit is set when a rate limit paused the run.
	RateLimit *RateLimitError
	// Paused reports that the run stopped early and the next sync resumes
	// it: a rate limit, the scheduler's time budget, or other scheduled work.
	Paused bool
}

// Syncer syncs the sources of one forge account.
type Syncer struct {
	store  *store.Store
	forge  Forge
	logger *slog.Logger
	now    func() time.Time
}

// NewSyncer syncs through forge, which holds the account's token.
func NewSyncer(st *store.Store, forge Forge, logger *slog.Logger) *Syncer {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Syncer{store: st, forge: forge, logger: logger, now: time.Now}
}

// run is one sync run of a forge source.
type run struct {
	syncer   *Syncer
	source   *store.Source
	scoped   *store.Store
	syncID   int64
	writer   *messageimport.Writer
	known    *knownThreads
	owners   OwnerFilter
	state    syncState
	counts   store.Checkpoint
	summary  Summary
	lastSave time.Time
	// stoppedEarly reports that discovery or archiving stopped before it was
	// done because it had to: the run pauses rather than completes.
	stoppedEarly bool
	// loaded reports that state holds the resume state this run started
	// from. Until it does, the run publishes no state, so an early failure
	// leaves the previous checkpoint the one the next run resumes from.
	loaded bool
}

// Sync finds the threads that changed since the last sync and archives
// them, newest first. A run that reaches a rate limit, its scheduled time
// budget or a request to yield pauses with its queue saved, and the next
// sync continues it.
func (s *Syncer) Sync(ctx context.Context, sourceID int64, opts Options) (summary Summary, retErr error) {
	source, err := s.store.GetSourceByIDContext(ctx, sourceID)
	if err != nil {
		return Summary{SourceID: sourceID}, fmt.Errorf("resolve %s source: %w", s.forge.Name(), err)
	}
	if source.SourceType != s.forge.Name() {
		return Summary{SourceID: sourceID}, fmt.Errorf("source %d is a %s source, not %s", sourceID, source.SourceType, s.forge.Name())
	}
	syncID, err := s.store.StartSyncContext(ctx, source.ID, s.forge.Name())
	if err != nil {
		return Summary{SourceID: source.ID}, fmt.Errorf("start %s sync: %w", s.forge.Name(), err)
	}
	scoped := s.store.ScopedToSync(source.ID, syncID)
	r := &run{
		syncer: s,
		source: source,
		scoped: scoped,
		syncID: syncID,
		writer: messageimport.NewWriter(scoped, source).
			WithSenderIdentifierType(s.forge.Name()).
			WithConversationType(ConversationType),
		known:   &knownThreads{store: scoped, sourceID: source.ID},
		owners:  opts.Owners,
		summary: Summary{SourceID: source.ID},
	}
	defer func() {
		retErr = r.finish(ctx, retErr)
		summary = r.summary
	}()

	if r.state, err = r.loadState(ctx); err != nil {
		return r.summary, err
	}
	r.loaded = true
	account, err := s.forge.Account(ctx)
	if err != nil {
		return r.summary, err
	}
	if SourceIdentifier(s.forge.Instance(), account.ID) != source.Identifier {
		return r.summary, fmt.Errorf("the stored token belongs to %s account %s, not this source's; add the account again", s.forge.Name(), account.Login)
	}
	r.summary.Login = account.Login

	if fingerprint := opts.Owners.fingerprint(); opts.Full || r.state.Owners != fingerprint {
		r.state.Owners = fingerprint
		r.state.Cursor = nil
	}
	if opts.Full {
		r.state.Full = true
		// A full sync fetches every queued thread from its start too.
		for i := range r.state.Queue {
			r.state.Queue[i].Continuation = nil
		}
		// It fetches every archived thread as well, including one no list
		// shows any more, such as a release whose notification is gone.
		var archived []ThreadRef
		if err := r.known.Each(ctx, func(thread KnownThread) error {
			if r.owners.Allows(thread.Ref.Owner) {
				archived = append(archived, thread.Ref)
			}
			return nil
		}); err != nil {
			return r.summary, err
		}
		r.summary.Found += r.state.queue(archived)
	}
	// The run's first checkpoint holds the state it resumes from, so the
	// newest checkpoint is always a whole state.
	if err := r.save(ctx); err != nil {
		return r.summary, err
	}
	if err := r.discover(ctx); err != nil {
		return r.summary, err
	}
	if err := r.archiveQueue(ctx); err != nil {
		return r.summary, err
	}
	if attempted := r.summary.Archived + r.summary.Failed; attempted > 0 && r.summary.Failed == attempted {
		return r.summary, fmt.Errorf("%w (%d threads)", ErrEveryThreadFailed, attempted)
	}
	return r.summary, nil
}

// loadState returns the newest resume state: the newest checkpoint since
// the source's last completed run, else that run's cursor, else a fresh
// state. A state this version can't read is replaced by a fresh one:
// discovery finds the same threads again and rewrites nothing unchanged.
func (r *run) loadState(ctx context.Context) (syncState, error) {
	blob := ""
	checkpointed, err := r.scoped.GetLatestCheckpointedSyncContext(ctx, r.source.ID)
	switch {
	case err == nil && checkpointed.CursorBefore.Valid:
		blob = checkpointed.CursorBefore.String
	case err == nil || errors.Is(err, store.ErrSyncRunNotFound):
		last, err := r.scoped.GetLastSuccessfulSyncContext(ctx, r.source.ID)
		switch {
		case err == nil && last.CursorAfter.Valid:
			blob = last.CursorAfter.String
		case err != nil && !errors.Is(err, store.ErrSyncRunNotFound):
			return syncState{}, fmt.Errorf("read last %s sync: %w", r.syncer.forge.Name(), err)
		}
	default:
		return syncState{}, fmt.Errorf("read latest %s checkpoint: %w", r.syncer.forge.Name(), err)
	}
	state, err := decodeState(blob)
	if err != nil {
		r.syncer.logger.WarnContext(ctx, "forge sync state unreadable; discovering again",
			"source_id", r.source.ID, "error", err)
		return syncState{Version: stateVersion}, nil
	}
	return state, nil
}

// discover queues the threads that changed since the last sync, saving
// after every page, so an interrupted discovery resumes where it stopped.
// A thread archived at the update discovery reports is unchanged and not
// queued: a forge's lists overlap at their boundaries, and a thread they
// show again needs no fetch. A full sync queues every thread it finds.
func (r *run) discover(ctx context.Context) error {
	err := r.syncer.forge.Discover(ctx, r.state.Cursor, r.owners, r.known,
		func(threads []ThreadRef, next []byte) error {
			var archived map[string]KnownThread
			if !r.state.Full {
				var err error
				if archived, err = r.known.all(ctx); err != nil {
					return err
				}
			}
			allowed := slices.DeleteFunc(slices.Clone(threads), func(ref ThreadRef) bool {
				copied, ok := archived[ref.Key]
				return !r.owners.Allows(ref.Owner) || ok && copied.Holds(ref.UpdatedAt)
			})
			r.summary.Found += r.state.queue(allowed)
			r.state.Cursor = next
			if err := r.save(ctx); err != nil {
				return err
			}
			if jobctx.PreemptionRequested(ctx) {
				return errPause
			}
			return nil
		})
	var incomplete *IncompleteError
	switch {
	case err == nil:
		r.state.Full = false
		return nil
	case errors.As(err, &incomplete):
		r.state.Full = false
		for _, missed := range incomplete.Missed {
			r.summary.Missed++
			r.counts.ErrorsCount++
			r.syncer.logger.WarnContext(ctx, "forge discovery missed a thread", "thread", missed.Name, "error", missed.Err)
			r.recordItemID(ctx, "discovery:"+missed.Name, store.SyncRunItemStatusError, "forge_discovery_missed", missed.Err)
		}
		return nil
	case errors.Is(err, errPause):
		r.stoppedEarly = true
		return nil
	case IsRateLimit(err):
		// What discovery found so far is queued; archive what other limits
		// allow before pausing.
		r.noteRateLimit(err)
		r.stoppedEarly = true
		return nil
	default:
		return err
	}
}

// archiveQueue archives queued threads, newest first, until each was tried
// once or the run must pause. A thread that fails moves behind the others,
// keeping the place its fetch reached. The queue it works on is the state's,
// so every checkpoint, including one in the middle of a thread, records the
// fetch's progress.
func (r *run) archiveQueue(ctx context.Context) error {
	defer func() { r.summary.Remaining = len(r.state.Queue) }()
	for tries := len(r.state.Queue); tries > 0 && len(r.state.Queue) > 0; tries-- {
		if ctx.Err() != nil {
			return nil
		}
		if r.paused(ctx) {
			r.stoppedEarly = true
			return nil
		}
		thread := &r.state.Queue[0]
		err := r.archive(ctx, thread)
		switch {
		case err == nil:
			r.summary.Archived++
		case errors.Is(err, errExcluded):
			r.summary.Excluded++
		case ctx.Err() != nil:
			// A failure of the thread's own, not of the stop, still counts.
			if failure := jobctx.ErrorAfterYield(ctx, err); budgetOrYield(ctx) && failure != nil &&
				!errors.Is(failure, context.Canceled) && !errors.Is(failure, context.DeadlineExceeded) {
				r.fail(ctx, thread, failure)
				r.state.Queue = r.state.Queue[1:]
			}
			return nil
		case errors.Is(err, errPause):
			r.stoppedEarly = true
			return nil
		case IsRateLimit(err):
			r.noteRateLimit(err)
			r.stoppedEarly = true
			return nil
		case errors.Is(err, ErrUnavailable):
			r.summary.Unavailable++
			r.recordItem(ctx, thread.Ref, store.SyncRunItemStatusSkipped, "forge_thread_unavailable", err)
		default:
			r.fail(ctx, thread, err)
		}
		r.state.Queue = r.state.Queue[1:]
		if r.syncer.now().Sub(r.lastSave) >= checkpointEvery {
			if err := r.save(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

// fail records a thread that failed and moves it behind the others, with
// the place its fetch reached; the caller drops it from the front.
func (r *run) fail(ctx context.Context, thread *queuedThread, err error) {
	r.summary.Failed++
	r.counts.ErrorsCount++
	r.syncer.logger.WarnContext(ctx, "forge thread sync failed", "thread", thread.Ref.Name(), "error", err)
	r.recordItem(ctx, thread.Ref, store.SyncRunItemStatusError, "forge_thread_failed", err)
	r.state.Queue = append(r.state.Queue, *thread)
}

// errExcluded drops a queued thread whose repository moved to an excluded
// owner after it was found.
var errExcluded = errors.New("owner excluded")

// archive checks the thread's owner, then fetches and writes its posts page
// by page, keeping the place each page reached on the queued thread.
func (r *run) archive(ctx context.Context, thread *queuedThread) error {
	forge := r.syncer.forge
	owner, err := forge.Owner(ctx, thread.Ref)
	if err != nil {
		return err
	}
	if !r.owners.Allows(owner) {
		return errExcluded
	}
	if thread.Continuation == nil {
		thread.ReadAt = r.syncer.now()
	}
	var head ThreadHead
	err = forge.Fetch(ctx, thread.Ref, thread.Continuation, func(page ThreadHead, posts []Post, next []byte) error {
		head = page
		for _, post := range posts {
			if err := r.write(ctx, page, post); err != nil {
				return err
			}
		}
		thread.Continuation = next
		if next != nil {
			// The page's posts are written; a sync that dies now resumes
			// after them. Only threads of several pages pay for this.
			if err := r.save(ctx); err != nil {
				return err
			}
		}
		if next != nil && r.paused(ctx) {
			return errPause
		}
		return nil
	})
	if err != nil {
		return err
	}
	return recordThread(ctx, r.scoped, r.source.ID, head, thread.Ref, thread.ReadAt, r.syncer.now())
}

// write archives one post. A post the archive can't store, such as one
// without a body or title, is recorded and skipped.
func (r *run) write(ctx context.Context, head ThreadHead, post Post) error {
	forge := r.syncer.forge
	result, err := r.writer.WriteFrom(ctx, importMessage(forge, head, post), messageimport.Evidence{
		Raw: canonicalJSON(post.Raw), Format: forge.Name() + "_json",
	})
	if errors.Is(err, messageimport.ErrValidation) {
		r.recordItemID(ctx, post.ID, store.SyncRunItemStatusSkipped, "forge_post_invalid", err)
		return nil
	}
	if err != nil {
		return fmt.Errorf("archive %s: %w", post.ID, err)
	}
	r.counts.MessagesProcessed++
	switch {
	case result.Created:
		r.summary.Added++
		r.counts.MessagesAdded++
	case result.Changed:
		r.summary.Updated++
		r.counts.MessagesUpdated++
	}
	return nil
}

// paused reports whether the run must stop at this boundary: a rate limit
// was reached, or the scheduler asked it to yield.
func (r *run) paused(ctx context.Context) bool {
	return r.summary.RateLimit != nil || jobctx.PreemptionRequested(ctx)
}

func (r *run) noteRateLimit(err error) {
	if limited, ok := errors.AsType[*RateLimitError](err); ok && r.summary.RateLimit == nil {
		r.summary.RateLimit = limited
	}
}

// checkpoint is the run's resume state with its counters.
func (r *run) checkpoint() (*store.Checkpoint, error) {
	blob, err := r.state.encode()
	if err != nil {
		return nil, err
	}
	cp := r.counts
	cp.PageToken = blob
	return &cp, nil
}

// save checkpoints the run. It runs even when the sync was canceled, so
// the queue survives an interruption, and it records the progress for the
// scheduler, which then queues a follow-up of a paused run.
func (r *run) save(ctx context.Context) error {
	cp, err := r.checkpoint()
	if err != nil {
		return err
	}
	if err := r.scoped.UpdateSyncCheckpointContext(context.WithoutCancel(ctx), r.syncID, cp); err != nil {
		return fmt.Errorf("checkpoint %s sync: %w", r.syncer.forge.Name(), err)
	}
	r.lastSave = r.syncer.now()
	jobctx.RecordProgress(ctx)
	return nil
}

// finish records the run's outcome. A finished queue completes the run and
// publishes the state as the source's cursor; a run stopped by a rate
// limit, its scheduled budget or a request to yield pauses; another
// interruption or an error is recorded as the store records them.
func (r *run) finish(ctx context.Context, retErr error) error {
	if IsRateLimit(retErr) && ctx.Err() == nil {
		r.noteRateLimit(retErr)
		r.stoppedEarly = true
		retErr = nil
	}
	r.summary.Remaining = len(r.state.Queue)
	cleanup := context.WithoutCancel(ctx)
	// A run that never loaded its resume state publishes none, so the
	// previous checkpoint stays the one the next run resumes from.
	var cp *store.Checkpoint
	if r.loaded {
		var err error
		if cp, err = r.checkpoint(); err != nil {
			return errors.Join(retErr, err, r.scoped.FailSyncWithCheckpointContext(cleanup, r.syncID, err.Error(), nil))
		}
	}
	if ctx.Err() != nil && budgetOrYield(ctx) {
		// The scheduler stopped the run; an independent failure still fails it.
		retErr = jobctx.ErrorAfterYield(ctx, retErr)
		if retErr == nil && cp != nil {
			r.summary.Paused = true
			return r.scoped.PauseSyncWithCheckpoint(cleanup, r.syncID, cp)
		}
	}
	switch {
	case retErr == nil && ctx.Err() == nil && r.stoppedEarly && cp != nil:
		r.summary.Paused = true
		return r.scoped.PauseSyncWithCheckpoint(cleanup, r.syncID, cp)
	case retErr != nil:
		return errors.Join(retErr, r.scoped.FailSyncWithCheckpointContext(cleanup, r.syncID, retErr.Error(), cp))
	case ctx.Err() != nil:
		return errors.Join(ctx.Err(), r.scoped.InterruptSyncWithCheckpoint(ctx, r.syncID, ctx.Err(), cp))
	}
	if err := r.scoped.UpdateSyncCheckpointContext(cleanup, r.syncID, cp); err != nil {
		err = fmt.Errorf("checkpoint %s sync: %w", r.syncer.forge.Name(), err)
		return errors.Join(err, r.scoped.FailSyncWithCheckpointContext(cleanup, r.syncID, err.Error(), cp))
	}
	if err := r.scoped.CompleteSyncAndUpdateSourceCursorContext(cleanup, r.syncID, r.source.ID, cp.PageToken); err != nil {
		return fmt.Errorf("complete %s sync: %w", r.syncer.forge.Name(), err)
	}
	return nil
}

// budgetOrYield reports whether a canceled run was stopped by the
// scheduler, not by the user: its time budget ran out or it yielded.
func budgetOrYield(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), jobctx.ErrRunBudgetExceeded) || jobctx.YieldedToWaiter(ctx)
}

// recordItem keeps a diagnostic on the sync run; failing to record it only
// loses the diagnostic.
func (r *run) recordItem(ctx context.Context, ref ThreadRef, status, kind string, err error) {
	r.recordItemID(ctx, "thread:"+ref.Name(), status, kind, err)
}

func (r *run) recordItemID(ctx context.Context, id, status, kind string, err error) {
	_ = r.scoped.RecordSyncRunItemContext(context.WithoutCancel(ctx), store.SyncRunItem{
		SyncRunID: r.syncID, SourceMessageID: id, Phase: "fetch",
		Status: status, ErrorKind: kind, ErrorMessage: truncate(err.Error(), maxStoredErrorChars),
	})
}

func truncate(text string, maxChars int) string {
	runes := []rune(text)
	if len(runes) <= maxChars {
		return text
	}
	return string(runes[:maxChars])
}
