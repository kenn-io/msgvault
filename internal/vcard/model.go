package vcard

import (
	"errors"
	"fmt"
	"strings"
)

// Version is a supported vCard syntax version.
type Version string

const (
	Version21 Version = "2.1"
	Version30 Version = "3.0"
	Version40 Version = "4.0"
)

// Document is an ordered collection of vCards.
type Document struct {
	Cards []Card
}

// Card is an ordered collection of content-line properties.
type Card struct {
	Properties []Property
}

// Property is one losslessly decoded vCard content line.
type Property struct {
	Group        string
	Name         string
	OriginalName string
	Parameters   []Parameter
	RawValue     string
}

// Parameter is one ordered property parameter.
type Parameter struct {
	Name         string
	OriginalName string
	Values       []ParameterValue
}

// ParameterValue retains source syntax and a decoded lookup value.
type ParameterValue struct {
	Raw      string
	Decoded  string
	Quoted   bool
	RawValid bool
}

// DecodeOptions bounds decoder resource use and compatibility behavior.
type DecodeOptions struct {
	MaxPhysicalLineBytes int
	MaxLogicalLineBytes  int
	MaxCards             int
	AllowV21             bool
}

// ParseError locates a malformed vCard input.
type ParseError struct {
	PhysicalLine int
	CardIndex    int
	Err          error
}

// Error implements error.
func (e *ParseError) Error() string {
	if e == nil {
		return "<nil>"
	}
	location := ""
	if e.PhysicalLine > 0 {
		location = fmt.Sprintf("physical line %d", e.PhysicalLine)
	}
	if e.CardIndex > 0 {
		if location != "" {
			location += fmt.Sprintf(", card %d", e.CardIndex)
		} else {
			location = fmt.Sprintf("card %d", e.CardIndex)
		}
	}
	if location == "" {
		return e.Err.Error()
	}
	return location + ": " + e.Err.Error()
}

// Unwrap returns the underlying syntax error.
func (e *ParseError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// PropertiesNamed returns properties matching name without regard to case.
func (c Card) PropertiesNamed(name string) []Property {
	var matches []Property
	for _, property := range c.Properties {
		if strings.EqualFold(property.Name, name) {
			matches = append(matches, property)
		}
	}
	return matches
}

// ParametersNamed returns parameters matching name without regard to case.
func (p Property) ParametersNamed(name string) []Parameter {
	var matches []Parameter
	for _, parameter := range p.Parameters {
		if strings.EqualFold(parameter.Name, name) {
			matches = append(matches, parameter)
		}
	}
	return matches
}

// NewProperty constructs a normalized property while retaining supplied
// spelling for rendering.
func NewProperty(group, name, rawValue string) (Property, error) {
	if group != "" && !validToken(group) {
		return Property{}, fmt.Errorf("invalid group name %q", group)
	}
	if !validToken(name) {
		if name == "" {
			return Property{}, errors.New("empty property name")
		}
		return Property{}, fmt.Errorf("invalid property name %q", name)
	}
	return Property{
		Group:        group,
		Name:         strings.ToUpper(name),
		OriginalName: name,
		RawValue:     rawValue,
	}, nil
}

// NewParameter constructs a normalized parameter whose values need encoding.
func NewParameter(name string, values ...string) (Parameter, error) {
	if !validToken(name) {
		if name == "" {
			return Parameter{}, errors.New("empty parameter name")
		}
		return Parameter{}, fmt.Errorf("invalid parameter name %q", name)
	}
	parameter := Parameter{
		Name:         strings.ToUpper(name),
		OriginalName: name,
		Values:       make([]ParameterValue, 0, len(values)),
	}
	for _, value := range values {
		parameter.Values = append(parameter.Values, ParameterValue{Decoded: value})
	}
	return parameter, nil
}

func validToken(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return false
		}
	}
	return true
}
