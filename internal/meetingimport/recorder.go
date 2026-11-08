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
	normalized, err := normalizeSource(source)
	if err != nil {
		return source, err
	}
	if strings.IndexFunc(normalized.Identifier, unicode.IsControl) >= 0 {
		return source, errors.New("source identifier contains control characters")
	}
	return normalized, nil
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
