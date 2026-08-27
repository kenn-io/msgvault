package personenrichment_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/personfacts"
)

func TestProviderProfileValidateRejectsEveryPolicyBoundFieldMutation(t *testing.T) {
	provider := validProviderConfig(personenrichment.ProviderExa)
	provider.AllowedIdentifiers = []personenrichment.IdentifierClass{
		personenrichment.IdentifierEmail,
		personenrichment.IdentifierName,
	}
	baseline, err := provider.Profile(profileCatalog())
	require.NoError(t, err)
	require.NoError(t, baseline.Validate())

	mutations := []struct {
		name   string
		mutate func(*personenrichment.ProviderProfile)
	}{
		{"fingerprint", func(p *personenrichment.ProviderProfile) { p.Fingerprint = strings.Repeat("0", 64) }},
		{"provider kind", func(p *personenrichment.ProviderProfile) { p.Kind = personenrichment.ProviderSixtyfour }},
		{"provider namespace", func(p *personenrichment.ProviderProfile) { p.ProviderNamespace = "exa:" + strings.Repeat("0", 64) }},
		{"catalog fingerprint", func(p *personenrichment.ProviderProfile) { p.CatalogFingerprint = "sha256:" + strings.Repeat("0", 64) }},
		{"start endpoint", func(p *personenrichment.ProviderProfile) { p.Endpoint = "https://other.example.test/search" }},
		{"poll endpoint", func(p *personenrichment.ProviderProfile) { p.PollEndpoint = "https://other.example.test/poll" }},
		{"API key environment", func(p *personenrichment.ProviderProfile) { p.APIKeyEnv = "OTHER_API_KEY" }},
		{"mode", func(p *personenrichment.ProviderProfile) { p.Mode = "deep" }},
		{"tier", func(p *personenrichment.ProviderProfile) { p.Tier = "standard" }},
		{"result count", func(p *personenrichment.ProviderProfile) { p.NumResults++ }},
		{"allowed identifiers", func(p *personenrichment.ProviderProfile) {
			p.AllowedIdentifiers = []personenrichment.IdentifierClass{personenrichment.IdentifierEmail}
		}},
		{"target descriptor", func(p *personenrichment.ProviderProfile) { p.Targets[0].Revision = "revision-2" }},
		{"sensitive target posture", func(p *personenrichment.ProviderProfile) { p.AllowSensitiveTargets = true }},
		{"retention posture", func(p *personenrichment.ProviderProfile) { p.RetentionPosture = "provider_retained" }},
		{"training posture", func(p *personenrichment.ProviderProfile) { p.TrainingPosture = "provider_training" }},
		{"refresh interval", func(p *personenrichment.ProviderProfile) { p.RefreshInterval++ }},
		{"per-run request cap", func(p *personenrichment.ProviderProfile) { p.MaxRequestsPerRun++ }},
		{"daily request cap", func(p *personenrichment.ProviderProfile) { p.MaxRequestsPerDay++ }},
		{"per-person daily cost cap", func(p *personenrichment.ProviderProfile) {
			p.MaxCostUSDMicrosPerPersonPerDay++
		}},
		{"per-run cost cap", func(p *personenrichment.ProviderProfile) { p.MaxCostUSDMicrosPerRun++ }},
		{"daily cost cap", func(p *personenrichment.ProviderProfile) { p.MaxCostUSDMicrosPerDay++ }},
		{"policy bytes", func(p *personenrichment.ProviderProfile) { p.PolicyJSON = append(p.PolicyJSON, '\n') }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			changed := baseline
			changed.AllowedIdentifiers = slices.Clone(baseline.AllowedIdentifiers)
			changed.Targets = slices.Clone(baseline.Targets)
			changed.PolicyJSON = slices.Clone(baseline.PolicyJSON)
			mutation.mutate(&changed)
			require.Error(t, changed.Validate())
		})
	}

	blankName := baseline
	blankName.Name = " "
	require.Error(t, blankName.Validate())
}

