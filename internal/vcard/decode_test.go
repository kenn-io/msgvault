package vcard

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodePreservesOrderGroupsParametersAndUnknowns(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	input := "BEGIN:VCARD\r\n" +
		"VERSION:4.0\r\n" +
		"FN:Alice Example\r\n" +
		"item1.EMAIL;type=home,INTERNET;X-LABEL=\"A^^B^'C\":alice@example.com\r\n" +
		"item1.X-ABLABEL:_$!<Home>!$_\r\n" +
		"X-EXAMPLE;X-ORDER=first,second:opaque\\,value\r\n" +
		"END:VCARD\r\n"

	doc, err := Decode(strings.NewReader(input))
	require.NoError(err)
	require.Len(doc.Cards, 1)

	card := doc.Cards[0]
	require.Len(card.Properties, 5)
	assert.Equal([]string{"VERSION", "FN", "EMAIL", "X-ABLABEL", "X-EXAMPLE"},
		propertyNames(card.Properties))

	email := card.Properties[2]
	assert.Equal("item1", email.Group)
	assert.Equal("EMAIL", email.Name)
	assert.Equal("type", email.Parameters[0].OriginalName)
	assert.Equal([]string{"home", "INTERNET"}, decodedParameterValues(email.Parameters[0]))
	assert.Equal(`A^B"C`, email.Parameters[1].Values[0].Decoded)
	assert.Equal("alice@example.com", email.RawValue)
	assert.Equal(`opaque\,value`, card.Properties[4].RawValue)
}

func TestDecodeUnfoldsOneWhitespaceOctet(t *testing.T) {
	doc, err := Decode(strings.NewReader(
		"BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice \r\n Example\r\nEND:VCARD\r\n",
	))
	require.NoError(t, err)
	assert.Equal(t, "Alice Example", doc.Cards[0].Properties[1].RawValue)
}

func TestDecodeAcceptsLFAndTabContinuations(t *testing.T) {
	doc, err := Decode(strings.NewReader(
		"\ufeffBEGIN:VCARD\nVERSION:4.0\nFN:Alice\n\tExample\nEND:VCARD\n",
	))
	require.NoError(t, err)
	assert.Equal(t, "AliceExample", doc.Cards[0].Properties[1].RawValue)
}

func TestDecodeJoinsOnlyQuotedPrintableSoftLines(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	input := "BEGIN:VCARD\r\n" +
		"VERSION:2.1\r\n" +
		"FN;ENCODING=QUOTED-PRINTABLE:Jo=C3=A3o da =\r\n" +
		" Silva\r\n" +
		"PHOTO;ENCODING=b:/9j/4AAQ=\r\n" +
		"END:VCARD\r\n"

	doc, err := Decode(strings.NewReader(input))
	require.NoError(err)
	require.Len(doc.Cards[0].Properties, 3)
	assert.Equal("Jo=C3=A3o da Silva", doc.Cards[0].Properties[1].RawValue)
	assert.Equal("/9j/4AAQ=", doc.Cards[0].Properties[2].RawValue)
}

func TestDecodeRejectsV21WhenDisabled(t *testing.T) {
	_, err := DecodeWithOptions(strings.NewReader(
		"BEGIN:VCARD\r\nVERSION:2.1\r\nFN:Alice\r\nEND:VCARD\r\n",
	), DecodeOptions{DisallowV21: true})
	require.Error(t, err)
	assert.ErrorContains(t, err, "vCard 2.1 is disabled")
}

func TestDecodeWithPartialOptionsAllowsV21(t *testing.T) {
	_, err := DecodeWithOptions(strings.NewReader(
		"BEGIN:VCARD\r\nVERSION:2.1\r\nFN:Alice\r\nEND:VCARD\r\n",
	), DecodeOptions{MaxCards: 1})
	require.NoError(t, err)
}

func TestDecodeRejectsNestedCardWithLocation(t *testing.T) {
	_, err := Decode(strings.NewReader(
		"BEGIN:VCARD\r\nVERSION:4.0\r\nBEGIN:VCARD\r\nEND:VCARD\r\n",
	))
	require.Error(t, err)
	require.ErrorContains(t, err, "physical line 3")
	require.ErrorContains(t, err, "nested BEGIN:VCARD")
}

