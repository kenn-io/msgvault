package query

import (
	"context"
	"errors"
)

var ErrThreadTooLarge = errors.New("thread exceeds restricted ingress membership limit")

type messageByteLimitKey struct{}

// WithMessageByteLimit bounds direct message content reads for restricted ingress.
func WithMessageByteLimit(ctx context.Context, maxBytes int64) context.Context {
	return context.WithValue(ctx, messageByteLimitKey{}, maxBytes)
}
func messageByteLimit(ctx context.Context) int64 {
	value, _ := ctx.Value(messageByteLimitKey{}).(int64)
	return value
}
