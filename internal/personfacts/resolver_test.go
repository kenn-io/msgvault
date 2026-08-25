package personfacts

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var resolverTestNow = time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)

func TestResolverV1DefaultPolicyAndScoring(t *testing.T) {
	assertions := assert.New(t)

	policy := DefaultPolicyV1()
	assertions.Equal(ResolverVersionV1, policy.Version)
	assertions.Equal(750, policy.ApplyThreshold)
	assertions.Equal(100, policy.ReplacementMargin)
	assertions.Equal(75, policy.HysteresisMargin)
	assertions.Equal(900, policy.MinimumIdentityScore)
	assertions.Equal(150, policy.MaxCorroborationBonus)
	assertions.Equal(map[EvidenceSourceClass]int{
		EvidenceArchive: 300, EvidencePublic: 250, EvidenceSystem: 500,
		EvidenceProviderAssertion: 150,
	}, policy.SourceClassWeights)
	assertions.Equal(map[EvidenceDirectness]int{
		DirectSelf: 400, DirectOther: 200, Indirect: 0,
	}, policy.DirectnessWeights)
	assertions.Equal(map[EvidenceAuthority]int{
		AuthorityAuthoritative: 300, AuthorityOrdinary: 100, AuthorityAggregator: 0,
	}, policy.AuthorityWeights)

	t.Run("recent direct self beats authoritative public evidence", func(t *testing.T) {
		assert := assert.New(t)

		input := resolverTestInput(resolverTestSingleTarget())
		input.Claims = []ResolvedClaim{
			resolverTestClaim("authoritative", "public", 0,
				resolverTestEvidence("public", EvidencePublic, DirectOther, AuthorityAuthoritative, 0)),
			resolverTestClaim("direct-self", "self", 800,
				resolverTestEvidence("self", EvidenceArchive, DirectSelf, AuthorityOrdinary, 0)),
		}

		resolution := resolverTestResolve(t, input)
		direct := resolverTestDecision(t, resolution, "direct-self", DecisionApplied)
		assert.Equal(ScoreBreakdown{
			SourceClass: 300, Directness: 400, Authority: 100,
			Confidence: 80, Freshness: 100, Total: 980,
		}, direct.Score)
		authoritative := resolverTestDecision(t, resolution, "authoritative", DecisionSuperseded)
		assert.Equal(850, authoritative.Score.Total)
		assert.Equal("direct-self", authoritative.CompetingClaimKey)
		require.Len(t, resolution.Projections, 1)
		assert.Equal("direct-self", resolution.Projections[0].ClaimKey)
	})

	t.Run("authoritative employment applies", func(t *testing.T) {
		assert := assert.New(t)

		input := resolverTestInput(resolverTestMultiTarget())
		input.Claims = []ResolvedClaim{resolverTestClaim("employment", "engineer", 900,
			resolverTestEvidence("company", EvidencePublic, DirectOther, AuthorityAuthoritative, 0))}

		resolution := resolverTestResolve(t, input)
		decision := resolverTestDecision(t, resolution, "employment", DecisionApplied)
		assert.Equal(940, decision.Score.Total)
		require.Len(t, resolution.Projections, 1)
		assert.Equal(ProjectionSet, resolution.Projections[0].Operation)
	})

	t.Run("future evidence receives no freshness bonus", func(t *testing.T) {
		assert := assert.New(t)

		input := resolverTestInput(resolverTestSingleTarget())
		input.Claims = []ResolvedClaim{resolverTestClaim("future", "value", 1000,
			resolverTestEvidence("future", EvidencePublic, DirectOther,
				AuthorityOrdinary, -365*24*time.Hour))}

		resolution := resolverTestResolve(t, input)
		decision := resolverTestDecision(t, resolution, "future", DecisionRetained)
		assert.Equal(ReasonBelowThreshold, decision.Reason)
		assert.Equal(0, decision.Score.Freshness)
		assert.Equal(650, decision.Score.Total)
		assert.Empty(resolution.Projections)
	})

	for _, test := range []struct {
		name      string
		evidence  Evidence
		wantScore int
	}{
		{
			name: "weak aggregator remains below threshold",
			evidence: resolverTestEvidence("directory", EvidencePublic, Indirect,
				AuthorityAggregator, 0),
			wantScore: 450,
		},
		{
			name: "uncited provider assertion cannot apply itself",
			evidence: resolverTestEvidence("provider", EvidenceProviderAssertion, Indirect,
				AuthorityOrdinary, 0),
			wantScore: 450,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)

			input := resolverTestInput(resolverTestSingleTarget())
			input.Claims = []ResolvedClaim{resolverTestClaim("claim", "value", 1000, test.evidence)}
			resolution := resolverTestResolve(t, input)
			decision := resolverTestDecision(t, resolution, "claim", DecisionRetained)
			assert.Equal(ReasonBelowThreshold, decision.Reason)
			assert.Equal(test.wantScore, decision.Score.Total)
			assert.Empty(resolution.Projections)
		})
	}
}

func TestResolverV1InputFingerprintUsesPortableTimestampPrecision(t *testing.T) {
	input := resolverTestInput(resolverTestSingleTarget())
	input.ResolvedAt = time.Date(2025, time.May, 3, 4, 5, 6, 123456111, time.UTC)
	evidence := resolverTestEvidence("portable-time", EvidencePublic, DirectOther,
		AuthorityAuthoritative, 0)
	evidence.Input.EventTime = time.Date(2025, time.May, 1, 2, 3, 4, 234567222, time.UTC)
	evidence.Input.RecordedTime = time.Date(2025, time.May, 2, 3, 4, 5, 345678333, time.UTC)
	claim := resolverTestClaim("portable-time", "value", 900, evidence)
	validFrom := time.Date(2024, time.May, 1, 0, 0, 0, 456789444, time.UTC)
	validUntil := time.Date(2026, time.May, 1, 0, 0, 0, 567891555, time.UTC)
	claim.Claim.ValidFrom, claim.Claim.ValidUntil = &validFrom, &validUntil
	input.Claims = []ResolvedClaim{claim}
	current := resolverTestCurrent("portable-current", false, 42)
	current.ActiveFrom = time.Date(2024, time.May, 2, 0, 0, 0, 678912666, time.UTC)
	activeUntil := time.Date(2026, time.May, 2, 0, 0, 0, 789123777, time.UTC)
	current.ActiveUntil = &activeUntil
	current.TransactionTime = time.Date(2025, time.May, 2, 0, 0, 0, 891234888, time.UTC)
	input.Current = []CurrentProjection{current}

	first := resolverTestResolve(t, input)
	input.ResolvedAt = input.ResolvedAt.Add(10 * time.Nanosecond)
	input.Claims[0].Evidence[0].Input.EventTime = input.Claims[0].Evidence[0].Input.EventTime.Add(10 * time.Nanosecond)
	input.Claims[0].Evidence[0].Input.RecordedTime = input.Claims[0].Evidence[0].Input.RecordedTime.Add(10 * time.Nanosecond)
	*input.Claims[0].Claim.ValidFrom = input.Claims[0].Claim.ValidFrom.Add(10 * time.Nanosecond)
	*input.Claims[0].Claim.ValidUntil = input.Claims[0].Claim.ValidUntil.Add(10 * time.Nanosecond)
	input.Current[0].ActiveFrom = input.Current[0].ActiveFrom.Add(10 * time.Nanosecond)
	*input.Current[0].ActiveUntil = input.Current[0].ActiveUntil.Add(10 * time.Nanosecond)
	input.Current[0].TransactionTime = input.Current[0].TransactionTime.Add(10 * time.Nanosecond)
	second := resolverTestResolve(t, input)
	assert.Equal(t, first.InputFingerprint, second.InputFingerprint)
}

