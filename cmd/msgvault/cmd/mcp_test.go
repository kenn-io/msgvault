package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/deletion"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
)

func TestMCPToolSummaryFlagsExist(t *testing.T) {
	for _, summary := range mcpserver.ToolSummaries() {
		if summary.Flag != "" {
			assert.NotNil(t, mcpCmd.Flags().Lookup(strings.TrimPrefix(summary.Flag, "--")), "%s", summary.Name)
		}
	}
}

func TestMCPCommandUsesDaemonInsteadOfOpeningLocalDatabase(t *testing.T) {
	requirementsForTest := require.New(t)
	assertionsForTest := assert.New(t)

	testCtx := withStoreResolverConfig(t, &config.Config{
		HomeDir: t.TempDir(),
		Data: config.DataConfig{
			DataDir: filepath.Join(t.TempDir(), "missing-parent", "data"),
		},
		Remote: config.RemoteConfig{
			URL:           "http://daemonclient.example:8080",
			AllowInsecure: true,
		},
	})

	savedHTTPAddr := mcpHTTPAddr
	savedAllowInsecure := mcpHTTPAllowInsecure
	mcpHTTPAddr = "127.0.0.1:0"
	mcpHTTPAllowInsecure = false
	t.Cleanup(func() {
		mcpHTTPAddr = savedHTTPAddr
		mcpHTTPAllowInsecure = savedAllowInsecure
	})

	ctx, cancel := context.WithCancel(testCtx)
	cancel()

	cmd := mcpCmd
	cmd.SetContext(testCtx)
	cmd.SetContext(ctx)
	err := cmd.RunE(cmd, nil)

	requirementsForTest.Error(err, "canceled MCP serve should return")
	requirementsForTest.ErrorIs(err, context.Canceled, "error should preserve context cancellation: %v", err)
	assertionsForTest.NotContains(err.Error(), "open database", "MCP command must not open SQLite directly")
}

