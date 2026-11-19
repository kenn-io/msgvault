package contactcadence_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/contactcadence"
)

func TestPresets(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for list, want := range map[string]int64{"A": 21, "B": 60, "C": 180, "D": 365} {
		days, ok := contactcadence.DaysForList(list)
		assert.True(ok)
		assert.Equal(want, days)
		assert.Equal(list, contactcadence.ListForDays(days))
	}
	_, ok := contactcadence.DaysForList("E")
	assert.False(ok)
	assert.Empty(contactcadence.ListForDays(30))
	presets := contactcadence.Presets()
	require.Len(presets, 4)
	presets[0].Days = 999
	assert.Equal(int64(21), contactcadence.Presets()[0].Days)
}

func FuzzPresetRoundTrip(f *testing.F) {
	for _, days := range []int64{-1, 0, 21, 60, 180, 365, 366} {
		f.Add(days)
	}
	f.Fuzz(func(t *testing.T, days int64) {
		list := contactcadence.ListForDays(days)
		if list == "" {
			assert.NotContains(t, []int64{21, 60, 180, 365}, days)
			return
		}
		got, ok := contactcadence.DaysForList(list)
		assert.True(t, ok)
		assert.Equal(t, days, got)
	})
}

// List spellings accept surrounding whitespace and ASCII case variations;
// every other name must be rejected instead of silently selecting a list.
func FuzzPresetNames(f *testing.F) {
	for _, name := range []string{"A", " b ", "c", "D", "AB", "", "Ａ", "\x00A"} {
		f.Add(name)
	}
	f.Fuzz(func(t *testing.T, name string) {
		got, ok := contactcadence.DaysForList(name)
		want, accepted := int64(0), false
		for list, days := range map[string]int64{"A": 21, "B": 60, "C": 180, "D": 365} {
			if strings.EqualFold(strings.TrimSpace(name), list) {
				want, accepted = days, true
			}
		}
		assert.Equal(t, accepted, ok)
		assert.Equal(t, want, got)
	})
}
