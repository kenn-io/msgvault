package loops

import (
	"regexp"

	"go.kenn.io/msgvault/internal/mime"
)

var (
	calendarSubject = regexp.MustCompile(`(?i)^(invitation|updated invitation|accepted|declined|tentatively accepted|canceled event|cancelled event):`)
	chatBot         = regexp.MustCompile(`(?i)(^|[^a-z])bot($|[^a-z])|^@.*bot:|bridge bot`)
)

func metadataSubstantive(m LoopMessage) bool {
	if m.Deleted || (m.Direction != Inbound && m.Direction != Outbound) {
		return false
	}
	if mime.IsAutomatedAddress(m.Sender.Email) || calendarSubject.MatchString(m.Subject) {
		return false
	}
	if m.Signals != nil && (m.Signals.AutoSubmitted || m.Signals.CalendarPart || m.Signals.ReplyToNoReply) {
		return false
	}
	return m.Sender.Email != "" || m.Sender.Phone != "" ||
		(!chatBot.MatchString(m.Sender.DisplayName) && !chatBot.MatchString(m.Sender.CanonicalID))
}
