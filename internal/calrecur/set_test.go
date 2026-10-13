package calrecur

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func instant(t *testing.T, value string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, value)
	require.NoError(t, err)
	return v
}

func parsed(t *testing.T, lines ...string) Set {
	t.Helper()
	s, err := ParseSet(instant(t, "2027-03-02T17:00:00Z"), false, "", time.UTC, lines)
	require.NoError(t, err)
	return s
}

func TestParseSetRejectsMixedWeekdayOrdinals(t *testing.T) {
	for _, line := range []string{
		"RRULE:FREQ=MONTHLY;BYDAY=MO,1MO;COUNT=3",
		"RRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=MO,1MO;COUNT=3",
	} {
		t.Run(line, func(t *testing.T) {
			_, err := ParseSet(instant(t, "2027-03-01T17:00:00Z"), false, "", time.UTC, []string{line})
			require.ErrorIs(t, err, ErrUnsupportedRecurrence)
			assert.Contains(t, err.Error(), line)
		})
	}
}

func keys(t *testing.T, s Set) []string {
	t.Helper()
	var got []string
	require.NoError(t, Iter(t.Context(), s, instant(t, "2028-01-01T00:00:00Z"), 100, func(i Instance) bool {
		got = append(got, i.Identity.Key())
		return true
	}))
	return got
}

func TestIterMultipleRRulesUnion(t *testing.T) {
	s := parsed(t, "RRULE:FREQ=DAILY;COUNT=3", "RRULE:FREQ=DAILY;INTERVAL=2;COUNT=3")
	assert.Equal(t, []string{"20270302T170000Z", "20270303T170000Z", "20270304T170000Z", "20270306T170000Z"}, keys(t, s))
}

func TestIterExDateForms(t *testing.T) {
	for _, line := range []string{"EXDATE:20270303T170000Z", "EXDATE;TZID=America/New_York:20270303T120000"} {
		t.Run(line, func(t *testing.T) {
			assert.Equal(t, []string{"20270302T170000Z", "20270304T170000Z"}, keys(t, parsed(t, "RRULE:FREQ=DAILY;COUNT=3", line)))
		})
	}
	zone, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	s, err := ParseSet(time.Time{}, true, "2027-03-02", zone, []string{"RRULE:FREQ=DAILY;COUNT=3", "EXDATE;VALUE=DATE:20270303"})
	require.NoError(t, err)
	assert.Equal(t, []string{"20270302", "20270304"}, keys(t, s))
}

func TestIterRDateAddsAndDedupes(t *testing.T) {
	assert.Equal(t, []string{"20270301T170000Z", "20270302T170000Z", "20270303T170000Z", "20270305T170000Z"},
		keys(t, parsed(t, "RRULE:FREQ=DAILY;COUNT=2", "RDATE:20270305T170000Z,20270302T170000Z,20270301T170000Z")))
}

func TestTimedDateTimeRDateAndExDateUseFirstFoldOccurrence(t *testing.T) {
	requirements := require.New(t)
	zone, err := time.LoadLocation("Europe/Berlin")
	requirements.NoError(err)
	start := instant(t, "2027-10-30T00:30:00Z")

	s, err := ParseSet(start, false, "", zone, []string{
		"RRULE:FREQ=DAILY;COUNT=3",
		"EXDATE;TZID=Europe/Berlin:20271031T023000",
	})
	requirements.NoError(err)
	assert.Equal(t, []string{"20271030T003000Z", "20271101T013000Z"}, keys(t, s))

	s, err = ParseSet(start, false, "", zone, []string{"RDATE;TZID=Europe/Berlin:20271031T023000"})
	requirements.NoError(err)
	assert.Equal(t, []string{"20271030T003000Z", "20271031T003000Z"}, keys(t, s))
}

func TestIterBudgetCountsUnderlyingCandidates(t *testing.T) {
	s, err := ParseSet(instant(t, "1997-03-02T17:00:00Z"), false, "", time.UTC, []string{"RRULE:FREQ=DAILY"})
	require.NoError(t, err)
	got, err := Expand(t.Context(), s, time.Hour, 0, nil, instant(t, "2027-03-02T00:00:00Z"), instant(t, "2027-03-03T00:00:00Z"), 1000)
	require.ErrorIs(t, err, ErrBudgetExceeded)
	assert.Empty(t, got)
}

func TestIterHonorsContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	s := parsed(t, "RRULE:FREQ=DAILY")
	count := 0
	err := Iter(ctx, s, instant(t, "2028-01-01T00:00:00Z"), 100, func(i Instance) bool { count++; cancel(); return true })
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, count)
	require.ErrorIs(t, Iter(ctx, s, instant(t, "2028-01-01T00:00:00Z"), 100, func(i Instance) bool { return true }), context.Canceled)
}

func TestAllDayIdentityCivilDate(t *testing.T) {
	for _, zoneName := range []string{"America/Los_Angeles", "Asia/Tokyo"} {
		t.Run(zoneName, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			zone, err := time.LoadLocation(zoneName)
			requirements.NoError(err)
			s, err := ParseSet(instant(t, "2027-03-07T00:00:00Z"), true, "2027-03-07", zone, []string{"RRULE:FREQ=WEEKLY;COUNT=3"})
			requirements.NoError(err)
			var dates []string
			requirements.NoError(Iter(t.Context(), s, instant(t, "2028-01-01T00:00:00Z"), 100, func(i Instance) bool {
				assertions.True(i.Identity.AllDay)
				assertions.Equal(0, i.Start.Hour())
				assertions.Equal(zone, i.Start.Location())
				assertions.Equal(i.Identity.Date, i.Start.Format("2006-01-02"))
				dates = append(dates, i.Identity.Date)
				return true
			}))
			assertions.Equal([]string{"2027-03-07", "2027-03-14", "2027-03-21"}, dates)
		})
	}
}

func TestAllDayIdentityPreservesCivilDateAtMidnightDSTTransition(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	zone, err := time.LoadLocation("America/Sao_Paulo")
	requirements.NoError(err)
	s, err := ParseSet(time.Time{}, true, "2018-11-04", zone, []string{"RRULE:FREQ=DAILY;COUNT=2"})
	requirements.NoError(err)
	var instances []Instance
	err = Iter(t.Context(), s, time.Date(2019, 1, 1, 0, 0, 0, 0, zone), 100, func(i Instance) bool {
		instances = append(instances, i)
		return true
	})
	requirements.NoError(err)
	dates := make([]string, 0, len(instances))
	for _, instance := range instances {
		assertions.Equal(instance.Identity.Date, instance.Start.In(zone).Format(dateLayout))
		dates = append(dates, instance.Identity.Date)
	}
	assertions.Equal([]string{"2018-11-04", "2018-11-05"}, dates)
	assertions.Equal(time.Date(2018, 11, 4, 1, 0, 0, 0, zone), instances[0].Start)

	found, err := Contains(t.Context(), s, nil, Identity{Date: "2018-11-04", AllDay: true}, 100)
	requirements.NoError(err)
	assertions.True(found)
}

func TestAllDayDateValuesPreserveCivilDateAtMidnightDSTTransition(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	zone, err := time.LoadLocation("America/Sao_Paulo")
	requirements.NoError(err)
	s, err := ParseSet(time.Time{}, true, "2018-11-03", zone,
		[]string{"RDATE;VALUE=DATE:20181104", "EXDATE;VALUE=DATE:20181104"})
	requirements.NoError(err)
	assertions.Equal([]Identity{{Date: "2018-11-04", AllDay: true}}, s.RDates)
	assertions.Equal([]Identity{{Date: "2018-11-04", AllDay: true}}, s.ExDates)
}

func TestAllDayRDateAtMidnightDSTTransitionIsNotDeduplicated(t *testing.T) {
	requirements := require.New(t)
	zone, err := time.LoadLocation("America/Sao_Paulo")
	requirements.NoError(err)
	s, err := ParseSet(time.Time{}, true, "2018-11-03", zone, []string{"RDATE;VALUE=DATE:20181104"})
	requirements.NoError(err)
	var dates []string
	err = Iter(t.Context(), s, time.Date(2018, 11, 6, 12, 0, 0, 0, zone), 100, func(i Instance) bool {
		dates = append(dates, i.Identity.Date)
		return true
	})
	requirements.NoError(err)
	assert.Equal(t, []string{"2018-11-03", "2018-11-04"}, dates)
}

