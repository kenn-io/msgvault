package daemonclient

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Invalid stream shapes cannot be produced by successful production handlers.
// These fixtures exercise the client's external NDJSON contract and interrupted
// transport behavior, rather than replacing an owning command's implementation.
func TestMCPCLIRejectsOversizedAndPostTerminalStreams(t *testing.T) {
	tests := []struct {
		name, wire, code string
	}{
		{"missing terminal", `{"type":"stdout","data":"{}"}`, "invalid_cli_stream"},
		{"unknown event", `{"type":"progress"}` + "\n" + `{"type":"complete"}`, "invalid_cli_stream"},
		{"duplicate terminal", `{"type":"complete"}` + "\n" + `{"type":"complete"}`, "invalid_cli_stream"},
		{"output after terminal", `{"type":"complete"}` + "\n" + `{"type":"stdout","data":"{}"}`, "invalid_cli_stream"},
		{"error after terminal", `{"type":"complete"}` + "\n" + `{"type":"error","error":"invalid_args"}`, "invalid_cli_stream"},
		{"malformed event", `{"type":"stdout","data":`, "invalid_cli_stream"},
		{"wrong data type", `{"type":"stdout","data":42}` + "\n" + `{"type":"complete"}`, "invalid_cli_stream"},
		{"escaped oversized event", `{"type":"stdout","data":"` + strings.Repeat(`\u0041`, 180000) + `"}` + "\n" + `{"type":"complete"}`, "output_limit_exceeded"},
		{"oversized whitespace", strings.Repeat(" ", 1<<20) + `{"type":"complete"}`, "output_limit_exceeded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tt.wire)
			}))
			t.Cleanup(server.Close)
			client, err := New(Config{URL: server.URL, AllowInsecure: true})
			requirements.NoError(err)
			result, err := client.RunMCPCLICommand(context.Background(), []string{"draft-get", "synthetic-draft", "--json"}, "")
			requirements.Error(err)
			requirements.NotNil(result)
			assertions.Equal(tt.code, result.ErrorCode)
			assertions.True(result.OperationMayHaveCompleted)
		})
	}
}

func TestMCPCLIPreservesStructuredFailure(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	wire := `{"type":"stdout","data":"{\"draft_id\":\"synthetic-draft\",\"content\":null}\n"}` + "\n" +
		`{"type":"stderr","data":"{\"code\":\"accepted_local_failed\",\"revision\":2}\n"}` + "\n" +
		`{"type":"error","error":"accepted_local_failed"}` + "\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, wire)
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{URL: server.URL, AllowInsecure: true})
	requirements.NoError(err)
	result, err := client.RunMCPCLICommand(context.Background(), []string{"draft-edit", "synthetic-draft", "--json"}, "")
	requirements.Error(err)
	requirements.NotNil(result)
	assertions.JSONEq(`{"draft_id":"synthetic-draft","content":null}`, result.Stdout)
	assertions.JSONEq(`{"code":"accepted_local_failed","revision":2}`, result.Stderr)
	assertions.Equal("accepted_local_failed", result.ErrorCode)
	assertions.True(result.Failed)
	assertions.True(result.OperationMayHaveCompleted)
}

func TestMCPCLIUsesSameCredentialAndFixedRequest(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	type observedRequest struct {
		method, path, token string
		body                map[string]any
	}
	observed := make(chan observedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		_ = json.Unmarshal(body, &decoded)
		observed <- observedRequest{r.Method, r.URL.Path, r.Header.Get("X-Msgvault-Agent-Token"), decoded}
		_, _ = io.WriteString(w, `{"type":"stdout","data":"{}"}`+"\n"+`{"type":"complete"}`+"\n")
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{URL: server.URL, AgentToken: "synthetic-token", AllowInsecure: true})
	requirements.NoError(err)
	result, err := client.RunMCPCLICommand(context.Background(), []string{"draft-get", "synthetic-draft", "--json"}, "")
	requirements.NoError(err)
	assertions.Equal("{}", result.Stdout)
	assertions.False(result.Failed)
	got := <-observed
	assertions.Equal(http.MethodPost, got.method)
	assertions.Equal("/api/v1/cli/run", got.path)
	assertions.Equal("synthetic-token", got.token)
	assertions.Equal([]any{"draft-get", "synthetic-draft", "--json"}, got.body["args"])
	assertions.NotContains(got.body, "env")
	assertions.NotContains(got.body, "cwd")
}

