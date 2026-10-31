package x

import (
	"errors"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

var (
	ErrRepeatedPageToken = errors.New("X pagination token repeated")
	ErrUnresolvedPage    = errors.New("X page has unidentified unresolved results")
)

func prepareTimelineWalk(owner, endpoint string, previous *store.XWalk, after, now time.Time) (*store.XWalk, error) {
	if ValidateID(owner) != nil || (endpoint != "timeline" && endpoint != "mentions") || now.IsZero() {
		return nil, errors.New("invalid X timeline walk")
	}
	after = after.UTC()
	if previous != nil {
		if err := validateTimelineWalk(previous, owner, endpoint); err != nil {
			return nil, err
		}
		if previous.After.Equal(after) && !previous.Complete {
			resumed := cloneWalk(previous)
			resumed.HistoryGapPossible = resumed.HistoryGapPossible || now.Sub(previous.Before) > 24*time.Hour
			return resumed, nil
		}
	}
	before := now.UTC().Truncate(time.Second)
	if !after.IsZero() && !after.Before(before) {
		return nil, errors.New("X lower bound must precede the frozen upper bound")
	}
	result := &store.XWalk{Endpoint: endpoint, Query: owner, After: after, Before: before}
	if previous != nil {
		result.Watermark = previous.Watermark
		result.HistoryGapPossible = previous.HistoryGapPossible || now.Sub(previous.Before) > 24*time.Hour
		if previous.After.Equal(after) && previous.Complete {
			result.BaseWatermark = previous.Watermark
		}
	}
	result.Fingerprint = walkFingerprint(result)
	return result, nil
}

func advanceWalk(walk *store.XWalk, page Page) (*store.XWalk, error) {
	if walk == nil || walk.Complete || len(walk.SeenTokens) >= 1000 {
		return nil, errors.New("X walk is complete or has reached page capacity")
	}
	if page.NextToken != "" && (page.NextToken == walk.Token || slices.Contains(walk.SeenTokens, page.NextToken)) {
		return nil, ErrRepeatedPageToken
	}
	result := cloneWalk(walk)
	if err := observeWalkPosts(result, page.Posts); err != nil {
		return nil, err
	}
	result.UnresolvedPage = false
	for _, item := range page.Errors {
		if item.ID == "" {
			result.UnresolvedPage = true
			continue
		}
		if ValidateID(item.ID) != nil {
			return nil, errors.New("invalid X unresolved item ID")
		}
		if !slices.Contains(result.UnresolvedIDs, item.ID) {
			if len(result.UnresolvedIDs) >= store.XMaxUnresolvedIDs {
				return nil, errors.New("X unresolved item capacity reached")
			}
			result.UnresolvedIDs = append(result.UnresolvedIDs, item.ID)
		}
	}
	if result.UnresolvedPage {
		// Its identity is not known: preserve the request token and replay this
		// exact page rather than advancing past the unidentified missing result.
		return result, nil
	}
	result.ObservedResults += int64(len(page.Posts) + len(page.Errors))
	reach := int64(0)
	switch walk.Endpoint {
	case "timeline":
		reach = 3200
	case "mentions":
		reach = 800
	}
	if reach > 0 && result.ObservedResults >= reach {
		result.HistoryGapPossible = true
	}
	// SeenTokens records committed REQUEST tokens, including the initial empty
	// token. An empty next token plus a nonempty history therefore represents
	// exhaustion even while identified item obligations are still outstanding.
	result.SeenTokens = append(result.SeenTokens, walk.Token)
	result.Token = page.NextToken
	finishWalk(result)
	return result, nil
}

func timelineWalkRequest(walk *store.XWalk, maxResults int) TimelineRequest {
	after := walk.After
	if walk.BaseWatermark != "" {
		after = time.Time{}
	}
	return TimelineRequest{OwnerID: walk.Query, Mentions: walk.Endpoint == "mentions", After: after, Before: walk.Before, SinceID: walk.BaseWatermark, Token: walk.Token, MaxResults: maxResults}
}

func postInWalk(post Post, walk *store.XWalk) bool {
	return (walk.After.IsZero() || post.CreatedAt.After(walk.After)) && post.CreatedAt.Before(walk.Before)
}

func replayWalk(walk *store.XWalk) *store.XWalk {
	result := cloneWalk(walk)
	result.Token = ""
	result.SeenTokens = nil
	result.ObservedResults = 0
	result.Complete = false
	return result
}

func resolveWalkItems(walk *store.XWalk, ids []string, posts ...Post) (*store.XWalk, error) {
	if walk.UnresolvedPage {
		return nil, ErrUnresolvedPage
	}
	for _, id := range ids {
		if ValidateID(id) != nil || !slices.Contains(walk.UnresolvedIDs, id) {
			return nil, errors.New("X item is not an outstanding obligation")
		}
	}
	result := cloneWalk(walk)
	if err := observeWalkPosts(result, posts); err != nil {
		return nil, err
	}
	result.UnresolvedIDs = slices.DeleteFunc(result.UnresolvedIDs, func(id string) bool { return slices.Contains(ids, id) })
	finishWalk(result)
	return result, nil
}

func prepareOverlapWalk(owner string, previous *store.XWalk, now time.Time) (*store.XWalk, error) {
	if previous != nil {
		if previous.Endpoint != "timeline" && previous.Endpoint != "mentions" {
			return nil, errors.New("invalid X overlap endpoint")
		}
		if err := validateTimelineWalk(previous, owner, previous.Endpoint); err != nil {
			return nil, err
		}
		if !previous.Complete || previous.Endpoint == "timeline" {
			return cloneWalk(previous), nil
		}
	}
	before := now.UTC().Truncate(time.Second)
	return prepareTimelineWalk(owner, "timeline", nil, before.Add(-time.Hour), before)
}

func nextOverlapEndpoint(walk *store.XWalk) (*store.XWalk, error) {
	if walk == nil || !walk.Complete || walk.Endpoint != "timeline" {
		return nil, errors.New("X overlap own-post walk is not complete")
	}
	if err := validateTimelineWalk(walk, walk.Query, "timeline"); err != nil {
		return nil, err
	}
	result := &store.XWalk{Endpoint: "mentions", Query: walk.Query, After: walk.After, Before: walk.Before}
	result.Fingerprint = walkFingerprint(result)
	return result, nil
}

func observeWalkPosts(walk *store.XWalk, posts []Post) error {
	for _, post := range posts {
		if ValidateID(post.ID) != nil || post.CreatedAt.IsZero() {
			return errors.New("invalid X page progress")
		}
		if !postInWalk(post, walk) {
			continue
		}
		if walk.LowestCommittedID == "" || CompareID(post.ID, walk.LowestCommittedID) < 0 {
			walk.LowestCommittedID = post.ID
		}
		if walk.MaxObservedID == "" || CompareID(post.ID, walk.MaxObservedID) > 0 {
			walk.MaxObservedID = post.ID
		}
	}
	return nil
}

func walkExhausted(walk *store.XWalk) bool {
	return walk != nil && walk.Token == "" && len(walk.SeenTokens) > 0 && !walk.UnresolvedPage
}

func finishWalk(walk *store.XWalk) {
	if !walkExhausted(walk) || len(walk.UnresolvedIDs) != 0 {
		return
	}
	if walk.MaxObservedID != "" && (walk.Watermark == "" || CompareID(walk.MaxObservedID, walk.Watermark) > 0) {
		walk.Watermark = walk.MaxObservedID
	}
	walk.Complete = true
}

func walkFingerprint(walk *store.XWalk) string {
	return store.XWalkFingerprint(walk)
}

func validateTimelineWalk(walk *store.XWalk, owner, endpoint string) error {
	if walk.Query != owner || walk.Endpoint != endpoint || walk.Before.IsZero() || walk.Fingerprint != walkFingerprint(walk) {
		return errors.New("X saved walk does not match its frozen request")
	}
	if !walk.After.IsZero() && !walk.After.Before(walk.Before) {
		return errors.New("invalid X saved walk bounds")
	}
	for _, id := range []string{walk.BaseWatermark, walk.Watermark, walk.LowestCommittedID, walk.MaxObservedID} {
		if id != "" && ValidateID(id) != nil {
			return errors.New("invalid X saved progress ID")
		}
	}
	if walk.Complete && (walk.Token != "" || walk.UnresolvedPage || len(walk.UnresolvedIDs) != 0) {
		return errors.New("X complete walk has outstanding work")
	}
	return nil
}

func cloneWalk(walk *store.XWalk) *store.XWalk {
	result := *walk
	result.SeenTokens = slices.Clone(walk.SeenTokens)
	result.UnresolvedIDs = slices.Clone(walk.UnresolvedIDs)
	return &result
}