func TestMCPCommandForwardsHTTPPolicy(t *testing.T) {
	assertionsForTest := assert.New(t)
	requirementsForTest := require.New(t)

	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "ok", "api_schema_version": api.APISchemaVersion,
			})
			return
		}
		if r.URL.Path == "/api/v1/multimodal/status" {
			http.Error(w, `{"error":"visual_search_not_ready"}`, http.StatusServiceUnavailable)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(daemon.Close)

	home := t.TempDir()
	testCtx := withStoreResolverConfig(t, &config.Config{
		HomeDir: home,
		Data:    config.DataConfig{DataDir: t.TempDir()},
		Server:  config.ServerConfig{APIKey: "mcp-http-key"},
		Remote: config.RemoteConfig{
			URL:           daemon.URL,
			APIKey:        "daemon-key",
			AllowInsecure: true,
		},
	})

	savedHTTPAddr := mcpHTTPAddr
	savedAllowInsecure := mcpHTTPAllowInsecure
	savedAllowProfileWrites := mcpAllowProfileWrites
	savedAllowIdentityDecisions := mcpAllowIdentityDecisions
	savedAllowIdentityScoring := mcpAllowIdentityScoring
	savedAllowPersonMerges := mcpAllowPersonMerges
	savedAllowCardDAVWrites := mcpAllowCardDAVWrites
	savedAllowCalendarWrites := mcpAllowCalendarWrites
	savedAllowLoops, savedAllowSources := mcpAllowLoopWrites, mcpAllowSourceWrites
	savedConfirmSensitiveDrafts := mcpConfirmSensitiveDrafts
	savedServeHTTP := serveMCPHTTPWithOptions
	allowWritesFlag := mcpCmd.Flags().Lookup("http-allow-writes")
	requirementsForTest.NotNil(allowWritesFlag, "mcp command must define --http-allow-writes")
	requirementsForTest.NoError(allowWritesFlag.Value.Set("true"))
	allowCalendarWritesFlag := mcpCmd.Flags().Lookup("allow-calendar-writes")
	requirementsForTest.NotNil(allowCalendarWritesFlag, "mcp command must define --allow-calendar-writes")
	requirementsForTest.NoError(allowCalendarWritesFlag.Value.Set("true"))
	requirementsForTest.NoError(mcpCmd.Flags().Set("confirm-sensitive-drafts", "true"))
	mcpHTTPAddr = "0.0.0.0:8081"
	mcpHTTPAllowInsecure = true
	loopWritesFlag := mcpCmd.Flags().Lookup("allow-loop-writes")
	requirementsForTest.NotNil(loopWritesFlag)
	requirementsForTest.NoError(loopWritesFlag.Value.Set("true"))
	mcpAllowSourceWrites = false
	mcpAllowProfileWrites = true
	mcpAllowIdentityDecisions = true
	mcpAllowIdentityScoring = true
	mcpAllowPersonMerges = true
	mcpAllowCardDAVWrites = true
	t.Cleanup(func() {
		assertionsForTest.NoError(allowWritesFlag.Value.Set("false"))
		assertionsForTest.NoError(allowCalendarWritesFlag.Value.Set("false"))
		mcpHTTPAddr = savedHTTPAddr
		mcpHTTPAllowInsecure = savedAllowInsecure
		mcpAllowProfileWrites = savedAllowProfileWrites
		mcpAllowIdentityDecisions = savedAllowIdentityDecisions
		mcpAllowIdentityScoring = savedAllowIdentityScoring
		mcpAllowPersonMerges = savedAllowPersonMerges
		mcpAllowCardDAVWrites = savedAllowCardDAVWrites
		mcpAllowCalendarWrites = savedAllowCalendarWrites
		mcpAllowLoopWrites, mcpAllowSourceWrites = savedAllowLoops, savedAllowSources
		mcpConfirmSensitiveDrafts = savedConfirmSensitiveDrafts
		serveMCPHTTPWithOptions = savedServeHTTP
	})

	wantErr := errors.New("stop after capture")
	var gotServeOpts mcpserver.ServeOptions
	var gotHTTPOpts mcpserver.HTTPOptions
	serveMCPHTTPWithOptions = func(_ context.Context, serveOpts mcpserver.ServeOptions, httpOpts mcpserver.HTTPOptions) error {
		gotServeOpts = serveOpts
		gotHTTPOpts = httpOpts
		return wantErr
	}

	mcpCmd.SetContext(testCtx)
	err := mcpCmd.RunE(mcpCmd, nil)

	requirementsForTest.ErrorIs(err, wantErr)
	assertionsForTest.ElementsMatch([]mcpserver.OperationFamily{mcpserver.OperationFamilyRecords, mcpserver.OperationFamilyLoops}, gotServeOpts.OperationWriteFamilies)
	assertionsForTest.True(gotServeOpts.AllowProfileWrites)
	assertionsForTest.True(gotServeOpts.AllowIdentityDecisions)
	assertionsForTest.True(gotServeOpts.AllowIdentityScoring)
	assertionsForTest.True(gotServeOpts.AllowPersonMerges)
	assertionsForTest.True(gotServeOpts.AllowCardDAVWrites)
	assertionsForTest.True(gotServeOpts.AllowCalendarWrites)
	assertionsForTest.True(gotServeOpts.ConfirmSensitiveDrafts)
	assertionsForTest.Equal(mcpserver.HTTPOptions{
		Addr:               "0.0.0.0:8081",
		DiscoveryDirectory: filepath.Join(home, "mcp"),
		BackendURL:         daemon.URL,
		APIKey:             "mcp-http-key",
		AllowWrites:        true,
	}, gotHTTPOpts)
}

