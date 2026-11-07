package youtube

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/url"
	"slices"

	"go.kenn.io/msgvault/internal/store"
)

type ImportOptions struct {
	ChannelID   string
	MaxRequests int
}

var ErrResourceUnavailable = errors.New("YouTube content unavailable; will retry")
var errRepeatedPageToken = errors.New("YouTube repeated a results page")

type ImportSummary struct {
	SourceID          int64
	MessagesProcessed int
	Partial           bool
	Requests          int
}

type messageCounts struct {
	processed int
	added     int
	updated   int
}

func (counts *messageCounts) record(change messageChange, persistErr error) {
	if persistErr == nil || change == messageAdded || change == messageUpdated {
		counts.processed++
	}
	switch change {
	case messageUnchanged:
		// Unchanged observations still count as processed, but add/update no rows.
	case messageAdded:
		counts.added++
	case messageUpdated:
		counts.updated++
	}
}

func (counts *messageCounts) add(other messageCounts) {
	counts.processed += other.processed
	counts.added += other.added
	counts.updated += other.updated
}

type Importer struct {
	store  *store.Store
	client *Client
}

func NewImporter(st *store.Store, c *Client) *Importer { return &Importer{st, c} }

type work struct {
	Kind     string   `json:"kind"`
	Video    Video    `json:"video,omitzero"`
	Parent   string   `json:"parent,omitempty"`
	ThreadID string   `json:"thread_id,omitempty"`
	Token    string   `json:"token,omitempty"`
	Pass     string   `json:"pass,omitempty"`
	Reset    bool     `json:"reset,omitempty"`
	Seen     []string `json:"seen,omitempty"`
}
type syncState struct {
	Version  int    `json:"version"`
	Queue    []work `json:"queue"`
	Deferred []work `json:"deferred,omitempty"`
}

func (imp *Importer) Register(ctx context.Context, channelID string) (*store.Source, error) {
	_, name, err := imp.client.ownedChannel(ctx, channelID)
	if err != nil {
		return nil, err
	}
	src, err := imp.store.GetOrCreateSource(SourceType, SourceIdentifier(channelID))
	if err != nil {
		return nil, err
	}
	if err = imp.store.UpdateSourceDisplayNameContext(ctx, src.ID, name); err != nil {
		if !errors.Is(err, store.ErrSourceSettingsInvalid) {
			return nil, err
		}
	} else {
		src.DisplayName.String = name
		src.DisplayName.Valid = name != ""
	}
	_, err = imp.store.EnsureParticipantByIdentifierContext(ctx, SourceType, SourceIdentifier(channelID), name)
	if err != nil {
		return nil, err
	}
	if err = imp.store.AddAccountIdentityContext(ctx, src.ID, SourceIdentifier(channelID), "youtube_channel"); err != nil {
		return nil, err
	}
	return src, nil
}

func (imp *Importer) resume(ctx context.Context, sourceID int64) (*syncState, error) {
	state := &syncState{Version: 1, Queue: []work{{Kind: "uploads"}}}
	prior, err := imp.store.GetLastSuccessfulSyncByType(ctx, sourceID, SourceType)
	if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
		return nil, err
	}
	checkpoint, err := imp.store.GetLatestCheckpointedSyncByType(ctx, sourceID, SourceType)
	if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
		return nil, err
	}
	blob := ""
	if checkpoint != nil && checkpoint.CursorBefore.Valid && (prior == nil || checkpoint.ID > prior.ID) {
		blob = checkpoint.CursorBefore.String
	}
	if blob != "" {
		if json.Unmarshal([]byte(blob), state) != nil || state.Version != 1 {
			return nil, errors.New("invalid saved sync progress")
		}
	}
	// A failed resource gets one turn per coverage cycle, not per invocation.
	// Otherwise budgets smaller than the retry backlog rotate failures forever
	// without ever letting the current cycle finish and revisit discovery.
	if len(state.Queue) == 0 {
		state.Queue = appendUniqueWork([]work{{Kind: "uploads"}}, nil, state.Deferred...)
		state.Deferred = nil
	}
	return state, nil
}

type workIdentity struct {
	kind, videoID, parent, pass string
}

func identity(job work) workIdentity {
	return workIdentity{job.Kind, job.Video.ID, job.Parent, job.Pass}
}

// Preserve the already pending job, including its page token and cycle
// history, when another discovery pass finds the same resource.
func appendUniqueWork(queue, blocked []work, jobs ...work) []work {
	known := make(map[workIdentity]bool, len(queue)+len(blocked)+len(jobs))
	for _, job := range queue {
		known[identity(job)] = true
	}
	for _, job := range blocked {
		known[identity(job)] = true
	}
	for _, job := range jobs {
		key := identity(job)
		if !known[key] {
			queue = append(queue, job)
			known[key] = true
		}
	}
	return queue
}

