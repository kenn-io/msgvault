package personobs

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/personfacts"
)

func TestClampStability(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := now.AddDate(0, 0, 30)
	boundary := now.AddDate(0, 0, 183)
	volatile := StabilityVolatile
	fixed := StabilityFixed
	for _, tt := range []struct {
		name        string
		level       Level
		proposed    Stability
		def         *Stability
		from, until *time.Time
		occurs      *personfacts.PartialDateValue
		want        Stability
	}{
		{"definition", LevelExplicit, StabilityDurable, &volatile, nil, nil, nil, StabilityVolatile},
		{"short", LevelExplicit, StabilityDurable, nil, &now, &end, nil, StabilityVolatile},
		{"boundary", LevelExplicit, StabilityDurable, nil, &now, &boundary, nil, StabilityDurable},
		{"event", LevelExplicit, StabilityDurable, nil, nil, nil, &personfacts.PartialDateValue{Year: 2027}, StabilityEpisodic},
		{"inductive", LevelInductive, StabilityFixed, nil, nil, nil, nil, StabilityDurable},
		{"keep volatile", LevelExplicit, StabilityEpisodic, &fixed, nil, nil, nil, StabilityEpisodic},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ClampStability(tt.level, tt.proposed, tt.def, tt.from, tt.until, tt.occurs))
		})
	}
}
