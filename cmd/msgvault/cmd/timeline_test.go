package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/timeline"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type timelineFakeClient struct {
	req   timeline.Request
	calls int
	err   error
}

func (f *timelineFakeClient) APISchemaVersion(context.Context) (string, error) { return "3.44.0", nil }
func (f *timelineFakeClient) Timeline(_ context.Context, req timeline.Request) (*timeline.Timeline, string, error) {
	f.req = req
	f.calls++
	return &timeline.Timeline{Mode: timeline.ModeAnchor}, "# Timeline\n", f.err
}
func TestTimelineCommandFlagsAndFormats(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	old := timelineOpen
	t.Cleanup(func() { timelineOpen = old })
	fake := &timelineFakeClient{}
	timelineOpen = func(context.Context) (timelineClient, func(), error) { return fake, func() {}, nil }
	command := newTimelineCommand()
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetArgs([]string{"--with", "person:7", "--around", "msg:9", "--date", "2027-03-01", "--before", "2", "--after", "3", "--conversation", "--tier", "1,3", "--include-cold", "--include-noise", "--brief", "--budget", "4096", "--cursor", "next", "--json"})
	require.NoError(command.Execute())
	assert.Equal(timeline.Request{With: "person:7", Around: "msg:9", Date: "2027-03-01", Before: 2, After: 3, BeforeSet: true, AfterSet: true, Conversation: true, Tiers: []int{1, 3}, IncludeCold: true, IncludeNoise: true, Brief: true, Budget: 4096, Cursor: "next", Format: "json"}, fake.req)
	assert.Contains(out.String(), `"mode":"anchor"`)
	command = newTimelineCommand()
	out.Reset()
	command.SetOut(&out)
	command.SetArgs(nil)
	require.NoError(command.Execute())
	assert.Equal("# Timeline\n", out.String())
	assert.Equal("markdown", fake.req.Format)
	fake.err = &timeline.ErrAmbiguousReference{Candidates: []timeline.Candidate{{PersonID: 7, DisplayName: "Alice", Address: "alice@example.com"}}}
	command = newTimelineCommand()
	out.Reset()
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetArgs([]string{"--with", "Alice"})
	require.Error(command.Execute())
	assert.Contains(out.String(), "Alice")
	assert.Contains(out.String(), "person:7")
}

