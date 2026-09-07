package remoteimage

import (
	"context"
	"errors"
	"log/slog"

	"go.kenn.io/msgvault/internal/store"
)

type BackfillResult struct {
	Messages   int
	Downloaded int
	Reused     int
	Errors     int
}

// Backfill archives remote images in existing email. Callers must obtain
// explicit tracking consent and the normal archive mutation gate first.
func (f *Fetcher) Backfill(ctx context.Context, st *store.Store, dir string, sourceID int64, limit int, logger *slog.Logger) (BackfillResult, error) {
	var result BackfillResult
	if sourceID < 0 || limit < 0 {
		return result, errors.New("source ID and limit must not be negative")
	}
	var after int64
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		pageSize := 100
		if limit > 0 {
			if result.Messages >= limit {
				return result, nil
			}
			pageSize = min(pageSize, limit-result.Messages)
		}
		ids, err := st.RemoteImageBackfillMessageIDs(ctx, after, sourceID, pageSize)
		if err != nil {
			return result, err
		}
		if len(ids) == 0 {
			return result, nil
		}
		for _, id := range ids {
			message, err := st.GetMessageContext(ctx, id)
			if err != nil {
				return result, err
			}
			archived := f.Archive(ctx, st, dir, id, message.BodyHTML)
			result.Messages++
			result.Downloaded += archived.Downloaded
			result.Reused += archived.Reused
			result.Errors += len(archived.Errors)
			for _, imageErr := range archived.Errors {
				logger.Warn("failed to archive remote image", "message", id, "error", imageErr)
			}
			after = id
			if err := ctx.Err(); err != nil {
				return result, err
			}
		}
	}
}
