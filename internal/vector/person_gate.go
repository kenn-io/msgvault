package vector

import (
	"context"
	"errors"
	"fmt"
)

var (
	// ErrSemanticPersonEmbeddingsDisabled reports the explicit default-off
	// configuration boundary before any curated text reaches a provider.
	ErrSemanticPersonEmbeddingsDisabled = errors.New(
		"semantic person embeddings require [vector.people] enabled = true",
	)
	// ErrSemanticPersonEmbeddingConsentRequired reports that the exact current
	// outbound policy has no active grant.
	ErrSemanticPersonEmbeddingConsentRequired = errors.New(
		"semantic person embeddings require active exact consent; run `msgvault person provider consent --semantic-embeddings` to review the current policy",
	)
	// ErrSemanticPersonEmbeddingRuntimeStale prevents a newly-consented config
	// from authorizing a provider client that was initialized under old routing
	// or model settings.
	ErrSemanticPersonEmbeddingRuntimeStale = fmt.Errorf(
		"semantic person embedding provider settings changed; restart to reinitialize the provider client: %w",
		ErrIndexStale,
	)
	// ErrSemanticPersonEmbeddingPolicyUnavailable reports that current
	// authorization could not be established because the live policy or its
	// consent source could not be read or validated. Person operations expose
	// the wrapped cause while message-generation convergence treats person
	// coverage as not currently required.
	ErrSemanticPersonEmbeddingPolicyUnavailable = errors.New(
		"semantic person embedding authorization policy is unavailable",
	)
)

// SemanticPersonEmbeddingGate is rechecked immediately before every curated
// person provider request and before each person search request.
type SemanticPersonEmbeddingGate interface {
	Check(ctx context.Context) error
}

// SemanticPersonEmbeddingGateFunc adapts focused callers and tests.
type SemanticPersonEmbeddingGateFunc func(context.Context) error

func (f SemanticPersonEmbeddingGateFunc) Check(ctx context.Context) error { return f(ctx) }

// SemanticPersonEmbeddingConsentChecker is the narrow store authority used by
// the policy gate. *store.Store implements it in a purpose-specific table.
type SemanticPersonEmbeddingConsentChecker interface {
	HasActivePersonSemanticEmbeddingConsent(ctx context.Context, fingerprint string) (bool, error)
}

// SemanticPersonEmbeddingConfigSource returns the current configuration. It
// must not be a startup snapshot in long-running processes.
type SemanticPersonEmbeddingConfigSource func() (Config, error)

type exactSemanticPersonEmbeddingGate struct {
	configSource SemanticPersonEmbeddingConfigSource
	consents     SemanticPersonEmbeddingConsentChecker
	initialized  *semanticPersonEmbeddingRuntimeIdentity
}

type semanticPersonEmbeddingRuntimeIdentity struct {
	endpoint  string
	apiFormat EmbeddingAPIFormat
	model     string
	apiKeyEnv string
}

func NewExactSemanticPersonEmbeddingGate(
	configSource SemanticPersonEmbeddingConfigSource,
	consents SemanticPersonEmbeddingConsentChecker,
) SemanticPersonEmbeddingGate {
	return &exactSemanticPersonEmbeddingGate{configSource: configSource, consents: consents}
}

// NewPinnedExactSemanticPersonEmbeddingGate additionally binds the live
// provider client identity. Provider-routing drift fails closed until restart,
// even if the operator has already granted the new policy.
func NewPinnedExactSemanticPersonEmbeddingGate(
	initialized Config,
	configSource SemanticPersonEmbeddingConfigSource,
	consents SemanticPersonEmbeddingConsentChecker,
) SemanticPersonEmbeddingGate {
	identity := semanticPersonEmbeddingRuntimeIdentityForConfig(initialized)
	return &exactSemanticPersonEmbeddingGate{
		configSource: configSource, consents: consents, initialized: &identity,
	}
}

func (g *exactSemanticPersonEmbeddingGate) Check(ctx context.Context) error {
	if g == nil || g.configSource == nil || g.consents == nil {
		return errors.New("semantic person embedding gate is not configured")
	}
	config, err := g.configSource()
	if err != nil {
		return fmt.Errorf("%w: read current semantic person embedding policy: %w",
			ErrSemanticPersonEmbeddingPolicyUnavailable, err)
	}
	if !config.Enabled || !config.People.Enabled {
		return ErrSemanticPersonEmbeddingsDisabled
	}
	if g.initialized != nil && *g.initialized != semanticPersonEmbeddingRuntimeIdentityForConfig(config) {
		return ErrSemanticPersonEmbeddingRuntimeStale
	}
	profile, err := config.SemanticPersonEmbeddingProfile()
	if err != nil {
		return fmt.Errorf("%w: validate current semantic person embedding policy: %w",
			ErrSemanticPersonEmbeddingPolicyUnavailable, err)
	}
	active, err := g.consents.HasActivePersonSemanticEmbeddingConsent(ctx, profile.Fingerprint)
	if err != nil {
		return fmt.Errorf("%w: check semantic person embedding consent: %w",
			ErrSemanticPersonEmbeddingPolicyUnavailable, err)
	}
	if !active {
		return fmt.Errorf("%w (fingerprint %s)",
			ErrSemanticPersonEmbeddingConsentRequired, profile.Fingerprint)
	}
	return nil
}

func semanticPersonEmbeddingRuntimeIdentityForConfig(
	config Config,
) semanticPersonEmbeddingRuntimeIdentity {
	return semanticPersonEmbeddingRuntimeIdentity{
		endpoint:  config.Embeddings.Endpoint,
		apiFormat: config.Embeddings.EffectiveAPIFormat(),
		model:     config.Embeddings.Model,
		apiKeyEnv: config.Embeddings.APIKeyEnv,
	}
}

// SemanticPersonEmbeddingInactive reports the expected default/administrative
// states that should skip person maintenance without failing message work.
func SemanticPersonEmbeddingInactive(err error) bool {
	return errors.Is(err, ErrSemanticPersonEmbeddingsDisabled) ||
		errors.Is(err, ErrSemanticPersonEmbeddingConsentRequired)
}

// SemanticPersonEmbeddingAuthorizationUnavailable reports every fail-closed
// state in which current authorization cannot require person coverage from a
// message generation. Person workers and searches still receive the original
// error and make no provider request.
func SemanticPersonEmbeddingAuthorizationUnavailable(err error) bool {
	return SemanticPersonEmbeddingInactive(err) ||
		errors.Is(err, ErrSemanticPersonEmbeddingRuntimeStale) ||
		errors.Is(err, ErrSemanticPersonEmbeddingPolicyUnavailable)
}
