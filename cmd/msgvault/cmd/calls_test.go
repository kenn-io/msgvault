package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type fakeCallCommandClient struct {
	imports          [][]byte
	lists, summaries []store.CallQuery
	importErrorAt    int
}

func (c *fakeCallCommandClient) APISchemaVersion(context.Context) (string, error) {
	return callsMinAPIVersion, nil
}

func (c *fakeCallCommandClient) ImportCallObservation(_ context.Context, data []byte) (*generated.CallImportResponse, error) {
	c.imports = append(c.imports, bytes.Clone(data))
	if len(c.imports) == c.importErrorAt {
		return nil, errors.New("import rejected")
	}
	return &generated.CallImportResponse{MessageID: 42, SourceID: 7}, nil
}

func (c *fakeCallCommandClient) ListCallObservations(_ context.Context, q store.CallQuery) (*generated.CallEvidenceResponse, error) {
	c.lists = append(c.lists, q)
	return &generated.CallEvidenceResponse{}, nil
}

func (c *fakeCallCommandClient) SummarizeCalls(_ context.Context, q store.CallQuery) (*generated.CallSummaryResponse, error) {
	c.summaries = append(c.summaries, q)
	return &generated.CallSummaryResponse{Items: []generated.PersonCallSummary{{PersonID: 23, Answered: 2, ExampleMessageID: 42}}}, nil
}

func TestCallsCommandThroughDaemon(t *testing.T) {
	t.Run("continue after rejected Signal row", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		st := testutil.NewTestStore(t)
		cfg := &config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key"}}
		logger := slog.New(slog.DiscardHandler)
		daemon := httptest.NewServer(api.NewServer(cfg, &storeAPIAdapter{store: st, config: cfg, logger: logger}, nil, logger).Router())
		t.Cleanup(daemon.Close)
		client, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, APIKey: cfg.Server.APIKey, AllowInsecure: true, HTTPClient: daemon.Client()})
		require.NoError(err)
		t.Cleanup(func() { assert.NoError(client.Close()) })
		command := newCallsCommand(callCommandDeps{open: func(context.Context) (callCommandClient, func(), error) {
			return client, func() {}, nil
		}})
		var output, stderr bytes.Buffer
		command.SetOut(&output)
		command.SetErr(&stderr)
		command.SetIn(strings.NewReader(
			`{"format":"signal_desktop","account":"example","record":{"callId":"one","mode":"Direct","status":"Accepted","timestamp":1790841600000}}` + "\n" +
				`{"format":"signal_desktop","account":"example","record":{"callId":"deleted","mode":"Direct","status":"Deleted","timestamp":1790841600000}}` + "\n" +
				`{"format":"signal_desktop","account":"example","record":{"callId":"two","mode":"Direct","status":"Accepted","timestamp":1790841660000}}`))
		command.SetArgs([]string{"import", "-"})
		require.ErrorContains(command.ExecuteContext(t.Context()), "1 call import records rejected")
		assert.Contains(stderr.String(), "call import record 2:")
		assert.Contains(stderr.String(), "Signal deleted call")

		output.Reset()
		command.SetArgs([]string{"list"})
		require.NoError(command.ExecuteContext(t.Context()))
		var calls generated.CallEvidenceResponse
		require.NoError(json.Unmarshal(output.Bytes(), &calls))
		require.Len(calls.Items, 2)
		assert.ElementsMatch([]string{"one", "two"}, []string{calls.Items[0].RecordID, calls.Items[1].RecordID})
	})

	assertions := assert.New(t)
	requirements := require.New(t)
	client := &fakeCallCommandClient{}
	cleanups := 0
	run := func(input string, args ...string) (string, error) {
		command := newCallsCommand(callCommandDeps{open: func(context.Context) (callCommandClient, func(), error) {
			return client, func() { cleanups++ }, nil
		}})
		command.SilenceUsage = true
		var output bytes.Buffer
		command.SetOut(&output)
		command.SetErr(io.Discard)
		command.SetIn(strings.NewReader(input))
		command.SetArgs(args)
		err := command.ExecuteContext(t.Context())
		return output.String(), err
	}
	const input = `{"format":"signal_desktop","account":"example","record":{"callId":"one","timestamp":1790841600000}}`
	output, err := run(input, "import", "-")
	requirements.NoError(err, output)
	assertions.Equal([][]byte{[]byte(input)}, client.imports)
	assertions.JSONEq(`{"message_id":42,"source_id":7}`, output)
	after := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	before := after.AddDate(0, 1, 0)
	output, err = run("", "summary", "--person-id", "23", "--limit", "3", "--offset", "2", "--after", "2026-10-01T00:00:00Z", "--before", "2026-11-01T00:00:00Z")
	requirements.NoError(err, output)
	assertions.Equal([]store.CallQuery{{PersonID: 23, Limit: 3, Offset: 2, After: &after, Before: &before}}, client.summaries)
	assertions.JSONEq(`{"items":[{"person_id":23,"answered":2,"missed":0,"declined":0,"cancelled":0,"unknown":0,"example_message_id":42}]}`, output)
	output, err = run("", "list", "--person-id", "23", "--limit", "1", "--after-id", "42", "--after", "2026-10-01T00:00:00Z", "--before", "2026-11-01T00:00:00Z")
	requirements.NoError(err, output)
	assertions.Equal([]store.CallQuery{{PersonID: 23, Limit: 1, AfterID: 42, After: &after, Before: &before}}, client.lists)
	assertions.JSONEq(`{"items":[]}`, output)
	_, err = run("", "summary", "--after", "invalid")
	requirements.Error(err)
	assertions.Len(client.summaries, 1)
	assertions.Equal(3, cleanups, "invalid --after must fail before opening the daemon client")

	second := strings.Replace(input, `"callId":"one"`, `"callId":"two"`, 1)
	for _, tc := range []struct {
		name          string
		input         string
		file          bool
		importErrorAt int
		wantImports   int
		wantResults   int
		wantError     string
	}{
		{name: "several from stdin", input: input + "\n" + second, wantImports: 2, wantResults: 2},
		{name: "several from file", input: input + second, file: true, wantImports: 2, wantResults: 2},
		{name: "stop at invalid JSON", input: input + "\n{invalid}\n" + input, wantImports: 1, wantResults: 1, wantError: "call import record 2:"},
		{name: "stop at import error", input: input + second + input, importErrorAt: 2, wantImports: 2, wantResults: 1, wantError: "call import record 2: import rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			client = &fakeCallCommandClient{importErrorAt: tc.importErrorAt}
			path := "-"
			stdin := tc.input
			if tc.file {
				path = filepath.Join(t.TempDir(), "calls.json")
				require.NoError(os.WriteFile(path, []byte(tc.input), 0600))
				stdin = ""
			}
			output, err := run(stdin, "import", path)
			if tc.wantError == "" {
				require.NoError(err, output)
			} else {
				require.ErrorContains(err, tc.wantError)
			}
			require.Len(client.imports, tc.wantImports)
			for i, data := range client.imports {
				assert.JSONEq([]string{input, second}[i], string(data))
			}
			assert.Equal(strings.Repeat("{\"message_id\":42,\"source_id\":7}\n", tc.wantResults), output)
		})
	}
}
