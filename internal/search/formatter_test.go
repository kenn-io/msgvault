package search

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatRoundTripsRepresentableQueryFields(t *testing.T) {
	hasAttachment := true
	after := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	before := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	larger := int64(1024)
	smaller := int64(4096)
	want := Query{
		TextTerms:     []string{"plain", "meeting notes", `quoted "phrase"`, `path\segment`, "subject:not-an-operator"},
		FromAddrs:     []string{"alice@example.com", "sender name"},
		ToAddrs:       []string{"bob@example.com"},
		CcAddrs:       []string{"carol@example.com"},
		BccAddrs:      []string{"archive@example.com"},
		SubjectTerms:  []string{"project update"},
		Labels:        []string{`Important "Review"`},
		HasAttachment: &hasAttachment,
		BeforeDate:    &before,
		AfterDate:     &after,
		LargerThan:    &larger,
		SmallerThan:   &smaller,
		MessageTypes:  []string{"meeting_transcript", "sms"},
	}

	formatted := Format(&want)
	got := Parse(formatted)

	require.NoError(t, got.Err(), "formatted query parses")
	assertQueryEqual(t, *got, want)
}

func TestFormatNilQuery(t *testing.T) {
	assert.Empty(t, Format(nil))
}

func TestFormatRoundTripsExactDateTimes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	after := time.Date(2024, 2, 1, 10, 30, 15, 123456789,
		time.FixedZone("UTC-5", -5*60*60))
	before := time.Date(2024, 3, 1, 0, 0, 0, 0,
		time.FixedZone("UTC+2", 2*60*60))

	formatted := Format(&Query{AfterDate: &after, BeforeDate: &before})
	assert.Contains(formatted, "after:2024-02-01T10:30:15.123456789-05:00")
	assert.Contains(formatted, "before:2024-03-01T00:00:00+02:00")

	parsed := Parse(formatted)
	require.NoError(parsed.Err(), "formatted exact dates parse")
	require.NotNil(parsed.AfterDate)
	require.NotNil(parsed.BeforeDate)
	assert.True(after.Equal(*parsed.AfterDate), "after instant")
	assert.True(before.Equal(*parsed.BeforeDate), "before instant")
	_, wantAfterOffset := after.Zone()
	_, gotAfterOffset := parsed.AfterDate.Zone()
	_, wantBeforeOffset := before.Zone()
	_, gotBeforeOffset := parsed.BeforeDate.Zone()
	assert.Equal(wantAfterOffset, gotAfterOffset, "after timezone offset")
	assert.Equal(wantBeforeOffset, gotBeforeOffset, "before timezone offset")
}

func TestFormatUsesDateOnlyForExactUTCMidnight(t *testing.T) {
	midnightUTC := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	nonMidnightUTC := time.Date(2024, 3, 1, 0, 0, 0, 1, time.UTC)

	formatted := Format(&Query{AfterDate: &midnightUTC, BeforeDate: &nonMidnightUTC})
	assert.Contains(t, formatted, "after:2024-02-01")
	assert.Contains(t, formatted, "before:2024-03-01T00:00:00.000000001Z")
}
