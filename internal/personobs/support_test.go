package personobs

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestPremiseSupported(t *testing.T) {
	now := time.Now()
	for _, tt := range []struct {
		name string
		o    Observation
		want bool
	}{
		{"owner", Observation{Level: LevelExplicit, Origin: OriginOwner, Status: StatusActive}, true},
		{"empty extraction", Observation{Level: LevelExplicit, Origin: OriginExtraction, Status: StatusActive}, false},
		{"supported extraction", Observation{Level: LevelExplicit, Origin: OriginExtraction, Status: StatusActive, SupportedCount: 1}, true},
		{"lapsed", Observation{Level: LevelExplicit, Origin: OriginOwner, Status: StatusActive, SupportFlag: SupportFlagLapsed}, false},
		{"contested", Observation{Level: LevelDeductive, Status: StatusActive, SupportFlag: SupportFlagContested}, false},
		{"derived", Observation{Level: LevelDeductive, Status: StatusActive, PremiseChangedAt: &now}, true},
		{"rejected owner", Observation{Level: LevelExplicit, Origin: OriginOwner, Status: StatusRejected}, false},
	} {
		t.Run(tt.name, func(t *testing.T) { assert.Equal(t, tt.want, PremiseSupported(tt.o)) })
	}
}