func TestProviderProfileValidateRecomputesNamespaceFromEndpoints(t *testing.T) {
	profile, err := validProviderConfig(personenrichment.ProviderExa).Profile(profileCatalog())
	require.NoError(t, err)
	forgedNamespace := "exa:" + strings.Repeat("f", 64)
	oldField := fmt.Sprintf(`"provider_namespace":%q`, profile.ProviderNamespace)
	newField := fmt.Sprintf(`"provider_namespace":%q`, forgedNamespace)
	forgedPolicy := strings.Replace(string(profile.PolicyJSON), oldField, newField, 1)
	require.NotEqual(t, string(profile.PolicyJSON), forgedPolicy)
	digest := sha256.Sum256([]byte(forgedPolicy))
	profile.ProviderNamespace = forgedNamespace
	profile.Fingerprint = hex.EncodeToString(digest[:])
	profile.PolicyJSON = []byte(forgedPolicy)

	require.ErrorContains(t, profile.Validate(), "canonical")
}

func TestProviderProfileFingerprintCoversConsentedPolicy(t *testing.T) {
	checks := assert.New(t)
	provider := validProviderConfig(personenrichment.ProviderExa)
	provider.AllowedIdentifiers = []personenrichment.IdentifierClass{
		personenrichment.IdentifierEmail,
		personenrichment.IdentifierName,
	}
	catalog := profileCatalog()
	baseline, err := provider.Profile(catalog)
	require.NoError(t, err)
	checks.Regexp(`^[a-f0-9]{64}$`, baseline.Fingerprint)
	checks.Regexp(`^exa:[a-f0-9]{64}$`, baseline.ProviderNamespace)
	checks.NotEmpty(baseline.CatalogFingerprint)

	mutations := []struct {
		name   string
		mutate func(*personenrichment.ProviderConfig, *personfacts.Catalog)
	}{
		{"endpoint", func(c *personenrichment.ProviderConfig, _ *personfacts.Catalog) {
			c.Endpoint = "https://other.example.test/search"
		}},
		{"kind", func(c *personenrichment.ProviderConfig, _ *personfacts.Catalog) {
			c.Kind = personenrichment.ProviderSixtyfour
			c.Endpoint = "https://api.example.test/start"
			c.PollEndpoint = "https://api.example.test/poll"
			c.Mode = ""
			c.Tier = "standard"
			c.NumResults = 0
			c.AllowedIdentifiers = []personenrichment.IdentifierClass{
				personenrichment.IdentifierName, personenrichment.IdentifierCurrentCompany,
			}
		}},
		{"credential environment", func(c *personenrichment.ProviderConfig, _ *personfacts.Catalog) { c.APIKeyEnv = "OTHER_API_KEY" }},
		{"identifier set", func(c *personenrichment.ProviderConfig, _ *personfacts.Catalog) {
			c.AllowedIdentifiers = []personenrichment.IdentifierClass{personenrichment.IdentifierEmail}
		}},
		{"target revision", func(_ *personenrichment.ProviderConfig, c *personfacts.Catalog) { c.Targets[0].Revision = "revision-2" }},
		{"sensitive posture", func(c *personenrichment.ProviderConfig, _ *personfacts.Catalog) { c.AllowSensitiveTargets = true }},
		{"retention posture", func(c *personenrichment.ProviderConfig, _ *personfacts.Catalog) {
			c.RetentionPosture = "provider_retained"
		}},
		{"training posture", func(c *personenrichment.ProviderConfig, _ *personfacts.Catalog) {
			c.TrainingPosture = "provider_training"
		}},
		{"refresh interval", func(c *personenrichment.ProviderConfig, _ *personfacts.Catalog) { c.RefreshInterval = 48 * time.Hour }},
		{"per run requests", func(c *personenrichment.ProviderConfig, _ *personfacts.Catalog) { c.MaxRequestsPerRun++ }},
		{"per day requests", func(c *personenrichment.ProviderConfig, _ *personfacts.Catalog) { c.MaxRequestsPerDay++ }},
		{"per person cost", func(c *personenrichment.ProviderConfig, _ *personfacts.Catalog) {
			c.MaxCostUSDMicrosPerPersonPerDay = 1
		}},
		{"per run cost", func(c *personenrichment.ProviderConfig, _ *personfacts.Catalog) { c.MaxCostUSDMicrosPerRun = 1 }},
		{"per day cost", func(c *personenrichment.ProviderConfig, _ *personfacts.Catalog) { c.MaxCostUSDMicrosPerDay = 1 }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			changedProvider := provider
			changedProvider.AllowedIdentifiers = slices.Clone(provider.AllowedIdentifiers)
			changedProvider.TargetKeys = slices.Clone(provider.TargetKeys)
			changedCatalog := catalog
			changedCatalog.Targets = slices.Clone(catalog.Targets)
			mutation.mutate(&changedProvider, &changedCatalog)
			changed, profileErr := changedProvider.Profile(changedCatalog)
			require.NoError(t, profileErr)
			assert.NotEqual(t, baseline.Fingerprint, changed.Fingerprint)
		})
	}
}

