package cmd

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/pkg/client/generated"
	"go.opentelemetry.io/otel/trace"
)

func TestNativeMCPBridge(t *testing.T) {
	t.Run("generated metadata", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		transport := nativeMCPTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal("127.0.0.1:0", r.RemoteAddr)
			assert.Equal("msgvault.internal", r.Host)
			assert.Equal("owner", r.Header.Get("X-Api-Key"))
			assert.Empty(r.Header.Get("Authorization"))
			assert.Equal(apiprotocol.ClientClassCLI, r.Header.Get(apiprotocol.ClientClassHeader))
			assert.NotEmpty(r.Header.Get("Traceparent"))
			w.Header().Set("Content-Type", "application/json")
			switch r.Method + " " + r.URL.Path {
			case "GET /api/v1/cli/accounts":
				assert.Equal("true", r.Header.Get(apiprotocol.AllowPendingCountsHeader))
				_, err := io.WriteString(w, `{"accounts":[],"counts_pending":true}`)
				assert.NoError(err)
			case "DELETE /api/v1/saved-views/42":
				assert.NotEmpty(r.Header.Get("If-Match"))
				w.WriteHeader(http.StatusNoContent)
			default:
				assert.Contains(r.URL.EscapedPath(), "project%23abcd")
				_, err := io.WriteString(w, `{}`)
				assert.NoError(err)
			}
		})}
		ctx := trace.ContextWithSpanContext(t.Context(), trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, TraceFlags: trace.FlagsSampled}))
		client, err := newDaemonCLIClient(ctx, daemonclient.Config{URL: nativeMCPOrigin, APIKey: "owner", AllowInsecure: true, HTTPClient: &http.Client{Transport: transport}})
		require.NoError(err)
		defer func() { require.NoError(client.Close()) }()
		_, pending, err := client.GetCLIAccounts(ctx)
		require.NoError(err)
		assert.True(pending)
		require.NoError(client.DeleteSavedView(ctx, 42, 1))
		_, err = client.GetKataIssueContext(ctx, "project#abcd", generated.GetKataIssueContextQuery{})
		require.NoError(err)
	})
	for _, cancelFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "body close joins router", true: "canceled body joins router"}[cancelFirst], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				canceled, exited := make(chan struct{}), make(chan struct{})
				unwind, release := context.WithCancel(context.Background())
				defer release()
				transport := nativeMCPTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					defer close(exited)
					w.WriteHeader(http.StatusOK)
					<-r.Context().Done()
					close(canceled)
					<-unwind.Done()
				})}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				client, err := newDaemonCLIClient(ctx, daemonclient.Config{URL: nativeMCPOrigin, APIKey: "owner", AllowInsecure: true, HTTPClient: &http.Client{Transport: transport}})
				require.NoError(err)
				defer func() { require.NoError(client.Close()) }()
				response, err := client.DoGeneratedStreamingRequestWithContext(ctx, http.MethodGet, "/api/v1/stream", nil)
				require.NoError(err)
				defer func() { release(); require.NoError(response.Body.Close()) }()
				if cancelFirst {
					cancel()
					<-canceled
					_, err = io.ReadAll(response.Body)
					require.Error(err)
				}
				closed := make(chan error, 1)
				go func() { closed <- response.Body.Close() }()
				<-canceled
				synctest.Wait()
				select {
				case err := <-closed:
					require.NoError(err)
					assert.Fail("body close returned before nested handler exited")
					release()
					return
				default:
				}
				release()
				require.NoError(<-closed)
				select {
				case <-exited:
				default:
					assert.Fail("body close did not join the handler")
				}
			})
		})
	}
	t.Run("canceled read drains before shutdown", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		entered, canceled, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
		unwind, release := context.WithCancel(context.Background())
		engine := &querytest.MockEngine{ListMessagesFunc: func(ctx context.Context, _ query.MessageFilter) ([]query.MessageSummary, error) {
			defer close(exited)
			close(entered)
			<-ctx.Done()
			close(canceled)
			<-unwind.Done()
			return nil, ctx.Err()
		}}
		cfg := &config.Config{Server: config.ServerConfig{APIKey: "owner", BindAddr: "127.0.0.1"}}
		server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler), Engine: engine})
		defer func() { release(); require.NoError(server.Shutdown(context.Background())) }()
		cmd := &cobra.Command{Use: "serve"}
		addServeConfigFlags(cmd)
		require.NoError(installServeMCP(t.Context(), cmd, server, &invocation{cfg: cfg}))
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(err)
		serving := make(chan error, 1)
		go func() { serving <- server.StartOnListener(listener) }()
		require.NoError(server.WaitStarted(t.Context()))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+listener.Addr().String()+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_messages","arguments":{}}}`))
		require.NoError(err)
		request.Header.Set("Authorization", "Bearer owner")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		request.Header.Set("Mcp-Protocol-Version", "2025-11-25")
		requested := make(chan error, 1)
		go func() {
			response, requestErr := http.DefaultClient.Do(request)
			if response != nil {
				assert.NoError(response.Body.Close())
			}
			requested <- requestErr
		}()
		<-entered
		cancel()
		<-canceled
		require.ErrorIs(<-requested, context.Canceled)
		shutdown := make(chan error, 1)
		go func() { shutdown <- server.Shutdown(context.Background()) }()
		require.ErrorIs(<-serving, http.ErrServerClosed)
		select {
		case err := <-shutdown:
			require.NoError(err)
			assert.Fail("shutdown returned while nested read was unwinding")
			release()
			return
		case <-time.After(time.Second):
		}
		release()
		require.NoError(<-shutdown)
		select {
		case <-exited:
		default:
			assert.Fail("shutdown did not join the nested handler")
		}
	})
	t.Run("serve cancellation preserves admitted read during drain", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		entered := make(chan struct{})
		unwind, release := context.WithCancel(context.Background())
		engine := &querytest.MockEngine{ListMessagesFunc: func(ctx context.Context, _ query.MessageFilter) ([]query.MessageSummary, error) {
			close(entered)
			<-unwind.Done()
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return []query.MessageSummary{{ID: 1, Subject: "completed during drain"}}, nil
		}}
		cfg := &config.Config{Server: config.ServerConfig{APIKey: "owner"}}
		server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler), Engine: engine})
		defer func() { release(); require.NoError(server.Shutdown(context.Background())) }()
		cmd := &cobra.Command{Use: "serve"}
		addServeConfigFlags(cmd)
		serveCtx, cancel := context.WithCancel(t.Context())
		defer cancel()
		require.NoError(installServeMCP(serveCtx, cmd, server, &invocation{cfg: cfg}))
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(err)
		serving := make(chan error, 1)
		go func() { serving <- server.StartOnListener(listener) }()
		require.NoError(server.WaitStarted(t.Context()))
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+listener.Addr().String()+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_messages","arguments":{}}}`))
		require.NoError(err)
		request.Header.Set("Authorization", "Bearer owner")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		request.Header.Set("Mcp-Protocol-Version", "2025-11-25")
		result := make(chan string, 1)
		go func() {
			response, err := http.DefaultClient.Do(request)
			if !assert.NoError(err) {
				result <- "request failed"
				return
			}
			defer func() { assert.NoError(response.Body.Close()) }()
			body, err := io.ReadAll(response.Body)
			assert.NoError(err)
			result <- string(body)
		}()
		<-entered
		cancel()
		drained := make(chan error, 1)
		go func() { drained <- server.Shutdown(context.Background()) }()
		require.ErrorIs(<-serving, http.ErrServerClosed)
		release()
		response := <-result
		require.NoError(<-drained)
		assert.Contains(response, "completed during drain")
	})
	t.Run("CLI read budget", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			cfg := &config.Config{Server: config.ServerConfig{APIKey: "owner"}}
			engine := &querytest.MockEngine{ListMessagesFunc: func(ctx context.Context, _ query.MessageFilter) ([]query.MessageSummary, error) {
				timer := time.NewTimer(50 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-timer.C:
					return []query.MessageSummary{{ID: 1, Subject: "synthetic slow read"}}, nil
				}
			}}
			server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler), Engine: engine, RequestTimeout: 20 * time.Millisecond})
			defer func() { require.NoError(server.Shutdown(context.Background())) }()
			cmd := &cobra.Command{Use: "serve"}
			addServeConfigFlags(cmd)
			require.NoError(installServeMCP(t.Context(), cmd, server, &invocation{cfg: cfg}))
			request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_messages","arguments":{}}}`))
			request.Header.Set("Authorization", "Bearer owner")
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept", "application/json, text/event-stream")
			request.Header.Set("Mcp-Protocol-Version", "2025-11-25")
			response := httptest.NewRecorder()
			server.Router().ServeHTTP(response, request)
			require.Equal(http.StatusOK, response.Code, response.Body.String())
			assert.Contains(response.Body.String(), "synthetic slow read")
			assert.NotContains(response.Body.String(), `"error":`)
		})
	})
}

