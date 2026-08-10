package beeper

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/rederive"
	"go.kenn.io/msgvault/internal/store"
)

// rawArchiveFormat is the message_raw.raw_format tag Beeper messages archive
// under; the repair reads back exactly what the importer wrote.
const rawArchiveFormat = "beeper_json"

// rederiveVersion identifies this package's derivation logic. Bump it whenever
// a change would produce different body text, snippets or attachment metadata
// for the same payload, so existing archives re-derive on their next sync.
//
//	v1 — plain-text conversion of HTML message text; link-preview classification.
//	v2 — refresh snippets and the search index even when body text is current.
//	v3 — exclude Matrix reply fallbacks from body text, snippets, and search.
const rederiveVersion = "v3"

// repairBatchSize bounds how many archived messages are held in memory per
// pass of the walk.
const repairBatchSize = 500

func init() {
	rederive.Register(sourceTypeBeeper, rederiveVersion,
		func(ctx context.Context, s *store.Store, sourceID int64, progress func(string)) (*rederive.Summary, error) {
			// The pass never calls Beeper, so a nil client is correct here.
			return NewImporter(s, nil).RepairSource(ctx, sourceID, progress)
		})
}

// RepairSource re-derives stored message text and attachment classification for
// one Beeper source from the verbatim JSON archived alongside each message.
//
// Both are computed from the API payload at import time, so changing how they
// are derived leaves already-archived rows stale. Reading the archive rather
// than re-fetching keeps the pass offline, fast, and able to repair messages
// Beeper Desktop no longer holds — a re-sync could do neither.
//
// Only derived columns are rewritten: message bodies, snippets, the search
// index, and attachment metadata. Raw payloads, stored media, and sync cursors
// are left alone, so the pass is idempotent and safe to re-run. It touches no
// Beeper endpoint, so an Importer built with a nil client is valid.
func (imp *Importer) RepairSource(ctx context.Context, sourceID int64, progress func(string)) (*rederive.Summary, error) {
	start := time.Now()
	sum := &rederive.Summary{}

	var afterID int64
	for {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		batch, err := imp.store.ScanArchivedRawMessages(sourceID, rawArchiveFormat, afterID, repairBatchSize)
		if err != nil {
			return sum, err
		}
		if len(batch) == 0 {
			break
		}
		for i := range batch {
			if err := ctx.Err(); err != nil {
				return sum, err
			}
			item := &batch[i]
			afterID = item.MessageID
			sum.MessagesScanned++
			imp.repairMessage(item, sourceID, sum)
		}
		if progress != nil {
			progress(fmt.Sprintf("%d scanned, %d bodies rewritten, %d attachments tagged",
				sum.MessagesScanned, sum.BodiesRewritten, sum.AttachmentsTagged))
		}
	}
	sum.Duration = time.Since(start)
	return sum, nil
}

// repairMessage re-derives one archived message's stored text and attachment
// classification. Per-message failures are counted rather than fatal: one
// unreadable payload must not abandon the rest of the archive.
func (imp *Importer) repairMessage(item *store.ArchivedRawMessage, sourceID int64, sum *rederive.Summary) {
	var m Message
	if err := json.Unmarshal(item.RawData, &m); err != nil {
		sum.Undecodable++
		return
	}
	// Reactions, tombstones, and hidden events never had a body of their own;
	// the importer skips them at persist time, so there is nothing to re-derive.
	if !persistsMessageRow(&m) {
		return
	}

	// Re-run the production mapping so repaired rows are exactly what a fresh
	// sync would now write.
	msg, text := mapMessage(&m, item.ConversationID, sourceID)

	// Refresh the whole derived-text tuple even when the body is current.
	// Snippet and FTS state can drift independently, and the store commits all
	// three fields atomically so a failure leaves the row retryable.
	bodyChanged := item.BodyText != text
	if err := imp.store.UpdateMessageDerivedText(
		item.MessageID,
		sql.NullString{String: text, Valid: text != ""},
		sql.NullString{},
		msg.Snippet,
		store.FTSDoc{Body: text, FromAddr: m.SenderName},
	); err != nil {
		sum.Errors++
		return
	}
	if bodyChanged {
		sum.BodiesRewritten++
	}

	if len(m.Attachments) == 0 {
		return
	}
	changed, err := imp.store.SetBeeperAttachmentMetadata(item.MessageID, shareMetadata(&m))
	if err != nil {
		sum.Errors++
		return
	}
	sum.AttachmentsTagged += changed
}
