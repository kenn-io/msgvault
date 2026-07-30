package registry

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAllRegistryTables(t *testing.T) {
	assert := assert.New(t)
	files := Files{
		Metadata:        []byte(`{"source":"https://www.iana.org/assignments/vcard-elements/vcard-elements.xhtml","updated":"2026-01-13"}`),
		Properties:      []byte("Namespace,Property,Reference\n,SOURCE,\"[RFC6350, Section 6.1.3]\"\n"),
		Parameters:      []byte("Namespace,Parameter,Reference\n,PREF,\"[RFC6350, Section 5.3]\"\n"),
		ValueDataTypes:  []byte("Value Data Type,Reference\nTEXT,\"[RFC6350, Section 4.1]\"\n"),
		PropertyValues:  []byte("Property,Value,Reference\nKIND,individual,\"[RFC6350, Section 6.1.4]\"\n"),
		ParameterValues: []byte("Property,Parameter,Value,Reference\nTEL,TYPE,voice,\"[RFC6350, Section 6.4.1]\"\n"),
	}

	got, err := Parse(files)
	require.NoError(t, err)
	assert.Equal("2026-01-13", got.Updated)
	assert.Equal("SOURCE", got.Properties[0].Name)
	assert.Equal("PREF", got.Parameters[0].Name)
	assert.Equal("TEXT", got.ValueDataTypes[0].Name)
	assert.Equal("individual", got.PropertyValues[0].Value)
	assert.Equal("voice", got.ParameterValues[0].Value)
}

func TestParseRejectsDuplicateElementNames(t *testing.T) {
	files := minimalFiles()
	files.Properties = []byte(
		"Namespace,Property,Reference\n,SOURCE,[RFC6350]\n,SOURCE,[RFC6350]\n",
	)

	_, err := Parse(files)
	require.Error(t, err)
	assert.ErrorContains(t, err, "duplicate property SOURCE")
}

func TestParseNormalizesNamesAndKeepsSourceOrder(t *testing.T) {
	assert := assert.New(t)
	files := minimalFiles()
	files.Properties = []byte(
		"\xef\xbb\xbfNamespace,Property,Reference\n,source,[RFC6350]\nx-example,custom,[Example]\n",
	)
	files.ParameterValues = []byte(
		"Property,Parameter,Value,Reference\n\"ADR, N\",type,home,[RFC6350]\n\"FN, ..., and CALURI\",type,work,[RFC6350]\n",
	)

	got, err := Parse(files)
	require.NoError(t, err)
	assert.Equal([]Element{
		{Namespace: "", Name: "SOURCE", Reference: "[RFC6350]"},
		{Namespace: "x-example", Name: "CUSTOM", Reference: "[Example]"},
	}, got.Properties)
	assert.Equal([]string{"ADR", "N"}, got.ParameterValues[0].Properties)
	assert.Equal([]string{"FN", "CALURI"}, got.ParameterValues[1].Properties)
	assert.Equal("TYPE", got.ParameterValues[0].Parameter)
	assert.Equal("home", got.ParameterValues[0].Value)
}

func TestParsePreservesDuplicateRegisteredValueRows(t *testing.T) {
	assert := assert.New(t)
	files := minimalFiles()
	files.ParameterValues = []byte(
		"Property,Parameter,Value,Reference\n" +
			"RELATED,TYPE,emergency,\"[RFC6350, Section 6.6.6]\"\n" +
			"RELATED,TYPE,emergency,\"[RFC6350, Section 6.6.6]\"\n",
	)

	got, err := Parse(files)
	require.NoError(t, err)
	assert.Len(got.ParameterValues, 2)
	assert.Equal("emergency", got.ParameterValues[0].Value)
	assert.Equal("emergency", got.ParameterValues[1].Value)
}

func TestParsePromotesRegisteredFramingValuesToProperties(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	files := minimalFiles()
	files.PropertyValues = []byte(
		"Property,Value,Reference\n" +
			"BEGIN,VCARD,\"[RFC6350, Section 6.1.1]\"\n" +
			"END,VCARD,\"[RFC6350, Section 6.1.2]\"\n" +
			"KIND,individual,\"[RFC6350, Section 6.1.4]\"\n",
	)

	got, err := Parse(files)
	require.NoError(err)
	require.Equal([]string{"BEGIN", "END", "SOURCE"}, elementNames(got.Properties))
	assert.Equal("[RFC6350, Section 6.1.1]", got.Properties[0].Reference)
	assert.Equal("[RFC6350, Section 6.1.2]", got.Properties[1].Reference)
}

