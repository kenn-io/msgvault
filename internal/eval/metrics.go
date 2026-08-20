// Package eval provides retrieval-quality evaluation for msgvault: standard
// information-retrieval metrics (precision@k, recall@k, nDCG@k, MAP, MRR)
// computed over a ranked result list scored against relevance judgments
// (qrels). Every one of them is bounded by the depth the ranking was
// retrieved to, MAP and MRR included even though they take no k of their own,
// so Cutoffs records that depth for a report to label them with.
//
// The metric functions are pure — no I/O, no engine or database dependencies —
// so they are unit-testable in isolation and reused by the `msgvault eval`
// command, which supplies the rankings by running the search engine.
//
// Judgments are binary here: a document is relevant (in the set) or not. This
// matches TREC-style qrels where a positive grade means "relevant". Graded
// relevance can be layered on later without changing the command surface.
package eval

import "math"

// Cutoffs are the rank depths one run scores at. They travel with the run
// rather than being baked into the metric names because a cutoff deeper than
// the retrieval depth is a mislabeled number, not a hard number: a run that
// only ever retrieves 20 results per query cannot have a recall@100, and
// reporting one as "R@100" invites a reader to compare it against a run that
// really did look 100 deep.
type Cutoffs struct {
	P      int // precision depth
	NDCG   int // nDCG depth
	Recall int // recall depth
	// Depth is the retrieval depth itself: the rank past which the run never
	// looked, so the length of the longest ranking Evaluate can be handed.
	//
	// It is here because MAP and MRR have no cutoff of their own — they run
	// over the whole list they are given — which makes the list's length their
	// cutoff whether or not anyone names it. Reporting them unqualified claims
	// they saw every relevant document, when a run at -n 20 cannot see one at
	// rank 21 and its "MRR" is really MRR@20. Zero when the depth is not known
	// (the standard set), in which case a caller has nothing to qualify them
	// with and should print them bare.
	Depth int
}

// StandardCutoffs is the conventional depth set: P@10, nDCG@10, R@100. It
// carries no Depth: it names the metrics wanted, not a run that produced them.
var StandardCutoffs = Cutoffs{P: 10, NDCG: 10, Recall: 100}

// CutoffsForDepth clamps StandardCutoffs to the number of results a run
// actually retrieves per query, so the reported metric is always one the run
// could in principle have maximised, and records that depth for the metrics
// that are bounded by it without being named after it. A non-positive limit is
// meaningless as a depth and falls back to the standard set.
func CutoffsForDepth(limit int) Cutoffs {
	if limit <= 0 {
		return StandardCutoffs
	}
	return Cutoffs{
		P:      min(StandardCutoffs.P, limit),
		NDCG:   min(StandardCutoffs.NDCG, limit),
		Recall: min(StandardCutoffs.Recall, limit),
		Depth:  limit,
	}
}

// IsStandard reports whether the named cutoffs are the conventional ones, so a
// caller can tell a clamped run from a full-depth one. Depth is deliberately
// ignored: runs at -n 100 and -n 500 both report P@10/nDCG@10/R@100, and only
// the clamp is worth warning about.
func (c Cutoffs) IsStandard() bool {
	return c.P == StandardCutoffs.P &&
		c.NDCG == StandardCutoffs.NDCG &&
		c.Recall == StandardCutoffs.Recall
}

// Scores holds the standard metric set for a single query's ranking. The
// depths every field was measured at are not stored here — they are a property
// of the run, carried in Cutoffs, and every Scores folded into one Aggregate
// must share them.
type Scores struct {
	P      float64 // precision@Cutoffs.P
	NDCG   float64 // normalized DCG@Cutoffs.NDCG (binary gains)
	Recall float64 // recall@Cutoffs.Recall
	// MAP is average precision (the "AP" that MAP averages) and MRR the
	// reciprocal rank of the first relevant hit. Neither takes a cutoff, but
	// both are bounded by one all the same: they are computed over the ranking
	// as handed in, and a run that retrieves Cutoffs.Depth results cannot see a
	// relevant document below that rank. They are therefore MAP@Depth and
	// MRR@Depth, and a report that labels them otherwise invites comparison
	// against a run that looked deeper.
	MAP float64
	MRR float64
}

