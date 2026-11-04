package meetingimport

import (
	"encoding/json/v2"
	"errors"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
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

// DecodeRecorderJSON enforces the common recorder envelope limit and strict JSON.
func DecodeRecorderJSON[T any](reader io.Reader, limit int64, malformed, tooLarge error) (out T, err error) {
	if limit <= 0 {
		return out, tooLarge
	}
	if limit > MaxRequestBytes {
		limit = MaxRequestBytes
	}
	if reader == nil {
		return out, malformed
	}
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return out, malformed
	}
	if int64(len(data)) > limit {
		return out, tooLarge
	}
	if !utf8.Valid(data) || json.Unmarshal(data, &out, json.RejectUnknownMembers(true)) != nil {
		return out, malformed
	}
	return out, nil
}
