package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
)

func TestPersonDirectoryProductionCommandRegistrationAndFlags(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	command, args, err := rootCmd.Find([]string{personValue, "directory"})
	require.NoError(err)
	require.Equal("directory", command.Name())
	assert.Empty(args)
	assert.Same(personCmd, command.Parent())
	var flags []string
	command.Flags().VisitAll(func(flag *pflag.Flag) { flags = append(flags, flag.Name) })
	assert.Equal([]string{"cursor", "json", "last-contact-after", "last-contact-before", "sort"}, flags)
	assert.Equal("last_contact_desc", command.Flags().Lookup("sort").DefValue)
	require.NoError(command.Args(command, nil))
	require.Error(command.Args(command, []string{"unexpected"}))
}

func personDirectoryTestResponse(t *testing.T, status int, payload string) <-chan *http.Request {
	t.Helper()
	requests := make(chan *http.Request, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})
	return requests
}

func runPersonDirectoryCommand(ctx context.Context, t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: "msgvault", SilenceErrors: true, SilenceUsage: true}
	localFlag := rootCmd.PersistentFlags().Lookup("local")
	savedChanged := localFlag.Changed
	t.Cleanup(func() { localFlag.Changed = savedChanged })
	root.PersistentFlags().AddFlag(localFlag)
	person := &cobra.Command{Use: personValue}
	person.AddCommand(newPersonDirectoryCommand())
	root.AddCommand(person)
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(io.Discard)
	root.SetArgs(append([]string{personValue, "directory"}, args...))
	err := root.ExecuteContext(ctx)
	return output.String(), err
}

func TestPersonDirectoryCommandMapsDirectoryQueryParameters(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want url.Values
	}{
		{name: "default recent ordering", want: url.Values{"sort": {"last_contact_desc"}}},
		{
			name: "date boundary 2026-06-01T00:00:00Z",
			args: []string{
				"--last-contact-after", "2026-06-01", "--last-contact-before", "2026-07-01",
				"--sort", "last_contact_asc", "--cursor", "opaque+/=&% cursor",
			},
			want: url.Values{
				"last_contact_after": {"2026-06-01T00:00:00Z"}, "last_contact_before": {"2026-07-01T00:00:00Z"},
				"sort": {"last_contact_asc"}, "cursor": {"opaque+/=&% cursor"},
			},
		},
		{
			name: "offset and fractional timestamps",
			args: []string{
				"--last-contact-after", "2026-06-01T02:30:00.123456789+02:30",
				"--last-contact-before", "2026-06-02T01:00:00.5-04:00", "--sort", "name",
			},
			want: url.Values{
				"last_contact_after":  {"2026-06-01T00:00:00.123456789Z"},
				"last_contact_before": {"2026-06-02T05:00:00.5Z"}, "sort": {"name"},
			},
		},
		{
			name: "empty optional values",
			args: []string{"--last-contact-after=", "--last-contact-before=", "--sort=", "--cursor="},
			want: url.Values{"sort": {"last_contact_desc"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			requests := personDirectoryTestResponse(t, http.StatusOK, `{"people":[]}`)
			output, err := runPersonDirectoryCommand(t.Context(), t, append(tc.args, "--json")...)
			require.NoError(err)
			require.Len(requests, 1)
			request := <-requests
			assert.Equal(http.MethodGet, request.Method)
			assert.Equal("/api/v1/people/directory", request.URL.Path)
			assert.Equal(tc.want, request.URL.Query())
			assert.False(request.URL.Query().Has("limit"))
			assert.JSONEq(`{"people":[]}`, output)
		})
	}
	for _, flag := range []string{"--last-contact-after", "--last-contact-before"} {
		for _, invalid := range []string{"yesterday", "2026-02-30", "2026-06-01T25:00:00Z"} {
			t.Run(flag+"="+invalid, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				requests := personDirectoryTestResponse(t, http.StatusOK, `{"people":[]}`)
				output, err := runPersonDirectoryCommand(t.Context(), t, flag, invalid, "--json")
				require.ErrorContains(err, flag+": must be YYYY-MM-DD or RFC3339")
				assert.Empty(requests)
				assert.Empty(output)
				cfg = nil
				_, err = runPersonDirectoryCommand(t.Context(), t, flag, invalid)
				assert.ErrorContains(err, flag+": must be YYYY-MM-DD or RFC3339")
			})
		}
	}
}

