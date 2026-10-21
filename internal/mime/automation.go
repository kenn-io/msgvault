package mime

import (
	"strings"

	"github.com/jhillyerd/enmime/v2"
)

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

// IsNoReplyAddress recognizes explicit no-reply markers. Other service
// addresses such as support@example.test may accept replies, so they are not
// enough to disqualify a message by Reply-To.
func IsNoReplyAddress(address string) bool {
	address = strings.TrimSpace(address)
	local, _, ok := strings.Cut(address, "@")
	if !ok {
		return false
	}
	local = strings.ToLower(local)
	return strings.Contains(local, "noreply") || strings.Contains(local, "no-reply") ||
		strings.Contains(local, "donotreply") || strings.Contains(local, "do-not-reply")
}

// AutomationSignals returns the header and part facts available to downstream
// rules. Missing evidence remains false, including on header-only recovery.
func (m *Message) AutomationSignals() AutomationSignals {
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
		if IsNoReplyAddress(address.Email) {
			result.ReplyToNoReply = true
			break
		}
	}
	return result
}

func hasCalendarPart(root *enmime.Part) bool {
	return root.DepthMatchFirst(func(p *enmime.Part) bool {
		return strings.EqualFold(p.ContentType, "text/calendar")
	}) != nil
}
