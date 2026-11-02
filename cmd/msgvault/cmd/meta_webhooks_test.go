package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/metawebhook"
)

// Only the remote HTTP boundary is redirected. The Graph client and its
// subscription parser, token binding check and write transport stay real.
func subscriptionFixture(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	destination, err := url.Parse(server.URL)
	require.NoError(t, err)
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = testTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "graph.facebook.com" {
			return nil, errors.New("unexpected fixture host")
		}
		assert.Equal(t, "Bearer synthetic-page-token", r.Header.Get("Authorization"))
		clone := r.Clone(r.Context())
		uri := *r.URL
		uri.Scheme, uri.Host = destination.Scheme, destination.Host
		clone.URL = &uri
		return original.RoundTrip(clone)
	})
}

func TestSubscribeUnionAndVerify(t *testing.T) {
	cfg := webhookTestConfig(t)
	paths, posted := []string{}, ""
	subscriptionFixture(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		switch {
		case r.URL.Path == "/v26.0/app":
			_, _ = fmt.Fprint(w, `{"id":"9"}`)
		case r.Method == http.MethodPost:
			require.NoError(t, r.ParseForm())
			posted = r.Form.Get("subscribed_fields")
			_, _ = fmt.Fprint(w, `{"success":true}`)
		default:
			fields := `["messages"]`
			if posted != "" {
				fields = `["feed","messages"]`
			}
			_, _ = fmt.Fprintf(w, `{"data":[{"id":"9","subscribed_fields":%s}]}`, fields)
		}
	})
	results, err := subscribeMetaWebhooks(t.Context(), cfg, nil, []string{"feed"}, false, meta.NewClient)
	require.NoError(t, err)
	assert.Equal(t, "feed,messages", posted)
	assert.Equal(t, []string{"GET /v26.0/app", "GET /v26.0/100/subscribed_apps", "POST /v26.0/100/subscribed_apps", "GET /v26.0/100/subscribed_apps"}, paths)
	require.Len(t, results, 1)
	assert.Equal(t, []string{"feed", "messages"}, results[0].Fields)
}

func TestSubscribePreservesFieldsPerPage(t *testing.T) {
	cfg := webhookTestConfig(t)
	second := facebookProfile()
	second.Identifier, second.PageID, second.Enabled = "second-page", "101", false
	cfg.FacebookComments = append(cfg.FacebookComments, second)
	posted := map[string]string{}
	subscriptionFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v26.0/app" {
			_, _ = fmt.Fprint(w, `{"id":"9"}`)
			return
		}
		if r.Method == http.MethodPost {
			require.NoError(t, r.ParseForm())
			posted[r.URL.Path] = r.Form.Get("subscribed_fields")
			_, _ = fmt.Fprint(w, `{"success":true}`)
			return
		}
		fields := []string{}
		if r.URL.Path == "/v26.0/100/subscribed_apps" {
			fields = []string{"messages"}
		}
		if value := posted[r.URL.Path]; value != "" {
			fields = strings.Split(value, ",")
		}
		body, err := json.Marshal(fields)
		require.NoError(t, err)
		_, _ = fmt.Fprintf(w, `{"data":[{"id":"9","subscribed_fields":%s}]}`, body)
	})
	_, err := subscribeMetaWebhooks(t.Context(), cfg, nil, []string{"feed"}, false, meta.NewClient)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"/v26.0/100/subscribed_apps": "feed,messages", "/v26.0/101/subscribed_apps": "feed"}, posted)
}

func TestSubscribeRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, app, subscriptions, want string
		status                         int
		check                          bool
	}{
		{"TokenAppMismatch", `{"id":"8"}`, "", "different app", 200, false},
		{"TokenAppError", `{"error":{"code":190}}`, "", "cannot verify", 400, false},
		{"MissingAppID", `{}`, "", "cannot verify", 200, false},
		{"CheckNotInstalled", `{"id":"9"}`, `{"data":[]}`, "not installed", 200, true},
		{"CheckMissingField", `{"id":"9"}`, `{"data":[{"id":"9","subscribed_fields":["messages"]}]}`, "missing requested", 200, true},
		{"UnidentifiableSubscription", `{"id":"9"}`, `{"data":[{"subscribed_fields":["feed"]}]}`, "cannot identify", 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := webhookTestConfig(t)
			posts := 0
			subscriptionFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts++
				}
				if r.URL.Path == "/v26.0/app" {
					w.WriteHeader(tc.status)
					_, _ = fmt.Fprint(w, tc.app)
				} else {
					_, _ = fmt.Fprint(w, tc.subscriptions)
				}
			})
			_, err := subscribeMetaWebhooks(t.Context(), cfg, nil, []string{"feed"}, tc.check, meta.NewClient)
			require.ErrorContains(t, err, tc.want)
			assert.Contains(t, err.Error(), "example-page")
			assert.NotContains(t, err.Error(), "synthetic-page-token")
			assert.Zero(t, posts)
		})
	}
}

func TestSubscribePermissionNamedError(t *testing.T) {
	cfg := webhookTestConfig(t)
	subscriptionFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v26.0/app" {
			_, _ = fmt.Fprint(w, `{"id":"9"}`)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, `{"error":{"code":200,"message":"synthetic-page-token"}}`)
	})
	_, err := subscribeMetaWebhooks(t.Context(), cfg, nil, []string{"feed"}, false, meta.NewClient)
	require.ErrorContains(t, err, "pages_manage_metadata")
	assert.Contains(t, err.Error(), "pages_show_list")
	assert.NotContains(t, err.Error(), "synthetic-page-token")
}

func TestSubscribeUnknownProfileAndField(t *testing.T) {
	cfg := webhookTestConfig(t)
	for _, tc := range []struct{ ids, fields []string }{{[]string{"missing"}, []string{"feed"}}, {nil, []string{"unsupported"}}} {
		_, err := subscribeMetaWebhooks(t.Context(), cfg, tc.ids, tc.fields, false, func(string) (*meta.Client, error) {
			assert.Fail(t, "invalid selection must fail before creating client")
			return nil, errors.New("unexpected client")
		})
		require.Error(t, err)
	}
}

func TestSubscribeMessengerFieldsWarn(t *testing.T) {
	cfg := webhookTestConfig(t)
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	subscriptionFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v26.0/app" {
			_, _ = fmt.Fprint(w, `{"id":"9"}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"data":[{"id":"9","subscribed_fields":["messages"]}]}`)
	})
	cmd := newSubscribeMetaWebhooksCmd()
	var out, warning bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&warning)
	cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	require.NoError(t, cmd.Flags().Set("fields", "messages"))
	require.NoError(t, cmd.Flags().Set("check", "true"))
	require.NoError(t, cmd.RunE(cmd, nil))
	assert.Contains(t, warning.String(), "primary receiver")
	assert.Contains(t, warning.String(), "pages_messaging")
}