func TestDecodeEnforcesLogicalLineLimit(t *testing.T) {
	input := "BEGIN:VCARD\r\nVERSION:4.0\r\nFN:" +
		strings.Repeat("a", 65) + "\r\n " + strings.Repeat("b", 65) +
		"\r\nEND:VCARD\r\n"
	_, err := DecodeWithOptions(strings.NewReader(input), DecodeOptions{
		MaxPhysicalLineBytes: 100,
		MaxLogicalLineBytes:  100,
		MaxCards:             10,
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "logical content line exceeds 100 bytes")
}

func TestDecodeEnforcesLogicalLimitOnFirstPhysicalLine(t *testing.T) {
	_, err := DecodeWithOptions(strings.NewReader(
		"BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\nEND:VCARD\r\n",
	), DecodeOptions{
		MaxPhysicalLineBytes: 100,
		MaxLogicalLineBytes:  5,
		MaxCards:             10,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "physical line 1")
	require.ErrorContains(t, err, "logical content line exceeds 5 bytes")
}

func TestDecodeEnforcesPhysicalLineAndCardLimits(t *testing.T) {
	require := require.New(t)
	_, err := DecodeWithOptions(strings.NewReader(
		"BEGIN:VCARD\r\nVERSION:4.0\r\nFN:"+strings.Repeat("a", 30)+"\r\nEND:VCARD\r\n",
	), DecodeOptions{
		MaxPhysicalLineBytes: 20,
		MaxLogicalLineBytes:  100,
		MaxCards:             10,
	})
	require.Error(err)
	require.ErrorContains(err, "physical line 3 exceeds 20 bytes")

	_, err = DecodeWithOptions(strings.NewReader(
		"BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\nEND:VCARD\r\n"+
			"BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Bob\r\nEND:VCARD\r\n",
	), DecodeOptions{
		MaxPhysicalLineBytes: 100,
		MaxLogicalLineBytes:  100,
		MaxCards:             1,
	})
	require.Error(err)
	require.ErrorContains(err, "card count exceeds 1")
}

func TestDecodeEnforcesCardLimitBeforeReadingTrailingInput(t *testing.T) {
	trailingErr := errors.New("unexpected trailing read")
	input := io.MultiReader(
		strings.NewReader(
			"BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\nEND:VCARD\r\n"+
				"BEGIN:VCARD\r\nVERSION:4.0\r\n",
		),
		errorReader{err: trailingErr},
	)

	_, err := DecodeWithOptions(input, DecodeOptions{
		MaxPhysicalLineBytes: 100,
		MaxLogicalLineBytes:  100,
		MaxCards:             1,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "card count exceeds 1")
	assert.NotErrorIs(t, err, trailingErr)
}

func TestDecodeEnforcesLogicalLimitBeforeReadingTrailingInput(t *testing.T) {
	trailingErr := errors.New("unexpected trailing read")
	input := io.MultiReader(
		strings.NewReader(
			"BEGIN:VCARD\r\nVERSION:4.0\r\nNOTE:aaaa\r\n bbbb\r\n",
		),
		errorReader{err: trailingErr},
	)

	_, err := DecodeWithOptions(input, DecodeOptions{
		MaxPhysicalLineBytes: 100,
		MaxLogicalLineBytes:  10,
		MaxCards:             1,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "logical content line exceeds 10 bytes")
	assert.NotErrorIs(t, err, trailingErr)
}

func TestDecodeFoldedLineAllocationsAreBounded(t *testing.T) {
	input := "BEGIN:VCARD\r\nVERSION:4.0\r\nNOTE:a\r\n" +
		strings.Repeat(" a\r\n", 4096) +
		"END:VCARD\r\n"
	options := DecodeOptions{
		MaxPhysicalLineBytes: 100,
		MaxLogicalLineBytes:  1 << 20,
		MaxCards:             1,
	}

	var decodeErr error
	allocations := testing.AllocsPerRun(3, func() {
		_, decodeErr = DecodeWithOptions(strings.NewReader(input), options)
	})

	require.NoError(t, decodeErr)
	assert.Less(t, allocations, 500.0)
}

func TestDecodeEnforcesParameterCountBoundary(t *testing.T) {
	tests := []struct {
		name    string
		count   int
		wantErr string
	}{
		{name: "maximum accepted", count: 100},
		{name: "one over maximum rejected", count: 101, wantErr: "parameter count exceeds 100"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := "BEGIN:VCARD\r\nVERSION:4.0\r\nFN" +
				strings.Repeat(";X-P=one", test.count) +
				":Alice\r\nEND:VCARD\r\n"
			_, err := Decode(strings.NewReader(input))
			if test.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestDecodeEnforcesParameterValueCountBoundary(t *testing.T) {
	tests := []struct {
		name    string
		count   int
		wantErr string
	}{
		{name: "maximum accepted", count: 100},
		{name: "one over maximum rejected", count: 101, wantErr: "parameter value count exceeds 100"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := strings.Repeat("home,", test.count-1) + "home"
			input := "BEGIN:VCARD\r\nVERSION:4.0\r\n" +
				"TEL;TYPE=" + values + ":+12025550123\r\nEND:VCARD\r\n"
			_, err := Decode(strings.NewReader(input))
			if test.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestDecodeRejectsMalformedFramingAndContentLines(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr string
	}{
		{
			name:    "stray end",
			input:   "END:VCARD\r\n",
			wantErr: "stray END:VCARD",
		},
		{
			name:    "missing end",
			input:   "BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\n",
			wantErr: "missing END:VCARD",
		},
		{
			name:    "outside card",
			input:   "VERSION:4.0\r\n",
			wantErr: "content outside VCARD",
		},
		{
			name:    "missing colon",
			input:   "BEGIN:VCARD\r\nVERSION:4.0\r\nBROKEN\r\nEND:VCARD\r\n",
			wantErr: "physical line 3",
		},
		{
			name:    "empty name",
			input:   "BEGIN:VCARD\r\n:empty\r\nEND:VCARD\r\n",
			wantErr: "empty property name",
		},
		{
			name:    "unclosed quote",
			input:   "BEGIN:VCARD\r\nFN;TYPE=\"home:Alice\r\nEND:VCARD\r\n",
			wantErr: "unclosed quote",
		},
		{
			name:    "invalid parameter",
			input:   "BEGIN:VCARD\r\nFN;BAD NAME=x:Alice\r\nEND:VCARD\r\n",
			wantErr: "invalid parameter name",
		},
		{
			name:    "bare carriage return",
			input:   "BEGIN:VCARD\rVERSION:4.0\n",
			wantErr: "bare CR",
		},
		{
			name:    "nul in property value",
			input:   "BEGIN:VCARD\r\nFN:Ali\x00ce\r\nEND:VCARD\r\n",
			wantErr: "raw value contains CR, LF, or NUL",
		},
		{
			name:    "nul in named parameter",
			input:   "BEGIN:VCARD\r\nFN;TYPE=ho\x00me:Alice\r\nEND:VCARD\r\n",
			wantErr: "parameter value contains CR, LF, or NUL",
		},
		{
			name:    "nul in bare parameter",
			input:   "BEGIN:VCARD\r\nTEL;CE\x00LL:+12025550123\r\nEND:VCARD\r\n",
			wantErr: "parameter value contains CR, LF, or NUL",
		},
		{
			name:    "invalid utf8",
			input:   "BEGIN:VCARD\r\nFN:Ali\xffce\r\nEND:VCARD\r\n",
			wantErr: "not valid UTF-8",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Decode(strings.NewReader(test.input))
			require.Error(t, err)
			assert.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestDecodePreservesBlankValuesAndBareV21Parameters(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	doc, err := Decode(strings.NewReader(
		"BEGIN:VCARD\r\nVERSION:2.1\r\nTEL;CELL:\r\nEND:VCARD\r\n",
	))
	require.NoError(err)
	tel := doc.Cards[0].Properties[1]
	assert.Empty(tel.RawValue)
	require.Len(tel.Parameters, 1)
	assert.Equal("TYPE", tel.Parameters[0].Name)
	assert.Empty(tel.Parameters[0].OriginalName)
	assert.Equal("CELL", tel.Parameters[0].Values[0].Decoded)
	assert.True(tel.Parameters[0].Values[0].RawValid)
}

func TestDecodePreservesLiteralCaretParameterValuesBeforeV40(t *testing.T) {
	for _, version := range []Version{Version21, Version30} {
		t.Run(string(version), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			doc, err := Decode(strings.NewReader(
				"BEGIN:VCARD\r\nVERSION:" + string(version) +
					"\r\nFN;X-LABEL=literal^n:Alice\r\nEND:VCARD\r\n",
			))
			require.NoError(err)
			require.Len(doc.Cards, 1)
			require.Len(doc.Cards[0].Properties, 2)
			require.Len(doc.Cards[0].Properties[1].Parameters, 1)
			assert.Equal("literal^n", doc.Cards[0].Properties[1].Parameters[0].Values[0].Decoded)
		})
	}
}

func TestCardAndPropertyLookupIsCaseInsensitive(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	doc, err := Decode(strings.NewReader(
		"BEGIN:VCARD\r\nVERSION:4.0\r\nitem1.EMAIL;TYPE=HOME:a@example.com\r\nEND:VCARD\r\n",
	))
	require.NoError(err)
	properties := doc.Cards[0].PropertiesNamed("email")
	require.Len(properties, 1)
	assert.Equal("item1", properties[0].Group)
	assert.Len(properties[0].ParametersNamed("type"), 1)
}

func propertyNames(properties []Property) []string {
	names := make([]string, 0, len(properties))
	for _, property := range properties {
		names = append(names, property.Name)
	}
	return names
}

func decodedParameterValues(parameter Parameter) []string {
	values := make([]string, 0, len(parameter.Values))
	for _, value := range parameter.Values {
		values = append(values, value.Decoded)
	}
	return values
}

type errorReader struct {
	err error
}

func (r errorReader) Read([]byte) (int, error) {
	return 0, r.err
}