// Exercise the production opener: endpoint selection and authentication must
// finish before the timeline-specific compatibility check runs.
func TestTimelineCommandRefusesOldDaemon(t *testing.T) {
	unsetServerKeyEnvironmentForTest(t)
	oldSchemaCheck := remoteAPISchemaCheckEnabled
	remoteAPISchemaCheckEnabled = true
	t.Cleanup(func() { remoteAPISchemaCheckEnabled = oldSchemaCheck })
	for _, kind := range []string{"local", "remote", "delegated"} {
		t.Run(kind, func(t *testing.T) {
			assert := assertpkg.New(t)
			require := requirepkg.New(t)
			var timelineCalls, healthCalls, cliHealthCalls, sessionCalls, wrongEndpointCalls atomic.Int32
			apiKey, agentToken := "synthetic-local", ""
			if kind == "remote" {
				apiKey = "synthetic-remote"
			} else if kind == "delegated" {
				apiKey, agentToken = "", "mva1_synthetic_timeline_reader"
			}
			checkAuth := func(r *http.Request) {
				assertpkg.Equal(t, apiKey, r.Header.Get("X-Api-Key"))
				assertpkg.Equal(t, agentToken, r.Header.Get(apiprotocol.AgentTokenHeader))
				// Local lifecycle discovery probes health before selecting CLI mode.
				if class := r.Header.Get(apiprotocol.ClientClassHeader); kind != "local" || class != "" {
					assertpkg.Equal(t, apiprotocol.ClientClassCLI, class)
				}
			}
			mux := http.NewServeMux()
			mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{Service: daemonService, Version: Version}))
			mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, r *http.Request) {
				checkAuth(r)
				healthCalls.Add(1)
				if r.Header.Get(apiprotocol.ClientClassHeader) == apiprotocol.ClientClassCLI {
					cliHealthCalls.Add(1)
				}
				w.Header().Set("Content-Type", "application/json")
				_, err := fmt.Fprint(w, `{"status":"ok","api_schema_version":"3.32.0"}`)
				assertpkg.NoError(t, err)
			})
			mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
				checkAuth(r)
				sessionCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, err := fmt.Fprint(w, `{"auth_mode":"delegated"}`)
				assertpkg.NoError(t, err)
			})
			mux.HandleFunc("/api/v1/timeline", func(w http.ResponseWriter, r *http.Request) {
				timelineCalls.Add(1)
				http.NotFound(w, r)
			})
			selected := httptest.NewServer(mux)
			t.Cleanup(selected.Close)
			decoy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wrongEndpointCalls.Add(1)
				http.NotFound(w, r)
			}))
			t.Cleanup(decoy.Close)
			cfg := lifecycleTestConfig(t.TempDir())
			cfg.Server.DaemonAutoStart = new(false)
			cfg.Server.APIKey = "synthetic-local"
			runtimeServer := decoy
			if kind == "local" {
				runtimeServer = selected
			}
			rt := daemonRuntimeForHTTPServer(t, runtimeServer, daemonAPIKeyFingerprint(cfg.Server.APIKey))
			rt.Record.Metadata[runtimeAPISchemaVersion] = "3.32.0"
			_, err := daemonRuntimeStore(cfg.Data.DataDir).Write(rt.Record)
			require.NoError(err)
			var options invocationOptions
			switch kind {
			case "remote":
				cfg.Remote = config.RemoteConfig{URL: selected.URL, APIKey: apiKey, AllowInsecure: true}
			case "delegated":
				cfg.Remote = config.RemoteConfig{URL: decoy.URL, APIKey: "synthetic-decoy", AllowInsecure: true}
				tokenPath := filepath.Join(t.TempDir(), "agent-token")
				require.NoError(os.WriteFile(tokenPath, []byte(agentToken), 0o600))
				options = invocationOptions{agentURL: selected.URL, agentTokenFile: tokenPath, agentAllowInsecure: true, agentURLChanged: true, agentTokenChanged: true}
			}
			ctx := testInvocationContext(t.Context(), cfg, options)
			command := newTimelineCommand()
			command.SetOut(&bytes.Buffer{})
			command.SetErr(&bytes.Buffer{})
			command.SetArgs(nil)
			require.EqualError(command.ExecuteContext(ctx), `timeline requires daemon API schema 3.44.0 or newer (daemon reports "3.32.0"); upgrade and restart the daemon`)
			assert.Greater(healthCalls.Load(), int32(0), "selected endpoint must report its schema")
			assert.Greater(cliHealthCalls.Load(), int32(0), "timeline compatibility probe must use CLI mode")
			assert.Zero(timelineCalls.Load())
			assert.Zero(wrongEndpointCalls.Load(), "resolver must select the expected endpoint")
			if kind == "delegated" {
				assert.Equal(int32(1), sessionCalls.Load(), "real delegated resolution must confirm token authentication")
			} else {
				assert.Zero(sessionCalls.Load())
			}
		})
	}
}

