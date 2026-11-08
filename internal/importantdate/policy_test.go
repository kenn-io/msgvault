package importantdate

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCadenceRequiresExplicitMapping(t *testing.T) {
	mapping := map[int]Tier{21: TierA, 60: TierB, 180: TierC, 365: TierD}
	for days, want := range mapping {
		got, ok := TierForCadence(days, nil)
		assert.False(t, ok)
		assert.Empty(t, got)
		got, ok = TierForCadence(days, mapping)
		assert.True(t, ok)
		assert.Equal(t, want, got)
	}
	_, ok := TierForCadence(30, mapping)
	assert.False(t, ok)
	_, ok = TierForCadence(0, mapping)
	assert.False(t, ok)
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
			got := ResolveEligibility(tt.subject, Eligibility{Enabled: true, Tier: TierC}, parent)
			assert.Equal(t, tt.want, got)
		})
	}
}
