package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValueEnvelopeValidate(t *testing.T) {
	require := require.New(t)

	confidence := 0.4
	tests := []struct {
		name    string
		env     ValueEnvelope
		wantErr error
	}{
		{
			name: "declared user value",
			env:  ValueEnvelope{Source: ProvenanceUser, Pref: new(1)},
		},
		{
			name:    "unknown source",
			env:     ValueEnvelope{Source: "beeper"},
			wantErr: ErrInvalidProvenance,
		},
		{
			name:    "pref below range",
			env:     ValueEnvelope{Source: ProvenanceUser, Pref: new(0)},
			wantErr: ErrInvalidProfilePref,
		},
		{
			name:    "pref above range",
			env:     ValueEnvelope{Source: ProvenanceUser, Pref: new(101)},
			wantErr: ErrInvalidProfilePref,
		},
		{
			name:    "confidence on declared value",
			env:     ValueEnvelope{Source: ProvenanceUser, Confidence: &confidence},
			wantErr: ErrConfidenceScope,
		},
		{
			name: "confidence on extracted value",
			env:  ValueEnvelope{Source: ProvenanceExtraction, Confidence: &confidence},
		},
	}
	for _, test := range tests {
		err := test.env.Validate()
		if test.wantErr == nil {
			require.NoError(err, test.name)
			continue
		}
		require.ErrorIs(err, test.wantErr, test.name)
	}
}

func TestValueEnvelopeIsCurrentUsesBothTimeAxes(t *testing.T) {
	assert := assert.New(t)

	closed := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	assert.True(ValueEnvelope{}.IsCurrent())
	assert.False(ValueEnvelope{ActiveUntil: &closed}.IsCurrent())
	assert.False(ValueEnvelope{SupersededAt: &closed}.IsCurrent())
	assert.False(ValueEnvelope{ActiveUntil: &closed, SupersededAt: &closed}.IsCurrent())
}

func TestTypeTokensRoundTripPreservesOrderAndSpelling(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	tokens := []string{"HOME", "voice", "pref"}
	stored := joinTypeTokens(tokens)
	require.NotNil(stored)
	assert.Equal("HOME,voice,pref", *stored)
	assert.Equal(tokens, splitTypeTokens(stored))
	assert.Nil(joinTypeTokens(nil))
	assert.Empty(splitTypeTokens(nil))
}
