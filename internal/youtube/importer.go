package youtube

import (
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"go.kenn.io/msgvault/internal/store"
	yt "google.golang.org/api/youtube/v3"
)

type ImportOptions struct {
	ChannelID   string
	MaxRequests int
}

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
	VideoID  string   `json:"video_id,omitempty"`
	VideoIDs []string `json:"video_ids,omitempty"`
	Parent   string   `json:"parent,omitempty"`
	Token    string   `json:"token,omitempty"`
	Pass     string   `json:"pass,omitempty"`
}
type syncState struct {
	Version         int    `json:"version"`
	OwnershipCursor string `json:"ownership_cursor,omitempty"`
	Queue           []work `json:"queue"`
}

func (imp *Importer) Register(ctx context.Context, channelID string) (*store.Source, error) {
	owner, err := imp.client.ownedChannel(ctx, channelID, "", 0)
	if err != nil {
		return nil, err
	}
	src, err := imp.store.GetOrCreateSource(SourceType, SourceIdentifier(channelID))
	if err != nil {
		return nil, err
	}
	if err = imp.store.UpdateSourceDisplayNameContext(ctx, src.ID, owner.name); err != nil {
		if !errors.Is(err, store.ErrSourceSettingsInvalid) {
			return nil, err
		}
	} else {
		src.DisplayName.String = owner.name
		src.DisplayName.Valid = owner.name != ""
	}
	_, err = imp.store.EnsureParticipantByIdentifierContext(ctx, SourceType, SourceIdentifier(channelID), owner.name)
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
	checkpoint, err := imp.store.GetLatestCheckpointedSyncByType(ctx, sourceID, SourceType)
	if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
		return nil, err
	}
	blob := ""
	if checkpoint != nil && checkpoint.CursorBefore.Valid {
		blob = checkpoint.CursorBefore.String
	}
	if blob != "" {
		var header struct {
			Version int `json:"version"`
		}
		if json.Unmarshal([]byte(blob), &header) == nil && header.Version > 1 {
			return nil, errors.New("unsupported YouTube saved progress version")
		}
		if err := json.Unmarshal([]byte(blob), state); err != nil || state.Version != 1 {
			slog.WarnContext(ctx, "YouTube saved progress is invalid; restarting discovery", "source_id", sourceID)
			state = &syncState{Version: 1, Queue: []work{{Kind: "uploads"}}}
		}
	}
	return state, nil
}

type workIdentity struct {
	kind, videoID, videoIDs, parent, pass string
}

func identity(job work) workIdentity {
	return workIdentity{job.Kind, job.VideoID, strings.Join(job.VideoIDs, ","), job.Parent, job.Pass}
}

// Preserve the already pending job and page token when discovery repeats it.
func appendUniqueWork(queue []work, jobs ...work) []work {
	known := make(map[workIdentity]bool, len(queue)+len(jobs))
	for _, job := range queue {
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
		resultErr = imp.store.CompleteSyncAndPreserveSourceCursorContext(ctx, runID, src.ID, cp.PageToken)
	}()
	owner, err := imp.client.ownedChannel(ctx, opts.ChannelID, state.OwnershipCursor, budget)
	sum.Requests += owner.requests
	state.OwnershipCursor = owner.cursor
	if err != nil {
		var apiErr *APIError
		if owner.cursor == "" || errors.As(err, &apiErr) && apiErr.Reason == "invalidPageToken" {
			state.OwnershipCursor = ""
		}
		return sum, err
	}
	if owner.uploads == "" || sum.Requests == budget {
		return sum, nil
	}
	state.OwnershipCursor = ""
	scoped := imp.store.ScopedToSync(src.ID, runID)
	for len(state.Queue) > 0 && sum.Requests < budget {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		job := state.Queue[0]
		next, stepCounts, err := imp.step(ctx, scoped, src.ID, opts.ChannelID, owner.uploads, job)
		sum.Requests++
		counts.add(stepCounts)
		sum.MessagesProcessed += stepCounts.processed
		if err != nil {
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				return sum, err
			}
			switch {
			case apiErr.Reason == "invalidPageToken":
				state.Queue = state.Queue[1:]
				if job.Token != "" {
					job.Token = ""
					state.Queue = appendUniqueWork(state.Queue, job)
				}
			case job.Kind == "videos" && (apiErr.Reason == "videoNotFound" || apiErr.Reason == "forbidden" && apiErr.StatusCode == http.StatusForbidden):
				individual := make([]work, 0, len(job.VideoIDs))
				for _, id := range job.VideoIDs {
					individual = append(individual, work{Kind: "video", VideoID: id})
				}
				state.Queue = appendUniqueWork(state.Queue[1:], individual...)
			case (job.Kind == "video" || job.Kind == "threads" || job.Kind == "replies") && (apiErr.Reason == "videoNotFound" || apiErr.Reason == "commentNotFound" || apiErr.Reason == "commentsDisabled" || apiErr.Reason == "forbidden" && apiErr.StatusCode == http.StatusForbidden):
				state.Queue = state.Queue[1:]
			default:
				return sum, err
			}
		} else {
			state.Queue = appendUniqueWork(state.Queue[1:], next...)
		}
		cp, err := checkpoint()
		if err != nil {
			return sum, err
		}
		if err = imp.store.UpdateSyncCheckpointContext(ctx, runID, cp); err != nil {
			return sum, err
		}
	}
	sum.Partial = len(state.Queue) > 0

	return sum, nil
}

func nextPage(job work, token string) []work {
	if token == "" {
		return nil
	}
	job.Token = token
	return []work{job}
}

