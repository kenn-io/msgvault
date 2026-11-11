package peoplesweep

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	CommitmentOwedBy = "owed_by"
	CommitmentOwedTo = "owed_to"

	// CommitmentQuoteMask marks original rune positions omitted from the source.
	CommitmentQuoteMask = '\x00'
)

// CommitmentSource is a scoped, participant-verified source. Start is an
// original-body rune coordinate, never an offset in a generated excerpt.
type CommitmentSource struct {
	SourceID      int64     `json:"-"`
	RecordedTime  time.Time `json:"-"`
	ID            string    `json:"id"`
	MessageID     int64     `json:"-"`
	Direction     string    `json:"direction"`
	Text          string    `json:"text"`
	Start         int64     `json:"start"`
	EventTime     time.Time `json:"event_time"`
	SourceRef     string    `json:"-"`
	SourceVersion string    `json:"-"`
	ContentSHA256 string    `json:"-"`
}

type CommitmentAnchor struct {
	ID         int64     `json:"id"`
	TargetKey  string    `json:"-"`
	Direction  string    `json:"direction"`
	Quote      string    `json:"quote"`
	PromisedAt time.Time `json:"promised_at"`
	// ReopenedAt is a floor that prevents old automatic fulfilment closing a
	// promise the owner explicitly reopened.
	ReopenedAt *time.Time `json:"reopened_at,omitempty"`
}

type CommitmentWindow struct {
	ScopeFingerprint  string             `json:"-"`
	SelectionAt       time.Time          `json:"-"`
	PersonID          int64              `json:"-"`
	Sources           []CommitmentSource `json:"sources"`
	Anchors           []CommitmentAnchor `json:"open_anchors"`
	Now               time.Time          `json:"now"`
	Timezone          string             `json:"timezone"`
	BackfillMessageID int64              `json:"-"`
	AnchorAfterID     int64              `json:"-"`
	AnchorBackfillID  int64              `json:"-"`
	SnapshotSequence  int64              `json:"-"`
	ThroughSequence   int64              `json:"-"`
}

type CommitmentPromise struct {
	SourceID  string  `json:"source_id"`
	Direction string  `json:"direction"`
	Quote     string  `json:"quote"`
	Start     int64   `json:"start"`
	End       int64   `json:"end"`
	Summary   string  `json:"summary"`
	DueOn     *string `json:"due_on"`
	TargetKey string  `json:"-"`
}

type CommitmentFulfilment struct {
	CommitmentID int64  `json:"commitment_id"`
	SourceID     string `json:"source_id"`
	Quote        string `json:"quote"`
	Start        int64  `json:"start"`
	End          int64  `json:"end"`
	Confirmed    bool   `json:"confirmed"`
}

type ParsedCommitments struct {
	Promises    []CommitmentPromise    `json:"promises"`
	Fulfilments []CommitmentFulfilment `json:"fulfilments"`
}

// ParseCommitments admits the entire bounded output or none of it. Provider
// direction, locators, dates, and proposed state changes are not trusted.
func ParseCommitments(output jsontext.Value, window CommitmentWindow) (ParsedCommitments, error) {
	invalid := func() (ParsedCommitments, error) {
		return ParsedCommitments{}, errors.New("commitment output does not match scoped evidence")
	}
	if len(output) > 32768 || window.PersonID <= 0 || window.Now.IsZero() {
		return invalid()
	}
	location, err := time.LoadLocation(window.Timezone)
	if err != nil {
		return invalid()
	}
	if err := validateCommitmentOutput(output); err != nil {
		return invalid()
	}
	var parsed ParsedCommitments
	decoder := jsontext.NewDecoder(bytes.NewReader(output), json.RejectUnknownMembers(true))
	if err := json.UnmarshalDecode(decoder, &parsed); err != nil {
		return invalid()
	}
	if err := json.UnmarshalDecode(decoder, &struct{}{}); !errors.Is(err, io.EOF) {
		return invalid()
	}
	sources := make(map[string]CommitmentSource, len(window.Sources))
	for _, source := range window.Sources {
		if source.ID == "" || source.MessageID <= 0 || (source.Direction != CommitmentOwedBy && source.Direction != CommitmentOwedTo) {
			return invalid()
		}
		if _, exists := sources[source.ID]; exists {
			return invalid()
		}
		sources[source.ID] = source
	}
	anchors := make(map[int64]CommitmentAnchor, len(window.Anchors))
	for _, anchor := range window.Anchors {
		anchors[anchor.ID] = anchor
	}
	type promiseSpan struct{ start, end int64 }
	spans := make(map[int64][]promiseSpan)
	for index := range parsed.Promises {
		promise := &parsed.Promises[index]
		source, ok := sources[promise.SourceID]
		if !ok || promise.Direction != source.Direction || !source.MatchesQuote(promise.Quote, promise.Start, promise.End) || strings.TrimSpace(promise.Summary) == "" {
			return invalid()
		}
		if promise.DueOn != nil {
			due, err := time.ParseInLocation(time.DateOnly, *promise.DueOn, location)
			if err != nil || due.Format(time.DateOnly) != *promise.DueOn {
				return invalid()
			}
		}
		// Human-readable wording is deliberately absent from identity.
		identity := fmt.Sprintf("%d/%s/%d/%d/%d/%s", window.PersonID, promise.Direction, source.MessageID, promise.Start, promise.End, promise.Quote)
		digest := sha256.Sum256([]byte(identity))
		promise.TargetKey = "commitment:" + hex.EncodeToString(digest[:])
		for _, span := range spans[source.MessageID] {
			if promise.Start < span.end && span.start < promise.End {
				return invalid()
			}
		}
		spans[source.MessageID] = append(spans[source.MessageID], promiseSpan{promise.Start, promise.End})
	}
	fulfilments := make(map[int64]bool)
	for _, fulfilment := range parsed.Fulfilments {
		source, ok := sources[fulfilment.SourceID]
		anchor, known := anchors[fulfilment.CommitmentID]
		if !ok || !known || fulfilments[anchor.ID] || source.Direction != anchor.Direction || !source.EventTime.After(anchor.PromisedAt) || !source.MatchesQuote(fulfilment.Quote, fulfilment.Start, fulfilment.End) {
			return invalid()
		}
		if anchor.ReopenedAt != nil && !source.EventTime.After(*anchor.ReopenedAt) {
			return invalid()
		}
		fulfilments[anchor.ID] = true
	}
	return parsed, nil
}

// MatchesQuote verifies an exact authored quote at its original-body rune offsets.
func (source CommitmentSource) MatchesQuote(quote string, start, end int64) bool {
	runes := []rune(source.Text)
	a, b := start-source.Start, end-source.Start
	return strings.TrimSpace(quote) != "" && !strings.ContainsRune(quote, CommitmentQuoteMask) && utf8.RuneCountInString(quote) <= 2000 && a >= 0 && b > a && b <= int64(len(runes)) && string(runes[a:b]) == quote
}
