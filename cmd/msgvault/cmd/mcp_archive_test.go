package cmd

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/testutil"
)

func archiveMCPCatalog(t *testing.T, opts mcpserver.ServeOptions) *sdkmcp.ListToolsResult {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- mcpserver.ServeTransport(ctx, opts, serverTransport) }()
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "archive-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	listed, err := session.ListTools(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, session.Close())
	cancel()
	if err := <-done; err != nil {
		require.ErrorIs(t, err, context.Canceled)
	}
	return listed
}

func TestMCPArchiveFamilies(t *testing.T) {
	for _, tc := range []struct{ source, tool string }{
		{"mbox", ""},
		{"granola", "get_meeting_context"}, {"chatwoot", "get_meeting_context"},
		{"circleback", "get_meeting_context"}, {"pocket", "get_meeting_context"},
		{"muesli", "get_meeting_context"}, {"notion_meetings", "get_meeting_context"},
		{"meeting_import", "get_meeting_context"}, {"omi", "get_meeting_context"},
		{"plaud", "get_meeting_context"}, {"twilio", "get_meeting_context"},
		{"twenty", "get_meeting_context"}, {"beeper", "find_chat"},
		{"slack", "get_slack_sync_policy"},
		{"imap", "draft_reply"},
		{"teams", "draft_compose"}, {"discord", "draft_compose"},
		{"whatsapp", "find_chat"}, {"telegram", "find_chat"}, {"matrix", "find_chat"},
		{"apple_messages", "find_chat"}, {"synctech_sms", "find_chat"}, {"imazing_csv", "find_chat"},
		{"facebook_messenger", "find_chat"},
		{"instagram_messages", "find_chat"},
		{"inline", "find_chat"}, {"google_voice", "find_chat"}, {"gmail", "find_chat"},
	} {
		t.Run(tc.source, func(t *testing.T) {
			assert := assert.New(t)
			client := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/health":
					_, _ = io.WriteString(w, `{"status":"ok","api_schema_version":"`+api.APISchemaVersion+`"}`)
				case "/api/v1/sources/status":
					_, _ = io.WriteString(w, `{"sources":[{"source_type":"`+tc.source+`"}]}`)
				case "/api/v1/carddav/connections":
					_, _ = io.WriteString(w, `{"connections":[]}`)
				case "/api/v1/settings":
					_, _ = io.WriteString(w, `{"settings":[]}`)
				default:
					http.NotFound(w, r)
				}
			})
			opts := daemonMCPServeOptions(t.Context(), client, nil)
			// Route availability is independent of source inventory.
			opts.Operations = newDaemonMCPOperations(client, nil)
			opts.OperationCapabilities = []string{"get_slack_sync_policy"}
			listed := archiveMCPCatalog(t, opts)
			var names []string
			for _, tool := range listed.Tools {
				names = append(names, tool.Name)
			}
			if tc.tool != "" {
				assert.Contains(names, tc.tool)
			}
			if tc.source == "mbox" {
				assert.Contains(names, "get_stats")
				assert.Contains(names, "list_saved_views")
				assert.Contains(names, "get_person_merge_context")
				for _, name := range names {
					assert.False(strings.HasPrefix(name, "calendar_") || strings.HasPrefix(name, "draft_") || strings.Contains(name, "_kata_") || strings.Contains(name, "carddav") || strings.Contains(name, "meeting") || strings.Contains(name, "slack") || name == "find_chat" || name == "get_person_agenda", "%s", name)
				}
			}
		})
	}
}

