package personfacts

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func resolverV2TestResolve(t *testing.T, input ResolutionInput) Resolution {
	t.Helper()
	for i := range input.Claims {
		input.Claims[i].Claim.Target = input.Target
	}
	resolver, err := NewResolver(DefaultPolicyV2())
	require.NoError(t, err)
	result, err := resolver.Resolve(input)
	require.NoError(t, err)
	return result
}

func TestResolverV1ReplayIgnoresStability(t *testing.T) {
	var baseline []byte
	for _, stability := range []Stability{"", StabilityFixed, StabilityVolatile} {
		input := legacyFingerprintResolutionInput()
		input.Stability = stability
		result := resolverTestResolve(t, input)
		assert.Equal(t, "sha256:d399cb5ff33c2adf563fc4934cb2d2f7d3bb64dd5246c9d051006c30bfa912d7", result.InputFingerprint)
		encoded, err := json.Marshal(result)
		require.NoError(t, err)
		if baseline == nil {
			baseline = encoded
		} else {
			assert.Equal(t, baseline, encoded)
		}
	}
}

func TestResolverV2DefaultPolicyIsFrozen(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	policy := DefaultPolicyV2()
	profile := policy.Stability[StabilityVolatile]
	profile.StaleAfterDays++
	policy.Stability[StabilityVolatile] = profile
	_, err := NewResolver(policy)
	require.Error(err)
	policy = DefaultPolicyV1()
	policy.Stability = DefaultPolicyV2().Stability
	_, err = NewResolver(policy)
	require.Error(err)
	policy.Stability = map[Stability]StabilityProfile{}
	_, err = NewResolver(policy)
	require.Error(err)
	policy = DefaultPolicyV2()
	resolver, err := NewResolver(policy)
	require.NoError(err)
	policy.Stability[StabilityFixed].Freshness[0].Points++
	assert.Equal(100, resolver.Policy.Stability[StabilityFixed].Freshness[0].Points)
	for _, tc := range []struct {
		stability Stability
		days      int
		ok        bool
	}{
		{StabilityFixed, 0, false}, {StabilityDurable, 730, true}, {StabilityVolatile, 180, true},
	} {
		duration, ok := DefaultPolicyV2().StaleAfter(tc.stability)
		assert.Equal(time.Duration(tc.days)*24*time.Hour, duration)
		assert.Equal(tc.ok, ok)
	}
	_, ok := DefaultPolicyV1().StaleAfter(StabilityVolatile)
	assert.False(ok)
}

func TestResolverV2FreshnessByStability(t *testing.T) {
	day := 24 * time.Hour
	for _, tc := range []struct {
		name      string
		stability Stability
		age       time.Duration
		want      int
	}{
		{"volatile future", StabilityVolatile, -400 * day, 0},
		{"durable future", StabilityDurable, -400 * day, 0},
		{"fixed future", StabilityFixed, -400 * day, 0},
		{"volatile thirty", StabilityVolatile, 30 * day, 100},
		{"volatile after thirty", StabilityVolatile, 30*day + time.Nanosecond, 40},
		{"volatile ninety", StabilityVolatile, 90 * day, 40},
		{"volatile after ninety", StabilityVolatile, 90*day + time.Nanosecond, 10},
		{"volatile one eighty", StabilityVolatile, 180 * day, 10},
		{"fixed ancient", StabilityFixed, 3000 * day, 100},
		{"durable thirty", StabilityDurable, 30 * day, 100},
		{"durable after thirty", StabilityDurable, 30*day + time.Nanosecond, 60},
		{"durable one eighty", StabilityDurable, 180 * day, 60},
		{"durable after one eighty", StabilityDurable, 180*day + time.Nanosecond, 20},
		{"durable seven thirty", StabilityDurable, 730 * day, 20},
		{"durable after seven thirty", StabilityDurable, 730*day + time.Nanosecond, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := resolverTestInput(resolverTestMultiTarget())
			input.Stability = tc.stability
			evidence := resolverTestEvidence("system", EvidenceSystem, DirectSelf, AuthorityOrdinary, tc.age)
			evidence.Input.RecordedTime = input.ResolvedAt.Add(365 * day)
			input.Claims = []ResolvedClaim{resolverTestClaim("claim", "value", 1000, evidence)}
			decision := resolverTestDecision(t, resolverV2TestResolve(t, input), "claim", DecisionApplied)
			assert.Equal(t, tc.want, decision.Score.Freshness)
		})
	}
}

