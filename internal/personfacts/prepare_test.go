package personfacts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerationKeyBindsProgramFingerprintClaimsEvidenceAndStatuses(t *testing.T) {
	assert := assert.New(t)

	base := validGenerationInput()
	base.Claims = []ProposedClaim{validClaim(validArchiveEvidence())}
	base.EvidenceStatusChanges = []EvidenceStatusChange{{
		EvidenceKey: "status-evidence", SourceVersion: "v1", Supported: false,
		Reason: EvidenceStatusSourceDeleted,
	}}

	basePrepared := mustPrepare(t, base)
	baseKey := basePrepared.GenerationKey()
	assert.NotEmpty(baseKey)

	programChanged := cloneGenerationInput(base)
	programChanged.ProgramFingerprint = strings.Repeat("1", 64)
	assert.NotEqual(baseKey, mustPrepare(t, programChanged).GenerationKey())

	claimChanged := cloneGenerationInput(base)
	claimChanged.Claims[0].SubmittedValue = json.RawMessage(`"different"`)
	assert.NotEqual(baseKey, mustPrepare(t, claimChanged).GenerationKey())

	evidenceChanged := cloneGenerationInput(base)
	evidenceChanged.Claims[0].Evidence[0].Excerpt = "different excerpt"
	assert.NotEqual(baseKey, mustPrepare(t, evidenceChanged).GenerationKey())

	statusChanged := cloneGenerationInput(base)
	statusChanged.EvidenceStatusChanges[0].Reason = EvidenceStatusSourceEdited
	assert.NotEqual(baseKey, mustPrepare(t, statusChanged).GenerationKey())

	resolvedAtChanged := cloneGenerationInput(base)
	resolvedAtChanged.ResolvedAt = base.ResolvedAt.Add(24 * time.Hour)
	assert.Equal(baseKey, mustPrepare(t, resolvedAtChanged).GenerationKey())
}

func TestPreparePersonFactGenerationSanitizesInvalidRelationAndOriginWithoutLosingIdentity(t *testing.T) {
	tests := []struct {
		name           string
		firstToken     string
		secondToken    string
		mutate         func(*ProposedClaim, string)
		assertSentinel func(*testing.T, PreparedClaim)
	}{
		{
			name: "relation", firstToken: "agrees", secondToken: "confirms",
			mutate: func(claim *ProposedClaim, token string) {
				claim.Relation = ClaimRelation(token)
			},
			assertSentinel: func(t *testing.T, claim PreparedClaim) {
				t.Helper()
				assert.Equal(t, RelationInvalid, claim.Relation)
				assert.Equal(t, OriginExtraction, claim.Origin)
			},
		},
		{
			name: "origin", firstToken: "crawler", secondToken: "importer",
			mutate: func(claim *ProposedClaim, token string) {
				claim.Origin = ClaimOrigin(token)
			},
			assertSentinel: func(t *testing.T, claim PreparedClaim) {
				t.Helper()
				assert.Equal(t, RelationSupport, claim.Relation)
				assert.Equal(t, OriginInvalid, claim.Origin)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			prepareWithToken := func(token string) PreparedGeneration {
				input := validGenerationInput()
				claim := validClaim()
				test.mutate(&claim, token)
				input.Claims = []ProposedClaim{claim}
				prepared, err := PreparePersonFactGeneration(t.Context(), input, nil)
				require.NoError(err)
				return prepared
			}

			first := prepareWithToken(test.firstToken)
			claim := first.Claims()[0]
			test.assertSentinel(t, claim)
			require.NotNil(claim.Failure)
			assert.Equal(DecisionInvalid, claim.Failure.Action)
			assert.Equal(ReasonMalformedValue, claim.Failure.Reason)
			assert.Contains(claim.Failure.Detail, test.firstToken)

			second := prepareWithToken(test.secondToken)
			assert.NotEqual(first.GenerationKey(), second.GenerationKey())
		})
	}
}

func TestPreparePersonFactGenerationPreservesValidationFailurePrecedenceOverDescriptorMismatch(t *testing.T) {
	tests := []struct {
		name           string
		mutate         func(*ProposedClaim)
		reason         DecisionReason
		wantNormalized bool
	}{
		{
			name: "missing target key",
			mutate: func(claim *ProposedClaim) {
				claim.Target.Key = ""
			},
			reason:         ReasonUnsupportedTarget,
			wantNormalized: true,
		},
		{
			name: "missing target revision",
			mutate: func(claim *ProposedClaim) {
				claim.Target.Revision = ""
			},
			reason:         ReasonUnsupportedTarget,
			wantNormalized: true,
		},
		{
			name: "malformed value with tampered descriptor",
			mutate: func(claim *ProposedClaim) {
				claim.Target.MaxLength = 1
				claim.SubmittedValue = json.RawMessage(`{"unterminated"`)
			},
			reason: ReasonMalformedValue,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			input := validGenerationInput()
			claim := validClaim()
			test.mutate(&claim)
			input.Claims = []ProposedClaim{claim}

			prepared, err := PreparePersonFactGeneration(t.Context(), input, nil)
			require.NoError(err)
			claims := prepared.Claims()
			require.Len(claims, 1)
			if test.wantNormalized {
				assert.NotNil(claims[0].Normalized)
			} else {
				assert.Nil(claims[0].Normalized)
			}
			require.NotNil(claims[0].Failure)
			assert.Equal(DecisionInvalid, claims[0].Failure.Action)
			assert.Equal(test.reason, claims[0].Failure.Reason)
		})
	}
}

