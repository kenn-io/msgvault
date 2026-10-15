package store

import (
	"strings"

	"go.kenn.io/msgvault/internal/search"
)

// CombineOR intersects parent criteria with alternative predicates.
func CombineOR(q *search.Query, build func(*search.Query) ([]string, []any)) ([]string, []any) {
	parent := *q
	parent.Alternatives = nil
	conditions, args := build(&parent)
	var alternatives []string
	for _, alternative := range q.Alternatives {
		branch := *alternative
		parts, branchArgs := build(&branch)
		alternatives = append(alternatives, "("+strings.Join(parts, " AND ")+")")
		args = append(args, branchArgs...)
	}
	conditions = append(conditions, "("+strings.Join(alternatives, " OR ")+")")
	return conditions, args
}
