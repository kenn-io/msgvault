package signalexport

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

const formatVersion = 3

var ErrNewerFormat = errors.New("saved Signal messages use a newer format; upgrade msgvault")

type ImportOptions struct {
	RootDir        string
	Account        string
	AttachmentsDir string
}
type ImportSummary struct {
	SourceID                                         int64
	MessagesAdded, MessagesExisting, MessagesChanged int64
	Skipped, Omitted, MediaMissing                   map[string]int64
	EmptyFrames, UnknownFrames                       int64
	AttachmentsStored, AttachmentsMissing            int
}

// Import replays native JSONL, preserving first snapshots and repairing media.
func Import(ctx context.Context, st *store.Store, opts ImportOptions) (s *ImportSummary, retErr error) {
	s = &ImportSummary{Skipped: map[string]int64{}, Omitted: map[string]int64{}, MediaMissing: map[string]int64{}}
	opts.Account = strings.TrimSpace(opts.Account)
	if opts.Account == "" {
		return s, errors.New("signal export: --account requires a stable nonempty label; reuse it for later exports of this account")
	}
	r, err := openReader(ctx, opts.RootDir)
	if err != nil {
		return s, err
	}
	defer func() { retErr = errors.Join(retErr, r.Close()) }()
	source, err := st.GetOrCreateSourceContext(ctx, SourceType, opts.Account)
	if err != nil {
		return s, err
	}
	var state struct {
		Format int `json:"signal_export_format"`
	}
	if source.SyncCursor.Valid && source.SyncCursor.String != "" {
		if err := json.Unmarshal([]byte(source.SyncCursor.String), &state); err != nil {
			return s, fmt.Errorf("read saved Signal format: %w", err)
		}
	}
	if state.Format > formatVersion {
		return s, ErrNewerFormat
	}
	s.SourceID = source.ID
	if state.Format != formatVersion {
		if err := st.UpdateSourceSyncState(ctx, source.ID, fmt.Sprintf(`{"signal_export_format":%d}`, formatVersion)); err != nil {
			return s, err
		}
	}
	syncID, err := st.StartSyncContext(ctx, source.ID, "import-signal-export")
	if err != nil {
		return s, err
	}
	st = st.ScopedToSync(source.ID, syncID)
	defer func() {
		// Committed partial progress needs accurate stats and a terminal run even
		// after the caller interrupts; replay uses those intact message snapshots.
		cleanup := context.WithoutCancel(ctx)
		retErr = errors.Join(retErr, st.RecomputeConversationStatsContext(cleanup, source.ID))
		if retErr != nil {
			retErr = errors.Join(retErr, st.FailSyncContext(cleanup, syncID, retErr.Error()))
		} else {
			retErr = errors.Join(retErr, st.CompleteSyncContext(cleanup, syncID, ""))
		}
		s.EmptyFrames, s.UnknownFrames = r.empty, r.unknown
		for key, n := range r.omitted {
			s.Omitted[key] += n
		}
	}()
	scope := accountScope(opts.Account)
	resolve := func(p peer) (int64, error) {
		return st.EnsureParticipantByIdentifierContext(ctx, SourceType, scopedKey(scope, p.key), p.name)
	}
	// Count collisions before replay so an inserted earlier frame cannot consume
	// a saved slot. The normal pass reports any input error and keeps progress.
	slots := newSlotCounter()
	counter, err := openReader(ctx, opts.RootDir)
	if err != nil {
		return s, err
	}
	_ = counter.frames(ctx, func(m *chatItem, _ []byte, _ int) error {
		if m.Standard == nil {
			return nil
		}
		conversation, author, date, reason := eligibleMessage(counter, m)
		if reason != "" {
			return nil
		}
		tuple := slotTuple(scopedKey(scope, conversation.key), scopedKey(scope, author.key), date, originalReceived(m))
		slots.count(tuple)
		return nil
	})
	if err := counter.Close(); err != nil {
		return s, err
	}
	selfConfirmed := false
	var ownerID int64
	err = r.frames(ctx, func(m *chatItem, raw []byte, line int) error {
		skip := func(reason string) error { s.Skipped[reason]++; return nil }
		if m.Standard == nil {
			fields, _ := decodeObject(raw)
			for _, kind := range []string{"contactMessage", "stickerMessage", "remoteDeletedMessage", "updateMessage", "paymentNotification", "giftBadge", "viewOnceMessage", "directStoryReplyMessage", "poll", "adminDeletedMessage"} {
				if fields[kind] != nil {
					return skip(kind)
				}
			}
			return skip("unsupported_item")
		}
		conversation, author, date, reason := eligibleMessage(r, m)
		if reason != "" {
			return skip(reason)
		}
		fromMe := m.AuthorID == r.self
		at, _ := sentTime(date)
		owner := r.peers[r.self]
		if !selfConfirmed {
			ownerID, err = resolve(owner)
			if err != nil {
				return err
			}
			if err := st.AddAccountIdentityContext(ctx, source.ID, scopedKey(scope, owner.key), "signal_export_self"); err != nil {
				return err
			}
			selfConfirmed = true
		}
		convKey := scopedKey(scope, conversation.key)
		authorKey := scopedKey(scope, author.key)
		key, id, ambiguous, err := slots.slot(ctx, st, source.ID, convKey, authorKey, date, originalReceived(m), m.Standard)
		if err != nil {
			return err
		}
		if ambiguous {
			return skip("ambiguous_message")
		}
		standard := m.Standard
		countOmissions(s, m)
		body, writes, knownChanged, err := mediaWrites(ctx, st, r, id, standard, opts, s)
		if err != nil {
			return err
		}
		text := sql.NullString{String: body, Valid: body != ""}
		doc := store.FTSDoc{Body: body}
		if id != 0 {
			matches, err := st.MessageContentMatchesContext(ctx, id, text, doc)
			if err != nil {
				return err
			}
			s.MessagesExisting++
			if !matches || knownChanged {
				s.MessagesChanged++
			}
			for _, write := range writes {
				if err := st.UpsertAttachmentRecordWithStats(ctx, id, write, true); err != nil {
					return err
				}
			}
			return nil
		}
		authorID, err := resolve(author)
		if err != nil {
			return err
		}
		members := []store.ConversationParticipantRef{{ParticipantID: ownerID, Role: "member"}, {ParticipantID: authorID, Role: "member"}}
		recipients := []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{authorID}, DisplayNames: []string{author.name}}}
		if conversation.kind == "direct_chat" {
			peerID, err := resolve(conversation)
			if err != nil {
				return err
			}
			members = append(members, store.ConversationParticipantRef{ParticipantID: peerID, Role: "member"})
			to, name := ownerID, owner.name
			if fromMe {
				to, name = peerID, conversation.name
			}
			recipients = append(recipients, store.RecipientSet{Type: "to", ParticipantIDs: []int64{to}, DisplayNames: []string{name}})
		}
		meta, err := json.Marshal(struct {
			Header          backupInfo `json:"header"`
			Line            int        `json:"source_line"`
			ChatID          decimal    `json:"chat_id"`
			AuthorID        decimal    `json:"author_id"`
			RecipientID     decimal    `json:"recipient_id"`
			ConversationKey string     `json:"conversation_key"`
			AuthorKey       string     `json:"author_key"`
			Roster          string     `json:"roster_coverage"`
		}{r.header, line, m.ChatID, m.AuthorID, r.chats[m.ChatID], convKey, authorKey, "partial_observed"})
		if err != nil {
			return err
		}
		var replacement *[]store.AttachmentWrite
		if len(writes) > 0 {
			replacement = &writes
		}
		_, err = st.PersistMessageContext(ctx, &store.MessagePersistData{
			Message:      &store.Message{SourceID: source.ID, SourceMessageID: key, MessageType: MessageType, SentAt: sql.NullTime{Time: at, Valid: true}, SenderID: sql.NullInt64{Int64: authorID, Valid: true}, IsFromMe: fromMe, Snippet: sql.NullString{String: textutil.PrefixRunes(body, 200), Valid: body != ""}, SizeEstimate: int64(len(body))},
			Conversation: &store.ConversationPersistData{SourceConversationID: convKey, ConversationType: conversation.kind, Title: conversation.name, Participants: members, PreserveExistingParticipants: true},
			BodyText:     text, RawMIME: raw, RawFormat: "signal-export-jsonl", Metadata: &sql.NullString{String: string(meta), Valid: true}, Edited: len(m.Revisions) > 0, Recipients: recipients, MIMEAttachmentReplacement: replacement, FTS: &doc,
		})
		if err != nil {
			return fmt.Errorf("persist Signal message: %w", err)
		}
		s.MessagesAdded++
		return nil
	})
	return s, err
}

