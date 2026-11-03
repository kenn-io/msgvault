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
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/metawebhook"
)

// Only the remote HTTP boundary is redirected. The Graph client and its
// subscription parser, token binding check and write transport stay real.
func subscriptionFixture(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	destination, err := url.Parse(server.URL)
	require.NoError(err)
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = testTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "graph.facebook.com" {
			return nil, errors.New("unexpected fixture host")
		}
		assert.Equal("Bearer synthetic-page-token", r.Header.Get("Authorization"))
		clone := r.Clone(r.Context())
		uri := *r.URL
		uri.Scheme, uri.Host = destination.Scheme, destination.Host
		clone.URL = &uri
		return original.RoundTrip(clone)
	})
}

func TestSubscribeUnionAndVerify(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg := webhookTestConfig(t)
	paths, posted := []string{}, ""
	subscriptionFixture(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		switch {
		case r.URL.Path == "/v26.0/app":
			_, _ = fmt.Fprint(w, `{"id":"9"}`)
		case r.Method == http.MethodPost:
			if !assert.NoError(r.ParseForm()) {
				return
			}
			posted = r.Form.Get("subscribed_fields")
			_, _ = fmt.Fprint(w, `{"success":true}`)
		default:
			fields := `["message_reads"]`
			if posted != "" {
				fields = `["feed","message_reads"]`
			}
			_, _ = fmt.Fprintf(w, `{"data":[{"id":"9","subscribed_fields":%s}]}`, fields)
		}
	})
	results, err := subscribeMetaWebhooks(t.Context(), cfg, nil, []string{"feed"}, false, meta.NewClient)
	require.NoError(err)
	assert.Equal("feed,message_reads", posted)
	assert.Equal([]string{"GET /v26.0/app", "GET /v26.0/100/subscribed_apps", "POST /v26.0/100/subscribed_apps", "GET /v26.0/100/subscribed_apps"}, paths)
	require.Len(results, 1)
	assert.Equal([]string{"feed", "message_reads"}, results[0].Fields)
}

func TestSubscribePreservesFieldsPerPage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
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
			if !assert.NoError(r.ParseForm()) {
				return
			}
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
		if !assert.NoError(err) {
			return
		}
		_, _ = fmt.Fprintf(w, `{"data":[{"id":"9","subscribed_fields":%s}]}`, body)
	})
	_, err := subscribeMetaWebhooks(t.Context(), cfg, nil, []string{"feed"}, false, meta.NewClient)
	require.NoError(err)
	assert.Equal(map[string]string{"/v26.0/100/subscribed_apps": "feed,messages", "/v26.0/101/subscribed_apps": "feed"}, posted)
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
		{"PartialSubscriptionListing", `{"id":"9"}`, `{"data":[{"id":"8","subscribed_fields":["feed"]}],"paging":{"next":"https://graph.facebook.com/v26.0/100/subscribed_apps?after=example"}}`, "incomplete subscription listing", 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
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
			require.ErrorContains(err, tc.want)
			assert.Contains(err.Error(), "example-page")
			assert.NotContains(err.Error(), "synthetic-page-token")
			assert.Zero(posts)
		})
	}
}

func TestSubscribePermissionNamedError(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
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
	require.ErrorContains(err, "pages_manage_metadata")
	assert.Contains(err.Error(), "pages_show_list")
	assert.NotContains(err.Error(), "synthetic-page-token")
}

func TestSubscribeUnknownProfileAndField(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg := webhookTestConfig(t)
	for _, tc := range []struct{ ids, fields []string }{{[]string{"missing"}, []string{"feed"}}, {nil, []string{"unsupported"}}} {
		_, err := subscribeMetaWebhooks(t.Context(), cfg, tc.ids, tc.fields, false, func(string) (*meta.Client, error) {
			assert.Fail("invalid selection must fail before creating client")
			return nil, errors.New("unexpected client")
		})
		require.Error(err)
	}
}

func TestSubscribeMessengerFieldsWarn(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
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
	require.NoError(cmd.Flags().Set("fields", "messages"))
	require.NoError(cmd.Flags().Set("check", "true"))
	require.NoError(cmd.RunE(cmd, nil))
	assert.Contains(warning.String(), "primary receiver")
	assert.Contains(warning.String(), "pages_messaging")
}

