package groupme

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

type ImportOptions struct {
	Me             string
	RootDir        string
	AttachmentsDir string
	Logger         *slog.Logger
}

type ImportSummary struct {
	SourceID                                                                          int64
	Duration                                                                          time.Duration
	ChatsProcessed                                                                    int
	MessagesAdded, MessagesSkipped, UnsupportedEntriesSkipped, ServiceMessagesSkipped int64
	AttachmentsStored, AttachmentsMissing, AttachmentsUnsupported                     int
	Oldest, Newest                                                                    time.Time
	Years                                                                             map[int]int64
	Types                                                                             map[string]int64
}

func messageKey(peer, id string) string {
	key, _ := json.Marshal([]string{peer, id})
	return string(key)
}

// Import repeats safely from the beginning, preserving first-imported evidence
// and filling only attachment slots that still lack bytes.
func Import(ctx context.Context, st *store.Store, opts ImportOptions) (summary *ImportSummary, retErr error) {
	started := time.Now()
	summary = &ImportSummary{Years: map[int]int64{}, Types: map[string]int64{}}
	defer func() { summary.Duration = time.Since(started) }()
	if strings.TrimSpace(opts.Me) == "" {
		return summary, errors.New("groupme: --me <account-id> is required")
	}
	absolute, err := filepath.Abs(opts.RootDir)
	if err != nil {
		return summary, err
	}
	root, err := os.OpenRoot(absolute)
	if err != nil {
		return summary, err
	}
	defer func() { retErr = errors.Join(retErr, root.Close()) }()
	source, err := st.GetOrCreateSourceContext(ctx, SourceType, opts.Me)
	if err != nil {
		return summary, err
	}
	summary.SourceID = source.ID
	syncID, err := st.StartSyncContext(ctx, source.ID, "import-groupme")
	if err != nil {
		return summary, err
	}
	st = st.ScopedToSync(source.ID, syncID)
	defer func() {
		// Finalize partial archive statistics and the run even after interruption.
		cleanup := context.WithoutCancel(ctx)
		retErr = errors.Join(retErr, st.ResolveDeferredMessageRepliesContext(cleanup, source.ID, MessageType, 500), st.RecomputeConversationStatsContext(cleanup, source.ID))
		if retErr != nil {
			retErr = errors.Join(retErr, st.FailSyncContext(cleanup, syncID, retErr.Error()))
		} else {
			retErr = st.CompleteSyncContext(cleanup, syncID, "")
		}
	}()
	err = discover(ctx, root, absolute, func(c chat) error {
		if err := importChat(ctx, st, root, absolute, source.ID, c, opts, summary); err != nil {
			return err
		}
		summary.ChatsProcessed++
		return nil
	})
	return summary, err
}

