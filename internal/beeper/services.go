package beeper

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"go.kenn.io/msgvault/internal/store"
)

var (
	errBridgeServiceNotFound = errors.New("beeper bridge service not found")
	errBridgeUnclassified    = errors.New("beeper bridge cannot be classified")
)

const (
	bridgeDiscoveryProvider        = "beeper"
	bridgeRoutingFallbackDiscovery = "routing_fallback"
)

// Bridge type -> communication service.
//
// The roadmap makes the account's bridge type the canonical service, network
// the display-only label, and accountID the routing key and best available
// account scope. Beeper's bridge set is open-ended (its main catalog and its
// bridge-manager catalog differ, and bridge-manager supports third-party and
// custom bridges), so an unknown bridge type is REGISTERED, never rejected.

// bridgeKey caches one resolution per account, network, and observed bridge
// prefix for the run. The prefix matters when an opaque account ID and label
// do not identify an unknown bridge.
type bridgeKey struct {
	accountID    string
	network      string
	bridgePrefix string
}

type bridgeServiceResolver struct {
	store *store.Store
	// cache holds resolved services, including the nil result for a bridge
	// that could not be classified, so an unusable account id costs one
	// lookup per run rather than one per chat.
	cache map[bridgeKey]*store.CommunicationService
}

func newBridgeServiceResolver(s *store.Store) *bridgeServiceResolver {
	return &bridgeServiceResolver{store: s, cache: map[bridgeKey]*store.CommunicationService{}}
}

// resolve maps a Beeper account to a communication service. ok is false when
// the bridge cannot be classified at all; the caller then records the
// observation with no service, which PR 3's normalization and scope
// validation both support. An unrecognised bridge TYPE is a different case:
// it is registered as a new service row.
func (r *bridgeServiceResolver) resolve(
	ctx context.Context, accountID, network, userID string,
) (*store.CommunicationService, bool, error) {
	return r.resolveBridge(ctx, accountID, network, bridgeServicePrefixFromUserID(userID))
}

