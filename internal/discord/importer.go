package discord

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

const (
	sourceTypeDiscord = "discord"
	discordPageSize   = 100
)

// ImportOptions controls one guild import. After is an exclusive lower time
// bound; it is converted once to the exact lowest snowflake in that
// millisecond and held fixed for every container in the run.
type ImportOptions struct {
	GuildID        string
	GuildConfig    config.DiscordGuildConfig
	AttachmentsDir string
	MaxMediaBytes  int64
	After          time.Time
	Full           bool
	Progress       func(string)
}

// ImportSummary reports durable core work and best-effort media outcomes.
type ImportSummary struct {
	Duration            time.Duration
	SourceID            int64
	SyncRunID           int64
	ContainersProcessed int64
	MessagesProcessed   int64
	MessagesAdded       int64
	MessagesUpdated     int64
	MediaDownloaded     int64
	MediaPending        int64
	CatalogIssues       []CatalogIssue
}

// Importer ingests one Discord guild through the shared store and sync-run
// lifecycle.
type Importer struct {
	store    *store.Store
	api      API
	pageSize int
}

// NewImporter constructs the provider orchestration layer.
func NewImporter(st *store.Store, api API) *Importer {
	return &Importer{store: st, api: api, pageSize: discordPageSize}
}

// Import discovers a guild catalog, resumes independent container cursors,
// persists complete pages, and publishes state only after the whole guild is
// successful.
func (imp *Importer) Import(ctx context.Context, opts ImportOptions) (summary *ImportSummary, retErr error) {
	started := time.Now()
	if imp == nil || imp.store == nil {
		return nil, errors.New("discord importer store is required")
	}
	if imp.api == nil {
		return nil, errors.New("discord importer API is required")
	}
	if opts.GuildID == "" {
		return nil, errors.New("discord guild ID is required")
	}
	if imp.pageSize <= 0 || imp.pageSize > discordPageSize {
		return nil, fmt.Errorf("invalid Discord importer page size %d", imp.pageSize)
	}

	source, err := imp.store.GetOrCreateSource(sourceTypeDiscord, opts.GuildID)
	if err != nil {
		return nil, fmt.Errorf("get Discord source: %w", err)
	}
	state, hadBaseline, stateErr := imp.initialState(source.ID, opts.Full)
	if state == nil {
		state = NewSyncState()
	}

	syncID, err := imp.store.StartSync(source.ID, sourceTypeDiscord)
	if err != nil {
		return nil, fmt.Errorf("start Discord sync: %w", err)
	}
	summary = &ImportSummary{SourceID: source.ID, SyncRunID: syncID}
	completed := false
	defer func() {
		summary.Duration = time.Since(started)
		if completed || retErr == nil {
			return
		}
		checkpoint := imp.checkpoint(state, summary)
		if failErr := imp.store.FailSyncWithCheckpoint(syncID, retErr.Error(), checkpoint); failErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("fail Discord sync run: %w", failErr))
		}
	}()
	if stateErr != nil {
		return summary, stateErr
	}

	guild, err := imp.api.Guild(ctx, opts.GuildID)
	if err != nil {
		return summary, fmt.Errorf("discover Discord guild %s: %w", opts.GuildID, err)
	}
	if guild.ID != "" && guild.ID != opts.GuildID {
		return summary, fmt.Errorf("discover Discord guild %s: response identified guild %s", opts.GuildID, guild.ID)
	}
	if guild.Name != "" {
		if err := imp.store.UpdateSourceDisplayName(source.ID, guild.Name); err != nil {
			return summary, fmt.Errorf("update Discord guild name: %w", err)
		}
	}

	catalog, catalogErr := DiscoverCatalog(
		ctx, imp.api, opts.GuildID, opts.GuildConfig, state.ThreadCatalog,
		opts.Full || !hadBaseline,
	)
	state.ThreadCatalog = catalog.ThreadCatalog
	summary.CatalogIssues = append(summary.CatalogIssues, catalog.Issues...)
	if err := imp.saveCheckpoint(syncID, state, summary); err != nil {
		return summary, err
	}
	if catalogErr != nil {
		return summary, fmt.Errorf("discover Discord catalog: %w", catalogErr)
	}

	lowerBound := ""
	if !opts.After.IsZero() {
		lowerBound, err = SnowflakeFromTimestamp(opts.After)
		if err != nil {
			return summary, fmt.Errorf("convert Discord after bound: %w", err)
		}
	}
	var media *MediaArchiver
	if opts.AttachmentsDir != "" {
		media, err = NewMediaArchiver(imp.store, imp.api, opts.AttachmentsDir, opts.MaxMediaBytes)
		if err != nil {
			return summary, fmt.Errorf("configure Discord media archiver: %w", err)
		}
	}

	containers := importerContainers(catalog.Containers, state, opts.GuildID, opts.GuildConfig)
	for _, container := range containers {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		if err := imp.importContainer(
			ctx, source.ID, syncID, container, lowerBound, state, summary, media,
		); err != nil {
			return summary, fmt.Errorf("import Discord container %s: %w", container.Channel.ID, err)
		}
		summary.ContainersProcessed++
		if opts.Progress != nil {
			opts.Progress(fmt.Sprintf("Discord container %s: %d messages", container.Channel.ID, summary.MessagesProcessed))
		}
	}

	if err := imp.resolveDeferredReplies(source.ID); err != nil {
		return summary, err
	}
	if err := imp.store.RecomputeConversationStats(source.ID); err != nil {
		return summary, fmt.Errorf("recompute Discord conversation stats: %w", err)
	}
	finalState, err := state.Marshal()
	if err != nil {
		return summary, err
	}
	if err := imp.store.CompleteSync(syncID, finalState); err != nil {
		return summary, fmt.Errorf("complete Discord sync: %w", err)
	}
	completed = true
	return summary, nil
}

