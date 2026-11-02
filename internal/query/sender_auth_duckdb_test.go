package query

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/senderauth"
)

func TestSenderAuthDuckDBSearchAndCachedPage(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	env := newTestEnv(t)
	e := newParquetEngine(t)
	e.sqliteEngine = env.Engine
	e.SetSenderAuthPins(map[int64]senderauth.TrustSet{1: {"mx.test"}})
	raw := func(result string) []byte {
		return []byte("From: alice@example.com\r\nAuthentication-Results: mx.test; dmarc=" + result + " header.from=example.com\r\n\r\nBody")
	}
	_, err := env.DB.Exec(`INSERT INTO message_raw(message_id,raw_data,raw_format,compression) VALUES (1,?,'mime','none')`, raw("pass"))
	must.NoError(err)
	ctx := context.Background()
	q := search.Parse("Hello")
	rows, err := e.SearchFast(ctx, q, MessageFilter{}, 10, 0)
	must.NoError(err)
	must.NotEmpty(rows)
	first := func(messages []MessageSummary) *senderauth.Verdict {
		for _, m := range messages {
			if m.ID == 1 {
				return m.SenderAuth
			}
		}
		return nil
	}
	receipt := first(rows)
	must.NotNil(receipt)
	checks.True(receipt.Verified)
	page, err := e.SearchFastWithStats(ctx, q, "Hello", MessageFilter{}, ViewNoStats, 10, 0)
	must.NoError(err)
	receipt = first(page.Messages)
	must.NotNil(receipt)
	checks.True(receipt.Verified)
	_, err = env.DB.Exec(`UPDATE message_raw SET raw_data=? WHERE message_id=1`, raw("fail"))
	must.NoError(err)
	page, err = e.SearchFastWithStats(ctx, q, "Hello", MessageFilter{}, ViewNoStats, 10, 0)
	must.NoError(err)
	receipt = first(page.Messages)
	must.NotNil(receipt)
	checks.False(receipt.Verified)
	checks.Equal(senderauth.Fail, receipt.DMARC)
}
