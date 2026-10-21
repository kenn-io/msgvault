package cmd

import (
	"bytes"
	"context"
	"log/slog"
	"net/http/httptest"
	"strconv"
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
	version          string
	imports          [][]byte
	slackImports     []int64
	lists, summaries []store.CallQuery
}

func (c *fakeCallCommandClient) APISchemaVersion(context.Context) (string, error) {
	if c.version != "" {
		return c.version, nil
	}
	return callsMinAPIVersion, nil
}

func (c *fakeCallCommandClient) ImportCallObservation(_ context.Context, data []byte) (*generated.CallImportResponse, error) {
	c.imports = append(c.imports, data)
	return &generated.CallImportResponse{MessageID: 42, SourceID: 7}, nil
}

func (c *fakeCallCommandClient) ImportSlackHuddle(_ context.Context, id int64) (*generated.CallImportResponse, error) {
	c.slackImports = append(c.slackImports, id)
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
	assertions := assert.New(t)
	requirements := require.New(t)
	client := &fakeCallCommandClient{}
	cleanups := 0
	run := func(input string, args ...string) (string, error) {
		command := newCallsCommand(callCommandDeps{open: func(context.Context) (callCommandClient, func(), error) {
			return client, func() { cleanups++ }, nil
		}})
		var output bytes.Buffer
		command.SetOut(&output)
		command.SetErr(&output)
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
}

func TestCallsRejectDaemonWithoutCallRoutes(t *testing.T) {
	client := &fakeCallCommandClient{version: "5.1.0"}
	cleanups := 0
	command := newCallsCommand(callCommandDeps{open: func(context.Context) (callCommandClient, func(), error) {
		return client, func() { cleanups++ }, nil
	}})
	command.SetArgs([]string{"list"})
	err := command.ExecuteContext(t.Context())
	require.ErrorContains(t, err, "calls require daemon API 5.2.0 or newer")
	assert.Empty(t, client.lists)
	assert.Equal(t, 1, cleanups)
}

func TestSlackCallsRequireNewerDaemon(t *testing.T) {
	client := &fakeCallCommandClient{version: callsMinAPIVersion}
	run := func(input string, args ...string) error {
		cmd := newCallsCommand(callCommandDeps{open: func(context.Context) (callCommandClient, func(), error) { return client, func() {}, nil }})
		cmd.SetIn(strings.NewReader(input))
		cmd.SetArgs(args)
		return cmd.ExecuteContext(t.Context())
	}
	slack := `{"format":"slack_huddle","account":"T:U","record":{"type":"message","subtype":"huddle_thread","ts":"1","room":{"id":"R","date_start":1}}}`
	require.ErrorContains(t, run(slack, "import", "-"), "calls require daemon API 5.2.0 or newer")
	require.ErrorContains(t, run("", "import-slack", "1"), "calls require daemon API 5.2.0 or newer")
	assert.Empty(t, client.imports)
	assert.Empty(t, client.slackImports)
	client.version = slackCallsMinAPIVersion
	require.NoError(t, run("", "import-slack", "1"))
	assert.Equal(t, []int64{1}, client.slackImports)
}

func TestCallsImportSlackThroughDaemon(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("slack", "T_EXAMPLE:U_OWNER")
	require.NoError(err)
	convID, err := st.EnsureConversation(source.ID, "C", "Example channel")
	require.NoError(err)
	id, err := st.UpsertMessage(&store.Message{ConversationID: convID, SourceID: source.ID, SourceMessageID: "C:1", MessageType: "slack"})
	require.NoError(err)
	raw := []byte(`{"type":"message","subtype":"huddle_thread","ts":"1","room":{"id":"R_EXAMPLE","date_start":1790841600,"participants":["U_OWNER","U_PEER"]}}`)
	require.NoError(st.UpsertMessageRawWithFormat(id, raw, "slack_json"))
	peer, err := st.EnsureParticipantByIdentifier("slack", "T_EXAMPLE:U_PEER", "Example Peer")
	require.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(peer)
	require.NoError(err)
	cfg := &config.Config{Server: config.ServerConfig{APIKey: "synthetic-call-key"}}
	adapter := &storeAPIAdapter{store: st, config: cfg, logger: slog.New(slog.DiscardHandler)}
	server := httptest.NewServer(api.NewServer(cfg, adapter, nil, slog.New(slog.DiscardHandler)).Router())
	defer server.Close()
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: cfg.Server.APIKey, AllowInsecure: true, HTTPClient: server.Client()})
	require.NoError(err)
	defer func() { _ = client.Close() }()
	var first string
	for range 2 {
		command := newCallsCommand(callCommandDeps{open: func(context.Context) (callCommandClient, func(), error) { return client, func() {}, nil }})
		var output bytes.Buffer
		command.SetOut(&output)
		command.SetArgs([]string{"import-slack", strconv.FormatInt(id, 10)})
		require.NoError(command.ExecuteContext(t.Context()))
		if first == "" {
			first = output.String()
		} else {
			assert.Equal(first, output.String())
		}
	}
	summary, err := st.SummarizeCallsContext(t.Context(), store.CallQuery{PersonID: person.ID})
	require.NoError(err)
	require.Len(summary, 1)
	assert.Zero(summary[0].Answered)
	assert.Equal(1, summary[0].Unknown)
}
