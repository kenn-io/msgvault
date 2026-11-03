package telegram

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

const syncType = "import-telegram"
const checkpointVersion = 1

type telegramCheckpoint struct {
	Version       int    `json:"version"`
	RootDir       string `json:"root_dir"`
	ExportSize    int64  `json:"export_size"`
	ExportModTime int64  `json:"export_mod_time"`
	NextChat      int    `json:"next_chat"`
}

type ImportOptions struct {
	Me             string
	RootDir        string
	AttachmentsDir string
	Limit          int
	NoResume       bool
	Logger         *slog.Logger
}

type ImportSummary struct {
	SourceID                  int64
	Duration                  time.Duration
	ChatsProcessed            int
	ChatsSkipped              int
	ServiceMessagesSkipped    int64
	UnsupportedEntriesSkipped int64
	MessagesAdded             int64
	MessagesSkipped           int64
	ParticipantsResolved      int
	AttachmentsStored         int
	AttachmentsMissing        int
	WasResumed                bool
	LimitReached              bool
}

// Import adds messages and fills media slots that still lack bytes.
func Import(ctx context.Context, st *store.Store, opts ImportOptions) (summary *ImportSummary, retErr error) {
	start := time.Now()
	summary = &ImportSummary{}
	defer func() { summary.Duration = time.Since(start) }()
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	root, err := filepath.Abs(opts.RootDir)
	if err != nil {
		return summary, err
	}
	opts.RootDir = root
	reader, err := OpenExport(ctx, root)
	if err != nil {
		return summary, err
	}
	defer func() { retErr = errors.Join(retErr, reader.Close()) }()
	info, err := reader.file.Stat()
	if err != nil {
		return summary, err
	}
	ownerID := reader.OwnerID()
	if opts.Me != "" {
		ownerID, err = strconv.ParseInt(strings.TrimSpace(opts.Me), 10, 64)
		if err != nil || ownerID <= 0 {
			return summary, errors.New("telegram: --me must be a positive numeric Telegram user ID")
		}
	}
	if ownerID <= 0 {
		return summary, ErrNoOwner
	}
	owner := "user" + strconv.FormatInt(ownerID, 10)
	source, err := st.GetOrCreateSource(SourceType, owner)
	if err != nil {
		return summary, err
	}
	summary.SourceID = source.ID
	cp := telegramCheckpoint{Version: checkpointVersion, RootDir: root, ExportSize: info.Size(), ExportModTime: info.ModTime().UnixNano()}
	if !opts.NoResume {
		prev, err := st.GetLatestCheckpointedSyncByType(source.ID, syncType)
		if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
			return summary, err
		}
		if prev != nil && prev.CursorBefore.Valid {
			var prior telegramCheckpoint
			if err := json.Unmarshal([]byte(prev.CursorBefore.String), &prior); err != nil {
				return summary, fmt.Errorf("telegram: decode checkpoint; use --no-resume: %w", err)
			}
			if prior.Version > checkpointVersion {
				return summary, fmt.Errorf("telegram: checkpoint version %d is newer than supported; use --no-resume", prior.Version)
			}
			if prior.Version == checkpointVersion && prior.RootDir == root && prior.ExportSize == cp.ExportSize && prior.ExportModTime == cp.ExportModTime {
				cp.NextChat = prior.NextChat
				summary.WasResumed = true
			}
		}
	}
	syncID, err := st.StartSyncContext(ctx, source.ID, syncType)
	if err != nil {
		return summary, err
	}
	st = st.ScopedToSync(source.ID, syncID)
	defer func() {
		cleanup := context.WithoutCancel(ctx)
		retErr = errors.Join(retErr, st.ResolveDeferredMessageRepliesContext(cleanup, source.ID, MessageType, 500), st.RecomputeConversationStatsContext(cleanup, source.ID))
		var checkpoint *store.Checkpoint
		if !opts.NoResume {
			payload, err := json.Marshal(cp)
			retErr = errors.Join(retErr, err)
			if err == nil {
				checkpoint = &store.Checkpoint{PageToken: string(payload), MessagesProcessed: summary.MessagesAdded + summary.MessagesSkipped, MessagesAdded: summary.MessagesAdded}
				retErr = errors.Join(retErr, st.UpdateSyncCheckpointContext(cleanup, syncID, checkpoint))
			}
		}
		if retErr != nil {
			retErr = errors.Join(retErr, st.FailSyncContext(cleanup, syncID, retErr.Error()))
		} else if summary.LimitReached && checkpoint != nil {
			retErr = errors.Join(retErr, st.PauseSyncWithCheckpoint(syncID, checkpoint))
		} else {
			retErr = errors.Join(retErr, st.CompleteSyncContext(cleanup, syncID, ""))
		}
	}()
	participants := map[string]int64{}
	named := map[string]bool{}
	resolve := func(id, name string) (int64, error) {
		if id == "" {
			return 0, nil
		}
		if pid, ok := participants[id]; ok && (name == "" || named[id]) {
			return pid, nil
		}
		pid, err := st.EnsureParticipantByIdentifierContext(ctx, SourceType, id, name)
		if err != nil {
			return 0, err
		}
		if _, seen := participants[id]; !seen && strings.HasPrefix(id, "user") {
			if err := beeper.LinkTelegramUser(ctx, st, source.ID, pid, strings.TrimPrefix(id, "user")); err != nil {
				return 0, err
			}
		}
		participants[id] = pid
		named[id] = name != ""
		return pid, nil
	}
	defer func() { summary.ParticipantsResolved = len(participants) }()
	ownerName := ""
	if ownerID == reader.OwnerID() {
		ownerName = reader.OwnerName()
	}
	ownerPID, err := resolve(owner, ownerName)
	if err != nil {
		return summary, err
	}
	index := 0
	err = reader.Chats(ctx, func(chat *Chat) error {
		current := index
		index++
		if err := ctx.Err(); err != nil {
			return err
		}
		if current < cp.NextChat {
			return nil
		}
		if chat.PeerKey() == "" {
			summary.ChatsSkipped++
			logger.Warn("telegram: skipping unknown chat type", "type", chat.Type)
		} else {
			if err := importChat(ctx, st, resolve, source.ID, ownerPID, ownerName, chat, opts, summary); err != nil {
				return err
			}
			logger.Info("telegram: chat imported", "peer", chat.PeerKey())
		}
		cp.NextChat = index
		if !opts.NoResume {
			payload, err := json.Marshal(cp)
			if err != nil {
				return err
			}
			return st.UpdateSyncCheckpointContext(context.WithoutCancel(ctx), syncID, &store.Checkpoint{PageToken: string(payload), MessagesProcessed: summary.MessagesAdded + summary.MessagesSkipped, MessagesAdded: summary.MessagesAdded})
		}
		return nil
	})
	if errors.Is(err, errLimitReached) {
		summary.LimitReached = true
		err = nil
	}
	return summary, err
}