func TestClaimKeyBindsGenerationAndCanonicalClaim(t *testing.T) {
	prepared := mustPrepare(t, generationWithTwoEvidence())
	claim := prepared.Claims()[0]
	base, err := ClaimKey(prepared.GenerationKey(), claim)
	require.NoError(t, err)

	mutations := []struct {
		name   string
		change func(*PreparedClaim)
	}{
		{name: "target key", change: func(v *PreparedClaim) { v.Target.Key = "different" }},
		{name: "target revision", change: func(v *PreparedClaim) { v.Target.Revision = "different" }},
		{name: "relation", change: func(v *PreparedClaim) { v.Relation = RelationContradict }},
		{name: "submitted", change: func(v *PreparedClaim) { v.SubmittedFingerprint = "different" }},
		{name: "submitted evidence", change: func(v *PreparedClaim) {
			v.SubmittedEvidenceFingerprints[0] = "different"
		}},
		{name: "normalized", change: func(v *PreparedClaim) { v.Normalized.Fingerprint = "different" }},
		{name: "valid from", change: func(v *PreparedClaim) { at := utcTime(2021, 1, 0); v.ValidFrom = &at }},
		{name: "valid until", change: func(v *PreparedClaim) { at := utcTime(2026, 1, 0); v.ValidUntil = &at }},
		{name: "origin", change: func(v *PreparedClaim) { v.Origin = OriginEnrichment }},
		{name: "confidence", change: func(v *PreparedClaim) { v.Confidence.ReportedScore++ }},
		{name: "evidence", change: func(v *PreparedClaim) { v.EvidenceKeys[0] = "different" }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			changed := clonePreparedClaim(claim)
			mutation.change(&changed)
			key, err := ClaimKey(prepared.GenerationKey(), changed)
			require.NoError(t, err)
			assert.NotEqual(t, base, key)
		})
	}

	otherGeneration, err := ClaimKey("sha256:"+strings.Repeat("f", 64), claim)
	require.NoError(t, err)
	assert.NotEqual(t, base, otherGeneration)
}