func TestInstallServeMCPCatalogs(t *testing.T) {
	for _, mode := range []string{"read", "writes", "families"} {
		t.Run(mode, func(t *testing.T) {
			checks := assert.New(t)
			must := require.New(t)
			cfg := &config.Config{Server: config.ServerConfig{APIKey: "owner", BindAddr: "0.0.0.0", TrustedProxies: []string{"127.0.0.1/32"}}}
			if mode == "families" {
				cfg.Server.TrustedProxies = nil
			}
			server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler), Engine: &querytest.MockEngine{Messages: map[int64]*query.MessageDetail{42: {ID: 42, Subject: "synthetic message"}}}})
			defer func() { must.NoError(server.Shutdown(context.Background())) }()
			daemon := httptest.NewServer(server.Router())
			defer daemon.Close()
			must.Zero(cfg.Server.APIPort)
			cmd := &cobra.Command{Use: "serve"}
			addServeConfigFlags(cmd)
			if mode != "read" {
				must.NoError(cmd.Flags().Set("mcp-allow-writes", "true"))
			}
			families := map[string]string{"profile-writes": "promote_person", "identity-decisions": "accept_identity_match", "identity-scoring": "score_identity_matches", "person-merges": "merge_person", "carddav-writes": "approve_carddav_publication", "calendar-writes": "calendar_create", "kata-writes": "create_kata_issue", "source-writes": "sync_source"}
			if mode != "writes" {
				for flag := range families {
					must.NoError(cmd.Flags().Set("mcp-allow-"+flag, "true"))
				}
			}
			must.NoError(installServeMCP(t.Context(), cmd, server, &invocation{cfg: cfg}))
			for _, path := range []string{"/mcp", "/mcp/writes"} {
				request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`))
				request.Header.Set("Authorization", "Bearer owner")
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Accept", "application/json, text/event-stream")
				request.Header.Set("Mcp-Protocol-Version", "2026-07-28")
				request.Header.Set("Mcp-Method", "tools/list")
				response := httptest.NewRecorder()
				server.Router().ServeHTTP(response, request)
				if path == "/mcp/writes" && mode == "read" {
					checks.Equal(http.StatusNotFound, response.Code)
					continue
				}
				must.Equal(http.StatusOK, response.Code, response.Body.String())
				checks.Contains(response.Body.String(), `"get_stats"`)
				if path == "/mcp" {
					checks.NotContains(response.Body.String(), `"stage_deletion"`)
				} else {
					checks.Contains(response.Body.String(), `"stage_deletion"`)
				}
				for _, tool := range families {
					if path == "/mcp/writes" && mode == "families" {
						checks.Contains(response.Body.String(), `"`+tool+`"`)
					} else {
						checks.NotContains(response.Body.String(), `"`+tool+`"`)
					}
				}
			}
			for _, origin := range []struct {
				name, host, forwarded, want, origin, key string
				status                                   int
			}{
				{name: "local listener", want: daemon.URL, key: "owner", status: http.StatusOK},
				{name: "remote wildcard", host: "archive.example:8080", want: "http://archive.example:8080", key: "owner", status: http.StatusOK},
				{name: "HTTPS proxy", host: "proxy.internal", forwarded: `proto=https;host=archive.example`, origin: "https://archive.example", want: "https://archive.example", key: "owner", status: http.StatusOK},
				{name: "bad origin", forwarded: `proto=https;host=archive.example`, origin: "https://unrelated.example", key: "owner", status: http.StatusForbidden},
				{name: "missing owner", status: http.StatusUnauthorized},
			} {
				t.Run(origin.name, func(t *testing.T) {
					assert := assert.New(t)
					require := require.New(t)
					path := "/mcp"
					if mode != "read" {
						path += "/writes"
					}
					request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, daemon.URL+path, strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_message","arguments":{"id":42}}}`))
					require.NoError(err)
					request.Host = origin.host
					if origin.key != "" {
						request.Header.Set("Authorization", "Bearer "+origin.key)
					}
					if origin.forwarded != "" {
						request.Header.Set("Forwarded", origin.forwarded)
					}
					request.Header.Set("Origin", origin.origin)
					request.Header.Set("Content-Type", "application/json")
					request.Header.Set("Accept", "application/json, text/event-stream")
					request.Header.Set("Mcp-Protocol-Version", "2025-11-25")
					response, err := daemon.Client().Do(request)
					require.NoError(err)
					defer func() { require.NoError(response.Body.Close()) }()
					body, err := io.ReadAll(response.Body)
					require.NoError(err)
					status := origin.status
					if mode == "families" && origin.forwarded != "" {
						status = http.StatusForbidden
					}
					require.Equal(status, response.StatusCode, string(body))
					if status != http.StatusOK {
						return
					}
					var rpc struct {
						Error  json.RawMessage `json:"error"`
						Result struct {
							Structured struct {
								WebURL string `json:"web_url"`
							} `json:"structuredContent"`
						} `json:"result"`
					}
					require.NoError(json.Unmarshal(body, &rpc))
					require.Empty(rpc.Error, string(body))
					assert.Equal(origin.want+"/messages/42", rpc.Result.Structured.WebURL)
				})
			}
		})
	}
}

