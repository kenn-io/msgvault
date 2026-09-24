package personmatch

import "math"

type PairClass string

const (
	PairUncurated      PairClass = "uncurated"
	PairSavedUncurated PairClass = "saved_plus_uncurated"
	PairSavedSaved     PairClass = "saved_plus_saved"
)

type LabeledPair struct {
	Class                PairClass `json:"class"`
	SamePerson           bool      `json:"same_person"`
	Probability          float64   `json:"probability"`
	ProposedAccept       bool      `json:"proposed_accept"`
	Eligible             bool      `json:"eligible"`
	IndependentlyLabeled bool      `json:"independently_labeled"`
	Synthetic            bool      `json:"synthetic"`
}

type ClassMetrics struct {
	Pairs           int     `json:"pairs"`
	ProposedAccepts int     `json:"proposed_accepts"`
	TrueMerges      int     `json:"true_merges"`
	FalseMerges     int     `json:"false_merges"`
	ReviewVolume    int     `json:"review_volume"`
	Precision       float64 `json:"precision"`
	Recall          float64 `json:"recall"`
}

type CalibrationBand struct {
	Lower            float64 `json:"lower"`
	Upper            float64 `json:"upper"`
	Count            int     `json:"count"`
	MeanProbability  float64 `json:"mean_probability"`
	ObservedSameRate float64 `json:"observed_same_rate"`
}

type EvaluationReport struct {
	Pairs                 int                        `json:"pairs"`
	Precision             float64                    `json:"precision"`
	Recall                float64                    `json:"recall"`
	FalseMerges           int                        `json:"false_merges"`
	EligibleNegativePairs int                        `json:"eligible_negative_pairs"`
	EvidenceGateSatisfied bool                       `json:"evidence_gate_satisfied"`
	Classes               map[PairClass]ClassMetrics `json:"classes"`
	Calibration           []CalibrationBand          `json:"calibration"`
}

// EvaluateLabeledPairs reports observed behavior. Independently labeled,
// nonsynthetic negative pairs are the only records eligible for the gate.
func EvaluateLabeledPairs(rows []LabeledPair) EvaluationReport {
	report := EvaluationReport{Classes: map[PairClass]ClassMetrics{
		PairUncurated: {}, PairSavedUncurated: {}, PairSavedSaved: {},
	}, Calibration: make([]CalibrationBand, 5)}
	var positives, trueMerges, proposed int
	classPositives := map[PairClass]int{}
	var bandProbability [5]float64
	var bandSame [5]int
	allGateRowsReal := true
	for i := range report.Calibration {
		report.Calibration[i].Lower = float64(i) / 5
		report.Calibration[i].Upper = float64(i+1) / 5
	}
	for _, row := range rows {
		if math.IsNaN(row.Probability) || math.IsInf(row.Probability, 0) || row.Probability < 0 || row.Probability > 1 {
			continue
		}
		metrics, known := report.Classes[row.Class]
		if !known {
			continue
		}
		report.Pairs++
		metrics.Pairs++
		if row.SamePerson {
			positives++
			classPositives[row.Class]++
		}
		if row.ProposedAccept {
			proposed++
			metrics.ProposedAccepts++
			if row.SamePerson {
				trueMerges++
				metrics.TrueMerges++
			} else {
				report.FalseMerges++
				metrics.FalseMerges++
			}
		} else {
			metrics.ReviewVolume++
		}
		if !row.SamePerson && row.IndependentlyLabeled && row.Eligible {
			report.EligibleNegativePairs++
			if row.Synthetic {
				allGateRowsReal = false
			}
		}
		band := int(row.Probability * 5)
		if band == 5 {
			band = 4
		}
		report.Calibration[band].Count++
		bandProbability[band] += row.Probability
		if row.SamePerson {
			bandSame[band]++
		}
		report.Classes[row.Class] = metrics
	}
	if proposed > 0 {
		report.Precision = float64(trueMerges) / float64(proposed)
	}
	if positives > 0 {
		report.Recall = float64(trueMerges) / float64(positives)
	}
	for class, metrics := range report.Classes {
		if metrics.ProposedAccepts > 0 {
			metrics.Precision = float64(metrics.TrueMerges) / float64(metrics.ProposedAccepts)
		}
		if classPositives[class] > 0 {
			metrics.Recall = float64(metrics.TrueMerges) / float64(classPositives[class])
		}
		report.Classes[class] = metrics
	}
	for i := range report.Calibration {
		if report.Calibration[i].Count == 0 {
			continue
		}
		report.Calibration[i].MeanProbability = bandProbability[i] / float64(report.Calibration[i].Count)
		report.Calibration[i].ObservedSameRate = float64(bandSame[i]) / float64(report.Calibration[i].Count)
	}
	report.EvidenceGateSatisfied = report.EligibleNegativePairs >= 1000 && report.FalseMerges == 0 && allGateRowsReal
	return report
}

// LiveAutomaticAcceptanceAvailable is deliberately false in this release.
// There is no operator override or synthetic fixture path to a live merge.
func LiveAutomaticAcceptanceAvailable() bool { return false }