func TestDeterministicKeysIncludeEveryIdentityField(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	evidence := validArchiveEvidence()
	baseEvidenceKey, err := EvidenceKey(evidence)
	requirements.NoError(err)
	evidenceMutations := []struct {
		name   string
		change func(*EvidenceInput)
	}{
		{name: "person", change: func(v *EvidenceInput) { v.PersonID++; *v.SubjectPersonID = v.PersonID }},
		{name: "source class", change: func(v *EvidenceInput) { v.SourceClass = EvidenceSystem; v.SpanStart = nil; v.SpanEnd = nil }},
		{name: "directness", change: func(v *EvidenceInput) { v.Directness = Indirect }},
		{name: "authority", change: func(v *EvidenceInput) { v.Authority = AuthorityOrdinary }},
		{name: "source ref", change: func(v *EvidenceInput) { v.SourceRef = "message-2" }},
		{name: "source URL", change: func(v *EvidenceInput) { v.SourceURL = "https://example.com/evidence" }},
		{name: "subject ref", change: func(v *EvidenceInput) { v.SubjectRef = "participant-2" }},
		{name: "span", change: func(v *EvidenceInput) { *v.SpanEnd++ }},
		{name: "excerpt", change: func(v *EvidenceInput) { v.Excerpt = "different" }},
		{name: "content hash", change: func(v *EvidenceInput) { v.ContentSHA256 = strings.Repeat("b", 64) }},
		{name: "source version", change: func(v *EvidenceInput) { v.SourceVersion = "v2" }},
		{name: "event time", change: func(v *EvidenceInput) { v.EventTime = v.EventTime.Add(time.Hour) }},
		{name: "recorded time", change: func(v *EvidenceInput) { v.RecordedTime = v.RecordedTime.Add(time.Hour) }},
		{name: "identity score", change: func(v *EvidenceInput) { v.IdentityScore-- }},
	}
	for _, mutation := range evidenceMutations {
		t.Run("evidence "+mutation.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			changed := cloneEvidence(evidence)
			mutation.change(&changed)
			key, err := EvidenceKey(changed)
			require.NoError(err)
			assert.NotEqual(baseEvidenceKey, key)
		})
	}

	base := validGenerationInput()
	base.Claims = []ProposedClaim{validClaim(evidence)}
	baseKey := mustPrepare(t, base).GenerationKey()
	generationMutations := []struct {
		name   string
		change func(*GenerationInput)
	}{
		{name: "person", change: func(v *GenerationInput) {
			v.PersonID++
			v.Claims[0].Evidence[0].PersonID++
			*v.Claims[0].Evidence[0].SubjectPersonID = v.PersonID
		}},
		{name: "source cursor set", change: func(v *GenerationInput) {
			v.SourceCursors = append(v.SourceCursors, SourceCursor{Lane: "backstop", Start: "a", End: "b"})
		}},
		{name: "program ID", change: func(v *GenerationInput) { v.ProgramID += "-2" }},
		{name: "program version", change: func(v *GenerationInput) { v.ProgramVersion += "-2" }},
		{name: "program fingerprint", change: func(v *GenerationInput) { v.ProgramFingerprint = strings.Repeat("1", 64) }},
		{name: "catalog fingerprint", change: func(v *GenerationInput) { v.CatalogFingerprint += "-2" }},
		{name: "policy allow sensitive", change: func(v *GenerationInput) { v.Policy.AllowSensitive = !v.Policy.AllowSensitive }},
		{name: "policy fingerprint", change: func(v *GenerationInput) { v.Policy.ProviderPolicyFingerprint += "-2" }},
		{name: "provider", change: func(v *GenerationInput) { v.Provider += "-2" }},
		{name: "provider version", change: func(v *GenerationInput) { v.ProviderVersion += "-2" }},
		{name: "model", change: func(v *GenerationInput) { v.Model += "-2" }},
		{name: "model version", change: func(v *GenerationInput) { v.ModelVersion += "-2" }},
	}
	for _, mutation := range generationMutations {
		t.Run("generation "+mutation.name, func(t *testing.T) {
			assert := assert.New(t)

			changed := cloneGenerationInput(base)
			mutation.change(&changed)
			assert.NotEqual(baseKey, mustPrepare(t, changed).GenerationKey())
		})
	}

	decisionA, err := DecisionKey("resolution-a", "claim-a", DecisionApplied)
	requirements.NoError(err)
	for _, tuple := range []struct {
		resolution string
		claim      string
		action     DecisionAction
	}{{"resolution-b", "claim-a", DecisionApplied}, {"resolution-a", "claim-b", DecisionApplied}, {"resolution-a", "claim-a", DecisionRetained}} {
		key, err := DecisionKey(tuple.resolution, tuple.claim, tuple.action)
		requirements.NoError(err)
		assertions.NotEqual(decisionA, key)
	}

	resolution := ResolutionInput{
		PersonID: 7, Target: testAttributeTarget(ValueText), ResolvedAt: utcTime(2025, 4, 12),
		Policy:                       PolicyContext{AllowSensitive: false, ProviderPolicyFingerprint: "policy-v1"},
		ProjectionContextFingerprint: "projection-v1",
	}
	resolutionA, err := ResolutionInputFingerprint(resolution)
	requirements.NoError(err)
	resolution.ResolvedAt = resolution.ResolvedAt.Add(time.Second)
	resolutionB, err := ResolutionInputFingerprint(resolution)
	requirements.NoError(err)
	assertions.NotEqual(resolutionA, resolutionB)
}

func TestDeterministicKeysIgnoreClaimEvidenceAndStatusOrder(t *testing.T) {
	first := generationWithTwoEvidence()
	second := cloneGenerationInput(first)
	second.SourceCursors[0], second.SourceCursors[1] = second.SourceCursors[1], second.SourceCursors[0]
	second.Claims[0], second.Claims[1] = second.Claims[1], second.Claims[0]
	second.Claims[0].Evidence[0], second.Claims[0].Evidence[1] = second.Claims[0].Evidence[1], second.Claims[0].Evidence[0]
	second.EvidenceStatusChanges[0], second.EvidenceStatusChanges[1] = second.EvidenceStatusChanges[1], second.EvidenceStatusChanges[0]

	preparedFirst := mustPrepare(t, first)
	preparedSecond := mustPrepare(t, second)
	assert.Equal(t, preparedFirst.GenerationKey(), preparedSecond.GenerationKey())
	assert.Equal(t, preparedFirst.CanonicalJSON(), preparedSecond.CanonicalJSON())
	assert.Equal(t, preparedFirst.Claims()[0].EvidenceKeys, preparedSecond.Claims()[0].EvidenceKeys)
}

