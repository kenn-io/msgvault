package github

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/messageimport"
	"go.kenn.io/msgvault/internal/store"
)

const (
	syncType = SourceType
	// saveEvery is how many threads pass between saves of the queue, so a
	// sync that dies without saving fetches at most this many again. An
	// interrupted or yielding sync saves when it stops.
	saveEvery = 100
	// maxStoredErrorChars bounds the error a sync-run item keeps.
	maxStoredErrorChars = 500
	// conversationMetadataKey holds a thread's GitHub facts in its
	// conversation's metadata.
	conversationMetadataKey = "github"
)

// ErrEveryThreadFailed means no thread a sync tried could be archived: the
// source is failing, not one thread.
var ErrEveryThreadFailed = errors.New("every GitHub thread failed")

// Options configures one sync.
type Options struct {
	Owners OwnerFilter
	// Full forgets how far discovery came: every notification and every
	// thread the account is involved in is found and fetched again.
	Full bool
}

// Summary is what one sync did.
type Summary struct {
	SourceID int64
	Login    string
	// Found counts the threads discovery found changed in this run.
	Found int
	// Archived counts the threads whose content this run archived.
	Archived int
	// Added and Updated count messages archived for the first time and
	// messages whose stored copy changed.
	Added   int
	Updated int
	// Unavailable counts threads that are gone or that the token may not
	// read; they are dropped.
	Unavailable int
	// Failed counts threads that failed in this run; they stay queued.
	Failed int
	// Remaining counts the threads still queued for a later sync.
	Remaining int
	// Excluded counts threads of excluded owners, which are never fetched.
	Excluded int
	// Unsupported counts notifications with no conversation to archive,
	// such as CI runs and security alerts.
	Unsupported int
	// RateLimit is set when a rate limit ended the run before the queue did.
	RateLimit *RateLimitError
}

// Syncer archives one GitHub account's threads.
type Syncer struct {
	store  *store.Store
	client *Client
	logger *slog.Logger
}

func NewSyncer(st *store.Store, client *Client, logger *slog.Logger) *Syncer {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Syncer{store: st, client: client, logger: logger}
}

// syncRun is one sync run of a GitHub source.
type syncRun struct {
	syncer     *Syncer
	source     *store.Source
	scoped     *store.Store
	syncID     int64
	writer     *messageimport.Writer
	checkpoint *store.Checkpoint
	state      syncState
	summary    Summary
}

