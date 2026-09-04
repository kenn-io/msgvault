package personenrichment_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
)

type recordingConsentChecker struct {
	events *[]string
	active bool
	err    error
}

func (c recordingConsentChecker) HasActivePersonEnrichmentConsent(
	_ context.Context,
	_ string,
) (bool, error) {
	*c.events = append(*c.events, "consent")
	return c.active, c.err
}

type recordingSuppressionChecker struct {
	events     *[]string
	keyIDs     []string
	keyIDsErr  error
	suppressed map[string]bool
	lookupErr  error
	lookups    []personenrichment.SuppressionLookup
}

func (c *recordingSuppressionChecker) ListPersonEnrichmentSuppressionKeyIDsContext(
	_ context.Context,
) ([]string, error) {
	*c.events = append(*c.events, "key_ids")
	return slices.Clone(c.keyIDs), c.keyIDsErr
}

func (c *recordingSuppressionChecker) HasPersonEnrichmentSuppressionContext(
	_ context.Context,
	lookup personenrichment.SuppressionLookup,
) (bool, error) {
	*c.events = append(*c.events, "suppression")
	c.lookups = append(c.lookups, lookup)
	if c.lookupErr != nil {
		return false, c.lookupErr
	}
	return c.suppressed[string(lookup.Digest)], nil
}

func TestEgressGateChecksSuppressionBeforeCredentialLookup(t *testing.T) {
	requirements := require.New(t)
	events := []string{}
	hasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{0x31}, 32))
	requirements.NoError(err)
	profile := egressProfile(t, []personenrichment.IdentifierClass{personenrichment.IdentifierEmail})
	email := hasher.Digest(
		profile.ProviderNamespace,
		personenrichment.SuppressionEmail,
		personenrichment.EmailNormalizationV1,
		"person@example.com",
	)
	suppressions := &recordingSuppressionChecker{
		events: &events, keyIDs: []string{email.KeyID},
		suppressed: map[string]bool{string(email.Digest): true},
	}
	gate, err := personenrichment.NewEgressGate(
		recordingConsentChecker{events: &events, active: true}, suppressions, hasher,
		func(string) (string, bool) {
			events = append(events, "credential")
			return "secret", true
		},
	)
	requirements.NoError(err)

	_, err = gate.Authorize(t.Context(), personenrichment.EgressInput{
		Request: personenrichment.Request{Identity: personenrichment.Identity{Email: "person@example.com"}},
		Profile: profile,
	})
	requirements.ErrorIs(err, personenrichment.ErrSuppressed)
	assert.Equal(t, []string{"consent", "key_ids", "suppression"}, events)
}

func TestEgressGateBlocksKnownProviderIDAndNameCompanySuppressions(t *testing.T) {
	tests := []struct {
		name       string
		identity   personenrichment.Identity
		knownIDs   []string
		class      personenrichment.SuppressionIdentifierClass
		version    string
		normalized string
	}{
		{
			name: "known provider person ID", identity: personenrichment.Identity{
				Name: "Alice Example", CurrentCompany: "Example Labs",
			},
			knownIDs: []string{"Opaque-ID"}, class: personenrichment.SuppressionProviderPersonID,
			version: personenrichment.ProviderPersonIDNormalizationV1, normalized: "Opaque-ID",
		},
		{
			name:       "exact name company composite",
			identity:   personenrichment.Identity{Name: " Alice Example ", CurrentCompany: " Example Labs "},
			class:      personenrichment.SuppressionNameCompany,
			version:    personenrichment.CompositeNormalizationV1,
			normalized: "13:alice example12:example labs",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requirements := require.New(t)
			events := []string{}
			hasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{0x41}, 32))
			requirements.NoError(err)
			profile := egressProfile(t, []personenrichment.IdentifierClass{
				personenrichment.IdentifierName, personenrichment.IdentifierCurrentCompany,
			})
			blocked := hasher.Digest(profile.ProviderNamespace, test.class, test.version, test.normalized)
			suppressions := &recordingSuppressionChecker{
				events: &events, keyIDs: []string{blocked.KeyID},
				suppressed: map[string]bool{string(blocked.Digest): true},
			}
			gate, err := personenrichment.NewEgressGate(
				recordingConsentChecker{events: &events, active: true}, suppressions, hasher,
				func(string) (string, bool) {
					events = append(events, "credential")
					return "secret", true
				},
			)
			requirements.NoError(err)

			_, err = gate.Authorize(t.Context(), personenrichment.EgressInput{
				Request: personenrichment.Request{Identity: test.identity},
				Profile: profile, KnownProviderPersonIDs: test.knownIDs,
			})
			requirements.ErrorIs(err, personenrichment.ErrSuppressed)
			assert.NotContains(t, events, "credential")
		})
	}
}

