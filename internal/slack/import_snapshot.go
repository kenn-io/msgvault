package slack

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/store"
)

type slackdumpParticipantPlan struct {
	participants []store.ParticipantPersistData
	byKey        map[string]int
}

func (p *slackdumpParticipantPlan) user(resolver *participantResolver, id string) int {
	if id == "" {
		return -1
	}
	key := "user:" + id
	if index, ok := p.byKey[key]; ok {
		return index
	}
	user, known := resolver.users[id]
	participant := store.ParticipantPersistData{}
	if known {
		participant.DisplayName = user.DisplayName()
	}
	if known && strings.Contains(user.Profile.Email, "@") && !user.IsBot {
		participant.EmailAddress = strings.ToLower(user.Profile.Email)
		participant.Domain = participant.EmailAddress[strings.LastIndex(participant.EmailAddress, "@")+1:]
		participant.Identifiers = []store.ParticipantIdentifierRef{{Type: participantIdentifierType, Value: resolver.identifierValue(id), MergeExisting: true}}
	} else {
		participant.IdentifierType = participantIdentifierType
		participant.IdentifierValue = resolver.identifierValue(id)
	}
	return p.add(key, participant)
}

func (p *slackdumpParticipantPlan) bot(resolver *participantResolver, id, name string) int {
	if id == "" {
		return -1
	}
	key := "bot:" + id
	if index, ok := p.byKey[key]; ok {
		return index
	}
	return p.add(key, store.ParticipantPersistData{IdentifierType: participantIdentifierType, IdentifierValue: resolver.identifierValue(key), DisplayName: name})
}

func (p *slackdumpParticipantPlan) add(key string, participant store.ParticipantPersistData) int {
	if p.byKey == nil {
		p.byKey = make(map[string]int)
	}
	index := len(p.participants)
	p.participants = append(p.participants, participant)
	p.byKey[key] = index
	return index
}

func slackdumpNativeMemberIDs(conversation *Conversation, me string) []string {
	if conversation.IsIM {
		return []string{conversation.User, me}
	}
	return conversation.Members
}

func slackdumpNativeMembershipCount(conversation *Conversation, me string, policy attachmentpolicy.Policy) int {
	seen := make(map[string]bool)
	for _, id := range slackdumpNativeMemberIDs(conversation, me) {
		if id != "" {
			seen[id] = true
		}
	}
	if len(seen) == 0 && conversation.NumMembers <= 0 && !conversation.IsIM {
		return unknownMembershipCount(policy)
	}
	return max(len(seen), conversation.NumMembers)
}

