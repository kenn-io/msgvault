package whatsapp

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

var textOwnerPhone = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)

// validateTextOptions checks caller assumptions without opening the archive.
func validateTextOptions(opts TextOptions) (*time.Location, error) {
	if !textOwnerPhone.MatchString(opts.Phone) {
		return nil, errors.New("--phone must be an E.164 number")
	}
	if strings.TrimSpace(opts.ChatID) == "" {
		return nil, errors.New("--chat-id is required and must stay stable across exports")
	}
	if opts.ChatType != "direct" && opts.ChatType != "group" {
		return nil, errors.New("--chat-type must be direct or group")
	}
	return textLocation(opts.DateOrder, opts.Timezone)
}

type textAssumptions struct {
	Timezone            string `json:"timezone_assumption"`
	ChatType            string `json:"chat_type"`
	AmbiguousTimePolicy string `json:"ambiguous_time_policy"`
}

func textAssumptionsFor(opts TextOptions) textAssumptions {
	return textAssumptions{opts.Timezone, opts.ChatType, "earlier"}
}

type textProvenance struct {
	textRecord
	textAssumptions

	DateOrder  string `json:"date_order_assumption"`
	FileSHA256 string `json:"file_sha256"`
	ChatID     string `json:"chat_id"`
	Direction  string `json:"direction"`
	Occurrence int    `json:"occurrence"`
}