var errLimitReached = errors.New("telegram: import limit reached")

func importChat(ctx context.Context, st *store.Store, resolve func(string, string) (int64, error), sourceID, ownerPID int64, ownerName string, chat *Chat, opts ImportOptions, summary *ImportSummary) error {
	peer := chat.PeerKey()
	summary.ChatsProcessed++
	direct := chat.Type == "personal_chat" || chat.Type == "bot_chat"
	var peerPID int64
	existing, err := prepareChat(ctx, st, sourceID, peer, chat)
	if err != nil {
		return err
	}
	for i := range chat.Messages {
		if err := ctx.Err(); err != nil {
			return err
		}
		m := &chat.Messages[i]
		if m.Type == "service" {
			summary.ServiceMessagesSkipped++
			continue
		}
		if !m.supported() {
			summary.UnsupportedEntriesSkipped++
			continue
		}
		if opts.Limit > 0 && summary.MessagesAdded >= int64(opts.Limit) {
			return errLimitReached
		}
		sender, err := resolve(m.FromID, m.FromName)
		if err != nil {
			return err
		}
		if direct && peerPID == 0 {
			peerPID, err = resolve(peer, chat.Name)
			if err != nil {
				return err
			}
		}
		key := fmt.Sprintf("%s:%d", peer, m.ID)
		if id, ok := existing[key]; ok {
			writes, err := mediaWrites(ctx, st, id, m, opts, summary)
			if err != nil {
				return err
			}
			for _, write := range writes {
				if err := st.UpsertAttachmentRecordWithStats(ctx, id, write, true); err != nil {
					return err
				}
			}
			summary.MessagesSkipped++
			continue
		}
		fromMe := sender != 0 && sender == ownerPID
		members := []store.ConversationParticipantRef{{ParticipantID: ownerPID, Role: "member"}}
		if sender != 0 {
			members = append(members, store.ConversationParticipantRef{ParticipantID: sender, Role: "member"})
		}
		if peerPID != 0 {
			members = append(members, store.ConversationParticipantRef{ParticipantID: peerPID, Role: "member"})
		}
		recipients := []store.RecipientSet{}
		if sender != 0 {
			recipients = append(recipients, store.RecipientSet{Type: "from", ParticipantIDs: []int64{sender}, DisplayNames: []string{m.FromName}})
		}
		if direct {
			to, name := ownerPID, ownerName
			if fromMe {
				to, name = peerPID, chat.Name
			}
			recipients = append(recipients, store.RecipientSet{Type: "to", ParticipantIDs: []int64{to}, DisplayNames: []string{name}})
		}
		metadata := struct {
			ReferencedMessageID string `json:"referenced_message_id,omitempty"`
			ForwardedFrom       string `json:"forwarded_from,omitempty"`
		}{ForwardedFrom: m.ForwardedFrom}
		if m.ReplyTo != 0 {
			parentPeer := m.ReplyToPeerID
			if parentPeer == "" {
				parentPeer = peer
			}
			metadata.ReferencedMessageID = fmt.Sprintf("%s:%d", parentPeer, m.ReplyTo)
		}
		encoded, err := json.Marshal(metadata)
		if err != nil {
			return err
		}
		writes, err := mediaWrites(ctx, st, 0, m, opts, summary)
		if err != nil {
			return err
		}
		snippet := textutil.PrefixRunes(strings.TrimSpace(m.Text), 200)
		id, err := st.PersistMessageContext(ctx, &store.MessagePersistData{
			Message:      &store.Message{SourceID: sourceID, SourceMessageID: key, MessageType: MessageType, SentAt: sql.NullTime{Time: m.SentAt, Valid: !m.SentAt.IsZero()}, SenderID: sql.NullInt64{Int64: sender, Valid: sender != 0}, IsFromMe: fromMe, Snippet: sql.NullString{String: snippet, Valid: snippet != ""}, SizeEstimate: int64(len(m.Text))},
			Conversation: &store.ConversationPersistData{SourceConversationID: peer, ConversationType: chat.ConvType(), Title: chat.Name, Participants: members, PreserveExistingParticipants: true},
			Edited:       m.Edited != "" || m.EditedUnixtime != "",
			BodyText:     sql.NullString{String: m.Text, Valid: m.Text != ""}, Metadata: &sql.NullString{String: string(encoded), Valid: true}, Recipients: recipients, MIMEAttachmentReplacement: replacement(writes), FTS: &store.FTSDoc{Body: m.Text},
		})
		if err != nil {
			return fmt.Errorf("telegram: persist %s: %w", key, err)
		}
		existing[key] = id
		summary.MessagesAdded++
	}
	return nil
}

// prepareChat keeps a conversation for every exported chat, even one with no
// supported messages, and returns the chat's already-imported message IDs.
func prepareChat(ctx context.Context, st *store.Store, sourceID int64, peer string, chat *Chat) (map[string]int64, error) {
	if _, err := st.EnsureConversationWithTypeContext(ctx, sourceID, peer, chat.ConvType(), chat.Name); err != nil {
		return nil, fmt.Errorf("telegram: ensure conversation %s: %w", peer, err)
	}
	ids := make([]string, 0, len(chat.Messages))
	for _, m := range chat.Messages {
		if m.supported() {
			ids = append(ids, fmt.Sprintf("%s:%d", peer, m.ID))
		}
	}
	return st.MessageExistsBatchContext(ctx, sourceID, ids)
}

// replacement leaves a text-only message's attachments untouched instead of clearing them.
func replacement(writes []store.AttachmentWrite) *[]store.AttachmentWrite {
	if len(writes) == 0 {
		return nil
	}
	return &writes
}
