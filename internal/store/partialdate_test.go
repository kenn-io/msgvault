package store

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPartialDateValidate(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	tests := []struct {
		name  string
		date  PartialDate
		valid bool
	}{
		{name: "full date", date: partialDate(1985, 4, 12), valid: true},
		{name: "year and month", date: partialDate(1985, 4, 0), valid: true},
		{name: "year only", date: partialDate(1985, 0, 0), valid: true},
		{name: "month and day without year", date: partialDate(0, 4, 12), valid: true},
		{name: "day only", date: partialDate(0, 0, 12), valid: true},
		{name: "month only", date: partialDate(0, 4, 0), valid: true},
		{name: "empty", date: PartialDate{}},
		{name: "month zero", date: partialDate(1985, 13, 1)},
		{name: "leap day in common year", date: partialDate(1985, 2, 29)},
		{name: "leap day in leap year", date: partialDate(1984, 2, 29), valid: true},
		{name: "year and day without month", date: partialDate(1985, 0, 12)},
	}
	for _, test := range tests {
		err := test.date.Validate()
		if test.valid {
			require.NoError(err, test.name)
			continue
		}
		assert.ErrorIs(err, ErrInvalidPartialDate, test.name)
	}
}

func TestPartialDateStringRoundTrips(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	tests := []struct {
		date PartialDate
		want string
	}{
		{date: partialDate(1985, 4, 12), want: "1985-04-12"},
		{date: partialDate(1985, 4, 0), want: "1985-04"},
		{date: partialDate(1985, 0, 0), want: "1985"},
		{date: partialDate(0, 4, 12), want: "--04-12"},
		{date: partialDate(0, 4, 0), want: "--04"},
		{date: partialDate(0, 0, 12), want: "---12"},
	}
	for _, test := range tests {
		assert.Equal(test.want, test.date.String())
		parsed, err := ParsePartialDate(test.want)
		require.NoError(err, test.want)
		assert.Equal(test.date, parsed, test.want)
	}
	_, err := ParsePartialDate("not-a-date")
	require.ErrorIs(err, ErrInvalidPartialDate)
	for _, malformed := range []string{"--+4", "--04+2"} {
		_, err := ParsePartialDate(malformed)
		require.ErrorIs(err, ErrInvalidPartialDate, malformed)
	}

	compact, err := ParsePartialDate("--0412")
	require.NoError(err)
	assert.Equal(partialDate(0, 4, 12), compact)
	assert.Equal("--04-12", compact.String())
}

func TestCompareAtSharedPrecisionUsesOnlyCommonComponents(t *testing.T) {
	assert := assert.New(t)

	tests := []struct {
		name string
		a    PartialDate
		b    PartialDate
		want int
	}{
		{name: "full dates ordered", a: partialDate(1985, 4, 12), b: partialDate(1985, 4, 13), want: -1},
		{name: "full dates equal", a: partialDate(1985, 4, 12), b: partialDate(1985, 4, 12), want: 0},
		{name: "years differ", a: partialDate(1984, 12, 31), b: partialDate(1985, 1, 1), want: -1},
		{
			name: "coarser value equal at shared precision",
			a:    partialDate(1985, 0, 0), b: partialDate(1985, 4, 12), want: 0,
		},
		{
			name: "coarser value still ordered by the component it has",
			a:    partialDate(1984, 0, 0), b: partialDate(1985, 4, 12), want: -1,
		},
		{
			name: "year-less dates compare by month and day",
			a:    partialDate(0, 4, 12), b: partialDate(0, 6, 1), want: -1,
		},
		{
			name: "no shared component compares equal",
			a:    partialDate(1985, 0, 0), b: partialDate(0, 0, 12), want: 0,
		},
	}
	for _, test := range tests {
		assert.Equal(test.want, CompareAtSharedPrecision(test.a, test.b), test.name)
		assert.Equal(-test.want, CompareAtSharedPrecision(test.b, test.a), test.name+" reversed")
	}
}

func TestPartialDateComponentColumnsRoundTrip(t *testing.T) {
	assert := assert.New(t)

	date := partialDate(1985, 4, 0)
	args := PartialDateArgs(date)
	assert.Equal([]any{1985, 4, nil}, args,
		"absent components bind as NULL, never as zero")

	scanned := ScanPartialDate(
		sql.NullInt64{Int64: 1985, Valid: true},
		sql.NullInt64{Int64: 4, Valid: true},
		sql.NullInt64{},
	)
	assert.Equal(date, scanned)
	assert.Equal(PartialDate{}, ScanPartialDate(
		sql.NullInt64{}, sql.NullInt64{}, sql.NullInt64{},
	))
}

// partialDate and intPtr are duplicated from the store_test package's copies
// in person_names_test.go. A helper cannot cross a package boundary, and this
// file must stay in package store to reach joinTypeTokens/splitTypeTokens.
func partialDate(year, month, day int) PartialDate {
	date := PartialDate{}
	if year != 0 {
		value := year
		date.Year = &value
	}
	if month != 0 {
		value := month
		date.Month = &value
	}
	if day != 0 {
		value := day
		date.Day = &value
	}
	return date
}