func TestEgressGateReturnsOnlyDisclosedSuppressibleIdentifiers(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	events := []string{}
	hasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{0x51}, 32))
	requirements.NoError(err)
	profile := egressProfile(t, []personenrichment.IdentifierClass{
		personenrichment.IdentifierName,
		personenrichment.IdentifierEmail,
		personenrichment.IdentifierPhone,
		personenrichment.IdentifierCurrentCompany,
		personenrichment.IdentifierPublicProfileURL,
	})
	probe := hasher.Digest(profile.ProviderNamespace, personenrichment.SuppressionEmail,
		personenrichment.EmailNormalizationV1, "person@example.com")
	suppressions := &recordingSuppressionChecker{
		events: &events, keyIDs: []string{probe.KeyID}, suppressed: map[string]bool{},
	}
	gate, err := personenrichment.NewEgressGate(
		recordingConsentChecker{events: &events, active: true}, suppressions, hasher,
		func(name string) (string, bool) {
			events = append(events, "credential:"+name)
			return "in-memory-secret", true
		},
	)
	requirements.NoError(err)

	authorization, err := gate.Authorize(t.Context(), personenrichment.EgressInput{
		Request: personenrichment.Request{Identity: personenrichment.Identity{
			Name: "Alice Example", Email: "PERSON@EXAMPLE.COM", Phone: "415-555-0123",
			CurrentCompany:    "Example Labs",
			PublicProfileURLs: []string{"https://example.com/people/a"},
		}},
		Profile: profile, KnownProviderPersonIDs: []string{"Opaque-ID"},
	})
	requirements.NoError(err)
	checks.Equal("in-memory-secret", authorization.Credential)
	classes := make([]personenrichment.SuppressionIdentifierClass, len(authorization.DisclosedIdentifiers))
	for i := range authorization.DisclosedIdentifiers {
		classes[i] = authorization.DisclosedIdentifiers[i].IdentifierClass
	}
	checks.Equal([]personenrichment.SuppressionIdentifierClass{
		personenrichment.SuppressionEmail,
		personenrichment.SuppressionNameCompany,
		personenrichment.SuppressionPhone,
		personenrichment.SuppressionPublicProfileURL,
	}, classes)
	checks.NotContains(classes, personenrichment.SuppressionProviderPersonID)
	checks.Equal([]string{
		"consent", "key_ids", "suppression", "suppression", "suppression",
		"suppression", "suppression", "credential:PROVIDER_API_KEY",
	}, events)

	authorization.DisclosedIdentifiers[0].Digest[0] ^= 0xff
	authorizationAgain, err := gate.Authorize(t.Context(), personenrichment.EgressInput{
		Request: personenrichment.Request{Identity: personenrichment.Identity{
			Name: "Alice Example", Email: "PERSON@EXAMPLE.COM", Phone: "415-555-0123",
			CurrentCompany:    "Example Labs",
			PublicProfileURLs: []string{"https://example.com/people/a"},
		}},
		Profile: profile, KnownProviderPersonIDs: []string{"Opaque-ID"},
	})
	requirements.NoError(err)
	checks.NotEqual(authorization.DisclosedIdentifiers[0].Digest, authorizationAgain.DisclosedIdentifiers[0].Digest)
}

func TestEgressGateRejectsUnpairedNameOrCompanyBeforeCredentialLookup(t *testing.T) {
	for _, identity := range []personenrichment.Identity{
		{Name: "Alice Example"},
		{CurrentCompany: "Example Labs"},
	} {
		t.Run(fmt.Sprintf("name=%t-company=%t", identity.Name != "", identity.CurrentCompany != ""), func(t *testing.T) {
			checks := assert.New(t)
			requirements := require.New(t)
			events := []string{}
			hasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{0x52}, 32))
			requirements.NoError(err)
			profile := egressProfile(t, []personenrichment.IdentifierClass{
				personenrichment.IdentifierName, personenrichment.IdentifierCurrentCompany,
			})
			gate, err := personenrichment.NewEgressGate(
				recordingConsentChecker{events: &events, active: true},
				&recordingSuppressionChecker{events: &events, suppressed: map[string]bool{}},
				hasher,
				func(string) (string, bool) {
					events = append(events, "credential")
					return "secret", true
				},
			)
			requirements.NoError(err)

			_, err = gate.Authorize(t.Context(), personenrichment.EgressInput{
				Request: personenrichment.Request{Identity: identity}, Profile: profile,
			})
			requirements.ErrorContains(err, "name and current company")
			checks.NotContains(events, "credential")
		})
	}
}