func TestResolverV1InputFingerprintPreservesLegacyPolicyEncoding(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	encoded, err := json.Marshal(DefaultPolicyV1())
	requirements.NoError(err)
	assertions.True(bytes.Equal([]byte(`{"Version":"person-fact-resolver-v1","ApplyThreshold":750,"ReplacementMargin":100,"HysteresisMargin":75,"MinimumIdentityScore":900,"MaxCorroborationBonus":150,"SourceClassWeights":{"archive":300,"provider_assertion":150,"public":250,"system":500},"DirectnessWeights":{"direct-other":200,"direct-self":400,"indirect":0},"AuthorityWeights":{"aggregator":0,"authoritative":300,"ordinary":100}}`), encoded),
		"resolver policy encoding changed")

	resolution := resolverTestResolve(t, legacyFingerprintResolutionInput())
	assertions.Equal("sha256:d399cb5ff33c2adf563fc4934cb2d2f7d3bb64dd5246c9d051006c30bfa912d7",
		resolution.InputFingerprint)
}

func TestResolverV1UsesFixedFreshnessTime(t *testing.T) {
	tests := []struct {
		name string
		age  time.Duration
		want int
	}{
		{name: "thirty days", age: 30 * 24 * time.Hour, want: 100},
		{name: "after thirty days", age: 30*24*time.Hour + time.Nanosecond, want: 60},
		{name: "one hundred eighty days", age: 180 * 24 * time.Hour, want: 60},
		{name: "after one hundred eighty days", age: 180*24*time.Hour + time.Nanosecond, want: 20},
		{name: "seven hundred thirty days", age: 730 * 24 * time.Hour, want: 20},
		{name: "after seven hundred thirty days", age: 730*24*time.Hour + time.Nanosecond, want: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := resolverTestInput(resolverTestMultiTarget())
			evidence := resolverTestEvidence("system", EvidenceSystem, DirectSelf, AuthorityOrdinary, test.age)
			evidence.Input.RecordedTime = input.ResolvedAt.Add(365 * 24 * time.Hour)
			input.Claims = []ResolvedClaim{resolverTestClaim("claim", "value", 1000, evidence)}

			decision := resolverTestDecision(t, resolverTestResolve(t, input), "claim", DecisionApplied)
			assert.Equal(t, test.want, decision.Score.Freshness)
		})
	}
}

func TestResolverV1CorroborationUsesIndependentSources(t *testing.T) {
	first := resolverTestEvidence("first-span", EvidenceArchive, DirectSelf, AuthorityOrdinary, 0)
	first.Input.SourceRef = "message-1"
	secondSpan := resolverTestEvidence("second-span", EvidenceArchive, DirectSelf, AuthorityOrdinary, 0)
	secondSpan.Input.SourceRef = "message-1"
	independent := resolverTestEvidence("second-message", EvidenceArchive, DirectSelf, AuthorityOrdinary, 0)
	independent.Input.SourceRef = "message-2"

	input := resolverTestInput(resolverTestMultiTarget())
	input.Claims = []ResolvedClaim{resolverTestClaim("duplicate-source", "one", 0, first, secondSpan)}
	decision := resolverTestDecision(t, resolverTestResolve(t, input), "duplicate-source", DecisionApplied)
	assert.Equal(t, 0, decision.Score.Corroboration)

	input.Claims[0].Evidence = append(input.Claims[0].Evidence, independent)
	decision = resolverTestDecision(t, resolverTestResolve(t, input), "duplicate-source", DecisionApplied)
	assert.Equal(t, 50, decision.Score.Corroboration)

	for index := 3; index <= 7; index++ {
		extra := resolverTestEvidence("source-"+string(rune('0'+index)), EvidenceArchive,
			DirectSelf, AuthorityOrdinary, 0)
		extra.Input.SourceRef = "message-" + string(rune('0'+index))
		input.Claims[0].Evidence = append(input.Claims[0].Evidence, extra)
	}
	decision = resolverTestDecision(t, resolverTestResolve(t, input), "duplicate-source", DecisionApplied)
	assert.Equal(t, 150, decision.Score.Corroboration)
}

func TestResolverV1PinAndExplicitUnpin(t *testing.T) {
	target := resolverTestSingleTarget()
	current := resolverTestCurrent("old", true, 11)
	claim := resolverTestClaim("new", "new", 1000,
		resolverTestEvidence("new", EvidenceArchive, DirectSelf, AuthorityOrdinary, 0))

	t.Run("declared current is initially pinned", func(t *testing.T) {
		input := resolverTestInput(target)
		input.Current = []CurrentProjection{current}
		input.Claims = []ResolvedClaim{claim}
		resolution := resolverTestResolve(t, input)
		decision := resolverTestDecision(t, resolution, "new", DecisionRetained)
		assert.Equal(t, ReasonPinRetained, decision.Reason)
		assert.Empty(t, resolution.Projections)
	})

	t.Run("explicit pin short circuits projection", func(t *testing.T) {
		input := resolverTestInput(target)
		input.Current = []CurrentProjection{resolverTestCurrent("old", false, 11)}
		input.Claims = []ResolvedClaim{claim}
		eventID := int64(21)
		input.Pin = PinState{Target: resolverTestTargetRef(target), Pinned: true, Actor: "user", EventID: &eventID}
		resolution := resolverTestResolve(t, input)
		decision := resolverTestDecision(t, resolution, "new", DecisionRetained)
		assert.Equal(t, ReasonPinRetained, decision.Reason)
		assert.Empty(t, resolution.Projections)
	})

	t.Run("explicit unpin overrides declared bootstrap pin", func(t *testing.T) {
		input := resolverTestInput(target)
		input.Current = []CurrentProjection{current}
		input.Claims = []ResolvedClaim{claim}
		eventID := int64(22)
		input.Pin = PinState{Target: resolverTestTargetRef(target), Pinned: false, Actor: "user", EventID: &eventID}
		resolution := resolverTestResolve(t, input)
		resolverTestDecision(t, resolution, "new", DecisionApplied)
		require.Len(t, resolution.Projections, 1)
		assert.Equal(t, current.Ref, *resolution.Projections[0].CurrentRef)
	})
}