func TestStatusReportsCountsAndDisabledReason(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg := webhookTestConfig(t)
	root := filepath.Join(cfg.Data.DataDir, "meta-webhooks")
	spool, err := metawebhook.OpenSpool(root, 1<<20, time.Now)
	require.NoError(err)
	id, _, err := spool.Accept("9", []byte(`{"object":"page"}`))
	require.NoError(err)
	require.NoError(spool.Settle("9", id, metawebhook.Settlement{State: metawebhook.StateHeld, Outcomes: map[string]int{"page/messages/unhandled": 1}}))
	require.NoError(metawebhook.WriteReceiverSnapshot(root, metawebhook.ReceiverSnapshot{Listener: "disabled", DisabledReason: "EXAMPLE_META_APP_SECRET missing", WrittenAt: time.Now()}))
	st, source := webhookArchive(t, false)
	poll, err := st.GetLastSuccessfulSyncByTypeContext(t.Context(), source.ID, meta.SourceType)
	require.NoError(err)
	report := metaWebhooksStatus(t.Context(), cfg, st)
	encoded, err := json.Marshal(report)
	require.NoError(err)
	var wire map[string]any
	require.NoError(json.Unmarshal(encoded, &wire))
	assert.ElementsMatch([]string{"listener", "addr", "disabled_reason", "written_at", "counters", "apps", "pages", "spool_bytes", "max_spool_bytes", "errors"}, mapKeys(wire))
	assert.Equal("disabled", wire["listener"])
	assert.Equal("EXAMPLE_META_APP_SECRET missing", wire["disabled_reason"])
	require.Len(report.Apps, 1)
	assert.Equal(1, report.Apps[0].Held)
	assert.Equal(map[string]int{"page/messages/unhandled": 1}, report.Apps[0].HeldOutcomes)
	assert.Positive(report.SpoolBytes)
	require.Len(report.Pages, 1)
	assert.Equal(poll.CompletedAt.Time, *report.Pages[0].LastPollCompletedAt)
}

func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func TestStatusStaleSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg := webhookTestConfig(t)
	root := filepath.Join(cfg.Data.DataDir, "meta-webhooks")
	for _, stale := range []bool{false, true} {
		if stale {
			require.NoError(metawebhook.WriteReceiverSnapshot(root, metawebhook.ReceiverSnapshot{Listener: "listening", WrittenAt: time.Now().Add(-3 * time.Minute)}))
		}
		report := metaWebhooksStatus(t.Context(), cfg, nil)
		assert.Equal("daemon not running or no snapshot", report.Listener)
	}
}

func TestReplayFiltersAndDryRun(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	root := t.TempDir()
	spool, err := metawebhook.OpenSpool(root, 1<<20, time.Now)
	require.NoError(err)
	ids := []string{}
	for _, app := range []string{"9", "8"} {
		id, _, err := spool.Accept(app, []byte(`{"object":"page","entry":[]}`))
		require.NoError(err)
		require.NoError(spool.Settle(app, id, metawebhook.Settlement{State: metawebhook.StateHeld}))
		ids = append(ids, id)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	worker := metawebhook.NewWorker(metawebhook.WorkerOptions{Spool: spool, Registry: metawebhook.NewRegistry(), Tick: 10 * time.Millisecond})
	go func() { done <- worker.Run(ctx) }()
	defer func() {
		cancel()
		require.NoError(<-done)
	}()
	filter := metawebhook.RequeueFilter{AppID: "9", States: []metawebhook.State{metawebhook.StateHeld}, DeliveryID: ids[0], Since: time.Now().Add(-time.Hour)}
	n, err := replayMetaWebhooks(t.Context(), root, filter, true)
	require.NoError(err)
	assert.Equal(1, n)
	stats, err := spool.Stats()
	require.NoError(err)
	assert.Equal(1, stats["9"].Held)
	n, err = replayMetaWebhooks(t.Context(), root, filter, false)
	require.NoError(err)
	assert.Equal(1, n)
	stats, err = spool.Stats()
	require.NoError(err)
	assert.Equal(1, stats["8"].Held)
}

func TestReplayCanceledWithoutDaemon(t *testing.T) {
	require := require.New(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := replayMetaWebhooks(ctx, t.TempDir(), metawebhook.RequeueFilter{States: []metawebhook.State{metawebhook.StateHeld}}, false)
	require.ErrorContains(err, "daemon not running")
}

func TestMetaWebhookCommandGrammar(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	group, _, err := rootCmd.Find([]string{"meta-webhooks"})
	require.NoError(err)
	require.Equal("meta-webhooks", group.Name())
	assert.Empty(group.Aliases)
	assert.Len(group.Commands(), 3)
	for _, action := range []string{"subscribe", "status", "replay"} {
		leaf, _, err := rootCmd.Find([]string{"meta-webhooks", action})
		require.NoError(err)
		assert.Equal(action, leaf.Name())
		assert.Empty(leaf.Aliases)
		assert.Equal([]string{"meta-webhooks", action}, daemonCLICommandPath(leaf))
	}
}

func TestMetaWebhookInvalidFilterErrorContract(t *testing.T) {
	for _, tc := range []struct{ flag, value, diagnostic string }{
		{"app", "../9", "--app must be a decimal app ID"},
		{"state", "pending", "--state must be held, failed or done"},
		{"delivery", strings.Repeat("A", 64), "--delivery must be a lowercase SHA-256 delivery ID"},
		{"since", "yesterday", "--since must be RFC3339"},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			command := newReplayMetaWebhooksCmd()
			require.NoError(command.Flags().Set(tc.flag, tc.value))
			err := command.RunE(command, nil)
			var diagnostic interface {
				error
				Hint() string
				ExitCode() int
			}
			require.ErrorAs(err, &diagnostic)
			assert.Equal(3, diagnostic.ExitCode())
			assert.Contains(err.Error(), tc.diagnostic)
			assert.Contains(diagnostic.Hint(), "meta-webhooks replay --help")
		})
	}
}

