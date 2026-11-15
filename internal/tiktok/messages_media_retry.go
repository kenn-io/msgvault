package tiktok

import (
	"context"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/msgvault/internal/store"
)

const pendingMediaBudget = 20

type messageMediaOccurrence struct {
	messageID int64
	partKey   string
}

var errPendingMessageMedia = errors.New("TikTok media remains pending; run sync again to retry")

// retryMedia runs only after a manual pull's account preflight, never for a
// webhook. Advance past failed/invalid candidates so one bad item cannot starve
// later media; the next finite sweep visits those items again.
func (r *messageRun) retryMedia(ctx context.Context) error {
	state, err := r.imp.store.LoadTikTokMediaSweepContext(ctx, r.source.ID, r.source.Identifier)
	if err != nil {
		return err
	}
	if err := r.imp.store.SaveTikTokMediaSweepContext(ctx, r.source.ID, state); err != nil {
		return err
	}
	ids, err := r.imp.store.ListPendingTikTokMediaContext(ctx, r.source.ID, state.After, state.Through, pendingMediaBudget)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		item, err := r.imp.store.ReadTikTokMediaCandidateContext(ctx, r.source.ID, id)
		if err == nil {
			var message BusinessMessage
			message, err = r.mediaCandidate(item)
			if err == nil {
				// A cancellation before dispatch must leave this candidate eligible.
				if err := ctx.Err(); err != nil {
					return errors.Join(append(errs, err)...)
				}
				err = r.attach(ctx, id, message)
			}
		}
		if err != nil {
			errs = append(errs, err)
		}
		if ctx.Err() != nil {
			return errors.Join(append(errs, ctx.Err())...)
		}
		state.After = id
		if err := r.imp.store.SaveTikTokMediaSweepContext(ctx, r.source.ID, state); err != nil {
			return errors.Join(append(errs, err)...)
		}
	}
	if len(ids) < pendingMediaBudget {
		state.After = state.Through
		errs = append(errs, r.imp.store.SaveTikTokMediaSweepContext(ctx, r.source.ID, state))
	}
	return errors.Join(errs...)
}

func (r *messageRun) mediaCandidate(item store.TikTokMediaCandidate) (BusinessMessage, error) {
	var metadata messageMetadata
	var message BusinessMessage
	if json.Unmarshal([]byte(item.Metadata), &metadata) != nil || json.Unmarshal(item.Raw, &message) != nil ||
		metadata.App != r.app || metadata.Account != r.account || !ValidID(string(metadata.Conversation)) || !ValidID(string(metadata.Message)) ||
		string(metadata.Conversation) != item.Conversation || messageKey(metadata.Conversation, metadata.Message) != item.SourceMessageID ||
		message.ID != metadata.Message || (message.ConversationID != "" && message.ConversationID != metadata.Conversation) ||
		!message.media() || message.Kind() != metadata.Type || item.PartKey != "tiktok:"+message.Kind() {
		return message, errors.New("TikTok pending media identity conflicts with the archive")
	}
	if message.FromUser != nil && message.ToUser != nil && message.FromUser.ID != "" && message.ToUser.ID != "" && string(message.FromUser.ID) != r.account && string(message.ToUser.ID) != r.account {
		return message, errors.New("TikTok pending media does not involve the selected account")
	}
	// Pull payloads can omit conversation_id. Restore only the typed request
	// projection, leaving the provider's archived raw JSON unchanged.
	message.ConversationID = metadata.Conversation
	return message, nil
}
