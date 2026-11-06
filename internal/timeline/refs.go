package timeline

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calrecur"
	"go.kenn.io/msgvault/internal/store"
)

var fullRefPattern = regexp.MustCompile(`^(msg|conv|evt):([1-9][0-9]*)(?:@([0-9TZ]+))?$`)
var shortRefPattern = regexp.MustCompile(`^(#|c|e)([1-9][0-9]*)(?:@([0-9TZ]+))?$`)

func FormatShortRef(s string) string {
	m := fullRefPattern.FindStringSubmatch(s)
	if m == nil {
		return s
	}
	prefix := map[string]string{"msg": "#", "conv": "c", "evt": "e"}[m[1]]
	key := m[3]
	if len(key) == 16 && strings.HasSuffix(key, "00Z") {
		key = key[:13] + "Z"
	}
	if key != "" {
		return prefix + m[2] + "@" + key
	}
	return prefix + m[2]
}
func ExpandShortRef(s string) string {
	m := shortRefPattern.FindStringSubmatch(s)
	if m == nil {
		return s
	}
	key := m[3]
	if len(key) == 14 && strings.HasSuffix(key, "Z") {
		key = key[:13] + "00Z"
	}
	ref := map[string]string{"#": "msg:", "c": "conv:", "e": "evt:"}[m[1]] + m[2]
	if key != "" {
		ref += "@" + key
	}
	return ref
}

// AnchorRef is a parsed reference; Time is populated only for date/datetime forms.
type AnchorRef struct {
	Ref, Kind                    string
	ID, SourceID, ConversationID int64
	LocalDate                    string
	Time                         time.Time
}

// personReference splits a with value into its kind and value; bare values are
// names, or phone numbers when they start with +.
func personReference(with string) (store.PersonRefKind, string) {
	kind, value := "name", strings.TrimSpace(with)
	if k, v, ok := strings.Cut(value, ":"); ok {
		kind, value = k, v
	} else if strings.HasPrefix(value, "+") {
		kind = "phone"
	}
	return store.PersonRefKind(kind), value
}
func ParseAnchor(s string, loc *time.Location) (AnchorRef, error) {
	if loc == nil {
		loc = time.UTC
	}
	s = ExpandShortRef(strings.TrimSpace(s))
	a := AnchorRef{Ref: s}
	if strings.HasPrefix(s, "burst:") {
		parts := strings.Split(s, ":")
		if len(parts) == 4 {
			source, e1 := strconv.ParseInt(parts[1], 10, 64)
			conv, e2 := strconv.ParseInt(parts[2], 10, 64)
			_, e3 := time.Parse(time.DateOnly, parts[3])
			if e1 == nil && e2 == nil && e3 == nil && source > 0 && conv > 0 {
				a.Kind = "burst"
				a.SourceID = source
				a.ConversationID = conv
				a.LocalDate = parts[3]
				return a, nil
			}
		}
	}
	if m := fullRefPattern.FindStringSubmatch(s); m != nil {
		var err error
		if m[1] == "evt" {
			var ref store.OccurrenceRef
			ref, err = store.ParseMeetingIdentityRef(s)
			a.ID = ref.MessageID
		} else {
			a.ID, err = strconv.ParseInt(m[2], 10, 64)
		}
		if err != nil || (m[3] != "" && m[1] != "evt") {
			return a, ErrUnknownReference
		}
		a.Kind = map[string]string{"msg": "message", "conv": "conversation", "evt": "meeting"}[m[1]]
		return a, nil
	}
	if at, exists, err := calrecur.CivilDateBoundary(s, loc); err == nil {
		if !exists {
			return a, fmt.Errorf("%w: civil date does not exist in %s", ErrInvalidRequest, loc)
		}
		a.Kind = "date"
		a.Time = at
		return a, nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02T15:04"} {
		if at, err := time.ParseInLocation(layout, s, loc); err == nil {
			a.Kind = "datetime"
			a.Time = at
			return a, nil
		}
	}
	return a, fmt.Errorf("%w: invalid anchor", ErrUnknownReference)
}
