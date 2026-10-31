package beeper

import (
	"context"
	"fmt"

	"go.kenn.io/msgvault/internal/store"
)

// LinkTelegramUser records an export user's shared Telegram identity and matches it.
func LinkTelegramUser(ctx context.Context, st *store.Store, sourceID, participantID int64, nativeID string) error {
	key := telegramUserKey(nativeID)
	if key == "" {
		return nil
	}
	result, err := st.RecordContactObservationContext(ctx, participantID, store.ParticipantContactObservationInput{
		AddressKind: store.ContactAddressProviderIdentity,
		ServiceSlug: new("telegram"), ProviderUserID: &key,
		OriginalValue: key, SourceID: &sourceID,
		Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation, SourceRef: new("user" + nativeID)},
	})
	if err != nil {
		return fmt.Errorf("telegram: record provider identity: %w", err)
	}
	if _, err := newIdentityMatcher(st).match(ctx, participantID, result); err != nil {
		return fmt.Errorf("telegram: match provider identity: %w", err)
	}
	return nil
}
