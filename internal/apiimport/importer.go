package apiimport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"go.kenn.io/msgvault/internal/store"
)

var ErrUnavailable = errors.New("importer is unavailable")

type Status string

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

// Source is the import source an item is archived under.
type Source struct {
	Type         string
	Identifier   string
	DisplayName  string
	AccountEmail string
}

// Write reports what persisting one item did. Changed means the archive
// write committed, even when a later step returned an error.
type Write struct {
	MessageID int64
	Created   bool
	Changed   bool
}

// Item is one validated item and the function that archives it.
type Item struct {
	Source          Source
	SourceMessageID string
	// ExcludeFromReattribution keeps this item's stored message out of the
	// re-attribution that a newly confirmed account address runs, for a
	// writer that derives its attribution itself.
	ExcludeFromReattribution bool
	// Write persists the item through st, which is scoped to the import's
	// sync run.
	Write func(ctx context.Context, st *store.Store, sourceID int64) (Write, error)
}

// Importer runs one item through source setup, a sync run, the item's write
// and the analytics cache refresh.
type Importer struct {
	store  *store.Store
	kind   string
	hooks  Hooks
	logger *slog.Logger

	beforeCheckpoint func()
}

// NewImporter returns an importer whose errors and logs name kind, such as
// "meeting import".
func NewImporter(s *store.Store, kind string, hooks Hooks) *Importer {
	return &Importer{store: s, kind: kind, hooks: hooks, logger: slog.Default()}
}

// WithLogger sets the logger used for best-effort post-import work.
func (i *Importer) WithLogger(logger *slog.Logger) *Importer {
	if logger != nil {
		i.logger = logger
	}
	return i
}

func (i *Importer) Available() bool {
	return i != nil && i.store != nil
}

func (i *Importer) Import(ctx context.Context, item Item) (result Result, retErr error) {
	if !i.Available() {
		return Result{}, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	src := item.Source

	source, err := i.store.GetOrCreateSourceContext(ctx, src.Type, src.Identifier)
	if err != nil {
		return Result{}, fmt.Errorf("resolve %s source: %w", i.kind, err)
	}
	result.SourceID = source.ID
	result.SourceMessageID = item.SourceMessageID

	displayName := src.DisplayName
	if displayName == "" && (!source.DisplayName.Valid || strings.TrimSpace(source.DisplayName.String) == "") {
		displayName = src.Identifier
	}
	if displayName != "" && (!source.DisplayName.Valid || source.DisplayName.String != displayName) {
		if err := i.store.UpdateSourceDisplayNameContext(ctx, source.ID, displayName); err != nil {
			if !errors.Is(err, store.ErrSourceSettingsInvalid) {
				return result, fmt.Errorf("update %s source display name: %w", i.kind, err)
			}
			i.logger.WarnContext(ctx,
				i.kind+" source display name conflicts with another source selector; keeping current name",
				"source_id", source.ID,
			)
		}
	}
	excluded := ""
	if item.ExcludeFromReattribution {
		excluded = item.SourceMessageID
	}
	if err := i.store.AddAccountIdentityAndRefreshMessageAttributionContext(
		ctx, source.ID, src.AccountEmail, "account-email", excluded,
	); err != nil {
		return result, fmt.Errorf("confirm %s source identity: %w", i.kind, err)
	}
	if i.hooks.AfterSourceSetup != nil {
		if err := i.hooks.AfterSourceSetup(); err != nil {
			return result, fmt.Errorf("run post-source setup: %w", err)
		}
	}

	syncID, err := i.store.StartSyncContext(ctx, source.ID, src.Type)
	if err != nil {
		return result, fmt.Errorf("start %s sync: %w", i.kind, err)
	}
	st := i.store.ScopedToSync(source.ID, syncID)
	checkpoint := &store.Checkpoint{}
	defer func() {
		if retErr == nil {
			return
		}
		// The failed run must be recorded even when the request was canceled.
		if failErr := st.FailSyncWithCheckpointContext(
			context.WithoutCancel(ctx), syncID, retErr.Error(), checkpoint,
		); failErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("record failed %s sync: %w", i.kind, failErr))
		}
	}()

	write, writeErr := item.Write(ctx, st, source.ID)
	if write.MessageID != 0 {
		result.MessageID = write.MessageID
		result.Status = StatusUpdated
		checkpoint.MessagesProcessed = 1
		if write.Created {
			result.Status = StatusCreated
			checkpoint.MessagesAdded = 1
		} else if write.Changed {
			checkpoint.MessagesUpdated = 1
		}
	}
	label := src.Type + ":" + src.Identifier
	if writeErr != nil {
		if write.Changed {
			// The item is stored; keep the cache in step even though the run failed.
			i.refreshCache(context.WithoutCancel(ctx), label, source.ID, item.SourceMessageID)
		}
		return result, writeErr
	}

	if i.beforeCheckpoint != nil {
		i.beforeCheckpoint()
	}
	if err := st.UpdateSyncCheckpointContext(ctx, syncID, checkpoint); err != nil {
		return result, fmt.Errorf("checkpoint %s sync: %w", i.kind, err)
	}
	if err := st.CompleteSyncContext(ctx, syncID, ""); err != nil {
		return result, fmt.Errorf("complete %s sync: %w", i.kind, err)
	}
	// Even an unchanged item can follow an account identity that
	// re-attributed earlier messages; the refresh's own staleness check
	// decides whether a build is due.
	i.refreshCache(ctx, label, source.ID, item.SourceMessageID)
	return result, nil
}

func (i *Importer) refreshCache(ctx context.Context, label string, sourceID int64, sourceMessageID string) {
	if i.hooks.RefreshCache == nil {
		return
	}
	// The import has completed before this best-effort hook runs.
	if err := i.hooks.RefreshCache(ctx, label); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		i.logger.Error(i.kind+" analytics cache refresh failed",
			"source", label,
			"source_id", sourceID,
			"source_message_id", sourceMessageID,
			"error", err,
		)
	}
}
