package activity

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func at(value string) *time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return &parsed
}
func TestDateEventLocalDateAcrossZones(t *testing.T) {
	tests := []struct {
		name          string
		instant       string
		zone          string
		wantLocalDate string
		wantOffset    int
		wantZone      string
	}{
		{
			name:          "utc midday stays on the same day",
			instant:       "2026-07-30T12:00:00Z",
			zone:          "UTC",
			wantLocalDate: "2026-07-30",
			wantOffset:    0,
			wantZone:      "UTC",
		},
		{
			name:          "new york daylight time falls back a day",
			instant:       "2026-07-30T02:30:00Z",
			zone:          "America/New_York",
			wantLocalDate: "2026-07-29",
			wantOffset:    -240,
			wantZone:      "America/New_York",
		},
		{
			name:          "new york standard time falls back a day",
			instant:       "2026-01-15T04:30:00Z",
			zone:          "America/New_York",
			wantLocalDate: "2026-01-14",
			wantOffset:    -300,
			wantZone:      "America/New_York",
		},
		{
			name:          "kiribati plus fourteen advances a day",
			instant:       "2026-07-30T11:00:00Z",
			zone:          "Pacific/Kiritimati",
			wantLocalDate: "2026-07-31",
			wantOffset:    840,
			wantZone:      "Pacific/Kiritimati",
		},
		{
			name:          "kiribati just before its midnight stays on the day",
			instant:       "2026-07-30T09:59:59Z",
			zone:          "Pacific/Kiritimati",
			wantLocalDate: "2026-07-30",
			wantOffset:    840,
			wantZone:      "Pacific/Kiritimati",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			check := assert.New(t)
			must := require.New(t)
			zone, err := LoadZone(test.zone)
			must.NoError(err)

			got, err := DateEvent(Timestamps{SentAt: at(test.instant)}, zone)
			must.NoError(err)
			check.Equal(test.wantLocalDate, got.LocalDate)
			check.Equal(test.wantOffset, got.UTCOffsetMinutes)
			check.Equal(test.wantZone, got.Timezone)
			check.Equal(PrecisionTimestamp, got.Precision)
			check.Equal(OriginSentAt, got.Origin)
			check.Equal(test.instant, got.OccurredAt.Format(time.RFC3339))
		})
	}
}

func TestDateEventPrefersSentThenReceivedThenInternal(t *testing.T) {
	tests := []struct {
		name       string
		timestamps Timestamps
		wantOrigin DateOrigin
		wantDate   string
	}{
		{
			name: "sent wins over the others",
			timestamps: Timestamps{
				SentAt:       at("2026-07-30T12:00:00Z"),
				ReceivedAt:   at("2026-07-31T12:00:00Z"),
				InternalDate: at("2026-08-01T12:00:00Z"),
			},
			wantOrigin: OriginSentAt,
			wantDate:   "2026-07-30",
		},
		{
			name: "received wins when sent is absent",
			timestamps: Timestamps{
				ReceivedAt:   at("2026-07-31T12:00:00Z"),
				InternalDate: at("2026-08-01T12:00:00Z"),
			},
			wantOrigin: OriginReceivedAt,
			wantDate:   "2026-07-31",
		},
		{
			name:       "internal date is the last resort",
			timestamps: Timestamps{InternalDate: at("2026-08-01T12:00:00Z")},
			wantOrigin: OriginInternalDate,
			wantDate:   "2026-08-01",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := DateEvent(test.timestamps, time.UTC)
			require.NoError(t, err)
			assert.Equal(t, test.wantOrigin, got.Origin)
			assert.Equal(t, test.wantDate, got.LocalDate)
		})
	}
}

func TestDateEventKeepsExplicitDayPrecisionInUTC(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	zone, err := LoadZone("Pacific/Kiritimati")
	must.NoError(err)

	got, err := DateEvent(Timestamps{
		SentAt:   at("2026-07-30T00:00:00Z"),
		DateOnly: true,
	}, zone)
	must.NoError(err)
	check.Equal(PrecisionDay, got.Precision)
	check.Equal("2026-07-30", got.LocalDate)
	check.Equal(0, got.UTCOffsetMinutes)
	check.Equal("UTC", got.Timezone)
}

func TestDateEventTreatsOrdinaryMidnightAsTimestamp(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	zone, err := LoadZone("America/New_York")
	must.NoError(err)

	got, err := DateEvent(Timestamps{SentAt: at("2026-07-30T00:00:00Z")}, zone)
	must.NoError(err)
	check.Equal(PrecisionTimestamp, got.Precision)
	check.Equal("2026-07-29", got.LocalDate)
	check.Equal(-240, got.UTCOffsetMinutes)
	check.Equal("America/New_York", got.Timezone)
}

func TestDateEventNormalizesNonUTCInput(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	zone, err := LoadZone("America/New_York")
	must.NoError(err)
	local := time.Date(2026, 7, 29, 22, 30, 0, 0, zone)

	got, err := DateEvent(Timestamps{SentAt: &local}, zone)
	must.NoError(err)
	check.Equal(time.UTC, got.OccurredAt.Location())
	check.Equal("2026-07-30T02:30:00Z", got.OccurredAt.Format(time.RFC3339))
	check.Equal("2026-07-29", got.LocalDate)
}

func TestDateEventRejectsMissingTimestamps(t *testing.T) {
	_, err := DateEvent(Timestamps{}, time.UTC)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoTimestamp)
}

func TestDateEventRejectsZeroTimestamps(t *testing.T) {
	zero := time.Time{}
	_, err := DateEvent(Timestamps{SentAt: &zero}, time.UTC)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoTimestamp)
}

func TestDateEventDefaultsNilZoneToUTC(t *testing.T) {
	got, err := DateEvent(Timestamps{SentAt: at("2026-07-30T23:30:00Z")}, nil)
	require.NoError(t, err)
	assert.Equal(t, "UTC", got.Timezone)
	assert.Equal(t, "2026-07-30", got.LocalDate)
}

func TestLoadZoneRejectsUnknownName(t *testing.T) {
	_, err := LoadZone("Not/AZone")
	require.Error(t, err)
	assert.ErrorContains(t, err, "Not/AZone")
}

func TestLoadZoneTreatsEmptyNameAsUTC(t *testing.T) {
	zone, err := LoadZone("")
	require.NoError(t, err)
	assert.Equal(t, time.UTC, zone)
}