func TestDaemonMCPHybridSearcherPreservesPhaseTimings(t *testing.T) {
	assertionsForTest := assert.New(t)
	client := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertionsForTest.Equal("/api/v1/search", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"query":"semantic terms",
			"mode":"hybrid",
			"returned":0,
			"pool_saturated":false,
			"accelerator":"vec1_ivf_opq",
			"has_more":false,
			"generation":{"id":7,"model":"fake","dimension":4,"fingerprint":"fake:4","state":"active"},
			"took_ms":12,
			"timings":{"query_embedding_ms":2,"retrieval_ms":7,"hydration_ms":3},
			"results":[]
		}`))
	})

	result, err := (daemonMCPHybridSearcher{client: client}).SearchHybrid(t.Context(), mcpserver.HybridSearchRequest{
		Query: "semantic terms", Mode: "hybrid", Limit: 10,
	})
	require.NoError(t, err)
	assertionsForTest.Equal(int64(12), result.TookMS)
	assertionsForTest.Equal("vec1_ivf_opq", result.Accelerator)
	assertionsForTest.Equal(mcpserver.HybridSearchTimings{
		QueryEmbeddingMS: 2,
		RetrievalMS:      7,
		HydrationMS:      3,
	}, result.Timings)
}

func TestDaemonMCPServeOptionsUsesHealthForVectorTools(t *testing.T) {
	testCtx := withStoreResolverConfig(t, &config.Config{
		Data: config.DataConfig{DataDir: t.TempDir()},
	})
	_ = testCtx
	tests := []struct {
		name                string
		health              string
		wantText            bool
		wantVisual          bool
		wantEmbeddingStatus bool
	}{
		{name: "both disabled", health: `{"status":"ok","api_schema_version":"2.28.0","vector":{"status":"disabled","text_enabled":false,"visual_enabled":false}}`},
		{name: "text only", health: `{"status":"ok","api_schema_version":"2.28.0","vector":{"status":"ready","text_enabled":true,"visual_enabled":false}}`, wantText: true},
		{name: "visual only", health: `{"status":"ok","api_schema_version":"2.28.0","vector":{"status":"ready","text_enabled":false,"visual_enabled":true}}`, wantVisual: true},
		{name: "both enabled", health: `{"status":"ok","api_schema_version":"2.28.0","vector":{"status":"ready","text_enabled":true,"visual_enabled":true}}`, wantText: true, wantVisual: true},
		{name: "initializing lanes stay registered", health: `{"status":"ok","api_schema_version":"2.28.0","vector":{"status":"initializing","text_enabled":true,"visual_enabled":true}}`, wantText: true, wantVisual: true},
		{name: "failed lanes stay registered", health: `{"status":"ok","api_schema_version":"2.28.0","vector":{"status":"error","text_enabled":true,"visual_enabled":true}}`, wantText: true, wantVisual: true},
		{name: "stale lanes stay registered", health: `{"status":"ok","api_schema_version":"2.28.0","vector":{"status":"stale","text_enabled":true,"visual_enabled":true}}`, wantText: true, wantVisual: true},
		{name: "legacy health without lane fields", health: `{"status":"ok","api_schema_version":"2.26.0","vector":{"status":"ready"}}`},
		{name: "visual route predecessor", health: `{"status":"ok","api_schema_version":"2.3.0","vector":{"status":"ready","text_enabled":true,"visual_enabled":true}}`},
		{name: "lane facts predecessor", health: `{"status":"ok","api_schema_version":"2.27.0","vector":{"status":"ready","text_enabled":true,"visual_enabled":true}}`},
		{name: "missing schema", health: `{"status":"ok","vector":{"status":"ready","text_enabled":true,"visual_enabled":true}}`},
		{name: "malformed schema", health: `{"status":"ok","api_schema_version":"unknown","vector":{"status":"ready","text_enabled":true,"visual_enabled":true}}`},
		{name: "embedding status predecessor", health: `{"status":"ok","api_schema_version":"3.20.0","vector":{"status":"ready","text_enabled":true}}`, wantText: true},
		{name: "embedding status ready", health: `{"status":"ok","api_schema_version":"3.21.0","vector":{"status":"ready","text_enabled":true}}`, wantText: true, wantEmbeddingStatus: true},
		{name: "embedding status without vector", health: `{"status":"ok","api_schema_version":"3.21.0"}`},
		{name: "health unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertionsForTest := assert.New(t)
			var healthRequests atomic.Int32
			var discoveryRequests atomic.Int32
			client := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/mcp/capabilities" {
					discoveryRequests.Add(1)
					http.NotFound(w, r)
					return
				}
				if r.URL.Path == "/api/v1/media/search/status" {
					http.NotFound(w, r)
					return
				}
				assertionsForTest.Equal("/api/v1/health", r.URL.Path, "startup must not request archive statistics")
				healthRequests.Add(1)
				if tt.health == "" {
					http.Error(w, `{"error":"temporarily_unavailable"}`, http.StatusServiceUnavailable)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.health))
			})

			opts := daemonMCPServeOptions(t.Context(), client, invocationFromContext(t.Context()))
			assertionsForTest.Equal(tt.wantText, opts.HybridSearcher != nil, "semantic search")
			assertionsForTest.Equal(tt.wantText, opts.SimilarSearcher != nil, "similar messages")
			assertionsForTest.Equal(tt.wantVisual, opts.VisualSearcher != nil, "visual search")
			assertionsForTest.Equal(tt.wantEmbeddingStatus, opts.EmbeddingStatus != nil, "embedding status")
			assertionsForTest.Equal(int32(1), healthRequests.Load(), "reuse the schema probe")
			assertionsForTest.Equal(int32(1), discoveryRequests.Load(), "probe optional operations once")
		})
	}
}

func TestDaemonMCPVectorReadinessIsCheckedAtRequestTime(t *testing.T) {
	assertionsForTest := assert.New(t)
	requirementsForTest := require.New(t)
	testCtx := withStoreResolverConfig(t, &config.Config{
		Data: config.DataConfig{DataDir: t.TempDir()},
	})

	requests := make(chan string, 8)
	client := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.Path
		if r.URL.Path == "/api/v1/media/search/status" {
			_ = json.NewEncoder(w).Encode(map[string]any{"configured": false})
			return
		}
		if r.URL.Path == "/api/v1/health" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "ok", "api_schema_version": api.APISchemaVersion,
				"vector": map[string]any{
					"status":         "initializing",
					"text_enabled":   true,
					"visual_enabled": false,
				},
			})
			return
		}
		http.Error(w, `{"error":"vector_initializing","message":"Vector search is initializing"}`, http.StatusServiceUnavailable)
	})

	opts := daemonMCPServeOptions(testCtx, client, invocationFromContext(testCtx))
	_, err := opts.HybridSearcher.SearchHybrid(testCtx, mcpserver.HybridSearchRequest{
		Query: "term",
		Mode:  "hybrid",
	})
	var coded interface{ APIErrorCode() string }
	requirementsForTest.ErrorAs(err, &coded)
	assertionsForTest.Equal("vector_initializing", coded.APIErrorCode())
	path := <-requests
	assertionsForTest.Equal("/api/v1/mcp/capabilities", path, "startup probes optional operations")
	path = <-requests
	assertionsForTest.Equal("/api/v1/health", path, "startup reads the daemon schema")
	path = <-requests
	assertionsForTest.Equal("/api/v1/media/search/status", path, "capability detection checks Docbank separately")
	path = <-requests
	assertionsForTest.Equal("/api/v1/search", path, "vector readiness belongs to the request")
}

func TestDaemonMCPServeOptionsGatesPeopleToolsByAPISchema(t *testing.T) {
	testCtx := withStoreResolverConfig(t, &config.Config{
		Data: config.DataConfig{DataDir: t.TempDir()},
	})
	_ = testCtx
	tests := []struct {
		name               string
		schemaVersion      string
		wantPeople         bool
		wantDirectory      bool
		wantChatDiscovery  bool
		wantSavedViews     bool
		wantMeetings       bool
		wantAgenda         bool
		wantArchiveSQL     bool
		wantReview         bool
		wantScoring        bool
		wantCardDAV        bool
		wantMessageTags    bool
		wantMessageArchive bool
		wantContacts       bool
	}{
		{name: "people schema", schemaVersion: "2.10.0", wantPeople: true},
		{name: "directory predecessor", schemaVersion: "2.12.9", wantPeople: true},
		{name: "directory schema", schemaVersion: "2.13.0", wantPeople: true, wantDirectory: true},
		{name: "newer schema", schemaVersion: "2.14.0", wantPeople: true, wantDirectory: true},
		{name: "schema before the Saved View run endpoint", schemaVersion: "2.20.0", wantPeople: true, wantDirectory: true},
		{name: "saved view run schema", schemaVersion: "2.21.0", wantPeople: true, wantDirectory: true, wantSavedViews: true},
		{name: "schema before the meeting endpoints", schemaVersion: "2.24.0", wantPeople: true, wantDirectory: true, wantSavedViews: true},
		{name: "meeting predecessor 2.25.x", schemaVersion: "2.25.0", wantPeople: true, wantDirectory: true, wantSavedViews: true},
		{name: "meeting predecessor 2.26.x", schemaVersion: "2.26.0", wantPeople: true, wantDirectory: true, wantSavedViews: true},
		{name: "meeting schema", schemaVersion: "2.27.0", wantPeople: true, wantDirectory: true, wantSavedViews: true, wantMeetings: true},
		{name: "person agenda predecessor", schemaVersion: "2.29.0", wantPeople: true, wantDirectory: true, wantSavedViews: true, wantMeetings: true},
		{name: "person agenda schema", schemaVersion: "2.30.0", wantPeople: true, wantDirectory: true, wantSavedViews: true, wantMeetings: true, wantAgenda: true},
		{name: "archive SQL schema", schemaVersion: "2.31.0", wantPeople: true, wantDirectory: true, wantSavedViews: true, wantMeetings: true, wantAgenda: true, wantArchiveSQL: true},
		{name: "person CardDAV schema", schemaVersion: "2.32.0", wantPeople: true, wantDirectory: true, wantSavedViews: true, wantMeetings: true, wantAgenda: true, wantArchiveSQL: true, wantCardDAV: true},
		{name: "identity review and scoring predecessor", schemaVersion: "2.35.0", wantPeople: true, wantDirectory: true, wantSavedViews: true, wantMeetings: true, wantAgenda: true, wantArchiveSQL: true, wantCardDAV: true},
		{name: "identity review and scoring schema", schemaVersion: "3.0.0", wantPeople: true, wantDirectory: true, wantSavedViews: true, wantMeetings: true, wantAgenda: true, wantArchiveSQL: true, wantCardDAV: true, wantReview: true, wantScoring: true},
		{name: "native message tags schema", schemaVersion: "3.15.0", wantPeople: true, wantDirectory: true, wantSavedViews: true, wantMeetings: true, wantAgenda: true, wantArchiveSQL: true, wantCardDAV: true, wantReview: true, wantScoring: true, wantMessageTags: true},
		{name: "archive predecessor", schemaVersion: "5.0.0", wantPeople: true, wantChatDiscovery: true, wantDirectory: true, wantSavedViews: true, wantMeetings: true, wantAgenda: true, wantArchiveSQL: true, wantCardDAV: true, wantReview: true, wantScoring: true, wantMessageTags: true, wantContacts: true},
		{name: "current schema", schemaVersion: api.APISchemaVersion, wantMessageArchive: true, wantPeople: true, wantChatDiscovery: true, wantDirectory: true, wantSavedViews: true, wantMeetings: true, wantAgenda: true, wantArchiveSQL: true, wantCardDAV: true, wantReview: true, wantScoring: true, wantMessageTags: true, wantContacts: true},
		{name: "tags predecessor 3.14.0", schemaVersion: "3.14.0", wantPeople: true, wantDirectory: true, wantSavedViews: true, wantMeetings: true, wantAgenda: true, wantArchiveSQL: true, wantCardDAV: true, wantReview: true, wantScoring: true},
		{name: "before contact schema", schemaVersion: "3.19.0", wantPeople: true, wantDirectory: true, wantSavedViews: true, wantMeetings: true, wantAgenda: true, wantArchiveSQL: true, wantCardDAV: true, wantReview: true, wantScoring: true, wantMessageTags: true},
		{name: "contact schema", schemaVersion: "3.20.0", wantPeople: true, wantDirectory: true, wantSavedViews: true, wantMeetings: true, wantAgenda: true, wantArchiveSQL: true, wantCardDAV: true, wantReview: true, wantScoring: true, wantContacts: true, wantMessageTags: true},
		{name: "chat discovery schema", schemaVersion: "3.25.0", wantPeople: true, wantContacts: true, wantDirectory: true, wantChatDiscovery: true, wantSavedViews: true, wantMeetings: true, wantAgenda: true, wantArchiveSQL: true, wantCardDAV: true, wantReview: true, wantScoring: true, wantMessageTags: true},
		{name: "older same-major schema", schemaVersion: "2.9.9"},
		{name: "malformed schema", schemaVersion: "not-a-version"},
		{name: "missing schema"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertionsForTest := assert.New(t)
			client := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/health":
					body := map[string]any{"status": "ok"}
					if tt.schemaVersion != "" {
						body["api_schema_version"] = tt.schemaVersion
					}
					_ = json.NewEncoder(w).Encode(body)
				default:
					http.NotFound(w, r)
				}
			})

			opts := daemonMCPServeOptions(t.Context(), client, invocationFromContext(t.Context()))
			if tt.wantPeople {
				assertionsForTest.NotNil(opts.PeopleBackend)
			} else {
				assertionsForTest.Nil(opts.PeopleBackend)
			}
			if tt.wantSavedViews {
				assertionsForTest.NotNil(opts.SavedViews, "Saved View tools need the daemon run endpoint")
			} else {
				assertionsForTest.Nil(opts.SavedViews, "an older daemon cannot run Saved Views")
			}
			assertionsForTest.Equal(tt.wantDirectory, opts.DirectoryBackend != nil)
			assertionsForTest.Equal(tt.wantChatDiscovery, opts.ChatDiscoveryBackend != nil)
			assertionsForTest.Equal(tt.wantMeetings, opts.Meetings != nil)
			assertionsForTest.Equal(tt.wantAgenda, opts.PersonAgendaBackend != nil)
			assertionsForTest.Equal(tt.wantContacts, opts.ContactRoutesBackend != nil)
			assertionsForTest.Equal(tt.wantArchiveSQL, opts.ArchiveSQLQuerier != nil)
			assertionsForTest.Equal(tt.wantReview, opts.IdentityReview != nil)
			assertionsForTest.Equal(tt.wantCardDAV, opts.PersonCardDAV != nil)
			assertionsForTest.Equal(tt.wantScoring, opts.IdentityScoring != nil)
			assertionsForTest.Equal(tt.wantMessageTags, opts.MessageTags != nil)
			assertionsForTest.Equal(tt.wantMessageArchive, opts.MessageArchive != nil)
		})
	}
}

func TestDaemonMCPServeOptionsWarnsWhenPeopleCapabilityProbeFails(t *testing.T) {
	logger := testLoggerValue()

	assertionsForTest := assert.New(t)
	testCtx := withStoreResolverConfig(t, &config.Config{
		Data: config.DataConfig{DataDir: t.TempDir()},
	})
	var logs bytes.Buffer
	previousLogger := logger
	logger = slog.New(slog.NewTextHandler(&logs, nil))
	inv := invocationFromContext(testCtx)
	previousInvocationLogger := inv.logger
	inv.logger = logger
	t.Cleanup(func() { logger = previousLogger })
	t.Cleanup(func() { inv.logger = previousInvocationLogger })

	client := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			http.Error(w, `{"error":"temporarily_unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		http.NotFound(w, r)
	})

	opts := daemonMCPServeOptions(testCtx, client, invocationFromContext(testCtx))
	assertionsForTest.Nil(opts.PeopleBackend)
	assertionsForTest.Nil(opts.DirectoryBackend)
	assertionsForTest.Nil(opts.ChatDiscoveryBackend)
	assertionsForTest.Nil(opts.ArchiveSQLQuerier)
	assertionsForTest.Contains(logs.String(), "people tools disabled")
}

