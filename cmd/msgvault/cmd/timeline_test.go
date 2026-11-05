package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/timeline"
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
	assertions := assert.New(t)
	requirements := require.New(t)
	old := timelineOpen
	t.Cleanup(func() { timelineOpen = old })
	fake := &timelineFakeClient{}
	timelineOpen = func(context.Context) (timelineClient, func(), error) { return fake, func() {}, nil }
	command := newTimelineCommand()
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetArgs([]string{"--with", "person:7", "--around", "msg:9", "--date", "2027-03-01", "--before", "2", "--after", "3", "--conversation", "--tier", "1,3", "--include-cold", "--include-noise", "--brief", "--budget", "4096", "--cursor", "next", "--json"})
	requirements.NoError(command.Execute())
	assertions.Equal(timeline.Request{With: "person:7", Around: "msg:9", Date: "2027-03-01", Before: 2, After: 3, BeforeSet: true, AfterSet: true, Conversation: true, Tiers: []int{1, 3}, IncludeCold: true, IncludeNoise: true, Brief: true, Budget: 4096, Cursor: "next", Format: "json"}, fake.req)
	assertions.Contains(out.String(), `"mode":"anchor"`)
	command = newTimelineCommand()
	out.Reset()
	command.SetOut(&out)
	command.SetArgs(nil)
	requirements.NoError(command.Execute())
	assertions.Equal("# Timeline\n", out.String())
	assertions.Equal("markdown", fake.req.Format)
}

// Exercise the production opener: endpoint selection and authentication must
// finish before the timeline-specific compatibility check runs.
func TestTimelineCommandRefusesOldDaemon(t *testing.T) {
	unsetServerKeyEnvironmentForTest(t)
	oldSchemaCheck := remoteAPISchemaCheckEnabled
	remoteAPISchemaCheckEnabled = true
	t.Cleanup(func() { remoteAPISchemaCheckEnabled = oldSchemaCheck })
	for _, tc := range []struct{ kind, version string }{
		{"local", "3.43.0"}, {"remote", "3.43.0"}, {"delegated", "3.43.0"},
	} {
		t.Run(tc.kind+"/"+tc.version, func(t *testing.T) {
			kind, version := tc.kind, tc.version
			assertions := assert.New(t)
			requirements := require.New(t)
			var timelineCalls, healthCalls, cliHealthCalls, sessionCalls, wrongEndpointCalls atomic.Int32
			apiKey, agentToken := "synthetic-local", ""
			switch kind {
			case "remote":
				apiKey = "synthetic-remote"
			case "delegated":
				apiKey, agentToken = "", "mva1_synthetic_timeline_reader"
			}
			checkAuth := func(r *http.Request) {
				assert.Equal(t, apiKey, r.Header.Get("X-Api-Key"))
				assert.Equal(t, agentToken, r.Header.Get(apiprotocol.AgentTokenHeader))
				// Local lifecycle discovery probes health before selecting CLI mode.
				if class := r.Header.Get(apiprotocol.ClientClassHeader); kind != "local" || class != "" {
					assert.Equal(t, apiprotocol.ClientClassCLI, class)
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
				_, err := fmt.Fprintf(w, `{"status":"ok","api_schema_version":%q}`, version)
				assert.NoError(t, err)
			})
			mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
				checkAuth(r)
				sessionCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, err := fmt.Fprint(w, `{"auth_mode":"delegated"}`)
				assert.NoError(t, err)
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
			rt.Record.Metadata[runtimeAPISchemaVersion] = version
			_, err := daemonRuntimeStore(cfg.Data.DataDir).Write(rt.Record)
			requirements.NoError(err)
			var options invocationOptions
			switch kind {
			case "remote":
				cfg.Remote = config.RemoteConfig{URL: selected.URL, APIKey: apiKey, AllowInsecure: true}
			case "delegated":
				cfg.Remote = config.RemoteConfig{URL: decoy.URL, APIKey: "synthetic-decoy", AllowInsecure: true}
				tokenPath := filepath.Join(t.TempDir(), "agent-token")
				requirements.NoError(os.WriteFile(tokenPath, []byte(agentToken), 0o600))
				options = invocationOptions{agentURL: selected.URL, agentTokenFile: tokenPath, agentAllowInsecure: true, agentURLChanged: true, agentTokenChanged: true}
			}
			ctx := testInvocationContext(t.Context(), cfg, options)
			command := newTimelineCommand()
			command.SetOut(&bytes.Buffer{})
			command.SetErr(&bytes.Buffer{})
			command.SetArgs(nil)
			requirements.EqualError(command.ExecuteContext(ctx), fmt.Sprintf(`timeline requires daemon API schema 3.44.0 or newer (daemon reports %q); upgrade and restart the daemon`, version))
			assertions.Positive(healthCalls.Load(), "selected endpoint must report its schema")
			assertions.Positive(cliHealthCalls.Load(), "timeline compatibility probe must use CLI mode")
			assertions.Zero(timelineCalls.Load())
			assertions.Zero(wrongEndpointCalls.Load(), "resolver must select the expected endpoint")
			if kind == "delegated" {
				assertions.Equal(int32(1), sessionCalls.Load(), "real delegated resolution must confirm token authentication")
			} else {
				assertions.Zero(sessionCalls.Load())
			}
		})
	}
}