func (imp *Importer) Import(ctx context.Context, opts ImportOptions) (sum *ImportSummary, resultErr error) {
	budget := opts.MaxRequests
	if budget == 0 {
		budget = 1000
	}
	if budget < 2 || budget > 100000 {
		return nil, errors.New("YouTube max-requests must be between 2 and 100000 (channel verification needs one request)")
	}
	src, err := imp.store.GetSourceByTypeAndIdentifier(ctx, SourceType, SourceIdentifier(opts.ChannelID))
	if err != nil {
		return nil, err
	}
	// StartSync holds the Store's cross-process source execution lock.
	runID, err := imp.store.StartSyncContext(ctx, src.ID, SourceType)
	if err != nil {
		return nil, err
	}
	state, err := imp.resume(ctx, src.ID)
	if err != nil {
		_ = imp.store.FailSync(runID, "Invalid saved progress.")
		return nil, err
	}
	sum = &ImportSummary{SourceID: src.ID, Partial: true}
	var counts messageCounts
	checkpoint := func() (*store.Checkpoint, error) {
		b, err := json.Marshal(state)
		return &store.Checkpoint{
			PageToken: string(b), MessagesProcessed: int64(sum.MessagesProcessed),
			MessagesAdded: int64(counts.added), MessagesUpdated: int64(counts.updated),
		}, err
	}
	defer func() {
		cp, err := checkpoint()
		if err != nil {
			resultErr = errors.Join(resultErr, err)
		}
		if resultErr != nil {
			resultErr = errors.Join(resultErr, imp.store.InterruptSyncWithCheckpoint(ctx, runID, resultErr, cp))
			return
		}
		if sum.Partial {
			resultErr = imp.store.PauseSyncWithCheckpoint(ctx, runID, cp)
			return
		}
		resultErr = imp.store.CompleteSyncAndUpdateSourceCursorContext(ctx, runID, src.ID, cp.PageToken)
	}()
	uploads, _, err := imp.client.ownedChannel(ctx, opts.ChannelID)
	sum.Requests++
	if err != nil {
		return sum, err
	}
	scoped := imp.store.ScopedToSync(src.ID, runID)
	cycleDetected := false
	for len(state.Queue) > 0 && sum.Requests < budget {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		job := state.Queue[0]
		next, stepCounts, err := imp.step(ctx, scoped, src.ID, opts.ChannelID, uploads, job)
		sum.Requests++
		counts.add(stepCounts)
		sum.MessagesProcessed += stepCounts.processed
		if err != nil {
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.Reason == "invalidPageToken" && job.Token != "" && !job.Reset {
				job.Token = ""
				job.Reset = true
				job.Seen = nil
				state.Queue = appendUniqueWork(state.Queue[1:], state.Deferred, job)
			} else if errors.Is(err, errRepeatedPageToken) || errors.As(err, &apiErr) && (apiErr.Reason == "videoNotFound" || apiErr.Reason == "commentNotFound" || apiErr.Reason == "invalidPageToken") {
				cycleDetected = cycleDetected || errors.Is(err, errRepeatedPageToken)
				state.Deferred = appendUniqueWork(state.Deferred, nil, job)
				state.Queue = state.Queue[1:]
			} else if errors.As(err, &apiErr) && apiErr.Reason == "commentsDisabled" {
				state.Queue = state.Queue[1:]
			} else {
				return sum, err
			}
		} else {
			state.Queue = appendUniqueWork(state.Queue[1:], state.Deferred, next...)
		}
		cp, err := checkpoint()
		if err != nil {
			return sum, err
		}
		if err = imp.store.UpdateSyncCheckpointContext(ctx, runID, cp); err != nil {
			return sum, err
		}
	}
	sum.Partial = len(state.Queue) > 0 || len(state.Deferred) > 0
	if len(state.Deferred) > 0 {
		if cycleDetected {
			return sum, errors.Join(ErrResourceUnavailable, errRepeatedPageToken)
		}
		return sum, ErrResourceUnavailable
	}
	return sum, nil
}

func paginated(q url.Values, token string) url.Values {
	q.Set("maxResults", "100")
	if token != "" {
		q.Set("pageToken", token)
	}
	return q
}
func nextPage(job work, token string) ([]work, error) {
	if token == "" {
		return nil, nil
	}
	if token == job.Token || slices.Contains(job.Seen, token) {
		return nil, errRepeatedPageToken
	}
	job.Seen = slices.Clone(job.Seen)
	// Include the current token for checkpoints created before token history
	// was recorded, as well as every newly emitted token.
	if job.Token != "" && !slices.Contains(job.Seen, job.Token) {
		job.Seen = append(job.Seen, job.Token)
	}
	job.Seen = append(job.Seen, token)
	job.Token = token
	job.Reset = false // A successful page permits recovery of a later expired token.
	return []work{job}, nil
}

