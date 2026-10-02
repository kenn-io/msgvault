package importer

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/remoteimage"
	"go.kenn.io/msgvault/internal/store"
)

// pstMessageIngester stores a new PST message and then records the threading
// facts that ReconcilePstEmailThreadsContext reads when the import finishes.
func pstMessageIngester(images *remoteimage.Fetcher) rawMessageIngestFunc {
	ingest := rawMessageIngester(images)
	return func(
		ctx context.Context, st *store.Store, sourceID int64, identifier, attachmentsDir string,
		labelIDs []int64, sourceMsgID, rawHash string, raw []byte, fallbackDate time.Time,
		log *slog.Logger,
	) error {
		if err := ingest(ctx, st, sourceID, identifier, attachmentsDir, labelIDs, sourceMsgID,
			rawHash, raw, fallbackDate, log); err != nil {
			return err
		}
		return recordPstMessageHeaders(ctx, st, sourceID, sourceMsgID, raw)
	}
}

func recordPstMessageHeaders(ctx context.Context, st *store.Store, sourceID int64, sourceMsgID string, raw []byte) error {
	ids, err := st.MessageExistsBatch(sourceID, []string{sourceMsgID})
	if err != nil {
		return fmt.Errorf("find imported PST message: %w", err)
	}
	id, ok := ids[sourceMsgID]
	if !ok {
		return fmt.Errorf("imported PST message %q missing from source %d", sourceMsgID, sourceID)
	}
	return recordPstMessageHeadersByID(ctx, st, sourceID, id, raw)
}

func recordPstMessageHeadersByID(ctx context.Context, st *store.Store, sourceID, messageID int64, raw []byte) error {
	rfcID, parent, refs := mime.ParseThreadingHeaders(raw)
	key := rfcID
	if parent != "" {
		key = parent
	}
	if len(refs) > 0 {
		if root := mime.NormalizeMessageID(refs[0]); root != "" {
			key = root
		}
	}
	if err := st.RecordPstEmailHeadersContext(ctx, sourceID, messageID, rfcID, parent, key); err != nil {
		return fmt.Errorf("record PST headers for message %d: %w", messageID, err)
	}
	return nil
}
