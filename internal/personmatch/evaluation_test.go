package personmatch

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEvaluateLabeledPairsReportsClassesAndCalibration(t *testing.T) {
	assert := assert.New(t)
	report := EvaluateLabeledPairs([]LabeledPair{
		{Class: PairUncurated, SamePerson: true, Probability: 0.9, ProposedAccept: true, IndependentlyLabeled: true},
		{Class: PairUncurated, SamePerson: false, Probability: 0.1, IndependentlyLabeled: true},
		{Class: PairSavedUncurated, SamePerson: false, Probability: 0.9, ProposedAccept: true, IndependentlyLabeled: true},
		{Class: PairSavedSaved, SamePerson: true, Probability: 0.7, IndependentlyLabeled: true},
	})
	assert.InDelta(0.5, report.Precision, 1e-9)
	assert.InDelta(0.5, report.Recall, 1e-9)
	assert.Equal(1, report.FalseMerges)
	assert.Equal(1, report.Classes[PairSavedUncurated].FalseMerges)
	assert.Equal(1, report.Classes[PairSavedSaved].ReviewVolume)
	assert.NotEmpty(report.Calibration)
}

func TestSyntheticOrInsufficientNegativesCannotOpenLiveGate(t *testing.T) {
	assert := assert.New(t)
	rows := make([]LabeledPair, 1000)
	for i := range rows {
		rows[i] = LabeledPair{Class: PairUncurated, SamePerson: false,
			Probability: 0.1, Eligible: true, IndependentlyLabeled: true, Synthetic: true}
	}
	report := EvaluateLabeledPairs(rows)
	assert.Equal(1000, report.EligibleNegativePairs)
	assert.False(report.EvidenceGateSatisfied)
	for i := range rows {
		rows[i].Synthetic = false
	}
	report = EvaluateLabeledPairs(rows)
	assert.True(report.EvidenceGateSatisfied)
	assert.False(LiveAutomaticAcceptanceAvailable(), "this release has no consented evaluation record")
}