func (imp *Importer) step(ctx context.Context, st *store.Store, sourceID int64, channelID, uploads string, job work) (nextWork []work, counts messageCounts, stepErr error) {
	var lastStatsMessageID int64
	defer func() {
		if lastStatsMessageID == 0 {
			return
		}
		statsCtx := ctx
		if ctx.Err() != nil {
			statsCtx = context.WithoutCancel(ctx)
		}
		stepErr = errors.Join(stepErr, st.RecomputeConversationStatsForMessageContext(statsCtx, lastStatsMessageID))
	}()

	switch job.Kind {
	case "uploads":
		var result page[struct {
			ContentDetails struct {
				VideoID string `json:"videoId"`
			} `json:"contentDetails"`
		}]
		query := paginated(url.Values{"playlistId": {uploads}, "part": {"contentDetails"}}, job.Token)
		query.Set("maxResults", "50")
		err := imp.client.get(ctx, "/playlistItems", query, &result)
		if err != nil {
			return nil, messageCounts{}, err
		}
		tail, err := nextPage(job, result.NextPageToken)
		if err != nil {
			return nil, messageCounts{}, err
		}
		next := []work{}
		for _, item := range result.Items {
			if !validID(item.ContentDetails.VideoID) {
				return nil, messageCounts{}, errors.New("invalid YouTube upload video ID")
			}
			next = append(next, work{Kind: "video", Video: Video{ID: item.ContentDetails.VideoID}})
		}
		return append(next, tail...), counts, nil
	case "video":
		var result page[Video]
		err := imp.client.get(ctx, "/videos", url.Values{"id": {job.Video.ID}, "part": {"snippet,status"}}, &result)
		if err != nil {
			return nil, messageCounts{}, err
		}
		if len(result.Items) != 1 || result.Items[0].ID != job.Video.ID {
			return nil, messageCounts{}, &APIError{StatusCode: 404, Reason: "videoNotFound"}
		}
		v := result.Items[0]
		videoMessageID, change, persistErr := persistVideo(ctx, st, sourceID, channelID, v)
		counts.record(change, persistErr)
		if videoMessageID != 0 {
			lastStatsMessageID = videoMessageID
		}
		if persistErr != nil {
			return nil, counts, persistErr
		}
		next := []work{}
		for _, pass := range []string{"published", "heldForReview", "likelySpam"} {
			next = append(next, work{Kind: "threads", Video: Video{ID: v.ID}, Pass: pass})
		}
		return next, counts, nil
	case "threads":
		var result page[thread]
		err := imp.client.get(ctx, "/commentThreads", paginated(url.Values{"videoId": {job.Video.ID}, "part": {"snippet,replies"}, "textFormat": {"plainText"}, "order": {"time"}, "moderationStatus": {job.Pass}}, job.Token), &result)
		if err != nil {
			return nil, messageCounts{}, err
		}
		next, err := nextPage(job, result.NextPageToken)
		if err != nil {
			return nil, messageCounts{}, err
		}
		for _, th := range result.Items {
			c := th.Snippet.TopLevelComment
			c.ThreadID = th.ID
			c.CanReply = th.Snippet.CanReply
			if c.Snippet.ParentID != "" || th.Snippet.VideoID != "" && th.Snippet.VideoID != job.Video.ID {
				return nil, counts, errors.New("YouTube thread identity mismatch")
			}
			commentID, change, persistErr := persistComment(ctx, st, sourceID, channelID, job.Video, c)
			counts.record(change, persistErr)
			if commentID != 0 {
				lastStatsMessageID = commentID
			}
			if persistErr != nil {
				return nil, counts, persistErr
			}
			for _, reply := range th.Replies.Comments {
				if reply.Snippet.ParentID != c.ID {
					return nil, counts, errors.New("YouTube inline reply parent mismatch")
				}
				reply.ThreadID = th.ID
				replyID, change, persistErr := persistComment(ctx, st, sourceID, channelID, job.Video, reply)
				counts.record(change, persistErr)
				if replyID != 0 {
					lastStatsMessageID = replyID
				}
				if persistErr != nil {
					return nil, counts, persistErr
				}
			}
			if th.Snippet.TotalReplyCount > len(th.Replies.Comments) {
				candidate := work{Kind: "replies", Video: Video{ID: job.Video.ID}, Parent: c.ID, ThreadID: th.ID}
				if !slices.ContainsFunc(next, func(w work) bool { return w.Kind == "replies" && w.Parent == c.ID }) {
					next = append(next, candidate)
				}
			}
		}
		return next, counts, nil
	case "replies":
		var result page[Comment]
		err := imp.client.get(ctx, "/comments", paginated(url.Values{"parentId": {job.Parent}, "part": {"snippet"}, "textFormat": {"plainText"}}, job.Token), &result)
		if err != nil {
			return nil, messageCounts{}, err
		}
		next, err := nextPage(job, result.NextPageToken)
		if err != nil {
			return nil, messageCounts{}, err
		}
		for _, c := range result.Items {
			if c.Snippet.ParentID != job.Parent {
				return nil, counts, errors.New("YouTube reply parent mismatch")
			}
			c.ThreadID = job.ThreadID
			commentID, change, persistErr := persistComment(ctx, st, sourceID, channelID, job.Video, c)
			counts.record(change, persistErr)
			if commentID != 0 {
				lastStatsMessageID = commentID
			}
			if persistErr != nil {
				return nil, counts, persistErr
			}
		}
		return next, counts, nil
	default:
		return nil, messageCounts{}, errors.New("cannot resume saved YouTube sync progress")
	}
}
