package agentdemo

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/fakevault"
	"go.kenn.io/msgvault/internal/store"
)

type chatScope struct {
	handle, source, typ, messageType, title, owner string
	members                                        []string
	total, own                                     int
}

var chatScopes = []chatScope{
	{"weekend_crew", "whatsapp", "group_chat", "whatsapp", "Weekend crew", "owner_phone", []string{"owner_phone", "sam_phone", "casey", "filler_one", "filler_two"}, 30, 8},
	{"sam_signal_dm", "signal", "direct_chat", "beeper", "Sam Rivera", "owner_phone", []string{"owner_phone", "sam_phone"}, 20, 7},
	{"sam_imessage_dm", "imessage", "direct_chat", "imessage", "Sam Rivera", "owner_phone", []string{"owner_phone", "sam_phone"}, 6, 2},
	{"casey_sms", "imessage", "direct_chat", "sms", "Casey Nguyen", "owner_phone", []string{"owner_phone", "casey"}, 1, 0},
	{"priya_slack_dm", "slack", "direct_chat", "slack", "Priya Shah", "owner_slack", []string{"owner_slack", "priya_slack"}, 1, 0},
	{"slack_general", "slack", "channel", "slack", "general", "owner_slack", []string{"owner_slack", "priya_slack", "slackbot"}, 85, 0},
	{"shortcode_sms", "imessage", "direct_chat", "sms", "72345", "owner_phone", []string{"owner_phone", "shortcode"}, 3, 0},
	{"dinner_group", "imessage", "group_chat", "imessage", "Dinner plans", "owner_phone", []string{"owner_phone", "sam_phone", "casey"}, 6, 2},
}

func (b *builder) persistChat(ctx context.Context, c chatScope, h, sender, body string, at time.Time) error {
	roster := make([]store.ConversationParticipantRef, 0, len(c.members))
	recipients := []int64{}
	for _, p := range c.members {
		role := "member"
		if p == c.owner {
			role = "owner"
		}
		roster = append(roster, store.ConversationParticipantRef{ParticipantID: b.participant(p), Role: role})
		if p != sender {
			recipients = append(recipients, b.participant(p))
		}
	}
	id, err := b.st.PersistMessageWithParticipantsContext(ctx, nil, func(_ []int64) *store.MessagePersistData {
		return &store.MessagePersistData{
			Message:      &store.Message{SourceID: b.source(c.source), SourceMessageID: h, MessageType: c.messageType, SenderID: sql.NullInt64{Int64: b.participant(sender), Valid: true}, IsFromMe: sender == c.owner, IsSent: sql.NullBool{Bool: true, Valid: true}, SentAt: sql.NullTime{Time: at, Valid: true}, Snippet: sql.NullString{String: body, Valid: true}, SizeEstimate: int64(len(body))},
			Conversation: &store.ConversationPersistData{SourceConversationID: c.handle, ConversationType: c.typ, Title: c.title, Participants: roster},
			BodyText:     sql.NullString{String: body, Valid: true}, Recipients: []store.RecipientSet{{Type: "to", ParticipantIDs: recipients}}, FTS: &store.FTSDoc{Body: body},
		}
	})
	if err != nil {
		return fmt.Errorf("chat %s: %w", h, err)
	}
	var conv int64
	if err = b.st.DB().QueryRowContext(ctx, "SELECT conversation_id FROM messages WHERE id=?", id).Scan(&conv); err != nil {
		return err
	}
	b.m.Messages[h] = id
	b.m.Conversations[c.handle] = Conversation{conv, c.source, c.typ}
	return nil
}
func chats(ctx context.Context, b *builder) error {
	automated := b.automatedMessages
	for _, source := range []string{"whatsapp", "signal", "imessage", "slack"} {
		run, err := b.st.StartSyncContext(ctx, b.source(source), "full")
		if err != nil {
			return err
		}
		for _, c := range chatScopes {
			if c.source != source {
				continue
			}
			for i := 0; i < c.total-c.own; i++ {
				h := fmt.Sprintf("%s_in_%d", c.handle, i+1)
				sender := c.members[1]
				body := filler(b.opts.Seed, 1000+i)
				at := b.m.AsOf.Add(-time.Duration(30-i) * 24 * time.Hour)
				switch c.handle {
				case "weekend_crew":
					sender = c.members[1+i%(len(c.members)-1)]
					if i == c.total-c.own-1 {
						h = "weekend_crew_question"
						body = "Can you join the weekend walk?"
						at = b.m.AsOf.Add(-48 * time.Hour)
					}
				case "sam_signal_dm":
					if i == c.total-c.own-1 {
						h = "sam_signal_question"
						body = "hey, can you confirm the train time?"
						at = b.m.AsOf.Add(-48 * time.Hour)
					}
				case "sam_imessage_dm":
					body = "The travel notes are ready."
					at = b.m.AsOf.Add(-time.Duration(10-i) * 24 * time.Hour)
				case "casey_sms":
					h = "casey_sms_question"
					sender = c.owner
					body = "Can you confirm the station pickup?"
					at = b.m.AsOf.Add(-72 * time.Hour)
				case "priya_slack_dm":
					h = "priya_contract_share"
					sender = c.owner
					body = "The contract version two is attached."
					at = b.m.AsOf.Add(-10 * 24 * time.Hour)
				case "slack_general":
					at = b.m.AsOf.Add(-time.Duration(120-i) * 24 * time.Hour)
					if i >= 80 {
						h = fmt.Sprintf("deploy_bot_%d", i-79)
						sender = "slackbot"
						body = "Deployment complete."
						automated = append(automated, h)
					}
				case "shortcode_sms":
					h = fmt.Sprintf("shortcode_%d", i+1)
					body = "Your synthetic verification code is 123456."
					automated = append(automated, h)
				case "dinner_group":
					sender = c.members[1+i%2]
					body = "dinner at the usual place"
					at = b.m.AsOf.Add(-time.Duration(8-i) * 24 * time.Hour)
				}
				if err = b.persistChat(ctx, c, h, sender, body, at); err != nil {
					return err
				}
			}
		}
		if err = b.st.CompleteSyncContext(ctx, run, ""); err != nil {
			return err
		}
		if source == "signal" {
			run, err = b.st.StartSyncContext(ctx, b.source(source), "incremental")
			if err != nil {
				return err
			}
			if err = b.st.FailSyncWithCheckpointContext(ctx, run, "synthetic interrupted sync", &store.Checkpoint{PageToken: "demo-signal-page-2", MessagesProcessed: 20, MessagesAdded: 20}); err != nil {
				return err
			}
		}
	}
	hash, path, err := fakevault.WriteBlob(b.attachmentsDir, contractV2Bytes())
	if err != nil {
		return err
	}
	if err = b.st.UpsertAttachmentRecord(ctx, b.message("priya_contract_share"), store.AttachmentWrite{Filename: "contract-v2.pdf", MIMEType: "application/pdf", ContentHash: hash, StoragePath: path, Size: int64(len(contractV2Bytes())), MediaType: "document", Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics}); err != nil {
		return err
	}
	a := b.m.Attachments["contract_v2"]
	a.MessageHandles = append(a.MessageHandles, "priya_contract_share")
	b.m.Attachments["contract_v2"] = a
	b.m.Expect["automated_messages"] = automated
	b.m.Expect["group_chat_questions_never_loops"] = []string{"weekend_crew_question"}
	b.m.Expect["interrupted_sources"] = []string{"signal"}
	b.m.Expect["persons_spanning_channels"] = map[string]any{"sam": []string{"email", "whatsapp", "signal", "imessage"}}
	return nil
}
