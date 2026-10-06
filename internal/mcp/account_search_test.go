package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSearchMetadataFindsMailByReceivingAccount(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "work@example.org", "manual"))
	forwarded := f.NewMessage().WithSourceMessageID("forwarded").WithSubject("forwarded").Create(t, f.Store)
	require.NoError(f.Store.UpsertMessageRaw(forwarded, []byte("X-Delivered-To: work@example.org\r\nTo: list@example.com\r\n\r\nbody")))
	f.NewMessage().WithSourceMessageID("other").WithSubject("other").Create(t, f.Store)

	engine := query.NewSQLiteEngine(f.Store.DB())
	if f.Store.IsPostgreSQL() {
		engine = query.NewEngineWithDialect(f.Store.DB(), query.PostgreSQLQueryDialect{})
	}
	resp := runTool[paginatedSearchMessages](t, "search_metadata", newTestHandlers(engine).searchMetadata,
		map[string]any{"query": "received:work@example.org"})
	require.Len(resp.Data, 1)
	assert.Equal(forwarded, resp.Data[0].ID)
}