func TestEgressGateFailsClosedBeforeCredentials(t *testing.T) {
	hasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{0x61}, 32))
	require.NoError(t, err)
	profile := egressProfile(t, []personenrichment.IdentifierClass{personenrichment.IdentifierEmail})
	keyProbe := hasher.Digest(profile.ProviderNamespace, personenrichment.SuppressionEmail,
		personenrichment.EmailNormalizationV1, "person@example.com")
	checkerFailure := errors.New("suppression store unavailable")
	tests := []struct {
		name       string
		profile    personenrichment.ProviderProfile
		consent    recordingConsentChecker
		configure  func(*recordingSuppressionChecker)
		wantEvents []string
		wantError  error
	}{
		{
			name: "invalid profile", profile: personenrichment.ProviderProfile{},
			consent: recordingConsentChecker{active: true}, wantEvents: []string{},
		},
		{
			name: "missing consent", profile: profile,
			consent: recordingConsentChecker{active: false}, wantEvents: []string{"consent"},
		},
		{
			name: "consent lookup failure", profile: profile,
			consent:    recordingConsentChecker{active: true, err: checkerFailure},
			wantEvents: []string{"consent"}, wantError: checkerFailure,
		},
		{
			name: "durable key mismatch", profile: profile,
			consent:    recordingConsentChecker{active: true},
			configure:  func(s *recordingSuppressionChecker) { s.keyIDs = []string{strings.Repeat("0", 64)} },
			wantEvents: []string{"consent", "key_ids"}, wantError: personenrichment.ErrSuppressionKeyMismatch,
		},
		{
			name: "key ID lookup failure", profile: profile,
			consent:    recordingConsentChecker{active: true},
			configure:  func(s *recordingSuppressionChecker) { s.keyIDsErr = checkerFailure },
			wantEvents: []string{"consent", "key_ids"}, wantError: checkerFailure,
		},
		{
			name: "suppression lookup failure", profile: profile,
			consent: recordingConsentChecker{active: true},
			configure: func(s *recordingSuppressionChecker) {
				s.keyIDs = []string{keyProbe.KeyID}
				s.lookupErr = checkerFailure
			},
			wantEvents: []string{"consent", "key_ids", "suppression"}, wantError: checkerFailure,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checks := assert.New(t)
			requirements := require.New(t)
			events := []string{}
			consent := test.consent
			consent.events = &events
			suppressions := &recordingSuppressionChecker{
				events: &events, keyIDs: []string{keyProbe.KeyID}, suppressed: map[string]bool{},
			}
			if test.configure != nil {
				test.configure(suppressions)
			}
			gate, gateErr := personenrichment.NewEgressGate(
				consent, suppressions, hasher,
				func(string) (string, bool) {
					events = append(events, "credential")
					return "secret", true
				},
			)
			requirements.NoError(gateErr)
			_, authorizeErr := gate.Authorize(t.Context(), personenrichment.EgressInput{
				Request: personenrichment.Request{Identity: personenrichment.Identity{Email: "person@example.com"}},
				Profile: test.profile,
			})
			requirements.Error(authorizeErr)
			if test.wantError != nil {
				requirements.ErrorIs(authorizeErr, test.wantError)
			}
			checks.Equal(test.wantEvents, events)
			checks.NotContains(events, "credential")
		})
	}
}

func TestEgressGateRejectsMissingCredentialAfterSuppressionChecks(t *testing.T) {
	requirements := require.New(t)
	events := []string{}
	hasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{0x71}, 32))
	requirements.NoError(err)
	profile := egressProfile(t, []personenrichment.IdentifierClass{personenrichment.IdentifierEmail})
	probe := hasher.Digest(profile.ProviderNamespace, personenrichment.SuppressionEmail,
		personenrichment.EmailNormalizationV1, "person@example.com")
	gate, err := personenrichment.NewEgressGate(
		recordingConsentChecker{events: &events, active: true},
		&recordingSuppressionChecker{events: &events, keyIDs: []string{probe.KeyID}, suppressed: map[string]bool{}},
		hasher,
		func(string) (string, bool) {
			events = append(events, "credential")
			return "", false
		},
	)
	requirements.NoError(err)

	_, err = gate.Authorize(t.Context(), personenrichment.EgressInput{
		Request: personenrichment.Request{Identity: personenrichment.Identity{Email: "person@example.com"}},
		Profile: profile,
	})
	requirements.Error(err)
	assert.Equal(t, []string{"consent", "key_ids", "suppression", "credential"}, events)
}

