package meetingimport

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

func DecodeRequest(r io.Reader, maxBytes int64) (Request, error) {
	if r == nil {
		return Request{}, fmt.Errorf("%w: empty body", ErrMalformedRequest)
	}
	if maxBytes <= 0 {
		return Request{}, ErrRequestTooLarge
	}

	body, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return Request{}, fmt.Errorf("%w: read body: %w", ErrMalformedRequest, err)
	}
	if int64(len(body)) > maxBytes {
		return Request{}, ErrRequestTooLarge
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return Request{}, fmt.Errorf("%w: empty body", ErrMalformedRequest)
	}
	if !utf8.Valid(body) {
		return Request{}, fmt.Errorf("%w: request must be valid UTF-8", ErrMalformedRequest)
	}

	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var req Request
	if err := decoder.Decode(&req); err != nil {
		return Request{}, fmt.Errorf("%w: %w", ErrMalformedRequest, err)
	}

	var trailing any
	err = decoder.Decode(&trailing)
	if err == nil {
		return Request{}, fmt.Errorf("%w: trailing JSON value", ErrMalformedRequest)
	}
	if !errors.Is(err, io.EOF) {
		return Request{}, fmt.Errorf("%w: trailing data: %w", ErrMalformedRequest, err)
	}
	return req, nil
}
