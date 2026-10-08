package meetingcontent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenericAuthoritativeCallDuration(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want *float64
	}{
		{"zero", `{"duration_seconds":0}`, new(float64(0))},
		{"precedence", `{"duration_seconds":45,"started_at":"2026-10-03T10:00:00Z","ended_at":"2026-10-03T10:01:00Z"}`, new(float64(45))},
		{"null", `{"duration_seconds":null}`, nil},
		{"negative", `{"duration_seconds":-1}`, nil},
		{"nonfinite", `{"duration_seconds":"Infinity"}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			content := Decode("meeting_json", []byte(tc.raw), nil)
			if tc.want == nil {
				assert.Nil(content.DurationSeconds)
				return
			}
			require.NotNil(content.DurationSeconds)
			assert.InDelta(*tc.want, *content.DurationSeconds, 1e-9)
			assert.Equal(DurationProvider, content.DurationBasis)
		})
	}
}