func TestDaemonMCPServeOptionsUsesOneCapabilityProbe(t *testing.T) {
	logger := testLoggerValue()

	assertionsForTest := assert.New(t)
	testCtx := withStoreResolverConfig(t, &config.Config{
		Data: config.DataConfig{DataDir: t.TempDir()},
	})
	var logs bytes.Buffer
	previousLogger := logger
	logger = slog.New(slog.NewTextHandler(&logs, nil))
	t.Cleanup(func() { logger = previousLogger })
	var healthRequests atomic.Int32
	client := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/health":
			if healthRequests.Add(1) == 1 {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"status": "ok", "api_schema_version": "2.28.0",
					"vector": map[string]any{
						"status":         "ready",
						"text_enabled":   true,
						"visual_enabled": true,
					},
				})
				return
			}
			http.Error(w, `{"error":"temporarily_unavailable"}`, http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	})

	opts := daemonMCPServeOptions(testCtx, client, invocationFromContext(testCtx))
	assertionsForTest.NotNil(opts.PeopleBackend)
	assertionsForTest.NotNil(opts.DirectoryBackend)
	assertionsForTest.NotNil(opts.SavedViews)
	assertionsForTest.NotNil(opts.HybridSearcher)
	assertionsForTest.NotNil(opts.SimilarSearcher)
	assertionsForTest.NotNil(opts.VisualSearcher)
	assertionsForTest.Equal(int32(1), healthRequests.Load())
	assertionsForTest.Empty(logs.String())
}

