package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRelationshipDateRequiresAYear(t *testing.T) {
	// Local helpers: make lint-ci runs testify-helper-check, which fails any
	// test body with more than three direct testify package calls and at least
	// two of one kind.
	assert := assert.New(t)
	require := require.New(t)

	for _, raw := range []string{"2019", "2019-04", "2019-04-12", "  2019-04  "} {
		got, err := ParseRelationshipDate(raw)
		require.NoErrorf(err, "value %q must parse", raw)
		require.NotNil(got.Year)
	}

	// The shared type also accepts the two year-less RFC 6350 truncated forms
	// its contract documents ("--04-12" and "---12"), which are meaningful for
	// a recurring BDAY but not for a relationship interval: a relationship
	// cannot start on "some April 12th" with no year. PR 6 narrows the accepted
	// set rather than storing a bound it cannot order. Only the two documented
	// forms are asserted here — do not add a form the dependency's contract
	// does not promise.
	for _, raw := range []string{"--04-12", "---12"} {
		shared, err := ParsePartialDate(raw)
		require.NoErrorf(err, "the shared type is expected to accept %q", raw)
		assert.Nil(shared.Year)

		_, err = ParseRelationshipDate(raw)
		require.Errorf(err, "a relationship interval must reject %q", raw)
		require.ErrorIs(err, ErrInvalidPartialDate)
	}

	for _, raw := range []string{"", "  ", "198", "2019-13", "2019-04-31", "abcd"} {
		_, err := ParseRelationshipDate(raw)
		require.Errorf(err, "value %q must not parse", raw)
		require.ErrorIs(err, ErrInvalidPartialDate)
	}
}

func TestValidateRelationshipIntervalUsesSharedPrecision(t *testing.T) {
	tests := []struct {
		name    string
		from    string
		until   string
		wantErr bool
	}{
		{name: "open interval", from: "2019", until: ""},
		{name: "no start", from: "", until: "2019"},
		{name: "same year", from: "2019", until: "2019"},
		{name: "ordered years", from: "2018", until: "2019"},
		{name: "ordered months", from: "2019-03", until: "2019-04"},
		{name: "ordered days", from: "2019-04-01", until: "2019-04-02"},
		{
			// Year is the only shared component, so this is not a
			// contradiction: started in June 2019, ended sometime in 2019.
			name: "coarser end inside the same year",
			from: "2019-06", until: "2019",
		},
		{
			name: "coarser start inside the same year",
			from: "2019", until: "2019-06",
		},
		{name: "reversed years", from: "2020", until: "2019", wantErr: true},
		{name: "reversed months", from: "2019-06", until: "2019-05", wantErr: true},
		{name: "reversed days", from: "2019-04-12", until: "2019-04-11", wantErr: true},
		{name: "reversed across years", from: "2020-01", until: "2019-12", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateRelationshipInterval(
				optionalRelationshipDate(t, test.from),
				optionalRelationshipDate(t, test.until),
			)
			if test.wantErr {
				require.Error(t, err)
				require.ErrorIs(t, err, ErrPersonRelationshipInterval)
				return
			}
			require.NoError(t, err)
		})
	}
}

// optionalRelationshipDate turns "" into a nil bound so the table can express
// an open-ended interval.
func optionalRelationshipDate(t *testing.T, raw string) *PartialDate {
	t.Helper()
	if raw == "" {
		return nil
	}
	parsed, err := ParseRelationshipDate(raw)
	require.NoError(t, err)
	return &parsed
}
