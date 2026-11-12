package meta

import (
	"context"

	"go.kenn.io/msgvault/internal/beeperidentity"
	"go.kenn.io/msgvault/internal/store"
)

// ActorIdentifier is globally collision-safe across app, selected Page and
// actor scope. Display names and usernames never participate in identity.
func ActorIdentifier(app, page, actor string) string {
	return SourceType + ":" + beeperidentity.EncodePart(app) + ":" + beeperidentity.EncodePart(page) + ":" + beeperidentity.EncodePart(actor)
}

func resolveActor(ctx context.Context, st *store.Store, sourceID int64, app, page string, actor *Actor) (int64, error) {
	key := ""
	if actor != nil {
		key = ActorIdentifier(app, page, actor.ID)
	}
	return resolveScopedActor(ctx, st, sourceID, SourceType, "facebook", key, actor)
}

func instagramActorIdentifier(app, page, account, family, actor string) string {
	return InstagramSourceType + ":" + beeperidentity.EncodePart(app) + ":" + beeperidentity.EncodePart(page) + ":" +
		beeperidentity.EncodePart(account) + ":" + beeperidentity.EncodePart(family) + ":" + beeperidentity.EncodePart(actor)
}

func resolveScopedActor(ctx context.Context, st *store.Store, sourceID int64, kind, serviceName, key string, actor *Actor) (int64, error) {
	if actor == nil || actor.ID == "" {
		return 0, nil
	}
	id, err := st.EnsureParticipantByIdentifierContext(ctx, kind, key, actor.Name)
	if err != nil {
		return 0, err
	}
	service, err := st.ResolveCommunicationServiceContext(ctx, serviceName)
	if err != nil {
		return 0, err
	}
	// The catalog service has scope_policy=none. Scope lives in the immutable
	// anchor and source_id; changing global catalog semantics is unnecessary.
	if err = st.ClassifyParticipantIdentifierServiceContext(ctx, kind, key, &service.ID, nil, nil); err != nil {
		return 0, err
	}
	_, err = st.RecordContactObservationContext(ctx, id, store.ParticipantContactObservationInput{
		SourceID: &sourceID, AddressKind: store.ContactAddressProviderIdentity, ServiceSlug: &serviceName,
		ProviderUserID: &key, OriginalValue: key,
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation, SourceRef: &key},
	})
	return id, err
}

func (r *importRun) messageActor(ctx context.Context, obj Object) (int64, string, bool, error) {
	actor, family, self := obj.From, "", obj.From != nil && obj.From.ID == r.page
	if r.imp.instagram != "" {
		actor, self = nil, false
		if obj.InstagramMedia != nil && obj.InstagramMedia.Owner != nil {
			owner := obj.InstagramMedia.Owner
			actor, family, self = &Actor{ID: owner.ID, Name: owner.Username}, "ig_user", owner.ID == r.imp.instagram
		} else if obj.InstagramComment != nil && obj.InstagramComment.From != nil {
			from := obj.InstagramComment.From
			actor, family = &Actor{ID: from.ID, Name: from.Username}, "ig_comment_from_user"
		}
	}
	name := ""
	if actor != nil {
		name = actor.Name
	}
	if r.imp.instagram == "" {
		id, err := resolveActor(ctx, r.imp.store, r.source.ID, r.app, r.page, actor)
		return id, name, self, err
	}
	key := ""
	if actor != nil {
		key = instagramActorIdentifier(r.app, r.page, r.imp.instagram, family, actor.ID)
	}
	id, err := resolveScopedActor(ctx, r.imp.store, r.source.ID, InstagramSourceType, "instagram", key, actor)
	return id, name, self, err
}