func TestResolverV1SingleValueThresholdTieAndMargins(t *testing.T) {
	t.Run("exact threshold applies and one point below is retained", func(t *testing.T) {
		for _, test := range []struct {
			name       string
			confidence int
			action     DecisionAction
			reason     DecisionReason
		}{
			{name: "threshold", confidence: 1000, action: DecisionApplied, reason: ReasonAppliedProjection},
			{name: "below", confidence: 990, action: DecisionRetained, reason: ReasonBelowThreshold},
		} {
			t.Run(test.name, func(t *testing.T) {
				assert := assert.New(t)

				input := resolverTestInput(resolverTestSingleTarget())
				input.Claims = []ResolvedClaim{resolverTestClaim("claim", "value", test.confidence,
					resolverTestEvidence("claim", EvidencePublic, DirectOther, AuthorityOrdinary, 0))}
				resolution := resolverTestResolve(t, input)
				decision := resolverTestDecision(t, resolution, "claim", test.action)
				assert.Equal(test.reason, decision.Reason)
				assert.Equal(650+test.confidence/10, decision.Score.Total)
			})
		}
	})

	t.Run("equal winners are ambiguous", func(t *testing.T) {
		assert := assert.New(t)

		input := resolverTestInput(resolverTestSingleTarget())
		input.Claims = []ResolvedClaim{
			resolverTestClaim("alpha", "alpha", 1000,
				resolverTestEvidence("alpha", EvidenceArchive, DirectSelf, AuthorityOrdinary, 0)),
			resolverTestClaim("beta", "beta", 1000,
				resolverTestEvidence("beta", EvidenceArchive, DirectSelf, AuthorityOrdinary, 0)),
		}
		resolution := resolverTestResolve(t, input)
		alpha := resolverTestDecision(t, resolution, "alpha", DecisionAmbiguousRetained)
		beta := resolverTestDecision(t, resolution, "beta", DecisionAmbiguousRetained)
		assert.Equal(ReasonCompetingTie, alpha.Reason)
		assert.Equal("beta", alpha.CompetingClaimKey)
		assert.Equal("alpha", beta.CompetingClaimKey)
		assert.Empty(resolution.Projections)
	})

	t.Run("winner must clear replacement margin over runner up", func(t *testing.T) {
		assert := assert.New(t)

		input := resolverTestInput(resolverTestSingleTarget())
		input.Claims = []ResolvedClaim{
			resolverTestClaim("winner", "winner", 800,
				resolverTestEvidence("winner", EvidenceArchive, DirectSelf, AuthorityOrdinary, 0)),
			resolverTestClaim("runner", "runner", 400,
				resolverTestEvidence("runner", EvidencePublic, DirectOther, AuthorityAuthoritative, 0)),
		}
		resolution := resolverTestResolve(t, input)
		decision := resolverTestDecision(t, resolution, "winner", DecisionRetained)
		assert.Equal(980, decision.Score.Total)
		assert.Equal(ReasonInsufficientMargin, decision.Reason)
		assert.Equal("runner", decision.CompetingClaimKey)
		assert.Empty(resolution.Projections)

		input.Claims[1].Claim.Confidence.ReportedScore = 300
		resolution = resolverTestResolve(t, input)
		resolverTestDecision(t, resolution, "winner", DecisionApplied)
		require.Len(t, resolution.Projections, 1)
	})

	t.Run("different current requires replacement and hysteresis margins", func(t *testing.T) {
		for _, test := range []struct {
			name       string
			confidence int
			action     DecisionAction
		}{
			{name: "one point short", confidence: 740, action: DecisionRetained},
			{name: "exact combined margin", confidence: 750, action: DecisionApplied},
		} {
			t.Run(test.name, func(t *testing.T) {
				assert := assert.New(t)

				input := resolverTestInput(resolverTestSingleTarget())
				input.Current = []CurrentProjection{resolverTestCurrent("old", false, 31)}
				input.Claims = []ResolvedClaim{resolverTestClaim("new", "new", test.confidence,
					resolverTestEvidence("new", EvidencePublic, DirectSelf, AuthorityOrdinary, 0))}
				resolution := resolverTestResolve(t, input)
				decision := resolverTestDecision(t, resolution, "new", test.action)
				assert.Equal(850+test.confidence/10, decision.Score.Total)
				if test.action == DecisionRetained {
					assert.Equal(ReasonInsufficientMargin, decision.Reason)
					assert.Empty(resolution.Projections)
				} else {
					require.Len(t, resolution.Projections, 1)
				}
			})
		}
	})
}

func TestResolverV1MultiValueAddsAndExplicitRetirements(t *testing.T) {
	target := resolverTestMultiTarget()
	input := resolverTestInput(target)
	input.Current = []CurrentProjection{
		resolverTestCurrent("alpha", false, 41),
		resolverTestCurrent("beta", false, 42),
	}
	input.Claims = []ResolvedClaim{
		resolverTestClaim("zeta-add", "zeta", 1000,
			resolverTestEvidence("zeta", EvidenceArchive, DirectSelf, AuthorityOrdinary, 0)),
		resolverTestClaim("alpha-existing", "alpha", 1000,
			resolverTestEvidence("alpha", EvidenceArchive, DirectSelf, AuthorityOrdinary, 0)),
		resolverTestClaimWithRelation("beta-retire", "beta", RelationContradict, 1000,
			resolverTestEvidence("beta-retire", EvidenceArchive, DirectSelf, AuthorityOrdinary, 0)),
		resolverTestClaimWithRelation("omega-add", "omega", RelationSupport, 1000,
			resolverTestEvidence("omega", EvidenceArchive, DirectSelf, AuthorityOrdinary, 0)),
	}

	resolution := resolverTestResolve(t, input)
	resolverTestDecision(t, resolution, "alpha-existing", DecisionApplied)
	resolverTestDecision(t, resolution, "beta-retire", DecisionApplied)
	assert.Equal(t, []ProjectionPlan{
		{
			Operation: ProjectionRetire, Target: resolverTestTargetRef(target), ClaimKey: "beta-retire",
			CurrentRef: new(input.Current[1].Ref), ActiveFrom: resolverTestNow,
		},
		{Operation: ProjectionSet, Target: resolverTestTargetRef(target), ClaimKey: "omega-add", ActiveFrom: resolverTestNow},
		{Operation: ProjectionSet, Target: resolverTestTargetRef(target), ClaimKey: "zeta-add", ActiveFrom: resolverTestNow},
	}, resolution.Projections)
}

func TestResolverV1MultiValueNeverRetiresByOmission(t *testing.T) {
	assert := assert.New(t)

	target := resolverTestMultiTarget()
	input := resolverTestInput(target)
	input.Current = []CurrentProjection{
		resolverTestCurrent("alpha", false, 51),
		resolverTestCurrent("beta", false, 52),
	}
	input.Claims = []ResolvedClaim{resolverTestClaim("alpha", "alpha", 1000,
		resolverTestEvidence("alpha", EvidenceArchive, DirectSelf, AuthorityOrdinary, 0))}

	resolution := resolverTestResolve(t, input)
	assert.Empty(resolution.Projections)

	weak := resolverTestClaimWithRelation("weak-retire", "beta", RelationSupersede, 0,
		resolverTestEvidence("weak-retire", EvidencePublic, Indirect, AuthorityAggregator, 0))
	input.Claims = append(input.Claims, weak)
	resolution = resolverTestResolve(t, input)
	decision := resolverTestDecision(t, resolution, "weak-retire", DecisionRetained)
	assert.Equal(ReasonBelowThreshold, decision.Reason)
	assert.Empty(resolution.Projections)

	input.Claims[1] = resolverTestClaimWithRelation("strong-retire", "beta", RelationSupersede, 1000,
		resolverTestEvidence("strong-retire", EvidenceArchive, DirectSelf, AuthorityOrdinary, 0))
	resolution = resolverTestResolve(t, input)
	decision = resolverTestDecision(t, resolution, "strong-retire", DecisionApplied)
	assert.Equal(ReasonExplicitSupersession, decision.Reason)
	require.Len(t, resolution.Projections, 1)
	assert.Equal(ProjectionRetire, resolution.Projections[0].Operation)
}

