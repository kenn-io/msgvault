// Package mime provides MIME message parsing using enmime.
package mime

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"regexp"
	"strings"
	"time"

	"github.com/jhillyerd/enmime"
)

// Message represents a parsed email message.
type Message struct {
	Subject string
	Date    time.Time
	// RawDateHeader holds the unparsed Date header even when Date could not
	// be resolved, so callers can distinguish a missing header from one that
	// failed to parse.
	RawDateHeader string
	ReceivedDates []time.Time
	From          []Address
	To            []Address
	Cc            []Address
	Bcc           []Address
	ReplyTo       []Address
	MessageID     string
	InReplyTo     string
	References    []string
	BodyText      string
	BodyHTML      string
	Attachments   []Attachment
	Errors        []string // Non-fatal parsing errors
}

// Address represents an email address with optional display name.
type Address struct {
	Name   string
	Email  string
	Domain string // Extracted from email for aggregation
}

// Attachment represents a file attachment or inline part.
type Attachment struct {
	Filename    string
	ContentType string
	ContentID   string
	Size        int
	ContentHash string // SHA-256 of content
	Content     []byte
	IsInline    bool
}

// Parse parses raw MIME data into a Message.
func Parse(raw []byte) (*Message, error) {
	env, err := enmime.ReadEnvelope(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("read MIME envelope: %w", err)
	}

	msg := &Message{
		Subject:   env.GetHeader("Subject"),
		MessageID: env.GetHeader("Message-ID"),
		InReplyTo: env.GetHeader("In-Reply-To"),
		BodyText:  env.Text,
		BodyHTML:  env.HTML,
	}

	// Parse date
	if dateStr := env.GetHeader("Date"); dateStr != "" {
		msg.RawDateHeader = dateStr
		if t := parseDate(dateStr); !t.IsZero() {
			msg.Date = t
		}
	}
	msg.ReceivedDates = ParseReceivedChain(env.GetHeaderValues("Received"))

	// Parse addresses using enmime's AddressList (handles edge cases better)
	msg.From = parseAddressList(env, "From")
	msg.To = parseAddressList(env, "To")
	msg.Cc = parseAddressList(env, "Cc")
	msg.Bcc = parseAddressList(env, "Bcc")
	msg.ReplyTo = parseAddressList(env, "Reply-To")

	// Parse References header
	if refs := env.GetHeader("References"); refs != "" {
		msg.References = parseReferences(refs)
	}

	// Process attachments (both explicit attachments and inlines)
	// Filter out text/plain and text/html parts that are actually body content,
	// matching Python's behavior: only include parts with a filename OR
	// explicit Content-Disposition: attachment
	msg.Attachments = append(msg.Attachments, processParts(env.Attachments, false)...)
	msg.Attachments = append(msg.Attachments, processParts(env.Inlines, true)...)

	// Collect any parsing errors
	for _, e := range env.Errors {
		msg.Errors = append(msg.Errors, e.Error())
	}

	return msg, nil
}

// parseAddressList parses an address header using enmime's AddressList method.
func parseAddressList(env *enmime.Envelope, header string) []Address {
	list, err := env.AddressList(header)
	if err != nil || list == nil {
		return fallbackAddressList(env.GetHeader(header))
	}

	addresses := make([]Address, 0, len(list))
	for _, addr := range list {
		if addr.Address == "" {
			continue
		}
		addresses = append(addresses, Address{
			Name:   addr.Name,
			Email:  strings.ToLower(addr.Address),
			Domain: extractDomain(addr.Address),
		})
	}
	return addresses
}

// fallbackAddressList keeps address tokens when one malformed token causes
// enmime to reject an otherwise useful address list. A malformed recipient
// must not hide the valid recipients that share its header.
func fallbackAddressList(value string) []Address {
	var addresses []Address
	for _, token := range splitAddressTokens(value) {
		for _, email := range emailAddressTokenRe.FindAllString(stripAddressDecorations(token), -1) {
			email = strings.ToLower(email)
			addresses = append(addresses, Address{
				Email:  email,
				Domain: extractDomain(email),
			})
		}
	}
	return addresses
}