func TestPreparePersonFactGenerationDeepCopiesCanonicalInput(t *testing.T) {
	assert := assert.New(t)

	input := generationWithTwoEvidence()
	prepared := mustPrepare(t, input)
	wantJSON := prepared.CanonicalJSON()
	wantKey := prepared.GenerationKey()
	wantInput := prepared.Input()
	wantClaims := prepared.Claims()
	wantStatuses := prepared.EvidenceStatusChanges()

	input.SourceCursors[0].Lane = "mutated"
	input.Claims[0].SubmittedValue[0] = 'x'
	input.Claims[0].Evidence[0].Excerpt = "mutated"
	*input.Claims[0].Evidence[0].SubjectPersonID = 99
	*input.Claims[0].ValidFrom = input.Claims[0].ValidFrom.Add(time.Hour)
	input.EvidenceStatusChanges[0].EvidenceKey = "mutated"
	assert.True(bytes.Equal(wantJSON, prepared.CanonicalJSON()), "canonical JSON changed")
	assert.Equal(wantKey, prepared.GenerationKey())
	assert.Equal(wantInput, prepared.Input())
	assert.Equal(wantClaims, prepared.Claims())
	assert.Equal(wantStatuses, prepared.EvidenceStatusChanges())

	gotInput := prepared.Input()
	gotInput.SourceCursors[0].Lane = "accessor-mutated"
	gotInput.Claims[0].SubmittedValue[0] = 'y'
	gotInput.Claims[0].Evidence[0].Excerpt = "accessor-mutated"
	*gotInput.Claims[0].Evidence[0].SubjectPersonID = 100
	gotClaims := prepared.Claims()
	gotClaims[0].SubmittedValue[0] = 'z'
	gotClaims[0].EvidenceKeys[0] = "accessor-mutated"
	gotStatuses := prepared.EvidenceStatusChanges()
	gotStatuses[0].EvidenceKey = "accessor-mutated"
	gotJSON := prepared.CanonicalJSON()
	gotJSON[0] = 'x'

	assert.Equal(wantInput, prepared.Input())
	assert.Equal(wantClaims, prepared.Claims())
	assert.Equal(wantStatuses, prepared.EvidenceStatusChanges())
	assert.True(bytes.Equal(wantJSON, prepared.CanonicalJSON()), "canonical JSON changed")
}

func TestPreparePersonFactGenerationCanonicalizesPortableTimestampPrecision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	input := validGenerationInput()
	input.ResolvedAt = time.Date(2025, time.January, 4, 12, 0, 0, 123456789, time.UTC)
	evidence := validArchiveEvidence()
	evidence.EventTime = time.Date(2025, time.January, 1, 12, 0, 0, 234567891, time.UTC)
	evidence.RecordedTime = time.Date(2025, time.January, 2, 12, 0, 0, 345678912, time.UTC)
	claim := validClaim(evidence)
	validFrom := time.Date(2020, time.January, 1, 0, 0, 0, 456789123, time.UTC)
	validUntil := time.Date(2021, time.January, 1, 0, 0, 0, 567891234, time.UTC)
	claim.ValidFrom, claim.ValidUntil = &validFrom, &validUntil
	input.Claims = []ProposedClaim{claim}

	first := mustPrepare(t, input)
	firstInput := first.Input()
	firstClaim := first.Claims()[0]
	assert.Equal(input.ResolvedAt.Truncate(time.Microsecond), firstInput.ResolvedAt)
	assert.Equal(evidence.EventTime.Truncate(time.Microsecond), firstClaim.Evidence[0].EventTime)
	assert.Equal(evidence.RecordedTime.Truncate(time.Microsecond), firstClaim.Evidence[0].RecordedTime)
	assert.Equal(validFrom.Truncate(time.Microsecond), *firstClaim.ValidFrom)
	assert.Equal(validUntil.Truncate(time.Microsecond), *firstClaim.ValidUntil)

	replayInput := cloneGenerationInput(input)
	replayInput.ResolvedAt = replayInput.ResolvedAt.Add(10 * time.Nanosecond)
	replayInput.Claims[0].Evidence[0].EventTime = replayInput.Claims[0].Evidence[0].EventTime.Add(10 * time.Nanosecond)
	replayInput.Claims[0].Evidence[0].RecordedTime = replayInput.Claims[0].Evidence[0].RecordedTime.Add(10 * time.Nanosecond)
	*replayInput.Claims[0].ValidFrom = replayInput.Claims[0].ValidFrom.Add(10 * time.Nanosecond)
	*replayInput.Claims[0].ValidUntil = replayInput.Claims[0].ValidUntil.Add(10 * time.Nanosecond)
	second := mustPrepare(t, replayInput)
	assert.Equal(first.GenerationKey(), second.GenerationKey())
	assert.Equal(first.CanonicalJSON(), second.CanonicalJSON())
	firstClaimKey, err := ClaimKey(first.GenerationKey(), firstClaim)
	require.NoError(err)
	secondClaimKey, err := ClaimKey(second.GenerationKey(), second.Claims()[0])
	require.NoError(err)
	assert.Equal(firstClaimKey, secondClaimKey)
	assert.Equal(firstClaim.EvidenceKeys, second.Claims()[0].EvidenceKeys)
}

func TestPreparePersonFactGenerationRejectsDuplicateEvidenceKeys(t *testing.T) {
	assert := assert.New(t)

	input := validGenerationInput()
	evidence := validArchiveEvidence()
	input.Claims = []ProposedClaim{validClaim(evidence, cloneEvidence(evidence))}

	prepared, err := PreparePersonFactGeneration(t.Context(), input, acceptingAligner())
	require.ErrorContains(t, err, "duplicate evidence key")
	assert.Empty(prepared.GenerationKey())
	assert.Nil(prepared.CanonicalJSON())
	assert.Empty(prepared.Claims())
}

