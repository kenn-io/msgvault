// Package callhistory preserves call claims without inferring attendance.
package callhistory

import (
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	unknown         = "unknown"
	MessageType     = "call_record"
	SourceType      = "call_history"
	MaxRequestBytes = 1 << 20
	MaxRawBytes     = 512 << 10
)

var ErrInvalid = errors.New("invalid call observation")

// Evidence contains scalar source claims. Archive participant IDs live only
// in recipient rows, so identity merges cannot leave stale metadata bindings.
type Evidence struct {
	Version            int        `json:"version"`
	Provider           string     `json:"provider"`
	Account            string     `json:"account"`
	NativeCallID       string     `json:"native_call_id,omitempty"`
	Kind               string     `json:"kind"`
	Direction          string     `json:"direction"`
	Outcome            string     `json:"outcome"`
	OwnerParticipation string     `json:"owner_participation"`
	OccurredAt         time.Time  `json:"occurred_at"`
	EndedAt            *time.Time `json:"ended_at,omitempty"`
	DurationSeconds    *float64   `json:"duration_seconds"`
}

type Participant struct {
	ID            int64  `json:"participant_id"`
	Participation string `json:"participation"`
}

type Observation struct {
	Evidence

	RecordID     string         `json:"record_id"`
	Participants []Participant  `json:"participants"`
	Raw          jsontext.Value `json:"raw"`
}

// SourceIdentifier keeps provider/account namespaces separate without
// ambiguous concatenation. It is archive metadata, not a display label.
func (e Evidence) SourceIdentifier() string {
	return fmt.Sprintf("calls/%d:%s%s", len(e.Provider), e.Provider, e.Account)
}

func invalid(field string) error {
	return fmt.Errorf("%w: %s", ErrInvalid, field)
}

func validID(s string) bool {
	return strings.TrimSpace(s) != "" && len(s) <= 1024 && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

func (e Evidence) Validate() error {
	if e.Version != 1 || !validID(e.Provider) || !validID(e.Account) ||
		(e.NativeCallID != "" && !validID(e.NativeCallID)) {
		return invalid("version or provenance")
	}
	if e.Kind != "call" ||
		!slices.Contains([]string{"incoming", "outgoing", unknown}, e.Direction) ||
		!slices.Contains([]string{"answered", "missed", "declined", "cancelled", unknown}, e.Outcome) ||
		!slices.Contains([]string{"joined", "not_joined", unknown}, e.OwnerParticipation) {
		return invalid("evidence classification")
	}
	if e.OccurredAt.IsZero() || e.OccurredAt.Year() < 1 || e.OccurredAt.Year() > 9999 ||
		(e.EndedAt != nil && (e.EndedAt.Before(e.OccurredAt) || e.EndedAt.Year() > 9999)) {
		return invalid("timestamps")
	}
	if e.DurationSeconds != nil && *e.DurationSeconds < 0 {
		return invalid("duration_seconds")
	}
	return nil
}

func (o Observation) Validate() error {
	if err := o.Evidence.Validate(); err != nil {
		return err
	}
	if o.NativeCallID != "" && o.RecordID != o.NativeCallID {
		return invalid("record_id must equal native_call_id")
	}
	if !validID(o.RecordID) || len(o.Raw) == 0 || len(o.Raw) > MaxRawBytes {
		return invalid("record_id or raw")
	}
	seen := make(map[int64]string, len(o.Participants))
	for _, p := range o.Participants {
		if p.ID <= 0 || !slices.Contains([]string{"joined", "invited", unknown}, p.Participation) {
			return invalid("participant")
		}
		if old, ok := seen[p.ID]; ok && old != p.Participation {
			return invalid("conflicting participant roles")
		}
		seen[p.ID] = p.Participation
	}
	return nil
}
