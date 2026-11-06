// Package agreements validates reviewed agreement history. It does not infer
// assent from text: actor roles and acceptance are assertions reviewed by the owner.
package agreements

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/kataevidence"
)

const (
	MaxEvents       = 32
	MaxCitations    = 64
	MaxEncodedBytes = 256 << 10
)

var (
	ErrInvalid     = errors.New("invalid reviewed agreement event")
	ErrConflict    = errors.New("agreement changed or event ID was used with different input")
	ErrUnsupported = errors.New("unsupported or malformed agreement history")
	ErrFull        = errors.New("agreement history is full")
)

type Terms struct {
	Text        string `json:"text" maxLength:"1000"`
	Due         string `json:"due,omitzero"`
	Obligor     string `json:"obligor" maxLength:"128"`
	Beneficiary string `json:"beneficiary" maxLength:"128"`
}

// Input records the source speaker's role, not the identity of the API caller.
// Reviewed attests to reviewing the source speaker and explicit agreement.
type Input struct {
	ID             string                   `json:"id" maxLength:"128"`
	Action         string                   `json:"action" enum:"request,accept,propose,replace"`
	Actor          string                   `json:"actor" enum:"obligor,beneficiary"`
	Target         string                   `json:"target,omitzero" maxLength:"128"`
	ExpectedEvents int                      `json:"expected_events" minimum:"0" maximum:"31"`
	Reviewed       bool                     `json:"reviewed"`
	Terms          *Terms                   `json:"terms,omitzero"`
	Evidence       []kataevidence.Reference `json:"evidence" minItems:"1" maxItems:"4"`
}

// Event keeps the original canonical request and exact resolved quote snapshots.
type Event struct {
	Input  Input    `json:"input"`
	Quotes []string `json:"quotes"`
}

type Envelope struct {
	Version int     `json:"version"`
	Events  []Event `json:"events"`
}

type View struct {
	History         []Event  `json:"history"`
	Accepted        *Terms   `json:"accepted,omitzero"`
	AcceptedEventID string   `json:"accepted_event_id,omitzero"`
	Pending         []string `json:"pending"`
	Replayed        bool     `json:"replayed"`
}

func validID(value string) bool {
	return len(value) > 0 && len(value) <= 128 && strings.IndexFunc(value, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune("._:-", r)
	}) < 0
}

func text(value string, limit int) bool {
	return strings.TrimSpace(value) != "" && utf8.ValidString(value) && utf8.RuneCountInString(value) <= limit
}

// Normalize validates caller input and copies all nested values. Reference order
// and content versions participate in exact replay; neither is discarded.
func Normalize(input Input) (Input, error) {
	if !validID(input.ID) || !input.Reviewed || input.ExpectedEvents < 0 || input.ExpectedEvents >= MaxEvents ||
		(input.Actor != "obligor" && input.Actor != "beneficiary") || len(input.Evidence) < 1 || len(input.Evidence) > 4 {
		return Input{}, ErrInvalid
	}
	switch input.Action {
	case "request", "propose":
		if input.Terms == nil || !text(input.Terms.Text, 1000) || !text(input.Terms.Obligor, 128) || !text(input.Terms.Beneficiary, 128) || input.Terms.Obligor == input.Terms.Beneficiary {
			return Input{}, ErrInvalid
		}
		if input.Terms.Due != "" {
			date, err := time.Parse(time.DateOnly, input.Terms.Due)
			if err != nil || date.Format(time.DateOnly) != input.Terms.Due {
				return Input{}, ErrInvalid
			}
		}
		terms := *input.Terms
		input.Terms = &terms
	case "accept", "replace":
		if input.Terms != nil {
			return Input{}, ErrInvalid
		}
	default:
		return Input{}, ErrInvalid
	}
	if input.Action == "request" && input.Target != "" || input.Action != "request" && !validID(input.Target) {
		return Input{}, ErrInvalid
	}
	input.Evidence = slices.Clone(input.Evidence)
	seen := map[string]bool{}
	for i, ref := range input.Evidence {
		canonical, err := kataevidence.Canonicalize(ref)
		if err != nil || seen[kataevidence.ID(canonical)] {
			return Input{}, ErrInvalid
		}
		input.Evidence[i] = canonical
		seen[kataevidence.ID(canonical)] = true
	}
	return input, nil
}