func (imp *Importer) step(ctx context.Context, st *store.Store, sourceID int64, channelID, uploads string, job work) (nextWork []work, counts messageCounts, stepErr error) {
	var statsMessageIDs []int64
	defer func() {
		if len(statsMessageIDs) == 0 {
			return
		}
		statsCtx := ctx
		if ctx.Err() != nil {
			statsCtx = context.WithoutCancel(ctx)
		}
		for _, id := range statsMessageIDs {
			stepErr = errors.Join(stepErr, st.RecomputeConversationStatsForMessageContext(statsCtx, id))
		}
	}()

	switch job.Kind {
	case "uploads":
		result, err := imp.client.service.PlaylistItems.List([]string{"contentDetails"}).PlaylistId(uploads).MaxResults(50).PageToken(job.Token).Context(ctx).Do()
		if err != nil {
			return nil, counts, safeError(ctx, err)
		}
		tail := nextPage(job, result.NextPageToken)
		var ids []string
		for _, item := range result.Items {
			if item == nil || item.ContentDetails == nil || !validID(item.ContentDetails.VideoId) {
				return nil, counts, errors.New("invalid YouTube upload video ID")
			}
			ids = append(ids, item.ContentDetails.VideoId)
		}
		if len(ids) > 0 {
			return append([]work{{Kind: "videos", VideoIDs: ids}}, tail...), counts, nil
		}
		return tail, counts, nil
	case "video", "videos":
		ids := job.VideoIDs
		if job.Kind == "video" {
			ids = []string{job.VideoID}
		}
		result, err := imp.client.service.Videos.List([]string{"snippet", "status"}).Id(strings.Join(ids, ",")).Context(ctx).Do()
		if err != nil {
			return nil, counts, safeError(ctx, err)
		}
		if job.Kind == "video" && len(result.Items) == 0 {
			return nil, counts, &APIError{404, "videoNotFound"}
		}
		wanted := make(map[string]bool, len(ids))
		for _, id := range ids {
			wanted[id] = true
		}
		next := []work{}
		for _, v := range result.Items {
			if v == nil || !wanted[v.Id] || v.Snippet == nil || v.Snippet.ChannelId != channelID {
				return nil, counts, errors.New("invalid YouTube video response")
			}
			delete(wanted, v.Id)
			id, change, persistErr := persistVideoSDK(ctx, st, sourceID, channelID, v)
			counts.record(change, persistErr)
			if id != 0 {
				statsMessageIDs = append(statsMessageIDs, id)
			}
			if persistErr != nil {
				return nil, counts, persistErr
			}
			for _, pass := range []string{"published", "heldForReview", "likelySpam"} {
				next = append(next, work{Kind: "threads", VideoID: v.Id, Pass: pass})
			}
		}
		return next, counts, nil
	case "threads":
		result, err := imp.client.service.CommentThreads.List([]string{"snippet", "replies"}).VideoId(job.VideoID).MaxResults(100).PageToken(job.Token).TextFormat("plainText").Order("time").ModerationStatus(job.Pass).Context(ctx).Do()
		if err != nil {
			return nil, counts, safeError(ctx, err)
		}
		next := nextPage(job, result.NextPageToken)
		for _, th := range result.Items {
			if th == nil || th.Snippet == nil || th.Snippet.TopLevelComment == nil || th.Snippet.TopLevelComment.Snippet == nil {
				return nil, counts, errors.New("invalid YouTube thread")
			}
			c := th.Snippet.TopLevelComment
			if c.Snippet.ParentId != "" || th.Snippet.VideoId != "" && th.Snippet.VideoId != job.VideoID {
				return nil, counts, errors.New("YouTube thread identity mismatch")
			}
			id, change, persistErr := persistCommentSDK(ctx, st, sourceID, channelID, job.VideoID, c)
			counts.record(change, persistErr)
			if id != 0 {
				statsMessageIDs = []int64{id}
			}
			if persistErr != nil {
				return nil, counts, persistErr
			}
			var replies []*yt.Comment
			if th.Replies != nil {
				replies = th.Replies.Comments
			}
			for _, reply := range replies {
				if reply == nil || reply.Snippet == nil || reply.Snippet.ParentId != c.Id {
					return nil, counts, errors.New("YouTube inline reply parent mismatch")
				}
				id, change, persistErr := persistCommentSDK(ctx, st, sourceID, channelID, job.VideoID, reply)
				counts.record(change, persistErr)
				if id != 0 {
					statsMessageIDs = []int64{id}
				}
				if persistErr != nil {
					return nil, counts, persistErr
				}
			}
			if th.Snippet.TotalReplyCount > int64(len(replies)) {
				next = appendUniqueWork(next, work{Kind: "replies", VideoID: job.VideoID, Parent: c.Id})
			}
		}
		return next, counts, nil
	case "replies":
		result, err := imp.client.service.Comments.List([]string{"snippet"}).ParentId(job.Parent).MaxResults(100).PageToken(job.Token).TextFormat("plainText").Context(ctx).Do()
		if err != nil {
			return nil, counts, safeError(ctx, err)
		}
		next := nextPage(job, result.NextPageToken)
		for _, c := range result.Items {
			if c == nil || c.Snippet == nil || c.Snippet.ParentId != job.Parent {
				return nil, counts, errors.New("YouTube reply parent mismatch")
			}
			id, change, persistErr := persistCommentSDK(ctx, st, sourceID, channelID, job.VideoID, c)
			counts.record(change, persistErr)
			if id != 0 {
				statsMessageIDs = []int64{id}
			}
			if persistErr != nil {
				return nil, counts, persistErr
			}
		}
		return next, counts, nil
	default:
		return nil, counts, errors.New("cannot resume saved YouTube sync progress")
	}
}
