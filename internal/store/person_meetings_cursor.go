package store

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"time"
)

type personMeetingsCursor struct {
	From, To, Start time.Time
	Ref, Query      string
}

func normalizePersonMeetingsQuery(scope SourceScope, q PersonMeetingsQuery, now time.Time) (PersonMeetingsQuery, personMeetingsCursor, string, error) {
	var cursor personMeetingsCursor
	invalid := func() (PersonMeetingsQuery, personMeetingsCursor, string, error) {
		return q, cursor, "", fmt.Errorf("%w: invalid person meeting query or cursor", ErrInvalidArgument)
	}
	if q.Cursor != "" {
		if len(q.Cursor) > 4096 {
			return invalid()
		}
		data, err := base64.RawURLEncoding.DecodeString(q.Cursor)
		if err != nil || json.Unmarshal(data, &cursor) != nil || cursor.From.IsZero() || cursor.To.IsZero() || cursor.Start.IsZero() || cursor.Ref == "" {
			return invalid()
		}
		if q.From.IsZero() {
			q.From = cursor.From
		}
		if q.To.IsZero() {
			q.To = cursor.To
		}
	}
	if q.To.IsZero() {
		q.To = now.UTC()
	}
	if q.From.IsZero() {
		q.From = q.To.Add(-365 * 24 * time.Hour)
	}
	q.From = q.From.UTC()
	q.To = q.To.UTC()
	if !q.To.After(q.From) || q.To.Sub(q.From) > 400*24*time.Hour {
		return invalid()
	}
	if q.Limit == 0 {
		q.Limit = 100
	}
	if q.Limit < 1 || q.Limit > 500 {
		return invalid()
	}
	q.ParticipantIDs = slices.Clone(q.ParticipantIDs)
	for _, id := range q.ParticipantIDs {
		if id <= 0 || id > 9007199254740991 {
			return invalid()
		}
	}
	slices.Sort(q.ParticipantIDs)
	q.ParticipantIDs = slices.Compact(q.ParticipantIDs)
	emails := []string{}
	for _, email := range q.Emails {
		if email = strings.ToLower(strings.TrimSpace(email)); email != "" {
			emails = append(emails, email)
		}
	}
	slices.Sort(emails)
	q.Emails = slices.Compact(emails)
	if len(q.ParticipantIDs) > 2000 || len(q.Emails) > 2000 {
		return q, cursor, "", ErrQueryTooBroad
	}
	scope.IDs = slices.Clone(scope.IDs)
	slices.Sort(scope.IDs)
	scope.IDs = slices.Compact(scope.IDs)
	encoded, err := json.Marshal(struct {
		Scope    SourceScope
		IDs      []int64
		Emails   []string
		From, To time.Time
	}{scope, q.ParticipantIDs, q.Emails, q.From, q.To})
	if err != nil {
		return q, cursor, "", err
	}
	key := fmt.Sprintf("%x", sha256.Sum256(encoded))
	if q.Cursor != "" && (cursor.Query != key || !cursor.From.Equal(q.From) || !cursor.To.Equal(q.To)) {
		return invalid()
	}
	return q, cursor, key, nil
}
