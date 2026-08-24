package personfacts

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvidenceValidationBoundsAndPairs(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	valid := validArchiveEvidence()
	valid.Excerpt = strings.Repeat("界", MaxEvidenceExcerptRunes)
	validPublic := valid
	validPublic.SourceClass = EvidencePublic
	validPublic.SourceRef = ""
	validPublic.SourceURL = "https://example.com/profile"
	validPublic.SpanStart = nil
	validPublic.SpanEnd = nil
	validProvider := validPublic
	validProvider.SourceClass = EvidenceProviderAssertion
	validProvider.SourceRef = "provider-job-123"
	validProvider.SourceURL = ""
	validProvider.Directness = Indirect
	validProvider.Authority = AuthorityOrdinary
	citedProvider := validProvider
	citedProvider.SourceURL = "https://example.com/provider-result"
	validSystem := validProvider
	validSystem.SourceClass = EvidenceSystem
	validSystem.SourceRef = "system-observation-123"

	tests := []struct {
		name    string
		mutate  func(*EvidenceInput)
		wantErr bool
	}{
		{name: "valid archive"},
		{name: "2000 rune excerpt"},
		{name: "2001 rune excerpt", mutate: func(v *EvidenceInput) { v.Excerpt += "界" }, wantErr: true},
		{name: "missing span end", mutate: func(v *EvidenceInput) { v.SpanEnd = nil }, wantErr: true},
		{name: "negative span", mutate: func(v *EvidenceInput) { n := int64(-1); v.SpanStart = &n }, wantErr: true},
		{name: "reversed span", mutate: func(v *EvidenceInput) { n := int64(5); v.SpanEnd = &n }, wantErr: true},
		{name: "uppercase hash", mutate: func(v *EvidenceInput) { v.ContentSHA256 = strings.Repeat("A", 64) }, wantErr: true},
		{name: "non UTC event time", mutate: func(v *EvidenceInput) { v.EventTime = v.EventTime.In(fixedZone()) }, wantErr: true},
		{name: "non UTC recorded time", mutate: func(v *EvidenceInput) { v.RecordedTime = v.RecordedTime.In(fixedZone()) }, wantErr: true},
		{name: "identity below bound", mutate: func(v *EvidenceInput) { v.IdentityScore = -1 }, wantErr: true},
		{name: "identity above bound", mutate: func(v *EvidenceInput) { v.IdentityScore = 1001 }, wantErr: true},
		{name: "archive missing ref", mutate: func(v *EvidenceInput) { v.SourceRef = "" }, wantErr: true},
		{name: "archive missing source version", mutate: func(v *EvidenceInput) { v.SourceVersion = "" }, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			input := valid
			if test.mutate != nil {
				test.mutate(&input)
			}
			key, err := EvidenceKey(input)
			if test.wantErr {
				require.Error(err)
				assert.Empty(key)
				return
			}
			require.NoError(err)
			assert.NotEmpty(key)
		})
	}

	for _, input := range []EvidenceInput{validPublic, validProvider, citedProvider} {
		key, err := EvidenceKey(input)
		requirements.NoError(err)
		assertions.NotEmpty(key)
	}
	for _, input := range []EvidenceInput{validPublic, validProvider, validSystem} {
		input.SourceVersion = ""
		key, err := EvidenceKey(input)
		requirements.NoError(err)
		assertions.NotEmpty(key)
	}

	invalidProvider := []EvidenceInput{validProvider, validProvider}
	invalidProvider[0].Directness = DirectSelf
	invalidProvider[1].Authority = AuthorityAuthoritative
	for _, input := range invalidProvider {
		_, err := EvidenceKey(input)
		requirements.Error(err)
	}

	for _, sourceURL := range []string{"http://example.com/profile", "https://:443/source"} {
		invalidPublic := validPublic
		invalidPublic.SourceURL = sourceURL
		_, err := EvidenceKey(invalidPublic)
		requirements.Error(err, sourceURL)
	}
}

