package discord

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/config"
)

const (
	channelTypeGuildText         = 0
	channelTypeGuildAnnouncement = 5
	channelTypeGuildForum        = 15
	channelTypeGuildMedia        = 16
)

// ErrMalformedCatalog classifies internally inconsistent successful catalog
// responses without requiring callers to inspect diagnostic strings.
var ErrMalformedCatalog = errors.New("discord catalog response is malformed")

// CatalogScope identifies the Discord catalog operation that produced an
// issue. Archive scopes are independent so callers can report and checkpoint
// public and private discovery separately.
type CatalogScope string

const (
	CatalogScopeGuildChannels  CatalogScope = "guild_channels"
	CatalogScopeActiveThreads  CatalogScope = "active_threads"
	CatalogScopePublicArchive  CatalogScope = "public_archive"
	CatalogScopePrivateArchive CatalogScope = "private_archive"
)

// CatalogIssueKind classifies catalog failures without requiring callers to
// parse error strings.
type CatalogIssueKind string

const (
	CatalogIssueForbidden      CatalogIssueKind = "forbidden"
	CatalogIssueUnknownChannel CatalogIssueKind = "unknown_channel"
	CatalogIssueCanceled       CatalogIssueKind = "canceled"
	CatalogIssueDecode         CatalogIssueKind = "decode"
	CatalogIssueMalformedPage  CatalogIssueKind = "malformed_page"
	CatalogIssueAPI            CatalogIssueKind = "api"
)

// CatalogIssue preserves the operation and Discord classification needed by
// importer failure accounting. Err is retained for wrapping and errors.Is/
// errors.As checks; its formatting is already sanitized by the REST client.
type CatalogIssue struct {
	Scope       CatalogScope
	Kind        CatalogIssueKind
	GuildID     string
	ParentID    string
	StatusCode  int
	DiscordCode int
	Fatal       bool
	Err         error
}

// CatalogContainer is one independently imported Discord message container.
// Parent is populated for threads and forum posts when the guild catalog
// included their parent channel.
type CatalogContainer struct {
	Channel Channel
	Parent  *Channel
}

// CatalogResult contains every container found before or during a catalog
// failure, a caller-owned copy of the next safe per-parent archive state, and
// structured issues for later sync-run accounting.
type CatalogResult struct {
	Containers    []CatalogContainer
	ThreadCatalog map[string]ThreadCatalogState
	Issues        []CatalogIssue
}

