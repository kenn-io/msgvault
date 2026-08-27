package personenrichment_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/personfacts"
)

type contractProvider struct{ immediate bool }

func (p *contractProvider) Start(
	_ context.Context,
	_ personenrichment.Request,
) (personenrichment.Attempt, error) {
	if p.immediate {
		result := personenrichment.Result{State: personenrichment.ResultComplete, ProviderVersion: "provider-v1"}
		return personenrichment.Attempt{
			State:  personenrichment.AttemptComplete,
			Result: &result,
		}, nil
	}
	return personenrichment.Attempt{
		State: personenrichment.AttemptPending,
		JobID: "opaque-job",
	}, nil
}

func (p *contractProvider) Poll(
	_ context.Context,
	attempt personenrichment.Attempt,
) (personenrichment.Result, error) {
	return personenrichment.Result{
		State: personenrichment.ResultComplete,
		JobID: attempt.JobID, ProviderVersion: "provider-v1",
	}, nil
}

func TestProviderContractSupportsImmediateAndAsyncResults(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	var _ personenrichment.Provider = (*contractProvider)(nil)
	immediate, err := (&contractProvider{immediate: true}).Start(
		t.Context(),
		personenrichment.Request{},
	)
	requirements.NoError(err)
	requirements.NotNil(immediate.Result)
	checks.Equal(personenrichment.AttemptComplete, immediate.State)

	pending, err := (&contractProvider{}).Start(t.Context(), personenrichment.Request{})
	requirements.NoError(err)
	checks.Equal(personenrichment.AttemptPending, pending.State)
	checks.Equal("opaque-job", pending.JobID)
}

func TestProviderContractValidationRejectsMalformedStates(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	complete := personenrichment.Result{State: personenrichment.ResultComplete, ProviderVersion: "provider-v1"}
	tests := []struct {
		name    string
		attempt personenrichment.Attempt
		want    string
	}{
		{
			name:    "complete result missing",
			attempt: personenrichment.Attempt{State: personenrichment.AttemptComplete},
			want:    "complete result",
		},
		{
			name: "complete result pending",
			attempt: personenrichment.Attempt{
				State:  personenrichment.AttemptComplete,
				Result: &personenrichment.Result{State: personenrichment.ResultPending},
			},
			want: "complete result",
		},
		{
			name: "complete request ID whitespace",
			attempt: personenrichment.Attempt{
				State:     personenrichment.AttemptComplete,
				RequestID: "  ",
				Result:    &complete,
			},
			want: "request ID",
		},
		{
			name: "complete job ID whitespace",
			attempt: personenrichment.Attempt{
				State:  personenrichment.AttemptComplete,
				JobID:  "\t",
				Result: &complete,
			},
			want: "job ID",
		},
		{
			name: "pending request ID whitespace",
			attempt: personenrichment.Attempt{
				State:     personenrichment.AttemptPending,
				RequestID: "\n",
				JobID:     "opaque-job",
			},
			want: "request ID",
		},
		{
			name: "pending job empty",
			attempt: personenrichment.Attempt{
				State: personenrichment.AttemptPending,
				JobID: "  ",
			},
			want: "job ID",
		},
		{
			name: "pending has result",
			attempt: personenrichment.Attempt{
				State:  personenrichment.AttemptPending,
				JobID:  "opaque-job",
				Result: &complete,
			},
			want: "nil result",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.ErrorContains(t, test.attempt.Validate(), test.want)
		})
	}

	validPending := personenrichment.Attempt{
		State:     personenrichment.AttemptPending,
		RequestID: " request-id ",
		JobID:     " opaque-job ",
	}
	requirements.NoError(validPending.Validate())
	checks.Equal(" request-id ", validPending.RequestID, "opaque IDs must not be rewritten")
	checks.Equal(" opaque-job ", validPending.JobID, "opaque IDs must not be rewritten")

	validComplete := personenrichment.Attempt{
		State:     personenrichment.AttemptComplete,
		RequestID: " request-id ",
		JobID:     " job-id ",
		Result: &personenrichment.Result{
			State:           personenrichment.ResultComplete,
			RequestID:       " result-request-id ",
			JobID:           " result-job-id ",
			ProviderVersion: "provider-v1",
		},
	}
	requirements.NoError(validComplete.Validate())
	checks.Equal(" request-id ", validComplete.RequestID)
	checks.Equal(" job-id ", validComplete.JobID)
	checks.Equal(" result-request-id ", validComplete.Result.RequestID)
	checks.Equal(" result-job-id ", validComplete.Result.JobID)
}

