package importer

import (
	"context"
	"fmt"

	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

func recordPstMessageHeaders(ctx context.Context, st *store.Store, sourceID int64, sourceMsgID string, raw []byte) error {
	ids, err := st.MessageExistsBatch(sourceID, []string{sourceMsgID})
	if err != nil {
		return fmt.Errorf("find imported PST message: %w", err)
	}
	id, ok := ids[sourceMsgID]
	if !ok {
		return fmt.Errorf("imported PST message missing from source %d", sourceID)
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
	return st.RecordPstEmailHeadersContext(ctx, sourceID, messageID, rfcID, parent, key)
}