func TestResolverV2FixedNeedsBonusMargin(t *testing.T) {
	input := resolverTestInput(resolverTestSingleTarget())
	input.Current = []CurrentProjection{resolverTestCurrent("Porto", false, 11)}
	input.Claims = []ResolvedClaim{
		resolverTestClaim("Lisbon", "Lisbon", 500, resolverTestEvidence("Lisbon", EvidencePublic, DirectSelf, AuthorityAuthoritative, 24*time.Hour)),
		resolverTestClaim("Porto", "Porto", 1000, resolverTestEvidence("Porto", EvidencePublic, DirectSelf, AuthorityOrdinary, 24*time.Hour)),
	}
	input.Stability = StabilityDurable
	durable := resolverTestDecision(t, resolverV2TestResolve(t, input), "Lisbon", DecisionApplied)
	assert.Equal(t, 1100, durable.Score.Total)
	input.Stability = StabilityFixed
	fixed := resolverTestDecision(t, resolverV2TestResolve(t, input), "Lisbon", DecisionRetained)
	assert.Equal(t, ReasonInsufficientMargin, fixed.Reason)
}

func TestResolverV2FixedResistsNewerEqualClaim(t *testing.T) {
	input := resolverTestInput(resolverTestSingleTarget())
	input.Current = []CurrentProjection{resolverTestCurrent("Porto", false, 11)}
	input.Claims = []ResolvedClaim{
		resolverTestClaim("Porto", "Porto", 1000, resolverTestEvidence("Porto", EvidencePublic, DirectSelf, AuthorityOrdinary, 8*365*24*time.Hour)),
		resolverTestClaim("Lisbon", "Lisbon", 1000, resolverTestEvidence("Lisbon", EvidencePublic, DirectSelf, AuthorityOrdinary, 24*time.Hour)),
	}
	resolverTestDecision(t, resolverTestResolve(t, input), "Lisbon", DecisionApplied)
	input.Stability = StabilityFixed
	resolution := resolverV2TestResolve(t, input)
	decision := resolverTestDecision(t, resolution, "Lisbon", DecisionAmbiguousRetained)
	assert.Equal(t, ReasonCompetingTie, decision.Reason)
	assert.Empty(t, resolution.Projections)
}

func TestResolverV2VolatileCeiling(t *testing.T) {
	for _, tc := range []struct {
		age    int
		action DecisionAction
	}{{181, DecisionRetained}, {179, DecisionApplied}} {
		input := resolverTestInput(resolverTestMultiTarget())
		input.Stability = StabilityVolatile
		input.Claims = []ResolvedClaim{resolverTestClaim("claim", "value", 1000, resolverTestEvidence("claim", EvidenceArchive, DirectSelf, AuthorityOrdinary, time.Duration(tc.age)*24*time.Hour))}
		resolution := resolverV2TestResolve(t, input)
		decision := resolverTestDecision(t, resolution, "claim", tc.action)
		assert.Equal(t, tc.age == 181, decision.Score.StabilityWindow)
		if tc.age == 181 {
			assert.Equal(t, ReasonBelowThreshold, decision.Reason)
			assert.Empty(t, resolution.Projections)
		}
	}
}

func TestResolverV2PinBeatsVolatileCeiling(t *testing.T) {
	input := resolverTestInput(resolverTestMultiTarget())
	input.Stability = StabilityVolatile
	input.Pin.Pinned = true
	input.Claims = []ResolvedClaim{resolverTestClaim("claim", "value", 1000, resolverTestEvidence("claim", EvidenceArchive, DirectSelf, AuthorityOrdinary, 200*24*time.Hour))}
	decision := resolverTestDecision(t, resolverV2TestResolve(t, input), "claim", DecisionRetained)
	assert.Equal(t, ReasonPinRetained, decision.Reason)
	assert.False(t, decision.Score.StabilityWindow)
}

