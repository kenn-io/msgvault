package peoplesweep

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
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
	SourceID        int64     `json:"-"`
	RecordedTime    time.Time `json:"-"`
	ID              string    `json:"id"`
	MessageID       int64     `json:"-"`
	SourceMessageID string    `json:"-"`
	Direction       string    `json:"direction"`
	Text            string    `json:"text"`
	Start           int64     `json:"start"`
	EventTime       time.Time `json:"event_time"`
	SourceRef       string    `json:"-"`
	SourceVersion   string    `json:"-"`
	ContentSHA256   string    `json:"-"`
}

type CommitmentWindow struct {
	ScopeFingerprint  string             `json:"-"`
	IdentityRevision  int64              `json:"-"`
	SelectionAt       time.Time          `json:"-"`
	PersonID          int64              `json:"-"`
	Sources           []CommitmentSource `json:"sources"`
	Now               time.Time          `json:"now"`
	Timezone          string             `json:"timezone"`
	BackfillMessageID int64              `json:"-"`
	SnapshotSequence  int64              `json:"-"`
	ThroughSequence   int64              `json:"-"`
	SkippedMessageIDs []int64            `json:"-"`
	RetriedMessageIDs []int64            `json:"-"`
	ResetSkipped      bool               `json:"-"`
}

type CommitmentPromise struct {
	SourceID  string  `json:"source_id"`
	Direction string  `json:"direction"`
	Quote     string  `json:"quote"`
	Start     int64   `json:"start"`
	End       int64   `json:"end"`
	Summary   string  `json:"summary"`
	DueOn     *string `json:"due_on"`
	DuePhrase *string `json:"due_phrase"`
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
	seen := make(map[string]bool)
	accepted := parsed.Promises[:0]
	for index := range parsed.Promises {
		promise := &parsed.Promises[index]
		source, ok := sources[promise.SourceID]
		if !ok || promise.Direction != source.Direction || !source.MatchesQuote(promise.Quote, promise.Start, promise.End) || strings.TrimSpace(promise.Summary) == "" {
			return invalid()
		}
		if promise.DueOn != nil {
			due, err := time.Parse(time.DateOnly, *promise.DueOn)
			if err != nil || due.Format(time.DateOnly) != *promise.DueOn {
				return invalid()
			}
			if promise.DuePhrase == nil || strings.TrimSpace(*promise.DuePhrase) == "" || !strings.Contains(promise.Quote, *promise.DuePhrase) || !commitmentQuoteHasDate(*promise.DuePhrase, due, source.EventTime, location) {
				promise.DueOn = nil
			}
		}
		// Human-readable wording is deliberately absent from identity.
		hash, start, end := commitmentSpanIdentity(source, promise.Quote, promise.Start, promise.End)
		promise.TargetKey = commitmentTargetKey(window.PersonID, source, promise.Quote, hash, start, end)
		if !seen[promise.TargetKey] {
			seen[promise.TargetKey] = true
			accepted = append(accepted, *promise)
		}
	}
	parsed.Promises = accepted
	return parsed, nil
}

// CommitmentQuoteRepeated counts overlapping occurrences in eligible text.
func CommitmentQuoteRepeated(text, quote string) bool {
	if quote == "" {
		return false
	}
	first := strings.Index(text, quote)
	return first >= 0 && strings.Contains(text[first+1:], quote)
}

// commitmentSpanIdentity keeps a quote's position in identity only when the
// quote repeats in its source, so a unique quote keeps its identity when it moves.
func commitmentSpanIdentity(source CommitmentSource, quote string, start, end int64) (string, int64, int64) {
	if !CommitmentQuoteRepeated(source.Text, quote) {
		return "", 0, 0
	}
	return source.ContentSHA256, start, end
}

// CommitmentSourceKey identifies a promise by its source message and quote,
// leaving out the person and the archive row, so the same promise keeps its
// key across person merges and row reuse while a changed quote, a replacement
// message or a separate arrival gets a new one.
func CommitmentSourceKey(source CommitmentSource, quote string, start, end int64) string {
	hash, start, end := commitmentSpanIdentity(source, quote, start, end)
	identity := fmt.Sprintf("%s/%d/%q/%s/%s/%d/%d/%q", source.Direction, source.SourceID, source.SourceMessageID, source.RecordedTime.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano), hash, start, end, quote)
	digest := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(digest[:])
}

