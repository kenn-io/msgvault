package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
)

// A SQL-capable engine must not make generic SQL available to MCP clients.
// DuckDB's read-only SELECT can still read local files through table functions.
type sqlToolEngine struct {
	*querytest.MockEngine
}

func (*sqlToolEngine) QuerySQLWithFresh(context.Context, string, bool) (*query.QueryResult, *daemonclient.CacheBuildAccepted, error) {
	return &query.QueryResult{Columns: []string{"value"}, Rows: [][]any{{"private-token"}}, RowCount: 1}, nil, nil
}

func TestMCPDoesNotExposeGenericSQL(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	engine := &sqlToolEngine{MockEngine: &querytest.MockEngine{}}
	listed := toolsByName(t, rawListTools(t, ServeOptions{Engine: engine}, false))
	assertions.NotContains(listed, "query_sql",
		"generic SQL allows local-file table functions such as read_text and read_csv_auto")
	assertions.Contains(listed, ToolAggregate)
	assertions.Contains(listed, ToolGetStats)

	secretPath := filepath.Join(t.TempDir(), "token.txt")
	requirements.NoError(os.WriteFile(secretPath, []byte("private-token"), 0600))
	client := task5ConnectClient(t, ServeOptions{Engine: engine}, false)
	result, err := client.CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name: "query_sql",
		Arguments: map[string]any{
			"sql": fmt.Sprintf("SELECT * FROM read_text('%s')", secretPath),
		},
	})
	requirements.Error(err)
	assertions.Nil(result)
}