func TestMCPArchiveIntegrationProbes(t *testing.T) {
	kataSettings := `[{"key":"integrations.kata.enabled","value":{"boolean":true}},{"key":"integrations.kata.endpoint","value":{"string":"https://kata.example"}}]`
	for _, tc := range []struct {
		name, connections, settings, failed string
		carddav, kata                       bool
	}{
		{"named CardDAV", `[{"connection":"work"}]`, `[]`, "", true, false},
		{"orphaned CardDAV", `[{"connection":"old","orphaned":true}]`, `[]`, "", false, false},
		{"Kata enabled", `[]`, kataSettings, "", false, true},
		{"Kata empty endpoint", `[]`, `[{"key":"integrations.kata.enabled","value":{"boolean":true}},{"key":"integrations.kata.endpoint","value":{"string":""}}]`, "", false, false},
		{"source timeout", `[{"connection":"default"}]`, kataSettings, "/api/v1/sources/status", true, true},
		{"CardDAV timeout", `[{"connection":"default"}]`, kataSettings, "/api/v1/carddav/connections", false, true},
		{"settings timeout", `[{"connection":"default"}]`, kataSettings, "/api/v1/settings", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				assert, require := assert.New(t), require.New(t)
				client, err := daemonclient.New(daemonclient.Config{URL: "http://daemon.example", AllowInsecure: true, HTTPClient: &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == tc.failed {
						<-r.Context().Done()
					}
					if err := r.Context().Err(); err != nil {
						return nil, err
					}
					body := ""
					switch r.URL.Path {
					case "/api/v1/sources/status":
						body = `{"sources":[{"source_type":"gcal"}]}`
					case "/api/v1/carddav/connections":
						body = `{"connections":` + tc.connections + `}`
					case "/api/v1/settings":
						body = `{"settings":` + tc.settings + `}`
					default:
						assert.Fail("unexpected discovery path", r.URL.Path)
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
				})}})
				require.NoError(err)
				defer func() { require.NoError(client.Close()) }()
				started := time.Now()
				families := discoverMCPFamilies(t.Context(), client, slog.New(slog.DiscardHandler))
				if tc.failed != "" {
					assert.Equal(10*time.Second, time.Since(started))
				}
				listed := archiveMCPCatalog(t, mcpserver.ServeOptions{Calendar: client, PersonCardDAV: client, Kata: client, KataLookup: true, PersonAgendaBackend: client, Families: families})
				assert.Equal(tc.failed != "/api/v1/sources/status", slices.ContainsFunc(listed.Tools, func(tool *sdkmcp.Tool) bool { return tool.Name == "calendar_freebusy" }))
				assert.Equal(tc.carddav, slices.ContainsFunc(listed.Tools, func(tool *sdkmcp.Tool) bool { return tool.Name == "get_carddav_publication" }))
				assert.Equal(tc.kata, slices.ContainsFunc(listed.Tools, func(tool *sdkmcp.Tool) bool { return tool.Name == "get_person_agenda" }))
				assert.Equal(tc.kata, slices.ContainsFunc(listed.Tools, func(tool *sdkmcp.Tool) bool { return tool.Name == "find_kata_issues" }))
			})
		})
	}
}

