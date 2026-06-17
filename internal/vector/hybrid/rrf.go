// Package hybrid implements reciprocal-rank fusion and the search
// engine that routes queries to the active vector.Backend.
package hybrid

import (
	"math"
	"sort"
	"strings"

	"go.kenn.io/msgvault/internal/vector"
)

// Fuse combines two ranked lists via Reciprocal Rank Fusion.
//
//   - rrfK:        standard RRF constant (60 is typical).
//   - boost:       >=1.0 multiplier applied to any message whose subject
//     contains any term in subjectTerms (case-insensitive).
//     Pass 1.0 to disable.
//   - subjectTerms: lowercased query terms used for subject matching.
//   - subjects:     map[message_id]subject_text, used ONLY for boost
//     lookup. May be nil (no boost applied).
//
// Returns hits ordered by RRFScore DESC.
func Fuse(
	bm25, vec []vector.Hit,
	rrfK int,
	boost float64,
	subjectTerms []string,
	subjects map[int64]string,
) []vector.FusedHit {
	byID := make(map[int64]*vector.FusedHit)
	for _, h := range bm25 {
		entry := getOrInit(byID, h.MessageID)
		entry.BM25Score = h.Score
		entry.RRFScore += 1.0 / float64(rrfK+h.Rank)
	}
	for _, h := range vec {
		entry := getOrInit(byID, h.MessageID)
		entry.VectorScore = h.Score
		entry.RRFScore += 1.0 / float64(rrfK+h.Rank)
	}

	lowerTerms := make([]string, len(subjectTerms))
	for i, t := range subjectTerms {
		lowerTerms[i] = strings.ToLower(t)
	}

	out := make([]vector.FusedHit, 0, len(byID))
	for _, h := range byID {
		if boost > 1.0 && len(lowerTerms) > 0 && subjects != nil {
			if subj, ok := subjects[h.MessageID]; ok {
				lc := strings.ToLower(subj)
				for _, t := range lowerTerms {
					if strings.Contains(lc, t) {
						h.RRFScore *= boost
						h.SubjectBoosted = true
						break
					}
				}
			}
		}
		out = append(out, *h)
	}
	// Sort by RRFScore descending with MessageID ascending as
	// tie-breaker. Tied RRF scores are realistic (e.g. two messages
	// with the same bm25/vector rank pair), and without a stable
	// secondary key the output order depends on map iteration,
	// which breaks pagination and reproducibility.
	sort.Slice(out, func(i, j int) bool {
		if out[i].RRFScore != out[j].RRFScore {
			return out[i].RRFScore > out[j].RRFScore
		}
		return out[i].MessageID < out[j].MessageID
	})
	return out
}

func getOrInit(m map[int64]*vector.FusedHit, id int64) *vector.FusedHit {
	if h, ok := m[id]; ok {
		return h
	}
	h := &vector.FusedHit{MessageID: id, BM25Score: math.NaN(), VectorScore: math.NaN()}
	m[id] = h
	return h
}
