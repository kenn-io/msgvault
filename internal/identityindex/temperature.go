package identityindex

import (
	"math"
	"slices"
)

// RelationshipScoreVersion identifies the scoring contract persisted in the
// analytical cache. Bump it when signal attribution or score composition
// changes independently of the Parquet schema version.
const (
	RelationshipScoreVersion  = 1
	temperatureWeightSent     = 2.0
	temperatureWeightReceived = 1.0
	temperatureWeightMeetings = 3.0
	temperatureBreadthStep    = 0.25
)

// HeatLevel is the server-owned GitHub-style intensity assigned to one local
// calendar day. Clients render the name; they do not derive quartiles.
type HeatLevel string

const (
	HeatNone           HeatLevel = "NONE"
	HeatFirstQuartile  HeatLevel = "FIRST_QUARTILE"
	HeatSecondQuartile HeatLevel = "SECOND_QUARTILE"
	HeatThirdQuartile  HeatLevel = "THIRD_QUARTILE"
	HeatFourthQuartile HeatLevel = "FOURTH_QUARTILE"
)

// DailyTemperatureSignals contains the qualifying message-grain counts for
// one person on one day. Total drives heat-level assignment; the other fields
// make the result explainable to API and agent consumers.
type DailyTemperatureSignals struct {
	Sent         int64 `json:"sent"`
	Received     int64 `json:"received"`
	Email        int64 `json:"email"`
	Chat         int64 `json:"chat"`
	Meetings     int64 `json:"meetings"`
	Total        int64 `json:"total"`
	ModalityMask uint8 `json:"modality_mask"`
}

// TemperatureSignals are the aggregates composed into a raw relationship
// score. SentSignal is already log-compressed per day; received volume is
// compressed once by RelationshipTemperatureScore.
type TemperatureSignals struct {
	SentSignal     float64 `json:"sent_signal"`
	ReceivedVolume float64 `json:"received_volume"`
	MeetingSignal  float64 `json:"meeting_signal"`
	Modalities     int     `json:"modalities"`
}

// TemperatureSummary is one graph-relative score and its explainability
// metadata.
type TemperatureSummary struct {
	Temperature int                `json:"temperature"`
	Rank        int64              `json:"rank"`
	Population  int64              `json:"population"`
	RawScore    float64            `json:"raw_score"`
	Signals     TemperatureSignals `json:"signals"`
}

// AnnualTemperatureSummary is a UTC calendar-year score.
type AnnualTemperatureSummary struct {
	TemperatureSummary

	Year int `json:"year"`
}

// ScoredCanonical is the minimal input to graph normalization.
type ScoredCanonical struct {
	CanonicalID int64
	RawScore    float64
}

// RankedCanonical is a normalized score for one canonical contact.
type RankedCanonical struct {
	CanonicalID int64
	RawScore    float64
	Temperature int
	Rank        int64
	Population  int64
}

// RelationshipTemperatureScore composes the explainable aggregate signals.
func RelationshipTemperatureScore(signals TemperatureSignals) float64 {
	breadth := 1.0
	if signals.Modalities > 1 {
		breadth += temperatureBreadthStep * float64(signals.Modalities-1)
	}
	return (temperatureWeightSent*signals.SentSignal +
		temperatureWeightReceived*math.Log1p(signals.ReceivedVolume) +
		temperatureWeightMeetings*signals.MeetingSignal) * breadth
}

// RelationshipDayWeight applies the shared 365-day half-life. Future facts
// are excluded before scoring; clamping here prevents a negative age from
// amplifying a signal if a caller hands the helper an unvalidated value.
func RelationshipDayWeight(ageDays float64) float64 {
	return math.Exp(-RelationshipDecayRate * max(ageDays, 0))
}

// NormalizeRelationshipTemperatures assigns distinct-score dense-rank
// temperatures while retaining the caller's stable row order.
func NormalizeRelationshipTemperatures(scored []ScoredCanonical) []RankedCanonical {
	if len(scored) == 0 {
		return nil
	}
	ascending := make([]float64, len(scored))
	for i, row := range scored {
		ascending[i] = row.RawScore
	}
	slices.Sort(ascending)
	distinct := slices.Compact(ascending)

	descending := slices.Clone(distinct)
	slices.Reverse(descending)
	ranks := make(map[float64]int64, len(distinct))
	for i, score := range descending {
		if _, exists := ranks[score]; !exists {
			ranks[score] = int64(i + 1)
		}
	}
	denseAscending := make(map[float64]int, len(distinct))
	for i, score := range distinct {
		denseAscending[score] = i
	}

	population := int64(len(scored))
	result := make([]RankedCanonical, len(scored))
	for i, row := range scored {
		temperature := 100
		if len(distinct) > 1 {
			temperature = int(math.Round(100 * float64(denseAscending[row.RawScore]) / float64(len(distinct)-1)))
		}
		result[i] = RankedCanonical{
			CanonicalID: row.CanonicalID,
			RawScore:    row.RawScore,
			Temperature: temperature,
			Rank:        ranks[row.RawScore],
			Population:  population,
		}
	}
	return result
}

// AssignRelationshipHeatLevels applies nearest-rank quartiles to positive
// active-day totals. Equal totals always receive equal levels.
func AssignRelationshipHeatLevels(days []DailyTemperatureSignals) []HeatLevel {
	levels := make([]HeatLevel, len(days))
	for i := range levels {
		levels[i] = HeatNone
	}
	positive := make([]int64, 0, len(days))
	for _, day := range days {
		if day.Total > 0 {
			positive = append(positive, day.Total)
		}
	}
	if len(positive) == 0 {
		return levels
	}
	slices.Sort(positive)
	nearestRank := func(fraction float64) int64 {
		position := int(math.Ceil(fraction*float64(len(positive)))) - 1
		return positive[max(position, 0)]
	}
	q25 := nearestRank(0.25)
	q50 := nearestRank(0.50)
	q75 := nearestRank(0.75)
	for i, day := range days {
		switch {
		case day.Total == 0:
			levels[i] = HeatNone
		case day.Total <= q25:
			levels[i] = HeatFirstQuartile
		case day.Total <= q50:
			levels[i] = HeatSecondQuartile
		case day.Total <= q75:
			levels[i] = HeatThirdQuartile
		default:
			levels[i] = HeatFourthQuartile
		}
	}
	return levels
}

// PeakAnnualRelationshipTemperature selects the highest annual temperature;
// the most recent UTC year wins a tie.
func PeakAnnualRelationshipTemperature(annual []AnnualTemperatureSummary) (AnnualTemperatureSummary, bool) {
	if len(annual) == 0 {
		return AnnualTemperatureSummary{}, false
	}
	peak := annual[0]
	for _, candidate := range annual[1:] {
		if candidate.Temperature > peak.Temperature ||
			(candidate.Temperature == peak.Temperature && candidate.Year > peak.Year) {
			peak = candidate
		}
	}
	return peak, true
}
