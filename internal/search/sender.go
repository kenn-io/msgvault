package search

import (
	"strings"

	"go.kenn.io/msgvault/internal/identityindex"
)

// SenderDomainCondition matches the recorded domain on an explicit From row.
func SenderDomainCondition(placeholder string) string {
	return `EXISTS (SELECT 1 FROM message_recipients mr JOIN participants p ON p.id = mr.participant_id WHERE mr.message_id = m.id AND mr.recipient_type = 'from' AND COALESCE(CASE WHEN mr.is_relayed THEN ` + identityindex.SenderEnvelopeDomainExpr("mr.email_address") + ` END, p.domain) = ` + placeholder + `)`
}

// escapeLike makes SQL LIKE wildcards literal with ESCAPE '\'.
func escapeLike(value string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value)
}

// senderMatch classifies domains, complete addresses, and metadata fragments.
func senderMatch(value string) (operator, pattern string, metadata bool) {
	value = normalizeAddr(value)
	if strings.HasPrefix(value, "@") {
		return "LIKE", "%" + escapeLike(value), false
	}
	if address, err := ParseAccountAddress(value); err == nil {
		return "=", address, false
	}
	return "LIKE", "%" + escapeLike(value) + "%", true
}

func senderCondition(operator, pattern string, metadata bool, lower func(string) string) (string, []any) {
	if operator == "=" {
		// Match the existing case-insensitive email index for exact mailboxes.
		return "(LOWER(p2.email_address) = ?)", []any{pattern}
	}
	columns := []string{"p2.email_address"}
	if metadata {
		columns = append(columns, "p2.phone_number", "p2.display_name")
	}
	var parts []string
	var args []any
	for _, column := range columns {
		condition := lower("COALESCE("+column+", '')") + ` LIKE ? ESCAPE '\'`
		parts = append(parts, condition)
		args = append(args, pattern)
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}

// SenderLookup matches one from: value through From rows and direct senders.
// Both forms bind identical arguments so adaptive search can switch between them.
// Callers supply trusted table references and the outer message alias.
func SenderLookup(value string, lower func(string) string, messages, recipients, participants, alias string) (condition, matching string, args []any) {
	return senderLookup(value, lower, messages, recipients, participants, alias, "email_address", false)
}

// SenderLookupCached distinguishes the recorded address from a cache's resolved address.
func SenderLookupCached(value string, lower func(string) string, messages, recipients, participants, alias string) (condition, matching string, args []any) {
	return senderLookup(value, lower, messages, recipients, participants, alias, "envelope_address", true)
}

func senderLookup(value string, lower func(string) string, messages, recipients, participants, alias, snapshotColumn string, cached bool) (condition, matching string, args []any) {
	operator, pattern, metadata := senderMatch(value)
	participant, participantArgs := senderCondition(operator, pattern, metadata, lower)
	args = append(args, participantArgs...)
	address := "mr2.email_address"
	if cached {
		guard := `NOT EXISTS (SELECT 1 FROM ` + recipients + ` mr_address_guard WHERE mr_address_guard.message_id = mr2.message_id AND mr_address_guard.recipient_type = 'from' AND COALESCE(mr_address_guard.` + snapshotColumn + `, '') <> '')`
		address = `CASE WHEN ` + guard + ` THEN mr2.email_address ELSE mr2.envelope_address END`
	}
	envelope := lower("COALESCE(("+address+"), '')") + ` LIKE ? ESCAPE '\'`
	if operator == "=" {
		envelope = "LOWER(" + address + ") = ?"
	}
	recipientFallback := ""
	directFallback := ""
	matchingFallback := ""
	if !metadata {
		guard := func(messageID string) string {
			return ` AND NOT EXISTS (SELECT 1 FROM ` + recipients + ` mr_guard WHERE mr_guard.message_id = ` + messageID + ` AND mr_guard.recipient_type = 'from' AND mr_guard.is_relayed AND COALESCE(mr_guard.` + snapshotColumn + `, '') <> '')`
		}
		recipientFallback = guard("mr2.message_id")
		directFallback = guard(alias + ".id")
		matchingFallback = guard("ms.id")
	}
	from := `EXISTS (SELECT 1 FROM ` + recipients + ` mr2 WHERE mr2.message_id = ` + alias + `.id AND mr2.recipient_type = 'from' AND ` + envelope + `) OR EXISTS (SELECT 1 FROM ` + recipients + ` mr2 JOIN ` + participants + ` p2 ON p2.id = mr2.participant_id
  WHERE mr2.message_id = ` + alias + `.id AND mr2.recipient_type = 'from' AND ` + participant + recipientFallback + `)`
	args = append([]any{pattern}, args...)
	matching = `SELECT mr2.message_id FROM ` + recipients + ` mr2 WHERE mr2.recipient_type = 'from' AND ` + envelope + ` UNION SELECT mr2.message_id FROM ` + recipients + ` mr2 WHERE mr2.recipient_type = 'from'
  AND mr2.participant_id IN (SELECT p2.id FROM ` + participants + ` p2 WHERE ` + participant + `)` + recipientFallback
	if metadata {
		header := lower("COALESCE(mr2.display_name, '')") + ` LIKE ? ESCAPE '\'`
		from += ` OR EXISTS (SELECT 1 FROM ` + recipients + ` mr2 WHERE mr2.message_id = ` + alias + `.id AND mr2.recipient_type = 'from' AND ` + header + `)`
		matching += ` UNION SELECT mr2.message_id FROM ` + recipients + ` mr2 WHERE mr2.recipient_type = 'from' AND ` + header
		args = append(args, pattern)
	}
	condition = `(` + from + ` OR EXISTS (SELECT 1 FROM ` + participants + ` p2 WHERE p2.id = ` + alias + `.sender_id AND ` + participant + `)` + directFallback + `)`
	matching += ` UNION SELECT ms.id FROM ` + messages + ` ms WHERE ms.sender_id IN (SELECT p2.id FROM ` + participants + ` p2 WHERE ` + participant + `)` + matchingFallback
	args = append(args, participantArgs...)
	return condition, matching, args
}