func TestEgressGateResolvesCredentialsByStableProviderProfile(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	hasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{0x79}, 32))
	requirements.NoError(err)
	profiles := make([]personenrichment.ProviderProfile, 0, 2)
	for _, name := range []string{"exa-primary", "exa-secondary"} {
		provider := validProviderConfig(personenrichment.ProviderExa)
		provider.Name = name
		provider.APIKeyEnv = "SHARED_EXA_KEY"
		provider.AllowedIdentifiers = []personenrichment.IdentifierClass{personenrichment.IdentifierEmail}
		profile, profileErr := provider.Profile(profileCatalog())
		requirements.NoError(profileErr)
		profiles = append(profiles, profile)
	}
	events := []string{}
	probe := hasher.Digest(profiles[0].ProviderNamespace, personenrichment.SuppressionEmail,
		personenrichment.EmailNormalizationV1, "person@example.com")
	gate, err := personenrichment.NewProviderBoundEgressGate(
		recordingConsentChecker{events: &events, active: true},
		&recordingSuppressionChecker{events: &events, keyIDs: []string{probe.KeyID}, suppressed: map[string]bool{}},
		hasher,
		func(profile personenrichment.ProviderProfile) (string, bool, error) {
			events = append(events, "credential:"+profile.Name)
			return "stored-" + profile.Name, true, nil
		},
	)
	requirements.NoError(err)

	for _, profile := range profiles {
		authorization, authorizeErr := gate.Authorize(t.Context(), personenrichment.EgressInput{
			Request: personenrichment.Request{Identity: personenrichment.Identity{Email: "person@example.com"}},
			Profile: profile,
		})
		requirements.NoError(authorizeErr)
		assertions.Equal("stored-"+profile.Name, authorization.Credential)
	}
	assertions.Contains(events, "credential:exa-primary")
	assertions.Contains(events, "credential:exa-secondary")
}

func TestEgressGateProviderCredentialResolutionFailureDoesNotUseEnvironmentFallback(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	hasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{0x7a}, 32))
	requirements.NoError(err)
	profile := egressProfile(t, []personenrichment.IdentifierClass{personenrichment.IdentifierEmail})
	events := []string{}
	probe := hasher.Digest(profile.ProviderNamespace, personenrichment.SuppressionEmail,
		personenrichment.EmailNormalizationV1, "person@example.com")
	resolutionErr := errors.New("credential store binding mismatch")
	gate, err := personenrichment.NewProviderBoundEgressGate(
		recordingConsentChecker{events: &events, active: true},
		&recordingSuppressionChecker{events: &events, keyIDs: []string{probe.KeyID}, suppressed: map[string]bool{}},
		hasher,
		func(personenrichment.ProviderProfile) (string, bool, error) {
			events = append(events, "provider_credential")
			return "", false, resolutionErr
		},
	)
	requirements.NoError(err)
	gate.LookupCredential = func(string) (string, bool) {
		events = append(events, "environment")
		return "environment-secret", true
	}

	_, err = gate.Authorize(t.Context(), personenrichment.EgressInput{
		Request: personenrichment.Request{Identity: personenrichment.Identity{Email: "person@example.com"}},
		Profile: profile,
	})
	requirements.ErrorIs(err, resolutionErr)
	assertions.NotContains(events, "environment")
}

func TestEgressGateRejectsInvalidConstruction(t *testing.T) {
	hasher, err := personenrichment.NewSuppressionHasher(bytes.Repeat([]byte{0x81}, 32))
	require.NoError(t, err)
	events := []string{}
	consent := recordingConsentChecker{events: &events, active: true}
	suppressions := &recordingSuppressionChecker{events: &events}
	lookup := personenrichment.CredentialLookup(func(string) (string, bool) { return "secret", true })

	tests := []struct {
		name         string
		consent      personenrichment.ConsentChecker
		suppressions personenrichment.SuppressionChecker
		hasher       *personenrichment.SuppressionHasher
		lookup       personenrichment.CredentialLookup
	}{
		{"nil consent", nil, suppressions, hasher, lookup},
		{"nil suppressions", consent, nil, hasher, lookup},
		{"nil hasher", consent, suppressions, nil, lookup},
		{"nil credential lookup", consent, suppressions, hasher, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, gateErr := personenrichment.NewEgressGate(
				test.consent, test.suppressions, test.hasher, test.lookup,
			)
			require.Error(t, gateErr)
		})
	}
}

