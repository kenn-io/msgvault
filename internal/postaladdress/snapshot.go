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

// DecodeOriginalInput recognizes only the exact tagged schema. False means
// callers must treat value as legacy/plain original text, without data loss.
func DecodeOriginalInput(value string) (Input, bool) {
	if !strings.HasPrefix(value, originalPrefix) {
		return Input{}, false
	}
	var snapshot originalSnapshot
	if err := json.Unmarshal([]byte(strings.TrimPrefix(value, originalPrefix)), &snapshot, json.RejectUnknownMembers(true)); err != nil || snapshot.Schema != 1 || snapshot.Input == nil {
		return Input{}, false
	}
	return *snapshot.Input, true
}
