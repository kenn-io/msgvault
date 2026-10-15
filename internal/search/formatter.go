package search

import (
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Format renders the representable fields of q as a canonical query string
// that ParseOR can read back without changing their meaning. AccountIDs,
// HideDeleted, UnsupportedOperators, and parse errors are transport/context
// state rather than query-language fields and are intentionally omitted.
func Format(q *Query) string {
	if q == nil {
		return ""
	}
	if q.HasOR() {
		parts := make([]string, 0, len(q.Alternatives))
		for _, alternative := range q.Alternatives {
			parent, branch := *q, *alternative
			parent.Alternatives = nil
			impossible := false
			// Repeated type and account values union, so intersect scopes before formatting.
			for _, values := range []struct{ scope, branch *[]string }{
				{&parent.MessageTypes, &branch.MessageTypes},
				{&parent.AccountAddrs, &branch.AccountAddrs},
			} {
				if len(*values.scope) == 0 || len(*values.branch) == 0 {
					continue
				}
				*values.scope, impossible = IntersectValues(*values.branch, *values.scope)
				if impossible {
					break
				}
				*values.branch = nil
			}
			if impossible {
				continue
			}
			parts = append(parts, strings.TrimSpace(Format(&parent)+" "+Format(&branch)))
		}
		if len(parts) == 0 {
			return Format(&Query{ConversationIDs: []int64{}})
		}
		return strings.Join(parts, " | ")
	}

	var parts []string
	for _, term := range q.TextTerms {
		parts = append(parts, formatSearchValue(term, true))
	}
	parts = appendSearchOperators(parts, "from", q.FromAddrs)
	parts = appendSearchOperators(parts, "to", q.ToAddrs)
	parts = appendSearchOperators(parts, "cc", q.CcAddrs)
	parts = appendSearchOperators(parts, "bcc", q.BccAddrs)
	parts = appendSearchOperators(parts, "subject", q.SubjectTerms)
	parts = appendSearchOperators(parts, "label", q.Labels)
	parts = appendSearchOperators(parts, "filename", q.Filenames)
	parts = appendSearchOperators(parts, "list", q.ListIDs)
	parts = appendSearchOperators(parts, "account", q.AccountAddrs)
	parts = appendSearchOperators(parts, "received", q.ReceivedAddrs)
	parts = appendSearchOperators(parts, "message_type", q.MessageTypes)
	if q.ConversationIDs != nil && len(q.ConversationIDs) == 0 {
		parts = append(parts, "conversation_id:"+conversationIDMatchNone)
	}
	for _, conversationID := range q.ConversationIDs {
		parts = append(parts, "conversation_id:"+strconv.FormatInt(conversationID, 10))
	}
	if q.HasAttachment != nil && *q.HasAttachment {
		parts = append(parts, "has:attachment")
	}
	if q.BeforeDate != nil {
		parts = append(parts, "before:"+formatSearchTime(*q.BeforeDate))
	}
	if q.AfterDate != nil {
		parts = append(parts, "after:"+formatSearchTime(*q.AfterDate))
	}
	if q.LargerThan != nil {
		parts = append(parts, "larger:"+strconv.FormatInt(*q.LargerThan, 10))
	}
	if q.SmallerThan != nil {
		parts = append(parts, "smaller:"+strconv.FormatInt(*q.SmallerThan, 10))
	}
	return strings.Join(parts, " ")
}

func formatSearchTime(value time.Time) string {
	utc := value.UTC()
	if utc.Hour() == 0 && utc.Minute() == 0 && utc.Second() == 0 && utc.Nanosecond() == 0 {
		return utc.Format(time.DateOnly)
	}
	return value.Format(time.RFC3339Nano)
}

func appendSearchOperators(parts []string, operator string, values []string) []string {
	for _, value := range values {
		parts = append(parts, operator+":"+formatSearchValue(value, false))
	}
	return parts
}

func formatSearchValue(value string, standalone bool) string {
	if !searchValueNeedsQuotes(value, standalone) {
		return value
	}
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value)
	return `"` + escaped + `"`
}

func searchValueNeedsQuotes(value string, standalone bool) bool {
	if value == "" || strings.ContainsAny(value, `"\'|`) {
		return true
	}
	for _, char := range value {
		if unicode.IsSpace(char) {
			return true
		}
	}
	return standalone && (strings.Contains(value, ":") ||
		strings.HasPrefix(strings.ToLower(value), "message_type="))
}
