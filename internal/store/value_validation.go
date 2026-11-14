package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"reflect"
	"strings"

	"go.kenn.io/msgvault/internal/jsonexact"
)

func equalJSON(first, second []byte) bool {
	decode := func(value []byte) (any, error) {
		decoder := jsontext.NewDecoder(bytes.NewReader(value), jsonexact.PreserveNumbers)

		var decoded any
		if err := json.UnmarshalDecode(decoder, &decoded); err != nil {
			return nil, err
		}
		return decoded, nil
	}
	firstValue, firstErr := decode(first)
	secondValue, secondErr := decode(second)
	return firstErr == nil && secondErr == nil && reflect.DeepEqual(firstValue, secondValue)
}

func validLowerSHA256(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