// splitAddressTokens splits an address header at commas that are outside
// quoted strings, comments, and angle-bracket addresses.
func splitAddressTokens(value string) []string {
	var tokens []string
	start := 0
	quoted := false
	escaped := false
	commentDepth := 0
	angleDepth := 0

	for i, r := range value {
		if escaped {
			escaped = false
			continue
		}
		if quoted {
			switch r {
			case '\\':
				escaped = true
			case '"':
				quoted = false
			}
			continue
		}
		if commentDepth > 0 {
			switch r {
			case '\\':
				escaped = true
			case '(':
				commentDepth++
			case ')':
				commentDepth--
			}
			continue
		}

		switch r {
		case '"':
			quoted = true
		case '(':
			commentDepth = 1
		case '<':
			angleDepth++
		case '>':
			if angleDepth > 0 {
				angleDepth--
			}
		case ',':
			if angleDepth == 0 {
				tokens = append(tokens, value[start:i])
				start = i + 1
			}
		}
	}

	return append(tokens, value[start:])
}

// stripAddressDecorations removes quoted display names and comments before
// salvage scans a single address token. Email-like text in either region is
// descriptive text, not a recipient address.
func stripAddressDecorations(value string) string {
	var stripped strings.Builder
	stripped.Grow(len(value))
	quoted := false
	escaped := false
	commentDepth := 0

	for _, r := range value {
		if escaped {
			escaped = false
			continue
		}
		if quoted {
			switch r {
			case '\\':
				escaped = true
			case '"':
				quoted = false
			}
			continue
		}
		if commentDepth > 0 {
			switch r {
			case '\\':
				escaped = true
			case '(':
				commentDepth++
			case ')':
				commentDepth--
			}
			continue
		}

		switch r {
		case '"':
			quoted = true
			stripped.WriteByte(' ')
		case '(':
			commentDepth = 1
			stripped.WriteByte(' ')
		default:
			stripped.WriteRune(r)
		}
	}

	return stripped.String()
}

// extractDomain extracts the domain from an email address.
func extractDomain(email string) string {
	if idx := strings.LastIndex(email, "@"); idx >= 0 {
		return strings.ToLower(email[idx+1:])
	}
	return ""
}

// isBodyPart returns true if the part should be treated as body content
// rather than an attachment. This matches Python's behavior: text/plain and
// text/html parts without a filename and without explicit Content-Disposition:
// attachment are body parts, not attachments.
func isBodyPart(part *enmime.Part) bool {
	// Extract base media type (strip parameters like charset)
	// e.g., "text/plain; charset=utf-8" → "text/plain"
	contentType := strings.ToLower(part.ContentType)
	if idx := strings.Index(contentType, ";"); idx >= 0 {
		contentType = strings.TrimSpace(contentType[:idx])
	}
	if contentType != "text/plain" && contentType != "text/html" {
		return false
	}
	// Has filename → treat as attachment
	if part.FileName != "" {
		return false
	}
	// Explicit Content-Disposition: attachment → treat as attachment
	// Handle parameters like "attachment; filename=x"
	disposition := strings.ToLower(part.Disposition)
	if idx := strings.Index(disposition, ";"); idx >= 0 {
		disposition = strings.TrimSpace(disposition[:idx])
	}
	if disposition == "attachment" {
		return false
	}
	// Text/plain or text/html without filename and not explicitly attachment → body part
	return true
}

// processParts filters body parts and converts the remaining parts to Attachments.
func processParts(parts []*enmime.Part, isInline bool) []Attachment {
	var result []Attachment
	for _, part := range parts {
		if !isBodyPart(part) {
			result = append(result, makeAttachment(part, isInline))
		}
	}
	return result
}

// makeAttachment creates an Attachment from an enmime Part.
func makeAttachment(part *enmime.Part, isInline bool) Attachment {
	content := part.Content
	hash := sha256.Sum256(content)

	return Attachment{
		Filename:    part.FileName,
		ContentType: part.ContentType,
		ContentID:   part.ContentID,
		Size:        len(content),
		ContentHash: hex.EncodeToString(hash[:]),
		Content:     content,
		IsInline:    isInline,
	}
}

