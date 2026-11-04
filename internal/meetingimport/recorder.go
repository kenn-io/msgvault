package meetingimport

import (
	"errors"
	"strings"
	"unicode"
)

// NormalizeRecorderSource applies the shared recorder registration contract.
func NormalizeRecorderSource(source Source) (Source, error) {
	probe := Request{Source: source, Meeting: Meeting{ExternalID: "validation", StartedAt: "2000-01-01T00:00:00Z", Transcript: "validation"}}
	normalized, err := probe.Normalize()
	if err != nil {
		return source, err
	}
	if strings.IndexFunc(normalized.Source.Identifier, unicode.IsControl) >= 0 {
		return source, errors.New("source identifier contains control characters")
	}
	return normalized.Source, nil
}