func TestPersonDirectoryCommandForwardsCursorAndPrintsNextCursor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	const cursor = "opaque+/=&% cursor"
	var cursors []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/people/directory", r.URL.Path)
		cursors = append(cursors, r.URL.Query().Get("cursor"))
		w.Header().Set("Content-Type", "application/json")
		if len(cursors) == 1 {
			_, _ = io.WriteString(w, `{"people":[],"next_cursor":"`+cursor+`"}`)
		} else {
			_, _ = io.WriteString(w, `{"people":[]}`)
		}
	}))
	t.Cleanup(server.Close)
	withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{URL: server.URL, AllowInsecure: true}})
	first, err := runPersonDirectoryCommand(t.Context(), t)
	require.NoError(err)
	assert.Contains(first, "Next cursor: "+cursor+"\n")
	second, err := runPersonDirectoryCommand(t.Context(), t, "--cursor", cursor)
	require.NoError(err)
	assert.NotContains(second, "Next cursor")
	assert.Equal([]string{"", cursor}, cursors)
}

const personDirectoryCLIPayload = `{
	"people": [
		{
			"id": 9007199254740993, "revision": 9007199254740995, "display_name": "Zulu Example",
			"categories": ["friend", "colleague"], "organizations": ["Example Org", "Test Org"],
			"contact_state": "active", "primary_channel": "email",
			"last_contact_at": "2026-06-01T12:34:56.123456789+02:00"
		},
		{"id": 7, "revision": 9, "categories": [], "organizations": [], "contact_state": "inactive"}
	],
	"next_cursor": "opaque+/=&% cursor"
}`