func TestProviderProfileCanonicalizesOrderAndExcludesOperationalSettings(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	provider := validProviderConfig(personenrichment.ProviderExa)
	provider.AllowedIdentifiers = []personenrichment.IdentifierClass{
		personenrichment.IdentifierEmail,
		personenrichment.IdentifierName,
	}
	provider.TargetKeys = []string{"attribute:timezone", "attribute:bio"}
	catalog := profileCatalog()
	catalog.Targets = append(catalog.Targets, personfacts.TargetDescriptor{
		Kind: personfacts.TargetAttribute, Key: "attribute:timezone", Revision: "revision-1",
		UniversalID: "attribute:timezone", Slug: "timezone", Description: "Fixture timezone",
		ValueType: personfacts.ValueText, Cardinality: personfacts.CardinalitySingle,
		Choices: []personfacts.ChoiceDescriptor{}, Fields: []personfacts.FieldDescriptor{},
	})
	baseline, err := provider.Profile(catalog)
	requirements.NoError(err)

	reordered := provider
	reordered.AllowedIdentifiers = []personenrichment.IdentifierClass{
		personenrichment.IdentifierName,
		personenrichment.IdentifierEmail,
	}
	reordered.TargetKeys = []string{"attribute:bio", "attribute:timezone"}
	reorderedCatalog := catalog
	reorderedCatalog.Targets = slices.Clone(catalog.Targets)
	slices.Reverse(reorderedCatalog.Targets)
	canonical, err := reordered.Profile(reorderedCatalog)
	requirements.NoError(err)
	checks.Equal(baseline.Fingerprint, canonical.Fingerprint)
	checks.Equal(baseline.PolicyJSON, canonical.PolicyJSON)

	operational := []struct {
		name   string
		mutate func(*personenrichment.ProviderConfig, *personenrichment.Config)
	}{
		{"request timeout", func(p *personenrichment.ProviderConfig, _ *personenrichment.Config) { p.RequestTimeout++ }},
		{"poll interval", func(p *personenrichment.ProviderConfig, _ *personenrichment.Config) { p.PollInterval++ }},
		{"maximum job age", func(p *personenrichment.ProviderConfig, _ *personenrichment.Config) { p.MaxJobAge++ }},
		{"batch size", func(_ *personenrichment.ProviderConfig, c *personenrichment.Config) { c.BatchSize++ }},
		{"lease duration", func(_ *personenrichment.ProviderConfig, c *personenrichment.Config) { c.LeaseDuration++ }},
		{"schedule", func(_ *personenrichment.ProviderConfig, c *personenrichment.Config) { c.Schedule = "0 * * * *" }},
	}
	for _, mutation := range operational {
		t.Run(mutation.name, func(t *testing.T) {
			changedProvider := provider
			owner := personenrichment.Config{Schedule: "*/15 * * * *", BatchSize: 25, LeaseDuration: 5 * time.Minute}
			mutation.mutate(&changedProvider, &owner)
			changed, profileErr := changedProvider.Profile(catalog)
			require.NoError(t, profileErr)
			assert.Equal(t, baseline.Fingerprint, changed.Fingerprint)
		})
	}

	var policy map[string]any
	requirements.NoError(json.Unmarshal(baseline.PolicyJSON, &policy))
	checks.NotContains(policy, "request_timeout")
	checks.NotContains(policy, "poll_interval")
	checks.NotContains(policy, "max_job_age")
}