func TestResolverV1MultiValueCompetesDirectionsPerFingerprint(t *testing.T) {
	strongEvidence := func(key string) Evidence {
		return resolverTestEvidence(key, EvidenceArchive, DirectSelf, AuthorityOrdinary, 0)
	}
	weakEvidence := func(key string) Evidence {
		return resolverTestEvidence(key, EvidencePublic, Indirect, AuthorityAggregator, 0)
	}

	for _, current := range []bool{false, true} {
		state := "absent"
		if current {
			state = "current"
		}
		t.Run("equal directions are ambiguous when "+state, func(t *testing.T) {
			input := resolverTestInput(resolverTestMultiTarget())
			if current {
				input.Current = []CurrentProjection{resolverTestCurrent("same", false, 61)}
			}
			input.Claims = []ResolvedClaim{
				resolverTestClaim("support", "same", 800, strongEvidence("support")),
				resolverTestClaimWithRelation(
					"negative", "same", RelationContradict, 800, strongEvidence("negative")),
			}

			resolution := resolverTestResolve(t, input)
			support := resolverTestDecision(t, resolution, "support", DecisionAmbiguousRetained)
			negative := resolverTestDecision(t, resolution, "negative", DecisionAmbiguousRetained)
			assert.Equal(t, ReasonCompetingTie, support.Reason)
			assert.Equal(t, ReasonCompetingTie, negative.Reason)
			assert.Empty(t, resolution.Projections)
		})

		t.Run("support winner is state independent when "+state, func(t *testing.T) {
			input := resolverTestInput(resolverTestMultiTarget())
			if current {
				input.Current = []CurrentProjection{resolverTestCurrent("same", false, 62)}
			}
			input.Claims = []ResolvedClaim{
				resolverTestClaim("support", "same", 1000, strongEvidence("support")),
				resolverTestClaimWithRelation(
					"negative", "same", RelationContradict, 0, weakEvidence("negative")),
			}

			resolution := resolverTestResolve(t, input)
			resolverTestDecision(t, resolution, "support", DecisionApplied)
			resolverTestDecision(t, resolution, "negative", DecisionSuperseded)
			if current {
				assert.Empty(t, resolution.Projections)
			} else {
				require.Len(t, resolution.Projections, 1)
				assert.Equal(t, ProjectionSet, resolution.Projections[0].Operation)
			}
		})

		t.Run("negative winner is state independent when "+state, func(t *testing.T) {
			input := resolverTestInput(resolverTestMultiTarget())
			if current {
				input.Current = []CurrentProjection{resolverTestCurrent("same", false, 63)}
			}
			input.Claims = []ResolvedClaim{
				resolverTestClaim("support", "same", 0, weakEvidence("support")),
				resolverTestClaimWithRelation(
					"negative", "same", RelationSupersede, 1000, strongEvidence("negative")),
			}

			resolution := resolverTestResolve(t, input)
			resolverTestDecision(t, resolution, "negative", DecisionApplied)
			resolverTestDecision(t, resolution, "support", DecisionSuperseded)
			if current {
				require.Len(t, resolution.Projections, 1)
				assert.Equal(t, ProjectionRetire, resolution.Projections[0].Operation)
			} else {
				assert.Empty(t, resolution.Projections)
			}
		})
	}
}

func TestResolverV1MultiValueAggregatesDirectionsAndKeepsFingerprintsIndependent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	input := resolverTestInput(resolverTestMultiTarget())
	input.Current = []CurrentProjection{resolverTestCurrent("retire", false, 64)}
	input.Claims = []ResolvedClaim{
		resolverTestClaim("support-first", "add", 500,
			resolverTestEvidence("support-first", EvidencePublic, Indirect, AuthorityAuthoritative, 0)),
		resolverTestClaim("support-second", "add", 500,
			resolverTestEvidence("support-second", EvidencePublic, Indirect, AuthorityAuthoritative, 0)),
		resolverTestClaimWithRelation("add-negative", "add", RelationContradict, 0,
			resolverTestEvidence("add-negative", EvidencePublic, Indirect, AuthorityAuthoritative, 0)),
		resolverTestClaimWithRelation("retire", "retire", RelationSupersede, 1000,
			resolverTestEvidence("retire", EvidenceArchive, DirectSelf, AuthorityOrdinary, 0)),
	}

	resolution := resolverTestResolve(t, input)
	first := resolverTestDecision(t, resolution, "support-first", DecisionApplied)
	second := resolverTestDecision(t, resolution, "support-second", DecisionSuperseded)
	assert.Equal(50, first.Score.Corroboration)
	assert.Equal(750, first.Score.Total)
	assert.Equal(first.Score, second.Score)
	resolverTestDecision(t, resolution, "add-negative", DecisionSuperseded)
	resolverTestDecision(t, resolution, "retire", DecisionApplied)
	require.Len(resolution.Projections, 2)
	assert.ElementsMatch([]ProjectionOperation{ProjectionSet, ProjectionRetire}, []ProjectionOperation{
		resolution.Projections[0].Operation, resolution.Projections[1].Operation,
	})
}

func TestResolverV1ValidityWindowUsesInclusiveStartAndExclusiveEnd(t *testing.T) {
	tests := []struct {
		name       string
		validFrom  *time.Time
		validUntil *time.Time
		action     DecisionAction
		reason     DecisionReason
	}{
		{
			name: "future start is outside validity", validFrom: new(resolverTestNow.Add(time.Microsecond)),
			action: DecisionRetained, reason: ReasonOutsideValidity,
		},
		{
			name: "exact start is eligible", validFrom: new(resolverTestNow),
			action: DecisionApplied, reason: ReasonAppliedProjection,
		},
		{
			name: "exact end is expired", validUntil: new(resolverTestNow),
			action: DecisionRetained, reason: ReasonOutsideValidity,
		},
		{
			name: "past end is expired", validUntil: new(resolverTestNow.Add(-time.Microsecond)),
			action: DecisionRetained, reason: ReasonOutsideValidity,
		},
		{
			name: "before end is eligible", validUntil: new(resolverTestNow.Add(time.Microsecond)),
			action: DecisionApplied, reason: ReasonAppliedProjection,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := resolverTestInput(resolverTestMultiTarget())
			claim := resolverTestClaim("claim", "value", 1000,
				resolverTestEvidence("claim", EvidenceArchive, DirectSelf, AuthorityOrdinary, 0))
			claim.Claim.ValidFrom = test.validFrom
			claim.Claim.ValidUntil = test.validUntil
			input.Claims = []ResolvedClaim{claim}

			resolution := resolverTestResolve(t, input)
			decision := resolverTestDecision(t, resolution, "claim", test.action)
			assert.Equal(t, test.reason, decision.Reason)
			if test.action == DecisionRetained {
				assert.Empty(t, resolution.Projections)
			} else {
				require.Len(t, resolution.Projections, 1)
			}
		})
	}
}

func TestResolverV1AllowsOnlyRejectionOnlyUnsupportedTargets(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	target := resolverTestSingleTarget()
	target.Kind = TargetKind("candidate")
	target.Key = "system:candidate"
	target.UniversalID = target.Key
	claim := resolverTestClaim("unsupported", "candidate", 1000)
	claim.Claim.Target = target
	claim.Normalized = nil
	claim.Failure = &ValidationFailure{
		Action: DecisionInvalid, Reason: ReasonUnsupportedTarget, Detail: "unsupported target",
	}
	input := resolverTestInput(target)
	input.Pin = PinState{}
	input.Claims = []ResolvedClaim{claim}
	resolver, err := NewResolver(DefaultPolicyV1())
	require.NoError(err)

	resolution, err := resolver.Resolve(input)
	require.NoError(err)
	require.Len(resolution.Decisions, 1)
	assert.Equal(DecisionInvalid, resolution.Decisions[0].Action)
	assert.Equal(ReasonUnsupportedTarget, resolution.Decisions[0].Reason)
	assert.Empty(resolution.Projections)

	for _, mutate := range []func(*ResolutionInput){
		func(value *ResolutionInput) { value.Current = []CurrentProjection{{}} },
		func(value *ResolutionInput) { value.Pin = PinState{Pinned: true} },
		func(value *ResolutionInput) { value.Claims[0].Failure = nil },
		func(value *ResolutionInput) { value.Claims[0].Failure.Reason = ReasonMalformedValue },
	} {
		invalid := copyResolutionInput(input)
		mutate(&invalid)
		_, err := resolver.Resolve(invalid)
		require.ErrorContains(err, "unsupported resolution target kind")
	}
}