// DiscoverCatalog refreshes top-level message containers, active threads, and
// public/private archived threads. full exhausts archive pagination; an
// incremental scan stops only after processing a complete page that reaches
// the prior watermark.
func DiscoverCatalog(
	ctx context.Context,
	api API,
	guildID string,
	guildConfig config.DiscordGuildConfig,
	prior map[string]ThreadCatalogState,
	full bool,
) (CatalogResult, error) {
	result := CatalogResult{ThreadCatalog: cloneThreadCatalog(prior)}
	if api == nil {
		return result, errors.New("discover Discord catalog: nil API")
	}

	channels, err := api.GuildChannels(ctx, guildID)
	if err != nil {
		issue := newCatalogIssue(CatalogScopeGuildChannels, guildID, "", err)
		result.Issues = append(result.Issues, issue)
		return result, fmt.Errorf("discover Discord guild channels: %w", err)
	}

	parents := make(map[string]Channel)
	containerIndexes := make(map[string]int)
	threadParents := make(map[string]string)
	for _, channel := range channels {
		parents[channel.ID] = channel
		if !isThreadCatalogParent(channel.Type) {
			continue
		}
		if isTopLevelMessageContainer(channel.Type) && ContainerIncluded(guildConfig, channel.ID, "") {
			addCatalogContainer(&result, containerIndexes, channel, nil)
		}
	}

	var fatalErrors []error
	active, activeErr := api.ActiveThreads(ctx, guildID)
	if activeErr != nil {
		issue := newCatalogIssue(CatalogScopeActiveThreads, guildID, "", activeErr)
		result.Issues = append(result.Issues, issue)
		fatalErrors = append(fatalErrors, fmt.Errorf("discover Discord active threads: %w", activeErr))
		if errors.Is(activeErr, context.Canceled) || errors.Is(activeErr, context.DeadlineExceeded) {
			return result, errors.Join(fatalErrors...)
		}
	} else {
		for _, thread := range active {
			if thread.ParentID == "" {
				malformedErr := fmt.Errorf("%w: active thread %s has no parent ID", ErrMalformedCatalog, thread.ID)
				result.Issues = append(result.Issues, newCatalogIssue(CatalogScopeActiveThreads, guildID, "", malformedErr))
				fatalErrors = append(fatalErrors, malformedErr)
				continue
			}
			if _, ok := parents[thread.ParentID]; !ok {
				malformedErr := fmt.Errorf("%w: active thread %s has unknown parent %s", ErrMalformedCatalog, thread.ID, thread.ParentID)
				result.Issues = append(result.Issues, newCatalogIssue(CatalogScopeActiveThreads, guildID, thread.ParentID, malformedErr))
				fatalErrors = append(fatalErrors, malformedErr)
				continue
			}
			if err := recordCatalogThreadParent(threadParents, thread.ID, thread.ParentID); err != nil {
				result.Issues = append(result.Issues, newCatalogIssue(CatalogScopeActiveThreads, guildID, thread.ParentID, err))
				fatalErrors = append(fatalErrors, err)
				continue
			}
			if !ContainerIncluded(guildConfig, thread.ID, thread.ParentID) {
				continue
			}
			addCatalogContainer(&result, containerIndexes, thread, catalogParent(parents, thread.ParentID))
		}
	}

	for _, parent := range orderedCatalogParents(channels) {
		if _, ok := result.ThreadCatalog[parent.ID]; !ok {
			result.ThreadCatalog[parent.ID] = ThreadCatalogState{}
		}
		archiveKinds := []bool{false}
		if parent.Type == channelTypeGuildText {
			archiveKinds = append(archiveKinds, true)
		}
		for _, private := range archiveKinds {
			scope := CatalogScopePublicArchive
			if private {
				scope = CatalogScopePrivateArchive
			}
			state := result.ThreadCatalog[parent.ID]
			priorWatermark := state.PublicArchiveWatermark
			if private {
				priorWatermark = state.PrivateArchiveWatermark
			}

			newWatermark, scanErr := discoverArchive(
				ctx, api, parent, private, priorWatermark, full,
				func(thread Channel) error {
					if err := recordCatalogThreadParent(threadParents, thread.ID, thread.ParentID); err != nil {
						return err
					}
					if ContainerIncluded(guildConfig, thread.ID, thread.ParentID) {
						addCatalogContainer(&result, containerIndexes, thread, catalogParent(parents, parent.ID))
					}
					return nil
				},
			)
			if scanErr != nil {
				issue := newCatalogIssue(scope, guildID, parent.ID, scanErr)
				if issue.Kind == CatalogIssueForbidden || issue.Kind == CatalogIssueUnknownChannel {
					issue.Fatal = false
				}
				result.Issues = append(result.Issues, issue)
				if issue.Fatal {
					fatalErrors = append(fatalErrors, fmt.Errorf("discover Discord %s for parent %s: %w", scope, parent.ID, scanErr))
					if issue.Kind == CatalogIssueCanceled {
						return result, errors.Join(fatalErrors...)
					}
				}
				continue
			}

			if private {
				state.PrivateArchiveWatermark = newWatermark
			} else {
				state.PublicArchiveWatermark = newWatermark
			}
			result.ThreadCatalog[parent.ID] = state
		}
	}

	return result, errors.Join(fatalErrors...)
}

