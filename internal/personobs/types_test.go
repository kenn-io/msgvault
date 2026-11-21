package personobs

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/personfacts"
)

func TestDirectnessFor(t *testing.T) {
	for _, tt := range []struct {
		level       Level
		origin      Origin
		attribution Attribution
		want        personfacts.EvidenceDirectness
	}{
		{LevelExplicit, OriginExtraction, AttributionSelf, personfacts.DirectSelf}, {LevelExplicit, OriginOwner, "", personfacts.DirectOther}, {LevelDeductive, OriginReasoning, "", personfacts.Indirect},
	} {
		assert.Equal(t, tt.want, DirectnessFor(tt.level, tt.origin, tt.attribution))
	}
}
