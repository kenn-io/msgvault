package cmd

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/metawebhook"
	"go.kenn.io/msgvault/internal/store"
)

func registerMentionsWebhookHandlers(reg *metawebhook.Registry, cfg *config.Config, st *store.Store) {
	reg.Register("instagram", "mentions", func(ctx context.Context, _ string, items []metawebhook.Item) []metawebhook.ItemOutcome {
		out := make([]metawebhook.ItemOutcome, len(items))
		// Each resolution makes three Graph reads. Release the worker's operation
		// lease after at most 48 reads, leaving the backlog for another turn.
		resolutions := 0
		for i, item := range items {
			out[i] = metawebhook.ItemOutcome{Class: metawebhook.Unrouted, ErrorClass: "mentions_profile_missing"}
			var profile *config.InstagramCommentsSource
			matches := 0
			for _, source := range cfg.InstagramComments {
				if source.Enabled && source.Mentions && source.AppID == item.AppID && source.InstagramAccountID == item.EntryID {
					profile = &source
					matches++
				}
			}
			if matches != 1 {
				if matches > 1 {
					out[i].ErrorClass = "mentions_profile_ambiguous"
				}
				continue
			}
			event, err := meta.DecodeMentionChange(item.EntryID, item.Value)
			if err != nil {
				out[i] = metawebhook.ItemOutcome{Class: metawebhook.Invalid, ErrorClass: "invalid_mention"}
				continue
			}
			out[i] = metawebhook.ItemOutcome{Class: metawebhook.Retry, ErrorClass: "mentions_resolve_failed", CountAttempt: true}
			client, err := configuredInstagramCommentsClient(*profile)
			if err != nil {
				out[i].ErrorClass = "mentions_token_invalid"
				out[i].CountAttempt = false
				continue
			}
			if resolutions >= 16 {
				out[i] = metawebhook.ItemOutcome{Class: metawebhook.Retry, ErrorClass: "request_budget"}
				continue
			}
			resolutions++
			sum, err := meta.NewMentionImporter(st, client).Resolve(ctx, profile.AppID, profile.PageID, profile.InstagramAccountID, event)
			changed := sum != nil && sum.MessagesProcessed > 0
			out[i].ArchiveChanged = changed
			if err != nil {
				switch {
				case ctx.Err() != nil:
					out[i].ErrorClass = "cancelled"
					out[i].CountAttempt = false
				case errors.Is(err, store.ErrSyncAlreadyActive):
					out[i].ErrorClass = "source_busy"
					out[i].CountAttempt = false
				case errors.Is(err, store.ErrSourceNotFound):
					out[i] = metawebhook.ItemOutcome{Class: metawebhook.Unrouted, ErrorClass: "mentions_source_missing", ArchiveChanged: changed}
				}
				continue
			}
			out[i] = metawebhook.ItemOutcome{Class: metawebhook.Applied, Checkpoint: true, ArchiveChanged: changed}
			if sum.MessagesProcessed == 0 {
				out[i].Class = metawebhook.Ignored
			}
		}
		return out
	})
}
