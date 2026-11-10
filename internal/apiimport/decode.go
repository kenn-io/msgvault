// Package apiimport holds what the HTTP API's single-item importers share:
// strict request decoding, field validation, and the source and sync-run
// bookkeeping around one archive write.
package apiimport

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/jsonexact"
)

// MaxRequestBytes bounds one import request body.
const MaxRequestBytes = int64(16 << 20)

var (
	ErrMalformedRequest = errors.New("malformed import request")
	ErrRequestTooLarge  = errors.New("import request too large")
)

// Decode reads one strict JSON request of at most maxBytes into T. Unknown
// members are rejected everywhere except inside free-form `any` values, whose
// numbers keep their exact text.
func Decode[T any](r io.Reader, maxBytes int64) (T, error) {
	var req T
	if r == nil {
		return req, fmt.Errorf("%w: empty body", ErrMalformedRequest)
	}
	if maxBytes <= 0 {
		return req, ErrRequestTooLarge
	}

	body, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return req, fmt.Errorf("%w: read body: %w", ErrMalformedRequest, err)
	}
	if int64(len(body)) > maxBytes {
		return req, ErrRequestTooLarge
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return req, fmt.Errorf("%w: empty body", ErrMalformedRequest)
	}
	if !utf8.Valid(body) {
		return req, fmt.Errorf("%w: request must be valid UTF-8", ErrMalformedRequest)
	}

	decoder := jsontext.NewDecoder(bytes.NewReader(body), json.RejectUnknownMembers(true), jsonexact.PreserveNumbers)
	if err := json.UnmarshalDecode(decoder, &req); err != nil {
		var zero T
		return zero, fmt.Errorf("%w: %w", ErrMalformedRequest, err)
	}

	var trailing any
	err = json.UnmarshalDecode(decoder, &trailing)
	if err == nil {
		var zero T
		return zero, fmt.Errorf("%w: trailing JSON value", ErrMalformedRequest)
	}
	if !errors.Is(err, io.EOF) {
		var zero T
		return zero, fmt.Errorf("%w: trailing data: %w", ErrMalformedRequest, err)
	}
	return req, nil
}