func TestLoadOfficialSnapshot(t *testing.T) {
	assert := assert.New(t)
	snapshot, err := Load()
	require.NoError(t, err)
	assert.Equal("2026-01-13", snapshot.Updated)
	assert.Len(snapshot.Properties, 52)
	assert.Len(snapshot.Parameters, 27)
	assert.NotEmpty(snapshot.ValueDataTypes)
	assert.NotEmpty(snapshot.PropertyValues)
	assert.NotEmpty(snapshot.ParameterValues)
	assert.Contains(elementNames(snapshot.Properties), "BEGIN")
	assert.Contains(elementNames(snapshot.Properties), "END")
	assert.Contains(elementNames(snapshot.Properties), "BIRTHPLACE")
	assert.Contains(elementNames(snapshot.Properties), "DEATHDATE")
	assert.Contains(elementNames(snapshot.Properties), "JSPROP")
	assert.Contains(elementNames(snapshot.Parameters), "PROP-ID")
	assert.Contains(elementNames(snapshot.Parameters), "JSCOMPS")
	assert.Contains(propertyValues(snapshot, "KIND"), "device")
	assert.Contains(propertyValues(snapshot, "GRAMGENDER"), "inanimate")
}

func TestParseRejectsMalformedRegistryData(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Files)
		wantErr string
	}{
		{
			name: "invalid metadata date",
			mutate: func(files *Files) {
				files.Metadata = []byte(`{"source":"https://example.invalid","updated":"01/13/2026"}`)
			},
			wantErr: "metadata updated",
		},
		{
			name: "empty property name",
			mutate: func(files *Files) {
				files.Properties = []byte("Namespace,Property,Reference\n,,[RFC6350]\n")
			},
			wantErr: "properties record 2",
		},
		{
			name: "malformed property row",
			mutate: func(files *Files) {
				files.Properties = []byte("Namespace,Property,Reference\n,SOURCE\n")
			},
			wantErr: "properties record 2",
		},
		{
			name: "empty parameter table",
			mutate: func(files *Files) {
				files.Parameters = []byte("Namespace,Parameter,Reference\n")
			},
			wantErr: "parameters table is empty",
		},
		{
			name: "empty property value",
			mutate: func(files *Files) {
				files.PropertyValues = []byte("Property,Value,Reference\nKIND,,[RFC6350]\n")
			},
			wantErr: "property-values record 2",
		},
		{
			name: "empty parameter applicability",
			mutate: func(files *Files) {
				files.ParameterValues = []byte("Property,Parameter,Value,Reference\n,TYPE,home,[RFC6350]\n")
			},
			wantErr: "parameter-values record 2",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			files := minimalFiles()
			test.mutate(&files)

			_, err := Parse(files)
			require.Error(t, err)
			assert.ErrorContains(t, err, test.wantErr)
		})
	}
}

func elementNames(elements []Element) []string {
	names := make([]string, 0, len(elements))
	for _, element := range elements {
		names = append(names, element.Name)
	}
	return names
}

func propertyValues(snapshot Snapshot, property string) []string {
	var values []string
	for _, registered := range snapshot.PropertyValues {
		if strings.EqualFold(registered.Property, property) {
			values = append(values, registered.Value)
		}
	}
	return values
}

func minimalFiles() Files {
	return Files{
		Metadata:        []byte(`{"source":"https://example.invalid/vcard-elements.xhtml","updated":"2026-01-13"}`),
		Properties:      []byte("Namespace,Property,Reference\n,SOURCE,[RFC6350]\n"),
		Parameters:      []byte("Namespace,Parameter,Reference\n,PREF,[RFC6350]\n"),
		ValueDataTypes:  []byte("Value Data Type,Reference\nTEXT,[RFC6350]\n"),
		PropertyValues:  []byte("Property,Value,Reference\nKIND,individual,[RFC6350]\n"),
		ParameterValues: []byte("Property,Parameter,Value,Reference\nTEL,TYPE,voice,[RFC6350]\n"),
	}
}
