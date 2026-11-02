package tiktok

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/beeperidentity"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/store"
)

type MessagesAPI interface {
	Account(ctx context.Context, account string) (Account, error)
	Conversations(ctx context.Context, account, kind, cursor string, limit int) (ConversationPage, error)
	Messages(ctx context.Context, account, conversation string) (MessagePage, error)
	OpenMessageMedia(ctx context.Context, account string, message BusinessMessage, maxBytes int64) (*MessageMediaResponse, error)
}
type MessageImportOptions struct {
	AttachmentsDir string
	MaxMediaBytes  int64
}
type MessageImporter struct {
	store   *store.Store
	api     MessagesAPI
	options MessageImportOptions
}

func NewMessageImporter(st *store.Store, api MessagesAPI, options MessageImportOptions) *MessageImporter {
	if options.MaxMediaBytes <= 0 {
		options.MaxMediaBytes = 50 << 20
	}
	return &MessageImporter{store: st, api: api, options: options}
}
func (imp *MessageImporter) Register(ctx context.Context, app, account string) (*store.Source, error) {
	return registerTikTokAccount(ctx, imp.store, imp.api, app, account, MessagesSourceType)
}

type messageRun struct {
	imp          *MessageImporter
	source       *store.Source
	app, account string
	sum          *ImportSummary
}

func (imp *MessageImporter) run(ctx context.Context, app, account, syncType string, fn func(*messageRun) error) (sum *ImportSummary, retErr error) {
	if !ValidID(app) || !ValidID(account) {
		return nil, errors.New("invalid TikTok app or account selector")
	}
	source, err := imp.store.GetSourceByTypeAndIdentifier(MessagesSourceType, SourceIdentifier(app, account))
	if err != nil {
		return nil, err
	}
	execution, err := imp.store.AcquireSyncExecutionContext(ctx, source.ID)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, execution.Release()) }()
	source, err = imp.store.GetSourceByIDContext(ctx, source.ID)
	if err != nil {
		return nil, err
	}
	if source.HistoryOnly || source.MergedIntoSourceID != 0 {
		return nil, store.ErrSourceRetired
	}
	syncID, err := execution.StartSyncContext(ctx, syncType, "")
	if err != nil {
		return nil, err
	}
	scoped := *imp
	scoped.store = imp.store.ScopedToSync(source.ID, syncID)
	sum = &ImportSummary{}
	defer func() {
		sum.Partial = retErr != nil
		cp := &store.Checkpoint{MessagesProcessed: int64(sum.MessagesProcessed), MessagesAdded: int64(sum.MessagesAdded)}
		if retErr != nil {
			retErr = errors.Join(retErr, imp.store.InterruptSyncWithCheckpoint(ctx, syncID, retErr, cp))
		} else if err := scoped.store.UpdateSyncCheckpointContext(ctx, syncID, cp); err != nil {
			retErr = err
		} else {
			retErr = scoped.store.CompleteSyncAndUpdateSourceCursorContext(ctx, syncID, source.ID, "")
		}
	}()
	retErr = fn(&messageRun{imp: &scoped, source: source, app: app, account: account, sum: sum})
	return sum, retErr
}

// Import takes the provider's shallow window: at most 100 conversations,
// chosen from SINGLE and STRANGER, and at most 20 messages in each.
// A failed run can safely be repeated; native IDs make every write idempotent.
func (imp *MessageImporter) Import(ctx context.Context, app, account string) (*ImportSummary, error) {
	return imp.run(ctx, app, account, MessagesSourceType, func(r *messageRun) error {
		r.sum.Requests++
		if _, err := imp.api.Account(ctx, account); err != nil {
			return err
		}
		var conversations []BusinessConversation
		for _, kind := range []string{"SINGLE", "STRANGER"} {
			cursor := ""
			count := 0
			for pageNumber := 0; pageNumber < 100 && count < 100; pageNumber++ {
				r.sum.Requests++
				page, err := imp.api.Conversations(ctx, account, kind, cursor, 100-count)
				if err != nil {
					return err
				}
				for _, conversation := range page.Conversations {
					if !ValidID(string(conversation.ID)) {
						return errors.New("TikTok conversation identity is missing")
					}
				}
				conversations = append(conversations, page.Conversations...)
				count += len(page.Conversations)
				if !page.HasMore {
					break
				}
				cursor, err = nextCursor(cursor, page.Cursor, true)
				if err != nil {
					return err
				}
			}
		}
		slices.SortStableFunc(conversations, func(a, b BusinessConversation) int { return cmp.Compare(b.UpdateTime, a.UpdateTime) })
		seen := map[ID]bool{}
		var errs []error
		for _, conversation := range conversations {
			if seen[conversation.ID] {
				continue
			}
			if len(seen) >= 100 {
				break
			}
			seen[conversation.ID] = true
			r.sum.Requests++
			page, err := imp.api.Messages(ctx, account, string(conversation.ID))
			if err == nil {
				// Older parents are persisted before their newer replies where
				// both are present in the provider's 20-message projection.
				slices.SortStableFunc(page.Messages, func(a, b BusinessMessage) int { return cmp.Compare(a.Timestamp, b.Timestamp) })
				for _, message := range page.Messages {
					if err = r.persist(ctx, message, ""); err != nil {
						break
					}
				}
			}
			if err != nil {
				errs = append(errs, err)
				if ctx.Err() != nil {
					break
				}
			}
		}
		return errors.Join(errs...)
	})
}

