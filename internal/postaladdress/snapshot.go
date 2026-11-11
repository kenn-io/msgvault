package postaladdress

import (
	"encoding/json/v2"
	"errors"
	"strings"
	"unicode/utf8"
)

const originalPrefix = "msgvault:postal-input:v1\n"

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
		return Input{}, false, errors.New("unsupported postal input envelope")
	}
	var snapshot originalSnapshot
	if err := json.Unmarshal([]byte(strings.TrimPrefix(value, originalPrefix)), &snapshot, json.RejectUnknownMembers(true)); err != nil {
		return Input{}, false, err
	}
	if snapshot.Schema != 1 || snapshot.Input == nil {
		return Input{}, false, errors.New("invalid postal input envelope")
	}
	return *snapshot.Input, true, nil
}
