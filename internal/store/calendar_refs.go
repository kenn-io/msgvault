package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/calrecur"
)

// OccurrenceRef identifies a standalone event or an original series occurrence.
type OccurrenceRef struct {
	MessageID int64
	Key       string
}

func ParseMeetingRef(ref string) (kind string, occ OccurrenceRef, msgID int64, err error) {
	kind, value, ok := strings.Cut(ref, ":")
	if !ok || (kind != "evt" && kind != "msg") {
		return "", occ, 0, fmt.Errorf("invalid meeting ref %q", ref)
	}
	number, key, hasKey := strings.Cut(value, "@")
	for _, c := range strings.TrimPrefix(number, "-") {
		if c < '0' || c > '9' {
			return "", occ, 0, fmt.Errorf("invalid meeting ref %q", ref)
		}
	}
	id, parseErr := strconv.ParseInt(number, 10, 64)
	if parseErr != nil {
		return "", occ, 0, fmt.Errorf("invalid meeting ref %q", ref)
	}
	if hasKey {
		if kind != "evt" {
			return "", occ, 0, fmt.Errorf("invalid meeting ref %q", ref)
		}
		if _, parseErr := calrecur.ParseKey(key); parseErr != nil {
			return "", occ, 0, fmt.Errorf("invalid meeting ref %q: %w", ref, parseErr)
		}
	}
	if kind == "msg" {
		return kind, occ, id, nil
	}
	return kind, OccurrenceRef{MessageID: id, Key: key}, 0, nil
}

func FormatOccurrenceRef(r OccurrenceRef) string {
	ref := "evt:" + strconv.FormatInt(r.MessageID, 10)
	if r.Key != "" {
		ref += "@" + r.Key
	}
	return ref
}

// resolveOccurrenceRefAlias retains orphan refs after a visible master arrives.
func resolveOccurrenceRefAlias(ctx context.Context, r rowReader, scope SourceScope, ref OccurrenceRef) (OccurrenceRef, error) {
	rows, err := calendarEventRowsByMessageIDs(ctx, r, scope, []int64{ref.MessageID})
	if err != nil {
		return OccurrenceRef{}, err
	}
	if len(rows) == 0 {
		return OccurrenceRef{}, sql.ErrNoRows
	}
	row := rows[0]
	if row.Event.Kind != "exception" || ref.Key != "" || row.Event.Original == nil {
		return ref, nil
	}
	masters, err := calendarEventRowsByProviderID(ctx, r, scope, row.Event.Provider, row.Event.Namespace, row.Event.SeriesProviderID)
	if err != nil {
		return OccurrenceRef{}, err
	}
	for _, master := range masters {
		if master.SourceID == row.SourceID && master.Event.Kind == "series" {
			return OccurrenceRef{MessageID: master.MessageID, Key: row.Event.Original.Key()}, nil
		}
	}
	return ref, nil
}
