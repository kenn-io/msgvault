package meta

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/url"

	"go.kenn.io/msgvault/internal/beeperidentity"
	"go.kenn.io/msgvault/internal/store"
)

const (
	FacebookMessagesSourceType  = "facebook_messages"
	InstagramMessagesSourceType = "instagram_messages"
	inboxRawFormat              = "meta-message-json"
)

// Inbox selects a Page's Messenger inbox, or the inbox of the Instagram
// professional account linked to it when Account is set.
type Inbox struct {
	App, Page, Account string
}

func (b Inbox) instagram() bool { return b.Account != "" }

func (b Inbox) SourceType() string {
	if b.instagram() {
		return InstagramMessagesSourceType
	}
	return FacebookMessagesSourceType
}

func (b Inbox) Identifier() string {
	if b.instagram() {
		return InstagramSourceIdentifier(b.App, b.Page, b.Account)
	}
	return SourceIdentifier(b.App, b.Page)
}

// owner is the Page or Instagram account ID that the inbox belongs to.
func (b Inbox) owner() string {
	if b.instagram() {
		return b.Account
	}
	return b.Page
}

func (b Inbox) platform() string {
	if b.instagram() {
		return "instagram"
	}
	return "messenger"
}

// actorIdentifier scopes an inbox participant by app and inbox owner. Page-
// and Instagram-scoped IDs are not comparable with comment author IDs.
func (b Inbox) actorIdentifier(actor string) string {
	return b.SourceType() + ":" + beeperidentity.EncodePart(b.App) + ":" + beeperidentity.EncodePart(b.owner()) + ":" + beeperidentity.EncodePart(actor)
}

// InboxParticipant is a Graph conversation participant or message sender.
type InboxParticipant struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Username string `json:"username,omitempty"`
	Email    string `json:"email,omitempty"`
}

// InboxMessage is one message as polling reads it.
type InboxMessage struct {
	ID          string           `json:"id"`
	CreatedTime string           `json:"created_time"`
	From        InboxParticipant `json:"from"`
	Message     string           `json:"message"`
}

// InboxConversation is a conversation with its most recent messages.
type InboxConversation struct {
	ID           string
	Participants []InboxParticipant
	Messages     []InboxMessage
}

type InboxConversationPage struct {
	Data  []InboxConversation
	After string
}

// InboxConversations reads one page of conversations with each one's most
// recent messages. Meta returns at most the 20 most recent messages of a
// conversation, so older history is not available.
func (c *Client) InboxConversations(ctx context.Context, page, platform, after string) (InboxConversationPage, error) {
	if !ValidAccountID(page) || (platform != "messenger" && platform != "instagram") {
		return InboxConversationPage{}, errors.New("invalid inbox Page or platform")
	}
	type wire struct {
		ID           string `json:"id"`
		Participants struct {
			Data []InboxParticipant `json:"data"`
		} `json:"participants"`
		Messages struct {
			Data []InboxMessage `json:"data"`
		} `json:"messages"`
	}
	got, err := readObjectPage(ctx, c, page+"/conversations", after, url.Values{
		"platform": {platform}, "limit": {"25"},
		"fields": {"id,participants,messages.limit(20){id,created_time,from,message}"},
	}, func(v wire) string { return v.ID }, false)
	if err != nil {
		return InboxConversationPage{}, err
	}
	out := InboxConversationPage{After: got.After}
	for _, v := range got.Data {
		for _, m := range v.Messages.Data {
			if !safeObjectID(m.ID) {
				return InboxConversationPage{}, errors.New("invalid Graph message identity")
			}
		}
		out.Data = append(out.Data, InboxConversation{ID: v.ID, Participants: v.Participants.Data, Messages: v.Messages.Data})
	}
	return out, nil
}

// inboxMetadata records which inbox a message came from.
type inboxMetadata struct {
	AppID              string `json:"app_id"`
	PageID             string `json:"page_id"`
	InstagramAccountID string `json:"instagram_account_id,omitempty"`
	CustomerID         string `json:"customer_id"`
}

