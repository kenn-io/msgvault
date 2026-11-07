package tiktok

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

func resolveActor(ctx context.Context, st *store.Store, sourceID int64, app, account, actor, name string) (int64, error) {
	if actor == "" {
		return 0, nil
	}
	if !ValidID(actor) {
		return 0, errors.New("invalid TikTok participant identity")
	}
	key := ActorIdentifier(app, account, actor)
	id, err := st.EnsureParticipantByIdentifierContext(ctx, "tiktok", key, name)
	if err != nil {
		return 0, err
	}
	serviceName := "tiktok"
	service, _, err := st.EnsureCommunicationServiceContext(ctx, store.CommunicationServiceInput{Slug: serviceName, DisplayLabel: "TikTok", ScopePolicy: store.ScopePolicyNone, Normalization: store.NormalizationNone, NormalizationVersion: 1})
	if err != nil {
		return 0, err
	}
	if err := st.ClassifyParticipantIdentifierServiceContext(ctx, "tiktok", key, &service.ID, nil, nil); err != nil {
		return 0, err
	}
	_, err = st.RecordContactObservationContext(ctx, id, store.ParticipantContactObservationInput{
		SourceID: &sourceID, AddressKind: store.ContactAddressProviderIdentity, ServiceSlug: &serviceName, ProviderUserID: &key, OriginalValue: key,
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation, SourceRef: &key},
	})
	return id, err
}

type commentMetadata struct {
	App     string `json:"app"`
	Account string `json:"account"`
	Video   string `json:"video"`
	Parent  string `json:"parent,omitempty"`
}

func (r *commentRun) persistVideo(ctx context.Context, v Video) error {
	return r.persist(ctx, "video:"+string(v.ID), v.Caption, string(v.ID), "", r.account, "", int64(v.CreateTime), true, v)
}
func (r *commentRun) persistComment(ctx context.Context, c Comment, video, parent string) error {
	if !ValidID(string(c.ID)) || (c.VideoID != "" && string(c.VideoID) != video) {
		return errors.New("TikTok comment identity or video mismatch")
	}
	if c.ParentID != "" && string(c.ParentID) != "0" {
		if parent == "" || string(c.ParentID) != parent {
			return errors.New("TikTok reply parent mismatch")
		}
	}
	if parent == string(c.ID) {
		return errors.New("TikTok comment cannot reply to itself")
	}
	if parent == "" {
		parent = "video:" + video
	}
	name := c.DisplayName
	if name == "" {
		name = c.Username
	}
	actor := string(c.ActorID)
	// owner is provider evidence of self, unlike a matching display name.
	if c.Owner {
		actor = r.account
	}
	return r.persist(ctx, string(c.ID), c.Text, video, parent, actor, name, int64(c.CreateTime), c.Owner, c)
}
func (r *commentRun) persist(ctx context.Context, id, body, video, parent, actor, name string, sent int64, self bool, raw any) error {
	existing, err := r.imp.store.StoredMessagesContext(ctx, r.source.ID, commentFormat, []string{id, parent})
	if err != nil {
		return err
	}
	metadata := commentMetadata{App: r.app, Account: r.account, Video: video, Parent: parent}
	prior, exists := existing[id]
	if exists {
		saved, err := r.imp.store.GetMessageMetadataContext(ctx, prior.ID)
		if err != nil {
			return err
		}
		var previous commentMetadata
		if !saved.Valid || json.Unmarshal([]byte(saved.String), &previous) != nil || previous != metadata {
			return errors.New("TikTok comment belongs to a different thread or parent")
		}
	}
	var reply sql.NullInt64
	if parent != "" {
		row, ok := existing[parent]
		if !ok {
			return errors.New("TikTok comment parent is not archived")
		}
		if exists && prior.ConversationID != row.ConversationID {
			return errors.New("TikTok reply crosses conversations")
		}
		reply = sql.NullInt64{Int64: row.ID, Valid: true}
	}
	sender, err := resolveActor(ctx, r.imp.store, r.source.ID, r.app, r.account, actor, name)
	if err != nil {
		return err
	}
	if sender == 0 && exists {
		sender = prior.SenderID.Int64
		self = prior.SourceIsFromMe.Bool
	}
	var recipients []store.RecipientSet
	var participants []store.ConversationParticipantRef
	if sender != 0 {
		recipients = []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{sender}, DisplayNames: []string{name}}}
		participants = []store.ConversationParticipantRef{{ParticipantID: sender, Role: "member"}}
	}
	meta, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	var sentAt sql.NullTime
	if sent > 0 {
		sentAt = sql.NullTime{Time: time.Unix(sent, 0).UTC(), Valid: true}
	} else if exists {
		sentAt = prior.SentAt
	}
	title := ""
	if parent == "" {
		title = body
		if title == "" {
			title = "TikTok video " + video
		}
	}
	_, err = r.imp.store.PersistMessageContext(ctx, &store.MessagePersistData{
		Message:      &store.Message{SourceID: r.source.ID, SourceMessageID: id, MessageType: CommentsSourceType, SentAt: sentAt, SenderID: sql.NullInt64{Int64: sender, Valid: sender != 0}, IsFromMe: self, ReplyToMessageID: reply, Snippet: sql.NullString{String: body, Valid: true}, SizeEstimate: int64(len(body))},
		Conversation: &store.ConversationPersistData{SourceConversationID: video, ConversationType: "public_comments", Title: title, Participants: participants, PreserveExistingParticipants: true},
		Metadata:     &sql.NullString{String: string(meta), Valid: true}, BodyText: sql.NullString{String: body, Valid: true}, RawMIME: data, RawFormat: commentFormat, Recipients: recipients, PreserveLabels: true,
		FTS: &store.FTSDoc{Body: body, FromAddr: name},
	})
	if err != nil {
		return err
	}
	r.sum.MessagesProcessed++
	if !exists {
		r.sum.MessagesAdded++
	}
	return nil
}
