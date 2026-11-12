package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/testutil"
)

// inboxHTTPFixture serves a Page whose inbox holds one one-to-one chat and
// one group conversation.
func inboxHTTPFixture(t *testing.T) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v26.0/me":
			_, _ = fmt.Fprint(w, `{"id":"100","name":"Example Page","category":"Community"}`)
		case "/v26.0/100/conversations":
			_, _ = fmt.Fprint(w, `{"data":[
				{"id":"t_1","participants":{"data":[{"id":"700","name":"Example Customer"},{"id":"100","name":"Example Page"}]},
				 "messages":{"data":[{"id":"m_1","created_time":"2026-10-01T12:00:00+0000","from":{"id":"700"},"message":"orchard question"}]}},
				{"id":"t_2","participants":{"data":[{"id":"100"},{"id":"701"},{"id":"702"}]},
				 "messages":{"data":[{"id":"m_2","created_time":"2026-10-01T12:00:00+0000","from":{"id":"701"},"message":"group hello"}]}}]}`)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	destination, err := url.Parse(server.URL)
	require.NoError(t, err)
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	// Intercept the external boundary only; client, importer and Store remain real.
	http.DefaultTransport = testTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "graph.facebook.com" {
			return nil, errors.New("unexpected fixture host")
		}
		clone := r.Clone(r.Context())
		uri := *r.URL
		uri.Scheme, uri.Host = destination.Scheme, destination.Host
		clone.URL = &uri
		return original.RoundTrip(clone)
	})
}

func TestInboxSyncReportsSkippedConversations(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	t.Setenv("EXAMPLE_FACEBOOK_PAGE_TOKEN", "synthetic-page-token")
	inboxHTTPFixture(t)
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Data.DataDir = t.TempDir()
	cfg.Analytics.Engine = config.AnalyticsEngineSQL
	cfg.Vector.Enabled = false
	cfg.FacebookMessages = []config.FacebookCommentsSource{facebookProfile()}
	st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
	_, err := meta.NewInboxImporter(st, mustInboxClient(t)).Register(t.Context(), meta.Inbox{App: "9", Page: "100"})
	require.NoError(err)

	cmd := facebookInbox.syncCmd("", "")
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	require.NoError(cmd.RunE(cmd, nil))
	assert.Contains(output.String(), "example-page: 1 messages processed (1 added, 0 updated), 1 requests, 1 conversations skipped (not a one-to-one chat)\n")
}

func mustInboxClient(t *testing.T) *meta.Client {
	t.Helper()
	c, err := meta.NewClient("synthetic-page-token")
	require.NoError(t, err)
	return c
}
