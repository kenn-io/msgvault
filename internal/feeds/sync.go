package feeds

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
	syncType = "feed"
	// maxStoredErrorChars bounds the fetch error a feed keeps for list-feeds.
	maxStoredErrorChars = 500
)

// FeedResult is what one sync did to one feed.
type FeedResult struct {
	URL         string
	Title       string
	NotModified bool
	Items       int
	Added       int
	Skipped     int
	Err         error
}

// Summary is what one sync did.
type Summary struct {
	SourceID int64
	Feeds    []FeedResult
}

// Added counts the items archived for the first time.
func (s Summary) Added() int {
	total := 0
	for _, feed := range s.Feeds {
		total += feed.Added
	}
	return total
}

// ErrEveryFeedFailed means no feed in a sync could be fetched or archived:
// the feed source is failing, not one feed.
var ErrEveryFeedFailed = errors.New("every feed failed")

// SourceErr is the sync's failure as a source: an error when every feed it
// tried failed, so a scheduled run records the outage, and nil when at
// least one feed worked. A single failing feed is recorded on the run and
// on the feed instead.
func (s Summary) SourceErr() error {
	if n := len(s.Feeds); n > 0 && s.Failed() == n {
		return fmt.Errorf("%w (%d feeds)", ErrEveryFeedFailed, n)
	}
	return nil
}

// Failed counts the feeds that could not be fetched or archived.
func (s Summary) Failed() int {
	total := 0
	for _, feed := range s.Feeds {
		if feed.Err != nil {
			total++
		}
	}
	return total
}

// Syncer fetches feeds and archives their new items.
type Syncer struct {
	store   *store.Store
	fetcher *Fetcher
	logger  *slog.Logger
	now     func() time.Time
}

func NewSyncer(st *store.Store, fetcher *Fetcher, logger *slog.Logger) *Syncer {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Syncer{store: st, fetcher: fetcher, logger: logger, now: time.Now}
}

// SyncOptions selects the feeds one sync fetches.
type SyncOptions struct {
	// URLs limits the sync to these followed feeds; empty means every followed feed.
	URLs []string
	// Full ignores stored HTTP validators, so every feed is downloaded again.
	Full bool
}

// Sync fetches followed feeds and archives each item the archive does not
// hold yet. A feed that fails is recorded on the sync run and on the feed;
// the other feeds still sync.
func (s *Syncer) Sync(ctx context.Context, opts SyncOptions) (Summary, error) {
	registry := NewRegistry(s.store)
	feeds, err := registry.List(ctx)
	if err != nil {
		return Summary{}, err
	}
	selected, err := selectFeeds(feeds, opts.URLs)
	if err != nil {
		return Summary{}, err
	}
	return s.run(ctx, registry, true, func(ctx context.Context, run *syncRun) {
		for _, feed := range selected {
			if ctx.Err() != nil || jobctx.PreemptionRequested(ctx) {
				return
			}
			validators := Validators{ETag: feed.State.ETag, LastModified: feed.State.LastModified}
			if opts.Full {
				validators = Validators{}
			}
			run.feed(ctx, feed.URL, feed.State.Title, nil, validators)
		}
	})
}

// Follow fetches each URL and, when it is a feed, follows it and archives its
// items. A URL that is not a reachable feed is reported and not followed.
func (s *Syncer) Follow(ctx context.Context, urls []string) (Summary, error) {
	normalized := make([]string, 0, len(urls))
	for _, raw := range urls {
		feedURL, err := NormalizeURL(raw)
		if err != nil {
			return Summary{}, fmt.Errorf("%q: %w", raw, err)
		}
		normalized = append(normalized, feedURL)
	}
	registry := NewRegistry(s.store)
	return s.run(ctx, registry, false, func(ctx context.Context, run *syncRun) {
		for _, feedURL := range normalized {
			if ctx.Err() != nil {
				return
			}
			fetched, err := s.fetcher.Fetch(ctx, feedURL, Validators{})
			if err != nil {
				run.report(ctx, FeedResult{URL: feedURL, Err: err})
				continue
			}
			if _, _, err := registry.Add(ctx, feedURL, feedTitle(feedURL, fetched.Feed)); err != nil {
				run.report(ctx, FeedResult{URL: feedURL, Err: err})
				continue
			}
			run.feed(ctx, feedURL, "", &fetched, Validators{})
		}
	})
}