// Sync finds the threads that changed since the last sync and archives
// them, newest first. A run that reaches the rate-limit reserve, or yields
// to other scheduled work, keeps the rest queued for the next sync.
func (s *Syncer) Sync(ctx context.Context, sourceID int64, opts Options) (summary Summary, retErr error) {
	source, err := s.store.GetSourceByIDContext(ctx, sourceID)
	if err != nil {
		return Summary{SourceID: sourceID}, fmt.Errorf("resolve GitHub source: %w", err)
	}
	syncID, err := s.store.StartSyncContext(ctx, source.ID, syncType)
	if err != nil {
		return Summary{SourceID: source.ID}, fmt.Errorf("start GitHub sync: %w", err)
	}
	scoped := s.store.ScopedToSync(source.ID, syncID)
	run := &syncRun{
		syncer:     s,
		source:     source,
		scoped:     scoped,
		syncID:     syncID,
		writer:     messageimport.NewWriter(scoped, source).WithSenderIdentifierType(ParticipantIdentifierType),
		checkpoint: &store.Checkpoint{},
		summary:    Summary{SourceID: source.ID},
	}
	defer func() {
		summary = run.summary
		// The run's outcome must be recorded even when the sync was canceled.
		cleanup := context.WithoutCancel(ctx)
		if ctx.Err() != nil {
			if retErr == nil {
				retErr = ctx.Err()
			}
			retErr = errors.Join(retErr, scoped.InterruptSyncWithCheckpoint(ctx, syncID, retErr, run.checkpoint))
			return
		}
		if retErr != nil {
			retErr = errors.Join(retErr, scoped.FailSyncWithCheckpointContext(cleanup, syncID, retErr.Error(), run.checkpoint))
			return
		}
		if err := scoped.UpdateSyncCheckpointContext(cleanup, syncID, run.checkpoint); err != nil {
			retErr = fmt.Errorf("checkpoint GitHub sync: %w", err)
			retErr = errors.Join(retErr, scoped.FailSyncWithCheckpointContext(cleanup, syncID, retErr.Error(), run.checkpoint))
			return
		}
		if err := scoped.CompleteSyncContext(cleanup, syncID, ""); err != nil {
			retErr = fmt.Errorf("complete GitHub sync: %w", err)
		}
	}()

	viewer, err := s.client.Viewer(ctx)
	if err != nil {
		return run.summary, err
	}
	if strconv.FormatInt(viewer.ID, 10) != source.Identifier {
		return run.summary, fmt.Errorf("the stored token belongs to GitHub account %s, not this source's; add the account again with msgvault add-github", viewer.Login)
	}
	run.summary.Login = viewer.Login

	run.state, err = decodeState(source.SyncCursor)
	if err != nil {
		// The state only says how far discovery came; starting over
		// finds the same threads again and rewrites nothing unchanged.
		s.logger.WarnContext(ctx, "GitHub sync state unreadable; discovering again", "source_id", source.ID, "error", err)
		run.state = syncState{}
	}
	if fingerprint := opts.Owners.fingerprint(); opts.Full || run.state.Owners != fingerprint {
		run.state.restartDiscovery(fingerprint)
	}

	discoverErr := run.discover(ctx, viewer.Login, opts.Owners)
	if err := run.save(ctx); err != nil {
		return run.summary, errors.Join(discoverErr, err)
	}
	if discoverErr != nil {
		return run.summary, discoverErr
	}
	if err := run.archiveQueue(ctx, opts.Owners); err != nil {
		return run.summary, err
	}
	if attempted := run.summary.Archived + run.summary.Failed; attempted > 0 && run.summary.Failed == attempted {
		return run.summary, fmt.Errorf("%w (%d threads)", ErrEveryThreadFailed, attempted)
	}
	return run.summary, nil
}

// discover queues the threads that changed since the last sync. A rate
// limit ends discovery early with what it found so far; the cursors only
// cover what was read, so the next sync continues from there.
func (r *syncRun) discover(ctx context.Context, login string, owners OwnerFilter) error {
	client := r.syncer.client
	var found []Thread

	notifications, err := client.scanNotifications(ctx, r.state.Notifications.Since, r.state.Notifications.LastModified, owners)
	switch {
	case isRateLimit(err):
		r.noteRateLimit(err)
	case err != nil:
		return err
	default:
		threads, err := r.resolveDiscussions(ctx, notifications.Discussions)
		switch {
		case isRateLimit(err):
			// The notification cursor stays put, so the next sync reads
			// these notifications again and resolves every discussion.
			r.noteRateLimit(err)
		case err != nil:
			return err
		default:
			found = append(found, notifications.Threads...)
			found = append(found, threads...)
			r.summary.Excluded += notifications.Excluded
			r.summary.Unsupported += notifications.Unsupported
			r.state.Notifications = notificationCursor{LastModified: notifications.LastModified, Since: notifications.Latest}
		}
	}

	for _, search := range []struct {
		kind    searchKind
		through *time.Time
	}{
		{searchIssues, &r.state.IssuesThrough},
		{searchDiscussions, &r.state.DiscussionsThrough},
	} {
		if r.summary.RateLimit != nil {
			break
		}
		scan, err := client.searchInvolvement(ctx, search.kind, login, *search.through, owners)
		// What the search read before an error is covered; keep it.
		found = append(found, scan.Threads...)
		r.summary.Excluded += scan.Excluded
		*search.through = scan.Through
		switch {
		case isRateLimit(err):
			r.noteRateLimit(err)
		case err != nil:
			r.queue(found)
			return err
		}
	}
	r.queue(found)
	return nil
}