func TestPreparePersonFactGenerationRejectsDuplicateCanonicalClaims(t *testing.T) {
	assert := assert.New(t)

	input := validGenerationInput()
	claim := validClaim()
	input.Claims = []ProposedClaim{claim, claim}

	prepared, err := PreparePersonFactGeneration(t.Context(), input, nil)
	require.ErrorContains(t, err, "duplicate canonical claim")
	assert.Empty(prepared.GenerationKey())
	assert.Nil(prepared.CanonicalJSON())
	assert.Empty(prepared.Claims())
}

func TestResolutionInputFingerprintCanonicalizesPortableTimestampPrecision(t *testing.T) {
	build := func(delta time.Duration) ResolutionInput {
		evidence := validArchiveEvidence()
		evidence.EventTime = time.Date(2025, time.April, 1, 2, 3, 4, 123456111, time.UTC).Add(delta)
		evidence.RecordedTime = time.Date(2025, time.April, 2, 3, 4, 5, 234567222, time.UTC).Add(delta)
		claim := validClaim(evidence)
		validFrom := time.Date(2024, time.April, 1, 0, 0, 0, 345678333, time.UTC).Add(delta)
		validUntil := time.Date(2026, time.April, 1, 0, 0, 0, 456789444, time.UTC).Add(delta)
		claim.ValidFrom, claim.ValidUntil = &validFrom, &validUntil
		activeUntil := time.Date(2026, time.April, 2, 0, 0, 0, 678912666, time.UTC).Add(delta)
		return ResolutionInput{
			PersonID:   7,
			Target:     testAttributeTarget(ValueText),
			ResolvedAt: time.Date(2025, time.April, 3, 4, 5, 6, 567891555, time.UTC).Add(delta),
			Current: []CurrentProjection{{
				Ref: ProjectionRef{Kind: "attribute", RowID: 42},
				Normalized: NormalizedValue{
					JSON: json.RawMessage(`"current"`), Fingerprint: "current",
				},
				ActiveFrom:  time.Date(2024, time.April, 2, 0, 0, 0, 789123777, time.UTC).Add(delta),
				ActiveUntil: &activeUntil,
				TransactionTime: time.Date(2025, time.April, 2, 0, 0, 0,
					891234888, time.UTC).Add(delta),
			}},
			Claims: []ResolvedClaim{{
				ClaimKey: "claim-key", Claim: claim,
				Evidence: []Evidence{{Key: "evidence-key", Input: evidence, Supported: true}},
			}},
		}
	}

	first, err := ResolutionInputFingerprint(build(0))
	require.NoError(t, err)
	second, err := ResolutionInputFingerprint(build(10 * time.Nanosecond))
	require.NoError(t, err)
	assert.Equal(t, first, second)
}

func TestResolutionInputFingerprintPreservesLegacyEncoding(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	input := legacyFingerprintResolutionInput()
	encoded, err := json.Marshal(input)
	requirements.NoError(err)
	assertions.True(bytes.Equal([]byte(`{"PersonID":7,"Target":{"kind":"attribute","key":"attribute:favorite","revision":"revision-1","universal_id":"attribute:favorite","slug":"favorite","description":"Fixture favorite","value_type":"text","cardinality":"single","choices":null,"fields":null,"sensitive":false},"ResolvedAt":"2026-08-22T12:00:00Z","Policy":{"allow_sensitive":true,"provider_policy_fingerprint":"policy-context-v1"},"ProjectionContextFingerprint":"projection-context-v1","Current":[{"Ref":{"kind":"attribute","row_id":44},"Normalized":{"JSON":"legacy-current","Fingerprint":"legacy-current"},"ActiveFrom":"2025-08-22T12:00:00Z","ActiveUntil":null,"TransactionTime":"2026-08-22T11:00:00Z","Declared":true}],"Claims":[{"ClaimKey":"legacy-claim","Claim":{"Target":{"kind":"attribute","key":"attribute:favorite","revision":"revision-1","universal_id":"attribute:favorite","slug":"favorite","description":"Fixture favorite","value_type":"text","cardinality":"single","choices":null,"fields":null,"sensitive":false},"Relation":"support","SubmittedValue":"legacy-value","Evidence":null,"ValidFrom":null,"ValidUntil":null,"Origin":"extraction","Confidence":{"reported_score":875}},"Normalized":{"JSON":"legacy-value","Fingerprint":"legacy-value"},"Evidence":[{"ID":7,"PersonID":7,"Key":"evidence-legacy","CreatedAt":"0001-01-01T00:00:00Z","Input":{"PersonID":7,"SourceClass":"archive","Directness":"direct-self","Authority":"ordinary","SourceRef":"legacy","SourceURL":"","SubjectPersonID":7,"SubjectRef":"person-7","SpanStart":0,"SpanEnd":8,"Excerpt":"fixture evidence","ContentSHA256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","SourceVersion":"v1","EventTime":"2026-08-22T12:00:00Z","RecordedTime":"2026-08-22T12:00:00Z","IdentityScore":950},"Supported":true,"LatestStatus":{"ID":91,"PersonID":7,"GenerationID":0,"EvidenceID":7,"EvidenceKey":"evidence-legacy","SourceVersion":"v1","Supported":false,"Reason":"source-edited","CreatedAt":"2026-08-22T12:00:00Z"}}],"Failure":null},{"ClaimKey":"legacy-failure","Claim":{"Target":{"kind":"attribute","key":"attribute:favorite","revision":"revision-1","universal_id":"attribute:favorite","slug":"favorite","description":"Fixture favorite","value_type":"text","cardinality":"single","choices":null,"fields":null,"sensitive":false},"Relation":"support","SubmittedValue":"broken","Evidence":null,"ValidFrom":null,"ValidUntil":null,"Origin":"extraction","Confidence":{"reported_score":400}},"Normalized":null,"Evidence":null,"Failure":{"Action":"invalid","Reason":"malformed-value","Detail":"legacy failure"}}],"Pin":{"target":{"kind":"attribute","key":"attribute:favorite","revision":"revision-1"},"pinned":true,"actor":"operator","event_id":73}}`), encoded),
		"resolution input encoding changed")

	digest, err := ResolutionInputFingerprint(input)
	requirements.NoError(err)
	assertions.Equal("sha256:2a2e78418caca2ffcb851dc66c3243df4fd19bc548986378b26fef357a0e8fb1", digest)
}