func (imp *Importer) initialState(sourceID int64, full bool) (*SyncState, bool, error) {
	if full {
		return NewSyncState(), false, nil
	}
	state := NewSyncState()
	hadBaseline := false
	last, err := imp.store.GetLastSuccessfulSync(sourceID)
	switch {
	case err == nil:
		hadBaseline = true
		if last.CursorAfter.Valid {
			state, err = LoadSyncState(last.CursorAfter.String)
			if err != nil {
				return nil, hadBaseline, fmt.Errorf("load last successful Discord sync state: %w", err)
			}
		}
	case errors.Is(err, store.ErrSyncRunNotFound):
	case err != nil:
		return nil, false, fmt.Errorf("load last successful Discord sync: %w", err)
	}

	checkpoint, err := imp.store.GetLatestCheckpointedSync(sourceID)
	switch {
	case err == nil:
		if checkpoint.CursorBefore.Valid {
			newer, loadErr := LoadSyncState(checkpoint.CursorBefore.String)
			if loadErr != nil {
				return nil, hadBaseline, fmt.Errorf("load latest Discord checkpoint: %w", loadErr)
			}
			if mergeErr := state.Merge(newer); mergeErr != nil {
				return nil, hadBaseline, fmt.Errorf("merge latest Discord checkpoint: %w", mergeErr)
			}
		}
	case errors.Is(err, store.ErrSyncRunNotFound):
	case err != nil:
		return nil, hadBaseline, fmt.Errorf("load latest Discord checkpoint: %w", err)
	}
	return state, hadBaseline, nil
}

type importerContainer struct {
	CatalogContainer

	preserveMetadata bool
}

func importerContainers(
	discovered []CatalogContainer,
	state *SyncState,
	guildID string,
	guildConfig config.DiscordGuildConfig,
) []importerContainer {
	containers := make([]importerContainer, 0, len(discovered)+len(state.Containers))
	seen := make(map[string]struct{}, len(containers))
	for _, container := range discovered {
		containers = append(containers, importerContainer{CatalogContainer: container})
		seen[container.Channel.ID] = struct{}{}
	}
	// A thread can disappear from the active/archived catalog while remaining
	// accessible. Preserve every previously imported, still-selected container.
	for containerID := range state.Containers {
		if _, ok := seen[containerID]; ok || slices.Contains(guildConfig.Exclude, containerID) {
			continue
		}
		containers = append(containers, importerContainer{
			CatalogContainer: CatalogContainer{Channel: Channel{ID: containerID, GuildID: guildID}},
			preserveMetadata: true,
		})
	}
	slices.SortStableFunc(containers, func(left, right importerContainer) int {
		return strings.Compare(left.Channel.ID, right.Channel.ID)
	})
	return containers
}

