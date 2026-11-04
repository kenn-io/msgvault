package meta

import (
	"context"

	"go.kenn.io/msgvault/internal/beeperidentity"
	"go.kenn.io/msgvault/internal/store"
)

// DMActorIdentifier keeps Page-scoped and IG-scoped customers separate from
// public comment authors, even when their provider IDs contain equal digits.
func DMActorIdentifier(src DMSource, actor string) string {
	asset := src.PageID
	if src.Platform == PlatformInstagram {
		asset = src.IGID
	}
	return src.Platform.SourceType() + ":" + beeperidentity.EncodePart(src.AppID) + ":" + beeperidentity.EncodePart(asset) + ":" + beeperidentity.EncodePart(actor)
}

func ResolveDMActor(ctx context.Context, st *store.Store, sourceID int64, src DMSource, actorID, display string) (int64, error) {
	if actorID == "" {
		return 0, nil
	}
	service := "facebook"
	if src.Platform == PlatformInstagram {
		service = "instagram"
	}
	return resolveScopedActor(ctx, st, sourceID, src.Platform.SourceType(), service, DMActorIdentifier(src, actorID), &Actor{ID: actorID, Name: display})
}