// eligibleMessage applies the same identity and direction rules in both passes.
func eligibleMessage(r *reader, m *chatItem) (conversation, author peer, date decimal, reason string) {
	ref, ok := r.chats[m.ChatID]
	if !ok {
		return peer{}, peer{}, "", "unresolved_chat"
	}
	conversation, author = r.peers[ref], r.peers[m.AuthorID]
	if conversation.key == "" {
		return conversation, author, "", "recipient_identity"
	}
	if author.key == "" || author.kind == "group_chat" {
		return conversation, author, "", "author_identity"
	}
	if r.self == "" {
		return conversation, author, "", "self_identity"
	}
	fromMe := m.AuthorID == r.self
	if (m.Incoming == nil) == (m.Outgoing == nil) {
		return conversation, author, "", "unknown_direction"
	}
	if (m.Outgoing != nil) != fromMe || (conversation.kind == "direct_chat" && !fromMe && m.AuthorID != ref) {
		return conversation, author, "", "direction_conflict"
	}
	date, ok = originalDate(m)
	if !ok {
		if len(m.Revisions) > 0 {
			return conversation, author, "", "revision"
		}
		return conversation, author, "", "sent_time"
	}
	return conversation, author, date, ""
}

func countOmissions(s *ImportSummary, m *chatItem) {
	standard := m.Standard
	if m.SMS {
		s.Omitted["sms"]++
	}
	if m.ExpireStartDate != nil {
		s.Omitted["expire_start_date"]++
	}
	if m.ExpiresInMs != nil {
		s.Omitted["expires_in_ms"]++
	}
	if standard.Quote != nil {
		s.Omitted["quote"]++
	}
	s.Omitted["reactions"] += int64(len(standard.Reactions))
	s.Omitted["link_preview"] += int64(len(standard.LinkPreview))
	for _, attachment := range standard.Attachments {
		if attachment.Flag != nil && attachment.Flag != "NONE" {
			s.Omitted["attachment_flag"]++
		}
	}
	if standard.Text != nil {
		s.Omitted["formatting"] += int64(len(standard.Text.Ranges))
	}
	if m.Pin != nil {
		s.Omitted["pin"]++
	}
	s.Omitted["revision_history"] += int64(len(m.Revisions))
}
