package search

import "strings"

// AccountConditions returns the SQL predicates for account: and received:
// against the messages table aliased as alias. Repeated values of one
// operator are OR'd; the two operators are AND'd. account: matches every
// path (received, sent and calendar); received: matches inbound mail only.
func AccountConditions(q *Query, alias string) ([]string, []any) {
	if q == nil {
		return nil, nil
	}
	var conditions []string
	var args []any
	in := func(values []string) string {
		for _, v := range values {
			args = append(args, v)
		}
		return alias + ".account_address IN (" + strings.TrimSuffix(strings.Repeat("?,", len(values)), ",") + ")"
	}
	if len(q.AccountAddrs) > 0 {
		conditions = append(conditions, in(q.AccountAddrs))
	}
	if len(q.ReceivedAddrs) > 0 {
		conditions = append(conditions, "("+alias+".account_path = 'inbound' AND "+in(q.ReceivedAddrs)+")")
	}
	return conditions, args
}