// selectFeeds returns the feeds a sync fetches. Without urls that is every
// followed feed, least recently checked first, so a sync that yields to
// other work resumes with the feeds it didn't reach.
func selectFeeds(feeds []Feed, urls []string) ([]Feed, error) {
	if len(urls) == 0 {
		followed := make([]Feed, 0, len(feeds))
		for _, feed := range feeds {
			if feed.Followed() {
				followed = append(followed, feed)
			}
		}
		slices.SortStableFunc(followed, func(a, b Feed) int {
			return compareCheckedAt(a.State.CheckedAt, b.State.CheckedAt)
		})
		return followed, nil
	}
	byURL := make(map[string]Feed, len(feeds))
	for _, feed := range feeds {
		byURL[feed.URL] = feed
	}
	selected := make([]Feed, 0, len(urls))
	for _, raw := range urls {
		feedURL, err := NormalizeURL(raw)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", raw, err)
		}
		feed, ok := byURL[feedURL]
		if !ok || !feed.Followed() {
			return nil, fmt.Errorf("%w: %s; add it with msgvault add-feed", ErrNotFollowed, feedURL)
		}
		selected = append(selected, feed)
	}
	return selected, nil
}

// compareCheckedAt orders a never-checked feed first, then earlier checks.
func compareCheckedAt(a, b *time.Time) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	default:
		return a.Compare(*b)
	}
}

// syncRun is one sync run of the feed source.
type syncRun struct {
	syncer     *Syncer
	source     *store.Source
	scoped     *store.Store
	syncID     int64
	writer     *messageimport.Writer
	checkpoint *store.Checkpoint
	summary    Summary
}

// run records one sync run around body. With outages set, a run in which
// every feed failed ends as failed, so the source's last good sync doesn't
// advance during an outage; adding feeds that fail is no outage. An
// interrupted run ends through InterruptSyncWithCheckpoint, which records a
// handoff to other scheduled work as cancelled.
func (s *Syncer) run(
	ctx context.Context, registry *Registry, outages bool, body func(context.Context, *syncRun),
) (summary Summary, retErr error) {
	source, err := registry.Source(ctx)
	if err != nil {
		return Summary{}, err
	}
	syncID, err := s.store.StartSyncContext(ctx, source.ID, syncType)
	if err != nil {
		return Summary{SourceID: source.ID}, fmt.Errorf("start feed sync: %w", err)
	}
	scoped := s.store.ScopedToSync(source.ID, syncID)
	run := &syncRun{
		syncer:     s,
		source:     source,
		scoped:     scoped,
		syncID:     syncID,
		writer:     messageimport.NewWriter(scoped, source),
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
			retErr = fmt.Errorf("checkpoint feed sync: %w", err)
			retErr = errors.Join(retErr, scoped.FailSyncWithCheckpointContext(cleanup, syncID, retErr.Error(), run.checkpoint))
			return
		}
		// Feeds keep their own validators, so the source has no cursor to
		// publish; completing through the source records when it last synced.
		if err := scoped.CompleteSyncAndPreserveSourceCursorContext(cleanup, syncID, source.ID, ""); err != nil {
			retErr = fmt.Errorf("complete feed sync: %w", err)
		}
	}()
	body(ctx, run)
	if outages && ctx.Err() == nil {
		return run.summary, run.summary.SourceErr()
	}
	return run.summary, nil
}

// feed fetches one feed, unless fetched is given, and archives its new items.
// knownTitle names the feed in the result until a download names it again.
func (r *syncRun) feed(ctx context.Context, feedURL, knownTitle string, fetched *Fetched, validators Validators) {
	result := FeedResult{URL: feedURL, Title: knownTitle}
	defer func() { r.report(ctx, result) }()

	if fetched == nil {
		got, err := r.syncer.fetcher.Fetch(ctx, feedURL, validators)
		if err != nil {
			result.Err = errors.Join(err, r.recordFailure(ctx, feedURL, "", err))
			return
		}
		fetched = &got
	}
	if fetched.NotModified {
		result.NotModified = true
		result.Err = r.recordCheck(ctx, feedURL, "", &fetched.Validators, nil)
		return
	}
	result.Title = feedTitle(feedURL, fetched.Feed)
	added, skipped, items, err := r.archive(ctx, feedURL, fetched)
	result.Added, result.Skipped, result.Items = added, skipped, items
	if err != nil {
		// Validators stay as they were so the next sync downloads the feed again.
		result.Err = errors.Join(err, r.recordFailure(ctx, feedURL, result.Title, err))
		return
	}
	result.Err = r.recordCheck(ctx, feedURL, result.Title, &fetched.Validators, nil)
}