// Apply archives one verified, routed event. EU events are handled by the
// receiver as an explicit unsupported outcome, never as empty messages.
func (imp *MessageImporter) Apply(ctx context.Context, app string, event MessageEvent, message BusinessMessage) (*ImportSummary, error) {
	if event.ClientKey != app || (event.Event != "im_receive_msg" && event.Event != "im_send_msg") {
		return nil, ErrInvalidEvent
	}
	return imp.run(ctx, app, string(event.Account), "tiktok_messages_webhook", func(r *messageRun) error { return r.persist(ctx, message, event.Event) })
}

type messageMetadata struct {
	App          string `json:"app"`
	Account      string `json:"account"`
	Conversation ID     `json:"conversation_id"`
	Message      ID     `json:"message_id"`
	Type         string `json:"type"`
	Event        string `json:"event,omitempty"`
	ReplyTo      ID     `json:"reply_to,omitempty"`
}

func messageKey(conversation, message ID) string {
	return beeperidentity.EncodePart(string(conversation)) + ":" + beeperidentity.EncodePart(string(message))
}

func (r *messageRun) persist(ctx context.Context, m BusinessMessage, event string) error {
	if !ValidID(string(m.ID)) || !ValidID(string(m.ConversationID)) {
		return errors.New("TikTok message identity is missing")
	}
	if m.Timestamp <= 0 {
		return errors.New("TikTok message timestamp is missing")
	}
	from, to := m.FromUser, m.ToUser
	if from == nil {
		from = &MessageActor{}
	}
	if to == nil {
		to = &MessageActor{}
	}
	// Native event direction is stronger than absent participant projections.
	if event == "im_send_msg" {
		if from.ID != "" && string(from.ID) != r.account {
			return errors.New("TikTok outbound account mismatch")
		}
		copyFrom := *from
		copyFrom.ID = ID(r.account)
		from = &copyFrom
	} else if event == "im_receive_msg" {
		if to.ID != "" && string(to.ID) != r.account {
			return errors.New("TikTok inbound account mismatch")
		}
		copyTo := *to
		copyTo.ID = ID(r.account)
		to = &copyTo
		if from.ID == "" && m.UniqueIdentifier != "" {
			copyFrom := *from
			copyFrom.ID = m.UniqueIdentifier
			from = &copyFrom
		}
	}
	self := string(from.ID) == r.account
	if from.ID != "" && to.ID != "" && string(from.ID) != r.account && string(to.ID) != r.account {
		return errors.New("TikTok message does not involve the selected account")
	}
	fromName := cmp.Or(from.DisplayName, m.From, m.Sender)
	toName := cmp.Or(to.DisplayName, m.To, m.Recipient)
	if self && fromName == "" {
		fromName = r.source.DisplayName.String
	}
	key := messageKey(m.ConversationID, m.ID)
	parentKey := ""
	if m.Reference != nil && m.Reference.ID != "" {
		parentKey = messageKey(m.ConversationID, m.Reference.ID)
	}
	existing, err := r.imp.store.StoredMessagesContext(ctx, r.source.ID, messageFormat, []string{key, parentKey})
	if err != nil {
		return err
	}
	prior, exists := existing[key]
	if exists {
		saved, err := r.imp.store.GetMessageMetadataContext(ctx, prior.ID)
		if err != nil {
			return err
		}
		var metadata messageMetadata
		if !saved.Valid || json.Unmarshal([]byte(saved.String), &metadata) != nil || metadata.App != r.app || metadata.Account != r.account || metadata.Conversation != m.ConversationID || metadata.Message != m.ID {
			return errors.New("TikTok message identity conflicts with the archive")
		}
	}
	sender, err := resolveActor(ctx, r.imp.store, r.source.ID, r.app, r.account, string(from.ID), fromName)
	if err != nil {
		return err
	}
	recipient, err := resolveActor(ctx, r.imp.store, r.source.ID, r.app, r.account, string(to.ID), toName)
	if err != nil {
		return err
	}
	if sender == 0 && exists {
		sender = prior.SenderID.Int64
		self = prior.SourceIsFromMe.Bool
	}
	var recipients []store.RecipientSet
	var participants []store.ConversationParticipantRef
	for _, actor := range []struct {
		kind, name string
		id         int64
	}{{"from", fromName, sender}, {"to", toName, recipient}} {
		if actor.id == 0 {
			continue
		}
		recipients = append(recipients, store.RecipientSet{Type: actor.kind, ParticipantIDs: []int64{actor.id}, DisplayNames: []string{actor.name}})
		participants = append(participants, store.ConversationParticipantRef{ParticipantID: actor.id, Role: "member"})
	}
	var parent sql.NullInt64
	if row, ok := existing[parentKey]; ok {
		parent = sql.NullInt64{Int64: row.ID, Valid: true}
	}
	metadata := messageMetadata{App: r.app, Account: r.account, Conversation: m.ConversationID, Message: m.ID, Type: m.Kind(), Event: event}
	if m.Reference != nil {
		metadata.ReplyTo = m.Reference.ID
	}
	meta, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	body := m.body()
	message := &store.Message{SourceID: r.source.ID, SourceMessageID: key, MessageType: MessagesSourceType, SentAt: sql.NullTime{Time: time.UnixMilli(int64(m.Timestamp)).UTC(), Valid: true}, SenderID: sql.NullInt64{Int64: sender, Valid: sender != 0}, IsFromMe: self, ReplyToMessageID: parent, Snippet: sql.NullString{String: body, Valid: true}, SizeEstimate: int64(len(body)), PreserveAttachmentStats: true}
	messageID, err := r.imp.store.PersistMessageContext(ctx, &store.MessagePersistData{
		Message: message, Conversation: &store.ConversationPersistData{SourceConversationID: string(m.ConversationID), ConversationType: "direct_chat", Participants: participants, PreserveExistingParticipants: true},
		Metadata: &sql.NullString{String: string(meta), Valid: true}, BodyText: sql.NullString{String: body, Valid: true}, RawMIME: raw, RawFormat: messageFormat, Recipients: recipients, PreserveLabels: true, FTS: &store.FTSDoc{Body: body, FromAddr: fromName, ToAddrs: toName},
	})
	if err != nil {
		return err
	}
	r.sum.MessagesProcessed++
	if !exists {
		r.sum.MessagesAdded++
	}
	if parentKey != "" {
		if err := r.imp.store.SetReplyToContext(ctx, r.source.ID, key, parentKey); err != nil {
			return err
		}
	}
	if !m.media() {
		return nil
	}
	if exists {
		stored, err := r.imp.store.GetMessageContext(ctx, messageID)
		if err != nil {
			return err
		}
		for _, att := range stored.Attachments {
			if att.ContentHash != "" {
				return nil
			}
		}
	}
	return r.attach(ctx, messageID, m)
}

