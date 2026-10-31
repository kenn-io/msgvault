package daemonclient_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
)

func TestSendReplyRequiresDaemonHealthCapability(t *testing.T) {
	var runs atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/health" {
			fmt.Fprint(w, `{"status":"ok","api_schema_version":"3.28.0"}`)
			return
		}
		runs.Add(1)
		fmt.Fprintln(w, `{"type":"complete"}`)
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "example-owner", AllowInsecure: true})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	err = client.RunCLICommand(t.Context(), daemonclient.CLIRunRequest{Args: []string{"send-reply", "--draft=chat-draft-example", "--revision=1"}}, func(string, string) error { return nil })
	require.ErrorContains(t, err, "x_reply_send")
	assert.Zero(t, runs.Load(), "old daemon must not receive a send request or trigger local fallback")
}