func TestEvidenceAlignmentBindsImmutableSourceVersion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	evidence := validArchiveEvidence()
	originalVersion := evidence.SourceVersion
	originalHash := evidence.ContentSHA256
	aligner := evidenceAlignerFunc(func(_ context.Context, got EvidenceInput) (AlignmentResult, error) {
		assert.Equal(originalVersion, got.SourceVersion)
		assert.Equal(originalHash, got.ContentSHA256)
		return AlignmentResult{
			Accepted: true, SourceVersion: "immutable-v2", ContentSHA256: strings.Repeat("b", 64),
		}, nil
	})
	input := validGenerationInput()
	input.Claims = []ProposedClaim{validClaim(evidence)}

	prepared, err := PreparePersonFactGeneration(t.Context(), input, aligner)
	require.NoError(err)
	claims := prepared.Claims()
	require.Len(claims, 1)
	require.Len(claims[0].Evidence, 1)
	assert.Equal("immutable-v2", claims[0].Evidence[0].SourceVersion)
	assert.Equal(strings.Repeat("b", 64), claims[0].Evidence[0].ContentSHA256)
	assert.Equal(originalVersion, input.Claims[0].Evidence[0].SourceVersion)
	assert.Equal(originalHash, input.Claims[0].Evidence[0].ContentSHA256)
	wantKey, err := EvidenceKey(claims[0].Evidence[0])
	require.NoError(err)
	assert.Equal([]string{wantKey}, claims[0].EvidenceKeys)
}

func TestPreparePersonFactGenerationAlignsExternalEvidenceWhenAlignerIsSupplied(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	public := validArchiveEvidence()
	public.SourceClass = EvidencePublic
	public.SourceRef = "citation-key"
	public.SourceURL = "https://example.com/profile"
	public.SpanStart = nil
	public.SpanEnd = nil
	public.SourceVersion = "submitted-version"
	public.ContentSHA256 = ""
	input := validGenerationInput()
	input.Claims = []ProposedClaim{validClaim(public)}
	called := 0
	prepared, err := PreparePersonFactGeneration(t.Context(), input,
		evidenceAlignerFunc(func(_ context.Context, got EvidenceInput) (AlignmentResult, error) {
			called++
			assert.Equal("citation-key", got.SourceRef)
			return AlignmentResult{
				Accepted: true, SourceVersion: "citation-version-v1",
				ContentSHA256: strings.Repeat("c", 64),
			}, nil
		}))
	require.NoError(err)
	assert.Equal(1, called)
	claims := prepared.Claims()
	require.Len(claims, 1)
	require.Len(claims[0].Evidence, 1)
	assert.Equal("citation-version-v1", claims[0].Evidence[0].SourceVersion)
	assert.Equal(strings.Repeat("c", 64), claims[0].Evidence[0].ContentSHA256)
}

func TestEvidenceSubjectIsCanonicalPersonID(t *testing.T) {
	tests := []struct {
		name    string
		subject *int64
		failed  bool
	}{
		{name: "canonical subject", subject: new(int64(7))},
		{name: "missing subject", subject: nil, failed: true},
		{name: "wrong subject", subject: new(int64(8)), failed: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			evidence := validArchiveEvidence()
			evidence.SubjectPersonID = test.subject
			input := validGenerationInput()
			input.Claims = []ProposedClaim{validClaim(evidence)}
			prepared, err := PreparePersonFactGeneration(t.Context(), input, acceptingAligner())
			require.NoError(err)
			claim := prepared.Claims()[0]
			if !test.failed {
				assert.Nil(claim.Failure)
				return
			}
			require.NotNil(claim.Failure)
			assert.Equal(DecisionIdentityRejected, claim.Failure.Action)
			assert.Equal(ReasonIdentityMismatch, claim.Failure.Reason)
			assert.Empty(claim.Evidence)
			assert.Empty(claim.EvidenceKeys)
		})
	}
}

func fixedZone() *time.Location { return time.FixedZone("fixture", 3600) }

type evidenceAlignerFunc func(context.Context, EvidenceInput) (AlignmentResult, error)

func (f evidenceAlignerFunc) Align(ctx context.Context, input EvidenceInput) (AlignmentResult, error) {
	return f(ctx, input)
}

func acceptingAligner() EvidenceAligner {
	return evidenceAlignerFunc(func(_ context.Context, input EvidenceInput) (AlignmentResult, error) {
		return AlignmentResult{Accepted: true, SourceVersion: input.SourceVersion, ContentSHA256: input.ContentSHA256}, nil
	})
}