func legacyFingerprintResolutionInput() ResolutionInput {
	evidence := resolverTestEvidence("legacy", EvidenceArchive, DirectSelf, AuthorityOrdinary, 0)
	evidence.LatestStatus = resolverTestStatus(evidence, 91, false, EvidenceStatusSourceEdited)
	input := resolverTestInput(resolverTestSingleTarget())
	input.Current = []CurrentProjection{resolverTestCurrent("legacy-current", true, 44)}
	eventID := int64(73)
	input.Pin = PinState{
		Target: resolverTestTargetRef(input.Target), Pinned: true, Actor: "operator", EventID: &eventID,
	}
	input.Claims = []ResolvedClaim{
		resolverTestClaim("legacy-claim", "legacy-value", 875, evidence),
		{
			ClaimKey: "legacy-failure",
			Claim: ProposedClaim{
				Target: input.Target, Relation: RelationSupport, SubmittedValue: json.RawMessage(`"broken"`),
				Origin: OriginExtraction, Confidence: ConfidenceInputs{ReportedScore: 400},
			},
			Failure: &ValidationFailure{
				Action: DecisionInvalid, Reason: ReasonMalformedValue, Detail: "legacy failure",
			},
		},
	}
	return input
}

func TestPreparePersonFactGenerationPerformsAlignmentBeforeReturn(t *testing.T) {
	input := validGenerationInput()
	first := validArchiveEvidence()
	second := cloneEvidence(first)
	second.SourceRef = "message-2"
	input.Claims = []ProposedClaim{validClaim(first), validClaim(second)}
	calls := 0
	aligner := evidenceAlignerFunc(func(_ context.Context, evidence EvidenceInput) (AlignmentResult, error) {
		calls++
		return AlignmentResult{Accepted: true, SourceVersion: evidence.SourceVersion + "-aligned", ContentSHA256: strings.Repeat(string(rune('a'+calls)), 64)}, nil
	})

	prepared, err := PreparePersonFactGeneration(t.Context(), input, aligner)
	require.NoError(t, err)
	assert.Equal(t, 2, calls)
	for _, claim := range prepared.Claims() {
		assert.Contains(t, claim.Evidence[0].SourceVersion, "-aligned")
	}
}

func TestPreparePersonFactGenerationOperationalAlignmentFailureReturnsNoPreparedValue(t *testing.T) {
	assert := assert.New(t)

	input := validGenerationInput()
	input.Claims = []ProposedClaim{validClaim(validArchiveEvidence())}
	wantErr := errors.New("archive unavailable")
	aligner := evidenceAlignerFunc(func(context.Context, EvidenceInput) (AlignmentResult, error) {
		return AlignmentResult{}, wantErr
	})

	prepared, err := PreparePersonFactGeneration(t.Context(), input, aligner)
	require.ErrorIs(t, err, wantErr)
	assert.Empty(prepared.GenerationKey())
	assert.Nil(prepared.CanonicalJSON())
	assert.Empty(prepared.Claims())
}

