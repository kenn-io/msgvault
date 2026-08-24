package identityindex

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRelationshipTemperatureScoreUsesDailySentAndGlobalReceivedCompression(t *testing.T) {
	signals := TemperatureSignals{
		SentSignal:     math.Log(3),
		ReceivedVolume: 8,
		MeetingSignal:  2,
		Modalities:     3,
	}

	assert.InDelta(t,
		(2*math.Log(3)+math.Log(9)+6)*1.5,
		RelationshipTemperatureScore(signals),
		1e-12,
	)
	assert.InDelta(t, 0.5, RelationshipDayWeight(RelationshipHalfLifeDays), 1e-12)
}

func TestNormalizeRelationshipTemperaturesUsesDistinctDenseRanks(t *testing.T) {
	got := NormalizeRelationshipTemperatures([]ScoredCanonical{
		{CanonicalID: 7, RawScore: 1},
		{CanonicalID: 9, RawScore: 1},
		{CanonicalID: 11, RawScore: 4},
	})

	assert.Equal(t, []RankedCanonical{
		{CanonicalID: 7, RawScore: 1, Temperature: 0, Rank: 2, Population: 3},
		{CanonicalID: 9, RawScore: 1, Temperature: 0, Rank: 2, Population: 3},
		{CanonicalID: 11, RawScore: 4, Temperature: 100, Rank: 1, Population: 3},
	}, got)
}

func TestNormalizeRelationshipTemperaturesMapsOneDistinctScoreToOneHundred(t *testing.T) {
	assert := assert.New(t)
	got := NormalizeRelationshipTemperatures([]ScoredCanonical{
		{CanonicalID: 2, RawScore: 3},
		{CanonicalID: 4, RawScore: 3},
	})

	require.Len(t, got, 2)
	assert.Equal(100, got[0].Temperature)
	assert.Equal(100, got[1].Temperature)
	assert.Equal(int64(1), got[0].Rank)
	assert.Equal(int64(1), got[1].Rank)
}

func TestAssignRelationshipHeatLevelsUsesNearestRankAndKeepsTiesTogether(t *testing.T) {
	days := []DailyTemperatureSignals{
		{Total: 0},
		{Total: 1},
		{Total: 1},
		{Total: 3},
		{Total: 9},
	}

	assert.Equal(t, []HeatLevel{
		HeatNone,
		HeatFirstQuartile,
		HeatFirstQuartile,
		HeatThirdQuartile,
		HeatFourthQuartile,
	}, AssignRelationshipHeatLevels(days))
	assert.Equal(t,
		[]HeatLevel{HeatFirstQuartile, HeatFirstQuartile},
		AssignRelationshipHeatLevels([]DailyTemperatureSignals{{Total: 4}, {Total: 4}}),
	)
}

func TestAssignRelationshipHeatLevelsMarksAllZeroDaysAsNone(t *testing.T) {
	assert.Equal(t,
		[]HeatLevel{HeatNone, HeatNone, HeatNone},
		AssignRelationshipHeatLevels([]DailyTemperatureSignals{{}, {}, {}}),
	)
}

func TestPeakAnnualRelationshipTemperatureUsesMostRecentYearForTie(t *testing.T) {
	assert := assert.New(t)
	peak, ok := PeakAnnualRelationshipTemperature([]AnnualTemperatureSummary{
		{Year: 2018, Temperature: 87},
		{Year: 2024, Temperature: 42},
		{Year: 2021, Temperature: 87},
	})

	require.True(t, ok)
	assert.Equal(2021, peak.Year)
	assert.Equal(87, peak.Temperature)
	_, ok = PeakAnnualRelationshipTemperature(nil)
	assert.False(ok)
}
