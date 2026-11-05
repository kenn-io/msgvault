package query

import "strings"

// mailboxStateConditions filters either projected Explore columns or facts
// identified by a message ID expression. Expressions come from query code;
// every filter value remains a bound argument.
func mailboxStateConditions(context Context, messageID string) ([]string, []any) {
	var conditions []string
	var args []any
	for _, filter := range []struct {
		values []string
		column string
	}{
		{context.InboxStates, "inbox_state"}, {context.ReadStates, "read_state"},
	} {
		if len(filter.values) == 0 {
			continue
		}
		expression := filter.column
		if messageID != "" {
			expression = "COALESCE((SELECT mailbox." + filter.column + " FROM mailbox_state mailbox WHERE mailbox.message_id=" + messageID + "),'unknown')"
		}
		placeholders := make([]string, len(filter.values))
		for i, value := range filter.values {
			placeholders[i] = "?"
			args = append(args, value)
		}
		conditions = append(conditions, expression+" IN ("+strings.Join(placeholders, ",")+")")
	}
	return conditions, args
}