// resolveDiscussions finds the numbers of notified discussions, which
// GitHub's notifications don't carry. A discussion that can't be found is
// recorded and dropped.
func (r *syncRun) resolveDiscussions(ctx context.Context, discussions []discussionNotification) ([]Thread, error) {
	threads := make([]Thread, 0, len(discussions))
	for _, d := range discussions {
		number, err := r.syncer.client.findDiscussion(ctx, d)
		if errors.Is(err, ErrUnavailable) {
			r.summary.Unavailable++
			r.recordItem(ctx, "discussion:"+d.Owner+"/"+d.Repo+":"+d.Title, store.SyncRunItemStatusSkipped, "github_thread_unavailable", err)
			continue
		}
		if err != nil {
			return nil, err
		}
		threads = append(threads, Thread{
			Kind: KindDiscussion, Owner: d.Owner, Repo: d.Repo, Number: number,
			UpdatedAt: d.UpdatedAt, Reason: d.Reason,
		})
	}
	return threads, nil
}

func (r *syncRun) queue(found []Thread) {
	before := len(r.state.Pending)
	r.state.Pending = mergeThreads(r.state.Pending, found)
	r.summary.Found += len(r.state.Pending) - before
	r.summary.Remaining = len(r.state.Pending)
}

// archiveQueue archives queued threads, newest first, until the queue is
// empty, a rate limit reaches its reserve, or the run is asked to yield.
// A thread that fails stays queued behind the others.
func (r *syncRun) archiveQueue(ctx context.Context, owners OwnerFilter) error {
	queue := r.state.Pending
	var retry []Thread
	sinceSave := 0
	pending := func() []Thread { return slices.Concat(queue, retry) }
	for len(queue) > 0 {
		if ctx.Err() != nil || jobctx.PreemptionRequested(ctx) || r.summary.RateLimit != nil {
			break
		}
		thread := queue[0]
		if !owners.Allows(thread.Owner) {
			// Queued before its owner was excluded.
			queue = queue[1:]
			r.summary.Excluded++
			continue
		}
		err := r.archive(ctx, thread)
		switch {
		case err == nil:
			r.summary.Archived++
		case isRateLimit(err):
			r.noteRateLimit(err)
			continue
		case ctx.Err() != nil:
			continue
		case errors.Is(err, ErrUnavailable):
			r.summary.Unavailable++
			r.recordItem(ctx, "thread:"+thread.Name(), store.SyncRunItemStatusSkipped, "github_thread_unavailable", err)
		default:
			r.summary.Failed++
			r.checkpoint.ErrorsCount++
			retry = append(retry, thread)
			r.syncer.logger.WarnContext(ctx, "GitHub thread sync failed", "thread", thread.Name(), "error", err)
			r.recordItem(ctx, "thread:"+thread.Name(), store.SyncRunItemStatusError, "github_thread_failed", err)
		}
		queue = queue[1:]
		if sinceSave++; sinceSave >= saveEvery {
			sinceSave = 0
			r.state.Pending = pending()
			if err := r.save(ctx); err != nil {
				return err
			}
		}
	}
	r.state.Pending = pending()
	r.summary.Remaining = len(r.state.Pending)
	return r.save(ctx)
}

// archive fetches one thread and writes each of its posts. Posts the
// archive holds unchanged are not written again.
func (r *syncRun) archive(ctx context.Context, thread Thread) error {
	client := r.syncer.client
	var messages threadMessages
	switch thread.Kind {
	case KindIssue, KindPullRequest:
		fetched, err := client.fetchIssueThread(ctx, thread)
		if err != nil {
			return err
		}
		messages = issueMessages(thread, fetched)
	case KindDiscussion:
		fetched, err := client.fetchDiscussionThread(ctx, thread)
		if err != nil {
			return err
		}
		messages = discussionMessages(thread, fetched)
	case KindRelease:
		fetched, err := client.fetchRelease(ctx, thread)
		if err != nil {
			return err
		}
		messages = releaseMessages(thread, fetched)
	default:
		return fmt.Errorf("%w: unknown thread kind %q", ErrUnavailable, thread.Kind)
	}
	for _, message := range messages.Messages {
		if err := ctx.Err(); err != nil {
			return err
		}
		write, err := r.writer.Write(ctx, message)
		if errors.Is(err, messageimport.ErrValidation) {
			r.recordItem(ctx, message.ExternalID, store.SyncRunItemStatusSkipped, "github_post_invalid", err)
			continue
		}
		if err != nil {
			return fmt.Errorf("archive %s: %w", thread.Name(), err)
		}
		r.checkpoint.MessagesProcessed++
		switch {
		case write.Created:
			r.summary.Added++
			r.checkpoint.MessagesAdded++
		case write.Changed:
			r.summary.Updated++
			r.checkpoint.MessagesUpdated++
		}
	}
	return r.describeConversation(ctx, thread, messages)
}

