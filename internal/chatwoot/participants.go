package chatwoot

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/store"
)

func actorKind(a Actor) string {
	switch strings.ToLower(a.Type) {
	case actorUser, "agent":
		return actorUser
	case "contact":
		return "contact"
	case "agent_bot", "agentbot":
		return "agent_bot"
	default:
		return "unknown"
	}
}

func (imp *Importer) actorIdentifier(a Actor) string {
	return fmt.Sprintf("%s/accounts/%d/%s/%d", imp.client.baseURL, imp.client.accountID, actorKind(a), a.ID)
}

func actorName(a Actor) string {
	if a.Name != "" {
		return a.Name
	}
	return a.AvailableName
}

func (imp *Importer) resolveActor(ctx context.Context, sourceID int64, a Actor) (int64, error) {
	if a.ID <= 0 || actorKind(a) == "unknown" {
		return 0, nil
	}
	if actorKind(a) == actorUser {
		if richer, ok := imp.agents[a.ID]; ok {
			if a.Email == "" {
				a.Email = richer.Email
			}
			if actorName(a) == "" {
				a.Name = actorName(richer)
			}
		}
	}
	identifier := imp.actorIdentifier(a)
	cacheKey := identifier + "\x00" + a.Email + "\x00" + a.PhoneNumber + "\x00" + actorName(a)
	if pid, ok := imp.resolvedActors[cacheKey]; ok {
		return pid, nil
	}
	pid, _, err := imp.store.ParticipantByIdentifier(SourceType, identifier)
	if err != nil {
		return 0, err
	}
	if pid == 0 {
		// Explicit contact addresses can join native mail/phone archives. Staff
		// and bot identities remain provider-scoped; names are never identities.
		switch {
		case actorKind(a) == "contact" && strings.HasPrefix(a.PhoneNumber, "+"):
			pid, err = imp.store.EnsureParticipantByPhone(a.PhoneNumber, actorName(a), SourceType)
		case actorKind(a) == "contact" && strings.Contains(a.Email, "@"):
			email := strings.ToLower(strings.TrimSpace(a.Email))
			pid, err = imp.store.EnsureParticipantContext(ctx, email, actorName(a), email[strings.LastIndex(email, "@")+1:])
		default:
			pid, err = imp.store.EnsureParticipantByIdentifier(SourceType, identifier, actorName(a))
		}
		if err != nil {
			return 0, err
		}
		if err = imp.store.SetParticipantIdentifier(pid, SourceType, identifier); err != nil {
			return 0, err
		}
	}
	for _, address := range []struct {
		kind  store.ContactAddressKind
		value string
	}{
		{store.ContactAddressEmail, a.Email}, {store.ContactAddressPhone, a.PhoneNumber},
	} {
		if strings.TrimSpace(address.value) == "" {
			continue
		}
		_, err = imp.store.RecordContactObservationContext(ctx, pid, store.ParticipantContactObservationInput{
			SourceID: &sourceID, AddressKind: address.kind, ProviderUserID: &identifier, OriginalValue: address.value,
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation, SourceRef: &identifier},
		})
		if err != nil {
			return 0, fmt.Errorf("record Chatwoot actor address: %w", err)
		}
	}
	imp.resolvedActors[cacheKey] = pid
	return pid, nil
}

func (imp *Importer) personalActor(a Actor, opts ImportOptions) bool {
	if actorKind(a) != actorUser || a.ID <= 0 {
		return false
	}
	if slices.Contains(opts.SelfAgentIDs, a.ID) {
		return true
	}
	for _, identity := range imp.identities {
		if store.EqualIdentifier(identity.Address, imp.actorIdentifier(a)) ||
			(a.Email != "" && store.EqualIdentifier(identity.Address, a.Email)) {
			return true
		}
	}
	return false
}