func TestTimelineRealTransportPreservesResponseAndFlagPresence(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Server.APIKey = "synthetic-owner"
	src, err := st.GetOrCreateSource("gmail", "owner@example.com")
	requirements.NoError(err)
	owner, err := st.EnsureParticipant("owner@example.com", "Owner", "example.com")
	requirements.NoError(err)
	peer, err := st.EnsureParticipant("alice@example.com", "Alice", "example.com")
	requirements.NoError(err)
	requirements.NoError(st.AddAccountIdentity(src.ID, "owner@example.com", "manual"))
	now := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	var ids []int64
	for i := range 24 {
		conv, err := st.EnsureConversation(src.ID, strconv.Itoa(i), "Synthetic")
		requirements.NoError(err)
		id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: strconv.Itoa(i), MessageType: "email", SenderID: sql.NullInt64{Int64: peer, Valid: true}, SentAt: sql.NullTime{Time: now.Add(-time.Duration(i) * time.Second), Valid: true}, Snippet: sql.NullString{String: strings.Repeat("\"\\ <é🙂>", 12), Valid: true}})
		requirements.NoError(err)
		requirements.NoError(st.ReplaceMessageRecipients(id, "from", []int64{peer}, nil))
		requirements.NoError(st.ReplaceMessageRecipients(id, "to", []int64{owner}, nil))
		ids = append(ids, id)
	}
	projector, err := activity.NewProjector(st, activity.Options{Timezone: "UTC"})
	requirements.NoError(err)
	_, err = projector.RunOnce(t.Context())
	requirements.NoError(err)
	adapter := &storeAPIAdapter{store: st, config: cfg}
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { require.NoError(t, server.Shutdown(context.Background())) })
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		server.Router().ServeHTTP(rec, r)
		if r.URL.Path == "/api/v1/timeline" {
			raw = bytes.Clone(rec.Body.Bytes())
		}
		maps.Copy(w.Header(), rec.Header())
		w.WriteHeader(rec.Code)
		_, err := w.Write(rec.Body.Bytes())
		assert.NoError(t, err)
	}))
	t.Cleanup(srv.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: srv.URL, APIKey: cfg.Server.APIKey, AllowInsecure: true})
	requirements.NoError(err)
	old := timelineOpen
	t.Cleanup(func() { timelineOpen = old })
	timelineOpen = func(context.Context) (timelineClient, func(), error) { return client, func() {}, nil }
	var out bytes.Buffer
	command := newTimelineCommand()
	command.SetOut(&out)
	command.SetArgs([]string{"--date", now.Format(time.DateOnly), "--json", "--budget", "4096"})
	requirements.NoError(command.Execute())
	requirements.NotEmpty(raw)
	assertions.Equal(string(raw), out.String(), "CLI JSON must preserve the fitted compact response")
	assertions.Contains(out.String(), fmt.Sprintf(`"id":%d`, ids[0]))
	assertions.Contains(out.String(), `\"`)
	assertions.Contains(out.String(), `\\`)

	// Exercise Cobra flag presence through the real HTTP route and composer.
	// The existing fixture has one burst per conversation around this anchor.
	anchor := fmt.Sprintf("msg:%d", ids[12])
	for _, explicitZero := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit-zero=%t", explicitZero), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			args := []string{"--around", anchor, "--json", "--budget", "65536"}
			if explicitZero {
				args = append(args, "--before", "0", "--after", "0")
			}
			var output bytes.Buffer
			command := newTimelineCommand()
			command.SetOut(&output)
			command.SetArgs(args)
			requirements.NoError(command.Execute())
			var got timeline.Timeline
			requirements.NoError(json.Unmarshal(output.Bytes(), &got))
			requirements.NotNil(got.Anchor)
			assertions.False(got.Anchor.Filtered)
			if explicitZero {
				requirements.Len(got.History, 1)
				requirements.NotNil(got.History[0].Burst)
				assertions.Equal([]int64{ids[12]}, got.History[0].Burst.LatestMessageIDs)
			} else {
				assertions.Greater(len(got.History), 1, "omitted flags retain default neighbors")
			}
		})
	}
}

