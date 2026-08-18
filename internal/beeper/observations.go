package beeper

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

var errObservationSkipped = errors.New("beeper observation skipped")

// Observation capture.
//
// This file records what Beeper showed us about a person's addresses. It sits
// beside the participant resolution ladder in participants.go and changes
// nothing about it: the ladder decides which participant a message points at,
// capture only adds rows to participant_contact_observations. No message
// foreign key, participant identifier ownership, or display name is rewritten
// here.
//
// Curated reachability (person_contact_points) and observed archive identity
// stay separate on purpose. Nothing here writes a curated person value, and
// nothing promotes an internal Beeper/Matrix ID into something exportable.

// captureContext is the per-chat context one capture runs in.
type captureContext struct {
	SourceID     int64
	AccountID    string
	Network      string
	BridgePrefix string
}

// observedAddress is one address kind and raw value seen on a Beeper user.
type observedAddress struct {
	Kind  store.ContactAddressKind
	Value string
}

// observationKey dedupes writes inside one run.
type observationKey struct {
	participantID int64
	sourceID      int64
	userID        string
	addressKind   store.ContactAddressKind
	serviceSlug   string
	scopeKind     string
	scopeValue    string
	providerID    string
	value         string
}

type observationRecorder struct {
	store    *store.Store
	services *bridgeServiceResolver

	// PR 3 serializes observation and candidate identity keys in the database.
	// This mutex and the run-local maps only avoid duplicate work inside one
	// importer.
	mu sync.Mutex
	// seen makes a re-walked chat free: one run sees the same participants in
	// many chats, while the dominant capture cost is the store round trip.
	seen map[observationKey]struct{}
	// classified remembers which Beeper identifier anchors already carry a
	// service classification this run.
	classified map[string]struct{}
}

func newObservationRecorder(s *store.Store) *observationRecorder {
	return &observationRecorder{
		store:      s,
		services:   newBridgeServiceResolver(s),
		seen:       map[observationKey]struct{}{},
		classified: map[string]struct{}{},
	}
}