func TestPersonDirectoryCommandJSONPreservesAbsentLastContact(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
	}{
		{name: "full envelope", payload: personDirectoryCLIPayload},
		{name: "empty page without cursor", payload: `{"people":[]}`},
		{name: "present empty cursor", payload: `{"people":[],"next_cursor":""}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			personDirectoryTestResponse(t, http.StatusOK, tc.payload)
			output, err := runPersonDirectoryCommand(t.Context(), t, "--json")
			require.NoError(err)
			decode := func(raw string) any {
				decoder := json.NewDecoder(strings.NewReader(raw))
				decoder.UseNumber()
				var value any
				require.NoError(decoder.Decode(&value))
				return value
			}
			assert.Equal(decode(tc.payload), decode(output))
		})
	}
}

func TestPersonDirectoryCommandHumanOutputShowsRecentContactFields(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	personDirectoryTestResponse(t, http.StatusOK, personDirectoryCLIPayload)
	savedJSON := personJSON
	personJSON = true
	t.Cleanup(func() { personJSON = savedJSON })
	output, err := runPersonDirectoryCommand(t.Context(), t)
	require.NoError(err)
	assert.True(personJSON)
	lines := strings.Split(strings.TrimSpace(output), "\n")
	require.Len(lines, 4)
	columns := regexp.MustCompile(` {2,}`)
	assert.Equal([]string{"ID", "DISPLAY NAME", "LAST CONTACT"}, columns.Split(lines[0], -1))
	assert.Equal([]string{"9007199254740993", "Zulu Example", "2026-06-01T10:34:56Z"}, columns.Split(lines[1], -1))
	assert.Equal([]string{"7", "-", "-"}, columns.Split(lines[2], -1))
	assert.Equal("Next cursor: opaque+/=&% cursor", lines[3])
}

func TestPersonDirectoryCommandSanitizesDaemonSuppliedText(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	const payload = `{
		"people": [{
			"id": 7, "revision": 9, "display_name": "\u001b[31mAlice\u001b[0m\r\n\u0007\u009b Example",
			"categories": [], "organizations": [], "contact_state": "active"
		}],
		"next_cursor": "\u001b]8;;https://example.test\u0007opaque\u001b]8;;\u0007\u001b[31m-cursor\u001b[0m\r\n"
	}`
	personDirectoryTestResponse(t, http.StatusOK, payload)
	output, err := runPersonDirectoryCommand(t.Context(), t)
	require.NoError(err)
	for _, control := range []string{"\x1b", "\r", "\a", "\u009b"} {
		assert.NotContains(output, control)
	}
	assert.NotContains(output, "https://example.test")
	assert.Contains(strings.Join(strings.Fields(output), " "), "Alice Example")
	assert.True(strings.HasSuffix(strings.TrimSpace(output), "Next cursor: opaque-cursor"))
	assert.Len(strings.Split(strings.TrimSpace(output), "\n"), 3)
	jsonOutput, err := runPersonDirectoryCommand(t.Context(), t, "--json")
	require.NoError(err)
	assert.JSONEq(payload, jsonOutput)
}

func personDirectoryLocalTestDaemon(t *testing.T, handler http.HandlerFunc) *config.Config {
	t.Helper()
	localCfg := lifecycleTestConfig(t.TempDir())
	localCfg.Server.APIKey = "local-directory-secret"
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{Service: daemonService, Version: Version}))
	mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, localCfg.Server.APIKey, r.Header.Get("X-Api-Key"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("/api/v1/people/directory", handler)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	runtime := daemonRuntimeForHTTPServer(t, server, daemonAPIKeyFingerprint(localCfg.Server.APIKey))
	runtime.Record.Metadata[runtimeShutdownToken] = "local-directory-runtime-token"
	_, err := daemonRuntimeStore(localCfg.Data.DataDir).Write(runtime.Record)
	require.NoError(t, err)
	return localCfg
}

func TestPersonDirectoryCLIDaemonRouting(t *testing.T) {
	for _, mode := range []string{"configured remote", "local default", "local override", "unreachable remote"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var localRequests, remoteRequests, starts atomic.Int32
			handler := func(key string, count *atomic.Int32) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					count.Add(1)
					assert.Equal(http.MethodGet, r.Method)
					assert.Equal("/api/v1/people/directory", r.URL.Path)
					assert.Equal(apiprotocol.ClientClassCLI, r.Header.Get(apiprotocol.ClientClassHeader))
					assert.Equal(key, r.Header.Get("X-Api-Key"))
					assert.Equal("application/json", r.Header.Get("Accept"))
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"people":[]}`)
				}
			}
			localCfg := personDirectoryLocalTestDaemon(t, handler("local-directory-secret", &localRequests))
			remote := httptest.NewServer(handler("remote-directory-secret", &remoteRequests))
			t.Cleanup(remote.Close)
			if mode != "local default" {
				localCfg.Remote = config.RemoteConfig{URL: remote.URL, APIKey: "remote-directory-secret", AllowInsecure: true}
			}
			withStoreResolverConfig(t, localCfg)
			stubStartServeBackgroundProcess(t, func(*config.Config, backgroundServeStartOptions) (*backgroundServeProcess, error) {
				starts.Add(1)
				return nil, errors.New("unexpected daemon start")
			})
			args := []string{"--json"}
			if mode == "local override" {
				args = append(args, "--local")
			}
			if mode == "unreachable remote" {
				remote.Close()
			}
			output, err := runPersonDirectoryCommand(t.Context(), t, args...)
			switch mode {
			case "unreachable remote":
				require.Error(err)
				assert.Empty(output)
				assert.Zero(localRequests.Load())
				assert.Zero(remoteRequests.Load())
			case "configured remote":
				require.NoError(err)
				assert.Zero(localRequests.Load())
				assert.Equal(int32(1), remoteRequests.Load())
			default:
				require.NoError(err)
				assert.Equal(int32(1), localRequests.Load())
				assert.Zero(remoteRequests.Load())
			}
			assert.Zero(starts.Load())
		})
	}
}