// report adds a feed's result to the summary. A failed feed counts as an
// error of the run, so the source shows the sync as failing, and is recorded
// as a sync-run item with its error.
func (r *syncRun) report(ctx context.Context, result FeedResult) {
	if result.Err != nil && ctx.Err() != nil {
		// Interrupted, not checked: neither a result nor a failure.
		return
	}
	r.summary.Feeds = append(r.summary.Feeds, result)
	if result.Err == nil {
		return
	}
	r.checkpoint.ErrorsCount++
	r.syncer.logger.WarnContext(ctx, "feed sync failed", "feed", result.URL, "error", result.Err)
	// The run continues; a recording failure only loses this diagnostic.
	_ = r.scoped.RecordSyncRunItemContext(ctx, store.SyncRunItem{
		SyncRunID: r.syncID, SourceMessageID: "feed:" + result.URL, Phase: "fetch",
		Status: store.SyncRunItemStatusError, ErrorKind: "feed_unavailable",
		ErrorMessage: truncate(result.Err.Error(), maxStoredErrorChars),
	})
}

// archive writes the feed's items that the archive does not hold yet.
func (r *syncRun) archive(ctx context.Context, feedURL string, fetched *Fetched) (added, skipped, items int, err error) {
	feed := fetched.Feed
	firstSeen := r.syncer.now().UTC()
	messages := make([]messageimport.ImportedMessage, 0, len(feed.Items))
	seen := make(map[string]bool, len(feed.Items))
	keys := make([]string, 0, len(feed.Items))
	for _, item := range feed.Items {
		if item == nil {
			continue
		}
		message := itemMessage(feedURL, fetched.URL, feed, item, firstSeen)
		if seen[message.ExternalID] {
			continue
		}
		seen[message.ExternalID] = true
		messages = append(messages, message)
		keys = append(keys, message.ExternalID)
	}
	existing, err := r.scoped.MessageExistsBatchContext(ctx, r.source.ID, keys)
	if err != nil {
		return 0, 0, len(messages), fmt.Errorf("look up archived items: %w", err)
	}
	for _, message := range messages {
		if ctx.Err() != nil {
			return added, skipped, len(messages), ctx.Err()
		}
		r.checkpoint.MessagesProcessed++
		if _, ok := existing[message.ExternalID]; ok {
			continue
		}
		write, err := r.writer.Write(ctx, message)
		if errors.Is(err, messageimport.ErrValidation) {
			skipped++
			_ = r.scoped.RecordSyncRunItemContext(ctx, store.SyncRunItem{
				SyncRunID: r.syncID, SourceMessageID: message.ExternalID, Phase: "ingest",
				Status: store.SyncRunItemStatusSkipped, ErrorKind: "feed_item_invalid",
				ErrorMessage: truncate(err.Error(), maxStoredErrorChars),
			})
			continue
		}
		if err != nil {
			return added, skipped, len(messages), fmt.Errorf("archive %q: %w", itemName(message), err)
		}
		if write.Created {
			added++
			r.checkpoint.MessagesAdded++
		}
	}
	return added, skipped, len(messages), nil
}

// itemName names an item in an error: its title, else its link, else its
// key in the archive.
func itemName(message messageimport.ImportedMessage) string {
	switch {
	case message.Title != "":
		return message.Title
	case message.URL != "":
		return message.URL
	default:
		return message.ExternalID
	}
}

// recordFailure stores a feed's failure, unless the run was interrupted:
// an interruption says nothing about the feed.
func (r *syncRun) recordFailure(ctx context.Context, feedURL, title string, failure error) error {
	if ctx.Err() == nil {
		return r.recordCheck(ctx, feedURL, title, nil, failure)
	}
	return nil
}

// recordCheck stores when a feed was checked and how it went. New validators
// replace the stored ones only after the feed's items were archived.
func (r *syncRun) recordCheck(ctx context.Context, feedURL, title string, validators *Validators, failure error) error {
	now := r.syncer.now().UTC()
	_, err := updateState(context.WithoutCancel(ctx), r.scoped, r.source, feedURL, title, func(state *State) {
		state.CheckedAt = &now
		state.LastError = ""
		if failure != nil {
			state.LastError = truncate(failure.Error(), maxStoredErrorChars)
		}
		if title != "" {
			state.Title = title
		}
		if validators != nil {
			state.ETag = validators.ETag
			state.LastModified = validators.LastModified
		}
	})
	if err != nil {
		return fmt.Errorf("record feed check: %w", err)
	}
	return nil
}