func TestMCPArchiveRefresh(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource("mbox", "mail@example.com")
	require.NoError(err)
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "key"}}
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: st}, Engine: &querytest.MockEngine{}, Logger: slog.New(slog.DiscardHandler)})
	defer func() { require.NoError(server.Shutdown(t.Context())) }()
	daemon := httptest.NewServer(server.Router())
	defer daemon.Close()
	client, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, APIKey: "key", AllowInsecure: true})
	require.NoError(err)
	defer func() { require.NoError(client.Close()) }()
	opts := daemonMCPServeOptions(t.Context(), client, &invocation{cfg: cfg})
	cmd := &cobra.Command{Use: "serve"}
	addServeConfigFlags(cmd)
	require.NoError(installServeMCP(t.Context(), cmd, server, &invocation{cfg: cfg}))
	for _, added := range []bool{false, true} {
		if added {
			_, err = st.GetOrCreateSource("gcal", "calendar@example.com")
			require.NoError(err)
		}
		listed := archiveMCPCatalog(t, opts)
		assert.Equal(added, slices.ContainsFunc(listed.Tools, func(tool *sdkmcp.Tool) bool { return tool.Name == "calendar_freebusy" }))
		for _, protocol := range []string{"2025-11-25", "2026-07-28"} {
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"`+protocol+`","io.modelcontextprotocol/clientCapabilities":{}}}}`))
			request.Header.Set("Authorization", "Bearer key")
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept", "application/json, text/event-stream")
			request.Header.Set("Mcp-Protocol-Version", protocol)
			if protocol == "2026-07-28" {
				request.Header.Set("Mcp-Method", "tools/list")
			}
			response := httptest.NewRecorder()
			server.Router().ServeHTTP(response, request)
			require.Equal(http.StatusOK, response.Code, response.Body.String())
			assert.Equal(added, strings.Contains(response.Body.String(), `"calendar_freebusy"`))
		}
	}
}

func TestMCPArchiveDelegatedScope(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	mail, err := st.GetOrCreateSource("mbox", "mail@example.com")
	require.NoError(err)
	calendar, err := st.GetOrCreateSource("gcal", "calendar@example.com")
	require.NoError(err)
	server := mcpDraftTestDaemon(t, &storeAPIAdapter{store: st}, nil)
	for _, tc := range []struct {
		name     string
		id       int64
		calendar bool
	}{
		{"mail grant", mail.ID, false}, {"calendar grant", calendar.ID, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			session := mcpDraftTestSession(mcpDraftAgentContext(t, server, tc.id, []string{"search.read"}), t)
			names := mcpDraftToolNames(t, session)
			assert.Contains(names, "search_metadata")
			assert.Equal(tc.calendar, slices.Contains(names, "calendar_freebusy"))
		})
	}
}

func TestMCPArchiveCatalogBudgets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		kinds  []string
		budget int
	}{
		{"mail only", []string{"mbox"}, 191_000},
		{"every source", []string{"gcal", "granola", "circleback", "pocket", "muesli", "notion_meetings", "meeting_import", "omi", "plaud", "twilio", "twenty", "chatwoot", "gmail", "imap", "beeper", "slack", "slackdump", "teams", "discord", "whatsapp", "telegram", "matrix", "apple_messages", "synctech_sms", "imazing_csv", "facebook_messenger", "google_voice", "inline", "mbox"}, 249_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			routes := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{}, Logger: slog.New(slog.DiscardHandler)})
			defer func() { require.NoError(routes.Shutdown(t.Context())) }()
			client := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/health":
					_, _ = io.WriteString(w, `{"status":"ok","api_schema_version":"`+api.APISchemaVersion+`","vector":{"text_enabled":true,"visual_enabled":true}}`)
				case "/api/v1/sources/status":
					var sources []map[string]string
					for _, kind := range tc.kinds {
						sources = append(sources, map[string]string{"source_type": kind})
					}
					assert.NoError(json.NewEncoder(w).Encode(map[string]any{"sources": sources}))
				case "/api/v1/carddav/connections":
					connections := `[]`
					if tc.name == "every source" {
						connections = `[{"connection":"default"}]`
					}
					_, _ = io.WriteString(w, `{"connections":`+connections+`}`)
				case "/api/v1/settings":
					if tc.name == "mail only" {
						_, _ = io.WriteString(w, `{"settings":[]}`)
						return
					}
					_, _ = io.WriteString(w, `{"settings":[{"key":"integrations.kata.enabled","value":{"boolean":true}},{"key":"integrations.kata.endpoint","value":{"string":"https://kata.example.com"}}]}`)
				case "/api/v1/media/search/status":
					_, _ = io.WriteString(w, `{"configured":true}`)
				default:
					routes.Router().ServeHTTP(w, r)
				}
			})
			opts := daemonMCPServeOptions(t.Context(), client, nil)
			listed := archiveMCPCatalog(t, opts)
			encoded, err := json.Marshal(listed)
			require.NoError(err)
			t.Logf("%s: %d bytes, %d tools", tc.name, len(encoded), len(listed.Tools))
			if !assert.LessOrEqual(len(encoded), tc.budget) {
				for _, tool := range listed.Tools {
					data, err := json.Marshal(tool)
					require.NoError(err)
					t.Logf("%s: %d bytes", tool.Name, len(data))
				}
			}
		})
	}
}

func TestMCPArchiveLocalChatDrafts(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("slackdump", "chat-archive")
	requirements.NoError(err)
	conversation, err := st.EnsureConversationWithType(source.ID, "C1", "channel", "General")
	requirements.NoError(err)
	server := mcpDraftTestDaemon(t, &storeAPIAdapter{store: st}, nil)
	for _, delegated := range []bool{false, true} {
		t.Run(map[bool]string{false: "owner", true: "delegated"}[delegated], func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			ctx := withStoreResolverConfig(t, &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Remote: config.RemoteConfig{URL: server.URL, APIKey: "owner-test-key", AllowInsecure: true}})
			if delegated {
				ctx = mcpDraftAgentContext(t, server, source.ID, []string{"draft.create", "draft.edit"})
			}
			session := mcpDraftTestSession(ctx, t)
			names := mcpDraftToolNames(t, session)
			assert.Contains(names, "draft_compose")
			assert.Contains(names, "draft_get")
			created, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "draft_compose", Arguments: map[string]any{"conversation": conversation, "body": "local chat draft"}})
			require.NoError(err)
			require.False(created.IsError, "%v", created.Content)
			output, ok := created.StructuredContent.(map[string]any)
			require.True(ok)
			assert.Equal("slackdump", output["source_type"])
			assert.Equal("msgvault", output["location"])
			read, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "draft_get", Arguments: map[string]any{"draft_id": output["draft_id"]}})
			require.NoError(err)
			require.False(read.IsError, "%v", read.Content)
			result, ok := read.StructuredContent.(map[string]any)
			require.True(ok)
			assert.Equal("local chat draft", result["body"])
			assert.Equal(output["draft_id"], result["draft_id"])
		})
	}
}