// threadFacts is what a conversation's metadata says about its thread.
type threadFacts struct {
	Kind Kind   `json:"kind"`
	URL  string `json:"url,omitempty"`
	// Reason is why GitHub last notified the account about the thread.
	Reason string `json:"reason,omitempty"`
}

// describeConversation records the thread's kind, URL and notification
// reason in its conversation's metadata. A thread found by search keeps
// the reason its last notification gave.
func (r *syncRun) describeConversation(ctx context.Context, thread Thread, messages threadMessages) error {
	conversationID, err := r.scoped.EnsureConversationWithTypeContext(ctx, r.source.ID,
		messages.ConversationID, messageimport.ConversationType, clean(messages.Title))
	if err != nil {
		return fmt.Errorf("resolve conversation of %s: %w", thread.Name(), err)
	}
	stored, err := r.scoped.GetConversationMetadataContext(ctx, conversationID)
	if err != nil {
		return err
	}
	object := map[string]jsontext.Value{}
	if stored.Valid && strings.TrimSpace(stored.String) != "" {
		if err := json.Unmarshal([]byte(stored.String), &object); err != nil {
			return fmt.Errorf("decode conversation metadata of %s: %w", thread.Name(), err)
		}
	}
	var facts threadFacts
	if raw, ok := object[conversationMetadataKey]; ok {
		_ = json.Unmarshal(raw, &facts)
	}
	next := threadFacts{Kind: thread.Kind, URL: messages.URL, Reason: facts.Reason}
	if thread.Kind == KindIssue || thread.Kind == KindPullRequest {
		// Search and notifications may disagree; the fetched thread knows.
		next.Kind = KindIssue
		if len(messages.Messages) > 0 && messages.Messages[0].Metadata["kind"] == postPullRequest {
			next.Kind = KindPullRequest
		}
	}
	if thread.Reason != "" {
		next.Reason = thread.Reason
	}
	if next == facts {
		return nil
	}
	encoded, err := json.Marshal(next, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("encode conversation metadata of %s: %w", thread.Name(), err)
	}
	object[conversationMetadataKey] = encoded
	metadata, err := json.Marshal(object, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("encode conversation metadata of %s: %w", thread.Name(), err)
	}
	return r.scoped.SetConversationMetadataContext(ctx, conversationID, sql.NullString{String: string(metadata), Valid: true})
}

func (r *syncRun) noteRateLimit(err error) {
	var limited *RateLimitError
	if errors.As(err, &limited) && r.summary.RateLimit == nil {
		r.summary.RateLimit = limited
	}
}

// save stores the discovery cursors and the queue. It runs even when the
// sync was canceled, so the queue survives an interruption.
func (r *syncRun) save(ctx context.Context) error {
	encoded, err := r.state.encode()
	if err != nil {
		return err
	}
	if err := r.scoped.UpdateSourceSyncState(context.WithoutCancel(ctx), r.source.ID, encoded); err != nil {
		return fmt.Errorf("save GitHub sync state: %w", err)
	}
	return nil
}

// recordItem keeps a diagnostic on the sync run; a failure to record it
// only loses the diagnostic.
func (r *syncRun) recordItem(ctx context.Context, id string, status, kind string, err error) {
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
