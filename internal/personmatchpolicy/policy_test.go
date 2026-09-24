package personmatchpolicy

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEvaluateRequiresScoreAndIndependentCorroboration(t *testing.T) {
	assert := assert.New(t)
	base := Input{Probability: 0.81, MinimumProbability: 0.80,
		Signals: []Signal{{Class: SignalEmail, SourceID: 1}, {Class: SignalSelfDeclaration, SourceID: 2}}}
	assert.Equal(ProposedAccept, Evaluate(base).Action)
	base.Probability = 0.80
	assert.Equal(NeedsReview, Evaluate(base).Action)
	base.Probability = 0.99
	base.Signals[1].SourceID = 1
	assert.Equal(NeedsReview, Evaluate(base).Action)
	base.Signals = []Signal{{Class: SignalDisplayName, SourceID: 1}, {Class: SignalConversation, SourceID: 2}}
	assert.Equal(NeedsReview, Evaluate(base).Action)
}

func TestEvaluateHardGuardsOverrideHighScore(t *testing.T) {
	base := Input{Probability: 0.99, MinimumProbability: 0.80,
		Signals: []Signal{{Class: SignalPhone, SourceID: 1}, {Class: SignalSelfDeclaration, SourceID: 2}}}
	for _, tc := range []struct {
		name   string
		change func(*Input)
	}{
		{"rejected", func(in *Input) { in.UserRejected = true }},
		{"stable ID conflict", func(in *Input) { in.ConflictingStableID = true }},
		{"shared phone", func(in *Input) { in.SharedContactPoint = true }},
		{"published profile", func(in *Input) { in.ActiveCardDAVPublication = true }},
		{"stale revision", func(in *Input) { in.StaleRevision = true }},
		{"manual merge", func(in *Input) { in.ManualMergeReview = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.change(&in)
			assert.Equal(t, NeedsReview, Evaluate(in).Action)
		})
	}
}

func TestEvaluateVerifiedFullNameRequiresIndependentContactEvidence(t *testing.T) {
	input := Input{Probability: 0.91, MinimumProbability: 0.8,
		Signals: []Signal{{Class: SignalVerifiedFullName, SourceID: 1}, {Class: SignalEmail, SourceID: 2}}}
	assert.Equal(t, ProposedAccept, Evaluate(input).Action)
	input.Signals[1].Class = SignalConversation
	assert.Equal(t, NeedsReview, Evaluate(input).Action)
}
