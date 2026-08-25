package personfacts

import (
	"errors"
	"fmt"
	"strings"
)

const targetRevisionPrefix = "sha256:"
const targetRevisionHexLength = 64

// EncodeTargetRef returns the canonical diagnostic wire form for a target.
// The revision suffix is fixed-width, so keys may contain any number of colons.
func EncodeTargetRef(target TargetRef) (string, error) {
	if err := validateTargetRef(target); err != nil {
		return "", err
	}
	return string(target.Kind) + ":" + target.Key + ":" + target.Revision, nil
}

// DecodeTargetRef parses the canonical diagnostic wire form. It locates the
// fixed-width descriptor revision from the right instead of splitting the
// colon-bearing key.
func DecodeTargetRef(encoded string) (TargetRef, error) {
	if encoded == "" || encoded != strings.TrimSpace(encoded) {
		return TargetRef{}, errors.New("target must be kind:key:sha256:<64 lowercase hex characters>")
	}
	kindValue, remainder, found := strings.Cut(encoded, ":")
	if !found {
		return TargetRef{}, errors.New("target must be kind:key:sha256:<64 lowercase hex characters>")
	}
	kind, err := decodeTargetKind(kindValue)
	if err != nil {
		return TargetRef{}, err
	}

	revisionLength := len(targetRevisionPrefix) + targetRevisionHexLength
	separatorIndex := len(remainder) - revisionLength - 1
	if separatorIndex < 0 || remainder[separatorIndex] != ':' {
		return TargetRef{}, targetRevisionError()
	}
	target := TargetRef{
		Kind: kind, Key: remainder[:separatorIndex], Revision: remainder[separatorIndex+1:],
	}
	if err := validateTargetRef(target); err != nil {
		return TargetRef{}, err
	}
	return target, nil
}

func validateTargetRef(target TargetRef) error {
	if _, err := decodeTargetKind(string(target.Kind)); err != nil {
		return err
	}
	if target.Key == "" || target.Key != strings.TrimSpace(target.Key) {
		return errors.New("target key must not be empty or contain surrounding whitespace")
	}
	if !validTargetRevision(target.Revision) {
		return targetRevisionError()
	}
	return nil
}

func decodeTargetKind(value string) (TargetKind, error) {
	kind := TargetKind(value)
	switch kind {
	case TargetAttribute, TargetEmployment:
		return kind, nil
	default:
		return "", fmt.Errorf("unknown person fact target kind %q", value)
	}
}

func validTargetRevision(revision string) bool {
	if len(revision) != len(targetRevisionPrefix)+targetRevisionHexLength ||
		!strings.HasPrefix(revision, targetRevisionPrefix) {
		return false
	}
	for _, value := range revision[len(targetRevisionPrefix):] {
		if (value < '0' || value > '9') && (value < 'a' || value > 'f') {
			return false
		}
	}
	return true
}

func targetRevisionError() error {
	return errors.New("target revision must be sha256 followed by 64 lowercase hexadecimal characters")
}