// Evaluate computes the standard metric set for one query. ranked is the
// ordered list of retrieved document ids (best first); rel is the set of
// document ids judged relevant for the query; c gives the rank depths to
// score at (see CutoffsForDepth).
func Evaluate(ranked []string, rel map[string]struct{}, c Cutoffs) Scores {
	return Scores{
		P:      PrecisionAt(ranked, rel, c.P),
		NDCG:   NDCGAt(ranked, rel, c.NDCG),
		Recall: RecallAt(ranked, rel, c.Recall),
		MAP:    AveragePrecision(ranked, rel),
		MRR:    ReciprocalRank(ranked, rel),
	}
}

func isRel(rel map[string]struct{}, d string) bool {
	_, ok := rel[d]
	return ok
}

// hitsInTopK counts relevant docs among the first k of ranked.
func hitsInTopK(ranked []string, rel map[string]struct{}, k int) int {
	hit := 0
	for i := 0; i < k && i < len(ranked); i++ {
		if isRel(rel, ranked[i]) {
			hit++
		}
	}
	return hit
}

// PrecisionAt returns the fraction of the top-k results that are relevant.
func PrecisionAt(ranked []string, rel map[string]struct{}, k int) float64 {
	if k <= 0 {
		return 0
	}
	return float64(hitsInTopK(ranked, rel, k)) / float64(k)
}

// RecallAt returns the fraction of all relevant docs found in the top-k.
func RecallAt(ranked []string, rel map[string]struct{}, k int) float64 {
	if len(rel) == 0 {
		return 0
	}
	return float64(hitsInTopK(ranked, rel, k)) / float64(len(rel))
}

// NDCGAt returns normalized discounted cumulative gain at k with binary gains.
func NDCGAt(ranked []string, rel map[string]struct{}, k int) float64 {
	dcg := 0.0
	for i := 0; i < k && i < len(ranked); i++ {
		if isRel(rel, ranked[i]) {
			dcg += 1.0 / math.Log2(float64(i)+2.0)
		}
	}
	idcg := 0.0
	for i := 0; i < k && i < len(rel); i++ {
		idcg += 1.0 / math.Log2(float64(i)+2.0)
	}
	if idcg == 0 {
		return 0
	}
	return dcg / idcg
}

// AveragePrecision returns the average of the precision values computed at
// each rank where a relevant document is retrieved, divided by the total
// number of relevant documents (the standard AP that MAP averages).
//
// It has no cutoff argument because it scores the whole of ranked. That makes
// the caller's retrieval depth its effective cutoff — see Cutoffs.Depth for
// why the reported label has to say so.
func AveragePrecision(ranked []string, rel map[string]struct{}) float64 {
	if len(rel) == 0 {
		return 0
	}
	hit := 0
	sum := 0.0
	for i, d := range ranked {
		if isRel(rel, d) {
			hit++
			sum += float64(hit) / float64(i+1)
		}
	}
	return sum / float64(len(rel))
}

// ReciprocalRank returns 1/rank of the first relevant hit, or 0 if none. Like
// AveragePrecision it scores the whole of ranked, so the caller's retrieval
// depth is its effective cutoff.
func ReciprocalRank(ranked []string, rel map[string]struct{}) float64 {
	for i, d := range ranked {
		if isRel(rel, d) {
			return 1.0 / float64(i+1)
		}
	}
	return 0
}

// Aggregate accumulates per-query Scores and reports their mean (macro-average
// over queries, the standard way MAP/mean-nDCG are reported).
type Aggregate struct {
	N         int
	sumP      float64
	sumNDCG   float64
	sumRecall float64
	sumMAP    float64
	sumMRR    float64
}

// Add folds one query's scores into the running totals.
func (a *Aggregate) Add(s Scores) {
	a.N++
	a.sumP += s.P
	a.sumNDCG += s.NDCG
	a.sumRecall += s.Recall
	a.sumMAP += s.MAP
	a.sumMRR += s.MRR
}

// Mean returns the per-query average of every metric (zero value if N == 0).
func (a *Aggregate) Mean() Scores {
	if a.N == 0 {
		return Scores{}
	}
	n := float64(a.N)
	return Scores{
		P:      a.sumP / n,
		NDCG:   a.sumNDCG / n,
		Recall: a.sumRecall / n,
		MAP:    a.sumMAP / n,
		MRR:    a.sumMRR / n,
	}
}