func (r *messageRun) attach(ctx context.Context, messageID int64, message BusinessMessage) error {
	kind := message.Kind()
	role := store.AttachmentRoleStandalone
	if kind == "sticker" || kind == "emoji" {
		role = store.AttachmentRoleSticker
	}
	write := store.AttachmentWrite{Filename: "tiktok-" + kind + ".bin", MediaType: kind, SourcePartKey: "tiktok:" + kind, Role: role, RoleSource: store.AttachmentRoleSourceProviderExplicit, State: attachmentpolicy.StatePending}
	if err := r.imp.store.UpsertAttachmentRecordWithStats(ctx, messageID, write, true); err != nil {
		return err
	}
	if r.imp.options.AttachmentsDir == "" {
		return errors.New("TikTok media needs an attachment directory")
	}
	r.sum.Requests++
	media, err := r.imp.api.OpenMessageMedia(ctx, r.account, message, r.imp.options.MaxMediaBytes)
	if err != nil {
		return err
	}
	defer func() { _ = media.Body.Close() }()
	storage, hash, size, err := export.StoreAttachmentStream(ctx, r.imp.options.AttachmentsDir, io.LimitReader(media.Body, r.imp.options.MaxMediaBytes+1), r.imp.options.MaxMediaBytes)
	if err != nil {
		return err
	}
	if err := durableMedia(filepath.Join(r.imp.options.AttachmentsDir, filepath.FromSlash(storage))); err != nil {
		return err
	}
	write.StoragePath, write.ContentHash, write.Size, write.MIMEType, write.State = storage, hash, size, media.ContentType, attachmentpolicy.StateStored
	return r.imp.store.UpsertAttachmentRecordWithStats(ctx, messageID, write, true)
}

func durableMedia(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errors.Join(err, closeErr)
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	for _, dir := range []string{filepath.Dir(path), filepath.Dir(filepath.Dir(path))} {
		d, err := os.Open(dir)
		if err != nil {
			return err
		}
		err = d.Sync()
		closeErr = d.Close()
		if err != nil || closeErr != nil {
			return errors.Join(err, closeErr)
		}
	}
	return nil
}