func TestAllDayExDateAtMidnightDSTTransitionExcludesCivilDate(t *testing.T) {
	requirements := require.New(t)
	zone, err := time.LoadLocation("America/Sao_Paulo")
	requirements.NoError(err)
	s, err := ParseSet(time.Time{}, true, "2018-11-03", zone,
		[]string{"RRULE:FREQ=DAILY;COUNT=3", "EXDATE;VALUE=DATE:20181104"})
	requirements.NoError(err)
	var dates []string
	err = Iter(t.Context(), s, time.Date(2018, 11, 6, 12, 0, 0, 0, zone), 100, func(i Instance) bool {
		dates = append(dates, i.Identity.Date)
		return true
	})
	requirements.NoError(err)
	assert.Equal(t, []string{"2018-11-03", "2018-11-05"}, dates)
}

func TestConservativeEndAllDayAtMidnightDSTTransition(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	zone, err := time.LoadLocation("America/Sao_Paulo")
	requirements.NoError(err)
	s, err := ParseSet(time.Time{}, true, "2018-11-03", zone, nil)
	requirements.NoError(err)

	end, bounded := ConservativeEnd(t.Context(), s, 0, 1, 100)
	requirements.True(bounded)
	want := time.Date(2018, 11, 4, 1, 0, 0, 0, zone).Add(14 * time.Hour)
	assertions.Equal(want, end)
}

func TestIterSkipsExcludedSkippedCivilDate(t *testing.T) {
	requirements := require.New(t)
	zone, err := time.LoadLocation("Pacific/Apia")
	requirements.NoError(err)
	s, err := ParseSet(time.Time{}, true, "2011-12-29", zone,
		[]string{"RRULE:FREQ=DAILY;COUNT=3", "EXDATE;VALUE=DATE:20111230"})
	requirements.NoError(err)
	var dates []string
	err = Iter(t.Context(), s, time.Date(2012, 1, 2, 12, 0, 0, 0, zone), 100, func(i Instance) bool {
		dates = append(dates, i.Identity.Date)
		return true
	})
	requirements.NoError(err)
	assert.Equal(t, []string{"2011-12-29", "2011-12-31"}, dates)
}

func TestContainsAfterExcludedSkippedCivilDate(t *testing.T) {
	requirements := require.New(t)
	zone, err := time.LoadLocation("Pacific/Apia")
	requirements.NoError(err)
	s, err := ParseSet(time.Time{}, true, "2011-12-29", zone,
		[]string{"RRULE:FREQ=DAILY;COUNT=3", "EXDATE;VALUE=DATE:20111230"})
	requirements.NoError(err)
	found, err := Contains(t.Context(), s, nil, Identity{Date: "2011-12-31", AllDay: true}, 100)
	requirements.NoError(err)
	assert.True(t, found)
}

func TestContainsExcludedSkippedCivilDate(t *testing.T) {
	requirements := require.New(t)
	zone, err := time.LoadLocation("Pacific/Apia")
	requirements.NoError(err)
	s, err := ParseSet(time.Time{}, true, "2011-12-29", zone,
		[]string{"RRULE:FREQ=DAILY;COUNT=3", "EXDATE;VALUE=DATE:20111230"})
	requirements.NoError(err)
	found, err := Contains(t.Context(), s, nil, Identity{Date: "2011-12-30", AllDay: true}, 100)
	requirements.NoError(err)
	assert.False(t, found)
}

func TestParseSetUnsupportedLine(t *testing.T) {
	for _, line := range []string{"EXRULE:FREQ=DAILY", "RRULE:FREQ=INVALID", "RDATE:garbage", "EXDATE;TZID=Invalid/Zone:20270303T120000", "RDATE;VALUE=PERIOD:20270303T170000Z/PT1H"} {
		t.Run(line, func(t *testing.T) {
			_, err := ParseSet(instant(t, "2027-03-02T17:00:00Z"), false, "", time.UTC, []string{line})
			require.ErrorIs(t, err, ErrUnsupportedRecurrence)
			assert.Contains(t, err.Error(), line)
		})
	}
}

