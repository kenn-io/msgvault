package cmd

import (
	"context"
	"strings"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/emailtags"
)

func (a *storeAPIAdapter) archiveBeeperChat(ctx context.Context, id int64, change emailtags.ArchiveChange, grant *agentgrant.Grant, _ func(context.Context) (func(), error)) (*emailtags.ArchiveResult, error) {
	sourceID, chatID, err := a.store.MessageArchiveChatContext(ctx, id)
	if err != nil {
		return nil, err
	}
	source, err := a.store.GetSourceByIDContext(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	if grant != nil {
		selected, ok := grant.MatchingSource(agentgrant.SourceRef{Type: source.SourceType, Identifier: source.Identifier})
		// A message-level sender filter cannot authorize a whole-chat mutation.
		if !ok || len(selected.IncomingSenderKeys) != 0 {
			return nil, emailtags.Failure("message_not_found", "Message not found", nil, nil)
		}
	}
	if strings.HasPrefix(chatID, "msgvault-archive:") {
		return nil, emailtags.Failure("stale_identity", "Chat was archived by a source merge", nil, nil)
	}
	if !change.WholeChat {
		return nil, emailtags.Failure("invalid_request", "Beeper archives entire chats; set whole_chat=true", nil, nil)
	}
	token, err := beeper.LoadToken(a.config.TokensDir())
	if err != nil {
		return nil, emailtags.Failure("provider_unavailable", "Cannot connect to Beeper", nil, err)
	}
	client := beeperClient(a.config, token)
	read := func() (*beeper.Chat, error) {
		chat, err := client.GetChat(ctx, chatID)
		if err != nil {
			return nil, emailtags.Failure("provider_unavailable", "Cannot read Beeper chat", nil, err)
		}
		if chat.ID != chatID || chat.AccountID != source.Identifier || chat.MergedIntoChatID != "" || chat.Merge != nil {
			return nil, emailtags.Failure("stale_identity", "Beeper chat identity changed; sync and retry", nil, nil)
		}
		if chat.IsArchived == nil {
			return nil, emailtags.Failure("archive_unsupported", "Beeper does not report chat archive state", nil, nil)
		}
		return chat, nil
	}
	before, err := read()
	if err != nil {
		return nil, err
	}
	result := &emailtags.ArchiveResult{MessageID: id, SourceID: sourceID, ChatID: chatID, Archived: *change.Archived, Changed: *before.IsArchived != *change.Archived, DryRun: change.DryRun}
	if change.DryRun {
		return result, nil
	}
	if result.Changed {
		if err := client.ArchiveChat(ctx, chatID, *change.Archived); err != nil {
			return nil, emailtags.Failure("remote_unknown", "Beeper archive outcome is unknown; retry the desired state", nil, err)
		}
		after, err := read()
		if err != nil {
			return nil, emailtags.Failure("remote_unknown", "Cannot verify Beeper archive state; retry the desired state", nil, err)
		}
		if *after.IsArchived != *change.Archived {
			return nil, emailtags.Failure("remote_unknown", "Beeper archive state did not match; retry the desired state", nil, nil)
		}
	}
	result.Verified = true
	return result, nil
}
