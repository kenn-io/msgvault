package search

import "strings"

// EscapeLike makes SQL LIKE wildcards literal with ESCAPE '\'.
func EscapeLike(value string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(value)
}

// senderMatch classifies domains, complete addresses, and metadata fragments.
func senderMatch(value string) (operator, pattern string, metadata bool) {
	value = strings.ToLower(value)
	if strings.HasPrefix(value, "@") {
		return "LIKE", "%" + EscapeLike(value), false
	}
	local, domain, address := strings.Cut(value, "@")
	if address && local != "" && strings.Contains(domain, ".") && !strings.HasPrefix(domain, ".") && !strings.HasSuffix(domain, ".") {
		return "=", value, false
	}
	return "LIKE", "%" + EscapeLike(value) + "%", true
}

// SenderCondition matches any normalized from: value against sender metadata.
// The SQL owner supplies column expressions and its Unicode folding function.
func SenderCondition(values []string, email, phone string, lower func(string) string, names ...string) (string, []any) {
	var parts []string
	var args []any
	for _, value := range values {
		operator, pattern, metadata := senderMatch(value)
		columns := []string{email}
		if metadata {
			columns = append(columns, phone)
			columns = append(columns, names...)
		}
		for _, column := range columns {
			condition := lower("COALESCE("+column+", '')") + " " + operator + " ?"
			if operator == "LIKE" {
				condition += ` ESCAPE '\'`
			}
			parts = append(parts, condition)
			args = append(args, pattern)
		}
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}

// SenderLookup shares correlated and participant-first message lookups.
// Both forms bind the same arguments so adaptive search can switch between them.
func SenderLookup(values []string, lower func(string) string) (condition, matching string, args []any) {
	participant, args := SenderCondition(values, "p2.email_address", "p2.phone_number", lower, "p2.display_name")
	predicate := participant
	matching = `SELECT mr2.message_id FROM message_recipients mr2 WHERE mr2.recipient_type = 'from'
  AND mr2.participant_id IN (SELECT p2.id FROM participants p2 WHERE ` + participant + `)`
	var headers []string
	for _, value := range values {
		_, pattern, metadata := senderMatch(value)
		if metadata {
			headers = append(headers, lower("COALESCE(mr2.display_name, '')")+` LIKE ? ESCAPE '\'`)
			args = append(args, pattern)
		}
	}
	if len(headers) > 0 {
		header := "(" + strings.Join(headers, " OR ") + ")"
		predicate = "(" + participant + " OR " + header + ")"
		matching += ` UNION SELECT mr2.message_id FROM message_recipients mr2 WHERE mr2.recipient_type = 'from' AND ` + header
	}
	condition = `EXISTS (SELECT 1 FROM message_recipients mr2 JOIN participants p2 ON p2.id = mr2.participant_id
  WHERE mr2.message_id = m.id AND mr2.recipient_type = 'from' AND ` + predicate + `)`
	return condition, matching, args
}