func TestPersonDirectoryCLICancellation(t *testing.T) {
	for _, mode := range []string{"remote", "local"} {
		t.Run(mode, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			started := make(chan struct{})
			canceled := make(chan struct{})
			var localRequests atomic.Int32
			handler := func(_ http.ResponseWriter, r *http.Request) {
				close(started)
				<-r.Context().Done()
				close(canceled)
			}
			localHandler := handler
			if mode == "remote" {
				localHandler = func(w http.ResponseWriter, _ *http.Request) {
					localRequests.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"people":[]}`)
				}
			}
			localCfg := personDirectoryLocalTestDaemon(t, localHandler)
			if mode == "remote" {
				remote := httptest.NewServer(http.HandlerFunc(handler))
				t.Cleanup(remote.Close)
				localCfg.Remote = config.RemoteConfig{URL: remote.URL, AllowInsecure: true}
			}
			withStoreResolverConfig(t, localCfg)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			go func() {
				select {
				case <-started:
					cancel()
				case <-ctx.Done():
				}
			}()
			output, err := runPersonDirectoryCommand(ctx, t, "--json")
			require.ErrorIs(err, context.Canceled)
			assert.Empty(output)
			select {
			case <-canceled:
			case <-time.After(30 * time.Second):
				require.FailNow("daemon did not observe request cancellation")
			}
			assert.Zero(localRequests.Load())
		})
	}
}

func TestPersonDirectoryCommandRejectsInvalidSortBeforeRequest(t *testing.T) {
	assert := assert.New(t)
	requests := personDirectoryTestResponse(t, http.StatusOK, `{"people":[]}`)
	output, err := runPersonDirectoryCommand(t.Context(), t, "--sort", "oldest", "--json")
	require.ErrorContains(t, err, "--sort: must be name, last_contact_desc, or last_contact_asc")
	assert.Empty(requests)
	assert.Empty(output)

	cfg = nil
	_, err = runPersonDirectoryCommand(t.Context(), t, "--sort", "oldest")
	assert.ErrorContains(err, "--sort:")
}

func TestPersonDirectoryCommandReturnsDaemonQueryErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		query  url.Values
		status int
		code   string
	}{
		{
			name: "inverted range",
			args: []string{"--last-contact-after", "2026-07-01", "--last-contact-before", "2026-06-01"},
			query: url.Values{
				"sort": {"last_contact_desc"}, "last_contact_after": {"2026-07-01T00:00:00Z"},
				"last_contact_before": {"2026-06-01T00:00:00Z"},
			},
			status: http.StatusBadRequest, code: "invalid_query",
		},
		{
			name: "invalid cursor", args: []string{"--cursor", "not-a-cursor"},
			query:  url.Values{"sort": {"last_contact_desc"}, "cursor": {"not-a-cursor"}},
			status: http.StatusBadRequest, code: "invalid_cursor",
		},
		{
			name: "stale projection", query: url.Values{"sort": {"last_contact_desc"}},
			status: http.StatusServiceUnavailable, code: "directory_projection_stale",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			requests := personDirectoryTestResponse(t, tc.status, `{"error":"`+tc.code+`","message":"Daemon rejected Directory query"}`)
			output, err := runPersonDirectoryCommand(t.Context(), t, append(tc.args, "--json")...)
			var apiErr *daemonclient.APIError
			require.ErrorAs(err, &apiErr)
			assert.Equal(tc.status, apiErr.Status)
			assert.Equal(tc.code, apiErr.Code)
			assert.Equal("Daemon rejected Directory query", apiErr.Message)
			assert.Empty(output)
			require.Len(requests, 1)
			assert.Equal(tc.query, (<-requests).URL.Query())
		})
	}
}

func TestPersonDirectoryCommandReturnsResponseErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		payload string
	}{
		{name: "missing route", status: http.StatusNotFound, payload: `{"error":"not_found","message":"Route unavailable"}`},
		{name: "malformed response", status: http.StatusOK, payload: `{"people":`},
		{name: "invalid timestamp", status: http.StatusOK, payload: `{"people":[{"last_contact_at":"\u001b[31mnow"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := personDirectoryTestResponse(t, tc.status, tc.payload)
			output, err := runPersonDirectoryCommand(t.Context(), t, "--json")
			require.Error(t, err)
			assert.Empty(t, output)
			assert.Len(t, requests, 1)
		})
	}
}
