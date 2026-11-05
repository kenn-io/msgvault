package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTimelineStepsOutsideDefaultWindow(t *testing.T) {
	for _, tc := range []struct {
		key  rune
		date string
	}{{'[', "2027-03-12"}, {']', "2027-03-15"}} {
		t.Run(string(tc.key), func(t *testing.T) {
			m := timelineModeModel()
			m.timelineState.result.Window.From = "2027-03-13"
			updated, cmd := sendKey(t, m, key(tc.key))
			require.NotNil(t, cmd)
			assert.Equal(t, tc.date, updated.timelineState.request.Date)
			updated.cancelTimelineRequest()
		})
	}
}
