package timeline

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShortRefRoundTrip(t *testing.T) {
	for _, tc := range []struct{ full, short string }{{"evt:8812@20261022T133000Z", "e8812@20261022T1330Z"}, {"evt:8812@20261022", "e8812@20261022"}, {"evt:8812", "e8812"}, {"msg:9031", "#9031"}, {"conv:412", "c412"}, {"evt:8812@20261022T133027Z", "e8812@20261022T133027Z"}} {
		assert.Equal(t, tc.short, FormatShortRef(tc.full))
		assert.Equal(t, tc.full, ExpandShortRef(tc.short))
	}
}
func TestParseAnchorForms(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	loc := time.FixedZone("archive", -4*3600)
	for _, tc := range []struct{ s, kind, ref string }{{"#9031", "message", "msg:9031"}, {"conv:412", "conversation", "conv:412"}, {"c412", "conversation", "conv:412"}, {"burst:1:412:2026-10-22", "burst", "burst:1:412:2026-10-22"}, {"e8812@20261022T1330Z", "meeting", "evt:8812@20261022T133000Z"}, {"evt:8812@20261022", "meeting", "evt:8812@20261022"}, {"evt:8812", "meeting", "evt:8812"}, {"2026-10-22", "date", "2026-10-22"}, {"2026-10-22T09:30:00", "datetime", "2026-10-22T09:30:00"}, {"2026-10-22T13:30:00Z", "datetime", "2026-10-22T13:30:00Z"}} {
		a, err := ParseAnchor(tc.s, loc)
		requirements.NoError(err, tc.s)
		assertions.Equal(tc.kind, a.Kind)
		assertions.Equal(tc.ref, a.Ref)
		if tc.kind == "date" {
			assertions.Equal(4, a.Time.UTC().Hour())
		}
	}
	santiago, err := time.LoadLocation("America/Santiago")
	requirements.NoError(err)
	date, err := ParseAnchor("2026-09-06", santiago)
	requirements.NoError(err)
	assertions.Equal("2026-09-06T01:00:00-03:00", date.Time.Format(time.RFC3339))
	for _, s := range []string{"msg:0", "msg:-1", "conv:", "e1@bad", "evt:1@20260230", "burst:1:2:bad", "nope"} {
		_, err := ParseAnchor(s, loc)
		requirements.ErrorIs(err, ErrUnknownReference, s)
	}
}

func TestParseAnchorRejectsSkippedCivilDate(t *testing.T) {
	loc, err := time.LoadLocation("Pacific/Apia")
	require.NoError(t, err)
	_, err = ParseAnchor("2011-12-30", loc)
	require.ErrorIs(t, err, ErrInvalidRequest)
	_, err = ParseAnchor("2011-12-31", loc)
	require.NoError(t, err)
}

func TestParseAnchorRejectsNonexistentLocalDatetime(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	for _, value := range []string{"2026-03-08T02:30", "2026-03-08T02:30:00", "2026-03-08T02:30:00.123"} {
		_, err := ParseAnchor(value, loc)
		require.ErrorIs(t, err, ErrInvalidRequest, value)
	}
	for _, value := range []string{"2026-03-08T03:30", "2026-03-08T02:30:00-05:00", "2026-11-01T01:30"} {
		_, err := ParseAnchor(value, loc)
		require.NoError(t, err, value)
	}
}
