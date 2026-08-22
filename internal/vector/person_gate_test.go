package vector

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type semanticPersonConsentSet map[string]bool

func (s semanticPersonConsentSet) HasActivePersonSemanticEmbeddingConsent(
	_ context.Context, fingerprint string,
) (bool, error) {
	return s[fingerprint], nil
}

// TestExactSemanticPersonGateRequiresOptInAndCurrentConsent catches either
// half of the two-part privacy gate being treated as sufficient.
func TestExactSemanticPersonGateRequiresOptInAndCurrentConsent(t *testing.T) {
	must := require.New(t)
	current := semanticPersonPolicyTestConfig()
	current.People.Enabled = false
	consents := semanticPersonConsentSet{}
	gate := NewExactSemanticPersonEmbeddingGate(
		func() (Config, error) { return current, nil }, consents,
	)

	err := gate.Check(t.Context())
	must.ErrorIs(err, ErrSemanticPersonEmbeddingsDisabled)
	must.ErrorContains(err, "[vector.people] enabled = true")

	current.People.Enabled = true
	err = gate.Check(t.Context())
	must.ErrorIs(err, ErrSemanticPersonEmbeddingConsentRequired)

	profile, err := current.SemanticPersonEmbeddingProfile()
	must.NoError(err)
	consents[profile.Fingerprint] = true
	must.NoError(gate.Check(t.Context()))

	current.Enabled = false
	must.ErrorIs(gate.Check(t.Context()), ErrSemanticPersonEmbeddingsDisabled,
		"disabling the enclosing vector feature must revoke runtime authority")
}

// TestExactSemanticPersonGateRechecksRevocationAndPolicyDrift catches a
// startup-only consent cache authorizing later requests under changed policy.
func TestExactSemanticPersonGateRechecksRevocationAndPolicyDrift(t *testing.T) {
	must := require.New(t)
	current := semanticPersonPolicyTestConfig()
	consents := semanticPersonConsentSet{}
	profile, err := current.SemanticPersonEmbeddingProfile()
	must.NoError(err)
	consents[profile.Fingerprint] = true
	gate := NewExactSemanticPersonEmbeddingGate(
		func() (Config, error) { return current, nil }, consents,
	)
	must.NoError(gate.Check(t.Context()))

	current.Embeddings.Model = "policy-drifted-model"
	err = gate.Check(t.Context())
	must.ErrorIs(err, ErrSemanticPersonEmbeddingConsentRequired)

	current.Embeddings.Model = profile.Model
	consents[profile.Fingerprint] = false
	err = gate.Check(t.Context())
	must.ErrorIs(err, ErrSemanticPersonEmbeddingConsentRequired)
}

// TestExactSemanticPersonGateRejectsConsentForProfileWithoutQueryDisclosure
// catches an active grant for the pre-query-disclosure policy authorizing the
// expanded current egress policy.
func TestExactSemanticPersonGateRejectsConsentForProfileWithoutQueryDisclosure(t *testing.T) {
	must := require.New(t)
	current := semanticPersonPolicyTestConfig()
	historical := historicalSemanticPersonEmbeddingProfile(t)
	currentProfile, err := current.SemanticPersonEmbeddingProfile()
	must.NoError(err)
	must.NotEqual(historical.Fingerprint, currentProfile.Fingerprint)

	consents := semanticPersonConsentSet{historical.Fingerprint: true}
	gate := NewExactSemanticPersonEmbeddingGate(
		func() (Config, error) { return current, nil }, consents,
	)
	err = gate.Check(t.Context())
	must.ErrorIs(err, ErrSemanticPersonEmbeddingConsentRequired)
	must.ErrorContains(err, currentProfile.Fingerprint)

	consents[currentProfile.Fingerprint] = true
	must.NoError(gate.Check(t.Context()))
}

