package api

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"net/url"
	"testing"
	"time"
)

func TestParsePersonCardOptions(t *testing.T) {
	assertions := assert.New(t)
	now := time.Date(2026, 10, 17, 14, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ raw, code string }{
		{"max_items=0", "invalid_max_items"}, {"max_items=11", "invalid_max_items"}, {"max_items=", "invalid_max_items"},
		{"max_bytes=2047", "invalid_max_bytes"}, {"max_bytes=65537", "invalid_max_bytes"},
		{"format=html", "invalid_format"}, {"affiliation=other", "invalid_affiliation"}, {"as_of=wrong", "invalid_as_of"},
		{"as_of=2015-01-01T00:00:00Z", "invalid_as_of"}, {"as_of=2028-01-01T00:00:00Z", "invalid_as_of"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			q, err := url.ParseQuery(tc.raw)
			require.NoError(t, err)
			_, code := parsePersonCardOptions(q, now)
			assert.Equal(t, tc.code, code)
		})
	}
	opts, code := parsePersonCardOptions(url.Values{}, now)
	require.Empty(t, code)
	assertions.Equal(3, opts.MaxItems)
	assertions.Equal(16384, opts.MaxBytes)
	assertions.Equal("json", string(opts.Format))
	assertions.Equal(now, opts.AsOf)
}