func commitmentTargetKey(personID int64, source CommitmentSource, quote, hash string, start, end int64) string {
	identity := fmt.Sprintf("%d/%s/%d/%d/%q/%s/%s/%d/%d/%s", personID, source.Direction, source.MessageID, source.SourceID, source.SourceMessageID, source.RecordedTime.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano), hash, start, end, quote)
	digest := sha256.Sum256([]byte(identity))
	return "commitment:" + hex.EncodeToString(digest[:])
}

var (
	commitmentDateLayouts     = []string{time.DateOnly, "January 2, 2006", "Jan 2, 2006", "2 January 2006", "2 Jan 2006"}
	commitmentYearlessLayouts = []string{"January 2", "Jan 2", "2 January", "2 Jan"}
	commitmentYearFollows     = regexp.MustCompile(`^[,\s]*\d{4}\b`)
	commitmentNumericDate     = regexp.MustCompile(`\b\d{1,2}/\d{1,2}/(?:\d{4}|\d{2})\b`)
	commitmentYearlessNumeric = regexp.MustCompile(`\b\d{1,2}/\d{1,2}\b`)
	commitmentTomorrow        = regexp.MustCompile(`\b(day after )?tomorrow\b`)
	commitmentRelativeDays    = regexp.MustCompile(`\bin ([a-z0-9]+) (days?|weeks?|months?)\b`)
	commitmentWeekdayDeadline = regexp.MustCompile(`\b(?:(?:by|on)\s+)?(?:(this|next)\s+)?(monday|tuesday|wednesday|thursday|friday|saturday|sunday)(?:\s+(?:(?:of\s+)?(this|next)\s+week|in\s+([a-z0-9]+)\s+weeks?))?\b`)
	commitmentInTail          = regexp.MustCompile(`^\s+in\b`)
)

// commitmentQuoteHasDate reports whether the quote itself states due, either
// literally or relative to the source event day in the configured timezone.
func commitmentQuoteHasDate(quote string, due, event time.Time, location *time.Location) bool {
	quote = normalizeCommitmentOrdinal(strings.ToLower(quote), due.Day())
	for _, layout := range commitmentDateLayouts {
		if quoteHasPhrase(quote, strings.ToLower(due.Format(layout)), nil) {
			return true
		}
	}
	for _, span := range commitmentNumericDate.FindAllStringIndex(quote, -1) {
		if (span[0] > 0 && quote[span[0]-1] == '/') || (span[1] < len(quote) && quote[span[1]] == '/') {
			continue
		}
		date := quote[span[0]:span[1]]
		year := "2006"
		if len(date)-strings.LastIndexByte(date, '/') == 3 {
			year = "06"
		}
		monthFirst, monthErr := time.Parse("1/2/"+year, date)
		dayFirst, dayErr := time.Parse("2/1/"+year, date)
		if (monthErr == nil && (dayErr != nil || monthFirst.Equal(dayFirst)) && monthFirst.Format(time.DateOnly) == due.Format(time.DateOnly)) ||
			(dayErr == nil && monthErr != nil && dayFirst.Format(time.DateOnly) == due.Format(time.DateOnly)) {
			return true
		}
	}
	if event.IsZero() {
		return false
	}
	day := event.In(location)
	endOfMonth := time.Date(day.Year(), day.Month()+1, 0, 0, 0, 0, 0, time.UTC)
	return day.Format(time.DateOnly) == due.Format(time.DateOnly) && quoteHasPhrase(quote, "today", nil) || due.Equal(endOfMonth) && quoteHasPhrase(quote, "end of this month", nil) || quoteHasTomorrow(quote, due, day) || quoteHasRelativeDays(quote, due, day) || quoteHasYearlessDate(quote, due, day) || quoteHasYearlessNumericDate(quote, due, day) || quoteHasWeekday(quote, due, day)
}