func TestProviderResultValidationRejectsUnsafeDurableValues(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	valid := personenrichment.Result{
		State:              personenrichment.ResultComplete,
		RequestID:          " request-id ",
		JobID:              " job-id ",
		IdentityConfidence: 1000,
		IdentityMatches: []personenrichment.IdentityMatch{{
			Class: personenrichment.IdentifierEmail, Confidence: 999,
		}},
		ProviderPersonIDs: []personenrichment.ProviderPersonID{{ID: " opaque-id ", Confidence: 1}},
		ProviderVersion:   "provider-v1",
		Cost:              personenrichment.Cost{Currency: "USD", AmountMicros: 1},
	}
	requirements.NoError(valid.Validate())
	checks.Equal(" request-id ", valid.RequestID)
	checks.Equal(" job-id ", valid.JobID)
	checks.Equal(" opaque-id ", valid.ProviderPersonIDs[0].ID)

	tests := []struct {
		name   string
		mutate func(*personenrichment.Result)
		want   string
	}{
		{"pending claims", func(r *personenrichment.Result) {
			r.State = personenrichment.ResultPending
			r.JobID = "job"
			r.Claims = make([]personfacts.ProposedClaim, 1)
		}, "claims"},
		{"identity confidence high", func(r *personenrichment.Result) { r.IdentityConfidence = 1001 }, "confidence"},
		{"identity confidence low", func(r *personenrichment.Result) { r.IdentityConfidence = -1 }, "confidence"},
		{"match confidence", func(r *personenrichment.Result) { r.IdentityMatches[0].Confidence = 1001 }, "confidence"},
		{"provider ID confidence", func(r *personenrichment.Result) { r.ProviderPersonIDs[0].Confidence = -1 }, "confidence"},
		{"provider ID whitespace", func(r *personenrichment.Result) { r.ProviderPersonIDs[0].ID = "\t" }, "ID"},
		{"request ID whitespace", func(r *personenrichment.Result) { r.RequestID = "\t" }, "request ID"},
		{"complete job ID whitespace", func(r *personenrichment.Result) { r.JobID = "\n" }, "job ID"},
		{"complete provider version missing", func(r *personenrichment.Result) { r.ProviderVersion = "" }, "provider version"},
		{"negative cost", func(r *personenrichment.Result) { r.Cost.AmountMicros = -1 }, "cost"},
		{"non USD cost", func(r *personenrichment.Result) { r.Cost.Currency = "EUR" }, "USD"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := valid
			changed.IdentityMatches = append([]personenrichment.IdentityMatch(nil), valid.IdentityMatches...)
			changed.ProviderPersonIDs = append([]personenrichment.ProviderPersonID(nil), valid.ProviderPersonIDs...)
			test.mutate(&changed)
			require.ErrorContains(t, changed.Validate(), test.want)
		})
	}

	claimConfidence := valid
	claimConfidence.Claims = []personfacts.ProposedClaim{{
		Confidence: personfacts.ConfidenceInputs{ReportedScore: 1001},
	}}
	requirements.ErrorContains(claimConfidence.Validate(), "confidence")
	for _, score := range []int{-1, 1001} {
		t.Run(fmt.Sprintf("evidence identity score %d", score), func(t *testing.T) {
			claimEvidence := valid
			claimEvidence.Claims = []personfacts.ProposedClaim{{
				Evidence: []personfacts.EvidenceInput{{IdentityScore: score}},
			}}
			require.ErrorContains(t, claimEvidence.Validate(), "evidence 0 identity confidence")
		})
	}

	estimated := personenrichment.Cost{Currency: "USD", AmountMicros: 1, Estimated: true}
	requirements.ErrorContains(estimated.ValidateGuaranteed(), "estimated")
	requirements.ErrorContains(personenrichment.Cost{}.ValidateGuaranteed(), "USD")
	requirements.NoError(personenrichment.Cost{Currency: "USD", AmountMicros: 1}.ValidateGuaranteed())
}

func TestProviderIdentityAssessmentValidationKeepsResolverScoresIntegral(t *testing.T) {
	valid := personenrichment.IdentityAssessment{
		Accepted: true,
		Score:    900,
		MatchedClasses: []personenrichment.IdentifierClass{
			personenrichment.IdentifierEmail,
			personenrichment.IdentifierCurrentCompany,
		},
	}
	require.NoError(t, valid.Validate())

	high := valid
	high.Score = 1001
	require.ErrorContains(t, high.Validate(), "confidence")
	invalidClass := valid
	invalidClass.MatchedClasses = []personenrichment.IdentifierClass{"provider_person_id"}
	require.ErrorContains(t, invalidClass.Validate(), "identifier class")
}

