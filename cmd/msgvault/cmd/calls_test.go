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
	run := func(input string, args ...string) (string, error) {
		command := newCallsCommand(callCommandDeps{open: func(context.Context) (callCommandClient, func(), error) {
			return client, func() {}, nil
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
}