func discoverArchive(
	ctx context.Context,
	api API,
	parent Channel,
	private bool,
	priorWatermark string,
	full bool,
	emit func(Channel) error,
) (string, error) {
	priorTime, err := parseCatalogWatermark(priorWatermark)
	if err != nil {
		return priorWatermark, fmt.Errorf("invalid prior archive watermark: %w", err)
	}
	candidate := priorTime
	before := time.Time{}

	for {
		page, pageErr := api.ArchivedThreads(ctx, parent.ID, private, before)
		if pageErr != nil {
			return priorWatermark, pageErr
		}

		reachedBoundary := false
		for _, thread := range page.Threads {
			if thread.ThreadMetadata == nil || thread.ThreadMetadata.ArchiveTimestamp.IsZero() {
				return priorWatermark, fmt.Errorf("%w: archived thread %s is missing an archive timestamp", ErrMalformedCatalog, thread.ID)
			}
			if thread.ParentID == "" {
				thread.ParentID = parent.ID
			} else if thread.ParentID != parent.ID {
				return priorWatermark, fmt.Errorf("%w: archived thread %s has parent %s, want endpoint parent %s", ErrMalformedCatalog, thread.ID, thread.ParentID, parent.ID)
			}
			archiveTime := thread.ThreadMetadata.ArchiveTimestamp
			if archiveTime.After(candidate) {
				candidate = archiveTime
			}
			if !full && !priorTime.IsZero() && !archiveTime.After(priorTime) {
				reachedBoundary = true
			}
			if err := emit(thread); err != nil {
				return priorWatermark, err
			}
		}

		if !page.HasMore || reachedBoundary {
			return formatCatalogWatermark(candidate, priorWatermark), nil
		}
		if page.NextBefore.IsZero() || (!before.IsZero() && !page.NextBefore.Before(before)) {
			return priorWatermark, fmt.Errorf("%w: archived thread pagination cursor did not advance", ErrMalformedCatalog)
		}
		before = page.NextBefore
	}
}

func recordCatalogThreadParent(parents map[string]string, threadID, parentID string) error {
	if previous, ok := parents[threadID]; ok && previous != parentID {
		return fmt.Errorf(
			"%w: thread %s appears under conflicting parents %s and %s",
			ErrMalformedCatalog, threadID, previous, parentID,
		)
	}
	parents[threadID] = parentID
	return nil
}

func cloneThreadCatalog(prior map[string]ThreadCatalogState) map[string]ThreadCatalogState {
	cloned := make(map[string]ThreadCatalogState, len(prior))
	maps.Copy(cloned, prior)
	return cloned
}

func parseCatalogWatermark(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse archive watermark: %w", err)
	}
	return parsed, nil
}

