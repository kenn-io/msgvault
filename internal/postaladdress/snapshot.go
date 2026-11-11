package postaladdress

import (
	"encoding/json/v2"
	"errors"
	"strings"
	"unicode/utf8"
)

const originalPrefix = "msgvault:postal-input:v1\n"

// ErrUnsupportedEnvelope marks a postal input envelope this reader cannot decode.
var ErrUnsupportedEnvelope = errors.New("unsupported postal input envelope")

type originalSnapshot struct {
	Schema int    `json:"schema"`
	Input  *Input `json:"input"`
}

// EncodeOriginalInput retains the raw draft, including whitespace. Invalid
// UTF-8 is rejected rather than silently replaced by encoding/json.
func EncodeOriginalInput(input Input) (string, error) {
	for _, field := range input.fields() {
		if !utf8.ValidString(*field) {
			return "", errors.New("postal input contains invalid UTF-8")
		}
	}
	data, err := json.Marshal(originalSnapshot{Schema: 1, Input: &input})
	if err != nil {
		return "", err
	}
	return originalPrefix + string(data), nil
}

// DecodeOriginalInput distinguishes legacy text from stored postal envelopes.
func DecodeOriginalInput(value string) (Input, bool, error) {
	if !strings.HasPrefix(value, "msgvault:postal-input:") {
		return Input{}, false, nil
	}
	if !strings.HasPrefix(value, originalPrefix) {
		return Input{}, false, ErrUnsupportedEnvelope
	}
	var snapshot originalSnapshot
	if err := json.Unmarshal([]byte(strings.TrimPrefix(value, originalPrefix)), &snapshot, json.RejectUnknownMembers(true)); err != nil {
		return Input{}, false, errors.Join(ErrUnsupportedEnvelope, err)
	}
	if snapshot.Schema != 1 || snapshot.Input == nil {
		return Input{}, false, errors.Join(ErrUnsupportedEnvelope, errors.New("invalid postal input envelope"))
	}
	return *snapshot.Input, true, nil
}

// DecodeStoredOriginalInput is the read-side decode. An envelope of an
// unreadable version has no recoverable source, so it reads as an empty
// snapshot: its text is never exposed and it cannot hide other addresses.
// Writes keep using DecodeOriginalInput, which rejects it.
func DecodeStoredOriginalInput(value string) (Input, bool) {
	input, snapshot, err := DecodeOriginalInput(value)
	if err != nil {
		return Input{}, true
	}
	return input, snapshot
}
