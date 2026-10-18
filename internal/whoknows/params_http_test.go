package whoknows

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"net/url"
	"testing"
	"time"
)

func TestParseHTTPParams(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	topic, p, err := ParseHTTPParams(url.Values{"q": {"cloud networking"}, "evidence": {"0"}, "half_life_days": {"0"}, "include_unsaved": {"false"}, "before": {"2026-10-17"}})
	must.NoError(err)
	check.Equal("cloud networking", topic)
	check.Zero(p.Evidence)
	check.Zero(p.HalfLifeDays)
	check.False(p.IncludeUnsaved)
	check.Equal(time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC), *p.Before)
	for _, query := range []string{"q=cloud&limit=0", "q=cloud&evidence=11", "q=cloud&include_owner=bad", "q=cloud&min_score=NaN", "q=cloud&min_profile_score=2", "q=cloud&tiebreak=other", "q=cloud&after=2026-10-18&before=2026-10-17", "q=cloud&limit=3&limit=4"} {
		v, err := url.ParseQuery(query)
		must.NoError(err)
		_, _, err = ParseHTTPParams(v)
		must.Error(err)
	}
	for _, topic := range []string{"", "  ", "from:someone@example.net"} {
		_, _, err := ParseHTTPParams(url.Values{"q": {topic}})
		must.Error(err)
	}
}