func TestConfigIsDisabledAndHardCostCapsAreOffByDefault(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	var cfg personenrichment.Config
	cfg.ApplyDefaults()
	checks.False(cfg.Enabled)
	checks.Equal("*/15 * * * *", cfg.Schedule)
	checks.Equal(25, cfg.BatchSize)
	checks.Equal(5*time.Minute, cfg.LeaseDuration)
	requirements.NoError(cfg.Validate())

	cfg.Schedule = "0 */15 * * * *"
	requirements.ErrorContains(cfg.Validate(), "schedule")
	cfg.Schedule = "not-a-cron"
	requirements.ErrorContains(cfg.Validate(), "schedule")
	cfg.Schedule = "*/15 * * * *"

	cfg.Enabled = true
	cfg.Providers = []personenrichment.ProviderConfig{{
		Name: "exa", Kind: personenrichment.ProviderExa, Enabled: true,
	}}
	requirements.ErrorContains(cfg.Validate(), "endpoint")

	provider := validProviderConfig(personenrichment.ProviderExa)
	provider.ApplyDefaults()
	checks.Zero(provider.MaxCostUSDMicrosPerPersonPerDay)
	checks.Zero(provider.MaxCostUSDMicrosPerRun)
	checks.Zero(provider.MaxCostUSDMicrosPerDay)
	requirements.NoError(provider.Validate())
}

func TestConfigRejectsUnsupportedBuiltInHardCostCaps(t *testing.T) {
	kinds := []string{personenrichment.ProviderExa, personenrichment.ProviderSixtyfour}
	fields := []struct {
		name   string
		mutate func(*personenrichment.ProviderConfig)
	}{
		{"per person per day", func(c *personenrichment.ProviderConfig) {
			c.MaxCostUSDMicrosPerPersonPerDay = 1
		}},
		{"per run", func(c *personenrichment.ProviderConfig) { c.MaxCostUSDMicrosPerRun = 1 }},
		{"per day", func(c *personenrichment.ProviderConfig) { c.MaxCostUSDMicrosPerDay = 1 }},
	}
	for _, kind := range kinds {
		t.Run(kind+" request caps only", func(t *testing.T) {
			require.NoError(t, validProviderConfig(kind).Validate())
		})
		for _, field := range fields {
			t.Run(kind+" "+field.name, func(t *testing.T) {
				provider := validProviderConfig(kind)
				field.mutate(&provider)
				require.ErrorContains(t, provider.Validate(),
					"hard cost caps unsupported by "+kind+"; use request caps")
			})
		}
	}
}