func TestStatusReportsCountsAndDisabledReason(t *testing.T) {
	cfg := webhookTestConfig(t)
	root := filepath.Join(cfg.Data.DataDir, "meta-webhooks")
	spool, err := metawebhook.OpenSpool(root, 1<<20, time.Now)
	require.NoError(t, err)
	id, _, err := spool.Accept("9", []byte(`{"object":"page"}`))
	require.NoError(t, err)
	require.NoError(t, spool.Settle("9", id, metawebhook.Settlement{State: metawebhook.StateHeld, Outcomes: map[string]int{"page/messages/unhandled": 1}}))
	require.NoError(t, metawebhook.WriteReceiverSnapshot(root, metawebhook.ReceiverSnapshot{Listener: "disabled", DisabledReason: "EXAMPLE_META_APP_SECRET missing", WrittenAt: time.Now()}))
	st, source := webhookArchive(t, false)
	poll, err := st.GetLastSuccessfulSyncByTypeContext(t.Context(), source.ID, meta.SourceType)
	require.NoError(t, err)
	report, err := metaWebhooksStatus(t.Context(), cfg, st)
	require.NoError(t, err)
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(encoded, &wire))
	assert.ElementsMatch(t, []string{"listener", "addr", "disabled_reason", "written_at", "counters", "apps", "pages", "spool_bytes", "max_spool_bytes", "errors"}, mapKeys(wire))
	assert.Equal(t, "disabled", wire["listener"])
	assert.Equal(t, "EXAMPLE_META_APP_SECRET missing", wire["disabled_reason"])
	require.Len(t, report.Apps, 1)
	assert.Equal(t, 1, report.Apps[0].Held)
	assert.Equal(t, map[string]int{"page/messages/unhandled": 1}, report.Apps[0].HeldOutcomes)
	assert.Positive(t, report.SpoolBytes)
	require.Len(t, report.Pages, 1)
	assert.Equal(t, poll.CompletedAt.Time, *report.Pages[0].LastPollCompletedAt)
}

func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func TestStatusStaleSnapshot(t *testing.T) {
	cfg := webhookTestConfig(t)
	root := filepath.Join(cfg.Data.DataDir, "meta-webhooks")
	for _, stale := range []bool{false, true} {
		if stale {
			require.NoError(t, metawebhook.WriteReceiverSnapshot(root, metawebhook.ReceiverSnapshot{Listener: "listening", WrittenAt: time.Now().Add(-3 * time.Minute)}))
		}
		report, err := metaWebhooksStatus(t.Context(), cfg, nil)
		require.NoError(t, err)
		assert.Equal(t, "daemon not running or no snapshot", report.Listener)
	}
}

func TestReplayFiltersAndDryRun(t *testing.T) {
	root := t.TempDir()
	spool, err := metawebhook.OpenSpool(root, 1<<20, time.Now)
	require.NoError(t, err)
	ids := []string{}
	for _, app := range []string{"9", "8"} {
		id, _, err := spool.Accept(app, []byte(`{"object":"page","entry":[]}`))
		require.NoError(t, err)
		require.NoError(t, spool.Settle(app, id, metawebhook.Settlement{State: metawebhook.StateHeld}))
		ids = append(ids, id)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	worker := metawebhook.NewWorker(metawebhook.WorkerOptions{Spool: spool, Registry: metawebhook.NewRegistry(), Tick: 10 * time.Millisecond})
	go func() { done <- worker.Run(ctx) }()
	defer func() { cancel(); require.NoError(t, <-done) }()
	filter := metawebhook.RequeueFilter{AppID: "9", States: []metawebhook.State{metawebhook.StateHeld}, DeliveryID: ids[0], Since: time.Now().Add(-time.Hour)}
	n, err := replayMetaWebhooks(t.Context(), root, filter, true)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	stats, err := spool.Stats()
	require.NoError(t, err)
	assert.Equal(t, 1, stats["9"].Held)
	n, err = replayMetaWebhooks(t.Context(), root, filter, false)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	stats, err = spool.Stats()
	require.NoError(t, err)
	assert.Equal(t, 1, stats["8"].Held)
}

func TestReplayNoDaemonTimesOut(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err := replayMetaWebhooks(ctx, t.TempDir(), metawebhook.RequeueFilter{States: []metawebhook.State{metawebhook.StateHeld}}, false)
	require.ErrorContains(t, err, "daemon not running")
}

func TestReplayRejectsInvalidFilters(t *testing.T) {
	for _, args := range [][]string{{"--app", "../9"}, {"--state", "pending"}, {"--delivery", strings.Repeat("A", 64)}, {"--since", "yesterday"}} {
		cmd := newReplayMetaWebhooksCmd()
		cmd.SetArgs(args)
		require.Error(t, cmd.Execute())
	}
}