func TestEgressGateRejectsZeroValueSuppressionHasherBeforeConsentOrCredential(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	events := []string{}
	profile := egressProfile(t, []personenrichment.IdentifierClass{personenrichment.IdentifierName})
	consent := recordingConsentChecker{events: &events, active: true}
	suppressions := &recordingSuppressionChecker{events: &events, suppressed: map[string]bool{}}
	lookup := personenrichment.CredentialLookup(func(string) (string, bool) {
		events = append(events, "credential")
		return "secret", true
	})
	invalidHasher := &personenrichment.SuppressionHasher{}

	_, err := personenrichment.NewEgressGate(consent, suppressions, invalidHasher, lookup)
	requirements.Error(err)
	checks.Empty(events)

	gate := personenrichment.EgressGate{
		Consent: consent, Suppressions: suppressions,
		Hasher: invalidHasher, LookupCredential: lookup,
	}
	_, err = gate.Authorize(t.Context(), personenrichment.EgressInput{
		Request: personenrichment.Request{Identity: personenrichment.Identity{Name: "Alice Example"}},
		Profile: profile,
	})
	requirements.Error(err)
	checks.Empty(events)
}

func TestEgressGateZeroValueFailsClosed(t *testing.T) {
	var gate personenrichment.EgressGate
	assert.NotPanics(t, func() {
		_, err := gate.Authorize(t.Context(), personenrichment.EgressInput{
			Request: personenrichment.Request{Identity: personenrichment.Identity{Email: "person@example.com"}},
			Profile: egressProfile(t, []personenrichment.IdentifierClass{personenrichment.IdentifierEmail}),
		})
		require.Error(t, err)
	})
}

func TestSuppressionOnlyClassesCannotBeAllowedForEgressAndNameAloneCannotBeSuppressed(t *testing.T) {
	for _, class := range []personenrichment.IdentifierClass{
		personenrichment.IdentifierClass(personenrichment.SuppressionProviderPersonID),
		personenrichment.IdentifierClass(personenrichment.SuppressionNameCompany),
	} {
		t.Run(string(class), func(t *testing.T) {
			provider := validProviderConfig(personenrichment.ProviderExa)
			provider.AllowedIdentifiers = []personenrichment.IdentifierClass{class}
			_, err := provider.Profile(profileCatalog())
			require.ErrorContains(t, err, "allowed_identifiers")
		})
	}

	_, err := personenrichment.NormalizeSuppressionIdentifier(
		personenrichment.SuppressionNameCompany, []string{"Alice Example"},
	)
	require.ErrorContains(t, err, "exactly name and company")
}

func TestProviderErrorIsSafeAndClassifiesRetryability(t *testing.T) {
	for _, test := range []struct {
		class     personenrichment.FailureClass
		retryable bool
	}{
		{personenrichment.FailurePolicy, false},
		{personenrichment.FailureSuppressed, false},
		{personenrichment.FailureRateLimited, true},
		{personenrichment.FailureTransient, true},
		{personenrichment.FailureInvalidOutput, false},
		{personenrichment.FailureIdentityRejected, false},
		{personenrichment.FailureTerminal, false},
		{personenrichment.FailureUncertainStart, false},
	} {
		t.Run(string(test.class), func(t *testing.T) {
			checks := assert.New(t)
			sentinels := []string{
				"api-key-secret", "person@example.com", "+14155550123",
				"https://example.com/private", `{"request":"private"}`, `{"response":"private"}`,
			}
			err := &personenrichment.ProviderError{
				Provider: "exa", RequestID: "opaque-request-42", Status: 429,
				Class: test.class, RetryAfter: strings.Join(sentinels, "|"),
			}
			message := err.Error()
			checks.Contains(message, "exa")
			checks.Contains(message, "429")
			checks.Contains(message, "opaque-request-42")
			checks.Contains(message, string(test.class))
			for _, sentinel := range sentinels {
				checks.NotContains(message, sentinel)
			}
			checks.Equal(test.retryable, err.Retryable())
		})
	}

	var nilError *personenrichment.ProviderError
	assert.False(t, nilError.Retryable())
}

func egressProfile(t *testing.T, identifiers []personenrichment.IdentifierClass) personenrichment.ProviderProfile {
	t.Helper()
	provider := validProviderConfig(personenrichment.ProviderExa)
	provider.AllowedIdentifiers = identifiers
	profile, err := provider.Profile(profileCatalog())
	require.NoError(t, err)
	return profile
}

var _ personenrichment.ConsentChecker = recordingConsentChecker{}
var _ personenrichment.SuppressionChecker = (*recordingSuppressionChecker)(nil)