func (imp *Importer) importContainer(
	ctx context.Context,
	sourceID, syncID int64,
	container importerContainer,
	lowerBound string,
	state *SyncState,
	summary *ImportSummary,
	media *MediaArchiver,
) error {
	if container.Channel.ID == "" {
		return errors.New("catalog container has an empty ID")
	}
	conversationTitle := container.Channel.Name
	conversationID, err := imp.store.EnsureConversationWithType(
		sourceID, container.Channel.ID, discordConversationType, conversationTitle,
	)
	if err != nil {
		return fmt.Errorf("ensure conversation: %w", err)
	}
	if !container.preserveMetadata {
		mapped, err := mapConversation(&container.Channel)
		if err != nil {
			return err
		}
		if err := imp.store.SetConversationMetadata(conversationID, sql.NullString{
			String: string(mapped.Metadata), Valid: len(mapped.Metadata) != 0,
		}); err != nil {
			return err
		}
	}

	containerState := state.Containers[container.Channel.ID]
	if !containerState.BackfillComplete {
		if err := imp.backfill(
			ctx, sourceID, syncID, conversationID, container.Channel.ID,
			lowerBound, &containerState, state, summary, media,
		); err != nil {
			return err
		}
	}
	if err := imp.forward(
		ctx, sourceID, syncID, conversationID, container.Channel.ID,
		&containerState, state, summary, media,
	); err != nil {
		return err
	}
	state.Containers[container.Channel.ID] = containerState
	return nil
}

func (imp *Importer) backfill(
	ctx context.Context,
	sourceID, syncID, conversationID int64,
	containerID, lowerBound string,
	containerState *ContainerState,
	state *SyncState,
	summary *ImportSummary,
	media *MediaArchiver,
) error {
	before := containerState.BackfillBefore
	if containerState.BackfillUpper == "" {
		head, err := imp.api.Messages(ctx, containerID, MessageQuery{Limit: 1})
		if err != nil {
			return fmt.Errorf("pin backfill head: %w", err)
		}
		if len(head) == 0 {
			containerState.BackfillComplete = true
			containerState.HighWater = lowerBound
			if containerState.HighWater == "" {
				containerState.HighWater = "0"
			}
			state.Containers[containerID] = *containerState
			return imp.saveCheckpoint(syncID, state, summary)
		}
		if len(head) != 1 {
			return fmt.Errorf("pin backfill head: expected one message, got %d", len(head))
		}
		if err := validateDiscordMessage(containerID, head[0]); err != nil {
			return fmt.Errorf("pin backfill head: %w", err)
		}
		containerState.BackfillUpper = head[0].ID
		before, err = snowflakeSuccessor(head[0].ID)
		if err != nil {
			return fmt.Errorf("pin backfill head: %w", err)
		}
		state.Containers[containerID] = *containerState
		if err := imp.saveCheckpoint(syncID, state, summary); err != nil {
			return err
		}
	} else if before == "" {
		var err error
		before, err = snowflakeSuccessor(containerState.BackfillUpper)
		if err != nil {
			return err
		}
	}

	for {
		page, err := imp.api.Messages(ctx, containerID, MessageQuery{Before: before, Limit: imp.pageSize})
		if err != nil {
			return fmt.Errorf("page backward before %s: %w", before, err)
		}
		eligible, pageMin, pageMax, reachedBound, err := filterBackfillPage(
			containerID, page, before, containerState.BackfillUpper, lowerBound,
		)
		if err != nil {
			return err
		}
		if err := imp.persistPage(ctx, sourceID, conversationID, eligible, summary, media); err != nil {
			return err
		}
		if pageMax != "" {
			containerState.HighWater, err = maximumSnowflake(containerState.HighWater, pageMax)
			if err != nil {
				return err
			}
		}
		if reachedBound && lowerBound != "" {
			containerState.HighWater, err = maximumSnowflake(containerState.HighWater, lowerBound)
			if err != nil {
				return err
			}
		}
		if pageMin != "" {
			containerState.BackfillBefore = pageMin
		}
		containerState.BackfillComplete = reachedBound || len(page) < imp.pageSize
		state.Containers[containerID] = *containerState
		if err := imp.saveCheckpoint(syncID, state, summary); err != nil {
			return err
		}
		if containerState.BackfillComplete {
			return nil
		}
		if containerState.BackfillBefore == "" || containerState.BackfillBefore == before {
			return errors.New("discord backward pagination cursor did not advance")
		}
		before = containerState.BackfillBefore
	}
}

