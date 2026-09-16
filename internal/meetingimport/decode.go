package meetingimport

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

	decoder := jsontext.NewDecoder(bytes.NewReader(body), json.RejectUnknownMembers(true), jsonexact.PreserveNumbers)

	var req Request
	if err := json.UnmarshalDecode(decoder, &req); err != nil {
		return Request{}, fmt.Errorf("%w: %w", ErrMalformedRequest, err)
	}

	var trailing any
	err = json.UnmarshalDecode(decoder, &trailing)
	if err == nil {
		return Request{}, fmt.Errorf("%w: trailing JSON value", ErrMalformedRequest)
	}
	if !errors.Is(err, io.EOF) {
		return Request{}, fmt.Errorf("%w: trailing data: %w", ErrMalformedRequest, err)
	}
	return req, nil
}
