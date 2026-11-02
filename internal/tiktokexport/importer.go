package tiktokexport

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/store"
)

// SourceType keeps local export identities independent of API integrations.
const SourceType = "tiktok_export"
const identifierType = "tiktok_export"

// Summary reports successful writes and ignored archive sections.
type Summary struct {
	SourceID          int64 `json:"source_id"`
	MessagesAdded     int64 `json:"messages_added"`
	MessagesProcessed int64 `json:"messages_processed"`
	Conversations     int64 `json:"conversations"`
	FromMe            int64 `json:"from_me"`
	UnknownSections   int   `json:"unknown_sections"`
}

// Import reads and validates the archive before creating a source. Re-running
// after cancellation is safe: message identities do not depend on record order.
func Import(ctx context.Context, st *store.Store, identifier, filename string) (summary *Summary, retErr error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return nil, errors.New("source identifier is required")
	}
	data, err := readArchive(ctx, filename)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	source, err := st.GetOrCreateSourceContext(ctx, SourceType, identifier)
	if err != nil {
		return nil, fmt.Errorf("create TikTok source: %w", err)
	}
	syncID, err := st.StartSyncContext(ctx, source.ID, "import tiktok")
	if err != nil {
		return nil, fmt.Errorf("start TikTok import: %w", err)
	}
	st = st.ScopedToSync(source.ID, syncID)
	progress := &Summary{SourceID: source.ID, UnknownSections: data.UnknownSections}
	summary = progress
	defer func() {
		checkpoint := &store.Checkpoint{MessagesProcessed: progress.MessagesProcessed, MessagesAdded: progress.MessagesAdded}
		if retErr != nil {
			checkpoint.ErrorsCount = 1
			retErr = errors.Join(retErr, st.FailSyncWithCheckpoint(syncID, retErr.Error(), checkpoint))
			return
		}
		if err := st.UpdateSyncCheckpoint(syncID, checkpoint); err != nil {
			retErr = errors.Join(fmt.Errorf("record import counts: %w", err), st.FailSync(syncID, err.Error()))
			return
		}
		if err := st.CompleteSync(syncID, ""); err != nil {
			retErr = fmt.Errorf("complete TikTok import: %w", err)
		}
	}()

	// Scope names to this source: an export exposes mutable usernames, not
	// stable user IDs, so matching another account's display name is insufficient.
	actorKey := func(name string) string { return "tiktok-export:" + digest(identifier, username(name)) }
	selfKey := actorKey(data.Username)
	identities, err := st.ListAccountIdentities(source.ID)
	if err != nil {
		return nil, fmt.Errorf("read account identity: %w", err)
	}
	for _, identity := range identities {
		if slices.Contains(strings.Split(identity.SourceSignal, ","), "tiktok-export-profile") && identity.Address != selfKey {
			return nil, errors.New("source belongs to a different profile; use another identifier")
		}
	}
	selfName := data.Nickname
	if selfName == "" {
		selfName = data.Username
	}
	self, err := st.EnsureParticipantByIdentifierContext(ctx, identifierType, selfKey, selfName)
	if err != nil {
		return nil, fmt.Errorf("resolve account identity: %w", err)
	}
	if err := st.AddAccountIdentityContext(ctx, source.ID, selfKey, "tiktok-export-profile"); err != nil {
		return nil, fmt.Errorf("record account identity: %w", err)
	}
	actors := map[string]int64{data.Username: self}
	resolve := func(name string) (int64, error) {
		name = username(name)
		if id, ok := actors[name]; ok {
			return id, nil
		}
		id, err := st.EnsureParticipantByIdentifierContext(ctx, identifierType, actorKey(name), name)
		if err == nil {
			actors[name] = id
		}
		return id, err
	}
	conversations := map[string]bool{}
	seen := map[string]bool{}
	for _, m := range data.Messages {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key := m.key()
		if seen[key] {
			continue
		}
		seen[key] = true
		sender, err := resolve(m.Sender)
		if err != nil {
			return nil, fmt.Errorf("resolve sender: %w", err)
		}
		fromMe := username(m.Sender) == data.Username
		participants := []store.ConversationParticipantRef{{ParticipantID: self, Role: "member"}}
		recipients := []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{sender}, DisplayNames: []string{m.Sender}}}
		convType, title := "public_comments", m.Link
		if !m.Comment {
			convType, title = "direct_chat", m.Chat
			partner, err := resolve(m.Chat)
			if err != nil {
				return nil, fmt.Errorf("resolve chat partner: %w", err)
			}
			participants = append(participants, store.ConversationParticipantRef{ParticipantID: partner, Role: "member"})
			if sender != self && sender != partner {
				return nil, errors.New("chat sender does not match profile or chat partner")
			}
			to, toName := self, data.Username
			if fromMe {
				to, toName = partner, m.Chat
			}
			recipients = append(recipients, store.RecipientSet{Type: "to", ParticipantIDs: []int64{to}, DisplayNames: []string{toName}})
		}
		prior, err := st.StoredMessagesContext(ctx, source.ID, m.Format, []string{key})
		if err != nil {
			return nil, fmt.Errorf("check existing message: %w", err)
		}
		body := m.body()
		snippet := body
		if utf8.RuneCountInString(snippet) > 200 {
			snippet = string([]rune(snippet)[:200])
		}
		meta, err := json.Marshal(struct {
			Username string `json:"account_username"`
			PostURL  string `json:"post_url,omitempty"`
		}{data.Username, m.Link})
		if err != nil {
			return nil, err
		}
		_, err = st.PersistMessageContext(ctx, &store.MessagePersistData{
			Message:      &store.Message{SourceID: source.ID, SourceMessageID: key, MessageType: SourceType, SentAt: sql.NullTime{Time: m.Date, Valid: true}, SenderID: sql.NullInt64{Int64: sender, Valid: true}, IsFromMe: fromMe, Snippet: sql.NullString{String: snippet, Valid: true}, SizeEstimate: int64(len(body))},
			Conversation: &store.ConversationPersistData{SourceConversationID: m.conversation(), ConversationType: convType, Title: title, Participants: participants, PreserveExistingParticipants: true},
			Metadata:     &sql.NullString{String: string(meta), Valid: true}, BodyText: sql.NullString{String: body, Valid: true}, RawMIME: m.Raw, RawFormat: m.Format, Recipients: recipients, PreserveLabels: true,
			FTS: &store.FTSDoc{Body: body, FromAddr: m.Sender},
		})
		if err != nil {
			return nil, fmt.Errorf("persist TikTok message: %w", err)
		}
		summary.MessagesProcessed++
		if _, exists := prior[key]; !exists {
			summary.MessagesAdded++
		}
		if fromMe {
			summary.FromMe++
		}
		conversations[m.conversation()] = true
	}
	summary.Conversations = int64(len(conversations))
	return summary, nil
}
