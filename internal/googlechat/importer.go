package googlechat

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

// Import replays explicit parts, preserving first-imported snapshots and
// enriching only missing media slots. A failed run can be replayed in full.
func Import(ctx context.Context, st *store.Store, opts ImportOptions) (summary *ImportSummary, retErr error) {
	summary = &ImportSummary{}
	start := time.Now()
	defer func() { summary.Duration = time.Since(start) }()
	owner, err := NormalizeOwner(opts.Me)
	if err != nil {
		return summary, err
	}
	reader, err := openExport(ctx, opts.Roots, owner)
	if err != nil {
		return summary, err
	}
	defer func() { retErr = errors.Join(retErr, reader.Close()) }()
	source, err := st.GetOrCreateSourceContext(ctx, SourceType, owner)
	if err != nil {
		return summary, err
	}
	summary.SourceID = source.ID
	syncID, err := st.StartSyncContext(ctx, source.ID, "import-google-chat")
	if err != nil {
		return summary, err
	}
	st = st.ScopedToSync(source.ID, syncID)
	defer func() {
		// Committed messages need accurate stats and a terminal run status after
		// cancellation; neither cleanup step reads more provider data.
		cleanup := context.WithoutCancel(ctx)
		retErr = errors.Join(retErr, st.RecomputeConversationStatsContext(cleanup, source.ID),
			st.UpdateSyncCheckpointContext(cleanup, syncID, &store.Checkpoint{MessagesProcessed: summary.MessagesAdded + int64(summary.Duplicates+summary.Unsupported), MessagesAdded: summary.MessagesAdded}))
		if retErr != nil {
			retErr = errors.Join(retErr, st.FailSyncContext(cleanup, syncID, retErr.Error()))
		} else {
			retErr = st.CompleteSyncContext(cleanup, syncID, "")
		}
	}()
	resolve := func(p person) (int64, error) {
		email, _ := NormalizeOwner(p.Email)
		if email == "" {
			return 0, nil
		}
		id, ensureErr := st.EnsureParticipantContext(ctx, email, p.Name, strings.SplitN(email, "@", 2)[1])
		if ensureErr != nil {
			return 0, ensureErr
		}
		_, updateErr := st.UpdateParticipantDisplayNameByEmail(ctx, email, p.Name)
		return id, updateErr
	}
	supplied := map[string]bool{}
	for _, group := range reader.groupIDs() {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		hasMessages, err := importGroup(ctx, st, reader, group, owner, source.ID, resolve, opts, summary)
		if err != nil {
			return summary, err
		}
		supplied[group] = hasMessages
	}
	for group := range reader.memberships {
		if !supplied[group] {
			summary.MembershipWithoutMessages++
		}
	}
	return summary, nil
}

