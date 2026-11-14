package personobs

import (
	"time"

	"go.kenn.io/msgvault/internal/personfacts"
)

// ClampStability takes the most volatile applicable bound.
func ClampStability(level Level, proposed Stability, def *Stability, validFrom, validUntil *time.Time, occursOn *personfacts.PartialDateValue) Stability {
	rank := map[Stability]int{StabilityFixed: 0, StabilityDurable: 1, StabilityVolatile: 2, StabilityEpisodic: 3}
	if _, ok := rank[proposed]; !ok {
		proposed = StabilityDurable
	}
	if def != nil && rank[*def] > rank[proposed] {
		proposed = *def
	}
	if validFrom != nil && validUntil != nil && validUntil.Sub(*validFrom) < 183*24*time.Hour && rank[proposed] < rank[StabilityVolatile] {
		proposed = StabilityVolatile
	}
	if occursOn != nil && validFrom == nil && validUntil == nil {
		proposed = StabilityEpisodic
	}
	if level == LevelInductive && proposed == StabilityFixed {
		proposed = StabilityDurable
	}
	return proposed
}