func TestResolverV1RejectsInvalidIdentityAndSensitiveClaims(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ResolutionInput)
		action DecisionAction
		reason DecisionReason
	}{
		{
			name: "missing subject",
			mutate: func(input *ResolutionInput) {
				input.Claims[0].Evidence[0].Input.SubjectPersonID = nil
			},
			action: DecisionIdentityRejected, reason: ReasonIdentityMismatch,
		},
		{
			name: "wrong subject",
			mutate: func(input *ResolutionInput) {
				input.Claims[0].Evidence[0].Input.SubjectPersonID = new(int64(99))
			},
			action: DecisionIdentityRejected, reason: ReasonIdentityMismatch,
		},
		{
			name: "identity below policy minimum",
			mutate: func(input *ResolutionInput) {
				input.Claims[0].Evidence[0].Input.IdentityScore = 899
			},
			action: DecisionIdentityRejected, reason: ReasonIdentityMismatch,
		},
		{
			name: "sensitive policy",
			mutate: func(input *ResolutionInput) {
				input.Target.Sensitive = true
				input.Claims[0].Claim.Target.Sensitive = true
				input.Policy.AllowSensitive = false
			},
			action: DecisionPolicyRejected, reason: ReasonSensitivePolicy,
		},
		{
			name: "prepared invalid failure remains durable",
			mutate: func(input *ResolutionInput) {
				input.Claims[0].Claim.SubmittedValue = json.RawMessage(`{"broken"`)
				input.Claims[0].Normalized = nil
				input.Claims[0].Failure = &ValidationFailure{
					Action: DecisionInvalid, Reason: ReasonMalformedValue, Detail: "fixture failure",
				}
			},
			action: DecisionInvalid, reason: ReasonMalformedValue,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := resolverTestInput(resolverTestSingleTarget())
			input.Claims = []ResolvedClaim{resolverTestClaim("claim", "value", 1000,
				resolverTestEvidence("claim", EvidenceArchive, DirectSelf, AuthorityOrdinary, 0))}
			test.mutate(&input)
			resolution := resolverTestResolve(t, input)
			decision := resolverTestDecision(t, resolution, "claim", test.action)
			assert.Equal(t, test.reason, decision.Reason)
			assert.Empty(t, resolution.Projections)
		})
	}
}

func TestResolverV1EffectiveEvidenceSupport(t *testing.T) {
	baseEvidence := resolverTestEvidence("evidence", EvidencePublic, DirectOther, AuthorityOrdinary, 0)
	baseEvidence.Supported = false

	t.Run("insertion is supported without a status event", func(t *testing.T) {
		input := resolverTestInput(resolverTestSingleTarget())
		input.Claims = []ResolvedClaim{resolverTestClaim("claim", "value", 1000, baseEvidence)}
		resolution := resolverTestResolve(t, input)
		resolverTestDecision(t, resolution, "claim", DecisionApplied)
	})

	t.Run("currently unsupported evidence is excluded", func(t *testing.T) {
		input := resolverTestInput(resolverTestSingleTarget())
		evidence := baseEvidence
		evidence.LatestStatus = resolverTestStatus(evidence, 61, false, EvidenceStatusSourceDeleted)
		input.Claims = []ResolvedClaim{resolverTestClaim("claim", "value", 1000, evidence)}
		resolution := resolverTestResolve(t, input)
		decision := resolverTestDecision(t, resolution, "claim", DecisionRetained)
		assert.Equal(t, ReasonEvidenceUnsupported, decision.Reason)
		assert.Equal(t, ScoreBreakdown{}, decision.Score)
		assert.Empty(t, resolution.Projections)
	})

	t.Run("later event reactivates the immutable evidence version", func(t *testing.T) {
		input := resolverTestInput(resolverTestSingleTarget())
		older := baseEvidence
		older.LatestStatus = resolverTestStatus(older, 61, false, EvidenceStatusSourceDeleted)
		later := baseEvidence
		later.LatestStatus = resolverTestStatus(later, 62, true, EvidenceStatusSourceReimported)
		input.Claims = []ResolvedClaim{resolverTestClaim("claim", "value", 1000, older, later)}
		resolution := resolverTestResolve(t, input)
		decision := resolverTestDecision(t, resolution, "claim", DecisionApplied)
		assert.Equal(t, 750, decision.Score.Total)
	})
}

func TestResolverV1MateriallyNewEvidenceReconsidersClaim(t *testing.T) {
	assert := assert.New(t)

	input := resolverTestInput(resolverTestSingleTarget())
	first := resolverTestEvidence("first", EvidencePublic, Indirect, AuthorityAuthoritative, 0)
	input.Claims = []ResolvedClaim{resolverTestClaim("claim", "value", 500, first)}

	before := resolverTestResolve(t, input)
	decision := resolverTestDecision(t, before, "claim", DecisionRetained)
	assert.Equal(700, decision.Score.Total)

	second := resolverTestEvidence("second", EvidencePublic, Indirect, AuthorityAuthoritative, 0)
	input.Claims[0].Evidence = append(input.Claims[0].Evidence, second)
	after := resolverTestResolve(t, input)
	decision = resolverTestDecision(t, after, "claim", DecisionApplied)
	assert.Equal(50, decision.Score.Corroboration)
	assert.Equal(750, decision.Score.Total)
	assert.NotEqual(before.InputFingerprint, after.InputFingerprint)
}

func TestResolverV1FingerprintChangesWithEffectiveStatusAndProjectionContext(t *testing.T) {
	evidence := resolverTestEvidence("evidence", EvidencePublic, DirectOther, AuthorityOrdinary, 0)
	input := resolverTestInput(resolverTestSingleTarget())
	input.ProjectionContextFingerprint = `{"organization_matches":[{"id":2,"status":"active"}]}`
	evidence.LatestStatus = resolverTestStatus(evidence, 71, false, EvidenceStatusScopeUnlinked)
	input.Claims = []ResolvedClaim{resolverTestClaim("claim", "value", 1000, evidence)}
	unsupported := resolverTestResolve(t, input)

	input.Claims[0].Evidence[0].LatestStatus = resolverTestStatus(evidence, 72, true, EvidenceStatusScopeRelinked)
	reactivated := resolverTestResolve(t, input)
	assert.NotEqual(t, unsupported.InputFingerprint, reactivated.InputFingerprint)

	input.ProjectionContextFingerprint = `{"organization_matches":[{"id":3,"status":"active"}]}`
	changedProjectionContext := resolverTestResolve(t, input)
	assert.NotEqual(t, reactivated.InputFingerprint, changedProjectionContext.InputFingerprint)
}

func TestResolverV1ShuffledInputProducesByteIdenticalOutput(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	target := resolverTestMultiTarget()
	input := resolverTestInput(target)
	input.Current = []CurrentProjection{
		resolverTestCurrent("beta", false, 82),
		resolverTestCurrent("alpha", false, 81),
	}
	firstEvidence := resolverTestEvidence("one-a", EvidenceArchive, DirectSelf, AuthorityOrdinary, 0)
	secondEvidence := resolverTestEvidence("one-b", EvidenceArchive, DirectSelf, AuthorityOrdinary, 0)
	input.Claims = []ResolvedClaim{
		resolverTestClaim("zeta", "zeta", 900,
			resolverTestEvidence("zeta", EvidenceArchive, DirectSelf, AuthorityOrdinary, 0)),
		resolverTestClaim("one", "one", 900, firstEvidence, secondEvidence),
		resolverTestClaimWithRelation("beta-retire", "beta", RelationContradict, 900,
			resolverTestEvidence("beta-retire", EvidenceArchive, DirectSelf, AuthorityOrdinary, 0)),
	}

	first := resolverTestResolve(t, input)
	shuffled := copyResolutionInput(input)
	shuffled.Current[0], shuffled.Current[1] = shuffled.Current[1], shuffled.Current[0]
	shuffled.Claims[0], shuffled.Claims[2] = shuffled.Claims[2], shuffled.Claims[0]
	shuffled.Claims[1].Evidence[0], shuffled.Claims[1].Evidence[1] =
		shuffled.Claims[1].Evidence[1], shuffled.Claims[1].Evidence[0]
	second := resolverTestResolve(t, shuffled)

	firstJSON, err := json.Marshal(first)
	require.NoError(err)
	secondJSON, err := json.Marshal(second)
	require.NoError(err)
	assert.True(bytes.Equal(firstJSON, secondJSON), "shuffled output bytes differ")
	assert.NotNil(first.Decisions)
	assert.NotNil(first.Projections)
}

