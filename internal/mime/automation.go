package mime

import (
	"regexp"
	"strings"

	"github.com/jhillyerd/enmime/v2"
)

// AutomationParserVersion identifies the frozen automation rules used by
// archive backfills. Change it when the extracted evidence changes.
const AutomationParserVersion = 1

// AutomationSignals records MIME evidence rather than deciding whether a
// conversation deserves a reply. The parser owns this value type so archive
// persistence can consume it without a mime-to-store dependency.
type AutomationSignals struct {
	AutoSubmitted   bool
	PrecedenceBulk  bool
	ListUnsubscribe bool
	CalendarPart    bool
	ReplyToNoReply  bool
}

var automatedAddress = regexp.MustCompile(`(?i)^(no-?reply|do-?not-?reply|donotreply|noreply[-_.]|notifications?|notify|mailer-daemon|postmaster|bounces?|alerts?|digest|newsletter|news|updates|billing|receipts?|support|info|hello|team|calendar-notification|invitations?)(\+[^@]*)?@`)

// IsAutomatedAddress applies the conservative sender rule shared by MIME
// Reply-To evidence and deterministic conversation turns. Only the local part
// is inspected for embedded no-reply markers.
func IsAutomatedAddress(address string) bool {
	address = strings.TrimSpace(address)
	local, _, ok := strings.Cut(address, "@")
	if !ok {
		return false
	}
	local = strings.ToLower(local)
	return automatedAddress.MatchString(address) ||
		strings.Contains(local, "noreply") || strings.Contains(local, "no-reply")
}

// AutomationSignals returns the header and part facts available to downstream
// rules. Missing evidence remains false, including on header-only recovery.
func (m *Message) AutomationSignals() AutomationSignals {
	if m == nil {
		return AutomationSignals{}
	}
	// RFC 3834 permits surrounding CFWS and optional parameters. Preserve the
	// raw field on Message, but compare only its keyword with "no".
	autoSubmitted, _, _ := strings.Cut(stripAddressDecorations(m.AutoSubmitted), ";")
	autoSubmitted = strings.TrimSpace(autoSubmitted)
	result := AutomationSignals{
		AutoSubmitted:   autoSubmitted != "" && !strings.EqualFold(autoSubmitted, "no"),
		ListUnsubscribe: m.ListUnsubscribe,
		CalendarPart:    m.HasCalendarPart,
	}
	switch strings.ToLower(strings.TrimSpace(m.Precedence)) {
	case "bulk", "list", "junk":
		result.PrecedenceBulk = true
	}
	for _, address := range m.ReplyTo {
		if IsAutomatedAddress(address.Email) {
			result.ReplyToNoReply = true
			break
		}
	}
	return result
}

func hasCalendarPart(root *enmime.Part) bool {
	if root == nil {
		return false
	}
	if strings.EqualFold(root.ContentType, "text/calendar") {
		return true
	}
	for child := root.FirstChild; child != nil; child = child.NextSibling {
		if hasCalendarPart(child) {
			return true
		}
	}
	return false
}