// TestPinnedSemanticPersonGateRejectsProviderDriftEvenAfterNewConsent catches
// a restarted-policy grant authorizing the still-running old provider client.
func TestPinnedSemanticPersonGateRejectsProviderDriftEvenAfterNewConsent(t *testing.T) {
	must := require.New(t)
	initialized := semanticPersonPolicyTestConfig()
	current := initialized
	consents := semanticPersonConsentSet{}
	initialProfile, err := initialized.SemanticPersonEmbeddingProfile()
	must.NoError(err)
	consents[initialProfile.Fingerprint] = true
	gate := NewPinnedExactSemanticPersonEmbeddingGate(
		initialized, func() (Config, error) { return current, nil }, consents,
	)
	must.NoError(gate.Check(t.Context()))

	current.Embeddings.Model = "newly-configured-model"
	newProfile, err := current.SemanticPersonEmbeddingProfile()
	must.NoError(err)
	consents[newProfile.Fingerprint] = true
	err = gate.Check(t.Context())
	must.ErrorIs(err, ErrSemanticPersonEmbeddingRuntimeStale)
	must.ErrorIs(err, ErrIndexStale)
}

func TestExactSemanticPersonGateFailsClosedOnPolicySourceError(t *testing.T) {
	gate := NewExactSemanticPersonEmbeddingGate(
		func() (Config, error) { return Config{}, errors.New("synthetic config read") },
		semanticPersonConsentSet{},
	)
	err := gate.Check(t.Context())
	require.ErrorContains(t, err, "synthetic config read")
	require.ErrorIs(t, err, ErrSemanticPersonEmbeddingPolicyUnavailable)
	assert.True(t, SemanticPersonEmbeddingAuthorizationUnavailable(err))
}

type failingSemanticPersonConsentChecker struct{ err error }

func (c failingSemanticPersonConsentChecker) HasActivePersonSemanticEmbeddingConsent(
	context.Context, string,
) (bool, error) {
	return false, c.err
}

// TestSemanticPersonEmbeddingAuthorizationUnavailableClassifiesEveryPolicyState
// defines the boundary used by convergence: message generations do not need
// person coverage when current authorization cannot be established, while the
// gate still returns the precise error to person workers and searches.
func TestSemanticPersonEmbeddingAuthorizationUnavailableClassifiesEveryPolicyState(t *testing.T) {
	valid := semanticPersonPolicyTestConfig()
	profile, err := valid.SemanticPersonEmbeddingProfile()
	require.NoError(t, err)
	consented := semanticPersonConsentSet{profile.Fingerprint: true}

	drifted := valid
	drifted.Embeddings.Endpoint = "https://drifted.example.test/v1"
	invalid := valid
	invalid.Embeddings.Model = ""

	tests := []struct {
		name string
		gate SemanticPersonEmbeddingGate
	}{
		{
			name: "disabled",
			gate: NewExactSemanticPersonEmbeddingGate(
				func() (Config, error) { return Config{}, nil }, consented,
			),
		},
		{
			name: "unconsented",
			gate: NewExactSemanticPersonEmbeddingGate(
				func() (Config, error) { return valid, nil }, semanticPersonConsentSet{},
			),
		},
		{
			name: "runtime drift",
			gate: NewPinnedExactSemanticPersonEmbeddingGate(
				valid, func() (Config, error) { return drifted, nil }, consented,
			),
		},
		{
			name: "invalid live policy",
			gate: NewExactSemanticPersonEmbeddingGate(
				func() (Config, error) { return invalid, nil }, consented,
			),
		},
		{
			name: "config source unavailable",
			gate: NewExactSemanticPersonEmbeddingGate(
				func() (Config, error) { return Config{}, errors.New("synthetic source unavailable") }, consented,
			),
		},
		{
			name: "consent source unavailable",
			gate: NewExactSemanticPersonEmbeddingGate(
				func() (Config, error) { return valid, nil },
				failingSemanticPersonConsentChecker{err: errors.New("synthetic consent store unavailable")},
			),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.gate.Check(t.Context())
			require.Error(t, err)
			assert.True(t, SemanticPersonEmbeddingAuthorizationUnavailable(err), err.Error())
		})
	}
	assert.False(t, SemanticPersonEmbeddingAuthorizationUnavailable(errors.New("unrelated failure")))
}

var _ SemanticPersonEmbeddingConsentChecker = semanticPersonConsentSet{}
