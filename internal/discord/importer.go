package discord

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
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
	GuildID          string
	GuildConfig      config.DiscordGuildConfig
	AttachmentsDir   string
	MaxMediaBytes    int64
	EditRescanWindow time.Duration
	After            time.Time
	Full             bool
	Progress         func(string)
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
	ContainerIssues     []ContainerIssue
	processedMessageIDs map[string]struct{}
}

// ContainerIssueKind classifies a message-container scan that was skipped
// without failing the guild import. Values are provider-stable and do not
// retain upstream response text.
type ContainerIssueKind string

const (
	ContainerIssueForbidden      ContainerIssueKind = "forbidden"
	ContainerIssueUnknownChannel ContainerIssueKind = "unknown_channel"
)

// ContainerIssue is the sanitized access diagnostic for one skipped channel,
// thread, or forum post. It intentionally contains no raw error or URL.
type ContainerIssue struct {
	ContainerID string
	Kind        ContainerIssueKind
	StatusCode  int
	DiscordCode int
}

// Importer ingests one Discord guild through the shared store and sync-run
// lifecycle.
type Importer struct {
	store    *store.Store
	api      API
	pageSize int
	now      func() time.Time
}

// NewImporter constructs the provider orchestration layer.
func NewImporter(st *store.Store, api API) *Importer {
	return &Importer{store: st, api: api, pageSize: discordPageSize, now: time.Now}
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
	lowerBound := ""
	if !opts.After.IsZero() {
		lowerBound, err = SnowflakeFromTimestamp(opts.After)
		if err != nil {
			return nil, fmt.Errorf("convert Discord after bound: %w", err)
		}
	}
	state, hadBaseline, stateErr := imp.initialState(source.ID, opts.Full, lowerBound)
	if stateErr != nil {
		return nil, stateErr
	}
	if state == nil {
		state = NewSyncState()
		state.Full = opts.Full
		state.LowerBound = lowerBound
	}

	syncID, err := imp.store.StartSync(source.ID, sourceTypeDiscord)
	if err != nil {
		return nil, fmt.Errorf("start Discord sync: %w", err)
	}
	summary = &ImportSummary{
		SourceID: source.ID, SyncRunID: syncID,
		processedMessageIDs: make(map[string]struct{}),
	}
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
	summary.CatalogIssues = append(summary.CatalogIssues, catalog.Issues...)
	if catalogErr != nil {
		return summary, fmt.Errorf("discover Discord catalog: %w", catalogErr)
	}

	var media *MediaArchiver
	if opts.AttachmentsDir != "" {
		media, err = NewMediaArchiver(imp.store, imp.api, opts.AttachmentsDir, opts.MaxMediaBytes)
		if err != nil {
			return summary, fmt.Errorf("configure Discord media archiver: %w", err)
		}
	}
	repairLower, err := imp.repairLowerBound(lowerBound, opts.Full, opts.EditRescanWindow)
	if err != nil {
		return summary, err
	}

	containers, err := imp.importerContainers(
		source.ID, catalog.Containers, state, opts.GuildID, opts.GuildConfig,
		repairLower, opts.Full,
	)
	if err != nil {
		return summary, err
	}
	if err := imp.stageCatalogContainers(source.ID, containers, state); err != nil {
		return summary, err
	}
	state.ThreadCatalog = catalog.ThreadCatalog
	if err := imp.saveCheckpoint(syncID, state, summary); err != nil {
		return summary, err
	}
	for _, container := range containers {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		if err := imp.importContainer(
			ctx, source.ID, syncID, container, lowerBound, state, summary, media,
			repairLower,
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

// stageCatalogContainers makes every discovered container recoverable before
// publishing archive watermarks that can page past it. Conversation metadata
// and an empty per-container state entry together form the durable catalog.
func (imp *Importer) stageCatalogContainers(
	sourceID int64, containers []importerContainer, state *SyncState,
) error {
	for _, container := range containers {
		if container.Channel.ID == "" {
			return errors.New("catalog container has an empty ID")
		}
		conversationID, err := imp.store.EnsureConversationWithType(
			sourceID, container.Channel.ID, discordConversationType, container.Channel.Name,
		)
		if err != nil {
			return fmt.Errorf("stage Discord container %s: ensure conversation: %w", container.Channel.ID, err)
		}
		if !container.preserveMetadata {
			mapped, err := mapConversation(&container.Channel)
			if err != nil {
				return fmt.Errorf("stage Discord container %s: %w", container.Channel.ID, err)
			}
			if err := imp.setConversationCatalogMetadata(conversationID, mapped.Metadata); err != nil {
				return fmt.Errorf("stage Discord container %s: %w", container.Channel.ID, err)
			}
		}
		if _, exists := state.Containers[container.Channel.ID]; !exists {
			state.Containers[container.Channel.ID] = ContainerState{}
		}
	}
	return nil
}

func (imp *Importer) initialState(sourceID int64, full bool, lowerBound string) (*SyncState, bool, error) {
	state := NewSyncState()
	state.Full = full
	state.LowerBound = lowerBound
	hadBaseline := false
	if !full {
		last, err := imp.store.GetLastSuccessfulSync(sourceID)
		switch {
		case err == nil:
			hadBaseline = true
			if last.CursorAfter.Valid {
				state, err = LoadSyncState(last.CursorAfter.String)
				if err != nil {
					return nil, hadBaseline, fmt.Errorf("load last successful Discord sync state: %w", err)
				}
				state.Full = full
				state.LowerBound = lowerBound
			}
		case errors.Is(err, store.ErrSyncRunNotFound):
		case err != nil:
			return nil, false, fmt.Errorf("load last successful Discord sync: %w", err)
		}
	}

	checkpoint, err := imp.store.GetLatestCheckpointedSync(sourceID)
	switch {
	case err == nil:
		if checkpoint.CursorBefore.Valid {
			newer, loadErr := LoadSyncState(checkpoint.CursorBefore.String)
			if loadErr != nil {
				if full {
					return state, false, nil
				}
				return nil, hadBaseline, fmt.Errorf("load latest Discord checkpoint: %w", loadErr)
			}
			if newer.CompatibleRun(full, lowerBound) {
				if full {
					state = newer
				} else if mergeErr := state.Merge(newer); mergeErr != nil {
					return nil, hadBaseline, fmt.Errorf("merge latest Discord checkpoint: %w", mergeErr)
				}
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

func (imp *Importer) importerContainers(
	sourceID int64,
	discovered []CatalogContainer,
	state *SyncState,
	guildID string,
	guildConfig config.DiscordGuildConfig,
	repairLower string,
	full bool,
) ([]importerContainer, error) {
	containers := make([]importerContainer, 0, len(discovered)+len(state.Containers))
	seen := make(map[string]struct{}, len(containers))
	for _, container := range discovered {
		containers = append(containers, importerContainer{CatalogContainer: container})
		seen[container.Channel.ID] = struct{}{}
	}
	storedIDs := make([]string, 0, len(state.Containers))
	for containerID := range state.Containers {
		if _, ok := seen[containerID]; ok {
			continue
		}
		storedIDs = append(storedIDs, containerID)
	}
	storedMetadata, err := imp.store.ConversationMetadataBatch(sourceID, storedIDs)
	if err != nil {
		return nil, fmt.Errorf("load stored Discord container metadata: %w", err)
	}

	// A thread can disappear from the active/archived catalog while remaining
	// accessible. Recover its archived parent before reapplying today's filters;
	// malformed or absent metadata is included only when filters cannot depend
	// on the unknown parent or explicitly name the container itself.
	for _, containerID := range storedIDs {
		if !storedContainerIncluded(guildConfig, containerID, storedMetadata[containerID]) {
			continue
		}
		needsImport, err := storedContainerNeedsImport(
			state.Containers[containerID], storedMetadata[containerID], repairLower, full,
			slices.Contains(guildConfig.Include, containerID),
		)
		if err != nil {
			return nil, fmt.Errorf("select stored Discord container %s: %w", containerID, err)
		}
		if !needsImport {
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
	return containers, nil
}

func storedContainerNeedsImport(
	state ContainerState, metadata sql.NullString, repairLower string, full, explicitlyIncluded bool,
) (bool, error) {
	if !state.BackfillComplete || explicitlyIncluded {
		return true, nil
	}
	if !metadata.Valid || strings.TrimSpace(metadata.String) == "" || !json.Valid([]byte(metadata.String)) {
		return true, nil
	}
	var archived struct {
		DiscordChannelType *int            `json:"discord_channel_type"`
		InaccessibleSince  json.RawMessage `json:"container_inaccessible_since"`
		MissingSince       json.RawMessage `json:"container_missing_since"`
	}
	_ = json.Unmarshal([]byte(metadata.String), &archived)
	if archived.DiscordChannelType == nil {
		return true, nil
	}
	if len(archived.InaccessibleSince) != 0 || len(archived.MissingSince) != 0 ||
		isTopLevelMessageContainer(*archived.DiscordChannelType) {
		return true, nil
	}
	if !isThreadMessageContainer(*archived.DiscordChannelType) {
		return true, nil
	}
	// A complete full catalog already exhausts every accessible archived-thread
	// endpoint. An absent completed thread needs no second message probe.
	if full {
		return false, nil
	}
	if state.HighWater == "" {
		return true, nil
	}
	return snowflakeAfter(state.HighWater, repairLower)
}

func storedContainerIncluded(
	guildConfig config.DiscordGuildConfig, containerID string, metadata sql.NullString,
) bool {
	if slices.Contains(guildConfig.Exclude, containerID) {
		return false
	}
	if slices.Contains(guildConfig.Include, containerID) {
		return true
	}
	if len(guildConfig.Include) == 0 && len(guildConfig.Exclude) == 0 {
		return true
	}

	parentID, ok := storedContainerParent(metadata)
	if !ok {
		return false
	}
	return ContainerIncluded(guildConfig, containerID, parentID)
}

func storedContainerParent(metadata sql.NullString) (string, bool) {
	if !metadata.Valid || strings.TrimSpace(metadata.String) == "" {
		return "", false
	}
	var archived struct {
		ParentChannelID    string `json:"parent_channel_id"`
		DiscordChannelType *int   `json:"discord_channel_type"`
	}
	if err := json.Unmarshal([]byte(metadata.String), &archived); err != nil || archived.DiscordChannelType == nil {
		return "", false
	}
	if isTopLevelMessageContainer(*archived.DiscordChannelType) {
		return "", true
	}
	parent, err := ParseSnowflake(archived.ParentChannelID)
	if err != nil || parent == 0 {
		return "", false
	}
	return archived.ParentChannelID, true
}

func (imp *Importer) importContainer(
	ctx context.Context,
	sourceID, syncID int64,
	container importerContainer,
	lowerBound string,
	state *SyncState,
	summary *ImportSummary,
	media *MediaArchiver,
	repairLower string,
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
		if err := imp.setConversationCatalogMetadata(conversationID, mapped.Metadata); err != nil {
			return err
		}
	}

	containerState := state.Containers[container.Channel.ID]
	// Retain even an empty first-seen entry so a denied archived thread remains
	// independently retryable after it drops out of later catalog responses.
	state.Containers[container.Channel.ID] = containerState
	if !containerState.BackfillComplete {
		if err := imp.backfill(
			ctx, sourceID, syncID, conversationID, container.Channel.ID,
			lowerBound, &containerState, state, summary, media,
		); err != nil {
			return imp.handleContainerError(
				syncID, conversationID, container.Channel.ID, err, state, summary,
			)
		}
	}
	if containerState.HighWater == "0" {
		// Repair checkpoints written by the pre-release importer review build.
		// The container snowflake has the same race-safe ordering property used
		// for a newly empty container and is accepted by Discord's API.
		containerState.HighWater, err = maximumSnowflake(container.Channel.ID, lowerBound)
		if err != nil {
			return fmt.Errorf("repair empty Discord container cursor: %w", err)
		}
		state.Containers[container.Channel.ID] = containerState
	}
	if err := imp.forward(
		ctx, sourceID, syncID, conversationID, container.Channel.ID,
		&containerState, state, summary, media,
	); err != nil {
		return imp.handleContainerError(
			syncID, conversationID, container.Channel.ID, err, state, summary,
		)
	}
	if err := imp.reconcile(
		ctx, sourceID, conversationID, container.Channel.ID,
		repairLower, summary, media,
	); err != nil {
		return imp.handleContainerError(
			syncID, conversationID, container.Channel.ID, err, state, summary,
		)
	}
	if err := imp.clearContainerAccessMarkers(conversationID); err != nil {
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
			// A container must exist before any message can be created in it, so
			// its own snowflake is a valid nonzero lower cursor after an empty
			// head response. This avoids both a local-clock race and the invalid
			// zero cursor rejected by the production REST client.
			containerState.HighWater, err = maximumSnowflake(containerID, lowerBound)
			if err != nil {
				return fmt.Errorf("pin empty Discord container cursor: %w", err)
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
		containerState.HighWater, err = maximumSnowflake(
			containerState.HighWater, containerState.BackfillUpper,
		)
		if err != nil {
			return fmt.Errorf("pin backfill high-water: %w", err)
		}
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

func (imp *Importer) repairLowerBound(
	explicitLower string, full bool, editRescanWindow time.Duration,
) (string, error) {
	if full {
		if explicitLower != "" {
			return explicitLower, nil
		}
		return "0", nil
	}
	if editRescanWindow <= 0 {
		editRescanWindow = config.DefaultDiscordEditRescanWindow
	}
	now := time.Now
	if imp.now != nil {
		now = imp.now
	}
	windowLower, err := SnowflakeFromTimestamp(now().UTC().Add(-editRescanWindow))
	if err != nil {
		return "", fmt.Errorf("convert Discord edit-rescan lower bound: %w", err)
	}
	if explicitLower == "" {
		return windowLower, nil
	}
	return maximumSnowflake(windowLower, explicitLower)
}

// reconcile refreshes and compares one complete, immutable snowflake interval.
// No tombstones are written until every remote page in the interval has been
// validated and durably persisted.
func (imp *Importer) reconcile(
	ctx context.Context,
	sourceID, conversationID int64,
	containerID, lower string,
	summary *ImportSummary,
	media *MediaArchiver,
) error {
	localUpper, err := imp.store.MaxMessageSourceIDInSnowflakeInterval(
		sourceID, conversationID, lower, strconv.FormatUint(math.MaxUint64, 10),
	)
	if err != nil {
		return fmt.Errorf("pin local Discord repair interval: %w", err)
	}

	head, err := imp.api.Messages(ctx, containerID, MessageQuery{Limit: 1})
	if err != nil {
		return fmt.Errorf("pin repair head: %w", err)
	}
	if len(head) > 1 {
		return fmt.Errorf("pin repair head: expected at most one message, got %d", len(head))
	}

	remoteHead := ""
	if len(head) == 1 {
		if err := validateDiscordMessage(containerID, head[0]); err != nil {
			return fmt.Errorf("pin repair head: %w", err)
		}
		remoteHead = head[0].ID
	}
	upper := localUpper
	if remoteHead != "" {
		upper, err = maximumSnowflake(upper, remoteHead)
		if err != nil {
			return err
		}
	}
	if upper == "" {
		return nil
	}
	afterLower, err := snowflakeAfter(upper, lower)
	if err != nil {
		return err
	}
	if !afterLower {
		return nil
	}
	stagedRemote, err := os.CreateTemp("", ".msgvault-discord-repair-*")
	if err != nil {
		return fmt.Errorf("stage Discord repair IDs: %w", err)
	}
	stagedPath := stagedRemote.Name()
	defer func() {
		_ = stagedRemote.Close()
		_ = os.Remove(stagedPath)
	}()
	remoteWriter := bufio.NewWriter(stagedRemote)
	stageRemoteID := func(messageID string) error {
		if _, err := remoteWriter.WriteString(messageID + "\n"); err != nil {
			return fmt.Errorf("stage Discord repair ID: %w", err)
		}
		return nil
	}

	before, successorErr := snowflakeSuccessor(upper)
	if successorErr != nil {
		// MaxUint64 has no successor. If it is the remote head, retain that
		// already-fetched object and enumerate the strict remainder below it.
		if remoteHead == upper {
			if err := imp.persistPage(ctx, sourceID, conversationID, head, summary, media); err != nil {
				return err
			}
			if err := stageRemoteID(remoteHead); err != nil {
				return err
			}
		}
		before = upper
	}

	for {
		page, err := imp.api.Messages(ctx, containerID, MessageQuery{Before: before, Limit: imp.pageSize})
		if err != nil {
			return fmt.Errorf("page repair before %s: %w", before, err)
		}
		eligible, pageMin, reachedLower, err := filterRepairPage(containerID, page, before, lower)
		if err != nil {
			return err
		}
		if err := imp.persistPage(ctx, sourceID, conversationID, eligible, summary, media); err != nil {
			return err
		}
		for _, message := range eligible {
			if err := stageRemoteID(message.ID); err != nil {
				return err
			}
		}
		if reachedLower || len(page) < imp.pageSize {
			break
		}
		if pageMin == "" || pageMin == before {
			return errors.New("discord repair pagination cursor did not advance")
		}
		before = pageMin
	}

	if err := remoteWriter.Flush(); err != nil {
		return fmt.Errorf("flush staged Discord repair IDs: %w", err)
	}
	if _, err := stagedRemote.Seek(0, 0); err != nil {
		return fmt.Errorf("rewind staged Discord repair IDs: %w", err)
	}
	remoteScanner := bufio.NewScanner(stagedRemote)
	remoteID, remoteOK := "", remoteScanner.Scan()
	if remoteOK {
		remoteID = remoteScanner.Text()
	}
	advanceRemote := func() {
		remoteOK = remoteScanner.Scan()
		if remoteOK {
			remoteID = remoteScanner.Text()
		}
	}
	stagedMissing, err := os.CreateTemp("", ".msgvault-discord-missing-*")
	if err != nil {
		return fmt.Errorf("stage missing Discord repair IDs: %w", err)
	}
	stagedMissingPath := stagedMissing.Name()
	defer func() {
		_ = stagedMissing.Close()
		_ = os.Remove(stagedMissingPath)
	}()
	missingWriter := bufio.NewWriter(stagedMissing)

	localBefore := ""
	for {
		localIDs, err := imp.store.MessageSourceIDsInSnowflakeIntervalPage(
			sourceID, conversationID, lower, upper, localBefore, imp.pageSize,
		)
		if err != nil {
			return fmt.Errorf("load archived Discord repair interval: %w", err)
		}
		for _, localID := range localIDs {
			for remoteOK {
				remoteAfterLocal, err := snowflakeAfter(remoteID, localID)
				if err != nil {
					return fmt.Errorf("compare staged Discord repair ID: %w", err)
				}
				if !remoteAfterLocal {
					break
				}
				advanceRemote()
			}
			if remoteOK && remoteID == localID {
				advanceRemote()
				continue
			}
			if _, err := missingWriter.WriteString(localID + "\n"); err != nil {
				return fmt.Errorf("stage missing Discord repair ID: %w", err)
			}
		}
		if len(localIDs) < imp.pageSize {
			break
		}
		localBefore = localIDs[len(localIDs)-1]
	}
	for remoteOK {
		advanceRemote()
	}
	if err := remoteScanner.Err(); err != nil {
		return fmt.Errorf("read staged Discord repair IDs: %w", err)
	}
	if err := missingWriter.Flush(); err != nil {
		return fmt.Errorf("flush missing Discord repair IDs: %w", err)
	}
	if _, err := stagedMissing.Seek(0, 0); err != nil {
		return fmt.Errorf("rewind missing Discord repair IDs: %w", err)
	}
	if err := imp.store.MarkMessagesDeletedFromReader(sourceID, stagedMissing, imp.pageSize); err != nil {
		return fmt.Errorf("mark deleted Discord messages: %w", err)
	}
	return nil
}

func filterRepairPage(
	containerID string, page []Message, before, lower string,
) (eligible []Message, pageMin string, reachedLower bool, retErr error) {
	previous := before
	for _, message := range page {
		if err := validateDiscordMessage(containerID, message); err != nil {
			return nil, "", false, err
		}
		belowCursor, err := snowflakeAfter(previous, message.ID)
		if err != nil || !belowCursor {
			if err == nil {
				err = fmt.Errorf("message is not strictly below prior cursor %s", previous)
			}
			return nil, "", false, fmt.Errorf("discord repair page message %s: %w", message.ID, err)
		}
		previous = message.ID
		pageMin, err = minimumSnowflake(pageMin, message.ID)
		if err != nil {
			return nil, "", false, err
		}
		afterLower, err := snowflakeAfter(message.ID, lower)
		if err != nil {
			return nil, "", false, err
		}
		if !afterLower {
			reachedLower = true
			continue
		}
		eligible = append(eligible, message)
	}
	return eligible, pageMin, reachedLower, nil
}

func (imp *Importer) handleContainerError(
	syncID, conversationID int64,
	containerID string,
	importErr error,
	state *SyncState,
	summary *ImportSummary,
) error {
	marker, reason, ok := containerAccessMarker(importErr)
	if !ok {
		return importErr
	}
	if err := imp.setContainerAccessMarker(conversationID, marker, reason); err != nil {
		return errors.Join(importErr, err)
	}
	if err := imp.saveCheckpoint(syncID, state, summary); err != nil {
		return errors.Join(importErr, err)
	}
	issue, classified := newContainerIssue(containerID, importErr)
	if classified {
		summary.ContainerIssues = append(summary.ContainerIssues, issue)
	}
	return nil
}

func newContainerIssue(containerID string, err error) (ContainerIssue, bool) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return ContainerIssue{}, false
	}
	issue := ContainerIssue{
		ContainerID: containerID,
		StatusCode:  apiErr.StatusCode,
		DiscordCode: apiErr.Code,
	}
	switch apiErr.StatusCode {
	case http.StatusForbidden:
		issue.Kind = ContainerIssueForbidden
		return issue, true
	case http.StatusNotFound:
		if apiErr.Code == 10003 {
			issue.Kind = ContainerIssueUnknownChannel
			return issue, true
		}
	}
	return ContainerIssue{}, false
}

func containerAccessMarker(err error) (marker, reason string, ok bool) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return "", "", false
	}
	switch apiErr.StatusCode {
	case http.StatusForbidden:
		return "container_inaccessible_since", "", true
	case http.StatusNotFound:
		if apiErr.Code == 10003 {
			return "container_missing_since", "unknown_channel", true
		}
	}
	return "", "", false
}

func (imp *Importer) setContainerAccessMarker(
	conversationID int64, marker, reason string,
) error {
	metadata, err := imp.containerMetadata(conversationID)
	if err != nil {
		return err
	}
	now := time.Now
	if imp.now != nil {
		now = imp.now
	}
	if _, exists := metadata[marker]; !exists {
		encodedTime, err := json.Marshal(now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
		metadata[marker] = encodedTime
	}
	if reason != "" {
		encodedReason, err := json.Marshal(reason)
		if err != nil {
			return err
		}
		metadata["container_missing_reason"] = encodedReason
	}
	return imp.writeContainerMetadata(conversationID, metadata)
}

func (imp *Importer) setConversationCatalogMetadata(
	conversationID int64, catalogMetadata json.RawMessage,
) error {
	metadata := make(map[string]json.RawMessage)
	if len(catalogMetadata) != 0 {
		if err := json.Unmarshal(catalogMetadata, &metadata); err != nil {
			return fmt.Errorf("decode mapped Discord conversation metadata: %w", err)
		}
	}
	stored, err := imp.store.GetConversationMetadata(conversationID)
	if err != nil {
		return err
	}
	if stored.Valid && json.Valid([]byte(stored.String)) {
		var existing map[string]json.RawMessage
		if err := json.Unmarshal([]byte(stored.String), &existing); err != nil {
			return fmt.Errorf("decode stored Discord conversation metadata: %w", err)
		}
		for _, key := range []string{
			"container_inaccessible_since", "container_missing_since", "container_missing_reason",
		} {
			if value, ok := existing[key]; ok {
				metadata[key] = value
			}
		}
	}
	return imp.writeContainerMetadata(conversationID, metadata)
}

func (imp *Importer) clearContainerAccessMarkers(conversationID int64) error {
	stored, err := imp.store.GetConversationMetadata(conversationID)
	if err != nil {
		return err
	}
	if !stored.Valid || strings.TrimSpace(stored.String) == "" {
		return nil
	}
	metadata := make(map[string]json.RawMessage)
	if !json.Valid([]byte(stored.String)) {
		// A legacy opaque metadata payload may still identify an explicitly
		// selected stored container. With no safely mergeable object, preserve it.
		return nil
	}
	if err := json.Unmarshal([]byte(stored.String), &metadata); err != nil {
		return fmt.Errorf("decode Discord conversation metadata: %w", err)
	}
	changed := false
	for _, key := range []string{
		"container_inaccessible_since", "container_missing_since", "container_missing_reason",
	} {
		if _, ok := metadata[key]; ok {
			delete(metadata, key)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return imp.writeContainerMetadata(conversationID, metadata)
}

func (imp *Importer) containerMetadata(conversationID int64) (map[string]json.RawMessage, error) {
	stored, err := imp.store.GetConversationMetadata(conversationID)
	if err != nil {
		return nil, err
	}
	metadata := make(map[string]json.RawMessage)
	if !stored.Valid || strings.TrimSpace(stored.String) == "" {
		return metadata, nil
	}
	if err := json.Unmarshal([]byte(stored.String), &metadata); err != nil {
		return nil, fmt.Errorf("decode Discord conversation metadata: %w", err)
	}
	return metadata, nil
}

func (imp *Importer) writeContainerMetadata(
	conversationID int64, metadata map[string]json.RawMessage,
) error {
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("encode Discord conversation metadata: %w", err)
	}
	return imp.store.SetConversationMetadata(conversationID, sql.NullString{
		String: string(encoded), Valid: len(metadata) != 0,
	})
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
		message := &page[i]
		_, alreadyProcessed := summary.processedMessageIDs[message.ID]
		mapped, err := mapMessage(message, conversationID, sourceID)
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
			return fmt.Errorf("persist Discord message %s: %w", message.ID, err)
		}
		if err := imp.store.ClearMessageDeletedFromSource(sourceID, message.ID); err != nil {
			return fmt.Errorf("clear Discord message %s tombstone: %w", message.ID, err)
		}
		if mapped.Edited {
			if err := imp.store.SetMessageEdited(messageID); err != nil {
				return fmt.Errorf("mark Discord message %s edited: %w", message.ID, err)
			}
		}
		for _, participantID := range participantIDs {
			if err := imp.store.EnsureConversationParticipant(conversationID, participantID, "member"); err != nil {
				return fmt.Errorf("persist Discord conversation participant: %w", err)
			}
		}
		if media != nil {
			result, err := media.persistAttachments(ctx, messageID, message.Attachments, !alreadyProcessed)
			if err != nil {
				return fmt.Errorf("persist Discord message %s media metadata: %w", message.ID, err)
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
			pendingCount := len(mapped.Attachments)
			if alreadyProcessed {
				existingAttachments, err := imp.store.MessageDiscordAttachments(messageID)
				if err != nil {
					return fmt.Errorf("load Discord message %s attachment metadata: %w", message.ID, err)
				}
				pendingCount = 0
				for _, attachment := range mapped.Attachments {
					if _, ok := existingAttachments[attachment.SourceAttachmentID]; !ok {
						pendingCount++
					}
				}
			}
			if err := imp.store.ReplaceMessageDiscordAttachments(messageID, mapped.Attachments); err != nil {
				return fmt.Errorf("persist Discord message %s attachment metadata: %w", message.ID, err)
			}
			if err := imp.store.RecomputeMessageAttachmentStats(messageID); err != nil {
				return fmt.Errorf("recompute Discord message %s attachment metadata: %w", message.ID, err)
			}
			summary.MediaPending += int64(pendingCount)
		}

		if reference := message.MessageReference; reference != nil && reference.MessageID != "" {
			if err := imp.store.SetReplyTo(sourceID, message.ID, reference.MessageID); err != nil {
				return fmt.Errorf("link Discord reply %s: %w", message.ID, err)
			}
		}
		if _, counted := summary.processedMessageIDs[message.ID]; !counted {
			summary.processedMessageIDs[message.ID] = struct{}{}
			summary.MessagesProcessed++
			if _, ok := existing[message.ID]; ok {
				summary.MessagesUpdated++
			} else {
				summary.MessagesAdded++
			}
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
	afterID := int64(0)
	for {
		unresolved, err := imp.store.ListUnresolvedMessageRepliesAfter(
			sourceID, discordMessageType, afterID, imp.pageSize,
		)
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
			if metadata.ReferencedMessageID != "" {
				if err := imp.store.SetReplyTo(sourceID, reply.SourceMessageID, metadata.ReferencedMessageID); err != nil {
					return fmt.Errorf("resolve deferred Discord reply %s: %w", reply.SourceMessageID, err)
				}
			}
			afterID = reply.MessageID
		}
		if len(unresolved) < imp.pageSize {
			return nil
		}
	}
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
