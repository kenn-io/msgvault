package vcard

import (
	"errors"
	"fmt"
	"strings"
)

const (
	maxParametersPerProperty = 100
	maxValuesPerParameter    = 100
)

var errSplitPartLimit = errors.New("split part count limit exceeded")

func parseContentLine(line string) (Property, error) {
	colon, err := delimiterOutsideQuotes(line, ':')
	if err != nil {
		return Property{}, err
	}
	if colon < 0 {
		return Property{}, errors.New("content line has no ':' delimiter")
	}
	left := line[:colon]
	rawValue := line[colon+1:]
	if containsInjection(rawValue) {
		return Property{}, errors.New("raw value contains CR, LF, or NUL")
	}
	parts, err := splitOutsideQuotesCapped(left, ';', maxParametersPerProperty+1)
	if err != nil {
		if errors.Is(err, errSplitPartLimit) {
			return Property{}, fmt.Errorf(
				"parameter count exceeds %d",
				maxParametersPerProperty,
			)
		}
		return Property{}, err
	}
	if len(parts) == 0 {
		return Property{}, errors.New("empty property name")
	}

	group, originalName := splitPropertyName(parts[0])
	property, err := NewProperty(group, originalName, rawValue)
	if err != nil {
		return Property{}, err
	}
	property.Parameters = make([]Parameter, 0, len(parts)-1)
	for _, rawParameter := range parts[1:] {
		parameter, err := parseParameter(rawParameter)
		if err != nil {
			return Property{}, err
		}
		property.Parameters = append(property.Parameters, parameter)
	}
	return property, nil
}

func splitPropertyName(raw string) (string, string) {
	group, name, found := strings.Cut(raw, ".")
	if !found {
		return "", raw
	}
	return group, name
}

func parseParameter(raw string) (Parameter, error) {
	equals, err := delimiterOutsideQuotes(raw, '=')
	if err != nil {
		return Parameter{}, err
	}
	if equals < 0 {
		if raw == "" {
			return Parameter{}, errors.New("empty parameter")
		}
		if containsInjection(raw) {
			return Parameter{}, errors.New("parameter value contains CR, LF, or NUL")
		}
		decoded := decodeRFC6868(raw)
		return Parameter{
			Name: "TYPE",
			Values: []ParameterValue{{
				Raw:      raw,
				Decoded:  decoded,
				RawValid: true,
			}},
		}, nil
	}

	originalName := raw[:equals]
	if !validToken(originalName) {
		if originalName == "" {
			return Parameter{}, errors.New("empty parameter name")
		}
		return Parameter{}, fmt.Errorf("invalid parameter name %q", originalName)
	}
	rawValues, err := splitOutsideQuotesCapped(
		raw[equals+1:],
		',',
		maxValuesPerParameter,
	)
	if err != nil {
		if errors.Is(err, errSplitPartLimit) {
			return Parameter{}, fmt.Errorf(
				"parameter value count exceeds %d",
				maxValuesPerParameter,
			)
		}
		return Parameter{}, err
	}
	parameter := Parameter{
		Name:         strings.ToUpper(originalName),
		OriginalName: originalName,
		Values:       make([]ParameterValue, 0, len(rawValues)),
	}
	for _, rawValue := range rawValues {
		value, err := parseParameterValue(rawValue)
		if err != nil {
			return Parameter{}, err
		}
		parameter.Values = append(parameter.Values, value)
	}
	return parameter, nil
}

func parseParameterValue(raw string) (ParameterValue, error) {
	quoted := len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"'
	if quoted {
		raw = raw[1 : len(raw)-1]
	}
	if strings.ContainsRune(raw, '"') {
		return ParameterValue{}, errors.New("unexpected quote in parameter value")
	}
	if containsInjection(raw) {
		return ParameterValue{}, errors.New("parameter value contains CR, LF, or NUL")
	}
	return ParameterValue{
		Raw:      raw,
		Decoded:  decodeRFC6868(raw),
		Quoted:   quoted,
		RawValid: true,
	}, nil
}

func delimiterOutsideQuotes(input string, delimiter byte) (int, error) {
	quoted := false
	for i := range len(input) {
		switch input[i] {
		case '"':
			quoted = !quoted
		case delimiter:
			if !quoted {
				return i, nil
			}
		}
	}
	if quoted {
		return -1, errors.New("unclosed quote")
	}
	return -1, nil
}

func splitOutsideQuotesCapped(input string, delimiter byte, maxParts int) ([]string, error) {
	if maxParts < 1 {
		return nil, errSplitPartLimit
	}
	initialCapacity := min(maxParts, 8)
	parts := make([]string, 0, initialCapacity)
	start := 0
	quoted := false
	for i := range len(input) {
		switch input[i] {
		case '"':
			quoted = !quoted
		case delimiter:
			if !quoted {
				if len(parts) >= maxParts-1 {
					return nil, errSplitPartLimit
				}
				parts = append(parts, input[start:i])
				start = i + 1
			}
		}
	}
	if quoted {
		return nil, errors.New("unclosed quote")
	}
	parts = append(parts, input[start:])
	return parts, nil
}

func decodeRFC6868(raw string) string {
	var decoded strings.Builder
	decoded.Grow(len(raw))
	for i := 0; i < len(raw); i++ {
		if raw[i] != '^' || i+1 >= len(raw) {
			decoded.WriteByte(raw[i])
			continue
		}
		switch raw[i+1] {
		case '^':
			decoded.WriteByte('^')
			i++
		case 'n', 'N':
			decoded.WriteByte('\n')
			i++
		case '\'':
			decoded.WriteByte('"')
			i++
		default:
			decoded.WriteByte('^')
		}
	}
	return decoded.String()
}