func TestMetaWebhookSetupErrorContracts(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cfg      bool
		profiles bool
		args     []string
		want     int
	}{
		{name: "no profiles", cfg: true, want: 5},
		{name: "missing profile", cfg: true, profiles: true, args: []string{"absent"}, want: 4},
		{name: "missing token", cfg: true, profiles: true, want: 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
			command := newSubscribeMetaWebhooksCmd()
			if tc.cfg {
				cfg := webhookTestConfig(t)
				if !tc.profiles {
					cfg.FacebookComments = nil
				}
				if tc.profiles {
					t.Setenv(cfg.FacebookComments[0].TokenEnv, "")
				}
				command.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
			}
			err := command.RunE(command, tc.args)
			var diagnostic interface {
				error
				Hint() string
				ExitCode() int
			}
			require.ErrorAs(err, &diagnostic)
			assert.Equal(tc.want, diagnostic.ExitCode())
			assert.NotEmpty(diagnostic.Hint())
		})
	}
}

// Project a downstream inbox profile at its stable integration boundary while
// keeping the real command, Graph transport, app verification and parser.
func TestSubscribeAdditionalPageProfiles(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	cfg := webhookTestConfig(t)
	cfg.FacebookComments = nil
	original := additionalWebhookPageProfiles
	t.Cleanup(func() { additionalWebhookPageProfiles = original })
	additionalWebhookPageProfiles = func(*config.Config) []webhookPageProfile {
		return []webhookPageProfile{{Kind: "facebook_messages", Identifier: "example-inbox", AppID: "9", PageID: "100", TokenEnv: "EXAMPLE_META_PAGE_TOKEN"}}
	}
	t.Setenv("EXAMPLE_META_PAGE_TOKEN", "synthetic-page-token")
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	subscriptionFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v26.0/app" {
			_, _ = fmt.Fprint(w, `{"id":"9"}`)
			return
		}
		assert.Equal("/v26.0/100/subscribed_apps", r.URL.Path)
		assert.Equal(http.MethodGet, r.Method)
		_, _ = fmt.Fprint(w, `{"data":[{"id":"9","subscribed_fields":["messages"]}]}`)
	})
	cmd := newSubscribeMetaWebhooksCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	require.NoError(cmd.Flags().Set("fields", "messages"))
	require.NoError(cmd.Flags().Set("check", "true"))
	for _, ids := range [][]string{nil, {"example-inbox"}, {"facebook_messages:example-inbox"}} {
		require.NoError(cmd.RunE(cmd, ids))
	}
}

func TestSubscribeProfileCollisionRefusesBeforeClient(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	cfg := webhookTestConfig(t)
	original := additionalWebhookPageProfiles
	t.Cleanup(func() { additionalWebhookPageProfiles = original })
	additionalWebhookPageProfiles = func(*config.Config) []webhookPageProfile {
		return []webhookPageProfile{{Kind: "instagram_messages", Identifier: "example-page", AppID: "9", PageID: "101", TokenEnv: "EXAMPLE_META_PAGE_TOKEN"}}
	}
	_, err := subscribeMetaWebhooks(t.Context(), cfg, []string{"example-page"}, []string{"messages"}, true, func(string) (*meta.Client, error) {
		assert.Fail("ambiguous selection must fail before token verification")
		return nil, errors.New("unexpected client")
	})
	require.ErrorContains(err, "ambiguous")
	assert.Contains(err.Error(), "facebook_comments:example-page")
	assert.Contains(err.Error(), "instagram_messages:example-page")
	profiles, err := resolveWebhookPageProfiles([]string{"instagram_messages:example-page"}, cfg)
	require.NoError(err)
	require.Len(profiles, 1)
	assert.Equal("101", profiles[0].PageID)
	profiles, err = resolveWebhookPageProfiles([]string{"facebook_comments:EXAMPLE-PAGE"}, cfg)
	require.NoError(err)
	require.Len(profiles, 1)
	assert.Equal("100", profiles[0].PageID)
}