func TestInstanceIDRoundTrip(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	for _, value := range []string{"abc_20270302T170000Z", "abc_20270302", "base_with_underscores_20270302"} {
		base, id, ok := ParseInstanceID(value)
		requirements.True(ok)
		assertions.Equal(value, FormatInstanceID(base, id))
	}
	for _, value := range []string{"abc", "abc_x", "abc_2027", "_20270302", "abc_20270230", "abc_20270302T170000"} {
		_, _, ok := ParseInstanceID(value)
		assertions.False(ok, value)
	}
}

func TestParseSetRejectsSubsecondDTStart(t *testing.T) {
	dtstart := time.Date(2027, 3, 2, 17, 0, 0, 500_000_000, time.UTC)
	_, err := ParseSet(dtstart, false, "", time.UTC, nil)
	require.ErrorIs(t, err, ErrUnsupportedRecurrence)
}

func TestParseSetRejectsSubsecondRecurrenceDates(t *testing.T) {
	for _, line := range []string{"RDATE:20270302T170000.5Z", "EXDATE:20270302T170000.5Z"} {
		t.Run(line, func(t *testing.T) {
			_, err := ParseSet(instant(t, "2027-03-02T17:00:00Z"), false, "", time.UTC, []string{line})
			require.ErrorIs(t, err, ErrUnsupportedRecurrence)
		})
	}
}

func TestContainsRejectsSubsecondOriginalIdentity(t *testing.T) {
	original := Identity{Instant: time.Date(2027, 3, 2, 17, 0, 0, 500_000_000, time.UTC)}
	found, err := Contains(t.Context(), parsed(t, "RRULE:FREQ=DAILY;COUNT=1"), nil, original, 100)
	require.ErrorIs(t, err, ErrUnsupportedRecurrence)
	assert.False(t, found)
}

func TestConservativeEndCountUntilRdate(t *testing.T) {
	assertions := assert.New(t)
	for _, rule := range []string{"RRULE:FREQ=DAILY;COUNT=3", "RRULE:FREQ=DAILY;UNTIL=20270304T170000Z"} {
		s := parsed(t, rule, "RDATE:20270310T170000Z", "EXDATE:20270310T170000Z")
		end, bounded := ConservativeEnd(t.Context(), s, time.Hour, 0, 100)
		assertions.True(bounded)
		assertions.Equal(instant(t, "2027-03-10T18:00:00Z"), end)
	}
	for _, s := range []Set{parsed(t, "RRULE:FREQ=DAILY"), {DTStart: instant(t, "2027-03-02T17:00:00Z"), RRules: []string{"RRULE:invalid"}}} {
		_, bounded := ConservativeEnd(t.Context(), s, time.Hour, 0, 100)
		assertions.False(bounded)
	}
	_, bounded := ConservativeEnd(t.Context(), parsed(t, "RRULE:FREQ=DAILY;COUNT=101"), time.Hour, 0, 100)
	assertions.False(bounded)
}

func TestIterBudgetAndEarlyStop(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s := parsed(t, "RRULE:FREQ=DAILY;COUNT=2", "EXDATE:20270302T170000Z", "RDATE:20270303T170000Z")
	count := 0
	err := Iter(t.Context(), s, instant(t, "2028-01-01T00:00:00Z"), 1, func(i Instance) bool { count++; return true })
	requirements.ErrorIs(err, ErrBudgetExceeded)
	assertions.Zero(count)
	requirements.NoError(Iter(t.Context(), s, instant(t, "2028-01-01T00:00:00Z"), 4, func(i Instance) bool { count++; return true }))
	assertions.Equal(1, count)
	requirements.NoError(Iter(t.Context(), parsed(t, "RRULE:FREQ=DAILY"), instant(t, "2028-01-01T00:00:00Z"), 2, func(i Instance) bool { return false }))
}

func TestParseSetRejectsInvalidRuleParameters(t *testing.T) {
	for _, line := range []string{
		"RRULE:FREQ=DAILY;COUNT=0",
		"RRULE:FREQ=DAILY;COUNT=-1",
		"RRULE:FREQ=DAILY;INTERVAL=0",
		"RRULE:FREQ=DAILY;INTERVAL=-1",
		"RRULE:FREQ=DAILY;COUNT=2;COUNT=3",
		"RRULE:FREQ=DAILY;DTSTART=20270301T170000Z",
	} {
		t.Run(line, func(t *testing.T) {
			_, err := ParseSet(instant(t, "2027-03-02T17:00:00Z"), false, "", time.UTC, []string{line})
			require.ErrorIs(t, err, ErrUnsupportedRecurrence)
			assert.Contains(t, err.Error(), line)
		})
	}
}

