package search

import "strings"

// SenderCondition matches a normalized from: value against sender metadata.
// Column expressions are supplied by the SQL owner; values are always bound.
func SenderCondition(value, email, phone string, names ...string) (string, []any) {
	value = strings.ToLower(value)
	escape := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	if strings.HasPrefix(value, "@") {
		return `LOWER(` + email + `) LIKE ? ESCAPE '\'`, []any{"%" + escape.Replace(value)}
	}
	if strings.Contains(value, "@") {
		return "LOWER(" + email + ") = ?", []any{value}
	}
	columns := append([]string{email, phone}, names...)
	parts := make([]string, 0, len(columns))
	args := make([]any, 0, len(columns))
	for _, column := range columns {
		parts = append(parts, `LOWER(`+column+`) LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escape.Replace(value)+"%")
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}
