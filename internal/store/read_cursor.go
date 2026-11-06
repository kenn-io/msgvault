package store

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"strings"
)

var errEncodedReadCursorTooLarge = errors.New("encoded position is too large")

// ReadFilterHash hashes the JSON representation of a read filter.
func ReadFilterHash(filter any) (string, error) {
	encoded, err := json.Marshal(filter)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// EncodeReadCursor encodes an opaque read position within the supplied byte limit.
func EncodeReadCursor(cursor any, maxBytes int, options ...json.Options) (string, error) {
	data, err := json.Marshal(cursor, options...)
	if err != nil {
		return "", err
	}
	if len(data) > maxBytes {
		return "", errEncodedReadCursorTooLarge
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

// DecodeReadCursor reads one strict base64url JSON position within the byte limit.
func DecodeReadCursor(value string, cursor any, maxBytes int, options ...json.Options) error {
	if len(value) > base64.RawURLEncoding.EncodedLen(maxBytes) {
		return errors.New("cursor is too large")
	}
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(data) > maxBytes {
		return errors.New("cursor is not valid base64url JSON")
	}
	decoder := jsontext.NewDecoder(strings.NewReader(string(data)))
	if err := json.UnmarshalDecode(decoder, cursor, append([]json.Options{json.RejectUnknownMembers(true)}, options...)...); err != nil {
		return errors.New("cursor has an invalid shape")
	}
	if _, err := decoder.ReadValue(); !errors.Is(err, io.EOF) {
		return errors.New("cursor has trailing data")
	}
	return nil
}
