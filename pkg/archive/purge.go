package archive

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector/pgvector"
)

// PurgeChannel removes a channel and its retained threads, serializing with sync.
// A later import can restore provider content; callers own future selection.
func (a *Archive) PurgeChannel(ctx context.Context, sourceID int64, channelID string) (retErr error) {
	defer func() { retErr = errors.Join(retErr, a.pruneEmbeddings(ctx)) }()
	return a.store.PurgeChannelContext(ctx, sourceID, channelID)
}

// PurgeSource removes a source and its retained message data, serializing with sync.
// An already removed source succeeds. Attachment files and external indexes
// are managed separately by their owners.
func (a *Archive) PurgeSource(ctx context.Context, sourceID int64) (retErr error) {
	defer func() { retErr = errors.Join(retErr, a.pruneEmbeddings(ctx)) }()
	_, err := a.store.GetSourceByIDContext(ctx, sourceID)
	if errors.Is(err, store.ErrSourceNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	execution, err := a.store.AcquireSyncExecutionContext(ctx, sourceID)
	if errors.Is(err, store.ErrSourceNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, execution.Release()) }()
	// Another caller may remove the source between the lookup and the lock.
	_, _, err = a.store.RemoveSourceSerialized(ctx, sourceID)
	if errors.Is(err, store.ErrSourceNotFound) {
		return nil
	}
	return err
}

func (a *Archive) pruneEmbeddings(ctx context.Context) error {
	if a.embeddings == nil && a.vectorErr == nil {
		return nil
	}
	var exists bool
	var schema string
	if err := a.store.DB().QueryRowContext(ctx, `SELECT current_schema(), to_regclass(format('%I.%I', current_schema(), 'embeddings')) IS NOT NULL`).Scan(&schema, &exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	_, err := pgvector.PruneOrphanEmbeddings(ctx, a.store.DB(), schema)
	return err
}