// Decode refuses unknown fields and invalid history rather than silently
// rewriting a format produced by a newer version. A missing value is empty.
func Decode(value any) (View, error) {
	if value == nil {
		return project(nil)
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > MaxEncodedBytes {
		return View{}, ErrUnsupported
	}
	var envelope Envelope
	if err := json.Unmarshal(encoded, &envelope, json.RejectUnknownMembers(true)); err != nil || envelope.Version != 1 || len(envelope.Events) < 1 || len(envelope.Events) > MaxEvents {
		return View{}, ErrUnsupported
	}
	count := 0
	for i, event := range envelope.Events {
		input, err := Normalize(event.Input)
		if err != nil || !reflect.DeepEqual(input, event.Input) || input.ExpectedEvents != i || len(event.Quotes) != len(input.Evidence) {
			return View{}, ErrUnsupported
		}
		for j, quote := range event.Quotes {
			start, end := input.Evidence[j].Range()
			if !utf8.ValidString(quote) || utf8.RuneCountInString(quote) != end-start {
				return View{}, ErrUnsupported
			}
		}
		count += len(input.Evidence)
	}
	if count > MaxCitations {
		return View{}, ErrUnsupported
	}
	view, err := project(envelope.Events)
	if err != nil {
		return View{}, fmt.Errorf("%w: %w", ErrUnsupported, err)
	}
	return view, nil
}

// Append validates a new event without changing earlier history. Callers persist
// the returned envelope with a revision guard; this function performs no I/O.
func Append(history []Event, input Input, quotes []string) (Envelope, View, error) {
	if input.ExpectedEvents != len(history) {
		return Envelope{}, View{}, ErrConflict
	}
	if len(history) >= MaxEvents {
		return Envelope{}, View{}, ErrFull
	}
	events := append(slices.Clone(history), Event{Input: input, Quotes: slices.Clone(quotes)})
	if _, err := project(events); err != nil {
		return Envelope{}, View{}, err
	}
	envelope := Envelope{Version: 1, Events: events}
	encoded, err := json.Marshal(envelope)
	if err != nil || len(encoded) > MaxEncodedBytes {
		return Envelope{}, View{}, ErrFull
	}
	citations := 0
	for _, event := range events {
		citations += len(event.Input.Evidence)
	}
	if citations > MaxCitations {
		return Envelope{}, View{}, ErrFull
	}
	view, err := Decode(envelope)
	return envelope, view, err
}

func project(events []Event) (View, error) {
	view := View{History: slices.Clone(events), Pending: []string{}}
	if view.History == nil {
		view.History = []Event{}
	}
	byID := map[string]Event{}
	for i, event := range events {
		input := event.Input
		if input.ID == "" || byID[input.ID].Input.ID != "" {
			return View{}, ErrInvalid
		}
		targetEvent, exists := byID[input.Target]
		target := targetEvent.Input
		switch input.Action {
		case "request":
			if i != 0 || input.Terms == nil {
				return View{}, ErrInvalid
			}
		case "accept":
			if !exists || target.Action != "request" || view.Accepted != nil || input.Actor == target.Actor || lacksSeparateEvidence(event, targetEvent) {
				return View{}, ErrInvalid
			}
			view.Accepted, view.AcceptedEventID = target.Terms, input.ID
		case "propose":
			if view.Accepted == nil || input.Target != view.AcceptedEventID || input.Terms == nil || input.Terms.Obligor != view.Accepted.Obligor || input.Terms.Beneficiary != view.Accepted.Beneficiary {
				return View{}, ErrInvalid
			}
			view.Pending = append(view.Pending, input.ID)
		case "replace":
			if !exists || target.Action != "propose" || target.Target != view.AcceptedEventID || input.Actor == target.Actor || lacksSeparateEvidence(event, targetEvent) {
				return View{}, ErrInvalid
			}
			view.Accepted, view.AcceptedEventID, view.Pending = target.Terms, input.ID, []string{}
		default:
			return View{}, ErrInvalid
		}
		byID[input.ID] = event
	}
	if view.Accepted != nil {
		terms := *view.Accepted
		view.Accepted = &terms
	}
	return view, nil
}

// Acceptance needs a separate passage, while allowing previously cited context.
// Source version changes alone do not create a new passage. Full references
// still govern Link membership and exact replay at the persistence boundary.
func lacksSeparateEvidence(current, target Event) bool {
	if len(current.Input.Evidence) == 0 || len(current.Quotes) != len(current.Input.Evidence) || len(target.Quotes) != len(target.Input.Evidence) {
		return true
	}
	passages := make(map[string]bool, len(target.Input.Evidence))
	for i, ref := range target.Input.Evidence {
		passages[kataevidence.PassageID(ref, target.Quotes[i])] = true
	}
	for i, ref := range current.Input.Evidence {
		if !passages[kataevidence.PassageID(ref, current.Quotes[i])] {
			return false
		}
	}
	return true
}
