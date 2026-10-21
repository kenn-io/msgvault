package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestCallsCommandThroughDaemon(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	peer, err := st.EnsureParticipantByPhone("+12025550101", "Example", "signal")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(peer)
	requirements.NoError(err)
	cfg := &config.Config{Server: config.ServerConfig{APIKey: "synthetic-call-key"}}
	adapter := &storeAPIAdapter{store: st, config: cfg, logger: slog.New(slog.DiscardHandler)}
	server := httptest.NewServer(api.NewServer(cfg, adapter, nil, slog.New(slog.DiscardHandler)).Router())
	defer server.Close()
	client, err := daemonclient.New(daemonclient.Config{
		URL: server.URL, APIKey: cfg.Server.APIKey, AllowInsecure: true, HTTPClient: server.Client(),
	})
	requirements.NoError(err)
	defer func() { _ = client.Close() }()
	var commandClient callCommandClient = client
	run := func(input string, args ...string) (string, error) {
		command := newCallsCommand(callCommandDeps{open: func(context.Context) (callCommandClient, func(), error) {
			return commandClient, func() {}, nil
		}})
		var output bytes.Buffer
		command.SetOut(&output)
		command.SetErr(&output)
		command.SetIn(strings.NewReader(input))
		command.SetArgs(args)
		err := command.ExecuteContext(t.Context())
		return output.String(), err
	}
	input := fmt.Sprintf(`{"format":"signal_desktop","account":"example","record":{"callId":"one","peerId":"peer","mode":"Direct","status":"Accepted","timestamp":1790841600000},"bindings":{"peer":%d}}`, peer)
	for range 2 {
		output, err := run(input, "import", "-")
		requirements.NoError(err, output)
		assertions.Contains(output, `"message_id"`)
	}
	output, err := run("", "summary", "--person-id", strconv.FormatInt(person.ID, 10))
	requirements.NoError(err, output)
	var result api.CallSummaryResponse
	requirements.NoError(json.Unmarshal([]byte(output), &result))
	requirements.Len(result.Items, 1)
	assertions.Equal(1, result.Items[0].Answered)
	output, err = run("", "list", "--limit", "1")
	requirements.NoError(err, output)
	var evidence api.CallEvidenceResponse
	requirements.NoError(json.Unmarshal([]byte(output), &evidence))
	requirements.Len(evidence.Items, 1)
	assertions.Equal("joined", evidence.Items[0].Participants[0].Participation)
	_, err = run("", "summary", "--after", "invalid")
	requirements.Error(err)
	_, err = run("{", "import", "-")
	requirements.Error(err)
	// Only the advertised schema version is replaced; imports still use the real daemon.
	commandClient = oldCallDaemon{client}
	slackInput := `{"format":"slack_huddle","account":"T:U","record":{"type":"message","subtype":"huddle_thread","ts":"1","room":{"id":"R","date_start":1}}}`
	_, err = run(slackInput, "import", "-")
	require.ErrorContains(t, err, "calls require daemon API 3.25.0 or newer; daemon reports 3.24.0")
	_, err = run("", "import-slack", "1")
	require.ErrorContains(t, err, "calls require daemon API 3.25.0 or newer; daemon reports 3.24.0")
	_, err = run(input, "import", "-")
	require.NoError(t, err, "existing formats remain compatible with 3.24.0")
}

type oldCallDaemon struct{ callCommandClient }

func (oldCallDaemon) APISchemaVersion(context.Context) (string, error) { return "3.24.0", nil }

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
