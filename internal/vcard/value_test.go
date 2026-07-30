package vcard

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnescapeText(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{raw: `Alice\\Bob`, want: `Alice\Bob`},
		{raw: `one\,two`, want: "one,two"},
		{raw: `one\;two`, want: "one;two"},
		{raw: `line1\nline2`, want: "line1\nline2"},
		{raw: `line1\Nline2`, want: "line1\nline2"},
		{raw: `keep\x`, want: `keep\x`},
		{raw: `trailing\`, want: `trailing\`},
	}
	for _, test := range tests {
		t.Run(test.raw, func(t *testing.T) {
			got, err := UnescapeText(test.raw)
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestEscapeTextRoundTrips(t *testing.T) {
	for _, value := range []string{
		"",
		"Alice Example",
		`Alice\Bob`,
		"one,two;three",
		"line1\nline2",
	} {
		t.Run(value, func(t *testing.T) {
			got, err := UnescapeText(EscapeText(value))
			require.NoError(t, err)
			assert.Equal(t, value, got)
		})
	}
}

func TestSplitStructuredTextHonorsEscapes(t *testing.T) {
	got, err := SplitStructuredText(`box\;42;123 Main St.;Exampleville;;;12345;US`)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"box;42", "123 Main St.", "Exampleville", "", "", "12345", "US",
	}, got)
	assert.Equal(t,
		`box\;42;123 Main St.;Exampleville;;;12345;US`,
		JoinStructuredText(got),
	)
}

func TestSplitTextListHonorsEscapedComma(t *testing.T) {
	got, err := SplitTextList(`friends,food\,wine,travel`)
	require.NoError(t, err)
	assert.Equal(t, []string{"friends", "food,wine", "travel"}, got)
	assert.Equal(t, `friends,food\,wine,travel`, JoinTextList(got))
}

func TestSplitTextValuesPreserveEmptyComponents(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	list, err := SplitTextList(`,one,,`)
	require.NoError(err)
	assert.Equal([]string{"", "one", "", ""}, list)

	structured, err := SplitStructuredText(`;;;;`)
	require.NoError(err)
	assert.Equal([]string{"", "", "", "", ""}, structured)
}

func TestParameterValueRoundTripsRFC6868(t *testing.T) {
	for _, value := range []string{
		"",
		"plain",
		"A^B",
		"A\"B",
		"line1\nline2",
		"A^B\"C\nD",
	} {
		t.Run(value, func(t *testing.T) {
			encoded := EncodeParameterValue(value)
			decoded, err := DecodeParameterValue(encoded)
			require.NoError(t, err)
			assert.Equal(t, value, decoded)
		})
	}

	decoded, err := DecodeParameterValue("keep^x")
	require.NoError(t, err)
	assert.Equal(t, "keep^x", decoded)
}

func TestDecodeQuotedPrintableStrict(t *testing.T) {
	got, err := DecodeQuotedPrintable("Ren=C3=A9=20Dupont")
	require.NoError(t, err)
	assert.Equal(t, "René Dupont", got)

	_, err = DecodeQuotedPrintable("broken\x01")
	require.Error(t, err)
}

func TestParsePartialDate(t *testing.T) {
	tests := []struct {
		raw        string
		want       PartialDate
		wantString string
	}{
		{"19850412", partialDate(1985, 4, 12), "1985-04-12"},
		{"1985-04-12", partialDate(1985, 4, 12), "1985-04-12"},
		{"1985-04", partialDate(1985, 4, 0), "1985-04"},
		{"1985", partialDate(1985, 0, 0), "1985"},
		{"--0412", partialDate(0, 4, 12), "--04-12"},
		{"--04-12", partialDate(0, 4, 12), "--04-12"},
		{"--04", partialDate(0, 4, 0), "--04"},
		{"---12", partialDate(0, 0, 12), "---12"},
	}
	for _, test := range tests {
		t.Run(test.raw, func(t *testing.T) {
			got, err := ParsePartialDate(test.raw)
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
			assert.Equal(t, test.wantString, got.String())
		})
	}
}

func TestParsePartialDateRejectsInvalidForms(t *testing.T) {
	for _, raw := range []string{
		"",
		"0000",
		"1985-00",
		"1985-13",
		"1985-02-29",
		"1984-02-30",
		"--0001",
		"--1331",
		"12",
		"1985-04-12T10:00:00",
		"---00",
		"---32",
		"1985-0412",
		"198504-12",
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := ParsePartialDate(raw)
			require.Error(t, err)
		})
	}
}

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
