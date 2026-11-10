package tumblr

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

func normalizeBlog(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
func participantKey(blog string, p Participant) string {
	if p.ID != "" {
		return "id:" + p.ID
	}
	if p.Name != "" {
		return "blog:" + blog + ":name:" + normalizeBlog(p.Name)
	}
	return ""
}
func messageKey(c *Conversation, m *Message, sender string, occurrence int) string {
	// Media references, filename and export ordering are deliberately excluded.
	if sender == "" {
		sender = "unknown:" + m.Sender
	}
	tuple, _ := json.Marshal(struct {
		Conversation, Sender, Date, Epoch, Type, Text string
		Targets                                       []string
	}{c.ID, sender, m.DateText, m.Epoch, m.Type, m.Text, m.Targets})
	return fmt.Sprintf("v1:%x:%d", sha256.Sum256(tuple), occurrence)
}

// Import archives observations within an explicitly named blog. Replay preserves
// the first observation and can fill attachment slots that still lack bytes.
func Import(ctx context.Context, st *store.Store, input string, opts ImportOptions) (summary *ImportSummary, retErr error) {
	summary = &ImportSummary{UnsupportedTypes: map[string]int{}}
	opts.Blog = normalizeBlog(opts.Blog)
	if opts.Blog == "" || strings.ContainsAny(opts.Blog, "/\\: \t\n") {
		return summary, errors.New("tumblr: an explicit blog name is required")
	}
	r, err := openExport(ctx, input)
	if err != nil {
		return summary, err
	}
	defer func() { retErr = errors.Join(retErr, r.close()) }()
	summary.FilesSkipped = r.skipped
	var sourceID, syncID int64
	defer func() {
		if syncID == 0 {
			return
		}
		// Finalize committed observations even when the caller cancels the import.
		cleanup := context.WithoutCancel(ctx)
		retErr = errors.Join(retErr, st.RecomputeConversationStatsContext(cleanup, sourceID), st.UpdateSyncCheckpointContext(cleanup, syncID, &store.Checkpoint{
			MessagesProcessed: int64(summary.MessagesAdded + summary.MessagesPresent + summary.Unsupported + summary.Malformed),
			MessagesAdded:     int64(summary.MessagesAdded),
		}))
		if retErr != nil {
			retErr = errors.Join(retErr, st.FailSyncContext(cleanup, syncID, retErr.Error()))
		} else {
			retErr = st.CompleteSyncContext(cleanup, syncID, "")
		}
	}()
	for _, name := range r.files {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		summary.Files++
		c, err := r.conversation(ctx, name)
		if errors.Is(err, errHTMLTooLarge) {
			summary.FilesSkipped++
			continue
		}
		if err != nil {
			return summary, fmt.Errorf("tumblr: %s: %w", name, err)
		}
		owner := ""
		for _, p := range c.Participants {
			if normalizeBlog(p.Name) == opts.Blog {
				key := participantKey(opts.Blog, p)
				if owner != "" && owner != key {
					return summary, errors.New("tumblr: ambiguous exporting participant")
				}
				owner = key
			}
		}
		if owner == "" {
			summary.ForeignConversations++
			continue
		}
		if summary.OwnerKey != "" && summary.OwnerKey != owner {
			return summary, errors.New("tumblr: inconsistent owner identity across conversations")
		}
		if sourceID == 0 {
			source, err := st.GetOrCreateSourceContext(ctx, SourceType, owner)
			if err != nil {
				return summary, err
			}
			sourceID, summary.SourceID = source.ID, source.ID
			if err := st.UpdateSourceDisplayNameContext(ctx, sourceID, opts.Blog); err != nil {
				return summary, err
			}
			syncID, err = st.StartSyncContext(ctx, sourceID, "import-tumblr")
			if err != nil {
				return summary, err
			}
			st = st.ScopedToSync(sourceID, syncID)
		}
		summary.Malformed += c.Malformed
		summary.UnknownBlocks += c.UnknownBlocks
		summary.OwnerKey = owner
		summary.Conversations++
		if err := importConversation(ctx, st, r, sourceID, c, opts, summary); err != nil {
			return summary, err
		}
		if opts.Logger != nil {
			opts.Logger.Info("tumblr: conversation imported", "conversation", c.ID)
		}
	}
	if summary.Conversations == 0 && summary.ForeignConversations > 0 {
		return summary, fmt.Errorf("tumblr: no conversations for selected blog %q; check the participant blog label", opts.Blog)
	}
	return summary, nil
}

func importConversation(ctx context.Context, st *store.Store, r *exportReader, sourceID int64, c *Conversation, opts ImportOptions, s *ImportSummary) error {
	names := map[string]string{}
	ids := map[string]int64{}
	members := []store.ConversationParticipantRef{}
	title := []string{}
	for _, p := range c.Participants {
		key := participantKey(opts.Blog, p)
		if key == "" {
			continue
		}
		if p.Name != "" {
			title = append(title, p.Name)
		}
		id, err := st.EnsureParticipantByIdentifierContext(ctx, SourceType, key, p.Name)
		if err != nil {
			return err
		}
		normalized := normalizeBlog(p.Name)
		// Duplicate labels with different IDs cannot identify a sender.
		if old, ok := names[normalized]; ok && old != key {
			names[normalized] = ""
		} else if !ok {
			names[normalized] = key
		}
		ids[key] = id
		members = append(members, store.ConversationParticipantRef{ParticipantID: id, Role: "member"})
	}
	conversationType := "direct_chat"
	if len(members) > 2 {
		conversationType = "group_chat"
	}
	if _, err := st.EnsureConversationWithTypeContext(ctx, sourceID, c.ID, conversationType, strings.Join(title, ", ")); err != nil {
		return err
	}
	keys := make([]string, len(c.Messages))
	occurrences := map[string]int{}
	for i := range c.Messages {
		m := &c.Messages[i]
		base := messageKey(c, m, names[normalizeBlog(m.Sender)], 0)
		occurrences[base]++
		keys[i] = messageKey(c, m, names[normalizeBlog(m.Sender)], occurrences[base])
	}
	existing, err := st.MessageExistsBatchContext(ctx, sourceID, keys)
	if err != nil {
		return err
	}
	for i := range c.Messages {
		if err := ctx.Err(); err != nil {
			return err
		}
		m := &c.Messages[i]
		switch m.Type {
		case "TEXT", "POSTREF", "IMAGE":
		default:
			s.Unsupported++
			s.UnsupportedTypes[m.Type]++
			continue
		}
		if !m.SentAt.Valid {
			s.Undated++
		}
		if m.DateConflict {
			s.ConflictingDates++
		}
		id, present := existing[keys[i]]
		writes, err := mediaWrites(ctx, st, r, id, c.Filename, m, opts, s)
		if err != nil {
			return err
		}
		if present {
			for _, w := range writes {
				if err := st.UpsertAttachmentRecordWithStats(ctx, id, w, true); err != nil {
					return err
				}
			}
			s.MessagesPresent++
			continue
		}
		senderKey := names[normalizeBlog(m.Sender)]
		sender := ids[senderKey]
		recipients := []store.RecipientSet{}
		if sender != 0 {
			recipients = append(recipients, store.RecipientSet{Type: "from", ParticipantIDs: []int64{sender}, DisplayNames: []string{m.Sender}})
			to := []int64{}
			for _, p := range members {
				if p.ParticipantID != sender {
					to = append(to, p.ParticipantID)
				}
			}
			if len(to) > 0 {
				recipients = append(recipients, store.RecipientSet{Type: "to", ParticipantIDs: to})
			}
		}
		raw, err := json.Marshal(struct {
			Version                          int
			Blog                             string
			ConversationID, Status, Filename string
			Participants                     []Participant
			Message                          *Message
		}{1, opts.Blog, c.ID, c.Status, c.Filename, c.Participants, m})
		if err != nil {
			return err
		}
		var replacement *[]store.AttachmentWrite
		if len(writes) > 0 {
			replacement = &writes
		}
		body := m.Text
		if m.Type == "POSTREF" {
			body = strings.TrimSpace(m.Text + "\n" + strings.Join(m.Targets, "\n"))
		}
		_, err = st.PersistMessageContext(ctx, &store.MessagePersistData{
			Message:      &store.Message{SourceID: sourceID, SourceMessageID: keys[i], MessageType: MessageType, SentAt: m.SentAt, SenderID: sql.NullInt64{Int64: sender, Valid: sender != 0}, IsFromMe: sender != 0 && senderKey == s.OwnerKey, Snippet: sql.NullString{String: textutil.PrefixRunes(body, 200), Valid: body != ""}, SizeEstimate: int64(len(body))},
			Conversation: &store.ConversationPersistData{SourceConversationID: c.ID, ConversationType: conversationType, Title: strings.Join(title, ", "), Participants: members, PreserveExistingParticipants: true},
			BodyText:     sql.NullString{String: body, Valid: body != ""}, RawMIME: raw, RawFormat: "json", Recipients: recipients, MIMEAttachmentReplacement: replacement, FTS: &store.FTSDoc{Body: body},
		})
		if err != nil {
			return err
		}
		s.MessagesAdded++
	}
	return nil
}