func TestResolverV1ValidatesClosedPolicyAndInputRanges(t *testing.T) {
	invalidPolicies := []func(*Policy){
		func(policy *Policy) { policy.Version = "" },
		func(policy *Policy) { policy.ApplyThreshold = -1 },
		func(policy *Policy) { policy.MinimumIdentityScore = 1001 },
		func(policy *Policy) { delete(policy.SourceClassWeights, EvidenceArchive) },
		func(policy *Policy) { policy.DirectnessWeights[EvidenceDirectness("unknown")] = 1 },
		func(policy *Policy) { policy.AuthorityWeights[AuthorityOrdinary] = -1 },
	}
	for index, mutate := range invalidPolicies {
		policy := DefaultPolicyV1()
		mutate(&policy)
		_, err := NewResolver(policy)
		require.Error(t, err, index)
	}

	resolver, err := NewResolver(DefaultPolicyV1())
	require.NoError(t, err)
	invalidInputs := []func(*ResolutionInput){
		func(input *ResolutionInput) { input.PersonID = 0 },
		func(input *ResolutionInput) { input.ResolvedAt = time.Time{} },
		func(input *ResolutionInput) { input.ResolvedAt = input.ResolvedAt.In(time.FixedZone("fixture", 3600)) },
		func(input *ResolutionInput) { input.Target.Cardinality = Cardinality("unknown") },
		func(input *ResolutionInput) { input.Target.Kind = TargetKind("unknown") },
		func(input *ResolutionInput) { input.Target.ValueType = ValueType("unknown") },
	}
	for index, mutate := range invalidInputs {
		input := resolverTestInput(resolverTestSingleTarget())
		mutate(&input)
		_, err := resolver.Resolve(input)
		require.Error(t, err, index)
	}
}

func TestResolverV1GroupsSameValueClaimsBeforeSingleValueCompetition(t *testing.T) {
	assert := assert.New(t)

	input := resolverTestInput(resolverTestSingleTarget())
	input.Claims = []ResolvedClaim{
		resolverTestClaim("same-direct", "same", 800,
			resolverTestEvidence("same-direct", EvidenceArchive, DirectSelf, AuthorityOrdinary, 0)),
		resolverTestClaim("same-authoritative", "same", 800,
			resolverTestEvidence("same-authoritative", EvidencePublic, DirectOther, AuthorityAuthoritative, 0)),
		resolverTestClaim("other", "other", 0,
			resolverTestEvidence("other", EvidencePublic, DirectOther, AuthorityAuthoritative, 0)),
	}

	resolution := resolverTestResolve(t, input)
	resolverTestDecision(t, resolution, "same-direct", DecisionApplied)
	resolverTestDecision(t, resolution, "same-authoritative", DecisionSuperseded)
	require.Len(t, resolution.Projections, 1)
	assert.Equal("same-direct", resolution.Projections[0].ClaimKey)

	input = resolverTestInput(resolverTestSingleTarget())
	input.Claims = []ResolvedClaim{
		resolverTestClaim("corroborated-first", "corroborated", 500,
			resolverTestEvidence("corroborated-first", EvidencePublic, Indirect, AuthorityAuthoritative, 0)),
		resolverTestClaim("corroborated-second", "corroborated", 500,
			resolverTestEvidence("corroborated-second", EvidencePublic, Indirect, AuthorityAuthoritative, 0)),
	}
	resolution = resolverTestResolve(t, input)
	decision := resolverTestDecision(t, resolution, "corroborated-first", DecisionApplied)
	assert.Equal(50, decision.Score.Corroboration)
	assert.Equal(750, decision.Score.Total)
	resolverTestDecision(t, resolution, "corroborated-second", DecisionSuperseded)
}