func (imp *Importer) forward(
	ctx context.Context,
	sourceID, syncID, conversationID int64,
	containerID string,
	containerState *ContainerState,
	state *SyncState,
	summary *ImportSummary,
	media *MediaArchiver,
) error {
	after := containerState.HighWater
	if after == "" {
		return nil
	}
	for {
		page, err := imp.api.Messages(ctx, containerID, MessageQuery{After: after, Limit: imp.pageSize})
		if err != nil {
			return fmt.Errorf("page forward after %s: %w", after, err)
		}
		pageMax := ""
		for _, message := range page {
			if err := validateDiscordMessage(containerID, message); err != nil {
				return err
			}
			isAfter, err := snowflakeAfter(message.ID, after)
			if err != nil {
				return err
			}
			if !isAfter {
				return fmt.Errorf("discord forward page returned message %s at or below cursor %s", message.ID, after)
			}
			pageMax, err = maximumSnowflake(pageMax, message.ID)
			if err != nil {
				return err
			}
		}
		if err := imp.persistPage(ctx, sourceID, conversationID, page, summary, media); err != nil {
			return err
		}
		if pageMax != "" {
			containerState.HighWater = pageMax
		}
		state.Containers[containerID] = *containerState
		if err := imp.saveCheckpoint(syncID, state, summary); err != nil {
			return err
		}
		if len(page) < imp.pageSize {
			return nil
		}
		if pageMax == "" || pageMax == after {
			return errors.New("discord forward pagination cursor did not advance")
		}
		after = pageMax
	}
}

func filterBackfillPage(
	containerID string,
	page []Message,
	before, upper, lower string,
) (eligible []Message, pageMin, pageMax string, reachedLower bool, retErr error) {
	for _, message := range page {
		if err := validateDiscordMessage(containerID, message); err != nil {
			return nil, "", "", false, err
		}
		belowCursor, err := snowflakeAfter(before, message.ID)
		if err != nil || !belowCursor {
			if err == nil {
				err = fmt.Errorf("message is not below before cursor %s", before)
			}
			return nil, "", "", false, fmt.Errorf("discord backward page message %s: %w", message.ID, err)
		}
		pageMin, err = minimumSnowflake(pageMin, message.ID)
		if err != nil {
			return nil, "", "", false, err
		}
		atOrBelowLower := false
		if lower != "" {
			afterLower, compareErr := snowflakeAfter(message.ID, lower)
			if compareErr != nil {
				return nil, "", "", false, compareErr
			}
			atOrBelowLower = !afterLower
		}
		if atOrBelowLower {
			reachedLower = true
			continue
		}
		atOrBelowUpper, err := snowflakeAtOrBefore(message.ID, upper)
		if err != nil {
			return nil, "", "", false, err
		}
		if !atOrBelowUpper {
			return nil, "", "", false, fmt.Errorf("discord backward page returned message %s above pinned head %s", message.ID, upper)
		}
		eligible = append(eligible, message)
		pageMax, err = maximumSnowflake(pageMax, message.ID)
		if err != nil {
			return nil, "", "", false, err
		}
	}
	return eligible, pageMin, pageMax, reachedLower, nil
}

