package calrecur

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestParseInstanceID(t *testing.T) {
	for _, tc := range []struct{ input, base, key string }{
		{"series_with_underscores_20270314T100000Z", "series_with_underscores", "20270314T100000Z"},
		{"series_20270314", "series", "20270314"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			base, id, ok := ParseInstanceID(tc.input)
			require.True(t, ok)
			assert.Equal(t, tc.base, base)
			assert.Equal(t, tc.key, id.Key())
		})
	}
	for _, input := range []string{"series", "_20270314", "series_20270230", "series_20270314T100000+00:00"} {
		t.Run(input, func(t *testing.T) { _, _, ok := ParseInstanceID(input); assert.False(t, ok) })
	}
}
