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
	// Store supplies participant reuse; no unbounded message/participant cache.
	resolve := func(p peer) (int64, error) {
		participant, err := st.EnsureParticipantByIdentifierContext(ctx, SourceType, scopedKey(scope, p.key), p.name)
		if err != nil {
			return 0, err
		}
		for _, candidate := range []string{p.aci, p.phone} {
			if candidate != "" {
				if err := addAlias(ctx, st, scope, participant, p, candidate); err != nil {
					return 0, err
				}
			}
		}
		return participant, nil
	}
	resolved := map[decimal]peer{} // only reader recipients, so bounded by its entry limit
	resolveKey := func(id decimal) (peer, error) {
		if p, ok := resolved[id]; ok {
			return p, nil
		}
		p, err := withoutReusedPhone(ctx, st, scope, r.peers[id])
		if err != nil {
			return p, err
		}
		if p.aci == "" && p.phone == "" {
			return p, nil
		}
		for _, candidate := range []string{p.aci, p.phone} {
			if candidate == "" {
				continue
			}
			key, found, err := knownKey(ctx, st, scope, candidate)
			if err != nil {
				return p, err
			}
			if found {
				p.key = key
				if _, err := resolve(p); err != nil {
					return p, err
				}
				break
			}
		}
		resolved[id] = p
		return p, nil
	}
	// Stored rows exist only after an earlier import, so a first import skips the scan.
	var hints *identityHints
	stored, err := st.CountMessagesForSourceContext(ctx, source.ID)
	if err != nil {
		return s, err
	}
	if stored > 0 {
		if hints, err = scanIdentities(ctx, opts.RootDir); err != nil {
			return s, err
		}
	}
	slots := newSlotCounter(hints)
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
		ref, ok := r.chats[m.ChatID]
		if !ok {
			return skip("unresolved_chat")
		}
		conversation, err := resolveKey(ref)
		if err != nil {
			return err
		}
		author, err := resolveKey(m.AuthorID)
		if err != nil {
			return err
		}
		if conversation.key == "" {
			return skip("recipient_identity")
		}
		if author.key == "" || author.kind == "group_chat" {
			return skip("author_identity")
		}
		if r.self == "" {
			return skip("self_identity")
		}
		fromMe := m.AuthorID == r.self
		if (m.Incoming == nil) == (m.Outgoing == nil) || m.Directionless != nil {
			return skip("unknown_direction")
		}
		if (m.Outgoing != nil) != fromMe {
			return skip("direction_conflict")
		}
		if conversation.kind == "direct_chat" && !fromMe && m.AuthorID != ref {
			return skip("direction_conflict")
		}
		date, ok := originalDate(m)
		if !ok {
			if len(m.Revisions) > 0 {
				return skip("revision")
			}
			return skip("sent_time")
		}
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
		key, id, err := slots.slot(ctx, st, source.ID, convKey, authorKey, date, originalReceived(m), m, line)
		if err != nil {
			return err
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
		}{r.header, line, m.ChatID, m.AuthorID, ref, convKey, authorKey, "partial_observed"})
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

// knownKey returns the primary key of the participant that already holds candidate.
func knownKey(ctx context.Context, st *store.Store, scope, candidate string) (string, bool, error) {
	participant, _, err := st.ParticipantByIdentifierContext(ctx, SourceType, scopedKey(scope, candidate))
	if err != nil || participant == 0 {
		return "", false, err
	}
	primary, err := st.PrimaryParticipantIdentifierContext(ctx, participant, SourceType)
	if err != nil {
		return "", false, err
	}
	key, ok := strings.CutPrefix(primary, "v1:"+scope+":")
	if !ok {
		key = candidate
	}
	return key, true, nil
}

// addAlias attaches candidate to participant. An established ACI participant
// stays authoritative: a phone already held by another participant is left there.
func addAlias(ctx context.Context, st *store.Store, scope string, participant int64, p peer, candidate string) error {
	err := st.AddParticipantIdentifierIfAbsentContext(ctx, participant, SourceType, scopedKey(scope, candidate))
	if !errors.Is(err, store.ErrParticipantIdentifierConflict) {
		return err
	}
	if p.aci != "" && candidate == p.phone {
		return nil
	}
	return fmt.Errorf("signal contact identifiers belong to different participants: %w", err)
}

// withoutReusedPhone drops a phone number held by a participant with another ACI.
func withoutReusedPhone(ctx context.Context, st *store.Store, scope string, p peer) (peer, error) {
	if p.aci == "" || p.phone == "" {
		return p, nil
	}
	var others int
	err := st.DB().QueryRowContext(ctx, st.Rebind(`SELECT COUNT(*) FROM participant_identifiers phone JOIN participant_identifiers aci ON aci.participant_id = phone.participant_id
		WHERE phone.identifier_type = ? AND phone.identifier_value = ? AND aci.identifier_type = ? AND aci.identifier_value LIKE ? AND aci.identifier_value <> ?`),
		SourceType, scopedKey(scope, p.phone), SourceType, scopedKey(scope, "aci:")+"%", scopedKey(scope, p.aci)).Scan(&others)
	if others > 0 {
		p.phone = ""
	}
	return p, err
}

func countOmissions(s *ImportSummary, m *chatItem) {
	standard := m.Standard
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