func TestParseSetAllDayRejectsTimeRules(t *testing.T) {
	for _, line := range []string{"RRULE:FREQ=HOURLY;COUNT=2", "RRULE:FREQ=DAILY;COUNT=2;BYHOUR=9"} {
		_, err := ParseSet(time.Time{}, true, "2027-03-02", time.UTC, []string{line})
		require.ErrorIs(t, err, ErrUnsupportedRecurrence)
	}
}

func TestRDateMixedUTCAndFloatingValuesUseSetZone(t *testing.T) {
	zone, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	s, err := ParseSet(instant(t, "2027-03-02T17:00:00Z"), false, "", zone,
		[]string{"RDATE:20270303T170000Z,20270304T120000"})
	require.NoError(t, err)
	assert.Equal(t, []string{"20270302T170000Z", "20270303T170000Z", "20270304T170000Z"}, keys(t, s))
}

func TestConservativeEndAllDayAcrossDST(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	zone, err := time.LoadLocation("America/Los_Angeles")
	requirements.NoError(err)
	s, err := ParseSet(time.Time{}, true, "2027-03-14", zone, []string{"RRULE:FREQ=WEEKLY;COUNT=2"})
	requirements.NoError(err)
	end, bounded := ConservativeEnd(t.Context(), s, 23*time.Hour, 1, 100)
	assertions.True(bounded)
	assertions.Equal(instant(t, "2027-03-22T21:00:00Z"), end.UTC())
}

func TestParseSetRejectsUnsupportedEaster(t *testing.T) {
	line := "RRULE:FREQ=YEARLY;COUNT=1;BYEASTER=1000"
	_, err := ParseSet(instant(t, "2027-03-02T17:00:00Z"), false, "", time.UTC, []string{line})
	require.ErrorIs(t, err, ErrUnsupportedRecurrence)
	assert.Contains(t, err.Error(), line)
}

func TestIterRepeatedRuleSelectors(t *testing.T) {
	for _, rule := range []string{
		"RRULE:FREQ=DAILY;COUNT=3;BYMONTH=3,3",
		"RRULE:FREQ=DAILY;COUNT=3;BYDAY=TU,TU,WE,TH",
	} {
		t.Run(rule, func(t *testing.T) {
			assert.Equal(t, []string{"20270302T170000Z", "20270303T170000Z", "20270304T170000Z"}, keys(t, parsed(t, rule)))
		})
	}
}

func TestParseSetAllowsFeb29Yearly(t *testing.T) {
	s := parsed(t, "RRULE:FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=29;COUNT=1")
	var got []string
	err := Iter(t.Context(), s, instant(t, "2028-03-01T00:00:00Z"), 10, func(i Instance) bool {
		got = append(got, i.Identity.Key())
		return true
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"20270302T170000Z", "20280229T170000Z"}, got)
}

func TestParseSetRejectsByWeekNo(t *testing.T) {
	line := "RRULE:FREQ=YEARLY;BYWEEKNO=20"
	_, err := ParseSet(instant(t, "2027-03-02T17:00:00Z"), false, "", time.UTC, []string{line})
	require.ErrorIs(t, err, ErrUnsupportedRecurrence)
	assert.Contains(t, err.Error(), line)
}

