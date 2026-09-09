package discord

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"go.kenn.io/msgvault/internal/rederive"
	"go.kenn.io/msgvault/internal/store"
)

const discordRederiveVersion = "v1"

const discordRepairBatchSize = 500

func init() {
	rederive.Register(sourceTypeDiscord, discordRederiveVersion,
		func(ctx context.Context, s *store.Store, sourceID int64, progress func(string)) (*rederive.Summary, error) {
			return NewImporter(s, nil).RepairSource(ctx, sourceID, progress)
		})
}

// RepairSource derives Discord metadata from archived JSON without contacting
// Discord or changing message content and stored media.
func (imp *Importer) RepairSource(
	ctx context.Context, sourceID int64, progress func(string),
) (*rederive.Summary, error) {
	start := time.Now()
	sum := &rederive.Summary{}
	var afterID int64
	for {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		batch, err := imp.store.ScanArchivedRawMessages(
			sourceID, discordRawFormat, afterID, discordRepairBatchSize,
		)
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
			progress(fmt.Sprintf("%d scanned, %d attachments tagged",
				sum.MessagesScanned, sum.AttachmentsTagged))
		}
	}
	sum.Duration = time.Since(start)
	return sum, nil
}

func (imp *Importer) repairMessage(
	item *store.ArchivedRawMessage, sourceID int64, sum *rederive.Summary,
) {
	var message Message
	if err := json.Unmarshal(item.RawData, &message); err != nil {
		sum.Undecodable++
		return
	}

	mapped, err := mapMessage(&message, item.ConversationID, sourceID)
	if err != nil {
		sum.Errors++
		return
	}
	if imp.messageMetadataChanged(item.MessageID, mapped.Metadata, sum) {
		return
	}

	attachmentMetadata := make(map[string]string, len(mapped.Attachments))
	if message.Flags&discordVoiceMessageFlag != 0 {
		for _, attachment := range mapped.Attachments {
			attachmentMetadata[attachment.SourceAttachmentID] = attachment.Metadata
		}
	}
	changed, err := imp.store.SetDiscordAttachmentMetadata(item.MessageID, attachmentMetadata)
	if err != nil {
		sum.Errors++
		return
	}
	sum.AttachmentsTagged += changed
}

func (imp *Importer) messageMetadataChanged(
	messageID int64, wanted json.RawMessage, sum *rederive.Summary,
) bool {
	current, err := imp.store.GetMessageMetadata(messageID)
	if err != nil {
		sum.Errors++
		return true
	}
	if current.Valid && jsonValuesEqual([]byte(current.String), wanted) {
		return false
	}
	if err := imp.store.SetMessageMetadata(messageID, sql.NullString{
		String: string(wanted), Valid: len(wanted) > 0,
	}); err != nil {
		sum.Errors++
		return true
	}
	sum.MessageMetadataRewritten++
	return false
}

func jsonValuesEqual(left, right []byte) bool {
	var leftValue, rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}
