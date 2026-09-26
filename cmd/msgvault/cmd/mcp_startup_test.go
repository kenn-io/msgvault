package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
)

const (
	mcpStartupChildEnv = "MSGVAULT_MCP_STARTUP_CHILD"
	mcpStartupHomeEnv  = "MSGVAULT_MCP_STARTUP_HOME"
)

// TestMCPInitializeWithoutStats exercises the real mcp command in a child
// process so stdio startup and the daemon request boundary stay in the test.
func TestMCPInitializeWithoutStats(t *testing.T) {
	testMCPStartupCatalog(t, api.AnalyticsModeDuckDB)
}

func TestMCPPostgresCatalogWithoutSQL(t *testing.T) {
	testMCPStartupCatalog(t, api.AnalyticsModePostgres)
}

func testMCPStartupCatalog(t *testing.T, analyticsEngine string) {
	t.Helper()
	require := require.New(t)
	assert := assert.New(t)

	releaseStats := make(chan struct{})
	var releaseStatsOnce sync.Once
	statsSeen := make(chan struct{}, 1)
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/health":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"status":"ok","api_schema_version":%q,"analytics_engine":%q}`, api.APISchemaVersion, analyticsEngine)
		case "/api/v1/stats":
			statsSeen <- struct{}{}
			<-releaseStats
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"total_messages":0,"total_threads":0,"total_accounts":0,"total_labels":0,"total_attachments":0,"database_size_bytes":0}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(func() {
		releaseStatsOnce.Do(func() { close(releaseStats) })
		daemon.Close()
	})

	home := t.TempDir()
	configText := fmt.Sprintf("[remote]\nurl = %q\nallow_insecure = true\n", daemon.URL)
	require.NoError(os.WriteFile(filepath.Join(home, "config.toml"), []byte(configText), 0o600))

	ctx, cancel := context.WithTimeout(t.Context(), serveLifecycleTestTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMCPStartupChild$") //nolint:gosec // the test binary and fixed test selector are local.
	cmd.Env = append(os.Environ(),
		mcpStartupChildEnv+"=1",
		mcpStartupHomeEnv+"="+home,
		"MSGVAULT_TEST_DB=",
	)
	stdin, err := cmd.StdinPipe()
	require.NoError(err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(err)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	require.NoError(cmd.Start())
	var waitOnce sync.Once
	var waitErr error
	waitChild := func() error {
		waitOnce.Do(func() { waitErr = cmd.Wait() })
		if waitErr != nil {
			return fmt.Errorf("wait for MCP child: %w", waitErr)
		}
		return nil
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		cancel()
		_ = waitChild()
	})

	reader := bufio.NewReader(stdout)
	waitForResponse := make(chan []byte, 1)
	go func() {
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil {
			waitForResponse <- nil
			return
		}
		waitForResponse <- line
	}()

	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"target-896","version":"test"}}}` + "\n"
	_, err = stdin.Write([]byte(initialize))
	require.NoError(err)

	var response []byte
	select {
	case <-statsSeen:
		require.FailNow("MCP initialization must not request /api/v1/stats")
	case response = <-waitForResponse:
	case <-ctx.Done():
		require.FailNow("MCP initialize did not return or request stats within the watchdog")
	}
	require.NotEmpty(response, "MCP initialize returned no response")
	var envelope map[string]any
	require.NoError(json.Unmarshal(response, &envelope))
	assert.InDelta(float64(1), envelope["id"], 0)
	require.Nil(envelope["error"])

	_, err = stdin.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n"))
	require.NoError(err)
	response, err = reader.ReadBytes('\n')
	require.NoError(err)
	var catalog struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"tools"`
		} `json:"result"`
	}
	require.NoError(json.Unmarshal(response, &catalog))
	require.NotEmpty(catalog.Result.Tools)
	var names []string
	for _, tool := range catalog.Result.Tools {
		names = append(names, tool.Name)
		if tool.Name == "semantic_search_messages" {
			assert.Contains(tool.Description, "unavailable: vector search is not configured")
		}
	}
	assert.Contains(names, "semantic_search_messages")
	assert.NotContains(names, "find_similar_messages")
	assert.NotContains(names, "search_visual_attachments")
	if analyticsEngine == api.AnalyticsModePostgres {
		assert.NotContains(names, "query_sql")
	} else {
		assert.Contains(names, "query_sql")
	}
	assert.Empty(statsSeen, "tool discovery must not request /api/v1/stats")

	_ = stdin.Close()
	waitDone := make(chan error, 1)
	go func() { waitDone <- waitChild() }()
	select {
	case waitErr = <-waitDone:
	case <-ctx.Done():
		require.FailNow("MCP child did not exit after stdin closed")
	}
	require.NoError(waitErr, stderr.String())
}

func TestMCPStartupChild(t *testing.T) {
	if os.Getenv(mcpStartupChildEnv) != "1" {
		return
	}
	runMCPStartupChild(t)
}

func runMCPStartupChild(t *testing.T) {
	t.Helper()
	home := os.Getenv(mcpStartupHomeEnv)
	require.NotEmpty(t, home)
	loaded, err := config.Load("", home)
	require.NoError(t, err)
	cfg = loaded
	remoteAPISchemaCheckEnabled = true

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mcpCmd.SetContext(ctx)
	err = mcpCmd.RunE(mcpCmd, nil)
	if err != nil && !errors.Is(err, context.Canceled) {
		assert.NoError(t, err)
	}
}
