// Package sourcescope carries caller-authorized archive sources through a read.
package sourcescope

import (
	"context"
	"slices"
)

type contextKey struct{}

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