func TestResolverV1SingleValueSupportAndNegativeUseOneCompetitionPolicy(t *testing.T) {
	t.Run("negative cannot retire current below combined replacement and hysteresis margin", func(t *testing.T) {
		assert := assert.New(t)

		input := resolverTestInput(resolverTestSingleTarget())
		input.Current = []CurrentProjection{resolverTestCurrent("same", false, 101)}
		input.Claims = []ResolvedClaim{resolverTestClaimWithRelation(
			"negative", "same", RelationContradict, 740,
			resolverTestEvidence("negative", EvidencePublic, DirectSelf, AuthorityOrdinary, 0),
		)}

		resolution := resolverTestResolve(t, input)
		decision := resolverTestDecision(t, resolution, "negative", DecisionRetained)
		assert.Equal(924, decision.Score.Total)
		assert.Equal(ReasonExplicitContradiction, decision.Reason)
		assert.Empty(resolution.Projections)
	})

	t.Run("equal positive and negative evidence retains current as ambiguous", func(t *testing.T) {
		assert := assert.New(t)

		input := resolverTestInput(resolverTestSingleTarget())
		input.Current = []CurrentProjection{resolverTestCurrent("same", false, 102)}
		input.Claims = []ResolvedClaim{
			resolverTestClaim("support", "same", 800,
				resolverTestEvidence("support", EvidenceArchive, DirectSelf, AuthorityOrdinary, 0)),
			resolverTestClaimWithRelation("negative", "same", RelationContradict, 800,
				resolverTestEvidence("negative", EvidenceArchive, DirectSelf, AuthorityOrdinary, 0)),
		}

		resolution := resolverTestResolve(t, input)
		resolverTestDecision(t, resolution, "support", DecisionAmbiguousRetained)
		resolverTestDecision(t, resolution, "negative", DecisionAmbiguousRetained)
		assert.Empty(resolution.Projections)
	})

	t.Run("near negative evidence blocks positive projection", func(t *testing.T) {
		assert := assert.New(t)

		input := resolverTestInput(resolverTestSingleTarget())
		input.Claims = []ResolvedClaim{
			resolverTestClaim("support", "same", 800,
				resolverTestEvidence("support", EvidenceArchive, DirectSelf, AuthorityOrdinary, 0)),
			resolverTestClaimWithRelation("negative", "same", RelationSupersede, 400,
				resolverTestEvidence("negative", EvidencePublic, DirectSelf, AuthorityOrdinary, 0)),
		}

		resolution := resolverTestResolve(t, input)
		decision := resolverTestDecision(t, resolution, "support", DecisionRetained)
		assert.Equal(ReasonInsufficientMargin, decision.Reason)
		assert.Empty(resolution.Projections)
	})

	t.Run("strong negative retires current after clearing every margin", func(t *testing.T) {
		assert := assert.New(t)

		input := resolverTestInput(resolverTestSingleTarget())
		input.Current = []CurrentProjection{resolverTestCurrent("same", false, 103)}
		input.Claims = []ResolvedClaim{
			resolverTestClaim("support", "same", 0,
				resolverTestEvidence("support", EvidencePublic, DirectOther, AuthorityAuthoritative, 0)),
			resolverTestClaimWithRelation("negative", "same", RelationSupersede, 800,
				resolverTestEvidence("negative", EvidenceArchive, DirectSelf, AuthorityOrdinary, 0)),
		}

		resolution := resolverTestResolve(t, input)
		resolverTestDecision(t, resolution, "negative", DecisionApplied)
		resolverTestDecision(t, resolution, "support", DecisionSuperseded)
		require.Len(t, resolution.Projections, 1)
		assert.Equal(ProjectionRetire, resolution.Projections[0].Operation)
	})

	t.Run("corroborated negative claims aggregate before retirement", func(t *testing.T) {
		assert := assert.New(t)

		input := resolverTestInput(resolverTestSingleTarget())
		input.Current = []CurrentProjection{resolverTestCurrent("same", false, 105)}
		input.Claims = []ResolvedClaim{
			resolverTestClaimWithRelation("negative-first", "same", RelationContradict, 250,
				resolverTestEvidence("negative-first", EvidencePublic, DirectSelf, AuthorityOrdinary, 0)),
			resolverTestClaimWithRelation("negative-second", "same", RelationContradict, 250,
				resolverTestEvidence("negative-second", EvidencePublic, DirectSelf, AuthorityOrdinary, 0)),
		}

		resolution := resolverTestResolve(t, input)
		decision := resolverTestDecision(t, resolution, "negative-first", DecisionApplied)
		assert.Equal(50, decision.Score.Corroboration)
		assert.Equal(925, decision.Score.Total)
		resolverTestDecision(t, resolution, "negative-second", DecisionSuperseded)
		require.Len(t, resolution.Projections, 1)
		assert.Equal(ProjectionRetire, resolution.Projections[0].Operation)
	})

	t.Run("mixed negative relations aggregate with relation-correct reasons", func(t *testing.T) {
		assert := assert.New(t)

		input := resolverTestInput(resolverTestSingleTarget())
		input.Current = []CurrentProjection{resolverTestCurrent("same", false, 106)}
		input.Claims = []ResolvedClaim{
			resolverTestClaimWithRelation("negative-contradict", "same", RelationContradict, 250,
				resolverTestEvidence("negative-contradict", EvidencePublic, DirectSelf, AuthorityOrdinary, 0)),
			resolverTestClaimWithRelation("negative-supersede", "same", RelationSupersede, 250,
				resolverTestEvidence("negative-supersede", EvidencePublic, DirectSelf, AuthorityOrdinary, 0)),
		}

		resolution := resolverTestResolve(t, input)
		contradiction := resolverTestDecision(t, resolution, "negative-contradict", DecisionApplied)
		assert.Equal(ReasonExplicitContradiction, contradiction.Reason)
		assert.Equal(50, contradiction.Score.Corroboration)
		assert.Equal(925, contradiction.Score.Total)
		supersession := resolverTestDecision(t, resolution, "negative-supersede", DecisionSuperseded)
		assert.Equal(ReasonExplicitSupersession, supersession.Reason)
		require.Len(t, resolution.Projections, 1)
		assert.Equal(ProjectionRetire, resolution.Projections[0].Operation)
	})

	t.Run("positive winner preserves losing negative relation reasons", func(t *testing.T) {
		assert := assert.New(t)

		input := resolverTestInput(resolverTestSingleTarget())
		input.Claims = []ResolvedClaim{
			resolverTestClaim("positive", "positive", 0,
				resolverTestEvidence("positive", EvidenceSystem, DirectSelf, AuthorityAuthoritative, 0)),
			resolverTestClaimWithRelation("negative-contradict", "negative", RelationContradict, 250,
				resolverTestEvidence("negative-contradict", EvidencePublic, DirectSelf, AuthorityOrdinary, 0)),
			resolverTestClaimWithRelation("negative-supersede", "negative", RelationSupersede, 250,
				resolverTestEvidence("negative-supersede", EvidencePublic, DirectSelf, AuthorityOrdinary, 0)),
		}

		resolution := resolverTestResolve(t, input)
		resolverTestDecision(t, resolution, "positive", DecisionApplied)
		contradiction := resolverTestDecision(t, resolution, "negative-contradict", DecisionApplied)
		assert.Equal(ReasonExplicitContradiction, contradiction.Reason)
		assert.Equal(925, contradiction.Score.Total)
		supersession := resolverTestDecision(t, resolution, "negative-supersede", DecisionSuperseded)
		assert.Equal(ReasonExplicitSupersession, supersession.Reason)
		assert.Equal(925, supersession.Score.Total)
		require.Len(t, resolution.Projections, 1)
		assert.Equal("positive", resolution.Projections[0].ClaimKey)
	})

	t.Run("unrelated negative cannot suppress supported value", func(t *testing.T) {
		assert := assert.New(t)
		input := resolverTestInput(resolverTestSingleTarget())
		input.Claims = []ResolvedClaim{
			resolverTestClaim("support-a", "a", 0,
				resolverTestEvidence("support-a", EvidencePublic, DirectSelf, AuthorityOrdinary, 0)),
			resolverTestClaimWithRelation("negative-b", "b", RelationContradict, 1000,
				resolverTestEvidence("negative-b", EvidenceArchive, DirectSelf, AuthorityAuthoritative, 0)),
		}

		resolution := resolverTestResolve(t, input)
		resolverTestDecision(t, resolution, "support-a", DecisionApplied)
		negative := resolverTestDecision(t, resolution, "negative-b", DecisionApplied)
		assert.Equal(ReasonExplicitContradiction, negative.Reason)
		require.Len(t, resolution.Projections, 1)
		assert.Equal("support-a", resolution.Projections[0].ClaimKey)
		assert.Equal(ProjectionSet, resolution.Projections[0].Operation)
	})
}

func TestResolverV1FingerprintIncludesEvidenceStatusPersonID(t *testing.T) {
	input := resolverTestInput(resolverTestSingleTarget())
	evidence := resolverTestEvidence("evidence", EvidencePublic, DirectOther, AuthorityOrdinary, 0)
	input.Claims = []ResolvedClaim{resolverTestClaim("claim", "value", 1000, evidence)}
	canonical := resolverTestResolve(t, input)

	input.Claims[0].Evidence[0].PersonID = 8
	differentStatusIdentity := resolverTestResolve(t, input)
	assert.NotEqual(t, canonical.InputFingerprint, differentStatusIdentity.InputFingerprint)
}

func TestResolverV1CorroborationUsesRequiredRefWhenPresent(t *testing.T) {
	tests := []struct {
		name        string
		sourceClass EvidenceSourceClass
		action      DecisionAction
	}{
		{name: "archive", sourceClass: EvidenceArchive, action: DecisionApplied},
		{name: "provider assertion", sourceClass: EvidenceProviderAssertion, action: DecisionRetained},
		{name: "system", sourceClass: EvidenceSystem, action: DecisionApplied},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			first := resolverTestEvidence("first", test.sourceClass, Indirect, AuthorityOrdinary, 0)
			second := resolverTestEvidence("second", test.sourceClass, Indirect, AuthorityOrdinary, 0)
			first.Input.SourceRef = "same-required-ref"
			second.Input.SourceRef = "same-required-ref"
			first.Input.SourceURL = "https://example.com/first"
			second.Input.SourceURL = "https://example.com/second"
			if test.sourceClass == EvidenceArchive {
				first.Input.Directness = DirectSelf
				second.Input.Directness = DirectSelf
			}

			input := resolverTestInput(resolverTestMultiTarget())
			input.Claims = []ResolvedClaim{resolverTestClaim("claim", "value", 1000, first, second)}
			decision := resolverTestDecision(t, resolverTestResolve(t, input), "claim", test.action)
			assert.Zero(t, decision.Score.Corroboration)
		})
	}
}

