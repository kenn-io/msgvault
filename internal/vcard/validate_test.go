package vcard

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vcard/registry"
)

func TestValidateV4Card(t *testing.T) {
	doc := mustDecode(t,
		"BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice Example\r\nUID:person-1\r\nEND:VCARD\r\n",
	)
	require.NoError(t, Validate(doc))
}

func TestValidateRejectsMissingFNInV4(t *testing.T) {
	doc := mustDecode(t,
		"BEGIN:VCARD\r\nVERSION:4.0\r\nUID:person-1\r\nEND:VCARD\r\n",
	)
	err := Validate(doc)
	require.Error(t, err)
	assert.ErrorContains(t, err, "card 1: vCard 4.0 requires at least one non-empty FN")
}

func TestValidateRejectsDuplicateVersion(t *testing.T) {
	doc := mustDecode(t,
		"BEGIN:VCARD\r\nVERSION:4.0\r\nVERSION:3.0\r\nFN:Alice\r\nEND:VCARD\r\n",
	)
	err := Validate(doc)
	require.Error(t, err)
	assert.ErrorContains(t, err, "card 1: requires exactly one VERSION")
}

func TestValidateAcceptsSupportedVersions(t *testing.T) {
	for _, version := range []Version{Version21, Version30, Version40} {
		t.Run(string(version), func(t *testing.T) {
			fn := ""
			if version != Version21 {
				fn = "FN:Alice\r\n"
			}
			doc := mustDecode(t,
				"BEGIN:VCARD\r\nVERSION:"+string(version)+"\r\n"+fn+"END:VCARD\r\n",
			)
			require.NoError(t, Validate(doc))
			got, err := doc.Cards[0].Version()
			require.NoError(t, err)
			assert.Equal(t, version, got)
		})
	}
}

func TestValidateAcceptsAlternateNamesAndUnknownExtensions(t *testing.T) {
	doc := mustDecode(t,
		"BEGIN:VCARD\r\n"+
			"VERSION:4.0\r\n"+
			"FN;ALTID=1;LANGUAGE=en:Alice Example\r\n"+
			"FN;ALTID=1;LANGUAGE=fr:Alicia Example\r\n"+
			"X-FUTURE;X-PARAM=future:opaque\r\n"+
			"END:VCARD\r\n",
	)
	require.NoError(t, Validate(doc))
}

func TestValidateRejectsInvalidVersionAndEmptyDocument(t *testing.T) {
	require := require.New(t)
	err := Validate(Document{})
	require.Error(err)
	require.ErrorContains(err, "document contains no cards")

	doc := mustDecode(t,
		"BEGIN:VCARD\r\nVERSION:5.0\r\nFN:Alice\r\nEND:VCARD\r\n",
	)
	err = Validate(doc)
	require.Error(err)
	require.ErrorContains(err, "unsupported VERSION")

	err = Validate(Document{Cards: []Card{{Properties: []Property{
		{Name: "FN", RawValue: "Alice"},
	}}}})
	require.Error(err)
	require.ErrorContains(err, "requires exactly one VERSION")
}

func TestValidateChecksPreferenceAndIndexBounds(t *testing.T) {
	tests := []struct {
		name       string
		parameters string
		wantErr    string
	}{
		{name: "pref zero", parameters: "PREF=0", wantErr: "PREF must be an integer from 1 through 100"},
		{name: "pref high", parameters: "PREF=101", wantErr: "PREF must be an integer from 1 through 100"},
		{name: "pref text", parameters: "PREF=first", wantErr: "PREF must be an integer from 1 through 100"},
		{name: "index zero", parameters: "INDEX=0", wantErr: "INDEX must be a positive integer"},
		{name: "index text", parameters: "INDEX=first", wantErr: "INDEX must be a positive integer"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			doc := mustDecode(t,
				"BEGIN:VCARD\r\nVERSION:4.0\r\nFN;"+test.parameters+":Alice\r\nEND:VCARD\r\n",
			)
			err := Validate(doc)
			require.Error(t, err)
			assert.ErrorContains(t, err, test.wantErr)
		})
	}

	doc := mustDecode(t,
		"BEGIN:VCARD\r\nVERSION:4.0\r\nFN;PREF=100;INDEX=1:Alice\r\nEND:VCARD\r\n",
	)
	require.NoError(t, Validate(doc))
}

func TestValidateRejectsEmptySyntaxNamesInConstructedDocument(t *testing.T) {
	doc := Document{Cards: []Card{{Properties: []Property{
		{Name: "VERSION", RawValue: "4.0"},
		{Name: "", RawValue: "Alice"},
		{Name: "FN", RawValue: "Alice", Parameters: []Parameter{{Name: ""}}},
	}}}}
	err := Validate(doc)
	require.Error(t, err)
	require.ErrorContains(t, err, "empty property name")
	require.ErrorContains(t, err, "empty parameter name")
}

func TestValidateRejectsEmptyConstructedPreferenceAndIndexValues(t *testing.T) {
	doc := Document{Cards: []Card{{Properties: []Property{
		{Name: "VERSION", RawValue: "4.0"},
		{
			Name:     "FN",
			RawValue: "Alice",
			Parameters: []Parameter{
				{Name: "PREF"},
				{Name: "INDEX"},
			},
		},
	}}}}
	err := Validate(doc)
	require.Error(t, err)
	require.ErrorContains(t, err, "PREF must be an integer from 1 through 100")
	require.ErrorContains(t, err, "INDEX must be a positive integer")
}

func TestRegisteredValueLookups(t *testing.T) {
	assert := assert.New(t)
	snapshot, err := registry.Load()
	require.NoError(t, err)

	assert.True(IsRegisteredPropertyValue(snapshot, "kind", "DEVICE"))
	assert.True(IsRegisteredPropertyValue(snapshot, "BEGIN", "vcard"))
	assert.True(IsRegisteredPropertyValue(snapshot, "END", "VCARD"))
	assert.False(IsRegisteredPropertyValue(snapshot, "KIND", "future-kind"))

	assert.True(IsRegisteredParameterValue(snapshot, "tel", "type", "VOICE"))
	assert.True(IsRegisteredParameterValue(snapshot, "FN", "type", "work"))
	assert.False(IsRegisteredParameterValue(snapshot, "BDAY", "TYPE", "voice"))
	assert.False(IsRegisteredParameterValue(snapshot, "TEL", "TYPE", "future-type"))
}

func mustDecode(tb testing.TB, input string) Document {
	tb.Helper()
	doc, err := Decode(strings.NewReader(input))
	require.NoError(tb, err)
	return doc
}
