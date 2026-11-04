package cmd

import (
	"context"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/metawebhook"
	"go.kenn.io/msgvault/internal/store"
)

func registerDMWebhookHandlers(reg *metawebhook.Registry, cfg *config.Config, st *store.Store) {
	handler := dmHandler(cfg, st)
	reg.Register("page", "messages", handler)
	reg.Register("page", "message_echoes", handler)
	reg.Register("page", "message_reactions", handler)
	reg.Register("page", "message_edits", handler)
	reg.Register("page", "standby", handler)
	reg.Register("instagram", "messages", handler)
	reg.Register("instagram", "message_reactions", handler)
}
func dmHandler(cfg *config.Config, st *store.Store) metawebhook.ItemHandler {
	return meta.NewDMHandler(st, func(ctx context.Context, app, object, entry string) (meta.DMSource, *store.Source, error) {
		profiles, platform := cfg.FacebookMessages, meta.PlatformMessenger
		switch object {
		case "page":
		case "instagram":
			profiles, platform = cfg.InstagramMessages, meta.PlatformInstagram
		default:
			return meta.DMSource{}, nil, meta.ErrDMSourceUnrouted
		}
		for _, p := range profiles {
			asset := p.PageID
			if platform == meta.PlatformInstagram {
				asset = p.InstagramAccountID
			}
			if !p.Enabled || p.AppID != app || asset != entry {
				continue
			}
			src := meta.DMSource{AppID: p.AppID, PageID: p.PageID, IGID: p.InstagramAccountID, Platform: platform}
			source, err := st.GetSourceByTypeAndIdentifier(src.Platform.SourceType(), src.Identifier())
			return src, source, err
		}
		return meta.DMSource{}, nil, meta.ErrDMSourceUnrouted
	}).Handle
}