func quoteHasYearlessNumericDate(quote string, due, day time.Time) bool {
	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, due.Location())
	if due.Before(start) || !due.Before(start.AddDate(1, 0, 0)) {
		return false
	}
	for _, span := range commitmentYearlessNumeric.FindAllStringIndex(quote, -1) {
		if (span[0] > 0 && quote[span[0]-1] == '/') || (span[1] < len(quote) && quote[span[1]] == '/') || commitmentYearFollows.MatchString(quote[span[1]:]) {
			continue
		}
		date := quote[span[0]:span[1]] + "/" + strconv.Itoa(due.Year())
		monthFirst, monthErr := time.Parse("1/2/2006", date)
		dayFirst, dayErr := time.Parse("2/1/2006", date)
		if (monthErr == nil && (dayErr != nil || monthFirst.Equal(dayFirst)) && monthFirst.Equal(due)) ||
			(dayErr == nil && monthErr != nil && dayFirst.Equal(due)) {
			return true
		}
	}
	return false
}

func normalizeCommitmentOrdinal(quote string, day int) string {
	suffix := "th"
	if day%100 < 11 || day%100 > 13 {
		switch day % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	ordinal := strconv.Itoa(day) + suffix
	return regexp.MustCompile(`\b`+ordinal+`\b`).ReplaceAllString(quote, strconv.Itoa(day))
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
	for _, match := range commitmentTomorrow.FindAllStringSubmatch(quote, -1) {
		days := 1
		if match[1] != "" {
			days = 2
		}
		if day.AddDate(0, 0, days).Format(time.DateOnly) == due.Format(time.DateOnly) {
			return true
		}
	}
	return false
}

func quoteHasRelativeDays(quote string, due, day time.Time) bool {
	for _, match := range commitmentRelativeDays.FindAllStringSubmatch(quote, -1) {
		count, err := strconv.Atoi(match[1])
		if err != nil {
			count = slices.Index([]string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten", "eleven", "twelve"}, match[1])
		}
		unit := strings.TrimSuffix(match[2], "s")
		if count > 0 && count <= commitmentRelativeLimits[unit] && commitmentRelativeDue(day, count, unit).Format(time.DateOnly) == due.Format(time.DateOnly) {
			return true
		}
	}
	return false
}

// commitmentRelativeLimits caps each unit at about a century of event days.
var commitmentRelativeLimits = map[string]int{"day": 36600, "week": 36600 / 7, "month": 1200}

func commitmentRelativeDue(day time.Time, count int, unit string) time.Time {
	switch unit {
	case "week":
		return day.AddDate(0, 0, count*7)
	case "month":
		return day.AddDate(0, count, 0)
	}
	return day.AddDate(0, 0, count)
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

func quoteHasWeekday(quote string, due, day time.Time) bool {
	for _, span := range commitmentWeekdayDeadline.FindAllStringIndex(quote, -1) {
		if commitmentInTail.MatchString(quote[span[1]:]) {
			continue
		}
		match := commitmentWeekdayDeadline.FindStringSubmatch(quote[span[0]:span[1]])
		if match[1] != "" && (match[3] != "" || match[4] != "") {
			continue
		}
		var weekday time.Weekday
		for candidate := time.Sunday; candidate <= time.Saturday; candidate++ {
			if match[2] == strings.ToLower(candidate.String()) {
				weekday = candidate
				break
			}
		}
		days := (int(weekday) - int(day.Weekday()) + 7) % 7
		period := match[3]
		if match[1] != "" {
			period = match[1]
		}
		switch period {
		case "this":
			days = (int(weekday)+6)%7 - (int(day.Weekday())+6)%7
		case "next":
			days = 7 - (int(day.Weekday())+6)%7 + (int(weekday)+6)%7
		}
		if match[4] != "" {
			weeks, err := strconv.Atoi(match[4])
			if err != nil {
				weeks = slices.Index([]string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten", "eleven", "twelve"}, match[4])
			}
			if weeks < 0 || weeks > 520000 {
				continue
			}
			days += weeks * 7
		}
		if day.AddDate(0, 0, days).Format(time.DateOnly) == due.Format(time.DateOnly) {
			return true
		}
	}
	return false
}

// MatchesQuote verifies an exact authored quote at its original-body rune offsets.
func (source CommitmentSource) MatchesQuote(quote string, start, end int64) bool {
	runes := []rune(source.Text)
	a, b := start-source.Start, end-source.Start
	return strings.TrimSpace(quote) != "" && !strings.ContainsRune(quote, CommitmentQuoteMask) && utf8.RuneCountInString(quote) <= 2000 && a >= 0 && b > a && b <= int64(len(runes)) && string(runes[a:b]) == quote
}