// persistSlackdumpSnapshot consumes the shared native message mapping. Export
// identities, recipients, membership, reactions and raw/body rows commit before
// the import occurrence. Resolving descriptors performs no file or provider I/O.
// A nil attachment snapshot preserves silent imports' post-commit file handling.
func (imp *Importer) persistSlackdumpSnapshot(ctx context.Context, cc *convScope, conversation *Conversation, message *Message, mapped store.Message, text string, raw []byte, attachments *[]store.AttachmentRef) (int64, error) {
	plan := slackdumpParticipantPlan{}
	var memberIndices []int
	var memberNames []string
	seenMembers := make(map[string]bool)
	for _, id := range slackdumpNativeMemberIDs(conversation, cc.opts.UserID) {
		if id == "" || seenMembers[id] {
			continue
		}
		seenMembers[id] = true
		memberIndices = append(memberIndices, plan.user(imp.res, id))
		memberNames = append(memberNames, imp.res.displayName(id))
	}
	senderIndex := plan.user(imp.res, message.User)
	if message.User == "" {
		senderIndex = plan.bot(imp.res, message.BotID, message.Username)
	}
	mentionIDs := message.MentionedUserIDs()
	mentionIndices := make([]int, len(mentionIDs))
	for index, id := range mentionIDs {
		mentionIndices[index] = plan.user(imp.res, id)
	}
	type reactionPlan struct {
		participant int
		value       string
	}
	var reactionIndices []reactionPlan
	for _, reaction := range message.Reactions {
		for _, id := range reaction.Users {
			if index := plan.user(imp.res, id); index >= 0 {
				reactionIndices = append(reactionIndices, reactionPlan{participant: index, value: reaction.Name})
			}
		}
	}
	metadataBytes, err := json.Marshal(map[string]string{
		"slack_team_id": cc.opts.TeamID, "slack_user_id": cc.opts.UserID,
		"slack_channel_id": cc.channelID, "slack_message_ts": message.TS, "slack_thread_ts": message.ThreadTS,
	})
	if err != nil {
		return 0, fmt.Errorf("marshal Slack message metadata: %w", err)
	}
	metadata := sql.NullString{String: string(metadataBytes), Valid: true}
	return imp.store.PersistMessageWithParticipantsContext(ctx, plan.participants, func(ids []int64) *store.MessagePersistData {
		msg := mapped
		from := store.RecipientSet{Type: "from"}
		if senderIndex >= 0 {
			msg.SenderID = sql.NullInt64{Int64: ids[senderIndex], Valid: true}
			name := imp.res.displayName(message.User)
			if name == "" {
				name = message.Username
			}
			from.ParticipantIDs, from.DisplayNames = []int64{ids[senderIndex]}, []string{name}
		}
		recipients := []store.RecipientSet{from}
		// Missing group membership is not an observed empty recipient set.
		if conversation.IsIM || (conversation.IsMpim && len(conversation.Members) > 0) {
			to := store.RecipientSet{Type: "to"}
			for index, member := range memberIndices {
				if !msg.SenderID.Valid || ids[member] != msg.SenderID.Int64 {
					to.ParticipantIDs = append(to.ParticipantIDs, ids[member])
					to.DisplayNames = append(to.DisplayNames, memberNames[index])
				}
			}
			recipients = append(recipients, to)
		}
		mentions := store.RecipientSet{Type: "mention"}
		for index, participant := range mentionIndices {
			if participant >= 0 {
				mentions.ParticipantIDs = append(mentions.ParticipantIDs, ids[participant])
				mentions.DisplayNames = append(mentions.DisplayNames, imp.res.displayName(mentionIDs[index]))
			}
		}
		recipients = append(recipients, mentions)
		var members []store.ConversationParticipantRef
		for _, participant := range memberIndices {
			members = append(members, store.ConversationParticipantRef{ParticipantID: ids[participant], Role: "member"})
		}
		projection := &store.ConversationPersistData{SourceConversationID: conversation.ID, ConversationType: conversationType(conversation), Title: conversationTitle(conversation, imp.res.displayName), Participants: members}
		if len(memberIndices) == 0 && conversation.NumMembers <= 0 && !conversation.IsIM {
			projection.MemberCountUnknown = true
		} else {
			count := max(len(members), conversation.NumMembers)
			projection.MemberCount = &count
		}
		var reactions []store.ReactionRef
		for _, reaction := range reactionIndices {
			reactions = append(reactions, store.ReactionRef{ParticipantID: ids[reaction.participant], Type: "emoji", Value: reaction.value, CreatedAt: tsTime(message.TS)})
		}
		deleted := message.Subtype == "tombstone"
		data := &store.MessagePersistData{SlackAttachmentSnapshot: attachments, RequireReadySlackAttachments: attachments != nil, Message: &msg, Conversation: projection, Metadata: &metadata, PreserveLabels: true, BodyText: sql.NullString{String: text, Valid: text != ""}, RawMIME: raw, RawFormat: "slack_json", Recipients: recipients, ReactionSnapshot: &reactions, DeletedFromSource: &deleted}
		if message.IsThreadReply() {
			data.ReplyToSourceMessageID = sourceMessageID(cc.channelID, message.ThreadTS)
		}
		if message.Edited != nil {
			edited := true
			data.Edited = &edited
		}
		return data
	})
}
