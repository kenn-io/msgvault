package discord

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

// PackageSummary describes actual committed snapshots, including partial runs.
type PackageSummary struct {
	SourceID                   int64
	Channels                   int
	MessagesProcessed          int64
	MessagesAdded              int64
	MessagesUpdated            int64
	MessagesSkipped            int64
	SkipReasons                map[string]int64
	AttachmentLinks            int64
	UnsupportedAttachmentLinks int64
}

// ImportPackage imports only the exporting account's sent messages. Replay
// replaces the last imported snapshot; absence never implies deletion.
func ImportPackage(ctx context.Context, st *store.Store, directory string) (summary *PackageSummary, retErr error) {
	summary = &PackageSummary{SkipReasons: map[string]int64{}}
	reader, err := openPackage(ctx, directory)
	if err != nil {
		return summary, err
	}
	defer func() { retErr = errors.Join(retErr, reader.root.Close()) }()
	source, err := st.GetOrCreateSourceContext(ctx, "discord_package", reader.owner)
	if err != nil {
		return summary, err
	}
	summary.SourceID = source.ID
	syncID, err := st.StartSyncContext(ctx, source.ID, "import-discord-package")
	if err != nil {
		return summary, err
	}
	st = st.ScopedToSync(source.ID, syncID)
	defer func() {
		// Run status and statistics must describe already committed rows even when
		// the caller cancels the import.
		cleanup := context.WithoutCancel(ctx)
		retErr = errors.Join(retErr, st.RecomputeConversationStatsContext(cleanup, source.ID))
		if retErr != nil {
			retErr = errors.Join(retErr, st.FailSyncContext(cleanup, syncID, retErr.Error()))
		} else {
			retErr = st.CompleteSyncContext(cleanup, syncID, "")
		}
	}()
	sender, err := st.EnsureParticipantByIdentifierContext(ctx, "discord_user_id", reader.owner, "")
	if err != nil {
		return summary, err
	}
	for _, folder := range reader.channels {
		channelID := strings.TrimPrefix(folder, "c")
		metadata := map[string]jsontext.Value{}
		blob, err := reader.readMetadata(ctx, path.Join(reader.messages, folder, "channel.json"))
		if err == nil {
			if err = json.Unmarshal(blob, &metadata); err != nil {
				return summary, fmt.Errorf("Discord package channel %s: %w", channelID, err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return summary, err
		}
		kind, title := packageConversation(metadata)
		if title == "" {
			title = reader.titles[channelID]
		}
		conversationID, err := st.EnsureConversationWithTypeContext(ctx, source.ID, channelID, kind, title)
		if err != nil {
			return summary, err
		}
		channelMeta, err := json.Marshal(map[string]any{"coverage": "sent_only", "channel": metadata})
		if err != nil {
			return summary, err
		}
		if err = st.SetConversationMetadataContext(ctx, conversationID, sql.NullString{String: string(channelMeta), Valid: true}); err != nil {
			return summary, err
		}
		summary.Channels++
		err = reader.transcript(ctx, folder, func(record packageRecord) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			summary.MessagesProcessed++
			writes := []store.AttachmentWrite{}
			unsupported := int64(0)
			for i, link := range record.Attachments {
				parsed, err := url.Parse(link)
				if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
					unsupported++
					continue
				}
				writes = append(writes, store.AttachmentWrite{StoragePath: link, Filename: path.Base(parsed.Path), MediaType: "link", SourcePartKey: fmt.Sprintf("url:%d", i), Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceProviderExplicit})
			}
			summary.UnsupportedAttachmentLinks += unsupported
			reason := ""
			switch {
			case record.Invalid:
				reason = "malformed_record"
			case strings.TrimSpace(record.ID) == "":
				reason = "missing_id"
			case record.Contents == "" && len(record.Attachments) == 0:
				reason = "empty_record"
			case record.Contents == "" && len(writes) == 0:
				reason = "unsupported_attachment_only"
			}
			if reason != "" {
				summary.MessagesSkipped++
				summary.SkipReasons[reason]++
				return st.RecordSyncRunItemContext(ctx, store.SyncRunItem{SyncRunID: syncID, SourceMessageID: record.ID, Phase: record.Path, Status: store.SyncRunItemStatusSkipped, ErrorKind: reason, ErrorMessage: fmt.Sprintf("record %d", record.Ordinal)})
			}
			sent, timeSource := packageSentTime(record.Timestamp, record.ID)
			messageMeta, err := json.Marshal(map[string]any{
				"coverage": "sent_only", "account_id": reader.owner, "original_timestamp": record.Timestamp,
				"time_source": timeSource, "format": record.Format, "input_path": record.Path, "row_ordinal": record.Ordinal,
			})
			if err != nil {
				return err
			}
			existing, err := st.MessageExistsBatchContext(ctx, source.ID, []string{record.ID})
			if err != nil {
				return err
			}
			_, err = st.PersistMessageContext(ctx, &store.MessagePersistData{
				Message:  &store.Message{SourceID: source.ID, ConversationID: conversationID, SourceMessageID: record.ID, MessageType: "discord", SenderID: sql.NullInt64{Int64: sender, Valid: true}, IsFromMe: true, SentAt: sent, Snippet: sql.NullString{String: textutil.PrefixRunes(record.Contents, 200), Valid: record.Contents != ""}, SizeEstimate: int64(len(record.Contents))},
				BodyText: sql.NullString{String: record.Contents, Valid: true}, Metadata: &sql.NullString{String: string(messageMeta), Valid: true}, RawMIME: record.Raw, RawFormat: "discord_package_" + record.Format,
				Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{sender}}}, MIMEAttachmentReplacement: &writes, FTS: &store.FTSDoc{Body: record.Contents}, PreserveLabels: true,
				SyncRunItem: &store.SyncRunItem{SyncRunID: syncID, SourceMessageID: record.ID, Phase: record.Path, Status: store.SyncRunItemStatusResolved},
			})
			if err != nil {
				return fmt.Errorf("persist Discord package record %d: %w", record.Ordinal, err)
			}
			if _, ok := existing[record.ID]; ok {
				summary.MessagesUpdated++
			} else {
				summary.MessagesAdded++
			}
			summary.AttachmentLinks += int64(len(writes))
			return nil
		})
		if err != nil {
			return summary, err
		}
	}
	return summary, nil
}

func packageConversation(metadata map[string]jsontext.Value) (kind, title string) {
	_ = json.Unmarshal(metadata["name"], &title)
	value := packageScalarID(metadata["type"])
	switch value {
	case "1", "DM":
		return "direct_chat", title
	case "3", "GROUP_DM":
		return "group_chat", title
	default:
		return "channel", title
	}
}

func packageSentTime(original, id string) (sql.NullTime, string) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999Z07:00"} {
		if stamp, err := time.Parse(layout, original); err == nil {
			return sql.NullTime{Time: stamp, Valid: true}, "timestamp"
		}
	}
	if value, err := ParseSnowflake(id); err == nil && value != 0 {
		stamp, err := TimestampFromSnowflake(id)
		if err == nil {
			return sql.NullTime{Time: stamp, Valid: true}, "snowflake"
		}
	}
	return sql.NullTime{}, "unknown"
}