func importChat(ctx context.Context, st *store.Store, root *os.Root, absolute string, sourceID int64, c chat, opts ImportOptions, summary *ImportSummary) error {
	conversationID, err := st.EnsureConversationWithTypeContext(ctx, sourceID, c.key, c.kind, c.metadata.Name)
	if err != nil {
		return err
	}
	names := map[string]string{}
	for _, m := range c.metadata.Members {
		name := m.Nickname
		if name == "" {
			name = m.Name
		}
		names[m.UserID] = name
	}
	resolve := func(id, name string) (int64, error) {
		if id == "" {
			return 0, nil
		}
		if name == "" {
			name = names[id]
		}
		// GroupMe contact-service identifiers are phone numbers. Export account IDs
		// are opaque and deliberately use a separate identifier namespace.
		return st.EnsureParticipantByIdentifierContext(ctx, "groupme_user", id, name)
	}
	var members []store.ConversationParticipantRef
	for _, m := range c.metadata.Members {
		pid, err := resolve(m.UserID, "")
		if err != nil {
			return err
		}
		if pid != 0 {
			members = append(members, store.ConversationParticipantRef{ParticipantID: pid, Role: "member"})
			if err := st.EnsureConversationParticipantContext(ctx, conversationID, pid, "member"); err != nil {
				return err
			}
		}
	}
	return messages(ctx, root, c.folder, func(m *Message, raw jsontext.Value) error {
		kind := m.SenderType
		if kind == "" {
			kind = "message"
		}
		if m.System {
			kind = "system"
		}
		summary.Types[kind]++
		if m.CreatedAt != nil {
			sent := time.Unix(*m.CreatedAt, 0).UTC()
			summary.Years[sent.Year()]++
			if summary.Oldest.IsZero() || sent.Before(summary.Oldest) {
				summary.Oldest = sent
			}
			if summary.Newest.IsZero() || sent.After(summary.Newest) {
				summary.Newest = sent
			}
		}
		if m.isService() {
			summary.ServiceMessagesSkipped++
			return nil
		}
		if m.ID == "" {
			summary.UnsupportedEntriesSkipped++
			return nil
		}
		key := messageKey(c.key, m.ID)
		existing, err := st.MessageExistsBatchContext(ctx, sourceID, []string{key})
		if err != nil {
			return err
		}
		id := existing[key]
		writes, err := mediaWrites(ctx, st, root, c.folder, id, m, opts, summary)
		if err != nil {
			return err
		}
		if id != 0 {
			for _, write := range writes {
				if err := st.UpsertAttachmentRecordWithStats(ctx, id, write, true); err != nil {
					return err
				}
			}
			summary.MessagesSkipped++
			return nil
		}
		senderID := m.SenderID
		sender, err := resolve(senderID, m.Name)
		if err != nil {
			return err
		}
		participants := append([]store.ConversationParticipantRef(nil), members...)
		recipients := []store.RecipientSet{}
		if sender != 0 {
			participants = append(participants, store.ConversationParticipantRef{ParticipantID: sender, Role: "member"})
			recipients = append(recipients, store.RecipientSet{Type: "from", ParticipantIDs: []int64{sender}, DisplayNames: []string{m.Name}})
		}
		metadata := struct {
			ExportRoot          string `json:"export_root"`
			Folder              string `json:"export_folder"`
			Timestamp           *int64 `json:"created_at"`
			Timezone            string `json:"original_timezone"`
			ReferencedMessageID string `json:"referenced_message_id,omitempty"`
		}{ExportRoot: absolute, Folder: c.folder, Timestamp: m.CreatedAt, Timezone: "unknown"}
		for _, a := range m.Attachments {
			if a.Type == "reply" && a.ReplyID != "" {
				metadata.ReferencedMessageID = messageKey(c.key, a.ReplyID)
			}
		}
		encoded, err := json.Marshal(metadata)
		if err != nil {
			return err
		}
		sent := sql.NullTime{}
		if m.CreatedAt != nil {
			sent = sql.NullTime{Time: time.Unix(*m.CreatedAt, 0).UTC(), Valid: true}
		}
		var replacement *[]store.AttachmentWrite
		if len(writes) > 0 {
			replacement = &writes
		}
		snippet := textutil.PrefixRunes(strings.TrimSpace(m.Text), 200)
		_, err = st.PersistMessageContext(ctx, &store.MessagePersistData{
			Message:      &store.Message{SourceID: sourceID, SourceMessageID: key, MessageType: MessageType, SentAt: sent, SenderID: sql.NullInt64{Int64: sender, Valid: sender != 0}, IsFromMe: senderID != "" && senderID == opts.Me, Snippet: sql.NullString{String: snippet, Valid: snippet != ""}, SizeEstimate: int64(len(raw))},
			Conversation: &store.ConversationPersistData{SourceConversationID: c.key, ConversationType: c.kind, Title: c.metadata.Name, Participants: participants, PreserveExistingParticipants: true},
			Metadata:     &sql.NullString{String: string(encoded), Valid: true}, BodyText: sql.NullString{String: m.Text, Valid: m.Text != ""}, RawMIME: raw, RawFormat: "groupme_json", Recipients: recipients, MIMEAttachmentReplacement: replacement, FTS: &store.FTSDoc{Body: m.Text},
		})
		if err != nil {
			return fmt.Errorf("groupme: persist message: %w", err)
		}
		summary.MessagesAdded++
		if opts.Logger != nil {
			opts.Logger.Debug("groupme: message imported")
		}
		return nil
	})
}