// parseReferences parses the References header into individual message IDs.
func parseReferences(refs string) []string {
	var result []string
	for ref := range strings.FieldsSeq(refs) {
		ref = strings.Trim(ref, "<>")
		if ref != "" {
			result = append(result, ref)
		}
	}
	return result
}

// dateFormats lists common email date formats for parseDate.
var dateFormats = []string{
	time.RFC1123Z,                           // "Mon, 02 Jan 2006 15:04:05 -0700"
	time.RFC1123,                            // "Mon, 02 Jan 2006 15:04:05 MST"
	"Mon, 2 Jan 2006 15:04:05 -0700",        // Single-digit day
	"Mon, 2 Jan 2006 15:04:05 MST",          // Single-digit day with named TZ
	"2 Jan 2006 15:04:05 -0700",             // No weekday
	"2 Jan 2006 15:04:05 MST",               // No weekday, named TZ
	"02 Jan 2006 15:04:05 -0700",            // No weekday, zero-padded
	"02 Jan 2006 15:04:05 MST",              // No weekday, zero-padded, named TZ
	time.RFC822Z,                            // "02 Jan 06 15:04 -0700"
	time.RFC822,                             // "02 Jan 06 15:04 MST"
	time.RFC850,                             // "Monday, 02-Jan-06 15:04:05 MST"
	time.ANSIC,                              // "Mon Jan _2 15:04:05 2006"
	time.UnixDate,                           // "Mon Jan _2 15:04:05 MST 2006"
	"Mon, 02 Jan 2006 15:04:05 -0700 (MST)", // With parenthesized TZ
	"Mon, 2 Jan 2006 15:04:05 -0700 (MST)",  // Single-digit day with paren TZ
	time.RFC3339,                            // "2006-01-02T15:04:05Z07:00" (ISO 8601)
	"2006-01-02T15:04:05Z",                  // ISO 8601 UTC
	"2006-01-02T15:04:05-07:00",             // ISO 8601 with offset
	"2006-01-02 15:04:05 -0700",             // SQL-like format
	"2006-01-02 15:04:05",                   // SQL-like without TZ
	"Mon, Jan 2 2006 15:04:05 -0700",        // Weekday, US month-day order, no comma after day
}

// numericOffsetRe matches numeric timezone offsets like +0000, -0700, +00:00, -07:00.
var numericOffsetRe = regexp.MustCompile(`[+-]\d{2}:?\d{2}`)

var emailAddressTokenRe = regexp.MustCompile("[A-Za-z0-9.!#$%&'*+/=?^_`{|}~-]+@[A-Za-z0-9.-]+")

// hasNumericOffset returns true if the string contains a numeric timezone offset or Z (UTC).
// Named timezones like "MST" have platform-dependent behavior in Go's time.Parse,
// so we need to handle them specially.
func hasNumericOffset(s string) bool {
	if strings.HasSuffix(s, "Z") {
		return true
	}
	return numericOffsetRe.MatchString(s)
}

// toUTC converts a time to UTC. If the original had a numeric offset, perform
// proper timezone conversion. Otherwise (named timezone only), keep the same
// local time values but mark them as UTC (since named TZ offsets are unreliable
// across platforms).
func toUTC(t time.Time, numericOffset bool) time.Time {
	if numericOffset {
		return t.UTC()
	}
	// Named timezone: keep same time values, mark as UTC
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
}

