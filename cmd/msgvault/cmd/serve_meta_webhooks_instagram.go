package cmd

import (
	"context"
	"encoding/json/v2"
	"os"
	"strings"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/metawebhook"
	"go.kenn.io/msgvault/internal/store"
)

func registerInstagramWebhookHandlers(reg *metawebhook.Registry, cfg *config.Config, st *store.Store) {
	handler := instagramCommentHandler(cfg, st, func(token string) (meta.InstagramObjectReader, error) { return meta.NewClient(token) })
	reg.Register("instagram", "comments", handler)
	reg.Register("instagram", "live_comments", handler)
}

func instagramCommentHandler(cfg *config.Config, st *store.Store, newReader func(string) (meta.InstagramObjectReader, error)) metawebhook.ItemHandler {
	return func(ctx context.Context, app string, items []metawebhook.Item) []metawebhook.ItemOutcome {
		out := make([]metawebhook.ItemOutcome, len(items))
		groups := map[string][]int{}
		accounts := []string{}
		for i, item := range items {
			if _, exists := groups[item.EntryID]; !exists {
				accounts = append(accounts, item.EntryID)
			}
			groups[item.EntryID] = append(groups[item.EntryID], i)
		}
		for _, account := range accounts {
			indices := groups[account]
			set := func(class metawebhook.Class, reason string) {
				for _, i := range indices {
					out[i] = metawebhook.ItemOutcome{Class: class, ErrorClass: reason, CountAttempt: class == metawebhook.Retry}
				}
			}
			var profile *config.InstagramCommentsSource
			matches := 0
			for _, source := range cfg.InstagramComments {
				if source.Enabled && source.AppID == app && source.InstagramAccountID == account {
					profile = &source
					matches++
				}
			}
			if matches != 1 {
				reason := "profile_missing"
				if matches > 1 {
					reason = "profile_ambiguous"
				}
				set(metawebhook.Unrouted, reason)
				continue
			}
			events, valid := []meta.InstagramCommentEvent{}, []int{}
			for _, i := range indices {
				var event meta.InstagramCommentEvent
				if json.Unmarshal(items[i].Value, &event) != nil {
					out[i] = metawebhook.ItemOutcome{Class: metawebhook.Invalid, ErrorClass: "invalid_comment"}
					continue
				}
				event.Time = items[i].Time
				events, valid = append(events, event), append(valid, i)
			}
			if len(valid) == 0 {
				continue
			}
			// Credential failures affect valid items only; malformed items remain invalid.
			indices = valid
			token := os.Getenv(profile.TokenEnv)
			if strings.TrimSpace(token) == "" {
				set(metawebhook.Retry, "token_missing")
				continue
			}
			reader, err := newReader(token)
			if err != nil || reader == nil {
				set(metawebhook.Retry, "token_invalid")
				continue
			}
			results, _ := meta.NewInstagramEventImporter(st, reader).ApplyEvents(ctx, app, profile.PageID, account, events, meta.EventOptions{})
			for j, i := range valid {
				out[i] = metawebhook.ItemOutcome{Class: metawebhook.Retry, ErrorClass: "archive_unavailable", CountAttempt: true}
				if j < len(results) {
					result := results[j]
					out[i] = metawebhook.ItemOutcome{Class: metawebhook.Class(result.Class), ErrorClass: result.ErrorClass, CountAttempt: result.CountAttempt, Checkpoint: result.Class == "applied" || result.Class == "ignored", ArchiveChanged: result.ArchiveChanged}
				}
			}
		}
		return out
	}
}
