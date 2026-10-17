package whoknows

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

// BenchmarkWhoKnows measures ranking, excluding query embedding and retrieval.
// Run explicitly with -run=NONE -bench=WhoKnows. The generated population is
// 100,000 messages across 5,000 people; the retrieval boundary admits 500 people.
func BenchmarkWhoKnows(b *testing.B) {
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	population := make([]Candidate, 5000)
	for i := range population {
		population[i] = Candidate{PersonID: int64(i + 1)}
		for j := range 20 {
			at := now.Add(-time.Duration((i+j)%900) * 24 * time.Hour)
			population[i].Evidence = append(population[i].Evidence, Evidence{MessageID: int64(i*20 + j + 1), ConversationID: int64(j + 1), MessageType: "email", Channel: "email", Role: "from", SentAt: &at, Relevance: 1, VectorScore: new(.9)})
		}
	}
	for _, window := range []bool{false, true} {
		b.Run(fmt.Sprintf("date_window=%t", window), func(b *testing.B) {
			p := DefaultParams()
			if window {
				after := now.Add(-180 * 24 * time.Hour)
				p.After = &after
			}
			candidates := append([]Candidate{}, population[:500]...)
			if window {
				for i := range candidates {
					evidence := []Evidence{}
					for _, e := range candidates[i].Evidence {
						if !e.SentAt.Before(*p.After) {
							evidence = append(evidence, e)
						}
					}
					candidates[i].Evidence = evidence
				}
			}
			samples := []float64{}
			b.ResetTimer()
			for b.Loop() {
				start := time.Now()
				Rank(candidates, p, now)
				samples = append(samples, float64(time.Since(start).Microseconds()))
			}
			b.StopTimer()
			slices.Sort(samples)
			if len(samples) > 0 {
				b.ReportMetric(samples[len(samples)/2], "p50-us")
				b.ReportMetric(samples[min(len(samples)-1, int(float64(len(samples))*.95))], "p95-us")
			}
		})
	}
}