// persistMessage archives one inbox message in the chat with customer,
// keyed by the message ID so a later read updates it in place. The customer's
// scoped ID is the conversation key because webhooks name the customer but
// not Meta's conversation ID.
func (b Inbox) persistMessage(ctx context.Context, st *store.Store, sourceID int64, sum *ImportSummary, owner, customer InboxParticipant, msg InboxMessage) error {
	if owner.ID != b.owner() || !safeObjectID(customer.ID) || customer.ID == owner.ID || !safeObjectID(msg.ID) {
		return errors.New("invalid inbox message identity")
	}
	existing, err := st.StoredMessagesContext(ctx, sourceID, inboxRawFormat, []string{msg.ID})
	if err != nil {
		return err
	}
	var prior *store.StoredMessage
	var priorMetadata sql.NullString
	if row, ok := existing[msg.ID]; ok {
		prior = &row
		if priorMetadata, err = st.GetMessageMetadataContext(ctx, row.ID); err != nil {
			return err
		}
	}
	customerID, err := b.participant(ctx, st, sourceID, customer)
	if err != nil {
		return err
	}
	ownerID, err := b.participant(ctx, st, sourceID, owner)
	if err != nil {
		return err
	}
	// A message whose sender is missing or unknown keeps any author already
	// archived for it instead of being credited to the inbox owner.
	var sender int64
	var fromName string
	switch msg.From.ID {
	case owner.ID:
		sender, fromName = ownerID, owner.label()
	case customer.ID:
		sender, fromName = customerID, customer.label()
	}
	metadata, err := json.Marshal(inboxMetadata{AppID: b.App, PageID: b.Page, InstagramAccountID: b.Account, CustomerID: customer.ID})
	if err != nil {
		return err
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return archiveObject(ctx, st, sum, archivedObject{
		sourceID: sourceID, messageType: b.SourceType(), format: inboxRawFormat, conversationType: "direct_chat",
		obj: Object{ID: msg.ID, Message: msg.Message, CreatedTime: msg.CreatedTime}, conversation: customer.ID, title: customer.label(),
		sender: sender, fromName: fromName, self: sender != 0 && sender == ownerID, members: []int64{customerID, ownerID},
		metadata: metadata, raw: raw,
	}, prior, priorMetadata)
}

func (p InboxParticipant) label() string {
	if p.Name != "" {
		return p.Name
	}
	return p.Username
}

func (b Inbox) participant(ctx context.Context, st *store.Store, sourceID int64, p InboxParticipant) (int64, error) {
	service := "facebook"
	if b.instagram() {
		service = "instagram"
	}
	return resolveScopedActor(ctx, st, sourceID, b.SourceType(), service, b.actorIdentifier(p.ID), &Actor{ID: p.ID, Name: p.label()})
}

// InboxAPI reads a Page's or linked Instagram account's inbox.
type InboxAPI interface {
	VerifyPage(ctx context.Context, pageID string) (Page, error)
	LinkedInstagramAccount(ctx context.Context, pageID, accountID string) (IGUser, error)
	InboxConversations(ctx context.Context, page, platform, after string) (InboxConversationPage, error)
}

type InboxImporter struct {
	store *store.Store
	api   InboxAPI
}

func NewInboxImporter(st *store.Store, api InboxAPI) *InboxImporter {
	return &InboxImporter{store: st, api: api}
}

// Register verifies the Page, and the linked account for an Instagram inbox,
// before any archive write.
func (imp *InboxImporter) Register(ctx context.Context, b Inbox) (*store.Source, error) {
	if !ValidAccountID(b.App) || !ValidAccountID(b.Page) || (b.instagram() && !ValidAccountID(b.Account)) {
		return nil, errors.New("invalid Facebook app, Page or Instagram account ID")
	}
	page, err := imp.api.VerifyPage(ctx, b.Page)
	if err != nil {
		return nil, err
	}
	name, provenance := page.Name, "facebook_page_token"
	if b.instagram() {
		account, err := imp.api.LinkedInstagramAccount(ctx, b.Page, b.Account)
		if err != nil {
			return nil, err
		}
		name, provenance = account.Username, "instagram_page_token"
	}
	return registerSource(ctx, imp.store, b.SourceType(), b.Identifier(), b.actorIdentifier(b.owner()), provenance, name)
}

// Import archives inbox conversations one page of up to 25 at a time, each
// with its most recent messages. The checkpoint is the next page cursor.
func (imp *InboxImporter) Import(ctx context.Context, b Inbox, opts ImportOptions) (*ImportSummary, error) {
	return pollPages(ctx, imp.store, b.SourceType(), b.Identifier(), opts, func(st *store.Store, source *store.Source, sum *ImportSummary, after string) (string, error) {
		page, err := imp.api.InboxConversations(ctx, b.Page, b.platform(), after)
		if err != nil {
			return "", err
		}
		for _, conversation := range page.Data {
			var owner, customer InboxParticipant
			for _, p := range conversation.Participants {
				if p.ID == b.owner() {
					owner = p
				} else if customer.ID == "" {
					customer = p
				}
			}
			// Meta reads the inbox of the account linked to the Page now. If
			// the Page was linked to another account, its conversations do
			// not include the configured one; refuse them instead of
			// archiving another account's messages in this source.
			if owner.ID == "" {
				return "", fmt.Errorf("conversation %q does not include inbox owner %s; check which account the Page is linked to", conversation.ID, b.owner())
			}
			for _, msg := range conversation.Messages {
				if err := b.persistMessage(ctx, st, source.ID, sum, owner, customer, msg); err != nil {
					return "", fmt.Errorf("conversation %q message %q: %w", conversation.ID, msg.ID, err)
				}
			}
		}
		return page.After, nil
	})
}