func importGroup(ctx context.Context, st *store.Store, reader *exportReader, group, owner string, sourceID int64, resolve func(person) (int64, error), opts ImportOptions, summary *ImportSummary) (bool, error) {
	raw, err := reader.groupFiles(ctx, group, "messages.json")
	if err != nil {
		return false, err
	}
	groupRaw, err := reader.groupFiles(ctx, group, "group_info.json")
	if err != nil {
		return false, err
	}
	var info groupInfo
	if groupRaw != nil {
		if err := json.Unmarshal(groupRaw, &info); err != nil {
			return false, fmt.Errorf("googlechat: group JSON: %w", err)
		}
	}
	kind, err := conversationKind(ctx, st, sourceID, group, len(info.Members))
	if err != nil {
		return false, err
	}
	conversationID, err := st.EnsureConversationWithTypeContext(ctx, sourceID, group, kind, info.Name)
	if err != nil {
		return false, err
	}
	var authors map[int64]bool
	if kind == "chat" {
		ids, err := st.ConversationSenderIDsContext(ctx, conversationID)
		if err != nil {
			return false, err
		}
		authors = make(map[int64]bool, 3)
		for _, id := range ids {
			authors[id] = true
		}
		if len(authors) == 3 {
			kind = "group_chat"
			if _, err := st.EnsureConversationWithTypeContext(ctx, sourceID, group, kind, info.Name); err != nil {
				return false, err
			}
		}
	}
	summary.Conversations++
	for _, member := range info.Members {
		id, err := resolve(member)
		if err != nil {
			return false, err
		}
		if id == 0 {
			summary.UnknownMembers++
			continue
		}
		if err := st.EnsureConversationParticipantContext(ctx, conversationID, id, "member"); err != nil {
			return false, err
		}
	}
	if raw == nil {
		return false, nil
	}
	var envelope struct {
		Messages []jsontext.Value `json:"messages"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return false, fmt.Errorf("googlechat: messages JSON: %w", err)
	}
	if envelope.Messages == nil {
		return false, errors.New("googlechat: messages JSON needs a messages array")
	}
	summary.MessagesSupplied += len(envelope.Messages)
	// Batch lookup stays bounded by one JSON file, without archive-wide ID maps.
	var ids []string
	for _, record := range envelope.Messages {
		var m message
		if json.Unmarshal(record, &m) == nil && m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	existing, err := st.StoredMessagesContext(ctx, sourceID, "google_chat_json", ids)
	if err != nil {
		return false, err
	}
	seen := map[string]jsontext.Value{}
	for _, record := range envelope.Messages {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		var m message
		if err := json.Unmarshal(record, &m); err != nil || m.ID == "" {
			summary.Unsupported++
			continue
		}
		if previous, ok := seen[m.ID]; ok && !bytes.Equal(previous, record) {
			summary.Unsupported++
			continue
		}
		seen[m.ID] = record
		stored := existing[m.ID]
		id := stored.ID
		if id != 0 {
			if stored.ConversationID != conversationID {
				summary.Unsupported++
				continue
			}
			if len(stored.Raw) == 0 {
				return false, fmt.Errorf("googlechat: unsupported or unreadable stored raw format for %q", m.ID)
			}
			// Repair the archived slots using their original provider references.
			// A later edit/reordering must not attach a different file by index.
			original := stored.Raw
			var archived message
			if err := json.Unmarshal(original, &archived); err != nil {
				return false, fmt.Errorf("googlechat: read archived media references: %w", err)
			}
			var mediaSummary ImportSummary
			writes, err := mediaWrites(ctx, st, reader, group, id, &archived, opts, &mediaSummary)
			if err != nil {
				return false, err
			}
			for _, write := range writes {
				if err := st.UpsertAttachmentRecordWithStats(ctx, id, write, true); err != nil {
					return false, err
				}
				summary.AttachmentsStored++
			}
			summary.addMediaOutcomes(mediaSummary)
			summary.Duplicates++
			continue
		}
		sender, err := resolve(m.Creator)
		if err != nil {
			return false, err
		}
		if sender == 0 {
			summary.UnknownSenders++
		}
		sent := parseDate(m.Created)
		if sent.IsZero() {
			summary.DatesUnparsed++
		}
		email, _ := NormalizeOwner(m.Creator.Email)
		var refs []store.ConversationParticipantRef
		var recipients []store.RecipientSet
		if sender != 0 {
			refs = append(refs, store.ConversationParticipantRef{ParticipantID: sender, Role: "member"})
			recipients = append(recipients, store.RecipientSet{Type: "from", ParticipantIDs: []int64{sender}, DisplayNames: []string{m.Creator.Name}})
		}
		metadata, err := json.Marshal(struct {
			Group   string `json:"group_id"`
			Topic   string `json:"topic_id"`
			Created string `json:"created_date"`
		}{group, m.Topic, m.Created})
		if err != nil {
			return false, err
		}
		var mediaSummary ImportSummary
		writes, err := mediaWrites(ctx, st, reader, group, 0, &m, opts, &mediaSummary)
		if err != nil {
			return false, err
		}
		var replacement *[]store.AttachmentWrite
		if len(writes) > 0 {
			replacement = &writes
		}
		snippet := textutil.PrefixRunes(strings.TrimSpace(m.Text), 200)
		id, err = st.PersistMessageContext(ctx, &store.MessagePersistData{
			Message:      &store.Message{SourceID: sourceID, SourceMessageID: m.ID, MessageType: store.MessageTypeGoogleChat, SentAt: sql.NullTime{Time: sent, Valid: !sent.IsZero()}, SenderID: sql.NullInt64{Int64: sender, Valid: sender != 0}, IsFromMe: sender != 0 && email == owner, Snippet: sql.NullString{String: snippet, Valid: snippet != ""}, SizeEstimate: int64(len(record))},
			Conversation: &store.ConversationPersistData{SourceConversationID: group, ConversationType: kind, Title: info.Name, Participants: refs, PreserveExistingParticipants: true},
			RawMIME:      record, RawFormat: "google_chat_json", BodyText: sql.NullString{String: m.Text, Valid: m.Text != ""}, Metadata: &sql.NullString{String: string(metadata), Valid: true}, Recipients: recipients, MIMEAttachmentReplacement: replacement, FTS: &store.FTSDoc{Body: m.Text, FromAddr: email},
		})
		if err != nil {
			return false, fmt.Errorf("googlechat: persist message: %w", err)
		}
		summary.addMediaOutcomes(mediaSummary)
		for _, write := range writes {
			if write.ContentHash != "" {
				summary.AttachmentsStored++
			}
		}
		existing[m.ID] = store.StoredMessage{ID: id, ConversationID: conversationID, Raw: record}
		summary.MessagesAdded++
		if kind == "chat" && sender != 0 {
			authors[sender] = true
			if len(authors) == 3 {
				kind = "group_chat"
				if _, err := st.EnsureConversationWithTypeContext(ctx, sourceID, group, kind, info.Name); err != nil {
					return false, err
				}
			}
		}
	}
	return len(envelope.Messages) > 0, nil
}

// conversationKind classifies a group from its roster size. A sparse roster is
// not evidence of a direct chat, so it keeps an established group_chat.
func conversationKind(ctx context.Context, st *store.Store, sourceID int64, group string, members int) (string, error) {
	if !strings.HasPrefix(group, "DM ") || members > 2 {
		return "group_chat", nil
	}
	prior, err := st.ConversationTypeContext(ctx, sourceID, group)
	if err != nil {
		return "", err
	}
	if prior == "group_chat" {
		return prior, nil
	}
	return "chat", nil
}

func (summary *ImportSummary) addMediaOutcomes(media ImportSummary) {
	summary.AttachmentsMissing += media.AttachmentsMissing
	summary.AttachmentsUnsupported += media.AttachmentsUnsupported
	summary.AttachmentsOversized += media.AttachmentsOversized
}