// LoadText checks and parses a complete TXT before opening an archive.
func LoadText(ctx context.Context, path string, opts TextOptions) (*TextInput, error) {
	loc, err := validateTextOptions(opts)
	if err != nil {
		return nil, err
	}
	if opts.MediaDir != "" && opts.AttachmentsDir == "" {
		return nil, errors.New("attachment storage directory is required with media")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("open TXT: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("TXT input must be a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open TXT: %w", err)
	}
	rows, digest, err := parseText(ctx, f, opts.DateOrder, loc, textLimitsDefault)
	closeErr := f.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return &TextInput{opts: opts, rows: rows, digest: digest}, nil
}

// ImportText persists a checked TXT. Committed records make interruption repeatable.
func (imp *Importer) ImportText(ctx context.Context, input *TextInput) (summary TextSummary, retErr error) {
	opts, rows, digest := input.opts, input.rows, input.digest
	var err error
	var root *os.Root
	if opts.MediaDir != "" {
		root, err = os.OpenRoot(opts.MediaDir)
		if err != nil {
			return summary, fmt.Errorf("open media directory: %w", err)
		}
		defer func() { _ = root.Close() }()
	}
	source, err := imp.store.GetOrCreateSourceContext(ctx, "whatsapp", opts.Phone)
	if err != nil {
		return summary, err
	}
	summary.SourceID = source.ID
	syncID, err := imp.store.StartSyncContext(ctx, source.ID, "full")
	if err != nil {
		return summary, err
	}
	st := imp.store.ScopedToSync(source.ID, syncID)
	var convID int64
	defer func() {
		// Partial messages remain committed after cancellation and need current counts.
		post := context.WithoutCancel(ctx)
		if convID != 0 {
			retErr = errors.Join(retErr, st.RecomputeConversationStatsForConversationContext(post, convID))
		}
		if retErr == nil {
			retErr = imp.store.CompleteSyncContext(ctx, syncID, "")
		}
		if retErr != nil {
			retErr = errors.Join(retErr, imp.store.FailSyncWithCheckpointContext(post, syncID, retErr.Error(), &store.Checkpoint{MessagesProcessed: int64(summary.Records), MessagesAdded: int64(summary.Added)}))
		}
	}()
	convID, err = prepareTextChat(ctx, st, source.ID, opts)
	if err != nil {
		return summary, err
	}
	counts := make(map[string]int)
	ids := make([]string, len(rows))
	occurrences := make([]int, len(rows))
	for i, row := range rows {
		base := textMessageKey(opts.ChatID, row, 0)
		occurrences[i] = counts[base]
		ids[i] = textMessageKey(opts.ChatID, row, counts[base])
		counts[base]++
	}
	participants := make(map[string]int64)
	for start := 0; start < len(rows); start += 500 {
		end := min(start+500, len(rows))
		existing, err := st.MessageMetadataBatch(ctx, source.ID, ids[start:end])
		if err != nil {
			return summary, err
		}
		for i := start; i < end; i++ {
			row := rows[i]
			messageID := existing[ids[i]].ID
			if messageID == 0 {
				var sender sql.NullInt64
				if row.Sender != "" {
					pid, ok := participants[row.Sender]
					if !ok {
						pid, err = st.EnsureParticipantByIdentifierContext(ctx, "whatsapp_txt", textHash(opts.Phone, opts.ChatID, row.Sender), row.Sender)
						if err != nil {
							return summary, err
						}
						participants[row.Sender] = pid
					}
					sender = sql.NullInt64{Int64: pid, Valid: true}
				}
				var members []store.ConversationParticipantRef
				if sender.Valid {
					members = []store.ConversationParticipantRef{{ParticipantID: sender.Int64, Role: "member"}}
				}
				raw, err := json.Marshal(textProvenance{row, textAssumptionsFor(opts), opts.DateOrder, digest, opts.ChatID, "unknown", occurrences[i]})
				if err != nil {
					return summary, err
				}
				messageID, err = st.PersistMessageContext(ctx, &store.MessagePersistData{
					Message:      &store.Message{SourceID: source.ID, ConversationID: convID, SourceMessageID: ids[i], MessageType: "whatsapp", SentAt: sql.NullTime{Time: row.SentAt, Valid: true}, SenderID: sender, Snippet: sql.NullString{String: row.Body, Valid: true}, SizeEstimate: int64(len(row.Body)), PreserveAttachmentStats: true},
					Conversation: &store.ConversationPersistData{SourceConversationID: textChatKey(opts.ChatID), ConversationType: map[bool]string{true: "group_chat", false: "direct_chat"}[opts.ChatType == "group"], Title: opts.ChatTitle, Participants: members, PreserveExistingParticipants: true},
					BodyText:     sql.NullString{String: row.Body, Valid: true}, RawMIME: raw, RawFormat: "whatsapp_txt", PreserveLabels: true,
					FTS: &store.FTSDoc{Body: row.Body, FromAddr: row.Sender},
				})
				if err != nil {
					return summary, fmt.Errorf("line %d: persist TXT: %w", row.StartLine, err)
				}
				summary.Added++
			} else {
				summary.Unchanged++
			}
			summary.Records++
			if row.Kind == "notice" {
				summary.Notices++
			}
			if occurrences[i] > 0 {
				summary.DuplicateAmbiguity++
			}
			if err := importTextMedia(ctx, st, root, messageID, row.Body, opts, &summary); err != nil {
				return summary, fmt.Errorf("line %d: import media: %w", row.StartLine, err)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return summary, err
	}
	if err := st.UpdateSyncCheckpoint(syncID, &store.Checkpoint{MessagesProcessed: int64(summary.Records), MessagesAdded: int64(summary.Added)}); err != nil {
		return summary, err
	}
	return summary, nil
}

func prepareTextChat(ctx context.Context, st *store.Store, sourceID int64, opts TextOptions) (int64, error) {
	key := textChatKey(opts.ChatID)
	existing, err := st.ConversationMetadataBatchContext(ctx, sourceID, []string{key})
	if err != nil {
		return 0, err
	}
	metadata := map[string]jsontext.Value{}
	if value := existing[key]; value.Valid && value.String != "" {
		if err := json.Unmarshal([]byte(value.String), &metadata); err != nil {
			return 0, fmt.Errorf("read TXT chat assumptions: %w", err)
		}
	}
	want := textAssumptionsFor(opts)
	if raw, ok := metadata["whatsapp_txt"]; ok {
		var got textAssumptions
		if err := json.Unmarshal(raw, &got, json.RejectUnknownMembers(true)); err != nil {
			return 0, fmt.Errorf("upgrade msgvault to import this chat: %w", err)
		}
		if got != want {
			return 0, errors.New("TXT chat parsing assumptions conflict; reuse the original timezone and chat type")
		}
	}
	raw, err := json.Marshal(want)
	if err != nil {
		return 0, err
	}
	metadata["whatsapp_txt"] = raw
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return 0, err
	}
	kind := "direct_chat"
	if opts.ChatType == "group" {
		kind = "group_chat"
	}
	id, err := st.EnsureConversationWithTypeContext(ctx, sourceID, key, kind, opts.ChatTitle)
	if err != nil {
		return 0, err
	}
	if err := st.SetConversationMetadataContext(ctx, id, sql.NullString{String: string(encoded), Valid: true}); err != nil {
		return 0, err
	}
	return id, nil
}