// parseDate attempts to parse a date string in various formats.
// Returns the time in UTC for consistent storage.
// Named timezones (like "MST") are treated as UTC since their offsets
// can't be reliably determined across platforms.
// parseDate returns the zero time when no known format matches; callers
// detect failure via the zero value rather than an error.
func parseDate(s string) time.Time {
	// Normalize whitespace efficiently: split on whitespace runs and rejoin
	s = strings.Join(strings.Fields(s), " ")

	// Some mbox-derived sources have a stray, unindented continuation line
	// (a lone ".") directly after the Date header. enmime folds it onto the
	// header value instead of treating it as a parse error, leaving a
	// trailing " ." that no real Date header would ever contain.
	s = strings.TrimSuffix(s, " .")

	// Strip trailing timezone name in parentheses like "(UTC)" or "(PST)"
	// but keep the numeric offset for parsing
	baseStr := s
	if idx := strings.LastIndex(s, "("); idx > 0 {
		baseStr = strings.TrimSpace(s[:idx])
	}

	// Check if we have a numeric offset for proper UTC conversion
	numericOffset := hasNumericOffset(baseStr)

	// Use ParseInLocation with time.UTC so that named timezone abbreviations
	// (EST, PST, etc.) are treated as offset 0 rather than resolved against
	// the local system timezone, which is platform-dependent. Numeric offsets
	// like -0700 are absolute and unaffected by the reference location.
	for _, format := range dateFormats {
		if t, err := time.ParseInLocation(format, baseStr, time.UTC); err == nil {
			return toUTC(t, numericOffset)
		}
	}

	// Try original string (some formats expect the parenthesized part)
	if baseStr != s {
		for _, format := range dateFormats {
			if t, err := time.ParseInLocation(format, s, time.UTC); err == nil {
				// Recompute numericOffset for the original string since it may
				// have a different offset than baseStr (e.g., "+0700 (UTC)")
				return toUTC(t, hasNumericOffset(s))
			}
		}
	}

	return time.Time{}
}

// Block tags that should create line breaks when stripped.
var blockTagRe = regexp.MustCompile(`(?i)<(/?)(p|div|br|hr|h[1-6]|li|tr|td|th|blockquote|pre|table|ul|ol|dl|dt|dd)[^>]*>`)

// Patterns for content-stripping tags (each needs separate pattern due to Go regex limitations).
var scriptTagRe = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
var styleTagRe = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
var headTagRe = regexp.MustCompile(`(?is)<head[^>]*>.*?</head>`)
var htmlTagRe = regexp.MustCompile(`<[^>]*>`)

// StripHTML removes HTML tags, decodes entities, and normalizes whitespace.
// Block elements are converted to line breaks for readable plain text output.
//
// Note: Preformatted content (<pre>, <code>) loses its whitespace formatting
// as all runs of spaces are collapsed. This is acceptable for email preview
// where preserving exact code formatting is less important than readability.
func StripHTML(rawHTML string) string {
	// Remove script, style, and head tags entirely (including their content)
	text := scriptTagRe.ReplaceAllString(rawHTML, "")
	text = styleTagRe.ReplaceAllString(text, "")
	text = headTagRe.ReplaceAllString(text, "")

	// Add newlines for block tags to create paragraph separation.
	// Both opening and closing block tags emit newlines so consecutive
	// blocks (like </p><p>) get proper spacing. Leading/trailing blank
	// lines are removed by the final TrimSpace.
	text = blockTagRe.ReplaceAllStringFunc(text, func(match string) string {
		return "\n"
	})

	// Strip remaining HTML tags
	text = htmlTagRe.ReplaceAllString(text, "")

	// Decode HTML entities (&nbsp;, &amp;, &#160;, etc.)
	text = html.UnescapeString(text)

	// Normalize whitespace
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	// Replace non-breaking spaces with regular spaces
	text = strings.ReplaceAll(text, "\u00A0", " ")

	// Collapse multiple spaces on the same line (but preserve newlines)
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.Join(strings.Fields(line), " ")
	}
	text = strings.Join(lines, "\n")

	// Collapse multiple newlines (max 2)
	for strings.Contains(text, "\n\n\n") {
		text = strings.ReplaceAll(text, "\n\n\n", "\n\n")
	}

	return strings.TrimSpace(text)
}

// GetBodyText returns the best available body text.
// Prefers plain text, falls back to stripped HTML.
func (m *Message) GetBodyText() string {
	if m.BodyText != "" {
		return m.BodyText
	}
	if m.BodyHTML != "" {
		return StripHTML(m.BodyHTML)
	}
	return ""
}

// GetFirstFrom returns the first From address, or empty if none.
func (m *Message) GetFirstFrom() Address {
	if len(m.From) > 0 {
		return m.From[0]
	}
	return Address{}
}