func (r *bridgeServiceResolver) resolveBridge(
	ctx context.Context, accountID, network, bridgePrefix string,
) (*store.CommunicationService, bool, error) {
	bridgePrefix = validBridgeServicePrefix(bridgePrefix)
	key := bridgeKey{
		accountID: accountID, network: network,
		bridgePrefix: bridgePrefix,
	}
	if cached, seen := r.cache[key]; seen {
		return cached, cached != nil, nil
	}

	service, err := r.lookup(ctx, accountID, network, bridgePrefix)
	if err != nil && !errors.Is(err, errBridgeServiceNotFound) {
		return nil, false, err
	}
	if service == nil {
		service, err = r.register(ctx, accountID, network, bridgePrefix)
		if errors.Is(err, errBridgeUnclassified) {
			r.cache[key] = nil
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
	}
	r.cache[key] = service
	return service, service != nil, nil
}

// lookup walks the resolution ladder: accountID, then the network label, then
// the bridge prefix Beeper encodes in the user ID. A configured account or
// network remains authoritative. A service that Beeper itself registered from
// an opaque routing value is only a fallback: a qualified participant with
// another prefix must not be trapped by that discovered row. Each candidate
// goes through the catalog's slug-or-alias resolution, so twitter/x,
// gmessages/google-messages, and bsky/bluesky collapse without this package
// knowing about them.
func (r *bridgeServiceResolver) lookup(
	ctx context.Context, accountID, network, bridgePrefix string,
) (*store.CommunicationService, error) {
	for _, candidate := range []struct {
		slug     string
		fallback bool
	}{
		{slug: serviceSlugCandidate(accountID), fallback: true},
		{slug: serviceSlugCandidate(network), fallback: true},
		{slug: bridgePrefix},
	} {
		if candidate.slug == "" {
			continue
		}
		var service *store.CommunicationService
		var discovered bool
		var err error
		if candidate.fallback && bridgePrefix != "" {
			service, discovered, err = r.store.ResolveCommunicationServiceDiscoveryContext(
				ctx, candidate.slug, bridgeDiscoveryProvider, bridgeRoutingFallbackDiscovery)
		} else {
			service, err = r.store.ResolveCommunicationServiceContext(ctx, candidate.slug)
		}
		switch {
		case err == nil:
			if service.Slug != bridgePrefix && discovered {
				continue
			}
			return service, nil
		case errors.Is(err, store.ErrServiceNotFound):
			continue
		default:
			return nil, err
		}
	}
	return nil, errBridgeServiceNotFound
}

// register adds an unknown bridge to the catalog. It claims NO aliases:
// EnsureCommunicationService rejects an alias already owned by another
// service and rolls the whole call back, so claiming the network label as an
// alias would make a colliding label fail the import instead of archiving it.
// The label is preserved as display_label, and the accountID survives on
// every observation as the account scope.
//
// normalization is "none" so an unknown service can never rewrite an observed
// value, and scope_policy is "optional" so the account scope is allowed but
// not demanded.
func (r *bridgeServiceResolver) register(
	ctx context.Context, accountID, network, bridgePrefix string,
) (*store.CommunicationService, error) {
	slug := bridgePrefix
	if slug == "" {
		slug = serviceSlugCandidate(accountID)
	}
	if slug == "" {
		slug = serviceSlugCandidate(network)
	}
	if slug == "" {
		// Nothing usable to name a service after. Leave the observation
		// unclassified rather than inventing a slug.
		slog.Warn("beeper bridge could not be classified",
			"account_id", accountID, "network", network)
		return nil, errBridgeUnclassified
	}
	label := strings.TrimSpace(network)
	if label == "" {
		label = strings.TrimSpace(accountID)
	}
	input := store.CommunicationServiceInput{
		Slug:                 slug,
		DisplayLabel:         label,
		ScopePolicy:          store.ScopePolicyOptional,
		Normalization:        store.NormalizationNone,
		NormalizationVersion: 1,
	}
	var service *store.CommunicationService
	var created bool
	var err error
	if bridgePrefix == "" {
		service, created, err = r.store.EnsureDiscoveredCommunicationServiceContext(
			ctx, input, bridgeDiscoveryProvider, bridgeRoutingFallbackDiscovery)
	} else {
		service, created, err = r.store.EnsureCommunicationServiceContext(ctx, input)
	}
	if err != nil {
		// A malformed slug or an alias collision must not stop messages being
		// archived; the observation degrades to unclassified.
		if errors.Is(err, store.ErrInvalidServiceSlug) || errors.Is(err, store.ErrServiceAliasConflict) {
			slog.Warn("beeper bridge service registration rejected",
				"account_id", accountID, "network", network, "slug", slug, "error", err)
			return nil, errBridgeUnclassified
		}
		return nil, err
	}
	if created {
		slog.Info("registered unknown Beeper bridge as a communication service",
			"slug", service.Slug, "display_label", service.DisplayLabel, "account_id", accountID)
	}
	return service, nil
}

// participantBridgePrefix finds the bridge type before participant capture
// starts. Self users often have an unqualified @me ID and appear first. A
// shared qualified prefix prevents an opaque account fallback from being
// registered for the whole chat; disagreement is ambiguous and returns no
// hint, so each qualified participant still uses its own prefix.
func participantBridgePrefix(participants []Participant) string {
	var shared string
	for i := range participants {
		prefix := bridgeServicePrefixFromUserID(participants[i].ID)
		if prefix == "" {
			continue
		}
		if shared == "" {
			shared = prefix
			continue
		}
		if prefix != shared {
			return ""
		}
	}
	return shared
}

func bridgeServicePrefixFromUserID(userID string) string {
	return validBridgeServicePrefix(bridgePrefixFromUserID(userID))
}

func validBridgeServicePrefix(prefix string) string {
	if prefix == "" || serviceSlugCandidate(prefix) != prefix {
		return ""
	}
	return prefix
}

// serviceScope derives the scope an observation is addressed in, honouring
// the service's own policy:
//
//   - policy "none": no scope. Sending one would fail ValidateServiceScope.
//   - matrix: the server from the observed user ID, which is the only server
//     value the archive actually has.
//   - otherwise: kind "account", value accountID.
//
// The kind stays "account" even for services whose default_scope_kind is
// "workspace" (slack) or "network" (irc). A Beeper accountID is a bridge
// login, not a Slack workspace ID or an IRC network name; labelling it
// "workspace" would assert a fact we cannot verify. The policy only requires
// that a scope kind and value exist, not that they match the default.
func serviceScope(
	service *store.CommunicationService, accountID, userID string,
) (scopeKind, scopeValue *string) {
	if service == nil || service.ScopePolicy == store.ScopePolicyNone {
		return nil, nil
	}
	if service.Slug == "matrix" {
		if server := matrixServerFromUserID(userID); server != "" {
			kind, value := "server", server
			return &kind, &value
		}
	}
	account := strings.TrimSpace(accountID)
	if account == "" {
		return nil, nil
	}
	kind := "account"
	return &kind, &account
}

// serviceSlugCandidate normalizes a raw bridge label into the catalog's slug
// shape (^[a-z0-9][a-z0-9-]*$): lowercase, spaces and underscores become
// dashes, anything else is dropped, runs of dashes collapse, and leading and
// trailing dashes are trimmed. An unusable input returns "".
func serviceSlugCandidate(raw string) string {
	var builder strings.Builder
	builder.Grow(len(raw))
	for _, r := range strings.ToLower(strings.TrimSpace(raw)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == ' ' || r == '_' || r == '-' || r == '.':
			builder.WriteByte('-')
		}
	}
	slug := strings.Trim(builder.String(), "-")
	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}
	return slug
}

// serviceSlugOf returns the slug of service, or "" for a nil service, so
// callers can namespace a provider ID without a nil check at every site.
func serviceSlugOf(service *store.CommunicationService) string {
	if service == nil {
		return ""
	}
	return service.Slug
}
