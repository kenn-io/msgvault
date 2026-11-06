//go:build cgo

package query

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
)

func TestSenderNameHintTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require := require.New(t)
		engine, observer := observedSearchEngine(t)
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		blocked := false
		observer.BeforeQuery = func(queryCtx context.Context, sql string) {
			if strings.Contains(sql, "WITH candidates AS") {
				blocked = true
				<-queryCtx.Done()
			}
		}
		var diagnostic bytes.Buffer
		previous := log.Writer()
		log.SetOutput(&diagnostic)
		t.Cleanup(func() { log.SetOutput(previous) })
		result, err := engine.SearchFastWithStats(ctx, search.Parse("from:alex"), "", MessageFilter{}, ViewNoStats, 10, 0)
		require.NoError(err)
		require.True(blocked)
		require.Empty(result.Hints)
		require.NoError(ctx.Err())
		require.NotContains(diagnostic.String(), "sender hint query failed")
	})
}

func TestSenderNameHintCancellation(t *testing.T) {
	engine, observer := observedSearchEngine(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	observer.BeforeQuery = func(_ context.Context, statement string) {
		if strings.Contains(statement, "WITH candidates AS") {
			cancel()
		}
	}
	_, err := engine.SearchFastWithStats(ctx, search.Parse("from:alex"), "", MessageFilter{}, ViewNoStats, 10, 0)
	require.ErrorIs(t, err, context.Canceled)
}