func (imp *Importer) persistPage(
	ctx context.Context,
	sourceID, conversationID int64,
	page []Message,
	summary *ImportSummary,
	media *MediaArchiver,
) error {
	ids := make([]string, 0, len(page))
	for _, message := range page {
		ids = append(ids, message.ID)
	}
	existing, err := imp.store.MessageExistsBatch(sourceID, ids)
	if err != nil {
		return fmt.Errorf("load existing Discord messages: %w", err)
	}

	for i := range page {
		mapped, err := mapMessage(&page[i], conversationID, sourceID)
		if err != nil {
			return err
		}
		recipients, participantIDs, senderID, fromLabel, mentionLabels, err := imp.resolveRecipients(mapped.Recipients)
		if err != nil {
			return err
		}
		if senderID != 0 {
			mapped.Message.SenderID = sql.NullInt64{Int64: senderID, Valid: true}
		}
		metadata := sql.NullString{String: string(mapped.Metadata), Valid: len(mapped.Metadata) != 0}
		messageID, err := imp.store.PersistMessage(&store.MessagePersistData{
			Message:        &mapped.Message,
			Metadata:       &metadata,
			BodyText:       sql.NullString{String: mapped.BodyText, Valid: mapped.BodyText != ""},
			RawMIME:        mapped.Raw,
			RawFormat:      mapped.RawFormat,
			Recipients:     recipients,
			PreserveLabels: true,
			FTS: &store.FTSDoc{
				Body: mapped.BodyText, FromAddr: fromLabel, ToAddrs: strings.Join(mentionLabels, " "),
			},
		})
		if err != nil {
			return fmt.Errorf("persist Discord message %s: %w", page[i].ID, err)
		}
		if mapped.Edited {
			if err := imp.store.SetMessageEdited(messageID); err != nil {
				return fmt.Errorf("mark Discord message %s edited: %w", page[i].ID, err)
			}
		}
		for _, participantID := range participantIDs {
			if err := imp.store.EnsureConversationParticipant(conversationID, participantID, "member"); err != nil {
				return fmt.Errorf("persist Discord conversation participant: %w", err)
			}
		}
		if media != nil {
			result, err := media.PersistAttachments(ctx, messageID, page[i].Attachments)
			if err != nil {
				return fmt.Errorf("persist Discord message %s media metadata: %w", page[i].ID, err)
			}
			for _, item := range result.Items {
				switch item.Outcome {
				case MediaDownloaded:
					summary.MediaDownloaded++
				case MediaPending, MediaUnrecoverable:
					summary.MediaPending++
				}
			}
		} else {
			if err := imp.store.ReplaceMessageDiscordAttachments(messageID, mapped.Attachments); err != nil {
				return fmt.Errorf("persist Discord message %s attachment metadata: %w", page[i].ID, err)
			}
			if err := imp.store.RecomputeMessageAttachmentStats(messageID); err != nil {
				return fmt.Errorf("recompute Discord message %s attachment metadata: %w", page[i].ID, err)
			}
			summary.MediaPending += int64(len(mapped.Attachments))
		}

		if reference := page[i].MessageReference; reference != nil && reference.MessageID != "" {
			if err := imp.store.SetReplyTo(sourceID, page[i].ID, reference.MessageID); err != nil {
				return fmt.Errorf("link Discord reply %s: %w", page[i].ID, err)
			}
		}
		summary.MessagesProcessed++
		if _, ok := existing[page[i].ID]; ok {
			summary.MessagesUpdated++
		} else {
			summary.MessagesAdded++
		}
	}
	return nil
}

func (imp *Importer) resolveRecipients(
	observations []recipientObservation,
) ([]store.RecipientSet, []int64, int64, string, []string, error) {
	sets := map[string]*store.RecipientSet{
		"from":    {Type: "from"},
		"mention": {Type: "mention"},
	}
	seenParticipants := map[int64]struct{}{}
	var participants []int64
	var senderID int64
	var fromLabel string
	var mentionLabels []string
	for _, recipient := range observations {
		observation := recipient.Participant
		participantID, err := imp.store.EnsureParticipantByIdentifier(
			observation.IdentifierType,
			observation.IdentifierValue,
			observation.ParticipantLabel,
		)
		if err != nil {
			return nil, nil, 0, "", nil, fmt.Errorf("resolve Discord participant: %w", err)
		}
		set := sets[recipient.Type]
		if set == nil {
			set = &store.RecipientSet{Type: recipient.Type}
			sets[recipient.Type] = set
		}
		set.ParticipantIDs = append(set.ParticipantIDs, participantID)
		label := observation.PresentationDisplayName
		if label == "" {
			label = observation.ParticipantLabel
		}
		set.DisplayNames = append(set.DisplayNames, label)
		if _, ok := seenParticipants[participantID]; !ok {
			seenParticipants[participantID] = struct{}{}
			participants = append(participants, participantID)
		}
		switch recipient.Type {
		case "from":
			senderID = participantID
			fromLabel = label
		case "mention":
			mentionLabels = append(mentionLabels, label)
		}
	}
	return []store.RecipientSet{*sets["from"], *sets["mention"]}, participants, senderID, fromLabel, mentionLabels, nil
}

