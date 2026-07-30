package vcard

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncodePreservesSyntaxOrderAndSourceSpelling(t *testing.T) {
	input := "BEGIN:VCARD\r\n" +
		"version:4.0\r\n" +
		"fn:Alice Example\r\n" +
		"item1.email;type=HOME;X-Foo=\"a,b\":alice@example.com\r\n" +
		"item1.X-ABLabel:_$!<Home>!$_\r\n" +
		"END:VCARD\r\n"
	doc, err := Decode(strings.NewReader(input))
	require.NoError(t, err)

	got, err := Marshal(doc)
	require.NoError(t, err)
	assert.Equal(t, input, string(got))
}

func TestEncodePreservesBareV21ParameterSyntax(t *testing.T) {
	input := "BEGIN:VCARD\r\nVERSION:2.1\r\nTEL;CELL:+12025550123\r\nEND:VCARD\r\n"
	doc, err := Decode(strings.NewReader(input))
	require.NoError(t, err)

	got, err := Marshal(doc)
	require.NoError(t, err)
	assert.Equal(t, input, string(got))
}

func TestEncodeFoldsAt75UTF8Octets(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	property, err := NewProperty("", "NOTE", strings.Repeat("é", 50))
	require.NoError(err)
	doc := Document{Cards: []Card{{Properties: []Property{
		mustProperty(t, "VERSION", "4.0"),
		mustProperty(t, "FN", "Alice Example"),
		property,
	}}}}

	got, err := Marshal(doc)
	require.NoError(err)
	for line := range strings.SplitSeq(strings.TrimSuffix(string(got), "\r\n"), "\r\n") {
		assert.LessOrEqual(len([]byte(line)), 75, "line %q", line)
		assert.True(utf8.ValidString(line), "fold split a UTF-8 code point")
	}
	assert.Contains(string(got), "\r\n ")
}

func TestEncodeCountsContinuationSpaceInsideFoldLimit(t *testing.T) {
	property := mustProperty(t, "NOTE", strings.Repeat("a", 150))
	var output strings.Builder
	require.NoError(t, EncodeWithOptions(&output, Document{
		Cards: []Card{{Properties: []Property{property}}},
	}, EncodeOptions{FoldBytes: 20}))

	for line := range strings.SplitSeq(strings.TrimSuffix(output.String(), "\r\n"), "\r\n") {
		assert.LessOrEqual(t, len(line), 20, "line %q", line)
	}
}

func TestEncodeDoesNotFoldLineOfExactly75Bytes(t *testing.T) {
	const prefix = "NOTE:"
	property := mustProperty(t, "NOTE", strings.Repeat("a", 75-len(prefix)))

	got, err := Marshal(Document{Cards: []Card{{Properties: []Property{property}}}})
	require.NoError(t, err)
	assert.Contains(t, string(got), prefix+strings.Repeat("a", 75-len(prefix))+"\r\n")
	assert.NotContains(t, string(got), prefix+strings.Repeat("a", 75-len(prefix))+"\r\n ")
}

func TestEncodeDoesNotFoldAfterQuotedPrintableEscapeEquals(t *testing.T) {
	require := require.New(t)
	input := "BEGIN:VCARD\r\n" +
		"VERSION:2.1\r\n" +
		"FN;ENCODING=QUOTED-PRINTABLE:" + strings.Repeat("A", 45) + "=C3=A9\r\n" +
		"END:VCARD\r\n"
	first, err := Decode(strings.NewReader(input))
	require.NoError(err)

	rendered, err := Marshal(first)
	require.NoError(err)
	second, err := Decode(strings.NewReader(string(rendered)))
	require.NoError(err)

	assert.Equal(t, first, second)
}

func TestEncodePreservesLegacyCharsetBytes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	input := []byte("BEGIN:VCARD\r\n" +
		"VERSION:2.1\r\n" +
		"FN;CHARSET=ISO-8859-1:Ren\xe9 Example\r\n" +
		"END:VCARD\r\n")
	first, err := Decode(bytes.NewReader(input))
	require.NoError(err)

	rendered, err := Marshal(first)
	require.NoError(err)
	assert.Equal(input, rendered)

	second, err := Decode(bytes.NewReader(rendered))
	require.NoError(err)
	assert.Equal(first, second)
}