func TestNativeMCPStartup(t *testing.T) {
	catalog := func(server *api.Server) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
		request.Header.Set("Authorization", "Bearer owner")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		request.Header.Set("Mcp-Protocol-Version", "2025-11-25")
		response := httptest.NewRecorder()
		server.Router().ServeHTTP(response, request)
		return response
	}
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "vectors initializing", true: "pre-cancelled"}[cancelled], func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			cfg := &config.Config{Server: config.ServerConfig{APIKey: "owner"}}
			cfg.Vector.Enabled = true
			cfg.Vector.Multimodal.Enabled = true
			server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler), VectorCfg: cfg.Vector, VectorStatus: api.VectorStatusInitializing})
			defer func() { require.NoError(server.Shutdown(context.Background())) }()
			cmd := &cobra.Command{Use: "serve"}
			addServeConfigFlags(cmd)
			assert.Equal(http.StatusServiceUnavailable, catalog(server).Code)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if cancelled {
				cancel()
			}
			done := startServeMCP(ctx, cmd, server, &invocation{cfg: cfg})
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				require.FailNow("MCP initialization did not stop")
			}
			response := catalog(server)
			if cancelled {
				assert.Equal(http.StatusServiceUnavailable, response.Code)
				return
			}
			require.Equal(http.StatusOK, response.Code, response.Body.String())
			assert.Contains(response.Body.String(), `"find_similar_messages"`)
			assert.Contains(response.Body.String(), `"search_visual_attachments"`)
			status, _ := server.VectorStatus()
			assert.Equal(api.VectorStatusInitializing, status)
		})
	}
	t.Run("discovery deadline", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			cfg := &config.Config{Server: config.ServerConfig{APIKey: "owner"}}
			cfg.Vector.Enabled = true
			cfg.Vector.Multimodal.Enabled = true
			logger := slog.New(slog.DiscardHandler)
			server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Logger: logger, VectorCfg: cfg.Vector, VectorStatus: api.VectorStatusInitializing})
			defer func() { require.NoError(t, server.Shutdown(context.Background())) }()
			server.SetVectorScopeCheck(func(ctx context.Context) (string, error) {
				<-ctx.Done()
				return "", ctx.Err()
			})
			cmd := &cobra.Command{Use: "serve"}
			addServeConfigFlags(cmd)
			done := startServeMCP(t.Context(), cmd, server, &invocation{cfg: cfg, logger: logger})
			<-done
			response := catalog(server)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), `"get_stats"`)
			assert.NotContains(t, response.Body.String(), `"find_similar_messages"`)

			request := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
			request.Header.Set("Authorization", "Bearer owner")
			response = httptest.NewRecorder()
			server.Router().ServeHTTP(response, request)
			assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
		})
	})
}