func TestDaemonMCPServeOptionsSavesDeletionManifestsThroughDaemon(t *testing.T) {
	requirementsForTest := require.New(t)

	testCtx := withStoreResolverConfig(t, &config.Config{
		Data: config.DataConfig{DataDir: t.TempDir()},
	})

	var manifestRequests atomic.Int32
	client := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/health":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "ok", "api_schema_version": api.APISchemaVersion,
			})
		case "/api/v1/cli/deletion-manifests":
			manifestRequests.Add(1)
			assert.Equal(t, http.MethodPost, r.Method, "method")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"batch-1","message_count":1}`))
		default:
			http.NotFound(w, r)
		}
	})

	opts := daemonMCPServeOptions(testCtx, client, invocationFromContext(testCtx))
	requirementsForTest.NotNil(opts.ManifestSaver, "manifest saver")

	manifest := deletion.NewManifest("mcp test", []string{"gmail-001"})
	err := opts.ManifestSaver.SaveManifest(testCtx, manifest)
	requirementsForTest.NoError(err)
	assert.Equal(t, int32(1), manifestRequests.Load(), "manifest requests")
}

func newMCPDaemonClient(t *testing.T, handler http.HandlerFunc) *daemonclient.Client {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "key", r.Header.Get("X-Api-Key"), "api key")
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	client, err := daemonclient.New(daemonclient.Config{
		URL:           srv.URL,
		APIKey:        "key",
		AllowInsecure: true,
	})
	require.NoError(t, err)
	return client
}

func TestNormalizeMCPHTTPAddr(t *testing.T) {
	t.Run("bare_port_defaults_to_loopback", func(t *testing.T) {
		got, err := normalizeMCPHTTPAddr("8080", false, false)
		require.NoError(t, err)
		require.Equal(t, "127.0.0.1:8080", got)
	})

	t.Run("colon_port_defaults_to_loopback", func(t *testing.T) {
		got, err := normalizeMCPHTTPAddr(":8080", false, false)
		require.NoError(t, err)
		require.Equal(t, "127.0.0.1:8080", got)
	})

	t.Run("explicit_loopback_passes", func(t *testing.T) {
		cases := []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080"}
		for _, c := range cases {
			got, err := normalizeMCPHTTPAddr(c, false, false)
			require.NoError(t, err, "%s", c)
			assert.Equal(t, c, got, "%s: should be unchanged", c)
		}
	})

	t.Run("non_loopback_rejected_without_optin", func(t *testing.T) {
		cases := []string{
			"0.0.0.0:8080",
			"192.168.1.5:8080",
			"vault.local:8080",
			// Regression: empty-bracket host parses cleanly via
			// net.SplitHostPort but binds to all interfaces. Must
			// be rejected, not silently treated as loopback.
			"[]:8080",
		}
		for _, c := range cases {
			_, err := normalizeMCPHTTPAddr(c, false, false)
			require.Error(t, err, "%s: expected refusal", c)
			assert.ErrorContains(t, err, "--http-allow-insecure", "%s: expected hint", c)
		}
	})

	t.Run("non_loopback_allowed_with_optin", func(t *testing.T) {
		got, err := normalizeMCPHTTPAddr("0.0.0.0:8080", true, false)
		require.NoError(t, err)
		require.Equal(t, "0.0.0.0:8080", got)
	})

	t.Run("non_loopback_allowed_with_api_key", func(t *testing.T) {
		got, err := normalizeMCPHTTPAddr("0.0.0.0:8080", false, true)
		require.NoError(t, err)
		require.Equal(t, "0.0.0.0:8080", got)
	})

	t.Run("empty_rejected", func(t *testing.T) {
		_, err := normalizeMCPHTTPAddr("", false, false)
		require.Error(t, err, "expected error for empty addr")
	})

	t.Run("garbage_rejected", func(t *testing.T) {
		_, err := normalizeMCPHTTPAddr("not-a-port", false, false)
		require.Error(t, err, "expected error for non-port, non-host:port")
	})
}

func TestDaemonMCPDocbankCapability(t *testing.T) {
	for _, tc := range []struct {
		version          string
		configured, want bool
	}{
		{"3.12.0", true, false}, {"3.13.0", false, false}, {"3.13.0", true, true},
	} {
		t.Run(tc.version+"/"+strconv.FormatBool(tc.configured), func(t *testing.T) {
			assertionsForTest := assert.New(t)
			ctx := withStoreResolverConfig(t, &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}})
			var probes atomic.Int32
			client := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/health":
					assertionsForTest.NoError(json.NewEncoder(w).Encode(map[string]any{"status": "ok", "api_schema_version": tc.version}))
				case "/api/v1/media/search/status":
					probes.Add(1)
					assertionsForTest.NoError(json.NewEncoder(w).Encode(map[string]any{"configured": tc.configured}))
				default:
					http.NotFound(w, r)
				}
			})
			opts := daemonMCPServeOptions(ctx, client, invocationFromContext(ctx))
			assertionsForTest.True(opts.KataLookup)
			assertionsForTest.Equal(tc.want, opts.KataDocbank)
			if tc.version == "3.12.0" {
				assertionsForTest.Zero(probes.Load())
			} else {
				assertionsForTest.Equal(int32(1), probes.Load())
			}
		})
	}
}

func TestDaemonMCPDocbankProbeFailureIsLogged(t *testing.T) {
	assertionsForTest := assert.New(t)
	ctx := withStoreResolverConfig(t, &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}})
	client := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/health":
			assertionsForTest.NoError(json.NewEncoder(w).Encode(map[string]any{"status": "ok", "api_schema_version": "3.13.0"}))
		case "/api/v1/media/search/status":
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	})
	var logs bytes.Buffer
	state := *invocationFromContext(ctx)
	state.logger = slog.New(slog.NewTextHandler(&logs, nil))
	opts := daemonMCPServeOptions(ctx, client, &state)
	assertionsForTest.False(opts.KataDocbank)
	assertionsForTest.Contains(logs.String(), "Kata transcript citations hidden because the Docbank status probe failed")
	assertionsForTest.Contains(logs.String(), "503")
}

func TestDelegatedMCPArchiveCapability(t *testing.T) {
	for _, version := range []string{"5.0.0", "5.1.0"} {
		t.Run(version, func(t *testing.T) {
			assertionsForTest := assert.New(t)
			requirementsForTest := require.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/health":
					assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"status": "ok", "api_schema_version": version}))
				case "/api/v1/agent-tokens/self":
					assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"id": "fixture", "label": "agent", "permissions": []string{"message.archive"}, "sources": []any{map[string]any{"id": 1, "type": "gmail", "identifier": "owner@example.test"}}, "created_at": time.Now().UTC()}))
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AgentToken: "fixture", AllowInsecure: true})
			requirementsForTest.NoError(err)
			t.Cleanup(func() { _ = client.Close() })
			opts, err := delegatedMCPServeOptions(t.Context(), client)
			requirementsForTest.NoError(err)
			if version == "5.0.0" {
				assertionsForTest.Nil(opts.MessageArchive)
			} else {
				assertionsForTest.NotNil(opts.MessageArchive)
			}
		})
	}
}