func TestResolverV2FingerprintIncludesStability(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	input := legacyFingerprintResolutionInput()
	input.Stability = StabilityDurable
	durableV1, durableV2 := resolverTestResolve(t, input), resolverV2TestResolve(t, input)
	input.Stability = StabilityVolatile
	volatileV1, volatileV2 := resolverTestResolve(t, input), resolverV2TestResolve(t, input)
	assert.Equal(durableV1.InputFingerprint, volatileV1.InputFingerprint)
	assert.NotEqual(durableV2.InputFingerprint, volatileV2.InputFingerprint)
	input.Stability = "weekly"
	resolver, err := NewResolver(DefaultPolicyV2())
	require.NoError(err)
	_, err = resolver.Resolve(input)
	require.Error(err)
}

func TestResolverV2DurableMatchesV1Decisions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	inputs := []ResolutionInput{}
	future := resolverTestInput(resolverTestMultiTarget())
	future.Claims = []ResolvedClaim{resolverTestClaim("future", "value", 1000,
		resolverTestEvidence("future", EvidenceSystem, DirectSelf, AuthorityOrdinary, -400*24*time.Hour))}
	inputs = append(inputs, future)
	for _, confidence := range []int{1000, 990} {
		input := resolverTestInput(resolverTestSingleTarget())
		input.Claims = []ResolvedClaim{resolverTestClaim("claim", "value", confidence, resolverTestEvidence("claim", EvidencePublic, DirectOther, AuthorityOrdinary, 0))}
		inputs = append(inputs, input)
	}
	tied := resolverTestInput(resolverTestSingleTarget())
	tied.Claims = []ResolvedClaim{
		resolverTestClaim("alpha", "alpha", 1000, resolverTestEvidence("alpha", EvidenceArchive, DirectSelf, AuthorityOrdinary, 0)),
		resolverTestClaim("beta", "beta", 1000, resolverTestEvidence("beta", EvidenceArchive, DirectSelf, AuthorityOrdinary, 0)),
	}
	inputs = append(inputs, tied)
	for _, confidence := range []int{400, 300} {
		input := resolverTestInput(resolverTestSingleTarget())
		input.Claims = []ResolvedClaim{
			resolverTestClaim("winner", "winner", 800, resolverTestEvidence("winner", EvidenceArchive, DirectSelf, AuthorityOrdinary, 0)),
			resolverTestClaim("runner", "runner", confidence, resolverTestEvidence("runner", EvidencePublic, DirectOther, AuthorityAuthoritative, 0)),
		}
		inputs = append(inputs, input)
	}
	for _, confidence := range []int{740, 750} {
		input := resolverTestInput(resolverTestSingleTarget())
		input.Current = []CurrentProjection{resolverTestCurrent("old", false, 31)}
		input.Claims = []ResolvedClaim{resolverTestClaim("new", "new", confidence, resolverTestEvidence("new", EvidencePublic, DirectSelf, AuthorityOrdinary, 0))}
		inputs = append(inputs, input)
	}
	for _, input := range inputs {
		input.Stability = StabilityDurable
		old, fresh := resolverTestResolve(t, input), resolverV2TestResolve(t, input)
		require.Len(fresh.Decisions, len(old.Decisions))
		for i, want := range old.Decisions {
			got := fresh.Decisions[i]
			assert.Equal(want.Action, got.Action)
			assert.Equal(want.Reason, got.Reason)
			assert.Equal(want.Score, got.Score)
		}
	}
}

func TestResolverCustomPolicyWithoutStabilityUsesLegacyFreshness(t *testing.T) {
	policy := DefaultPolicyV1()
	policy.Version = "custom-policy"
	resolver, err := NewResolver(policy)
	require.NoError(t, err)
	input := resolverTestInput(resolverTestMultiTarget())
	input.Claims = []ResolvedClaim{resolverTestClaim("claim", "value", 1000,
		resolverTestEvidence("claim", EvidenceSystem, DirectSelf, AuthorityOrdinary, 60*24*time.Hour))}
	input.Claims[0].Claim.Target = input.Target
	got, err := resolver.Resolve(input)
	require.NoError(t, err)
	decision := resolverTestDecision(t, got, "claim", DecisionApplied)
	assert.Equal(t, 60, decision.Score.Freshness)
}
