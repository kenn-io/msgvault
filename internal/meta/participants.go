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
	if actor == nil || actor.ID == "" {
		return 0, nil
	}
	key := ActorIdentifier(app, page, actor.ID)
	id, err := st.EnsureParticipantByIdentifierContext(ctx, SourceType, key, actor.Name)
	if err != nil {
		return 0, err
	}
	service, err := st.ResolveCommunicationServiceContext(ctx, "facebook")
	if err != nil {
		return 0, err
	}
	// The catalog service has scope_policy=none. Scope lives in the immutable
	// anchor and source_id; changing global catalog semantics is unnecessary.
	if err = st.ClassifyParticipantIdentifierServiceContext(ctx, SourceType, key, &service.ID, nil, nil); err != nil {
		return 0, err
	}
	_, err = st.RecordContactObservationContext(ctx, id, store.ParticipantContactObservationInput{
		SourceID: &sourceID, AddressKind: store.ContactAddressProviderIdentity, ServiceSlug: new("facebook"),
		ProviderUserID: &key, OriginalValue: key,
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation, SourceRef: &key},
	})
	return id, err
}
