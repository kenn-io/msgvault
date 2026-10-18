package store

import (
	"fmt"
	"strconv"
	"strings"
)

// ParsePersonMeetingRef accepts saved person IDs and observed participant IDs.
func ParsePersonMeetingRef(ref string) (string, int64, error) {
	kind, value := string(AttributeObjectPerson), ref
	if before, after, found := strings.Cut(ref, ":"); found {
		kind, value = before, after
	}
	if kind != string(AttributeObjectPerson) && kind != "participant" {
		return "", 0, fmt.Errorf("%w: invalid person reference", ErrInvalidArgument)
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return "", 0, fmt.Errorf("%w: invalid person reference", ErrInvalidArgument)
		}
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 || id > 9007199254740991 {
		return "", 0, fmt.Errorf("%w: invalid person reference", ErrInvalidArgument)
	}
	return kind, id, nil
}
