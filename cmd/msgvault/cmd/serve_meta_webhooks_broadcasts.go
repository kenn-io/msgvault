package cmd

import (
	"context"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/metawebhook"
	"go.kenn.io/msgvault/internal/store"
)

func registerBroadcastWebhookHandlers(reg *metawebhook.Registry, cfg *config.Config, st *store.Store) {
	handler := func(ctx context.Context, app string, items []metawebhook.Item) []metawebhook.ItemOutcome {
		out := make([]metawebhook.ItemOutcome, len(items))
		for i, item := range items {
			out[i] = metawebhook.ItemOutcome{Class: metawebhook.Unrouted}
			if item.AppID != app || item.Object != "page" {
				continue
			}
			for _, p := range cfg.MessengerBroadcasts {
				if !p.Enabled || p.AppID != app || p.PageID != item.EntryID {
					continue
				}
				var err error
				if item.Field == "messaging_optins" {
					out[i], err = meta.HandleMarketingOptIn(ctx, st, item)
				} else {
					out[i], err = meta.HandleMarketingMessageEvent(ctx, st, item.Field, item)
				}
				if err != nil {
					out[i] = metawebhook.ItemOutcome{Class: metawebhook.Retry, ErrorClass: "archive_unavailable", CountAttempt: true}
				}
				break
			}
		}
		return out
	}
	for _, field := range []string{"messaging_optins", "marketing_message_echoes", "marketing_message_deliveries", "marketing_message_reads", "marketing_message_clicks", "marketing_message_delivery_failed"} {
		reg.Register("page", field, handler)
	}
}
