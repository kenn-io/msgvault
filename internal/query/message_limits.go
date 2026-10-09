package query

import (
	"context"
	"errors"
	"fmt"
	"math"
)

// ErrThreadTooLarge means a whole-thread read exceeded ThreadQuery.MaxMembers.
var ErrThreadTooLarge = errors.New("thread exceeds remote client membership limit")

type messageByteLimitKey struct{}

// WithMessageByteLimit bounds direct message content reads for remote clients.
func WithMessageByteLimit(ctx context.Context, maxBytes int64) context.Context {
	return context.WithValue(ctx, messageByteLimitKey{}, maxBytes)
}

func messageByteLimit(ctx context.Context) int64 {
	value, _ := ctx.Value(messageByteLimitKey{}).(int64)
	return value
}

// messageRawColumns returns the raw_data column (NULL when over the limit) and its stored size.
func messageRawColumns(tablePrefix string, limit int64) (string, string) {
	rawSize := "length(mr.raw_data)"
	if tablePrefix == "sqlite_db." {
		rawSize = "octet_length(mr.raw_data)"
	}
	if limit <= 0 {
		return "mr.raw_data", rawSize
	}
	// zlib output is never much larger than its input, so this only skips rows that would inflate past limit.
	return fmt.Sprintf("CASE WHEN %s <= CASE WHEN mr.compression = 'zlib' THEN %d ELSE %d END THEN mr.raw_data END",
		rawSize, compressedMessageByteLimit(limit), limit), rawSize
}

func compressedMessageByteLimit(limit int64) int64 {
	if limit <= 0 || limit > (math.MaxInt64-1024)/2 {
		return math.MaxInt64
	}
	return 2*limit + 1024
}
