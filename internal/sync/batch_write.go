package sync

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/gmail"
)

// ingestFetchedBatch runs ingest, the persistence loop of one fetched Gmail
// batch, in one source write transaction.
//
// Each message used to commit on its own, and several bookkeeping writes
// around it committed separately again. On a WAL database every commit
// rewrites every b-tree page it touched (message, body, raw, recipient,
// label, FTS and change-journal indexes) and fsyncs, so a full sync wrote
// hundreds of kilobytes per message. One transaction per batch writes each
// touched page once per batch. ingest opens a savepoint per message, so a
// message that fails still leaves no writes behind.
//
// The transaction must not wait on the network or take the exclusive
// identity lock. IMAP validation and remote image archiving do network work
// per message, so those syncs keep one transaction per write. Relay authors
// are participants created under the exclusive lock; prepareBatch creates
// them first.
func (s *Syncer) ingestFetchedBatch(
	ctx context.Context, sourceID int64, ids []string, raws []gmail.RawMessageBatchResult,
	threadIDs map[string]string, ingest func(context.Context) error,
) error {
	if (s.opts.SourceType != "" && s.opts.SourceType != sourceTypeGmail) || s.opts.RemoteImages != nil {
		return ingest(ctx)
	}
	defer clear(s.prepared)
	if err := s.prepareBatch(ctx, sourceID, ids, raws, threadIDs); err != nil {
		return err
	}
	// A message's committed writes survive an error that stops the sync, as
	// they did when each message committed on its own: commit the work done
	// before the error, then report it.
	var stopErr error
	err := s.store.WithSourceWriteTxContext(ctx, sourceID, func(ctx context.Context) error {
		s.batchCtx = ctx
		defer func() { s.batchCtx = nil }()
		stopErr = ingest(ctx)
		return nil
	})
	if err != nil {
		return err
	}
	return stopErr
}

// prepareBatch parses the batch's messages for ingestMessage and creates
// their relay authors before the write transaction opens. A message that
// fails to parse is left for ingestMessage, which reports the error.
func (s *Syncer) prepareBatch(
	ctx context.Context, sourceID int64, ids []string, raws []gmail.RawMessageBatchResult,
	threadIDs map[string]string,
) error {
	if s.prepared == nil {
		s.prepared = make(map[*gmail.RawMessage]*messageData)
	}
	for i := range raws {
		raw := raws[i].Message
		if raws[i].Err != nil || raw == nil || i >= len(ids) {
			continue
		}
		data, err := s.prepareMessage(sourceID, raw, threadIDs[ids[i]], false)
		if err != nil {
			continue
		}
		s.prepared[raw] = data
		if data.relayType == "" {
			continue
		}
		if _, err := s.ensureRelayAuthor(ctx, data); err != nil {
			return fmt.Errorf("ensure relay author: %w", err)
		}
	}
	return nil
}

// ensureRelayAuthor returns the participant of a relayed message's author,
// creating it once per sync.
func (s *Syncer) ensureRelayAuthor(ctx context.Context, data *messageData) (int64, error) {
	key := data.relayType + "\x00" + data.relayValue
	if id, ok := s.relayAuthors[key]; ok {
		return id, nil
	}
	id, err := s.store.EnsureParticipantByIdentifierContext(ctx, data.relayType, data.relayValue, strings.TrimSpace(data.from[0].Name))
	if err != nil {
		return 0, err
	}
	if s.relayAuthors == nil {
		s.relayAuthors = make(map[string]int64)
	}
	s.relayAuthors[key] = id
	return id, nil
}

// writeContext is the context for bookkeeping writes that have no request
// context of their own: the open batch transaction, so they join it instead
// of waiting for its write lock, or the background context.
func (s *Syncer) writeContext() context.Context {
	if s.batchCtx != nil {
		return s.batchCtx
	}
	return context.Background()
}