func TestMCPCLIDoesNotRetryBusyOrUnknownFailure(t *testing.T) {
	t.Run("busy", func(t *testing.T) {
		requirements := require.New(t)
		assertions := assert.New(t)
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":"operation_in_progress","message":"synthetic busy"}`)
		}))
		t.Cleanup(server.Close)
		client, err := New(Config{URL: server.URL, AllowInsecure: true})
		requirements.NoError(err)
		_, err = client.RunMCPCLICommand(context.Background(), []string{"draft-compose", "--json"}, "")
		requirements.Error(err)
		assertions.Equal(int32(1), requests.Load())
	})
	t.Run("private terminal error", func(t *testing.T) {
		requirements := require.New(t)
		assertions := assert.New(t)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"type":"error","error":"provider secret at /synthetic/private/path"}`)
		}))
		t.Cleanup(server.Close)
		client, err := New(Config{URL: server.URL, AllowInsecure: true})
		requirements.NoError(err)
		result, err := client.RunMCPCLICommand(context.Background(), []string{"draft-compose", "--json"}, "")
		requirements.Error(err)
		assertions.Equal("cli_execution_failed", result.ErrorCode)
		assertions.NotContains(err.Error(), "/synthetic/private/path")
	})
}

func TestMCPCLICancellationStopsStream(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	started := make(chan struct{})
	released := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"type":"stdout","data":"{}"}`+"\n")
		flusher, ok := w.(http.Flusher)
		if !assertions.True(ok) {
			return
		}
		flusher.Flush()
		close(started)
		select {
		case <-r.Context().Done():
		case <-released:
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(released) })
	client, err := New(Config{URL: server.URL, AllowInsecure: true})
	requirements.NoError(err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		_, err := client.RunMCPCLICommand(ctx, []string{"draft-get", "synthetic-draft", "--json"}, "")
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		requirements.FailNow("stream did not become observable")
	}
	cancel()
	select {
	case err := <-done:
		requirements.Error(err)
		requirements.ErrorIs(err, context.Canceled)
	case <-time.After(10 * time.Second):
		requirements.FailNow("stream did not stop after cancellation")
	}
}

func TestMCPCLIWireLimitIncludesEnvelopeAndAllowsExactBoundary(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	terminal := `{"type":"complete"}`
	wire := strings.Repeat(" ", (1<<20)-len(terminal)) + terminal
	result, err := decodeMCPCLIStream(strings.NewReader(wire))
	requirements.NoError(err)
	assertions.False(result.Failed)
	_, err = decodeMCPCLIStream(strings.NewReader(" " + wire))
	requirements.Error(err)
	assertions.EqualError(err, "output_limit_exceeded")
}

func TestMCPCLICancellationBeforeHeadersRetainsUncertainty(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	started, released := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-released:
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(released) })
	client, err := New(Config{URL: server.URL, AllowInsecure: true})
	requirements.NoError(err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	type outcome struct {
		result *MCPCLIResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := client.RunMCPCLICommand(ctx, []string{"draft-compose", "--json"}, "")
		done <- outcome{result, err}
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		requirements.FailNow("operation request did not become observable")
	}
	cancel()
	select {
	case got := <-done:
		requirements.Error(got.err)
		requirements.ErrorIs(got.err, context.Canceled)
		requirements.NotNil(got.result)
		assertions.True(got.result.OperationMayHaveCompleted)
	case <-time.After(10 * time.Second):
		requirements.FailNow("operation request did not stop after cancellation")
	}
}

func FuzzMCPCLIStreamOutputRoundTrip(f *testing.F) {
	f.Add("", "")
	f.Add("first", "second")
	f.Add("\x00\n—🙂", "{\"content\":null}")
	f.Fuzz(func(t *testing.T, first, second string) {
		// Materialized events are bounded; valid JSON strings are UTF-8.
		if len(first)+len(second) > 65536 {
			t.Skip()
		}
		first, second = string([]rune(first)), string([]rune(second))
		var wire strings.Builder
		for _, text := range []string{first, second} {
			data, err := json.Marshal(struct {
				Type string `json:"type"`
				Data string `json:"data"`
			}{"stdout", text})
			require.NoError(t, err)
			wire.Write(data)
			wire.WriteByte('\n')
		}
		wire.WriteString(`{"type":"complete"}`)
		result, err := decodeMCPCLIStream(strings.NewReader(wire.String()))
		require.NoError(t, err)
		assert.Equal(t, first+second, result.Stdout)
	})
}

func FuzzMCPCLIRejectsPostTerminalOutput(f *testing.F) {
	f.Add("", false)
	f.Add("\x00\n—🙂", true)
	f.Fuzz(func(t *testing.T, text string, failed bool) {
		if len(text) > 65536 {
			t.Skip()
		}
		text = string([]rune(text))
		data, err := json.Marshal(struct {
			Type string `json:"type"`
			Data string `json:"data"`
		}{"stdout", text})
		require.NoError(t, err)
		terminal := `{"type":"complete"}`
		if failed {
			terminal = `{"type":"error","error":"invalid_args"}`
		}
		_, err = decodeMCPCLIStream(strings.NewReader(terminal + "\n" + string(data)))
		assert.EqualError(t, err, "invalid_cli_stream")
	})
}

func TestMCPCLIPreservesProductionOutcomeCodes(t *testing.T) {
	for _, code := range []string{"revision_mismatch", "pending_operation", "provider_absent", "provider_refused", "insufficient_scope", "changed_externally", "not_supported", "local_persistence_failed", "claim_failed", "unknown_replacement", "invalid_draft", "invalid_state", "append_failed"} {
		t.Run(code, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			result, err := decodeMCPCLIStream(strings.NewReader(`{"type":"error","error":"` + code + `"}`))
			requirements.Error(err)
			requirements.NotNil(result)
			assertions.Equal(code, result.ErrorCode)
			assertions.Equal(code, err.Error())
		})
	}
}