func TestResolverV1RejectsMutatedFrozenV1Policy(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Policy)
	}{
		{name: "apply threshold", mutate: func(policy *Policy) { policy.ApplyThreshold++ }},
		{name: "replacement margin", mutate: func(policy *Policy) { policy.ReplacementMargin++ }},
		{name: "hysteresis margin", mutate: func(policy *Policy) { policy.HysteresisMargin++ }},
		{name: "minimum identity", mutate: func(policy *Policy) { policy.MinimumIdentityScore-- }},
		{name: "corroboration cap", mutate: func(policy *Policy) { policy.MaxCorroborationBonus++ }},
		{name: "archive weight", mutate: func(policy *Policy) { policy.SourceClassWeights[EvidenceArchive]++ }},
		{name: "public weight", mutate: func(policy *Policy) { policy.SourceClassWeights[EvidencePublic]++ }},
		{name: "system weight", mutate: func(policy *Policy) { policy.SourceClassWeights[EvidenceSystem]++ }},
		{name: "provider weight", mutate: func(policy *Policy) { policy.SourceClassWeights[EvidenceProviderAssertion]++ }},
		{name: "direct self weight", mutate: func(policy *Policy) { policy.DirectnessWeights[DirectSelf]++ }},
		{name: "direct other weight", mutate: func(policy *Policy) { policy.DirectnessWeights[DirectOther]++ }},
		{name: "indirect weight", mutate: func(policy *Policy) { policy.DirectnessWeights[Indirect]++ }},
		{name: "authoritative weight", mutate: func(policy *Policy) { policy.AuthorityWeights[AuthorityAuthoritative]++ }},
		{name: "ordinary weight", mutate: func(policy *Policy) { policy.AuthorityWeights[AuthorityOrdinary]++ }},
		{name: "aggregator weight", mutate: func(policy *Policy) { policy.AuthorityWeights[AuthorityAggregator]++ }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy := DefaultPolicyV1()
			test.mutate(&policy)
			_, err := NewResolver(policy)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "frozen")
		})
	}

	custom := DefaultPolicyV1()
	custom.Version = "person-fact-resolver-custom"
	custom.ApplyThreshold++
	_, err := NewResolver(custom)
	require.NoError(t, err)
}

func TestResolverV1RejectsInvalidPreparedFailurePairsWithoutPanic(t *testing.T) {
	tests := []ValidationFailure{
		{Action: DecisionApplied, Reason: ReasonAppliedProjection, Detail: "must reject"},
		{Action: DecisionRetained, Reason: ReasonBelowThreshold, Detail: "must reject"},
		{Action: DecisionIdentityRejected, Reason: ReasonMalformedValue, Detail: "must reject"},
		{Action: DecisionPolicyRejected, Reason: ReasonIdentityMismatch, Detail: "must reject"},
		{Action: DecisionInvalid, Reason: ReasonPinRetained, Detail: "must reject"},
	}
	for _, failure := range tests {
		t.Run(string(failure.Action)+"_"+string(failure.Reason), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			input := resolverTestInput(resolverTestSingleTarget())
			input.Current = []CurrentProjection{resolverTestCurrent("current", false, 104)}
			claim := resolverTestClaim("claim", "value", 1000,
				resolverTestEvidence("claim", EvidenceArchive, DirectSelf, AuthorityOrdinary, 0))
			claim.Normalized = nil
			claim.Failure = &failure
			input.Claims = []ResolvedClaim{claim}
			resolver, err := NewResolver(DefaultPolicyV1())
			require.NoError(err)

			assert.NotPanics(func() {
				_, err = resolver.Resolve(input)
			})
			require.Error(err)
			assert.Contains(err.Error(), "rejection")
		})
	}
}

func resolverTestInput(target TargetDescriptor) ResolutionInput {
	return ResolutionInput{
		PersonID: 7, Target: target, ResolvedAt: resolverTestNow,
		Policy:                       PolicyContext{AllowSensitive: true, ProviderPolicyFingerprint: "policy-context-v1"},
		ProjectionContextFingerprint: "projection-context-v1",
		Pin:                          PinState{Target: resolverTestTargetRef(target)},
	}
}

func resolverTestSingleTarget() TargetDescriptor {
	return TargetDescriptor{
		Kind: TargetAttribute, Key: "attribute:favorite", Revision: "revision-1",
		UniversalID: "attribute:favorite", Slug: "favorite", Description: "Fixture favorite",
		ValueType: ValueText, Cardinality: CardinalitySingle,
	}
}

func resolverTestMultiTarget() TargetDescriptor {
	target := resolverTestSingleTarget()
	target.Key = "attribute:aliases"
	target.UniversalID = target.Key
	target.Slug = "aliases"
	target.Cardinality = CardinalityMulti
	return target
}

func resolverTestClaim(key, value string, confidence int, evidence ...Evidence) ResolvedClaim {
	return resolverTestClaimWithRelation(key, value, RelationSupport, confidence, evidence...)
}

func resolverTestClaimWithRelation(
	key, value string, relation ClaimRelation, confidence int, evidence ...Evidence,
) ResolvedClaim {
	target := resolverTestSingleTarget()
	normalized := &NormalizedValue{JSON: json.RawMessage(`"` + value + `"`), Fingerprint: value}
	return ResolvedClaim{
		ClaimKey: key,
		Claim: ProposedClaim{
			Target: target, Relation: relation, SubmittedValue: append(json.RawMessage(nil), normalized.JSON...),
			Origin: OriginExtraction, Confidence: ConfidenceInputs{ReportedScore: confidence},
		},
		Normalized: normalized,
		Evidence:   append([]Evidence(nil), evidence...),
	}
}

func resolverTestEvidence(
	key string, sourceClass EvidenceSourceClass, directness EvidenceDirectness,
	authority EvidenceAuthority, age time.Duration,
) Evidence {
	subject := int64(7)
	start, end := int64(0), int64(8)
	input := EvidenceInput{
		PersonID: 7, SourceClass: sourceClass, Directness: directness, Authority: authority,
		SubjectPersonID: &subject, SubjectRef: "person-7", Excerpt: "fixture evidence",
		SourceVersion: "v1", EventTime: resolverTestNow.Add(-age), RecordedTime: resolverTestNow,
		IdentityScore: 950,
	}
	switch sourceClass {
	case EvidenceArchive:
		input.SourceRef = key
		input.SpanStart = &start
		input.SpanEnd = &end
		input.ContentSHA256 = strings.Repeat("a", 64)
	case EvidencePublic:
		input.SourceURL = "https://example.com/" + key
	case EvidenceSystem:
		input.SourceRef = key
	case EvidenceProviderAssertion:
		input.SourceRef = key
	}
	return Evidence{ID: int64(len(key) + 1), PersonID: 7, Key: "evidence-" + key, Input: input, Supported: true}
}

func resolverTestStatus(
	evidence Evidence, id int64, supported bool, reason EvidenceStatusReason,
) *EvidenceStatusEvent {
	return &EvidenceStatusEvent{
		ID: id, PersonID: evidence.PersonID, EvidenceID: evidence.ID,
		EvidenceKey: evidence.Key, SourceVersion: evidence.Input.SourceVersion,
		Supported: supported, Reason: reason, CreatedAt: resolverTestNow,
	}
}

func resolverTestCurrent(fingerprint string, declared bool, rowID int64) CurrentProjection {
	return CurrentProjection{
		Ref:        ProjectionRef{Kind: "attribute", RowID: rowID},
		Normalized: NormalizedValue{JSON: json.RawMessage(`"` + fingerprint + `"`), Fingerprint: fingerprint},
		ActiveFrom: resolverTestNow.AddDate(-1, 0, 0), TransactionTime: resolverTestNow.Add(-time.Hour),
		Declared: declared,
	}
}

func resolverTestTargetRef(target TargetDescriptor) TargetRef {
	return TargetRef{Kind: target.Kind, Key: target.Key, Revision: target.Revision}
}

func resolverTestResolve(t *testing.T, input ResolutionInput) Resolution {
	t.Helper()
	for index := range input.Claims {
		input.Claims[index].Claim.Target = input.Target
	}
	resolver, err := NewResolver(DefaultPolicyV1())
	require.NoError(t, err)
	resolution, err := resolver.Resolve(input)
	require.NoError(t, err)
	return resolution
}

func resolverTestDecision(
	t *testing.T, resolution Resolution, claimKey string, action DecisionAction,
) Decision {
	t.Helper()
	for _, decision := range resolution.Decisions {
		if decision.ClaimKey == claimKey && decision.Action == action {
			return decision
		}
	}
	require.FailNow(t, "decision not found", "claim=%s action=%s decisions=%+v", claimKey, action, resolution.Decisions)
	return Decision{}
}
