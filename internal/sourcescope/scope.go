// Package sourcescope carries caller-authorized archive sources through a read.
package sourcescope

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
)

type contextKey struct{}
type incomingKey struct{}

// IncomingRestricted reports whether an incoming sender scope was installed.
func IncomingRestricted(ctx context.Context) bool {
	_, ok := ctx.Value(incomingKey{}).(map[int64][]string)
	return ok
}

// WithIncomingSenders records sender restrictions for authorized sources.
func WithIncomingSenders(ctx context.Context, senders map[int64][]string) context.Context {
	owned := make(map[int64][]string, len(senders))
	for id, keys := range senders {
		owned[id] = slices.Clone(keys)
	}
	return context.WithValue(ctx, incomingKey{}, owned)
}

// UnrestrictedIncomingSourceIDs returns sources without sender restrictions.
func UnrestrictedIncomingSourceIDs(ctx context.Context) []int64 {
	senders, _ := ctx.Value(incomingKey{}).(map[int64][]string)
	var ids []int64
	for _, id := range slices.Sorted(maps.Keys(senders)) {
		if len(senders[id]) == 0 {
			ids = append(ids, id)
		}
	}
	return ids
}

// ConversationTitle hides shared titles when a source has a sender restriction.
func ConversationTitle(ctx context.Context, sourceID int64, title string) string {
	senders, _ := ctx.Value(incomingKey{}).(map[int64][]string)
	if len(senders[sourceID]) > 0 {
		return ""
	}
	return title
}

// IncomingPredicate restricts messages to their source's literal From snapshots.
func IncomingPredicate(ctx context.Context, message, source string, lower func(string) string) (string, []any) {
	senders, ok := ctx.Value(incomingKey{}).(map[int64][]string)
	if !ok {
		return "1=1", nil
	}
	var branches []string
	var args []any
	for _, id := range slices.Sorted(maps.Keys(senders)) {
		keys := senders[id]
		args = append(args, id)
		branch := source + " = ?"
		if len(keys) > 0 {
			placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
			branch += fmt.Sprintf(` AND EXISTS (SELECT 1 FROM message_recipients scope_from WHERE scope_from.message_id = %s AND scope_from.recipient_type = 'from') AND NOT EXISTS (SELECT 1 FROM message_recipients scope_bad WHERE scope_bad.message_id = %s AND scope_bad.recipient_type = 'from' AND (scope_bad.email_address IS NULL OR scope_bad.email_address = '' OR %s NOT IN (%s)))`, message, message, lower("scope_bad.email_address"), placeholders)
			for _, key := range keys {
				args = append(args, key)
			}
		}
		branches = append(branches, "("+branch+")")
	}
	if len(branches) == 0 {
		return "1=0", nil
	}
	return "(" + strings.Join(branches, " OR ") + ")", args
}

// WithSourceIDs installs a copied source set. An explicit nil or empty set
// authorizes no sources; absence of the value leaves a request unrestricted.
func WithSourceIDs(ctx context.Context, ids []int64) context.Context {
	owned := make([]int64, len(ids))
	copy(owned, ids)
	return context.WithValue(ctx, contextKey{}, owned)
}

// IDs returns an owned copy; nil means no authorization scope was installed.
func IDs(ctx context.Context) []int64 {
	ids, _ := ctx.Value(contextKey{}).([]int64)
	return slices.Clone(ids)
}

// Restricted reports whether the caller installed an authorization scope.
func Restricted(ctx context.Context) bool {
	_, ok := ctx.Value(contextKey{}).([]int64)
	return ok
}
