package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
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

func TestCallsCommandRequiresSchemaForPublishedEndpoints(t *testing.T) {
	st := testutil.NewTestStore(t)
	cfg := &config.Config{Server: config.ServerConfig{APIKey: "synthetic-call-key"}}
	adapter := &storeAPIAdapter{store: st, config: cfg, logger: slog.New(slog.DiscardHandler)}
	router := api.NewServer(cfg, adapter, nil, slog.New(slog.DiscardHandler)).Router()
	for _, version := range []string{"3.24.0", "3.25.0", "3.26.0", "3.27.0", "3.28.0", "3.29.0", "3.34.0", "3.35.0"} {
		t.Run(version, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/health" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w, `{"status":"ok","api_schema_version":%q}`, version)
					return
				}
				router.ServeHTTP(w, r)
			}))
			defer server.Close()
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: cfg.Server.APIKey, AllowInsecure: true, HTTPClient: server.Client()})
			requirements.NoError(err)
			defer func() { _ = client.Close() }()
			command := newCallsCommand(callCommandDeps{open: func(context.Context) (callCommandClient, func(), error) { return client, func() {}, nil }})
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetErr(&output)
			command.SetArgs([]string{"list"})
			err = command.ExecuteContext(t.Context())
			if version == "3.35.0" {
				requirements.NoError(err)
				assertions.Contains(output.String(), `"items"`)
			} else {
				requirements.ErrorContains(err, "calls require daemon API 3.35.0 or newer")
				assertions.Contains(err.Error(), "daemon reports "+version)
			}
		})
	}
}