func TestTimelineRealTransportBudgetAndIntegerOpeners(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.Server.APIKey = "synthetic-owner"
	src, err := st.GetOrCreateSource("gmail", "owner@example.com")
	require.NoError(err)
	owner, err := st.EnsureParticipant("owner@example.com", "Owner", "example.com")
	require.NoError(err)
	peer, err := st.EnsureParticipant("alice@example.com", "Alice", "example.com")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(src.ID, "owner@example.com", "manual"))
	now := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	var ids []int64
	for i := range 24 {
		conv, err := st.EnsureConversation(src.ID, fmt.Sprint(i), "Synthetic")
		require.NoError(err)
		// Explicit synthetic IDs exercise the real wire above JavaScript's exact integer range.
		largeID := int64(9007199254740993 + i)
		_, err = st.DB().Exec(st.Rebind("INSERT INTO messages(id,source_id,conversation_id,source_message_id,message_type,sender_id,sent_at,snippet) VALUES(?,?,?,?,?,?,?,?)"), largeID, src.ID, conv, fmt.Sprint(i), "email", peer, now.Add(-time.Duration(i)*time.Second), strings.Repeat("\"\\ <é🙂>", 12))
		require.NoError(err)
		require.NoError(st.ReplaceMessageRecipients(largeID, "from", []int64{peer}, nil))
		require.NoError(st.ReplaceMessageRecipients(largeID, "to", []int64{owner}, nil))
		ids = append(ids, largeID)
	}
	projector, err := activity.NewProjector(st, activity.Options{Timezone: "UTC"})
	require.NoError(err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(err)
	adapter := &storeAPIAdapter{store: st, config: cfg}
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { requirepkg.NoError(t, server.Shutdown(context.Background())) })
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		server.Router().ServeHTTP(rec, r)
		if r.URL.Path == "/api/v1/timeline" {
			raw = bytes.Clone(rec.Body.Bytes())
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, err := w.Write(rec.Body.Bytes())
		assertpkg.NoError(t, err)
	}))
	t.Cleanup(srv.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: srv.URL, APIKey: cfg.Server.APIKey, AllowInsecure: true})
	require.NoError(err)
	old := timelineOpen
	t.Cleanup(func() { timelineOpen = old })
	timelineOpen = func(context.Context) (timelineClient, func(), error) { return client, func() {}, nil }
	var out bytes.Buffer
	command := newTimelineCommand()
	command.SetOut(&out)
	command.SetArgs([]string{"--date", now.Format(time.DateOnly), "--json", "--budget", "4096"})
	require.NoError(command.Execute())
	require.NotEmpty(raw)
	assert.LessOrEqual(len(raw), 4096)
	assert.Greater(len(raw), 3000)
	assert.LessOrEqual(out.Len(), 4096)
	assert.Equal(string(raw), out.String(), "CLI JSON must preserve the fitted compact response")
	assert.Contains(out.String(), `"id":9007199254740993`)
	assert.Contains(out.String(), `\"`)
	assert.Contains(out.String(), `\\`)

	// Exercise Cobra flag presence through the real HTTP route and composer.
	// The existing fixture has one burst per conversation around this anchor.
	anchor := fmt.Sprintf("msg:%d", ids[12])
	for _, explicitZero := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit-zero=%t", explicitZero), func(t *testing.T) {
			assert := assertpkg.New(t)
			require := requirepkg.New(t)
			args := []string{"--around", anchor, "--json", "--budget", "65536"}
			if explicitZero {
				args = append(args, "--before", "0", "--after", "0")
			}
			var output bytes.Buffer
			command := newTimelineCommand()
			command.SetOut(&output)
			command.SetArgs(args)
			require.NoError(command.Execute())
			var got timeline.Timeline
			require.NoError(json.Unmarshal(output.Bytes(), &got))
			require.NotNil(got.Anchor)
			assert.False(got.Anchor.Filtered)
			if explicitZero {
				require.Len(got.History, 1)
				require.NotNil(got.History[0].Burst)
				assert.Equal([]int64{ids[12]}, got.History[0].Burst.LatestMessageIDs)
			} else {
				assert.Greater(len(got.History), 1, "omitted flags retain default neighbors")
			}
		})
	}
}

func TestTimelineCommandExplicitZeros(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	old := timelineOpen
	t.Cleanup(func() { timelineOpen = old })
	fake := &timelineFakeClient{}
	opens := 0
	timelineOpen = func(context.Context) (timelineClient, func(), error) {
		opens++
		return fake, func() {}, nil
	}
	for _, flag := range []string{"days", "budget"} {
		command := newTimelineCommand()
		command.SetOut(&bytes.Buffer{})
		command.SetErr(&bytes.Buffer{})
		command.SetArgs([]string{"--" + flag, "0"})
		require.ErrorContains(command.Execute(), "--"+flag+" must be between")
	}
	assert.Zero(opens, "invalid explicit zeros fail before endpoint resolution")
	command := newTimelineCommand()
	command.SetOut(&bytes.Buffer{})
	command.SetArgs([]string{"--around", "msg:7", "--before", "0", "--after", "0"})
	require.NoError(command.Execute())
	assert.Zero(fake.req.Before)
	assert.Zero(fake.req.After)
	assert.True(fake.req.BeforeSet)
	assert.True(fake.req.AfterSet)
	command = newTimelineCommand()
	command.SetOut(&bytes.Buffer{})
	command.SetArgs(nil)
	require.NoError(command.Execute())
	assert.Equal(5, fake.req.Before)
	assert.Equal(5, fake.req.After)
	assert.False(fake.req.BeforeSet)
	assert.False(fake.req.AfterSet)
}