func TestPreparePersonFactGenerationAllowsStatusOnlyAndEmptyGenerations(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	empty := validGenerationInput()
	prepared, err := PreparePersonFactGeneration(t.Context(), empty, nil)
	requirements.NoError(err)
	assertions.NotEmpty(prepared.GenerationKey())
	assertions.Empty(prepared.Claims())
	assertions.Empty(prepared.EvidenceStatusChanges())

	statusOnly := validGenerationInput()
	statusOnly.EvidenceStatusChanges = []EvidenceStatusChange{
		{EvidenceKey: "evidence-a", SourceVersion: "v1", Supported: false, Reason: EvidenceStatusSourceDeleted},
		{EvidenceKey: "evidence-a", SourceVersion: "v2", Supported: true, Reason: EvidenceStatusSourceReimported},
		{EvidenceKey: "evidence-b", SourceVersion: "v1", Supported: false, Reason: EvidenceStatusSourceEdited},
		{EvidenceKey: "evidence-c", SourceVersion: "v1", Supported: false, Reason: EvidenceStatusScopeUnlinked},
		{EvidenceKey: "evidence-d", SourceVersion: "v1", Supported: false, Reason: EvidenceStatusIdentityReassigned},
		{EvidenceKey: "evidence-e", SourceVersion: "v1", Supported: true, Reason: EvidenceStatusScopeRelinked},
	}
	prepared, err = PreparePersonFactGeneration(t.Context(), statusOnly, nil)
	requirements.NoError(err)
	assertions.Empty(prepared.Claims())
	assertions.Len(prepared.EvidenceStatusChanges(), 6)

	invalidCases := []GenerationInput{
		func() GenerationInput { value := validGenerationInput(); value.SourceCursors = nil; return value }(),
		func() GenerationInput {
			value := validGenerationInput()
			value.SourceCursors[0].Lane = ""
			return value
		}(),
		func() GenerationInput {
			value := validGenerationInput()
			value.SourceCursors[0].Start = ""
			return value
		}(),
		func() GenerationInput {
			value := validGenerationInput()
			value.SourceCursors[0].End = ""
			return value
		}(),
		func() GenerationInput {
			value := validGenerationInput()
			value.SourceCursors = append(value.SourceCursors, value.SourceCursors[0])
			return value
		}(),
		func() GenerationInput {
			value := validGenerationInput()
			value.ProgramFingerprint = "not-a-hash"
			return value
		}(),
		func() GenerationInput {
			value := validGenerationInput()
			value.EvidenceStatusChanges = []EvidenceStatusChange{{EvidenceKey: "", SourceVersion: "v1", Supported: false, Reason: EvidenceStatusSourceDeleted}}
			return value
		}(),
		func() GenerationInput {
			value := validGenerationInput()
			value.EvidenceStatusChanges = []EvidenceStatusChange{{EvidenceKey: "e", SourceVersion: "v1", Supported: true, Reason: EvidenceStatusSourceDeleted}}
			return value
		}(),
		func() GenerationInput {
			value := validGenerationInput()
			change := EvidenceStatusChange{EvidenceKey: "e", SourceVersion: "v1", Supported: false, Reason: EvidenceStatusSourceDeleted}
			value.EvidenceStatusChanges = []EvidenceStatusChange{change, change}
			return value
		}(),
	}
	for index, input := range invalidCases {
		t.Run(string(rune('a'+index)), func(t *testing.T) {
			require := require.New(t)

			_, err := PreparePersonFactGeneration(t.Context(), input, nil)
			require.Error(err)
		})
	}

	for _, score := range []int{-1, 1001} {
		t.Run(fmt.Sprintf("reported confidence %d", score), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			input := validGenerationInput()
			claim := validClaim()
			claim.Confidence.ReportedScore = score
			input.Claims = []ProposedClaim{claim}
			prepared, err := PreparePersonFactGeneration(t.Context(), input, nil)
			require.NoError(err)
			failure := prepared.Claims()[0].Failure
			require.NotNil(failure)
			assert.Equal(ReasonMalformedValue, failure.Reason)
		})
	}
}

func TestPreparePersonFactGenerationRejectsMultipleStatusesForOneEvidenceVersion(t *testing.T) {
	changes := []EvidenceStatusChange{
		{EvidenceKey: "evidence", SourceVersion: "v1", Supported: false, Reason: EvidenceStatusSourceDeleted},
		{EvidenceKey: "evidence", SourceVersion: "v1", Supported: false, Reason: EvidenceStatusSourceEdited},
		{EvidenceKey: "evidence", SourceVersion: "v1", Supported: true, Reason: EvidenceStatusSourceReimported},
	}
	for left := range changes {
		for right := range changes {
			t.Run(fmt.Sprintf("%d-then-%d", left, right), func(t *testing.T) {
				require := require.New(t)
				input := validGenerationInput()
				input.EvidenceStatusChanges = []EvidenceStatusChange{changes[left], changes[right]}

				_, err := PreparePersonFactGeneration(t.Context(), input, nil)
				require.ErrorContains(err, `duplicate evidence status tuple for "evidence" at "v1"`)
			})
		}
	}
}

func validGenerationInput() GenerationInput {
	return GenerationInput{
		PersonID:      7,
		SourceCursors: []SourceCursor{{Lane: "optimistic", Start: "100", End: "200"}},
		ProgramID:     "people-extraction", ProgramVersion: "v1",
		ProgramFingerprint: strings.Repeat("f", 64), CatalogFingerprint: "catalog-v1",
		Provider: "fixture-provider", ProviderVersion: "2026-08-22",
		Model: "fixture-model", ModelVersion: "model-v1",
		ResolvedAt: utcTime(2025, 4, 12),
		Policy:     PolicyContext{AllowSensitive: false, ProviderPolicyFingerprint: "policy-v1"},
	}
}