func (imp *Importer) resolveDeferredReplies(sourceID int64) error {
	unresolved, err := imp.store.ListUnresolvedMessageReplies(sourceID, discordMessageType)
	if err != nil {
		return err
	}
	for _, reply := range unresolved {
		var metadata struct {
			ReferencedMessageID string `json:"referenced_message_id"`
		}
		if err := json.Unmarshal([]byte(reply.Metadata), &metadata); err != nil {
			return fmt.Errorf("decode Discord reply metadata for %s: %w", reply.SourceMessageID, err)
		}
		if metadata.ReferencedMessageID == "" {
			continue
		}
		if err := imp.store.SetReplyTo(sourceID, reply.SourceMessageID, metadata.ReferencedMessageID); err != nil {
			return fmt.Errorf("resolve deferred Discord reply %s: %w", reply.SourceMessageID, err)
		}
	}
	return nil
}

func (imp *Importer) saveCheckpoint(
	syncID int64,
	state *SyncState,
	summary *ImportSummary,
) error {
	checkpoint := imp.checkpoint(state, summary)
	if checkpoint == nil {
		return errors.New("marshal Discord checkpoint")
	}
	if err := imp.store.UpdateSyncCheckpoint(syncID, checkpoint); err != nil {
		return fmt.Errorf("save Discord checkpoint: %w", err)
	}
	return nil
}

func (imp *Importer) checkpoint(state *SyncState, summary *ImportSummary) *store.Checkpoint {
	if state == nil || summary == nil {
		return nil
	}
	blob, err := state.Marshal()
	if err != nil {
		return nil
	}
	return &store.Checkpoint{
		PageToken:         blob,
		MessagesProcessed: summary.MessagesProcessed,
		MessagesAdded:     summary.MessagesAdded,
		MessagesUpdated:   summary.MessagesUpdated,
	}
}

func validateDiscordMessage(containerID string, message Message) error {
	if message.ID == "" {
		return errors.New("discord message has an empty ID")
	}
	if _, err := ParseSnowflake(message.ID); err != nil {
		return err
	}
	if message.ChannelID != "" && message.ChannelID != containerID {
		return fmt.Errorf("discord message %s belongs to channel %s, expected %s", message.ID, message.ChannelID, containerID)
	}
	return nil
}

func snowflakeSuccessor(value string) (string, error) {
	parsed, err := ParseSnowflake(value)
	if err != nil {
		return "", err
	}
	if parsed == math.MaxUint64 {
		return "", errors.New("discord snowflake has no successor")
	}
	return strconv.FormatUint(parsed+1, 10), nil
}

func snowflakeAtOrBefore(candidate, upper string) (bool, error) {
	after, err := snowflakeAfter(candidate, upper)
	return !after, err
}

func maximumSnowflake(left, right string) (string, error) {
	if left == "" {
		if _, err := ParseSnowflake(right); err != nil {
			return "", err
		}
		return right, nil
	}
	after, err := snowflakeAfter(right, left)
	if err != nil {
		return "", err
	}
	if after {
		return right, nil
	}
	return left, nil
}

func minimumSnowflake(left, right string) (string, error) {
	if left == "" {
		if _, err := ParseSnowflake(right); err != nil {
			return "", err
		}
		return right, nil
	}
	after, err := snowflakeAfter(left, right)
	if err != nil {
		return "", err
	}
	if after {
		return right, nil
	}
	return left, nil
}
