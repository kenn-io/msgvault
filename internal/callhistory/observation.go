// Package callhistory preserves call claims without inferring attendance.
package callhistory

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"math"
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
	Version            int           `json:"version"`
	Provider           string        `json:"provider"`
	Account            string        `json:"account"`
	NativeCallID       string        `json:"native_call_id,omitempty"`
	Kind               string        `json:"kind"`
	Direction          string        `json:"direction"`
	Outcome            string        `json:"outcome"`
	OwnerParticipation string        `json:"owner_participation"`
	OccurredAt         time.Time     `json:"occurred_at"`
	EndedAt            *time.Time    `json:"ended_at,omitempty"`
	DurationSeconds    *float64      `json:"duration_seconds"`
	Timezone           string        `json:"timezone,omitempty"`
	Precision          string        `json:"precision"`
	Withdrawn          bool          `json:"withdrawn,omitempty"`
	AccessPoints       []AccessPoint `json:"access_points,omitempty"`
}

type AccessPoint struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
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

// CanonicalKey requires an explicit provider/account/native-call tuple.
// Empty means callers must use this observation's own archive identity.
func (e Evidence) CanonicalKey() string {
	if e.NativeCallID == "" {
		return ""
	}
	var data []byte
	for _, value := range []string{e.Provider, e.Account, e.NativeCallID} {
		data = binary.BigEndian.AppendUint64(data, uint64(len(value)))
		data = append(data, value...)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func invalid(field string) error {
	return fmt.Errorf("%w: %s", ErrInvalid, field)
}

func validID(s string) bool {
	return strings.TrimSpace(s) != "" && len(s) <= 256 && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

func (e Evidence) Validate() error {
	if e.Version != 1 || !validID(e.Provider) || !validID(e.Account) ||
		(e.NativeCallID != "" && !validID(e.NativeCallID)) {
		return invalid("version or provenance")
	}
	if !slices.Contains([]string{"call", "scheduled_meeting"}, e.Kind) ||
		!slices.Contains([]string{"incoming", "outgoing", unknown}, e.Direction) ||
		!slices.Contains([]string{"answered", "missed", "declined", "cancelled", unknown}, e.Outcome) ||
		!slices.Contains([]string{"joined", "not_joined", unknown}, e.OwnerParticipation) ||
		!slices.Contains([]string{"instant", "day"}, e.Precision) {
		return invalid("evidence classification")
	}
	if e.OccurredAt.IsZero() || e.OccurredAt.Year() < 1 || e.OccurredAt.Year() > 9999 ||
		(e.EndedAt != nil && (e.EndedAt.Before(e.OccurredAt) || e.EndedAt.Year() > 9999)) {
		return invalid("timestamps")
	}
	if e.DurationSeconds != nil && (*e.DurationSeconds < 0 ||
		math.IsInf(*e.DurationSeconds, 0) || math.IsNaN(*e.DurationSeconds)) {
		return invalid("duration_seconds")
	}
	if e.Timezone != "" {
		if len(e.Timezone) > 256 {
			return invalid("timezone")
		}
		if _, err := time.LoadLocation(e.Timezone); err != nil {
			return invalid("timezone")
		}
	}
	if e.Kind == "scheduled_meeting" && (e.OwnerParticipation == "joined" ||
		(e.Outcome != unknown && e.Outcome != "cancelled") || e.DurationSeconds != nil || e.NativeCallID != "") {
		return invalid("scheduled evidence cannot assert an actual call")
	}
	if len(e.AccessPoints) > 32 {
		return invalid("access_points")
	}
	for _, p := range e.AccessPoints {
		if !slices.Contains([]string{"phone", "url"}, p.Kind) ||
			strings.TrimSpace(p.Value) == "" || len(p.Value) > 2048 || !utf8.ValidString(p.Value) {
			return invalid("access_points")
		}
	}
	return nil
}

func (o Observation) Validate() error {
	if err := o.Evidence.Validate(); err != nil {
		return err
	}
	if !validID(o.RecordID) || len(o.Participants) > 100 ||
		len(o.Raw) == 0 || len(o.Raw) > MaxRawBytes || !o.Raw.IsValid() {
		return invalid("record_id, participants or raw")
	}
	seen := make(map[int64]string, len(o.Participants))
	for _, p := range o.Participants {
		if p.ID <= 0 || !slices.Contains([]string{"joined", "invited", unknown}, p.Participation) {
			return invalid("participant")
		}
		if old, ok := seen[p.ID]; ok && old != p.Participation {
			return invalid("conflicting participant roles")
		}
		if o.Kind == "scheduled_meeting" && p.Participation == "joined" {
			return invalid("scheduled attendance")
		}
		seen[p.ID] = p.Participation
	}
	return nil
}
