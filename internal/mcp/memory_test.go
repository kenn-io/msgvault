package mcp

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query/querytest"
)

// Run in a fresh process: other MCP tests populate the process-wide catalogs
// and SDK schema cache. The budget deliberately leaves ample headroom for
// platforms and future tools, while catching the old eager 256-catalog build.
func TestMCPMemory(t *testing.T) {
	for _, phase := range []string{"startup", "initialized"} {
		t.Run(phase, func(t *testing.T) {
			require := require.New(t)
			executable, err := os.Executable()
			require.NoError(err)
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			child := exec.CommandContext(ctx, executable, "-test.run=^TestMCPMemoryChild$", "-test.v")
			child.Env = append(os.Environ(), "MSGVAULT_MCP_MEMORY_PHASE="+phase)
			output, err := child.CombinedOutput()
			require.NoError(err, "%s", output)
			t.Logf("%s", output)
		})
	}
}

func TestMCPMemoryChild(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	phase := os.Getenv("MSGVAULT_MCP_MEMORY_PHASE")
	if phase == "" {
		t.Skip("subprocess measurement only")
	}
	require.Contains([]string{"startup", "initialized"}, phase)
	if phase == "initialized" {
		peer := newTask5RawStdioPeer(t, ServeOptions{Engine: &querytest.MockEngine{}})
		initialized := peer.call(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"memory-test","version":"1"}}}`)
		require.Nil(initialized.Error)
		peer.writeLiteralLine(t, `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`)
		listed := peer.call(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
		require.Nil(listed.Error)
		require.Contains(task3ToolNames(t, listed), ToolGetStats)
		// The transport goroutine and cleanup keep the server alive through GC.
	}
	runtime.GC()
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	t.Logf("phase=%s total_alloc_bytes=%d heap_alloc_bytes=%d heap_sys_bytes=%d", phase, memory.TotalAlloc, memory.HeapAlloc, memory.HeapSys)
	assert.Less(memory.HeapAlloc, uint64(64<<20), "retained Go heap; not RSS or macOS physical footprint")
}

func BenchmarkOperationCatalogMemory(b *testing.B) {
	b.Run("cold", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var cache operationCatalogCache
			definitions := cache.get(catalogCapabilities{})
			runtime.KeepAlive(definitions)
		}
	})
	b.Run("cached", func(b *testing.B) {
		var cache operationCatalogCache
		cache.get(catalogCapabilities{})
		b.ReportAllocs()
		for b.Loop() {
			definitions := cache.get(catalogCapabilities{})
			runtime.KeepAlive(definitions)
		}
	})
	b.Run("server", func(b *testing.B) {
		opts := ServeOptions{Engine: &querytest.MockEngine{}}
		// Explicitly measure warmed schema reuse, as in the stateless HTTP listener.
		newMCPServer(opts, false)
		b.ReportAllocs()
		for b.Loop() {
			server := newMCPServer(opts, false)
			runtime.KeepAlive(server)
		}
	})
}