func formatCatalogWatermark(value time.Time, prior string) string {
	if value.IsZero() {
		return prior
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func isTopLevelMessageContainer(channelType int) bool {
	return channelType == channelTypeGuildText || channelType == channelTypeGuildAnnouncement
}

func isThreadCatalogParent(channelType int) bool {
	return isTopLevelMessageContainer(channelType) || channelType == channelTypeGuildForum || channelType == channelTypeGuildMedia
}

func orderedCatalogParents(channels []Channel) []Channel {
	parents := make([]Channel, 0, len(channels))
	for _, channel := range channels {
		if isThreadCatalogParent(channel.Type) {
			parents = append(parents, channel)
		}
	}
	slices.SortStableFunc(parents, func(left, right Channel) int {
		return strings.Compare(left.ID, right.ID)
	})
	return parents
}

func catalogParent(parents map[string]Channel, parentID string) *Channel {
	parent, ok := parents[parentID]
	if !ok {
		return nil
	}
	parentCopy := parent
	return &parentCopy
}

func addCatalogContainer(result *CatalogResult, indexes map[string]int, channel Channel, parent *Channel) {
	if index, ok := indexes[channel.ID]; ok {
		result.Containers[index].Channel = mergeCatalogChannel(result.Containers[index].Channel, channel)
		if parent != nil {
			result.Containers[index].Parent = parent
		}
		return
	}
	indexes[channel.ID] = len(result.Containers)
	result.Containers = append(result.Containers, CatalogContainer{Channel: channel, Parent: parent})
}

func mergeCatalogChannel(existing, incoming Channel) Channel {
	merged := existing
	if incoming.Type != 0 {
		merged.Type = incoming.Type
	}
	if incoming.GuildID != "" {
		merged.GuildID = incoming.GuildID
	}
	if incoming.Position != 0 {
		merged.Position = incoming.Position
	}
	if incoming.Name != "" {
		merged.Name = incoming.Name
	}
	if incoming.Topic != "" {
		merged.Topic = incoming.Topic
	}
	merged.NSFW = merged.NSFW || incoming.NSFW
	if incoming.LastMessageID != "" {
		merged.LastMessageID = incoming.LastMessageID
	}
	if incoming.ParentID != "" {
		merged.ParentID = incoming.ParentID
	}
	if incoming.OwnerID != "" {
		merged.OwnerID = incoming.OwnerID
	}
	if incoming.RateLimitPerUser != 0 {
		merged.RateLimitPerUser = incoming.RateLimitPerUser
	}
	if incoming.MessageCount != 0 {
		merged.MessageCount = incoming.MessageCount
	}
	if incoming.MemberCount != 0 {
		merged.MemberCount = incoming.MemberCount
	}
	if incoming.Flags != 0 {
		merged.Flags = incoming.Flags
	}
	if len(incoming.AppliedTags) != 0 {
		merged.AppliedTags = incoming.AppliedTags
	}
	if len(incoming.AvailableTags) != 0 {
		merged.AvailableTags = incoming.AvailableTags
	}
	if incoming.DefaultReactionEmoji != nil {
		merged.DefaultReactionEmoji = incoming.DefaultReactionEmoji
	}
	if incoming.ThreadMetadata != nil {
		merged.ThreadMetadata = mergeThreadMetadata(merged.ThreadMetadata, incoming.ThreadMetadata)
	}
	return merged
}

func mergeThreadMetadata(existing, incoming *ThreadMetadata) *ThreadMetadata {
	if existing == nil {
		incomingCopy := *incoming
		return &incomingCopy
	}
	merged := *existing
	merged.Archived = merged.Archived || incoming.Archived
	if incoming.AutoArchiveDuration != 0 {
		merged.AutoArchiveDuration = incoming.AutoArchiveDuration
	}
	if incoming.ArchiveTimestamp.After(merged.ArchiveTimestamp) {
		merged.ArchiveTimestamp = incoming.ArchiveTimestamp
	}
	merged.Locked = merged.Locked || incoming.Locked
	merged.Invitable = merged.Invitable || incoming.Invitable
	if incoming.CreateTimestamp != nil {
		merged.CreateTimestamp = incoming.CreateTimestamp
	}
	return &merged
}

func newCatalogIssue(scope CatalogScope, guildID, parentID string, err error) CatalogIssue {
	issue := CatalogIssue{
		Scope:    scope,
		Kind:     CatalogIssueAPI,
		GuildID:  guildID,
		ParentID: parentID,
		Fatal:    true,
		Err:      err,
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		issue.Kind = CatalogIssueCanceled
		return issue
	}
	if errors.Is(err, ErrDecodeResponse) {
		issue.Kind = CatalogIssueDecode
		return issue
	}
	if errors.Is(err, ErrMalformedCatalog) {
		issue.Kind = CatalogIssueMalformedPage
		return issue
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		issue.StatusCode = apiErr.StatusCode
		issue.DiscordCode = apiErr.Code
		switch apiErr.StatusCode {
		case http.StatusForbidden:
			issue.Kind = CatalogIssueForbidden
		case http.StatusNotFound:
			issue.Kind = CatalogIssueUnknownChannel
		default:
			issue.Kind = CatalogIssueAPI
		}
		return issue
	}
	return issue
}