func TestParseSetRejectsUnsupportedSelectors(t *testing.T) {
	for _, rule := range []string{
		"FREQ=DAILY;BYYEARDAY=10",
		"FREQ=DAILY;BYHOUR=17",
		"FREQ=DAILY;BYMINUTE=0",
		"FREQ=DAILY;BYSECOND=0",
		"FREQ=DAILY;BYMONTHDAY=2",
		"FREQ=DAILY;BYSETPOS=1",
		"FREQ=DAILY;WKST=MO",
		"FREQ=DAILY;BYDAY=1TU",
		"FREQ=WEEKLY;BYMONTH=3",
		"FREQ=WEEKLY;BYMONTHDAY=2",
		"FREQ=WEEKLY;BYSETPOS=1",
		"FREQ=WEEKLY;BYDAY=1TU",
		"FREQ=MONTHLY;BYMONTH=3",
		"FREQ=MONTHLY;BYMONTHDAY=2;BYDAY=TU",
		"FREQ=MONTHLY;BYMONTHDAY=2;BYSETPOS=1",
		"FREQ=MONTHLY;BYSETPOS=1",
		"FREQ=MONTHLY;WKST=MO",
		"FREQ=YEARLY;BYMONTH=3",
		"FREQ=YEARLY;BYMONTHDAY=2",
		"FREQ=YEARLY;BYDAY=TU",
		"FREQ=YEARLY;BYMONTH=3;BYMONTHDAY=2;BYDAY=TU",
		"FREQ=YEARLY;BYMONTH=3;BYDAY=TU;BYSETPOS=1",
		"FREQ=YEARLY;WKST=MO",
	} {
		t.Run(rule, func(t *testing.T) {
			_, err := ParseSet(instant(t, "2027-03-02T17:00:00Z"), false, "", time.UTC, []string{"RRULE:" + rule})
			require.ErrorIs(t, err, ErrUnsupportedRecurrence)
		})
	}
}

func TestParseSetRejectsImpossibleMonthDays(t *testing.T) {
	for _, rule := range []string{
		"FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=30",
		"FREQ=YEARLY;BYMONTH=4;BYMONTHDAY=31",
		"FREQ=YEARLY;BYMONTH=2,4;BYMONTHDAY=31",
		"FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=-30",
		"FREQ=YEARLY;BYMONTH=4;BYMONTHDAY=-31",
	} {
		t.Run(rule, func(t *testing.T) {
			_, err := ParseSet(instant(t, "2027-03-02T17:00:00Z"), false, "", time.UTC, []string{"RRULE:" + rule})
			require.ErrorIs(t, err, ErrUnsupportedRecurrence)
		})
	}
}

func TestParseSetSupportedSubset(t *testing.T) {
	for _, rule := range []string{
		"FREQ=DAILY;BYDAY=MO,WE;BYMONTH=3,4",
		"FREQ=WEEKLY;BYDAY=MO,WE;WKST=SU",
		"FREQ=MONTHLY;BYMONTHDAY=-31,31",
		"FREQ=MONTHLY;BYDAY=1MO,-1FR",
		"FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-1",
		"FREQ=YEARLY;BYMONTH=2,3;BYMONTHDAY=30",
		"FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=-29",
		"FREQ=YEARLY;BYMONTH=4;BYMONTHDAY=-30",
		"FREQ=YEARLY;BYMONTH=3;BYDAY=1TU",
		"FREQ=YEARLY",
	} {
		t.Run(rule, func(t *testing.T) {
			_, err := ParseSet(instant(t, "2027-03-02T17:00:00Z"), false, "", time.UTC, []string{"RRULE:" + rule})
			require.NoError(t, err)
		})
	}
}

func TestParseSetRejectsUnreachableSelectorIntervalRules(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dtstart string
		rule    string
	}{
		{
			name:    "daily interval preserves excluded weekday",
			dtstart: "2027-03-02T17:00:00Z",
			rule:    "RRULE:FREQ=DAILY;INTERVAL=7;BYDAY=MO",
		},
		{
			name:    "monthly interval preserves month without selected day",
			dtstart: "2027-02-28T17:00:00Z",
			rule:    "RRULE:FREQ=MONTHLY;INTERVAL=12;BYMONTHDAY=30",
		},
		{
			name:    "yearly interval preserves non-leap years",
			dtstart: "2025-03-02T17:00:00Z",
			rule:    "RRULE:FREQ=YEARLY;INTERVAL=4;BYMONTH=2;BYMONTHDAY=29",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseSet(instant(t, tc.dtstart), false, "", time.UTC, []string{tc.rule})
			require.ErrorIs(t, err, ErrUnsupportedRecurrence)
		})
	}
}

