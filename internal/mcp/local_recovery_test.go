package mcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calcontrol"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type unavailablePersonFiles struct{ err error }

func (s unavailablePersonFiles) SearchPersonFiles(context.Context, PersonFileSearchRequest) (generated.PersonFileSearchHTTPResponse, error) {
	return generated.PersonFileSearchHTTPResponse{}, s.err
}

func TestLocalRecoveryErrorsAreSafeAcrossMCPHandlers(t *testing.T) {
	requirements := require.New(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	requirements.NoError(err)
	endpoint := "http://" + listener.Addr().String()
	requirements.NoError(listener.Close())
	client, err := daemonclient.New(daemonclient.Config{URL: endpoint, AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	requirements.NoError(client.EnableLocalDaemonRecovery(func(context.Context) (daemonclient.LocalDaemonEndpoint, error) {
		return daemonclient.LocalDaemonEndpoint{}, errors.New("private-path /synthetic/archive private-key-value http://127.0.0.1:1234")
	}))
	_, err = client.GetStats()
	requirements.Error(err)
	local := fmt.Errorf("backend request: %w", err)
	h := &handlers{mediaSearcher: client, personFileSearcher: unavailablePersonFiles{local}, calendar: calendarServiceBackend(func(context.Context, calcontrol.Request) (*calcontrol.Result, error) { return nil, local })}
	tests := []struct {
		name    string
		handler toolHandler
		args    map[string]any
	}{
		{"wrapped", func(context.Context, toolRequest) (*toolResult, error) {
			return nil, newInternalError("read stats", local)
		}, nil},
		{"media", h.searchMedia, map[string]any{"query": "synthetic"}},
		{"files", h.searchPersonFiles, map[string]any{"person_id": float64(7)}},
	}
	for _, definition := range stableCalendarTools() {
		if definition.name == "calendar_freebusy" {
			tests = append(tests, struct {
				name    string
				handler toolHandler
				args    map[string]any
			}{"calendar read", definition.bind(h), map[string]any{"account": "synthetic", "calendar_id": "primary", "time_min": "2026-11-03T00:00:00Z", "time_max": "2026-11-04T00:00:00Z"}})
		}
		if definition.name == "calendar_delete" {
			tests = append(tests, struct {
				name    string
				handler toolHandler
				args    map[string]any
			}{"calendar preview", definition.bind(h), map[string]any{"account": "synthetic", "calendar_id": "primary", "event_id": "synthetic-event"}})
		}
	}
	requirements.Len(tests, 5)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			result, output, err := officialToolHandler(tt.handler)(t.Context(), &sdkmcp.CallToolRequest{}, tt.args)
			requirements.NoError(err)
			requirements.NotNil(result)
			assertions.True(result.IsError)
			assertions.Nil(output)
			requirements.Len(result.Content, 1)
			content, ok := result.Content[0].(*sdkmcp.TextContent)
			requirements.True(ok)
			assertions.Equal("daemon_unavailable: "+daemonclient.LocalDaemonUnavailableMessage, content.Text)
		})
	}
}

func TestRemoteDaemonUnavailableCodeKeepsExistingClassification(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	remote := &daemonclient.APIError{Status: 503, Code: "daemon_unavailable", Message: "remote diagnostic"}
	assertions.Nil(translateDaemonRequestError(remote))
	_, _, err := officialToolHandler(func(context.Context, toolRequest) (*toolResult, error) {
		return nil, newInternalError("remote", remote)
	})(t.Context(), &sdkmcp.CallToolRequest{}, nil)
	requirements.Error(err)
	assertions.Contains(err.Error(), "internal server error")
	h := &handlers{personFileSearcher: unavailablePersonFiles{remote}}
	result := runToolExpectError(t, ToolSearchPersonFiles, h.searchPersonFiles, map[string]any{"person_id": float64(7)})
	assertions.Equal("person file search failed: "+remote.Error(), resultText(t, result))
}
