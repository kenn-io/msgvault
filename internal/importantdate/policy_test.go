package importantdate

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCadenceRequiresExplicitMapping(t *testing.T) {
	checks := assert.New(t)
	mapping := map[int]Tier{21: TierA, 60: TierB, 180: TierC, 365: TierD}
	for days, want := range mapping {
		got, ok := TierForCadence(days, nil)
		checks.False(ok)
		checks.Empty(got)
		got, ok = TierForCadence(days, mapping)
		checks.True(ok)
		checks.Equal(want, got)
	}
	_, ok := TierForCadence(30, mapping)
	checks.False(ok)
	_, ok = TierForCadence(0, mapping)
	checks.False(ok)
}

func TestSubjectExclusionOverridesParent(t *testing.T) {
	parent := Eligibility{Enabled: true, Tier: TierA}
	tests := []struct {
		name    string
		subject SubjectPreference
		want    Eligibility
	}{
		{"excluded", SubjectExclude, Eligibility{}},
		{"inherit", SubjectInherit, parent},
		{"direct", SubjectInclude, Eligibility{Enabled: true, Tier: TierC}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checks := assert.New(t)
			got := ResolveEligibility(tt.subject, Eligibility{Enabled: true, Tier: TierC}, parent)
			checks.Equal(tt.want, got)
		})
	}
}