func TestTimelineCommandExplicitZeros(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
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
		requirements.ErrorContains(command.Execute(), "--"+flag+" must be between")
	}
	assertions.Zero(opens, "invalid explicit zeros fail before endpoint resolution")
	command := newTimelineCommand()
	command.SetOut(&bytes.Buffer{})
	command.SetArgs([]string{"--around", "msg:7", "--before", "0", "--after", "0"})
	requirements.NoError(command.Execute())
	assertions.Zero(fake.req.Before)
	assertions.Zero(fake.req.After)
	assertions.True(fake.req.BeforeSet)
	assertions.True(fake.req.AfterSet)
	command = newTimelineCommand()
	command.SetOut(&bytes.Buffer{})
	command.SetArgs(nil)
	requirements.NoError(command.Execute())
	assertions.Equal(5, fake.req.Before)
	assertions.Equal(5, fake.req.After)
	assertions.False(fake.req.BeforeSet)
	assertions.False(fake.req.AfterSet)
}

func TestTimelineRealTransportAmbiguityPrintsUsableReferences(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Server.APIKey = "synthetic-owner"
	const name = "Alex\x1b[31m"
	savedParticipant, err := st.EnsureParticipant("saved@example.com", name, "example.com")
	requirements.NoError(err)
	unsavedParticipant, err := st.EnsureParticipant("unsaved@example.com", name, "example.com")
	requirements.NoError(err)
	person, _, err := st.CreatePersonFromParticipant(savedParticipant)
	requirements.NoError(err)
	adapter := &storeAPIAdapter{store: st, config: cfg}
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: adapter, Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { requirements.NoError(server.Shutdown(context.Background())) })
	srv := httptest.NewServer(server.Router())
	t.Cleanup(srv.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: srv.URL, APIKey: cfg.Server.APIKey, AllowInsecure: true})
	requirements.NoError(err)
	old := timelineOpen
	t.Cleanup(func() { timelineOpen = old })
	timelineOpen = func(context.Context) (timelineClient, func(), error) { return client, func() {}, nil }
	var candidates, output bytes.Buffer
	command := newTimelineCommand()
	command.SilenceErrors = true
	command.SilenceUsage = true
	command.SetErr(&candidates)
	command.SetOut(&output)
	command.SetArgs([]string{"--with", "name:" + name, "--json"})
	var ambiguous *timeline.ErrAmbiguousReference
	requirements.ErrorAs(command.Execute(), &ambiguous)
	requirements.Len(ambiguous.Candidates, 2)
	assertions.NotContains(candidates.String(), "\x1b")
	assertions.NotContains(candidates.String(), "person:0")
	lines := strings.Split(strings.TrimSpace(candidates.String()), "\n")
	requirements.Len(lines, 2)
	refs := []string{}
	for _, line := range lines {
		refs = append(refs, strings.Split(line, "\t")[0])
	}
	assertions.ElementsMatch([]string{fmt.Sprintf("person:%d", person.ID), fmt.Sprintf("participant:%d", unsavedParticipant)}, refs)
	for _, ref := range refs {
		var selected bytes.Buffer
		command := newTimelineCommand()
		command.SetOut(&selected)
		command.SetArgs([]string{"--with", ref, "--json"})
		requirements.NoError(command.Execute(), ref)
		var result timeline.Timeline
		requirements.NoError(json.Unmarshal(selected.Bytes(), &result))
		assertions.Equal(timeline.ModeRelationship, result.Mode)
	}
}