func validArchiveEvidence() EvidenceInput {
	start, end := int64(10), int64(20)
	return EvidenceInput{
		PersonID: 7, SourceClass: EvidenceArchive, Directness: DirectSelf,
		Authority: AuthorityAuthoritative, SourceRef: "message-1",
		SubjectPersonID: new(int64(7)), SubjectRef: "participant-1",
		SpanStart: &start, SpanEnd: &end, Excerpt: "I joined Example",
		ContentSHA256: strings.Repeat("a", 64), SourceVersion: "immutable-v1",
		EventTime:    utcTime(2025, 1, 12),
		RecordedTime: utcTime(2025, 2, 12), IdentityScore: 950,
	}
}

func validClaim(evidence ...EvidenceInput) ProposedClaim {
	validFrom := utcTime(2020, 1, 0)
	return ProposedClaim{
		Target: testAttributeTarget(ValueText), Relation: RelationSupport,
		SubmittedValue: json.RawMessage(`"Example value"`), Evidence: evidence,
		ValidFrom: &validFrom, Origin: OriginExtraction,
		Confidence: ConfidenceInputs{ReportedScore: 800},
	}
}

func generationWithTwoEvidence() GenerationInput {
	first := validArchiveEvidence()
	second := cloneEvidence(first)
	second.SourceRef = "message-2"
	second.Excerpt = "Second source"
	second.ContentSHA256 = strings.Repeat("b", 64)
	input := validGenerationInput()
	input.SourceCursors = append(input.SourceCursors, SourceCursor{Lane: "backstop", Start: "a", End: "z"})
	firstClaim := validClaim(first, second)
	secondClaim := validClaim(second, first)
	secondClaim.Target = choiceTarget()
	secondClaim.SubmittedValue = json.RawMessage(`"active"`)
	input.Claims = []ProposedClaim{firstClaim, secondClaim}
	input.EvidenceStatusChanges = []EvidenceStatusChange{
		{EvidenceKey: "evidence-b", SourceVersion: "v1", Supported: false, Reason: EvidenceStatusSourceEdited},
		{EvidenceKey: "evidence-a", SourceVersion: "v1", Supported: true, Reason: EvidenceStatusSourceReimported},
	}
	return input
}

func mustPrepare(t *testing.T, input GenerationInput) PreparedGeneration {
	t.Helper()
	prepared, err := PreparePersonFactGeneration(t.Context(), input, acceptingAligner())
	require.NoError(t, err)
	return prepared
}

func cloneEvidence(input EvidenceInput) EvidenceInput {
	cloned := input
	if input.SubjectPersonID != nil {
		value := *input.SubjectPersonID
		cloned.SubjectPersonID = &value
	}
	if input.SpanStart != nil {
		value := *input.SpanStart
		cloned.SpanStart = &value
	}
	if input.SpanEnd != nil {
		value := *input.SpanEnd
		cloned.SpanEnd = &value
	}
	return cloned
}

func cloneGenerationInput(input GenerationInput) GenerationInput {
	cloned := input
	cloned.SourceCursors = append([]SourceCursor(nil), input.SourceCursors...)
	cloned.Claims = make([]ProposedClaim, len(input.Claims))
	for i, claim := range input.Claims {
		cloned.Claims[i] = claim
		cloned.Claims[i].Target = canonicalTarget(claim.Target)
		cloned.Claims[i].SubmittedValue = append(json.RawMessage(nil), claim.SubmittedValue...)
		cloned.Claims[i].Evidence = make([]EvidenceInput, len(claim.Evidence))
		for j, evidence := range claim.Evidence {
			cloned.Claims[i].Evidence[j] = cloneEvidence(evidence)
		}
		if claim.ValidFrom != nil {
			value := *claim.ValidFrom
			cloned.Claims[i].ValidFrom = &value
		}
		if claim.ValidUntil != nil {
			value := *claim.ValidUntil
			cloned.Claims[i].ValidUntil = &value
		}
	}
	cloned.EvidenceStatusChanges = append([]EvidenceStatusChange(nil), input.EvidenceStatusChanges...)
	return cloned
}

func clonePreparedClaim(input PreparedClaim) PreparedClaim {
	cloned := input
	cloned.Target = canonicalTarget(input.Target)
	cloned.SubmittedValue = append(json.RawMessage(nil), input.SubmittedValue...)
	cloned.SubmittedEvidenceFingerprints = append(
		[]string(nil), input.SubmittedEvidenceFingerprints...)
	if input.Normalized != nil {
		value := *input.Normalized
		value.JSON = append(json.RawMessage(nil), input.Normalized.JSON...)
		cloned.Normalized = &value
	}
	cloned.Evidence = make([]EvidenceInput, len(input.Evidence))
	for i, evidence := range input.Evidence {
		cloned.Evidence[i] = cloneEvidence(evidence)
	}
	cloned.EvidenceKeys = append([]string(nil), input.EvidenceKeys...)
	if input.ValidFrom != nil {
		value := *input.ValidFrom
		cloned.ValidFrom = &value
	}
	if input.ValidUntil != nil {
		value := *input.ValidUntil
		cloned.ValidUntil = &value
	}
	if input.Failure != nil {
		value := *input.Failure
		cloned.Failure = &value
	}
	return cloned
}
