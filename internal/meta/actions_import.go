package meta

import "context"

func (r *importRun) deletedByAction(ctx context.Context, id string) (bool, error) {
	return r.imp.store.HasSucceededMetaCommentDeletionContext(ctx, r.source.ID, r.source.Identifier, id)
}
