package meetingimport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"go.kenn.io/msgvault/internal/meetingarchive"
	"go.kenn.io/msgvault/internal/store"
)

type Status string

var ErrUnavailable = errors.New("meeting importer is unavailable")

const (
	StatusCreated Status = "created"
	StatusUpdated Status = "updated"
)

type Result struct {
	Status          Status
	SourceID        int64
	MessageID       int64
	SourceMessageID string
}

type Hooks struct {
	AfterSourceSetup func() error
	RefreshCache     func(context.Context, string) error
}

type Importer struct {
	store  *store.Store
	hooks  Hooks
	logger *slog.Logger

	beforeCheckpointForTest func()
}

func NewImporter(s *store.Store, hooks Hooks) *Importer {
	return &Importer{store: s, hooks: hooks, logger: slog.Default()}
}

// WithLogger sets the logger used for best-effort post-import work.
func (i *Importer) WithLogger(logger *slog.Logger) *Importer {
	if logger != nil {
		i.logger = logger
	}
	return i
}

func (i *Importer) Import(ctx context.Context, req Request) (result Result, retErr error) {
	if i == nil || i.store == nil {
		return Result{}, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	normalized, err := req.Normalize()
	if err != nil {
		return Result{}, err
	}
	snapshot, err := BuildSnapshot(normalized)
	if err != nil {
		return Result{}, err
	}

	source, err := i.store.GetOrCreateSource(SourceType, snapshot.SourceIdentifier)
	if err != nil {
		return Result{}, fmt.Errorf("resolve meeting source: %w", err)
	}
	result.SourceID = source.ID
	result.SourceMessageID = snapshot.SourceMessageID

	displayName := snapshot.SourceDisplayName
	if displayName == "" && (!source.DisplayName.Valid || strings.TrimSpace(source.DisplayName.String) == "") {
		displayName = snapshot.SourceIdentifier
	}
	if displayName != "" && (!source.DisplayName.Valid || source.DisplayName.String != displayName) {
		if err := i.store.UpdateSourceDisplayNameContext(ctx, source.ID, displayName); err != nil {
			return result, fmt.Errorf("update meeting source display name: %w", err)
		}
	}
	if err := i.store.AddAccountIdentityAndRefreshMessageAttributionContext(
		ctx,
		source.ID,
		snapshot.AccountEmail,
		"account-email",
		snapshot.SourceMessageID,
	); err != nil {
		return result, fmt.Errorf("confirm meeting source identity: %w", err)
	}
	if i.hooks.AfterSourceSetup != nil {
		if err := i.hooks.AfterSourceSetup(); err != nil {
			return result, fmt.Errorf("run post-source setup: %w", err)
		}
	}

	syncID, err := i.store.StartSyncContext(ctx, source.ID, SourceType)
	if err != nil {
		return result, fmt.Errorf("start meeting import sync: %w", err)
	}
	scoped := *i
	scoped.store = i.store.ScopedToSync(source.ID, syncID)
	i = &scoped
	checkpoint := &store.Checkpoint{}
	defer func() {
		if retErr == nil {
			return
		}
		if failErr := i.store.FailSyncWithCheckpoint(syncID, retErr.Error(), checkpoint); failErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("record failed meeting import sync: %w", failErr))
		}
	}()

	var organizer *meetingarchive.Person
	if snapshot.Organizer != nil {
		organizer = &meetingarchive.Person{
			Name:  snapshot.Organizer.Name,
			Email: snapshot.Organizer.Email,
		}
	}
	attendees := make([]meetingarchive.Person, 0, len(snapshot.Attendees))
	for _, attendee := range snapshot.Attendees {
		attendees = append(attendees, meetingarchive.Person{
			Name:  attendee.Name,
			Email: attendee.Email,
		})
	}
	archiveResult, archiveErr := meetingarchive.New(i.store).Upsert(ctx, meetingarchive.Snapshot{
		SourceID:             source.ID,
		AccountEmail:         snapshot.AccountEmail,
		SourceMessageID:      snapshot.SourceMessageID,
		SourceConversationID: snapshot.SourceMessageID,
		Title:                snapshot.Title,
		StartedAt:            snapshot.StartedAt,
		Body:                 snapshot.Body,
		Snippet:              snapshot.Snippet,
		Metadata:             snapshot.Metadata,
		Raw:                  snapshot.Raw,
		RawFormat:            RawFormat,
		Organizer:            organizer,
		Attendees:            attendees,
	}, meetingarchive.UpsertOptions{})
	if archiveResult.MessageID != 0 {
		result.MessageID = archiveResult.MessageID
		result.Status = StatusUpdated
		checkpoint.MessagesProcessed = 1
		if archiveResult.Created {
			result.Status = StatusCreated
			checkpoint.MessagesAdded = 1
		} else if archiveResult.Changed {
			checkpoint.MessagesUpdated = 1
		}
	}
	if archiveErr != nil {
		if archiveResult.Changed {
			i.refreshCache(context.WithoutCancel(ctx), SourceType+":"+snapshot.SourceIdentifier,
				source.ID, snapshot.SourceMessageID)
		}
		return result, archiveErr
	}

	if i.beforeCheckpointForTest != nil {
		i.beforeCheckpointForTest()
	}
	if err := i.store.UpdateSyncCheckpointContext(ctx, syncID, checkpoint); err != nil {
		return result, fmt.Errorf("checkpoint meeting import sync: %w", err)
	}
	if err := i.store.CompleteSyncContext(ctx, syncID, ""); err != nil {
		return result, fmt.Errorf("complete meeting import sync: %w", err)
	}
	i.refreshCache(ctx, SourceType+":"+snapshot.SourceIdentifier,
		source.ID, snapshot.SourceMessageID)
	return result, nil
}

func (i *Importer) refreshCache(ctx context.Context, label string, sourceID int64, sourceMessageID string) {
	if i.hooks.RefreshCache == nil {
		return
	}
	// The import has completed successfully before this best-effort hook runs.
	if err := i.hooks.RefreshCache(ctx, label); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		logger := i.logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Error("meeting analytics cache refresh failed",
			"source", label,
			"source_id", sourceID,
			"source_message_id", sourceMessageID,
			"error", err,
		)
	}
}
