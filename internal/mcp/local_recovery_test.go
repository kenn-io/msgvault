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
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type unavailablePersonFiles struct{ err error }

func (s unavailablePersonFiles) SearchPersonFiles(context.Context, PersonFileSearchRequest) (generated.PersonFileSearchHTTPResponse, error) {
	return generated.PersonFileSearchHTTPResponse{}, s.err
}

func TestLocalRecoveryErrorsAreSafeAcrossMCPHandlers(t *testing.T) {
	requirements := require.New(t)
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	requirements.NoError(err)
	endpoint := "http://" + listener.Addr().String()
	requirements.NoError(listener.Close())
	client, err := daemonclient.New(daemonclient.Config{URL: endpoint, AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	client.EnableLocalDaemonRecovery(func(context.Context) (daemonclient.LocalDaemonEndpoint, error) {
		return daemonclient.LocalDaemonEndpoint{}, errors.New("private-path /synthetic/archive private-key-value http://127.0.0.1:1234")
	})
	_, err = client.GetStats()
	requirements.Error(err)
	local := fmt.Errorf("backend request: %w", err)
	h := &handlers{embeddingStatus: client}
	tests := []struct {
		name    string
		handler toolHandler
		args    map[string]any
	}{
		{"wrapped", func(context.Context, toolRequest) (*toolResult, error) {
			return nil, newInternalError("read stats", local)
		}, nil},
		{"embeddings", h.getEmbeddingStatus, map[string]any{"source_id": float64(7)}},
	}

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
			assertions.Contains(content.Text, "daemon_unavailable: "+daemonclient.LocalDaemonUnavailableMessage)
			assertions.NotContains(content.Text, "private-path")
			assertions.NotContains(content.Text, "/synthetic/archive")
			assertions.NotContains(content.Text, "private-key-value")
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
