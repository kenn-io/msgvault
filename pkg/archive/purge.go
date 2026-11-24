package archive

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

// PurgeChannel removes a channel and its retained threads, serializing with sync.
// A later import can restore provider content; callers own future selection.
func (a *Archive) PurgeChannel(ctx context.Context, sourceID int64, channelID string) error {
	if err := a.store.PurgeChannelContext(ctx, sourceID, channelID); err != nil {
		return err
	}
	return a.pruneEmbeddings(ctx)
}

// PurgeSource removes a source and its retained message data, serializing with sync.
// An already removed source succeeds. Attachment files and external indexes
// are managed separately by their owners.
func (a *Archive) PurgeSource(ctx context.Context, sourceID int64) (retErr error) {
	_, err := a.store.GetSourceByIDContext(ctx, sourceID)
	if errors.Is(err, store.ErrSourceNotFound) {
		return a.pruneEmbeddings(ctx)
	}
	if err != nil {
		return err
	}
	execution, err := a.store.AcquireSyncExecutionContext(ctx, sourceID)
	if errors.Is(err, store.ErrSourceNotFound) {
		return a.pruneEmbeddings(ctx)
	}
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, execution.Release()) }()
	// Another caller may remove the source between the lookup and the lock.
	_, _, err = a.store.RemoveSourceSerialized(ctx, sourceID)
	if errors.Is(err, store.ErrSourceNotFound) {
		return a.pruneEmbeddings(ctx)
	}
	if err != nil {
		return err
	}
	return a.pruneEmbeddings(ctx)
}

func (a *Archive) pruneEmbeddings(ctx context.Context) error {
	if a.embeddings == nil {
		return nil
	}
	pruner, ok := any(a.embeddings.backend).(vector.OrphanEmbeddingPruner)
	if !ok {
		return nil
	}
	_, err := pruner.PruneOrphanEmbeddings(ctx)
	return err
}
