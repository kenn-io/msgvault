package mcp

import (
	"context"
	"maps"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/savedview"
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
		opts := daemonProxyMemoryOptions()
		requireDaemonProxyCapabilities(t, capabilitiesFor(opts))
		peer := newTask5RawStdioPeer(t, opts)
		initialized := peer.call(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"memory-test","version":"1"}}}`)
		require.Nil(initialized.Error)
		peer.writeLiteralLine(t, `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`)
		listed := peer.call(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
		require.Nil(listed.Error)
		names := task3ToolNames(t, listed)
		require.Contains(names, ToolDraftReply)
		require.Contains(names, ToolGetIdentityScoringStatus)
		t.Logf("tools=%d", len(names))
		// The transport goroutine and cleanup keep the server alive through GC.
	}
	runtime.GC()
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	t.Logf("phase=%s total_alloc_bytes=%d heap_alloc_bytes=%d heap_sys_bytes=%d", phase, memory.TotalAlloc, memory.HeapAlloc, memory.HeapSys)
	assert.Less(memory.HeapAlloc, uint64(64<<20), "retained Go heap; not RSS or macOS physical footprint")
}

// daemonProxyMemoryOptions enables the backends a stdio proxy sets when its
// daemon serves every capability. Building and listing the catalog never calls
// a backend, so empty implementations suffice.
func daemonProxyMemoryOptions() ServeOptions {
	return ServeOptions{
		Engine:              &querytest.MockEngine{},
		AttachmentReader:    struct{ AttachmentReader }{},
		ManifestSaver:       struct{ DeletionManifestSaver }{},
		HybridSearcher:      struct{ HybridSearcher }{},
		SimilarSearcher:     struct{ SimilarSearcher }{},
		DocumentSearcher:    struct{ DocumentSearcher }{},
		MediaSearcher:       struct{ MediaSearcher }{},
		PersonFileSearcher:  struct{ PersonFileSearcher }{},
		PeopleBackend:       struct{ peoplebrowser.Backend }{},
		DirectoryBackend:    struct{ peoplebrowser.DirectoryLister }{},
		PersonAgendaBackend: struct{ PersonAgendaBackend }{},
		Kata:                struct{ KataBackend }{},
		KataLookup:          true,
		KataDocbank:         true,
		VisualSearcher:      struct{ VisualSearcher }{},
		SavedViews:          struct{ savedview.Service }{},
		Meetings:            struct{ MeetingBackend }{},
		Calendar:            struct{ CalendarBackend }{},
		ArchiveSQLQuerier:   struct{ ArchiveSQLQuerier }{},
		IdentityReview:      struct{ IdentityReviewBackend }{},
		PersonCardDAV:       struct{ PersonCardDAVBackend }{},
		IdentityScoring:     struct{ IdentityScoringBackend }{},
		Drafts:              struct{ DraftRunner }{},
		MessageTags:         struct{ MessageTagBackend }{},
		DraftCommands:       slices.Sorted(maps.Keys(stableDraftDefinitions)),
	}
}

// requireDaemonProxyCapabilities fails when a new catalog capability is not
// enabled by daemonProxyMemoryOptions, so the guard measures every served tool.
func requireDaemonProxyCapabilities(t *testing.T, capabilities catalogCapabilities) {
	t.Helper()
	value := reflect.ValueOf(capabilities)
	for i := range value.NumField() {
		name := value.Type().Field(i).Name
		if name == "vectorInMessage" {
			continue // Requires a local vector backend; a daemon proxy never has one.
		}
		require.True(t, value.Field(i).Bool(), "daemonProxyMemoryOptions must enable %s", name)
	}
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
		opts := daemonProxyMemoryOptions()
		// Explicitly measure warmed schema reuse, as in the stateless HTTP listener.
		newMCPServer(opts, false)
		b.ReportAllocs()
		for b.Loop() {
			server := newMCPServer(opts, false)
			runtime.KeepAlive(server)
		}
	})
}
