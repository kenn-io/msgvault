//go:build cgo

package query

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
)

func TestSenderNameHintCancellation(t *testing.T) {
	engine, observer := observedSearchEngine(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	observer.BeforeQuery = func(_ context.Context, statement string) {
		if strings.Contains(statement, "WITH candidates AS") {
			cancel()
		}
	}
	_, err := engine.SearchFastWithStats(ctx, search.Parse("from:alex"), "", MessageFilter{IncludeHints: true}, ViewNoStats, 10, 0)
	require.ErrorIs(t, err, context.Canceled)
}