func TestParseSetAllowsReachableSelectorIntervalRules(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dtstart string
		rule    string
	}{
		{
			name:    "daily weekday interval",
			dtstart: "2027-03-02T17:00:00Z",
			rule:    "RRULE:FREQ=DAILY;INTERVAL=7;BYDAY=TU",
		},
		{
			name:    "weekly weekday interval",
			dtstart: "2027-03-02T17:00:00Z",
			rule:    "RRULE:FREQ=WEEKLY;INTERVAL=2;BYDAY=MO",
		},
		{
			name:    "monthly day interval",
			dtstart: "2027-02-28T17:00:00Z",
			rule:    "RRULE:FREQ=MONTHLY;INTERVAL=12;BYMONTHDAY=28",
		},
		{
			name:    "yearly leap-day interval",
			dtstart: "2024-03-02T17:00:00Z",
			rule:    "RRULE:FREQ=YEARLY;INTERVAL=4;BYMONTH=2;BYMONTHDAY=29",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseSet(instant(t, tc.dtstart), false, "", time.UTC, []string{tc.rule})
			require.NoError(t, err)
		})
	}
}

func TestParseSetRejectsUnreachableOrdinalWeekdayPosition(t *testing.T) {
	requirements := require.New(t)
	rule := "RRULE:FREQ=MONTHLY;BYDAY=1MO;BYSETPOS=2"
	_, err := ParseSet(instant(t, "2027-03-01T17:00:00Z"), false, "", time.UTC, []string{rule})
	requirements.ErrorIs(err, ErrUnsupportedRecurrence)
}

func TestIterMonthlyOrdinalWeekday(t *testing.T) {
	assertions := assert.New(t)
	s, err := ParseSet(instant(t, "2027-03-01T17:00:00Z"), false, "", time.UTC,
		[]string{"RRULE:FREQ=MONTHLY;BYDAY=1MO;COUNT=3"})
	require.NoError(t, err)
	assertions.Equal([]string{"20270301T170000Z", "20270405T170000Z", "20270503T170000Z"}, keys(t, s))
}

func TestIterBudgetCountsDuplicateStreams(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s := parsed(t, "RRULE:FREQ=DAILY;COUNT=2", "RRULE:FREQ=DAILY;COUNT=2")
	count := 0
	err := Iter(t.Context(), s, instant(t, "2028-01-01T00:00:00Z"), 3, func(i Instance) bool { count++; return true })
	requirements.ErrorIs(err, ErrBudgetExceeded)
	assertions.Equal(1, count)
	count = 0
	requirements.NoError(Iter(t.Context(), s, instant(t, "2028-01-01T00:00:00Z"), 5, func(i Instance) bool { count++; return true }))
	assertions.Equal(2, count)
}

func TestParseSetRejectsSubDaily(t *testing.T) {
	for _, line := range []string{
		"RRULE:FREQ=HOURLY;COUNT=2",
		"RRULE:FREQ=HOURLY;INTERVAL=24;BYHOUR=10;COUNT=1",
		"RRULE:FREQ=MINUTELY;COUNT=2",
		"RRULE:FREQ=SECONDLY;COUNT=2",
	} {
		_, err := ParseSet(instant(t, "2027-03-02T09:00:00Z"), false, "", time.UTC, []string{line})
		require.ErrorIs(t, err, ErrUnsupportedRecurrence)
		assert.Contains(t, err.Error(), line)
	}
}

func TestIterUntilBoundsIteration(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s := parsed(t, "RRULE:FREQ=DAILY", "RRULE:FREQ=WEEKLY", "RDATE:20270305T170000Z")
	var got []string
	err := Iter(t.Context(), s, instant(t, "2027-03-04T17:00:00Z"), 5, func(i Instance) bool {
		got = append(got, i.Identity.Key())
		return true
	})
	requirements.NoError(err)
	assertions.Equal([]string{"20270302T170000Z", "20270303T170000Z", "20270304T170000Z"}, got)
	s = parsed(t, "RRULE:FREQ=DAILY;UNTIL=20270302T170000Z")
	got = nil
	requirements.NoError(Iter(t.Context(), s, instant(t, "2027-03-04T17:00:00Z"), 100, func(i Instance) bool {
		got = append(got, i.Identity.Key())
		return true
	}))
	assertions.Equal([]string{"20270302T170000Z"}, got)
}