func TestConfigRejectsUnsafeProviderPolicies(t *testing.T) {
	requirements := require.New(t)
	tests := []struct {
		name   string
		mutate func(*personenrichment.ProviderConfig)
		want   string
	}{
		{"http endpoint", func(c *personenrichment.ProviderConfig) { c.Endpoint = "http://api.example.test" }, "HTTPS"},
		{"bad credential env", func(c *personenrichment.ProviderConfig) { c.APIKeyEnv = "BAD-NAME" }, "api_key_env"},
		{"unsafe provider name", func(c *personenrichment.ProviderConfig) { c.Name = "exa default" }, "name"},
		{"no identifiers", func(c *personenrichment.ProviderConfig) { c.AllowedIdentifiers = nil }, "allowed_identifiers"},
		{"suppression-only identifier", func(c *personenrichment.ProviderConfig) {
			c.AllowedIdentifiers = []personenrichment.IdentifierClass{"provider_person_id"}
		}, "allowed_identifiers"},
		{"no targets", func(c *personenrichment.ProviderConfig) { c.TargetKeys = nil }, "target_keys"},
		{"no refresh", func(c *personenrichment.ProviderConfig) { c.RefreshInterval = 0 }, "refresh_interval"},
		{"no timeout", func(c *personenrichment.ProviderConfig) { c.RequestTimeout = 0 }, "request_timeout"},
		{"no poll interval", func(c *personenrichment.ProviderConfig) { c.PollInterval = 0 }, "poll_interval"},
		{"no max job age", func(c *personenrichment.ProviderConfig) { c.MaxJobAge = 0 }, "max_job_age"},
		{"negative retries", func(c *personenrichment.ProviderConfig) { c.MaxRetries = -1 }, "max_retries"},
		{"no per run request cap", func(c *personenrichment.ProviderConfig) { c.MaxRequestsPerRun = 0 }, "max_requests_per_run"},
		{"no daily request cap", func(c *personenrichment.ProviderConfig) { c.MaxRequestsPerDay = 0 }, "max_requests_per_day"},
		{"negative cost cap", func(c *personenrichment.ProviderConfig) { c.MaxCostUSDMicrosPerRun = -1 }, "non-negative"},
		{"exa bad mode", func(c *personenrichment.ProviderConfig) { c.Mode = "quick" }, "mode"},
		{"exa poll endpoint", func(c *personenrichment.ProviderConfig) { c.PollEndpoint = "https://poll.example.test" }, "poll_endpoint"},
		{"exa tier", func(c *personenrichment.ProviderConfig) { c.Tier = "standard" }, "tier"},
		{"exa zero results", func(c *personenrichment.ProviderConfig) { c.NumResults = 0 }, "num_results"},
		{"exa multiple results", func(c *personenrichment.ProviderConfig) { c.NumResults = 2 }, "num_results"},
		{"exa too many results", func(c *personenrichment.ProviderConfig) { c.NumResults = 101 }, "num_results"},
		{"too many targets", func(c *personenrichment.ProviderConfig) {
			c.TargetKeys = make([]string, 101)
			for i := range c.TargetKeys {
				c.TargetKeys[i] = fmt.Sprintf("attribute:target-%03d", i)
			}
		}, "target_keys"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := validProviderConfig(personenrichment.ProviderExa)
			test.mutate(&provider)
			require.ErrorContains(t, provider.Validate(), test.want)
		})
	}

	sixtyfour := validProviderConfig(personenrichment.ProviderSixtyfour)
	sixtyfour.Tier = ""
	requirements.ErrorContains(sixtyfour.Validate(), "tier")
	sixtyfour = validProviderConfig(personenrichment.ProviderSixtyfour)
	sixtyfour.PollEndpoint = ""
	requirements.ErrorContains(sixtyfour.Validate(), "poll_endpoint")
	sixtyfour = validProviderConfig(personenrichment.ProviderSixtyfour)
	sixtyfour.Mode = "people"
	requirements.ErrorContains(sixtyfour.Validate(), "mode")
	sixtyfour = validProviderConfig(personenrichment.ProviderSixtyfour)
	sixtyfour.AllowedIdentifiers = []personenrichment.IdentifierClass{personenrichment.IdentifierPublicProfileURL}
	requirements.ErrorContains(sixtyfour.Validate(), "allowed_identifiers")
	sixtyfour = validProviderConfig(personenrichment.ProviderSixtyfour)
	sixtyfour.AllowedIdentifiers = []personenrichment.IdentifierClass{personenrichment.IdentifierName}
	requirements.ErrorContains(sixtyfour.Validate(), "allowed_identifiers")
	sixtyfour = validProviderConfig(personenrichment.ProviderSixtyfour)
	sixtyfour.AllowedIdentifiers = []personenrichment.IdentifierClass{personenrichment.IdentifierEmail}
	requirements.ErrorContains(sixtyfour.Validate(), "allowed_identifiers")
	sixtyfour = validProviderConfig(personenrichment.ProviderSixtyfour)
	sixtyfour.AllowedIdentifiers = []personenrichment.IdentifierClass{personenrichment.IdentifierPhone}
	requirements.ErrorContains(sixtyfour.Validate(), "allowed_identifiers")
	sixtyfour = validProviderConfig(personenrichment.ProviderSixtyfour)
	sixtyfour.AllowedIdentifiers = []personenrichment.IdentifierClass{
		personenrichment.IdentifierName, personenrichment.IdentifierCurrentCompany,
	}
	requirements.NoError(sixtyfour.Validate())
}

func TestConfigRequiresUniqueEnabledProviderNames(t *testing.T) {
	provider := validProviderConfig(personenrichment.ProviderExa)
	cfg := personenrichment.Config{
		Enabled:   true,
		Providers: []personenrichment.ProviderConfig{provider, provider},
	}
	cfg.ApplyDefaults()
	require.ErrorContains(t, cfg.Validate(), "duplicate provider name")
}

func validProviderConfig(kind string) personenrichment.ProviderConfig {
	provider := personenrichment.ProviderConfig{
		Name:               kind,
		Kind:               kind,
		Enabled:            true,
		APIKeyEnv:          "PROVIDER_API_KEY",
		AllowedIdentifiers: []personenrichment.IdentifierClass{personenrichment.IdentifierName},
		TargetKeys:         []string{"attribute:location"},
		RetentionPosture:   "zero_retention",
		TrainingPosture:    "no_training",
		RefreshInterval:    24 * time.Hour,
		RequestTimeout:     time.Minute,
		PollInterval:       30 * time.Second,
		MaxJobAge:          15 * time.Minute,
		MaxRetries:         5,
		MaxRequestsPerRun:  10,
		MaxRequestsPerDay:  100,
	}
	switch kind {
	case personenrichment.ProviderExa:
		provider.Endpoint = "https://api.example.test/search"
		provider.Mode = "people"
		provider.NumResults = 1
	case personenrichment.ProviderSixtyfour:
		provider.AllowedIdentifiers = []personenrichment.IdentifierClass{
			personenrichment.IdentifierName, personenrichment.IdentifierCurrentCompany,
		}
		provider.Endpoint = "https://api.example.test/start"
		provider.PollEndpoint = "https://api.example.test/poll"
		provider.Tier = "standard"
	}
	return provider
}