// capture records every address observed on u for participantID and returns
// one result per write attempted, so the caller can count and match them.
// A partial slice is returned alongside an error: what was written stays
// written.
func (r *observationRecorder) capture(
	ctx context.Context, participantID int64, u *User, cc captureContext,
) ([]*store.RecordContactObservationResult, error) {
	if participantID == 0 || u == nil || strings.TrimSpace(u.ID) == "" {
		return nil, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	bridgePrefix := bridgeServicePrefixFromUserID(u.ID)
	if bridgePrefix == "" {
		bridgePrefix = cc.BridgePrefix
	}
	service, _, err := r.services.resolveBridge(
		ctx, cc.AccountID, cc.Network, bridgePrefix)
	if err != nil {
		return nil, err
	}
	slug := serviceSlugOf(service)
	scopeKind, scopeValue := serviceScope(service, cc.AccountID, u.ID)
	anchor := providerUserIDScoped(slug, cc.AccountID, scopeKind, scopeValue, u)
	if err := r.classifyAnchor(
		ctx, u.ID, cc.AccountID, service, scopeKind, scopeValue,
	); err != nil {
		return nil, err
	}

	observedAt := time.Now().UTC()
	sourceRef := observationSourceRef(cc.AccountID, u.ID)
	var results []*store.RecordContactObservationResult
	for _, address := range observedAddresses(u, anchor) {
		key := observationKey{
			participantID: participantID, sourceID: cc.SourceID,
			userID: u.ID, addressKind: address.Kind, serviceSlug: slug,
			scopeKind: derefString(scopeKind), scopeValue: derefString(scopeValue),
			providerID: anchor, value: address.Value,
		}
		if _, seen := r.seen[key]; seen {
			continue
		}
		request := recordRequest{
			SourceID: cc.SourceID, AddressKind: address.Kind,
			ScopeKind: scopeKind, ScopeValue: scopeValue,
			ProviderUserID: anchor, OriginalValue: address.Value,
			ObservedAt: observedAt, SourceRef: sourceRef, SourceUserID: u.ID,
		}
		if slug != "" {
			serviceSlug := slug
			request.ServiceSlug = &serviceSlug
		}
		result, err := r.record(ctx, participantID, request)
		if errors.Is(err, errObservationSkipped) {
			r.seen[key] = struct{}{}
			continue
		}
		if err != nil {
			return results, err
		}
		if result != nil && address.Kind == store.ContactAddressUsername {
			if err := r.supersedeRenamed(ctx, participantID, result.Observation); err != nil {
				return results, err
			}
		}
		r.seen[key] = struct{}{}
		if result == nil {
			continue
		}
		results = append(results, result)
	}
	return results, nil
}

// supersedeRenamed closes the participant's other current username
// observations on the same service and scope when a new username arrives
// under the same stable ID.
//
// Beeper exposes exactly one username per user per network at a time, so a
// different value under the same (stable ID, service, scope) is a rename and
// the old value becomes history. Multiple concurrently active usernames stay
// representable across services, scopes, and accounts; multiple phone numbers
// and emails are never superseded.
//
// The stable ID is the guard. A current row with a different provider_user_id
// is another person's claim on the same address and remains untouched, so
// history never moves between people.
func (r *observationRecorder) supersedeRenamed(
	ctx context.Context, participantID int64, kept *store.ParticipantContactObservation,
) error {
	if kept == nil || kept.ProviderUserID == nil {
		return nil
	}
	current, err := r.store.ListParticipantObservationsContext(ctx, participantID, true)
	if err != nil {
		return err
	}
	for i := range current {
		previous := &current[i]
		switch {
		case previous.Envelope.ID == kept.Envelope.ID,
			previous.AddressKind != store.ContactAddressUsername,
			previous.NormalizedValue == kept.NormalizedValue,
			!sameServiceScope(previous, kept),
			!equalInt64Ptr(previous.SourceID, kept.SourceID),
			previous.ProviderUserID == nil,
			*previous.ProviderUserID != *kept.ProviderUserID:
			continue
		}
		if err := r.store.SupersedeParticipantObservationContext(
			ctx, participantID, previous.Envelope.ID, nil); err != nil {
			return err
		}
	}
	return nil
}

// sameServiceScope reports whether two observations address the same service
// and scope, treating absent values as equal only to each other.
func sameServiceScope(a, b *store.ParticipantContactObservation) bool {
	return equalStringPtr(a.ServiceSlug, b.ServiceSlug) &&
		equalStringPtr(a.ScopeKind, b.ScopeKind) &&
		equalStringPtr(a.ScopeValue, b.ScopeValue)
}

func equalStringPtr(a, b *string) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

func equalInt64Ptr(a, b *int64) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

// recordRequest is one observation write, kept as a value so the
// service-rejected retry below is an obvious copy-with-one-change rather than
// a second hand-built input.
type recordRequest struct {
	SourceID       int64
	AddressKind    store.ContactAddressKind
	ServiceSlug    *string
	ScopeKind      *string
	ScopeValue     *string
	ProviderUserID string
	OriginalValue  string
	ObservedAt     time.Time
	SourceRef      string
	SourceUserID   string
}

// input builds the store input. Every pointer is taken from a local copy, so
// two requests built in one loop iteration never share a pointee.
func (q recordRequest) input() store.ParticipantContactObservationInput {
	observedAt := q.ObservedAt
	sourceRef := q.SourceRef
	input := store.ParticipantContactObservationInput{
		AddressKind:   q.AddressKind,
		ServiceSlug:   q.ServiceSlug,
		ScopeKind:     q.ScopeKind,
		ScopeValue:    q.ScopeValue,
		OriginalValue: q.OriginalValue,
		ObservedAt:    &observedAt,
		Envelope: store.ValueEnvelopeInput{
			Source:    store.ProvenanceArchiveObservation,
			SourceRef: &sourceRef,
		},
	}
	if q.SourceID != 0 {
		sourceID := q.SourceID
		input.SourceID = &sourceID
	}
	if q.ProviderUserID != "" {
		providerUserID := q.ProviderUserID
		input.ProviderUserID = &providerUserID
	}
	return input
}

// record writes one observation.
//
// A value the service's normalization strategy rejects is re-recorded with no
// service. Seeded phone-first services use phone_e164 for every address kind,
// so a non-phone username on one of them cannot be normalized. The store
// supports a nil service explicitly and then normalizes by address kind alone,
// preserving the value while losing only its service classification.
func (r *observationRecorder) record(
	ctx context.Context, participantID int64, request recordRequest,
) (*store.RecordContactObservationResult, error) {
	result, err := r.store.RecordContactObservationContext(ctx, participantID, request.input())
	if err == nil {
		return result, nil
	}
	if errors.Is(err, store.ErrObservationValueMissing) {
		return nil, errObservationSkipped
	}
	if !errors.Is(err, store.ErrNormalizationRejected) || request.ServiceSlug == nil {
		return nil, err
	}
	slog.Warn("beeper observed value rejected by its service normalization; recording it unclassified",
		"service", *request.ServiceSlug, "address_kind", request.AddressKind,
		"beeper_user_id", request.SourceUserID)

	unclassified := request
	unclassified.ServiceSlug = nil
	unclassified.ScopeKind = nil
	unclassified.ScopeValue = nil
	result, err = r.store.RecordContactObservationContext(ctx, participantID, unclassified.input())
	if err != nil {
		if errors.Is(err, store.ErrNormalizationRejected) ||
			errors.Is(err, store.ErrObservationValueMissing) {
			slog.Warn("beeper observed value could not be recorded",
				"address_kind", request.AddressKind,
				"beeper_user_id", request.SourceUserID, "error", err)
			return nil, errObservationSkipped
		}
		return nil, err
	}
	return result, nil
}

// classifyAnchor stamps the service and scope classification onto the Beeper
// identifier anchor row the resolution ladder wrote. PR 3's backfill leaves
// identifier_type "beeper" unclassified because Beeper is a bridge host, not
// a service: the bridge type is only knowable here.
//
// A missing anchor row is not an error. The phone and email rungs persist the
// identifier through recordRich, but a cached rich resolution returns before
// re-persisting it, and a participant from an earlier run may have been merged
// away since.
func (r *observationRecorder) classifyAnchor(
	ctx context.Context,
	userID string,
	accountID string,
	service *store.CommunicationService,
	scopeKind, scopeValue *string,
) error {
	identifier := providerFallbackUserID(accountID, userID)
	if _, done := r.classified[identifier]; done {
		return nil
	}

	var serviceID *int64
	if service != nil {
		id := service.ID
		serviceID = &id
	}
	err := r.store.ClassifyParticipantIdentifierServiceContext(
		ctx, participantIdentifierType, identifier, serviceID, scopeKind, scopeValue)
	if errors.Is(err, store.ErrParticipantIdentifierNotFound) && identifier != userID {
		// Keep older archives readable while all new resolver writes use the
		// account-scoped identifier. A legacy raw row is never used for matching;
		// this fallback only repairs its display classification in place.
		err = r.store.ClassifyParticipantIdentifierServiceContext(
			ctx, participantIdentifierType, userID, serviceID, scopeKind, scopeValue,
		)
	}
	if errors.Is(err, store.ErrParticipantIdentifierNotFound) {
		r.classified[identifier] = struct{}{}
		return nil
	}
	if err != nil {
		return err
	}
	r.classified[identifier] = struct{}{}
	return nil
}

// observedAddresses lists the addresses a Beeper user payload carries, in the
// order the resolution ladder ranks them, so ordering is deterministic. An
// ID-only user gets one importer-only provider-identity observation. Its
// provider_user_id still drives matching, while the synthetic address kind
// prevents the internal anchor from being promoted into an exportable contact
// point.
func observedAddresses(u *User, providerIdentity string) []observedAddress {
	var addresses []observedAddress
	for _, candidate := range []observedAddress{
		{Kind: store.ContactAddressPhone, Value: u.PhoneNumber},
		{Kind: store.ContactAddressEmail, Value: u.Email},
		{Kind: store.ContactAddressUsername, Value: u.Username},
	} {
		if strings.TrimSpace(candidate.Value) != "" {
			addresses = append(addresses, candidate)
		}
	}
	if len(addresses) == 0 && strings.TrimSpace(providerIdentity) != "" {
		addresses = append(addresses, observedAddress{
			Kind: store.ContactAddressProviderIdentity, Value: providerIdentity,
		})
	}
	return addresses
}

// observationSourceRef identifies the fact that produced an observation: the
// Beeper account and the user it was seen on.
func observationSourceRef(accountID, userID string) string {
	return "beeper:" + accountID + ":" + userID
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