func TestProviderProfileRejectsUnknownAndSensitiveTargets(t *testing.T) {
	provider := validProviderConfig(personenrichment.ProviderExa)
	provider.TargetKeys = []string{"attribute:missing"}
	_, err := provider.Profile(profileCatalog())
	require.ErrorContains(t, err, "target")

	provider.TargetKeys = []string{"attribute:bio"}
	catalog := profileCatalog()
	catalog.Targets[0].Sensitive = true
	_, err = provider.Profile(catalog)
	require.ErrorContains(t, err, "sensitive")
}

func TestSixtyfourProviderProfileRejectsStructuredTargets(t *testing.T) {
	catalog, err := personfacts.BuildCatalog(nil, personfacts.CatalogOptions{})
	require.NoError(t, err)
	provider := validProviderConfig(personenrichment.ProviderSixtyfour)
	provider.TargetKeys = []string{"system:employment"}

	_, err = provider.Profile(catalog)
	require.Error(t, err)
}

func TestProviderProfileValidatesPolicyWhenProviderIsDisabled(t *testing.T) {
	requirements := require.New(t)
	catalog := profileCatalog()
	enabled := validProviderConfig(personenrichment.ProviderExa)
	want, err := enabled.Profile(catalog)
	requirements.NoError(err)

	disabled := enabled
	disabled.Enabled = false
	got, err := disabled.Profile(catalog)
	requirements.NoError(err)
	assert.Equal(t, want.Fingerprint, got.Fingerprint)

	var defaults personenrichment.ProviderConfig
	requirements.NoError(defaults.Validate(), "an unconfigured disabled provider stays runtime-safe")
	_, err = defaults.Profile(catalog)
	requirements.Error(err)

	tests := []struct {
		name   string
		mutate func(*personenrichment.ProviderConfig)
		want   string
	}{
		{"empty kind", func(c *personenrichment.ProviderConfig) { c.Kind = "" }, "kind"},
		{"unknown kind", func(c *personenrichment.ProviderConfig) { c.Kind = "unknown" }, "kind"},
		{"invalid credential env", func(c *personenrichment.ProviderConfig) { c.APIKeyEnv = "BAD-NAME" }, "api_key_env"},
		{"no identifiers", func(c *personenrichment.ProviderConfig) { c.AllowedIdentifiers = nil }, "allowed_identifiers"},
		{"zero request cap", func(c *personenrichment.ProviderConfig) { c.MaxRequestsPerRun = 0 }, "max_requests_per_run"},
		{"missing posture", func(c *personenrichment.ProviderConfig) { c.RetentionPosture = "" }, "retention_posture"},
		{"invalid provider fields", func(c *personenrichment.ProviderConfig) { c.Mode = "quick" }, "mode"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := disabled
			test.mutate(&changed)
			_, profileErr := changed.Profile(catalog)
			require.ErrorContains(t, profileErr, test.want)
		})
	}
}

func profileCatalog() personfacts.Catalog {
	return personfacts.Catalog{
		Version: "1",
		Targets: []personfacts.TargetDescriptor{{
			Kind: personfacts.TargetAttribute, Key: "attribute:bio", Revision: "revision-1",
			UniversalID: "attribute:bio", Slug: "bio", Description: "Fixture biography",
			ValueType: personfacts.ValueText, Cardinality: personfacts.CardinalitySingle,
			Choices: []personfacts.ChoiceDescriptor{}, Fields: []personfacts.FieldDescriptor{},
		}},
	}
}