func TestEncodeQuotesAndCaretEscapesConstructedParameters(t *testing.T) {
	property := mustProperty(t, "EMAIL", "alice@example.com")
	parameter, err := NewParameter("X-LABEL", "a,b", "A^B\"C\nD")
	require.NoError(t, err)
	property.Parameters = []Parameter{parameter}

	got, err := Marshal(Document{Cards: []Card{{Properties: []Property{
		mustProperty(t, "VERSION", "4.0"),
		property,
	}}}})
	require.NoError(t, err)
	assert.Contains(t, string(got), `EMAIL;X-LABEL="a,b",A^^B^'C^nD:alice@example.com`)
}

func TestEncodePreservesLiteralCaretParametersBeforeV40(t *testing.T) {
	for _, version := range []Version{Version21, Version30} {
		t.Run(string(version), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			property := mustProperty(t, "FN", "Alice")
			parameter, err := NewParameter("X-LABEL", "literal^n")
			require.NoError(err)
			property.Parameters = []Parameter{parameter}

			got, err := Marshal(Document{Cards: []Card{{Properties: []Property{
				mustProperty(t, "VERSION", string(version)),
				property,
			}}}})
			require.NoError(err)
			assert.Contains(string(got), "FN;X-LABEL=literal^n:Alice\r\n")
			assert.NotContains(string(got), "FN;X-LABEL=literal^^n:Alice\r\n")
		})
	}
}

func TestEncodeRejectsContentLineInjection(t *testing.T) {
	property, err := NewProperty("", "NOTE", "safe\r\nX-EVIL:true")
	require.NoError(t, err)
	_, err = Marshal(Document{Cards: []Card{{Properties: []Property{property}}}})
	require.Error(t, err)
	assert.ErrorContains(t, err, "raw value contains CR, LF, or NUL")
}

func TestEncodeRejectsSyntaxTokenAndParameterInjection(t *testing.T) {
	tests := []struct {
		name     string
		property Property
		wantErr  string
	}{
		{
			name:     "group",
			property: Property{Group: "item\r\nX", Name: "FN", RawValue: "Alice"},
			wantErr:  "group contains CR, LF, or NUL",
		},
		{
			name:     "property name",
			property: Property{Name: "FN\nX", RawValue: "Alice"},
			wantErr:  "property name contains CR, LF, or NUL",
		},
		{
			name: "parameter name",
			property: Property{Name: "FN", RawValue: "Alice", Parameters: []Parameter{{
				Name: "TYPE\r\nX",
			}}},
			wantErr: "parameter name contains CR, LF, or NUL",
		},
		{
			name: "raw parameter value",
			property: Property{Name: "FN", RawValue: "Alice", Parameters: []Parameter{{
				Name: "TYPE",
				Values: []ParameterValue{{
					Raw:      "home\r\nX",
					RawValid: true,
				}},
			}}},
			wantErr: "parameter value contains CR, LF, or NUL",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Marshal(Document{Cards: []Card{{Properties: []Property{test.property}}}})
			require.Error(t, err)
			assert.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestEncodeHandlesEmptyValuesCardsAndDocuments(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	got, err := Marshal(Document{})
	require.NoError(err)
	assert.Empty(got)

	got, err = Marshal(Document{Cards: []Card{
		{},
		{Properties: []Property{mustProperty(t, "FN", "")}},
	}})
	require.NoError(err)
	assert.Equal(
		"BEGIN:VCARD\r\nEND:VCARD\r\n"+
			"BEGIN:VCARD\r\nFN:\r\nEND:VCARD\r\n",
		string(got),
	)
}

func TestEncodeRejectsInvalidFoldLimit(t *testing.T) {
	err := EncodeWithOptions(io.Discard, Document{}, EncodeOptions{FoldBytes: 3})
	require.Error(t, err)
	assert.ErrorContains(t, err, "fold limit must be at least 4 bytes")
}

func TestEncodeReturnsShortWriteError(t *testing.T) {
	err := Encode(shortWriter{}, Document{Cards: []Card{{}}})
	require.Error(t, err)
	assert.ErrorIs(t, err, io.ErrShortWrite)
}

func mustProperty(tb testing.TB, name, rawValue string) Property {
	tb.Helper()
	property, err := NewProperty("", name, rawValue)
	require.NoError(tb, err)
	return property
}

type shortWriter struct{}

func (shortWriter) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	return len(data) - 1, nil
}
