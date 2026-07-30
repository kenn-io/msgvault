// Package registry provides the vendored IANA vCard Elements registry.
package registry

import (
	"bytes"
	"embed"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

// Files contains the serialized registry tables and their metadata.
type Files struct {
	Metadata        []byte
	Properties      []byte
	Parameters      []byte
	ValueDataTypes  []byte
	PropertyValues  []byte
	ParameterValues []byte
}

// Element is a named entry in a registry table.
type Element struct {
	Namespace string
	Name      string
	Reference string
}

// PropertyValue is a registered value for a vCard property.
type PropertyValue struct {
	Property  string
	Value     string
	Reference string
}

// ParameterValue is a registered parameter value and its applicable properties.
type ParameterValue struct {
	Properties []string
	Parameter  string
	Value      string
	Reference  string
}

// Snapshot is a validated view of all five IANA vCard registry tables.
type Snapshot struct {
	Source          string
	Updated         string
	Properties      []Element
	Parameters      []Element
	ValueDataTypes  []Element
	PropertyValues  []PropertyValue
	ParameterValues []ParameterValue
}

type metadata struct {
	Source  string `json:"source"`
	Updated string `json:"updated"`
}

//go:embed data/*.csv data/metadata.json
var embeddedData embed.FS

// Parse validates and parses a complete registry snapshot.
func Parse(files Files) (Snapshot, error) {
	meta, err := parseMetadata(files.Metadata)
	if err != nil {
		return Snapshot{}, err
	}
	properties, err := parseElements("properties", files.Properties, "Property")
	if err != nil {
		return Snapshot{}, err
	}
	parameters, err := parseElements("parameters", files.Parameters, "Parameter")
	if err != nil {
		return Snapshot{}, err
	}
	valueDataTypes, err := parseElements("value-data-types", files.ValueDataTypes, "Value Data Type")
	if err != nil {
		return Snapshot{}, err
	}
	propertyValues, err := parsePropertyValues(files.PropertyValues)
	if err != nil {
		return Snapshot{}, err
	}
	properties = promoteFramingProperties(properties, propertyValues)
	parameterValues, err := parseParameterValues(files.ParameterValues)
	if err != nil {
		return Snapshot{}, err
	}

	return Snapshot{
		Source:          meta.Source,
		Updated:         meta.Updated,
		Properties:      properties,
		Parameters:      parameters,
		ValueDataTypes:  valueDataTypes,
		PropertyValues:  propertyValues,
		ParameterValues: parameterValues,
	}, nil
}

// Load loads the embedded registry snapshot.
func Load() (Snapshot, error) {
	read := func(name string) ([]byte, error) {
		data, err := embeddedData.ReadFile("data/" + name)
		if err != nil {
			return nil, fmt.Errorf("read embedded %s: %w", name, err)
		}
		return data, nil
	}
	metadataData, err := read("metadata.json")
	if err != nil {
		return Snapshot{}, err
	}
	properties, err := read("properties.csv")
	if err != nil {
		return Snapshot{}, err
	}
	parameters, err := read("parameters.csv")
	if err != nil {
		return Snapshot{}, err
	}
	valueDataTypes, err := read("value-data-types.csv")
	if err != nil {
		return Snapshot{}, err
	}
	propertyValues, err := read("property-values.csv")
	if err != nil {
		return Snapshot{}, err
	}
	parameterValues, err := read("parameter-values.csv")
	if err != nil {
		return Snapshot{}, err
	}
	return Parse(Files{
		Metadata:        metadataData,
		Properties:      properties,
		Parameters:      parameters,
		ValueDataTypes:  valueDataTypes,
		PropertyValues:  propertyValues,
		ParameterValues: parameterValues,
	})
}

func promoteFramingProperties(
	properties []Element,
	values []PropertyValue,
) []Element {
	existing := make(map[string]struct{}, len(properties))
	for _, property := range properties {
		existing[property.Name] = struct{}{}
	}
	var framing []Element
	for _, registered := range values {
		if !strings.EqualFold(registered.Value, "VCARD") {
			continue
		}
		name := strings.ToUpper(registered.Property)
		if name != "BEGIN" && name != "END" {
			continue
		}
		if _, ok := existing[name]; ok {
			continue
		}
		existing[name] = struct{}{}
		framing = append(framing, Element{Name: name, Reference: registered.Reference})
	}
	return append(framing, properties...)
}

func parseMetadata(data []byte) (metadata, error) {
	var got metadata
	if err := json.Unmarshal(trimBOM(data), &got); err != nil {
		return metadata{}, fmt.Errorf("metadata: %w", err)
	}
	if strings.TrimSpace(got.Source) == "" {
		return metadata{}, errors.New("metadata source is empty")
	}
	parsed, err := time.Parse("2006-01-02", got.Updated)
	if err != nil || parsed.Format("2006-01-02") != got.Updated {
		return metadata{}, fmt.Errorf("metadata updated %q is not YYYY-MM-DD", got.Updated)
	}
	return got, nil
}

func parseElements(table string, data []byte, nameColumn string) ([]Element, error) {
	rows, headers, err := readTable(table, data)
	if err != nil {
		return nil, err
	}
	nameIndex, err := requireColumn(table, headers, nameColumn)
	if err != nil {
		return nil, err
	}
	namespaceIndex := columnIndex(headers, "Namespace")
	referenceIndex, err := requireColumn(table, headers, "Reference")
	if err != nil {
		return nil, err
	}

	elements := make([]Element, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for i, row := range rows {
		record := i + 2
		name := strings.ToUpper(strings.TrimSpace(row[nameIndex]))
		if name == "" {
			return nil, fmt.Errorf("%s record %d: empty %s", table, record, strings.ToLower(nameColumn))
		}
		namespace := ""
		if namespaceIndex >= 0 {
			namespace = strings.TrimSpace(row[namespaceIndex])
		}
		key := strings.ToUpper(namespace) + "\x00" + name
		if _, exists := seen[key]; exists {
			return nil, fmt.Errorf("duplicate %s %s", singularTableName(table), name)
		}
		seen[key] = struct{}{}
		elements = append(elements, Element{
			Namespace: namespace,
			Name:      name,
			Reference: strings.TrimSpace(row[referenceIndex]),
		})
	}
	return elements, nil
}

func parsePropertyValues(data []byte) ([]PropertyValue, error) {
	const table = "property-values"
	rows, headers, err := readTable(table, data)
	if err != nil {
		return nil, err
	}
	propertyIndex, err := requireColumn(table, headers, "Property")
	if err != nil {
		return nil, err
	}
	valueIndex, err := requireColumn(table, headers, "Value")
	if err != nil {
		return nil, err
	}
	referenceIndex, err := requireColumn(table, headers, "Reference")
	if err != nil {
		return nil, err
	}

	values := make([]PropertyValue, 0, len(rows))
	for i, row := range rows {
		record := i + 2
		property := strings.ToUpper(strings.TrimSpace(row[propertyIndex]))
		value := strings.TrimSpace(row[valueIndex])
		if property == "" || value == "" {
			return nil, fmt.Errorf("%s record %d: property and value are required", table, record)
		}
		values = append(values, PropertyValue{
			Property:  property,
			Value:     value,
			Reference: strings.TrimSpace(row[referenceIndex]),
		})
	}
	return values, nil
}

func parseParameterValues(data []byte) ([]ParameterValue, error) {
	const table = "parameter-values"
	rows, headers, err := readTable(table, data)
	if err != nil {
		return nil, err
	}
	propertyIndex, err := requireColumn(table, headers, "Property")
	if err != nil {
		return nil, err
	}
	parameterIndex, err := requireColumn(table, headers, "Parameter")
	if err != nil {
		return nil, err
	}
	valueIndex, err := requireColumn(table, headers, "Value")
	if err != nil {
		return nil, err
	}
	referenceIndex, err := requireColumn(table, headers, "Reference")
	if err != nil {
		return nil, err
	}

	values := make([]ParameterValue, 0, len(rows))
	for i, row := range rows {
		record := i + 2
		properties := parseApplicability(row[propertyIndex])
		parameter := strings.ToUpper(strings.TrimSpace(row[parameterIndex]))
		value := strings.TrimSpace(row[valueIndex])
		if len(properties) == 0 || parameter == "" || value == "" {
			return nil, fmt.Errorf("%s record %d: property, parameter, and value are required", table, record)
		}
		values = append(values, ParameterValue{
			Properties: properties,
			Parameter:  parameter,
			Value:      value,
			Reference:  strings.TrimSpace(row[referenceIndex]),
		})
	}
	return values, nil
}

func readTable(table string, data []byte) ([][]string, []string, error) {
	reader := csv.NewReader(bytes.NewReader(trimBOM(data)))
	reader.FieldsPerRecord = -1
	headers, err := reader.Read()
	if err != nil {
		return nil, nil, fmt.Errorf("%s record 1: %w", table, err)
	}
	for i := range headers {
		headers[i] = strings.TrimSpace(headers[i])
	}
	if len(headers) == 0 {
		return nil, nil, fmt.Errorf("%s has no columns", table)
	}

	var rows [][]string
	for record := 2; ; record++ {
		row, readErr := reader.Read()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, nil, fmt.Errorf("%s record %d: %w", table, record, readErr)
		}
		if len(row) != len(headers) {
			return nil, nil, fmt.Errorf(
				"%s record %d: got %d columns, want %d",
				table, record, len(row), len(headers),
			)
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil, nil, fmt.Errorf("%s table is empty", table)
	}
	return rows, headers, nil
}

func requireColumn(table string, headers []string, name string) (int, error) {
	index := columnIndex(headers, name)
	if index < 0 {
		return -1, fmt.Errorf("%s: missing %q column", table, name)
	}
	return index, nil
}

func columnIndex(headers []string, name string) int {
	return slices.IndexFunc(headers, func(header string) bool {
		return strings.EqualFold(header, name)
	})
}

func parseApplicability(raw string) []string {
	raw = strings.ReplaceAll(raw, "\n", " ")
	parts := strings.Split(raw, ",")
	properties := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		part = strings.TrimSpace(strings.TrimPrefix(strings.ToLower(part), "and "))
		if part == "" || part == "..." {
			continue
		}
		properties = append(properties, strings.ToUpper(part))
	}
	return properties
}

func singularTableName(table string) string {
	switch table {
	case "properties":
		return "property"
	case "parameters":
		return "parameter"
	case "value-data-types":
		return "value data type"
	default:
		return strings.TrimSuffix(table, "s")
	}
}

func trimBOM(data []byte) []byte {
	return bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
}
