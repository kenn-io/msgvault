package peoplesweep

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
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

type CommitmentWindow struct {
	ScopeFingerprint  string             `json:"-"`
	SelectionAt       time.Time          `json:"-"`
	PersonID          int64              `json:"-"`
	Sources           []CommitmentSource `json:"sources"`
	Now               time.Time          `json:"now"`
	Timezone          string             `json:"timezone"`
	BackfillMessageID int64              `json:"-"`
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

type ParsedCommitments struct {
	Promises []CommitmentPromise `json:"promises"`
}

// ParseCommitments admits the entire bounded output or none of it. Provider
// direction, locators, dates, and proposed state changes are not trusted.
func ParseCommitments(output jsontext.Value, window CommitmentWindow) (ParsedCommitments, error) {
	invalid := func() (ParsedCommitments, error) {
		return ParsedCommitments{}, errors.New("commitment output does not match scoped evidence")
	}
	if window.PersonID <= 0 || window.Now.IsZero() {
		return invalid()
	}
	location, err := time.LoadLocation(window.Timezone)
	if err != nil || validateProgramOutput(output, commitmentSchema, "commitment") != nil {
		return invalid()
	}
	var parsed ParsedCommitments
	if err := decodeSingleJSON(output, &parsed); err != nil {
		return invalid()
	}
	sources := make(map[string]CommitmentSource, len(window.Sources))
	for _, source := range window.Sources {
		if _, exists := sources[source.ID]; exists || source.ID == "" || source.MessageID <= 0 || (source.Direction != CommitmentOwedBy && source.Direction != CommitmentOwedTo) {
			return invalid()
		}
		sources[source.ID] = source
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
			if !commitmentQuoteHasDate(promise.Quote, due, source.EventTime, location) {
				promise.DueOn = nil
			}
		}
		// Human-readable wording is deliberately absent from identity.
		identity := fmt.Sprintf("%d/%s/%d/%s/%d/%d/%s", window.PersonID, promise.Direction, source.MessageID, source.RecordedTime.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano), promise.Start, promise.End, promise.Quote)
		digest := sha256.Sum256([]byte(identity))
		promise.TargetKey = "commitment:" + hex.EncodeToString(digest[:])
		for _, span := range spans[source.MessageID] {
			if promise.Start < span.end && span.start < promise.End {
				return invalid()
			}
		}
		spans[source.MessageID] = append(spans[source.MessageID], promiseSpan{promise.Start, promise.End})
	}
	return parsed, nil
}

var (
	commitmentDateLayouts     = []string{time.DateOnly, "January 2, 2006", "Jan 2, 2006", "2 January 2006", "2 Jan 2006"}
	commitmentYearlessLayouts = []string{"January 2", "Jan 2", "2 January", "2 Jan"}
	commitmentYearFollows     = regexp.MustCompile(`^[,\s]*\d{4}\b`)
)

// commitmentQuoteHasDate reports whether the quote itself states due, either
// literally or relative to the source event day in the configured timezone.
func commitmentQuoteHasDate(quote string, due, event time.Time, location *time.Location) bool {
	quote = strings.ToLower(quote)
	for _, layout := range commitmentDateLayouts {
		if quoteHasPhrase(quote, strings.ToLower(due.Format(layout)), nil) {
			return true
		}
	}
	if event.IsZero() {
		return false
	}
	day := event.In(location)
	return quoteHasTomorrow(quote, due, day) || quoteHasYearlessDate(quote, due, day) || quoteHasByWeekday(quote, due, day)
}

// quoteHasPhrase finds phrase as a whole token; allow, when set, vets the text after each match.
func quoteHasPhrase(quote, phrase string, allow func(rest string) bool) bool {
	isWord := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }
	for from := 0; ; {
		i := strings.Index(quote[from:], phrase)
		if i < 0 {
			return false
		}
		start, end := from+i, from+i+len(phrase)
		before, _ := utf8.DecodeLastRuneInString(quote[:start])
		after, _ := utf8.DecodeRuneInString(quote[end:])
		if !isWord(before) && !isWord(after) && (allow == nil || allow(quote[end:])) {
			return true
		}
		from = end
	}
}

func quoteHasTomorrow(quote string, due, day time.Time) bool {
	return day.AddDate(0, 0, 1).Format(time.DateOnly) == due.Format(time.DateOnly) && quoteHasPhrase(quote, "tomorrow", nil)
}

// quoteHasYearlessDate accepts a month and day only for its next occurrence
// within a year of the event day, and never when the quote names a year.
func quoteHasYearlessDate(quote string, due, day time.Time) bool {
	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, due.Location())
	if due.Before(start) || !due.Before(start.AddDate(1, 0, 0)) {
		return false
	}
	for _, layout := range commitmentYearlessLayouts {
		if quoteHasPhrase(quote, strings.ToLower(due.Format(layout)), func(rest string) bool { return !commitmentYearFollows.MatchString(rest) }) {
			return true
		}
	}
	return false
}

func quoteHasByWeekday(quote string, due, day time.Time) bool {
	days := (int(due.Weekday()) - int(day.Weekday()) + 7) % 7
	phrase := "by " + strings.ToLower(due.Weekday().String())
	end := strings.TrimRightFunc(quote, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsPunct(r) })
	if day.AddDate(0, 0, days).Format(time.DateOnly) == due.Format(time.DateOnly) && strings.HasSuffix(end, phrase) {
		before, _ := utf8.DecodeLastRuneInString(end[:len(end)-len(phrase)])
		return !unicode.IsLetter(before) && !unicode.IsDigit(before)
	}
	return false
}

// MatchesQuote verifies an exact authored quote at its original-body rune offsets.
func (source CommitmentSource) MatchesQuote(quote string, start, end int64) bool {
	runes := []rune(source.Text)
	a, b := start-source.Start, end-source.Start
	return strings.TrimSpace(quote) != "" && !strings.ContainsRune(quote, CommitmentQuoteMask) && utf8.RuneCountInString(quote) <= 2000 && a >= 0 && b > a && b <= int64(len(runes)) && string(runes[a:b]) == quote
}
